package k8s

import (
	"context"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	runnerv1 "code.gitea.io/actions-proto-go/runner/v1"
	"github.com/myers/drawbar/pkg/reporter"
	"github.com/myers/drawbar/pkg/types"
	"connectrpc.com/connect"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

// mockPodExecutor implements PodExecutor for testing.
type mockPodExecutor struct {
	mu      sync.Mutex
	outputs []string // sequential outputs for each ExecStream call
	errs    []error
	idx     int
}

func (m *mockPodExecutor) ExecStream(_ context.Context, _, _, _ string, _ []string) (io.ReadCloser, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	i := m.idx
	m.idx++
	if i < len(m.errs) && m.errs[i] != nil {
		return nil, m.errs[i]
	}
	if i < len(m.outputs) {
		return io.NopCloser(strings.NewReader(m.outputs[i])), nil
	}
	return nil, fmt.Errorf("terminated")
}

// mockLogStreamer implements LogStreamer for testing.
type mockLogStreamer struct {
	content string
	err     error
}

func (m *mockLogStreamer) StreamLogs(_ context.Context, _, _, _ string) (io.ReadCloser, error) {
	if m.err != nil {
		return nil, m.err
	}
	return io.NopCloser(strings.NewReader(m.content)), nil
}

type noopClient struct{}

func (n *noopClient) UpdateLog(_ context.Context, _ *connect.Request[runnerv1.UpdateLogRequest]) (*connect.Response[runnerv1.UpdateLogResponse], error) {
	return connect.NewResponse(&runnerv1.UpdateLogResponse{AckIndex: 9999}), nil
}

func (n *noopClient) UpdateTask(_ context.Context, _ *connect.Request[runnerv1.UpdateTaskRequest]) (*connect.Response[runnerv1.UpdateTaskResponse], error) {
	return connect.NewResponse(&runnerv1.UpdateTaskResponse{}), nil
}

func newTestReporter(taskID int64, numSteps int) *reporter.Reporter {
	return reporter.New(&noopClient{}, taskID, numSteps, time.Hour)
}

func TestWaitForPod_Found(t *testing.T) {
	client := fake.NewSimpleClientset()
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-pod",
			Namespace: "default",
			Labels:    map[string]string{"job-name": "test-job"},
		},
	}
	_, err := client.CoreV1().Pods("default").Create(context.Background(), pod, metav1.CreateOptions{})
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	name, err := waitForPod(ctx, client, "default", "test-job", 10*time.Millisecond)
	require.NoError(t, err)
	assert.Equal(t, "test-pod", name)
}

func TestWaitForPod_Timeout(t *testing.T) {
	client := fake.NewSimpleClientset()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	_, err := waitForPod(ctx, client, "default", "nonexistent", 10*time.Millisecond)
	assert.Error(t, err)
}

func TestWaitForContainerRunning(t *testing.T) {
	client := fake.NewSimpleClientset()
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "pod1", Namespace: "default"},
		Status: corev1.PodStatus{
			ContainerStatuses: []corev1.ContainerStatus{
				{Name: "runner", State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}},
			},
		},
	}
	_, err := client.CoreV1().Pods("default").Create(context.Background(), pod, metav1.CreateOptions{})
	require.NoError(t, err)

	err = waitForContainerRunning(context.Background(), client, "default", "pod1", "runner", 10*time.Millisecond)
	assert.NoError(t, err)
}

func TestWaitForContainerRunning_ImagePullBackOff(t *testing.T) {
	client := fake.NewSimpleClientset()
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "pod1", Namespace: "default"},
		Status: corev1.PodStatus{
			ContainerStatuses: []corev1.ContainerStatus{
				{
					Name: "runner",
					State: corev1.ContainerState{
						Waiting: &corev1.ContainerStateWaiting{Reason: "ImagePullBackOff", Message: "back-off"},
					},
				},
			},
		},
	}
	_, err := client.CoreV1().Pods("default").Create(context.Background(), pod, metav1.CreateOptions{})
	require.NoError(t, err)

	err = waitForContainerRunning(context.Background(), client, "default", "pod1", "runner", 10*time.Millisecond)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "ImagePullBackOff")
}

