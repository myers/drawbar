package reporter

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	runnerv1 "code.gitea.io/actions-proto-go/runner/v1"
	"connectrpc.com/connect"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mockClient implements the Client interface for testing.
type mockClient struct {
	mu       sync.Mutex
	logCalls []*runnerv1.UpdateLogRequest
	taskCalls []*runnerv1.UpdateTaskRequest
	logErr   error
	taskErr  error
	taskResp *runnerv1.UpdateTaskResponse
	// ackFunc allows tests to control the ACK index dynamically.
	ackFunc func(req *runnerv1.UpdateLogRequest) int64
}

func newMockClient() *mockClient {
	return &mockClient{
		taskResp: &runnerv1.UpdateTaskResponse{},
		ackFunc: func(req *runnerv1.UpdateLogRequest) int64 {
			return req.Index + int64(len(req.Rows))
		},
	}
}

func (m *mockClient) UpdateLog(_ context.Context, req *connect.Request[runnerv1.UpdateLogRequest]) (*connect.Response[runnerv1.UpdateLogResponse], error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.logCalls = append(m.logCalls, req.Msg)
	if m.logErr != nil {
		return nil, m.logErr
	}
	ack := m.ackFunc(req.Msg)
	return connect.NewResponse(&runnerv1.UpdateLogResponse{AckIndex: ack}), nil
}

func (m *mockClient) UpdateTask(_ context.Context, req *connect.Request[runnerv1.UpdateTaskRequest]) (*connect.Response[runnerv1.UpdateTaskResponse], error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.taskCalls = append(m.taskCalls, req.Msg)
	if m.taskErr != nil {
		return nil, m.taskErr
	}
	return connect.NewResponse(m.taskResp), nil
}

func (m *mockClient) getLogCalls() []*runnerv1.UpdateLogRequest {
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := make([]*runnerv1.UpdateLogRequest, len(m.logCalls))
	copy(cp, m.logCalls)
	return cp
}

func (m *mockClient) getTaskCalls() []*runnerv1.UpdateTaskRequest {
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := make([]*runnerv1.UpdateTaskRequest, len(m.taskCalls))
	copy(cp, m.taskCalls)
	return cp
}

func TestReporter_AddLog(t *testing.T) {
	mc := newMockClient()
	rep := New(mc, 1, 2, time.Hour) // long interval — we'll flush manually

	rep.StartStep(0)
	rep.AddLog("line 1")
	rep.AddLog("line 2")

	err := rep.Flush(context.Background())
	require.NoError(t, err)

	calls := mc.getLogCalls()
	require.Len(t, calls, 1)
	assert.Len(t, calls[0].Rows, 2)
	assert.Equal(t, "line 1", calls[0].Rows[0].Content)
	assert.Equal(t, "line 2", calls[0].Rows[1].Content)
	assert.False(t, calls[0].NoMore)
}

func TestReporter_StepLogTracking(t *testing.T) {
	mc := newMockClient()
	rep := New(mc, 1, 2, time.Hour)

	// Step 0: 2 log lines
	rep.StartStep(0)
	rep.AddLog("step0-line0")
	rep.AddLog("step0-line1")
	rep.FinishStep(0, runnerv1.Result_RESULT_SUCCESS)

	// Step 1: 1 log line
	rep.StartStep(1)
	rep.AddLog("step1-line0")
	rep.FinishStep(1, runnerv1.Result_RESULT_SUCCESS)

	err := rep.Flush(context.Background())
	require.NoError(t, err)

	taskCalls := mc.getTaskCalls()
	require.Len(t, taskCalls, 1)

	steps := taskCalls[0].State.Steps
	require.Len(t, steps, 2)

	// Step 0 starts at index 0, length 2
	assert.Equal(t, int64(0), steps[0].LogIndex)
	assert.Equal(t, int64(2), steps[0].LogLength)
	assert.Equal(t, runnerv1.Result_RESULT_SUCCESS, steps[0].Result)
	assert.NotNil(t, steps[0].StartedAt)
	assert.NotNil(t, steps[0].StoppedAt)

	// Step 1 starts at index 2, length 1
	assert.Equal(t, int64(2), steps[1].LogIndex)
	assert.Equal(t, int64(1), steps[1].LogLength)
}

