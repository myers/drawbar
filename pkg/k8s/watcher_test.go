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
	outputs []string // sequential outputs for each Exec call
	errs    []error
	idx     int
}

func (m *mockPodExecutor) Exec(_ context.Context, _, _, _ string, _ []string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	i := m.idx
	m.idx++
	if i < len(m.errs) && m.errs[i] != nil {
		return "", m.errs[i]
	}
	if i < len(m.outputs) {
		return m.outputs[i], nil
	}
	return "", fmt.Errorf("terminated")
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

// --- parseStateEvents ---

func TestParseStateEvents_Empty(t *testing.T) {
	events, offset := parseStateEvents("", 0)
	assert.Empty(t, events)
	assert.Equal(t, 0, offset)
}

func TestParseStateEvents_SingleEvent(t *testing.T) {
	output := `{"event":"start","step":0,"name":"Build","exit_code":0,"time":"2024-01-01T00:00:00Z"}`
	events, offset := parseStateEvents(output, 0)
	require.Len(t, events, 1)
	assert.Equal(t, "start", events[0].Event)
	assert.Equal(t, 0, events[0].Step)
	assert.Equal(t, "Build", events[0].Name)
	assert.Equal(t, 1, offset)
}

func TestParseStateEvents_MultipleWithOffset(t *testing.T) {
	output := `{"event":"start","step":0,"name":"A","exit_code":0,"time":"t1"}
{"event":"end","step":0,"name":"A","exit_code":0,"time":"t2"}
{"event":"start","step":1,"name":"B","exit_code":0,"time":"t3"}`

	// First call — read all 3.
	events, offset := parseStateEvents(output, 0)
	require.Len(t, events, 3)
	assert.Equal(t, 3, offset)

	// Second call with same output — no new events.
	events2, offset2 := parseStateEvents(output, offset)
	assert.Empty(t, events2)
	assert.Equal(t, 3, offset2)
}

func TestParseStateEvents_MalformedSkipped(t *testing.T) {
	output := `{"event":"start","step":0,"name":"A","exit_code":0,"time":"t1"}
not valid json
{"event":"end","step":0,"name":"A","exit_code":1,"time":"t2"}`

	events, _ := parseStateEvents(output, 0)
	require.Len(t, events, 2)
	assert.Equal(t, "start", events[0].Event)
	assert.Equal(t, "end", events[1].Event)
}

func TestParseStateEvents_BlankLines(t *testing.T) {
	output := `{"event":"start","step":0,"name":"A","exit_code":0,"time":"t1"}

{"event":"end","step":0,"name":"A","exit_code":0,"time":"t2"}`

	events, _ := parseStateEvents(output, 0)
	require.Len(t, events, 2)
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

// --- pollStateFileWith ---

func TestPollStateFileWith(t *testing.T) {
	executor := &mockPodExecutor{
		outputs: []string{
			`{"event":"start","step":0,"name":"Build","exit_code":0,"time":"t1"}`,
			`{"event":"start","step":0,"name":"Build","exit_code":0,"time":"t1"}
{"event":"end","step":0,"name":"Build","exit_code":0,"time":"t2"}`,
		},
		errs: []error{nil, nil, fmt.Errorf("terminated")},
	}

	rep := newTestReporter(1, 2)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	err := pollStateFileWith(ctx, executor, "ns", "pod", rep, 10*time.Millisecond)
	assert.NoError(t, err)
}

func TestPollStateFileWith_ContainerExit(t *testing.T) {
	executor := &mockPodExecutor{
		errs: []error{fmt.Errorf("container terminated")},
	}

	rep := newTestReporter(1, 1)
	ctx := context.Background()

	err := pollStateFileWith(ctx, executor, "ns", "pod", rep, 10*time.Millisecond)
	assert.NoError(t, err) // terminated is a clean exit
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