func TestWaitForContainerRunning_PodFailed(t *testing.T) {
	client := fake.NewSimpleClientset()
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "pod1", Namespace: "default"},
		Status:     corev1.PodStatus{Phase: corev1.PodFailed, Reason: "Evicted"},
	}
	_, err := client.CoreV1().Pods("default").Create(context.Background(), pod, metav1.CreateOptions{})
	require.NoError(t, err)

	err = waitForContainerRunning(context.Background(), client, "default", "pod1", "runner", 10*time.Millisecond)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "pod failed")
	assert.Contains(t, err.Error(), "Evicted")
}

func TestWaitForContainerRunning_PodFailed_InitContainerTerminated(t *testing.T) {
	// When an init container terminates non-zero and pod-level Reason is
	// empty, the error must still name the failing container, exit code,
	// and termination reason — that's the diagnostic info the CI UI gets.
	client := fake.NewSimpleClientset()
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "pod1", Namespace: "default"},
		Status: corev1.PodStatus{
			Phase: corev1.PodFailed,
			InitContainerStatuses: []corev1.ContainerStatus{
				{
					Name: "svc-buildkit",
					State: corev1.ContainerState{
						Terminated: &corev1.ContainerStateTerminated{
							ExitCode: 1,
							Reason:   "Error",
						},
					},
				},
			},
		},
	}
	_, err := client.CoreV1().Pods("default").Create(context.Background(), pod, metav1.CreateOptions{})
	require.NoError(t, err)

	err = waitForContainerRunning(context.Background(), client, "default", "pod1", "runner", 10*time.Millisecond)
	require.Error(t, err)
	msg := err.Error()
	assert.Contains(t, msg, "pod failed")
	assert.Contains(t, msg, "init container svc-buildkit")
	assert.Contains(t, msg, "exit code 1")
	assert.Contains(t, msg, "Error")
}

func TestWaitForContainerRunning_PodFailed_SidecarCrashLoopBackOff(t *testing.T) {
	// A sidecar init container in CrashLoopBackOff has its terminal state
	// in LastTerminationState, not State (current State is Waiting). The
	// error must surface the previous-termination reason and exit code so
	// users can diagnose without kubectl.
	client := fake.NewSimpleClientset()
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "pod1", Namespace: "default"},
		Status: corev1.PodStatus{
			Phase: corev1.PodFailed,
			InitContainerStatuses: []corev1.ContainerStatus{
				{
					Name: "svc-buildkit",
					State: corev1.ContainerState{
						Waiting: &corev1.ContainerStateWaiting{
							Reason: "CrashLoopBackOff",
						},
					},
					LastTerminationState: corev1.ContainerState{
						Terminated: &corev1.ContainerStateTerminated{
							ExitCode: 1,
							Reason:   "Error",
						},
					},
				},
			},
		},
	}
	_, err := client.CoreV1().Pods("default").Create(context.Background(), pod, metav1.CreateOptions{})
	require.NoError(t, err)

	err = waitForContainerRunning(context.Background(), client, "default", "pod1", "runner", 10*time.Millisecond)
	require.Error(t, err)
	msg := err.Error()
	assert.Contains(t, msg, "init container svc-buildkit")
	assert.Contains(t, msg, "exit code 1")
}

func TestWaitForContainerRunning_PodFailed_PrefersFailingInitOverHealthyOnes(t *testing.T) {
	// With multiple init containers, only the failing one should be named.
	// (We pick the first non-zero / waiting-with-LastTerminationState entry.)
	client := fake.NewSimpleClientset()
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "pod1", Namespace: "default"},
		Status: corev1.PodStatus{
			Phase: corev1.PodFailed,
			InitContainerStatuses: []corev1.ContainerStatus{
				{
					Name: "svc-postgres",
					State: corev1.ContainerState{
						Terminated: &corev1.ContainerStateTerminated{ExitCode: 0, Reason: "Completed"},
					},
				},
				{
					Name: "setup-shim",
					State: corev1.ContainerState{
						Terminated: &corev1.ContainerStateTerminated{ExitCode: 2, Reason: "Error"},
					},
				},
			},
		},
	}
	_, err := client.CoreV1().Pods("default").Create(context.Background(), pod, metav1.CreateOptions{})
	require.NoError(t, err)

	err = waitForContainerRunning(context.Background(), client, "default", "pod1", "runner", 10*time.Millisecond)
	require.Error(t, err)
	msg := err.Error()
	assert.Contains(t, msg, "setup-shim")
	assert.Contains(t, msg, "exit code 2")
	assert.NotContains(t, msg, "svc-postgres")
}

