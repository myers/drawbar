package k8s

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"

	runnerv1 "code.forgejo.org/forgejo/actions-proto/runner/v1"
	"github.com/myers/drawbar/pkg/reporter"
	"github.com/myers/drawbar/pkg/types"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"
)

// PodExecutor runs commands inside a pod container.
type PodExecutor interface {
	Exec(ctx context.Context, namespace, pod, container string, cmd []string) (string, error)
}

// LogStreamer opens a log stream for a container.
type LogStreamer interface {
	StreamLogs(ctx context.Context, namespace, pod, container string) (io.ReadCloser, error)
}

// WatchConfig controls polling behavior.
type WatchConfig struct {
	PollInterval time.Duration
	Executor     PodExecutor                // optional; defaults to SPDYExecutor
	Streamer     LogStreamer                 // optional; defaults to K8sLogStreamer
	CommandProc  *reporter.CommandProcessor  // optional; if set, parses workflow commands from log lines
}

// DefaultWatchConfig returns production defaults.
func DefaultWatchConfig() WatchConfig {
	return WatchConfig{PollInterval: 500 * time.Millisecond}
}

// SPDYExecutor implements PodExecutor using the k8s SPDY protocol.
type SPDYExecutor struct {
	Client  kubernetes.Interface
	RestCfg *rest.Config
}

func (s *SPDYExecutor) Exec(ctx context.Context, namespace, pod, container string, cmd []string) (string, error) {
	return execInPod(ctx, s.Client, s.RestCfg, namespace, pod, container, cmd)
}

// K8sLogStreamer implements LogStreamer using the k8s log API.
type K8sLogStreamer struct {
	Client kubernetes.Interface
}

func (l *K8sLogStreamer) StreamLogs(ctx context.Context, namespace, pod, container string) (io.ReadCloser, error) {
	return l.Client.CoreV1().Pods(namespace).GetLogs(pod, &corev1.PodLogOptions{
		Container: container,
		Follow:    true,
	}).Stream(ctx)
}

// WatchJob monitors the runner container, streams logs, and tracks step state
// via the entrypoint's state.jsonl file.
func WatchJob(ctx context.Context, client kubernetes.Interface, restCfg *rest.Config, namespace, jobName string, rep *reporter.Reporter, cfg WatchConfig) (runnerv1.Result, error) {
	executor := cfg.Executor
	if executor == nil {
		executor = &SPDYExecutor{Client: client, RestCfg: restCfg}
	}
	streamer := cfg.Streamer
	if streamer == nil {
		streamer = &K8sLogStreamer{Client: client}
	}
	return watchJobWith(ctx, client, executor, streamer, namespace, jobName, rep, cfg)
}

func watchJobWith(ctx context.Context, client kubernetes.Interface, executor PodExecutor, streamer LogStreamer, namespace, jobName string, rep *reporter.Reporter, cfg WatchConfig) (runnerv1.Result, error) {
	if cfg.PollInterval == 0 {
		cfg.PollInterval = 500 * time.Millisecond
	}

	// Wait for the pod.
	podName, err := waitForPod(ctx, client, namespace, jobName, cfg.PollInterval)
	if err != nil {
		return runnerv1.Result_RESULT_FAILURE, fmt.Errorf("waiting for pod: %w", err)
	}
	slog.Info("pod created", "pod", podName, "job", jobName)

	// Wait for the runner container to start.
	if err := waitForContainerRunning(ctx, client, namespace, podName, "runner", cfg.PollInterval); err != nil {
		return runnerv1.Result_RESULT_FAILURE, fmt.Errorf("waiting for runner container: %w", err)
	}
	slog.Info("runner container started", "pod", podName)

	// Stream logs and poll state in parallel.
	logDone := make(chan error, 1)
	go func() {
		logDone <- streamLogs(ctx, streamer, namespace, podName, "runner", rep, cfg.CommandProc)
	}()

	stateDone := make(chan error, 1)
	go func() {
		stateDone <- pollStateFileWith(ctx, executor, namespace, podName, rep, cfg.PollInterval)
	}()

	// Wait for log streaming to finish (container exits).
	<-logDone

	// Give state polling a moment to catch up.
	time.Sleep(cfg.PollInterval * 2)

	// Determine result from container exit code.
	result, err := getContainerResult(ctx, client, namespace, podName)
	if err != nil {
		return runnerv1.Result_RESULT_FAILURE, err
	}

	return result, nil
}

// pollStateFileWith reads the entrypoint's state.jsonl file to track step lifecycle.
func pollStateFileWith(ctx context.Context, executor PodExecutor, namespace, podName string, rep *reporter.Reporter, poll time.Duration) error {
	var lastOffset int

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(poll):
		}

		// Read state file via exec.
		output, err := executor.Exec(ctx, namespace, podName, "runner",
			[]string{"cat", "/shim/state.jsonl"})
		if err != nil {
			// Container may have exited — that's OK.
			if strings.Contains(err.Error(), "terminated") || strings.Contains(err.Error(), "not found") {
				return nil
			}
			continue
		}

		// Parse new events since last read.
		events, newOffset := parseStateEvents(output, lastOffset)
		for _, event := range events {
			routeStateEvent(event, rep)
		}
		lastOffset = newOffset
	}
}

