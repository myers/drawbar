package k8s

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strconv"
	"strings"
	"time"

	runnerv1 "code.gitea.io/actions-proto-go/runner/v1"
	"github.com/myers/drawbar/pkg/reporter"
	"github.com/myers/drawbar/pkg/types"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"
)

// PodExecutor opens a streaming exec session against a pod container. The
// returned reader streams stdout from the command. The caller MUST Close it.
type PodExecutor interface {
	ExecStream(ctx context.Context, namespace, pod, container string, cmd []string) (io.ReadCloser, error)
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

func (s *SPDYExecutor) ExecStream(ctx context.Context, namespace, pod, container string, cmd []string) (io.ReadCloser, error) {
	return execStream(ctx, s.Client, s.RestCfg, namespace, pod, container, cmd)
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

	// Stream logs from the runner container.
	logDone := make(chan error, 1)
	go func() {
		logDone <- streamLogs(ctx, streamer, namespace, podName, "runner", rep, cfg.CommandProc)
	}()

	// Stream step-state events via the entrypoint tail subcommand.
	type streamResult struct {
		offset int
		err    error
	}
	streamCtx, cancelStream := context.WithCancel(ctx)
	defer cancelStream()
	stateDone := make(chan streamResult, 1)
	go func() {
		off, sErr := streamStateFileWith(streamCtx, executor, namespace, podName, rep)
		stateDone <- streamResult{offset: off, err: sErr}
	}()

	// Wait for log streaming to finish (container exits).
	<-logDone

	// Stop the live state streamer and pick up its final offset.
	cancelStream()
	res := <-stateDone

	// Authoritatively drain anything written after the last streamed line.
	drainStateFile(ctx, executor, namespace, podName, rep, res.offset)

	// Determine result from container exit code.
	result, err := getContainerResult(ctx, client, namespace, podName)
	if err != nil {
		return runnerv1.Result_RESULT_FAILURE, err
	}

	return result, nil
}


// streamStateFileWith maintains a long-lived `entrypoint tail` exec session
// against /shim/state.jsonl and routes each newline-terminated state event
// into rep. On transient stream failure it reconnects with --skip <lastOffset>
// so events are never replayed. After maxStreamAttempts failures in a row it
// returns. The returned offset is the number of newline-terminated lines
// successfully processed (whether routed or skipped as malformed).
func streamStateFileWith(ctx context.Context, executor PodExecutor, namespace, podName string, rep *reporter.Reporter) (int, error) {
	const (
		initialBackoff = 50 * time.Millisecond
		maxBackoff     = 2 * time.Second
		maxAttempts    = 5
	)

	lastOffset := 0
	backoff := initialBackoff
	attempt := 0

	for {
		if err := ctx.Err(); err != nil {
			return lastOffset, err
		}

		cmd := []string{"/shim/entrypoint", "tail",
			"--skip", strconv.Itoa(lastOffset),
			"/shim/state.jsonl"}
		stream, err := executor.ExecStream(ctx, namespace, podName, "runner", cmd)
		if err != nil {
			if next, stop := backoffStep(ctx, &attempt, &backoff, maxAttempts, maxBackoff); stop {
				slog.Error("state stream gave up after retries",
					"lastOffset", lastOffset, "err", err)
				return lastOffset, err
			} else if next != nil {
				return lastOffset, next
			}
			continue
		}

		// Connect succeeded. The attempt counter is reset only after we
		// observe at least one full line — otherwise an immediate-EOF cycle
		// would bypass the maxAttempts cap and busy-reconnect forever.
		linesRead := 0
		reader := bufio.NewReader(stream)
		for {
			line, rErr := reader.ReadString('\n')
			if rErr == nil && len(line) > 0 {
				if linesRead == 0 {
					attempt = 0
					backoff = initialBackoff
				}
				linesRead++
				trimmed := strings.TrimRight(line, "\n\r")
				if trimmed == "" {
					lastOffset++
					continue
				}
				var ev types.StateEvent
				if jErr := json.Unmarshal([]byte(trimmed), &ev); jErr != nil {
					slog.Debug("skipping malformed state event",
						"line", trimmed, "err", jErr)
					lastOffset++
					continue
				}
				routeStateEvent(ev, rep)
				lastOffset++
				continue
			}
			stream.Close()
			if errors.Is(rErr, context.Canceled) ||
				errors.Is(rErr, context.DeadlineExceeded) {
				return lastOffset, rErr
			}
			break
		}

		// If the stream produced no lines, treat it like a failed connect:
		// rate-limit reconnects so we don't hammer the API during the
		// startup race where state.jsonl doesn't yet exist.
		if linesRead == 0 {
			if next, stop := backoffStep(ctx, &attempt, &backoff, maxAttempts, maxBackoff); stop {
				slog.Error("state stream produced no lines after retries",
					"lastOffset", lastOffset)
				return lastOffset, fmt.Errorf("state stream produced no lines after %d attempts", maxAttempts)
			} else if next != nil {
				return lastOffset, next
			}
		}
	}
}

// backoffStep applies one step of bounded exponential backoff against attempt
// and *backoff. Returns (nil, true) when maxAttempts is reached, (ctxErr,
// false) on ctx-cancel during the sleep, (nil, false) otherwise. Mutates
// *attempt and *backoff in place.
func backoffStep(ctx context.Context, attempt *int, backoff *time.Duration, maxAttempts int, maxBackoff time.Duration) (error, bool) {
	*attempt++
	if *attempt >= maxAttempts {
		return nil, true
	}
	select {
	case <-ctx.Done():
		return ctx.Err(), false
	case <-time.After(*backoff):
	}
	if *backoff < maxBackoff {
		*backoff *= 2
		if *backoff > maxBackoff {
			*backoff = maxBackoff
		}
	}
	return nil, false
}

// drainStateFile performs a single best-effort one-shot read of any state
// events written to /shim/state.jsonl after the streaming tail's last
// observed line. Errors are logged at warn and not returned: by the time we
// drain, the runner container may already be terminated.
func drainStateFile(ctx context.Context, executor PodExecutor, namespace, podName string, rep *reporter.Reporter, lastOffset int) {
	if err := ctx.Err(); err != nil {
		slog.Warn("post-exit state drain skipped: ctx already canceled",
			"err", err, "lastOffset", lastOffset)
		return
	}

	drainCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	cmd := []string{"/shim/entrypoint", "tail", "--once",
		"--skip", strconv.Itoa(lastOffset),
		"/shim/state.jsonl"}
	stream, err := executor.ExecStream(drainCtx, namespace, podName, "runner", cmd)
	if err != nil {
		slog.Warn("post-exit state drain exec failed",
			"err", err, "lastOffset", lastOffset, "pod", podName)
		return
	}
	defer stream.Close()

	reader := bufio.NewReader(stream)
	for {
		line, rErr := reader.ReadString('\n')
		if rErr == nil && len(line) > 0 {
			trimmed := strings.TrimRight(line, "\n\r")
			if trimmed == "" {
				continue
			}
			var ev types.StateEvent
			if jErr := json.Unmarshal([]byte(trimmed), &ev); jErr != nil {
				slog.Debug("skipping malformed state event",
					"line", trimmed, "err", jErr)
				continue
			}
			routeStateEvent(ev, rep)
			continue
		}
		if rErr != io.EOF {
			slog.Warn("post-exit state drain stream error",
				"err", rErr, "lastOffset", lastOffset)
		}
		return
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

// execStream runs a command in a running container and returns a reader that
// streams stdout. The command keeps running until it exits or the caller
// closes the reader. Stderr is discarded.
func execStream(ctx context.Context, client kubernetes.Interface, restCfg *rest.Config, namespace, podName, container string, cmd []string) (io.ReadCloser, error) {
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
		return nil, fmt.Errorf("creating SPDY executor: %w", err)
	}

	pr, pw := io.Pipe()
	go func() {
		err := exec.StreamWithContext(ctx, remotecommand.StreamOptions{
			Stdout: pw,
		})
		// Close the writer so the reader sees EOF (or the error).
		pw.CloseWithError(err)
	}()
	return pr, nil
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
			return fmt.Errorf("pod failed: %s", formatPodFailure(ctx, client, namespace, pod))
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

// formatPodFailure produces a diagnostic string for a Phase=Failed pod.
// pod.Status.Reason alone is frequently empty (it's only set for evictions,
// preemption, deadline-exceeded, etc.); the actual cause lives in the per-
// container statuses. We walk init containers first (since failures during
// pod startup almost always happen there) and then regular containers, and
// surface the first one that terminated non-zero — falling back to the
// top-level Reason when nothing more specific is available.
func formatPodFailure(ctx context.Context, client kubernetes.Interface, namespace string, pod *corev1.Pod) string {
	if cs, kind := findFailingContainer(pod); cs != nil {
		summary := fmt.Sprintf("%s container %s terminated with exit code %d (%s)",
			kind, cs.name, cs.exitCode, cs.reason)
		if cs.message != "" {
			summary += ": " + truncate(strings.TrimSpace(cs.message), 1024)
		}
		if tail := fetchPreviousLogTail(ctx, client, namespace, pod.Name, cs.name, 1024); tail != "" {
			summary += "\nlast log lines:\n" + tail
		}
		return summary
	}
	if pod.Status.Reason != "" {
		return pod.Status.Reason
	}
	return "unknown reason"
}

type failingContainer struct {
	name     string
	exitCode int32
	reason   string
	message  string
}

// findFailingContainer returns the first init or main container that
// terminated non-zero (checking current State first, then LastTerminationState
// for sidecars in CrashLoopBackOff). The kind string is "init" or "main".
func findFailingContainer(pod *corev1.Pod) (*failingContainer, string) {
	pick := func(statuses []corev1.ContainerStatus) *failingContainer {
		for _, cs := range statuses {
			if t := cs.State.Terminated; t != nil && t.ExitCode != 0 {
				return &failingContainer{cs.Name, t.ExitCode, t.Reason, t.Message}
			}
			if t := cs.LastTerminationState.Terminated; t != nil && t.ExitCode != 0 {
				return &failingContainer{cs.Name, t.ExitCode, t.Reason, t.Message}
			}
		}
		return nil
	}
	if c := pick(pod.Status.InitContainerStatuses); c != nil {
		return c, "init"
	}
	if c := pick(pod.Status.ContainerStatuses); c != nil {
		return c, "main"
	}
	return nil, ""
}

// fetchPreviousLogTail returns up to maxBytes of the named container's
// previous-instance logs, indented for readability. Best effort: any error
// (no previous instance, fake client, RBAC) returns "" silently — diagnostics
// shouldn't block error reporting.
func fetchPreviousLogTail(ctx context.Context, client kubernetes.Interface, namespace, pod, container string, maxBytes int64) string {
	stream, err := client.CoreV1().Pods(namespace).GetLogs(pod, &corev1.PodLogOptions{
		Container:  container,
		Previous:   true,
		LimitBytes: &maxBytes,
	}).Stream(ctx)
	if err != nil {
		return ""
	}
	defer stream.Close()
	buf, err := io.ReadAll(stream)
	if err != nil || len(buf) == 0 {
		return ""
	}
	lines := strings.Split(strings.TrimRight(string(buf), "\n"), "\n")
	for i, line := range lines {
		lines[i] = "  " + line
	}
	return strings.Join(lines, "\n")
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}