func TestGetContainerResult_Success(t *testing.T) {
	client := fake.NewSimpleClientset()
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "pod1", Namespace: "default"},
		Status: corev1.PodStatus{
			ContainerStatuses: []corev1.ContainerStatus{
				{Name: "runner", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}}},
			},
		},
	}
	_, err := client.CoreV1().Pods("default").Create(context.Background(), pod, metav1.CreateOptions{})
	require.NoError(t, err)

	result, err := getContainerResult(context.Background(), client, "default", "pod1")
	require.NoError(t, err)
	assert.Equal(t, runnerv1.Result_RESULT_SUCCESS, result)
}

func TestGetContainerResult_Failure(t *testing.T) {
	client := fake.NewSimpleClientset()
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "pod1", Namespace: "default"},
		Status: corev1.PodStatus{
			ContainerStatuses: []corev1.ContainerStatus{
				{Name: "runner", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 1}}},
			},
		},
	}
	_, err := client.CoreV1().Pods("default").Create(context.Background(), pod, metav1.CreateOptions{})
	require.NoError(t, err)

	result, err := getContainerResult(context.Background(), client, "default", "pod1")
	require.NoError(t, err)
	assert.Equal(t, runnerv1.Result_RESULT_FAILURE, result)
}

func TestGetContainerResult_NoRunnerContainer(t *testing.T) {
	client := fake.NewSimpleClientset()
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "pod1", Namespace: "default"},
		Status: corev1.PodStatus{
			ContainerStatuses: []corev1.ContainerStatus{
				{Name: "other-container", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}}},
			},
		},
	}
	_, err := client.CoreV1().Pods("default").Create(context.Background(), pod, metav1.CreateOptions{})
	require.NoError(t, err)

	result, err := getContainerResult(context.Background(), client, "default", "pod1")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "not found")
	assert.Equal(t, runnerv1.Result_RESULT_FAILURE, result)
}

func TestWaitForContainerRunning_AlreadyTerminated(t *testing.T) {
	client := fake.NewSimpleClientset()
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "pod1", Namespace: "default"},
		Status: corev1.PodStatus{
			ContainerStatuses: []corev1.ContainerStatus{
				{Name: "runner", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}}},
			},
		},
	}
	_, err := client.CoreV1().Pods("default").Create(context.Background(), pod, metav1.CreateOptions{})
	require.NoError(t, err)

	// Already terminated should return nil (not block).
	err = waitForContainerRunning(context.Background(), client, "default", "pod1", "runner", 10*time.Millisecond)
	assert.NoError(t, err)
}


func TestRouteStateEvent_Start(t *testing.T) {
	rep := newTestReporter(1, 2)
	routeStateEvent(types.StateEvent{Event: "start", Step: 0, Name: "Build"}, rep)
	// No panic, reporter step started.
}

func TestRouteStateEvent_End_Success(t *testing.T) {
	rep := newTestReporter(1, 2)
	rep.StartStep(0)
	routeStateEvent(types.StateEvent{Event: "end", Step: 0, Name: "Build", ExitCode: 0}, rep)
}

func TestRouteStateEvent_End_Failure(t *testing.T) {
	rep := newTestReporter(1, 2)
	rep.StartStep(0)
	routeStateEvent(types.StateEvent{Event: "end", Step: 0, Name: "Build", ExitCode: 1}, rep)
}


// --- streamLogs ---

func TestStreamLogs(t *testing.T) {
	streamer := &mockLogStreamer{content: "line1\nline2\nline3\n"}

	mc := &trackingClient{}
	rep := reporter.New(mc, 1, 1, time.Hour)
	rep.StartStep(0)

	err := streamLogs(context.Background(), streamer, "ns", "pod", "runner", rep, nil)
	assert.NoError(t, err)

	// Flush to capture logs.
	rep.Flush(context.Background())

	mc.mu.Lock()
	defer mc.mu.Unlock()
	require.NotEmpty(t, mc.logCalls)
	// Should have 3 log lines.
	totalRows := 0
	for _, call := range mc.logCalls {
		totalRows += len(call.Rows)
	}
	assert.Equal(t, 3, totalRows)
}

func TestStreamLogs_Error(t *testing.T) {
	streamer := &mockLogStreamer{err: fmt.Errorf("connection refused")}
	rep := newTestReporter(1, 1)

	err := streamLogs(context.Background(), streamer, "ns", "pod", "runner", rep, nil)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "connection refused")
}

// trackingClient records UpdateLog/UpdateTask calls for assertion.
type trackingClient struct {
	mu        sync.Mutex
	logCalls  []*runnerv1.UpdateLogRequest
	taskCalls []*runnerv1.UpdateTaskRequest
}