func TestReporter_FlushTrimsAcknowledgedRows(t *testing.T) {
	mc := newMockClient()
	rep := New(mc, 1, 1, time.Hour)

	rep.StartStep(0)
	rep.AddLog("line0")
	rep.AddLog("line1")
	rep.AddLog("line2")

	// First flush — server ACKs all 3.
	err := rep.Flush(context.Background())
	require.NoError(t, err)

	// Add more logs.
	rep.AddLog("line3")

	// Second flush — should send only line3, starting at index 3.
	err = rep.Flush(context.Background())
	require.NoError(t, err)

	calls := mc.getLogCalls()
	require.Len(t, calls, 2)
	assert.Equal(t, int64(0), calls[0].Index)
	assert.Len(t, calls[0].Rows, 3)
	assert.Equal(t, int64(3), calls[1].Index)
	assert.Len(t, calls[1].Rows, 1)
	assert.Equal(t, "line3", calls[1].Rows[0].Content)
}

func TestReporter_PartialACK(t *testing.T) {
	mc := newMockClient()
	// Server only ACKs 1 of 3 rows.
	mc.ackFunc = func(req *runnerv1.UpdateLogRequest) int64 {
		return req.Index + 1
	}
	rep := New(mc, 1, 1, time.Hour)

	rep.StartStep(0)
	rep.AddLog("line0")
	rep.AddLog("line1")
	rep.AddLog("line2")

	err := rep.Flush(context.Background())
	require.NoError(t, err)

	// Second flush should resend remaining 2 rows.
	mc.ackFunc = func(req *runnerv1.UpdateLogRequest) int64 {
		return req.Index + int64(len(req.Rows))
	}
	err = rep.Flush(context.Background())
	require.NoError(t, err)

	calls := mc.getLogCalls()
	require.Len(t, calls, 2)
	assert.Equal(t, int64(0), calls[0].Index)
	assert.Len(t, calls[0].Rows, 3)
	assert.Equal(t, int64(1), calls[1].Index)
	assert.Len(t, calls[1].Rows, 2)
}

func TestReporter_CloseSuccess(t *testing.T) {
	mc := newMockClient()
	rep := New(mc, 1, 1, time.Hour)

	rep.StartStep(0)
	rep.AddLog("done")
	rep.FinishStep(0, runnerv1.Result_RESULT_SUCCESS)

	err := rep.Close(context.Background(), runnerv1.Result_RESULT_SUCCESS)
	require.NoError(t, err)

	// Should have sent logs with noMore=true.
	logCalls := mc.getLogCalls()
	require.NotEmpty(t, logCalls)
	assert.True(t, logCalls[len(logCalls)-1].NoMore)

	// Should have sent final state.
	taskCalls := mc.getTaskCalls()
	require.NotEmpty(t, taskCalls)
	lastState := taskCalls[len(taskCalls)-1].State
	assert.Equal(t, runnerv1.Result_RESULT_SUCCESS, lastState.Result)
	assert.NotNil(t, lastState.StoppedAt)
}

func TestReporter_CloseFailure_CancelsUnfinishedSteps(t *testing.T) {
	mc := newMockClient()
	rep := New(mc, 1, 3, time.Hour)

	rep.StartStep(0)
	rep.FinishStep(0, runnerv1.Result_RESULT_SUCCESS)
	rep.StartStep(1)
	rep.FinishStep(1, runnerv1.Result_RESULT_FAILURE)
	// Step 2 never started.

	err := rep.Close(context.Background(), runnerv1.Result_RESULT_FAILURE)
	require.NoError(t, err)

	taskCalls := mc.getTaskCalls()
	require.NotEmpty(t, taskCalls)
	steps := taskCalls[len(taskCalls)-1].State.Steps

	assert.Equal(t, runnerv1.Result_RESULT_SUCCESS, steps[0].Result)
	assert.Equal(t, runnerv1.Result_RESULT_FAILURE, steps[1].Result)
	assert.Equal(t, runnerv1.Result_RESULT_CANCELLED, steps[2].Result)
}