// streamLogs follows container logs and routes each line to the reporter.
// If cmdProc is non-nil, workflow commands (::add-mask::, ::debug::, etc.) are
// parsed and handled before the line is sent to the reporter.
func streamLogs(ctx context.Context, streamer LogStreamer, namespace, podName, container string, rep *reporter.Reporter, cmdProc *reporter.CommandProcessor) error {
	stream, err := streamer.StreamLogs(ctx, namespace, podName, container)
	if err != nil {
		return fmt.Errorf("opening log stream for %s: %w", container, err)
	}
	defer stream.Close()

	reader := bufio.NewReader(stream)
	for {
		line, err := reader.ReadString('\n')
		if line != "" {
			line = strings.TrimRight(line, "\n\r")
			if cmdProc != nil {
				if processed := cmdProc.ProcessLine(line); processed != nil {
					rep.AddLog(*processed)
				}
			} else {
				rep.AddLog(line)
			}
		}
		if err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
	}
}

// parseStateEvents parses JSONL state events from raw output, starting at lastOffset.
// Returns parsed events and the new offset for the next call.
func parseStateEvents(output string, lastOffset int) ([]types.StateEvent, int) {
	trimmed := strings.TrimSpace(output)
	if trimmed == "" {
		return nil, lastOffset
	}
	lines := strings.Split(trimmed, "\n")
	var events []types.StateEvent
	for i := lastOffset; i < len(lines); i++ {
		line := strings.TrimSpace(lines[i])
		if line == "" {
			continue
		}
		var event types.StateEvent
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			slog.Debug("skipping malformed state event", "line", line, "error", err)
			continue
		}
		events = append(events, event)
	}
	return events, len(lines)
}

// routeStateEvent dispatches a state event to the reporter.
func routeStateEvent(event types.StateEvent, rep *reporter.Reporter) {
	switch event.Event {
	case "start":
		rep.StartStep(event.Step)
		slog.Info("step started", "step", event.Step, "name", event.Name)
	case "end":
		result := runnerv1.Result_RESULT_SUCCESS
		if event.ExitCode != 0 {
			result = runnerv1.Result_RESULT_FAILURE
		}
		rep.FinishStep(event.Step, result)
		slog.Info("step completed", "step", event.Step, "name", event.Name, "exit_code", event.ExitCode)
	case "skip":
		rep.FinishStep(event.Step, runnerv1.Result_RESULT_SKIPPED)
		slog.Info("step skipped (condition false)", "step", event.Step, "name", event.Name)
	}
}

// execInPod runs a command in a running container and returns stdout.
func execInPod(ctx context.Context, client kubernetes.Interface, restCfg *rest.Config, namespace, podName, container string, cmd []string) (string, error) {
	req := client.CoreV1().RESTClient().Post().
		Resource("pods").
		Name(podName).
		Namespace(namespace).
		SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: container,
			Command:   cmd,
			Stdout:    true,
			Stderr:    false,
		}, scheme.ParameterCodec)

	exec, err := remotecommand.NewSPDYExecutor(restCfg, "POST", req.URL())
	if err != nil {
		return "", fmt.Errorf("creating SPDY executor: %w", err)
	}

	var stdout bytes.Buffer
	err = exec.StreamWithContext(ctx, remotecommand.StreamOptions{
		Stdout: &stdout,
	})
	if err != nil {
		return "", err
	}
	return stdout.String(), nil
}

// getContainerResult checks the runner container's exit code.
func getContainerResult(ctx context.Context, client kubernetes.Interface, namespace, podName string) (runnerv1.Result, error) {
	pod, err := client.CoreV1().Pods(namespace).Get(ctx, podName, metav1.GetOptions{})
	if err != nil {
		return runnerv1.Result_RESULT_FAILURE, err
	}

	for _, cs := range pod.Status.ContainerStatuses {
		if cs.Name == "runner" {
			if cs.State.Terminated != nil {
				if cs.State.Terminated.ExitCode == 0 {
					return runnerv1.Result_RESULT_SUCCESS, nil
				}
				return runnerv1.Result_RESULT_FAILURE, nil
			}
		}
	}

	return runnerv1.Result_RESULT_FAILURE, fmt.Errorf("runner container status not found")
}

func waitForPod(ctx context.Context, client kubernetes.Interface, namespace, jobName string, poll time.Duration) (string, error) {
	labelSelector := fmt.Sprintf("job-name=%s", jobName)
	for {
		pods, err := client.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
			LabelSelector: labelSelector,
		})
		if err != nil {
			return "", err
		}
		if len(pods.Items) > 0 {
			return pods.Items[0].Name, nil
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(poll):
		}
	}
}

// waitForContainerRunning waits for a specific container (not init container) to start.
func waitForContainerRunning(ctx context.Context, client kubernetes.Interface, namespace, podName, containerName string, poll time.Duration) error {
	for {
		pod, err := client.CoreV1().Pods(namespace).Get(ctx, podName, metav1.GetOptions{})
		if err != nil {
			return err
		}

		if pod.Status.Phase == corev1.PodFailed {
			return fmt.Errorf("pod failed: %s", pod.Status.Reason)
		}

		for _, cs := range pod.Status.ContainerStatuses {
			if cs.Name == containerName {
				if cs.State.Running != nil || cs.State.Terminated != nil {
					return nil
				}
				if cs.State.Waiting != nil {
					reason := cs.State.Waiting.Reason
					switch reason {
					case "ImagePullBackOff", "ErrImagePull", "InvalidImageName", "CreateContainerConfigError":
						return fmt.Errorf("%s: %s", reason, cs.State.Waiting.Message)
					}
				}
			}
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(poll):
		}
	}
}