func (c *trackingClient) UpdateLog(_ context.Context, req *connect.Request[runnerv1.UpdateLogRequest]) (*connect.Response[runnerv1.UpdateLogResponse], error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.logCalls = append(c.logCalls, req.Msg)
	ack := req.Msg.Index + int64(len(req.Msg.Rows))
	return connect.NewResponse(&runnerv1.UpdateLogResponse{AckIndex: ack}), nil
}

func (c *trackingClient) UpdateTask(_ context.Context, _ *connect.Request[runnerv1.UpdateTaskRequest]) (*connect.Response[runnerv1.UpdateTaskResponse], error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.taskCalls = append(c.taskCalls, nil)
	return connect.NewResponse(&runnerv1.UpdateTaskResponse{}), nil
}

// --- watchJobWith ---

func TestWatchJobWith_Success(t *testing.T) {
	client := fake.NewSimpleClientset()
	ns := "default"
	jobName := "test-job"

	// Pre-create pod with terminated runner container (exit 0).
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-pod", Namespace: ns,
			Labels: map[string]string{"job-name": jobName},
		},
		Status: corev1.PodStatus{
			ContainerStatuses: []corev1.ContainerStatus{{
				Name:  "runner",
				State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}},
			}},
		},
	}
	_, err := client.CoreV1().Pods(ns).Create(context.Background(), pod, metav1.CreateOptions{})
	require.NoError(t, err)

	executor := &mockPodExecutor{
		outputs: []string{
			`{"event":"start","step":0,"name":"Build","exit_code":0,"time":"t1"}`,
			`{"event":"start","step":0,"name":"Build","exit_code":0,"time":"t1"}
{"event":"end","step":0,"name":"Build","exit_code":0,"time":"t2"}`,
		},
	}
	logStreamer := &mockLogStreamer{content: "build output\n"}
	rep := newTestReporter(1, 1)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	result, err := watchJobWith(ctx, client, executor, logStreamer, ns, jobName, rep, WatchConfig{PollInterval: 20 * time.Millisecond})
	require.NoError(t, err)
	assert.Equal(t, runnerv1.Result_RESULT_SUCCESS, result)
}

func TestWatchJobWith_Failure(t *testing.T) {
	client := fake.NewSimpleClientset()
	ns := "default"
	jobName := "fail-job"

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "fail-pod", Namespace: ns,
			Labels: map[string]string{"job-name": jobName},
		},
		Status: corev1.PodStatus{
			ContainerStatuses: []corev1.ContainerStatus{{
				Name:  "runner",
				State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 1}},
			}},
		},
	}
	_, err := client.CoreV1().Pods(ns).Create(context.Background(), pod, metav1.CreateOptions{})
	require.NoError(t, err)

	executor := &mockPodExecutor{errs: []error{fmt.Errorf("terminated")}}
	logStreamer := &mockLogStreamer{content: "error output\n"}
	rep := newTestReporter(1, 1)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	result, err := watchJobWith(ctx, client, executor, logStreamer, ns, jobName, rep, WatchConfig{PollInterval: 20 * time.Millisecond})
	require.NoError(t, err)
	assert.Equal(t, runnerv1.Result_RESULT_FAILURE, result)
}

func TestDefaultWatchConfig(t *testing.T) {
	cfg := DefaultWatchConfig()
	assert.Equal(t, 500*time.Millisecond, cfg.PollInterval)
	assert.Nil(t, cfg.Executor)
	assert.Nil(t, cfg.Streamer)
}

func TestWatchJob_UsesConfigExecutor(t *testing.T) {
	client := fake.NewSimpleClientset()
	ns := "default"
	jobName := "cfg-job"

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "cfg-pod", Namespace: ns,
			Labels: map[string]string{"job-name": jobName},
		},
		Status: corev1.PodStatus{
			ContainerStatuses: []corev1.ContainerStatus{{
				Name:  "runner",
				State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}},
			}},
		},
	}
	_, err := client.CoreV1().Pods(ns).Create(context.Background(), pod, metav1.CreateOptions{})
	require.NoError(t, err)

	executor := &mockPodExecutor{errs: []error{fmt.Errorf("terminated")}}
	logStreamer := &mockLogStreamer{content: ""}
	rep := newTestReporter(1, 0)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	// WatchJob (the public function) should use Executor/Streamer from config.
	result, err := WatchJob(ctx, client, nil, ns, jobName, rep, WatchConfig{
		PollInterval: 20 * time.Millisecond,
		Executor:     executor,
		Streamer:     logStreamer,
	})
	require.NoError(t, err)
	assert.Equal(t, runnerv1.Result_RESULT_SUCCESS, result)
}