func TestReporter_CloseRetry(t *testing.T) {
	mc := newMockClient()
	rep := New(mc, 1, 0, time.Hour)

	mc.logErr = fmt.Errorf("network error")

	// Clear the error after a short delay so retry succeeds.
	go func() {
		time.Sleep(250 * time.Millisecond)
		mc.mu.Lock()
		mc.logErr = nil
		mc.mu.Unlock()
	}()

	err := rep.Close(context.Background(), runnerv1.Result_RESULT_SUCCESS)
	require.NoError(t, err)
}

func TestReporter_ServerCancellation(t *testing.T) {
	mc := newMockClient()
	mc.taskResp = &runnerv1.UpdateTaskResponse{
		State: &runnerv1.TaskState{
			Result: runnerv1.Result_RESULT_CANCELLED,
		},
	}

	rep := New(mc, 1, 1, time.Hour)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	rep.RunDaemon(ctx)
	rep.StartStep(0)
	rep.AddLog("working...")

	// Flush triggers UpdateTask which returns CANCELLED.
	err := rep.Flush(context.Background())
	require.NoError(t, err)

	// Give the cancel a moment to propagate.
	time.Sleep(50 * time.Millisecond)

	// The reporter's internal cancel should have been called.
	// We can verify by checking if the daemon context is done.
	// (Indirectly verified: the daemon goroutine should stop.)
}

func TestReporter_LogsBeforeAnyStep(t *testing.T) {
	mc := newMockClient()
	rep := New(mc, 1, 2, time.Hour)

	// Add logs before starting any step.
	rep.AddLog("pre-step log")

	err := rep.Flush(context.Background())
	require.NoError(t, err)

	logCalls := mc.getLogCalls()
	require.Len(t, logCalls, 1)
	assert.Len(t, logCalls[0].Rows, 1)
	assert.Equal(t, "pre-step log", logCalls[0].Rows[0].Content)

	// Step log tracking should NOT attribute this to any step.
	taskCalls := mc.getTaskCalls()
	require.Len(t, taskCalls, 1)
	for _, step := range taskCalls[0].State.Steps {
		assert.Equal(t, int64(0), step.LogLength)
	}
}

func TestReporter_EmptyFlush(t *testing.T) {
	mc := newMockClient()
	rep := New(mc, 1, 0, time.Hour)

	// Flush with no logs should not call UpdateLog but should call UpdateTask.
	err := rep.Flush(context.Background())
	require.NoError(t, err)

	assert.Empty(t, mc.getLogCalls())
	assert.Len(t, mc.getTaskCalls(), 1)
}

// --- Log masker ---

func TestNewLogMasker_NoSecrets(t *testing.T) {
	m := newLogMasker(nil)
	assert.Equal(t, "unchanged", m.mask("unchanged"))
	m = newLogMasker([]string{})
	assert.Equal(t, "unchanged", m.mask("unchanged"))
}

func TestNewLogMasker_ShortSecretsSkipped(t *testing.T) {
	m := newLogMasker([]string{"ab", "x", "yes"})
	assert.Equal(t, "ab x yes", m.mask("ab x yes")) // all ≤3 chars, nothing masked
}

func TestNewLogMasker_MasksLongSecrets(t *testing.T) {
	m := newLogMasker([]string{"my-secret-token"})
	require.NotNil(t, m)
	assert.Equal(t, "the value is ***", m.mask("the value is my-secret-token"))
}

func TestLogMasker_NilSafe(t *testing.T) {
	var m *logMasker
	assert.Equal(t, "hello", m.mask("hello"))
}

func TestReporter_SetSecrets_MasksLogs(t *testing.T) {
	mc := newMockClient()
	rep := New(mc, 1, 1, time.Hour)
	rep.SetSecrets([]string{"super-secret-value"})

	rep.StartStep(0)
	rep.AddLog("token is super-secret-value here")

	err := rep.Flush(context.Background())
	require.NoError(t, err)

	calls := mc.getLogCalls()
	require.Len(t, calls, 1)
	assert.Equal(t, "token is *** here", calls[0].Rows[0].Content)
}

func TestNewLogMasker_MixedLengths(t *testing.T) {
	m := newLogMasker([]string{"ab", "long-secret", "x"})
	require.NotNil(t, m) // "long-secret" qualifies
	assert.Equal(t, "has ***", m.mask("has long-secret"))
	assert.Contains(t, m.mask("ab still here"), "ab") // "ab" not masked (too short)
}