func TestWaitForContainerRunning_ErrImagePull(t *testing.T) {
	client := fake.NewSimpleClientset()
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "pod1", Namespace: "default"},
		Status: corev1.PodStatus{
			ContainerStatuses: []corev1.ContainerStatus{
				{
					Name: "runner",
					State: corev1.ContainerState{
						Waiting: &corev1.ContainerStateWaiting{Reason: "ErrImagePull", Message: "image not found"},
					},
				},
			},
		},
	}
	_, err := client.CoreV1().Pods("default").Create(context.Background(), pod, metav1.CreateOptions{})
	require.NoError(t, err)

	err = waitForContainerRunning(context.Background(), client, "default", "pod1", "runner", 10*time.Millisecond)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "ErrImagePull")
}

// --- streamStateFileWith and drainStateFile ---

// findFlagValue scans cmd for `flag` and returns the next argument's value.
// Returns "" if not found.
func findFlagValue(cmd []string, flag string) string {
	for i := 0; i < len(cmd)-1; i++ {
		if cmd[i] == flag {
			return cmd[i+1]
		}
	}
	return ""
}

// recordingExecutor records every ExecStream invocation and serves outputs
// from a programmable script.
type recordingExecutor struct {
	mu      sync.Mutex
	calls   [][]string
	outputs []string
	errs    []error
	idx     int
}

func (r *recordingExecutor) ExecStream(_ context.Context, _, _, _ string, cmd []string) (io.ReadCloser, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, append([]string(nil), cmd...))
	i := r.idx
	r.idx++
	if i < len(r.errs) && r.errs[i] != nil {
		return nil, r.errs[i]
	}
	if i < len(r.outputs) {
		return io.NopCloser(strings.NewReader(r.outputs[i])), nil
	}
	return nil, fmt.Errorf("terminated")
}

func (r *recordingExecutor) callCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.calls)
}

func (r *recordingExecutor) callAt(i int) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls[i]
}

func TestStreamStateFile_RoutesAllEvents(t *testing.T) {
	rep := newTestReporter(1, 3)
	jsonl := `{"event":"start","step":0,"name":"checkout","time":"2026-05-06T14:35:54Z"}
{"event":"end","step":0,"name":"checkout","exit_code":0,"time":"2026-05-06T14:35:58Z"}
{"event":"start","step":1,"name":"build","time":"2026-05-06T14:35:58Z"}
`
	exec := &recordingExecutor{outputs: []string{jsonl}}

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		// Once exec returns the stream EOF, the function will try to reconnect.
		// Cancel ctx after a short pause to break it out of the retry loop.
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()

	off, _ := streamStateFileWith(ctx, exec, "ns", "pod", rep)
	if off != 3 {
		t.Errorf("offset = %d, want 3", off)
	}
	// The first call should NOT carry --skip > 0 (skip starts at 0).
	if got := findFlagValue(exec.callAt(0), "--skip"); got != "0" && got != "" {
		t.Errorf("first call --skip = %q, want \"0\" or absent", got)
	}
}

func TestStreamStateFile_ReconnectAfterError(t *testing.T) {
	rep := newTestReporter(1, 5)
	first := `{"event":"start","step":0,"name":"a","time":"t"}
{"event":"end","step":0,"name":"a","exit_code":0,"time":"t"}
{"event":"start","step":1,"name":"b","time":"t"}
`
	// After 3 routed events, EOF triggers reconnect. Second call returns more.
	second := `{"event":"end","step":1,"name":"b","exit_code":0,"time":"t"}
`
	exec := &recordingExecutor{outputs: []string{first, second}}

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(150 * time.Millisecond)
		cancel()
	}()

	off, _ := streamStateFileWith(ctx, exec, "ns", "pod", rep)
	if off != 4 {
		t.Errorf("offset = %d, want 4", off)
	}
	if exec.callCount() < 2 {
		t.Fatalf("expected >= 2 calls, got %d", exec.callCount())
	}
	if got := findFlagValue(exec.callAt(1), "--skip"); got != "3" {
		t.Errorf("second call --skip = %q, want \"3\"", got)
	}
}

func TestStreamStateFile_MalformedLineSkipped(t *testing.T) {
	rep := newTestReporter(1, 2)
	jsonl := `{not json}
{"event":"start","step":0,"name":"a","time":"t"}
`
	exec := &recordingExecutor{outputs: []string{jsonl}}

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()

	off, _ := streamStateFileWith(ctx, exec, "ns", "pod", rep)
	if off != 2 {
		t.Errorf("offset = %d, want 2 (both lines counted)", off)
	}
}

func TestStreamStateFile_MaxRetries(t *testing.T) {
	rep := newTestReporter(1, 0)
	exec := &recordingExecutor{
		errs: []error{
			fmt.Errorf("boom1"),
			fmt.Errorf("boom2"),
			fmt.Errorf("boom3"),
			fmt.Errorf("boom4"),
			fmt.Errorf("boom5"),
		},
	}

	ctx := context.Background()
	off, err := streamStateFileWith(ctx, exec, "ns", "pod", rep)
	if err == nil {
		t.Errorf("expected error after max retries")
	}
	if off != 0 {
		t.Errorf("offset = %d, want 0", off)
	}
	if exec.callCount() != 5 {
		t.Errorf("call count = %d, want 5", exec.callCount())
	}
}

func TestDrainStateFile_RoutesEvents(t *testing.T) {
	rep := newTestReporter(1, 2)
	jsonl := `{"event":"end","step":0,"name":"a","exit_code":0,"time":"t"}
{"event":"end","step":1,"name":"b","exit_code":1,"time":"t"}
`
	exec := &recordingExecutor{outputs: []string{jsonl}}

	drainStateFile(context.Background(), exec, "ns", "pod", rep, 3)

	if exec.callCount() != 1 {
		t.Errorf("call count = %d, want 1", exec.callCount())
	}
	cmd := exec.callAt(0)
	if findFlagValue(cmd, "--skip") != "3" {
		t.Errorf("--skip = %q, want \"3\"", findFlagValue(cmd, "--skip"))
	}
	hasOnce := false
	for _, a := range cmd {
		if a == "--once" {
			hasOnce = true
		}
	}
	if !hasOnce {
		t.Errorf("drain command missing --once flag: %v", cmd)
	}
}

func TestDrainStateFile_TerminatedContainer(t *testing.T) {
	rep := newTestReporter(1, 1)
	exec := &recordingExecutor{errs: []error{fmt.Errorf("container terminated")}}

	// Should not panic, should not block; just log a warning and return.
	drainStateFile(context.Background(), exec, "ns", "pod", rep, 0)
}

func TestStreamStateFile_ImmediateEOFAppliesBackoff(t *testing.T) {
	// When ExecStream succeeds but the returned stream EOFs immediately
	// (e.g. entrypoint tail finds /shim/state.jsonl missing during the
	// startup race and exits 1), the loop must NOT busy-reconnect.
	// Empty outputs (zero lines) trigger this case 5 times in a row;
	// after 5 fruitless attempts the function should return.
	rep := newTestReporter(1, 0)
	exec := &recordingExecutor{
		outputs: []string{"", "", "", "", ""},
	}

	ctx := context.Background()
	off, err := streamStateFileWith(ctx, exec, "ns", "pod", rep)
	if err == nil {
		t.Errorf("expected error after 5 unproductive streams")
	}
	if off != 0 {
		t.Errorf("offset = %d, want 0", off)
	}
	if exec.callCount() != 5 {
		t.Errorf("call count = %d, want 5", exec.callCount())
	}
}

func TestStreamStateFile_ProductiveStreamResetsAttempts(t *testing.T) {
	// A stream that delivers at least one line should reset the attempt
	// counter, so subsequent unproductive cycles get their own retry budget.
	// Sequence: 4 unproductive, 1 productive (1 line), 5 unproductive.
	// Without the gate-on-routed fix, the 4 unproductive at the start would
	// run away. With the fix, attempt accumulates to 4, productive resets it
	// to 0, then the next 5 unproductive trip the cap.
	rep := newTestReporter(1, 1)
	productive := `{"event":"start","step":0,"name":"a","time":"t"}` + "\n"
	exec := &recordingExecutor{
		outputs: []string{
			"", "", "", "",
			productive,
			"", "", "", "", "",
		},
	}

	ctx := context.Background()
	off, err := streamStateFileWith(ctx, exec, "ns", "pod", rep)
	if err == nil {
		t.Errorf("expected error after final unproductive run")
	}
	if off != 1 {
		t.Errorf("offset = %d, want 1 (one productive line)", off)
	}
	if exec.callCount() != 10 {
		t.Errorf("call count = %d, want 10", exec.callCount())
	}
}

// --- waitForContainerTerminated ---

func TestWaitForContainerTerminated_AlreadyTerminated(t *testing.T) {
	client := fake.NewSimpleClientset(&corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "pod1", Namespace: "default"},
		Status: corev1.PodStatus{
			ContainerStatuses: []corev1.ContainerStatus{
				{Name: "runner", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}}},
			},
		},
	})

	err := waitForContainerTerminated(context.Background(), client, "default", "pod1", "runner", time.Second, 10*time.Millisecond)
	assert.NoError(t, err)
}

func TestWaitForContainerTerminated_TransitionsFromRunning(t *testing.T) {
	client := fake.NewSimpleClientset(&corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "pod1", Namespace: "default"},
		Status: corev1.PodStatus{
			ContainerStatuses: []corev1.ContainerStatus{
				{Name: "runner", State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}},
			},
		},
	})

	// After a short delay, flip the pod's runner container to Terminated.
	go func() {
		time.Sleep(40 * time.Millisecond)
		_, _ = client.CoreV1().Pods("default").UpdateStatus(context.Background(), &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "pod1", Namespace: "default"},
			Status: corev1.PodStatus{
				ContainerStatuses: []corev1.ContainerStatus{
					{Name: "runner", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}}},
				},
			},
		}, metav1.UpdateOptions{})
	}()

	err := waitForContainerTerminated(context.Background(), client, "default", "pod1", "runner", time.Second, 10*time.Millisecond)
	assert.NoError(t, err)
}

func TestWaitForContainerTerminated_TimesOut(t *testing.T) {
	client := fake.NewSimpleClientset(&corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "pod1", Namespace: "default"},
		Status: corev1.PodStatus{
			ContainerStatuses: []corev1.ContainerStatus{
				{Name: "runner", State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}},
			},
		},
	})

	// Container stays Running. Helper should return a deadline-exceeded error
	// distinguishable from the parent ctx being cancelled.
	err := waitForContainerTerminated(context.Background(), client, "default", "pod1", "runner", 50*time.Millisecond, 10*time.Millisecond)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "did not terminate")
}

// erroringLogStreamer yields some bytes, then returns a non-EOF error on the
// next read. Models the apiserver-flap scenario: stream opens, delivers some
// log, then the connection drops mid-job.
type erroringLogStreamer struct {
	content string
	readErr error
}

type errReader struct {
	rest io.Reader
	err  error
}

func (r *errReader) Read(p []byte) (int, error) {
	n, err := r.rest.Read(p)
	if err == io.EOF {
		// Substitute the configured non-EOF error.
		return n, r.err
	}
	return n, err
}

func (r *errReader) Close() error { return nil }

func (s *erroringLogStreamer) StreamLogs(_ context.Context, _, _, _ string) (io.ReadCloser, error) {
	return &errReader{rest: strings.NewReader(s.content), err: s.readErr}, nil
}

// TestWatchJobWith_LogStreamErrorWaitsForTermination simulates Finding A's
// scenario: the log stream breaks mid-job (non-EOF error) while the runner
// container is still Running. The function must wait for the container to
// terminate before reading exit code, not immediately fall through to
// "runner container status not found" and report a healthy job as failed.
func TestWatchJobWith_LogStreamErrorWaitsForTermination(t *testing.T) {
	client := fake.NewSimpleClientset()
	ns := "default"
	jobName := "flap-job"

	// Pod starts with the runner container Running, not Terminated.
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "flap-pod", Namespace: ns,
			Labels: map[string]string{"job-name": jobName},
		},
		Status: corev1.PodStatus{
			ContainerStatuses: []corev1.ContainerStatus{{
				Name:  "runner",
				State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
			}},
		},
	}
	_, err := client.CoreV1().Pods(ns).Create(context.Background(), pod, metav1.CreateOptions{})
	require.NoError(t, err)

	// After log error fires, transition the container to Terminated(ExitCode=0).
	go func() {
		time.Sleep(80 * time.Millisecond)
		_, _ = client.CoreV1().Pods(ns).UpdateStatus(context.Background(), &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "flap-pod", Namespace: ns,
				Labels: map[string]string{"job-name": jobName}},
			Status: corev1.PodStatus{
				ContainerStatuses: []corev1.ContainerStatus{{
					Name:  "runner",
					State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}},
				}},
			},
		}, metav1.UpdateOptions{})
	}()

	executor := &mockPodExecutor{errs: []error{fmt.Errorf("terminated")}}
	logStreamer := &erroringLogStreamer{
		content: "build output\n",
		readErr: fmt.Errorf("connection reset by peer"),
	}
	rep := newTestReporter(1, 1)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	result, err := watchJobWith(ctx, client, executor, logStreamer, ns, jobName, rep, WatchConfig{PollInterval: 20 * time.Millisecond})
	require.NoError(t, err)
	assert.Equal(t, runnerv1.Result_RESULT_SUCCESS, result)
}

// TestWatchJobWith_EOFSkipsWait locks in the EOF path: when streamLogs
// returns nil (clean EOF), watchJobWith must NOT route through
// waitForContainerTerminated. The pod here stays Running with no Terminated
// state — if the wait branch were entered we'd see the wrap error
// "waiting for container after log stream failure"; instead we expect the
// existing "runner container status not found" from getContainerResult.
func TestWatchJobWith_EOFSkipsWait(t *testing.T) {
	client := fake.NewSimpleClientset()
	ns := "default"
	jobName := "eof-job"

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "eof-pod", Namespace: ns,
			Labels: map[string]string{"job-name": jobName},
		},
		Status: corev1.PodStatus{
			ContainerStatuses: []corev1.ContainerStatus{{
				Name:  "runner",
				State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
			}},
		},
	}
	_, err := client.CoreV1().Pods(ns).Create(context.Background(), pod, metav1.CreateOptions{})
	require.NoError(t, err)

	executor := &mockPodExecutor{errs: []error{fmt.Errorf("terminated")}}
	logStreamer := &mockLogStreamer{content: "build output\n"} // returns clean EOF
	rep := newTestReporter(1, 1)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	// LogErrorTerminationTimeout is set short so a regression (wait branch
	// incorrectly entered on EOF) surfaces the wrap-error message in well
	// under the 2s parent ctx, instead of racing the ctx deadline.
	_, err = watchJobWith(ctx, client, executor, logStreamer, ns, jobName, rep,
		WatchConfig{
			PollInterval:               20 * time.Millisecond,
			LogErrorTerminationTimeout: 100 * time.Millisecond,
		})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "runner container status not found")
	assert.NotContains(t, err.Error(), "waiting for container after log stream failure")
}

func TestWaitForContainerTerminated_PodNotFound(t *testing.T) {
	// Empty fake clientset — the pod doesn't exist. The helper should treat
	// NotFound as terminal and return nil so getContainerResult's existing
	// diagnostic surfaces, rather than timing out with "did not terminate".
	client := fake.NewSimpleClientset()

	err := waitForContainerTerminated(context.Background(), client, "default", "ghost-pod", "runner",
		200*time.Millisecond, 10*time.Millisecond)
	assert.NoError(t, err)
}

func TestWaitForContainerTerminated_PodPhaseFailed(t *testing.T) {
	// Pod evicted: Phase=Failed but the runner container's State is still
	// Running (eviction races the container state). The helper should treat
	// Phase=Failed as terminal — the pod is dead and won't update further.
	client := fake.NewSimpleClientset(&corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "pod1", Namespace: "default"},
		Status: corev1.PodStatus{
			Phase: corev1.PodFailed,
			ContainerStatuses: []corev1.ContainerStatus{
				{Name: "runner", State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}},
			},
		},
	})

	err := waitForContainerTerminated(context.Background(), client, "default", "pod1", "runner",
		200*time.Millisecond, 10*time.Millisecond)
	assert.NoError(t, err)
}

func TestWaitForContainerTerminated_PodPhaseSucceeded(t *testing.T) {
	// Symmetric to the Failed test: Phase=Succeeded with runner still
	// reported as Running. Helper should treat the pod as terminal rather
	// than spin to timeout.
	client := fake.NewSimpleClientset(&corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "pod1", Namespace: "default"},
		Status: corev1.PodStatus{
			Phase: corev1.PodSucceeded,
			ContainerStatuses: []corev1.ContainerStatus{
				{Name: "runner", State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}},
			},
		},
	})

	err := waitForContainerTerminated(context.Background(), client, "default", "pod1", "runner",
		200*time.Millisecond, 10*time.Millisecond)
	assert.NoError(t, err)
}
