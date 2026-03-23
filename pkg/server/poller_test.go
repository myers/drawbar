package server

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	runnerv1 "code.forgejo.org/forgejo/actions-proto/runner/v1"
	"connectrpc.com/connect"
	gouuid "github.com/google/uuid"
	"github.com/stretchr/testify/assert"
)

// mockPollerClient implements PollerClient for testing.
type mockPollerClient struct {
	mu        sync.Mutex
	responses []*runnerv1.FetchTaskResponse
	errs      []error
	callCount int
	endpoint  string
	interval  time.Duration
}

func (m *mockPollerClient) FetchTask(_ context.Context, _ *connect.Request[runnerv1.FetchTaskRequest]) (*connect.Response[runnerv1.FetchTaskResponse], error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	idx := m.callCount
	m.callCount++
	if idx < len(m.errs) && m.errs[idx] != nil {
		return nil, m.errs[idx]
	}
	if idx < len(m.responses) {
		return connect.NewResponse(m.responses[idx]), nil
	}
	return connect.NewResponse(&runnerv1.FetchTaskResponse{}), nil
}

func (m *mockPollerClient) Endpoint() string           { return m.endpoint }
func (m *mockPollerClient) FetchInterval() time.Duration { return m.interval }
func (m *mockPollerClient) SetRequestKey(_ gouuid.UUID) func() {
	return func() {}
}

func TestPoller_DispatchesTask(t *testing.T) {
	var handled atomic.Int64
	handler := func(_ context.Context, task *runnerv1.Task) {
		handled.Store(task.GetId())
	}

	mock := &mockPollerClient{
		interval:  10 * time.Millisecond,
		responses: []*runnerv1.FetchTaskResponse{{Task: &runnerv1.Task{Id: 42}}},
	}

	p := NewPoller(mock, handler, 1, time.Second, slog.Default())
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	p.Run(ctx)
	p.Drain(time.Second)

	assert.Equal(t, int64(42), handled.Load())
}

func TestPoller_AdditionalTasks(t *testing.T) {
	var count atomic.Int64
	handler := func(_ context.Context, _ *runnerv1.Task) {
		count.Add(1)
	}

	mock := &mockPollerClient{
		interval: 10 * time.Millisecond,
		responses: []*runnerv1.FetchTaskResponse{{
			Task:            &runnerv1.Task{Id: 1},
			AdditionalTasks: []*runnerv1.Task{{Id: 2}, {Id: 3}},
		}},
	}

	p := NewPoller(mock, handler, 3, time.Second, slog.Default())
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	p.Run(ctx)
	p.Drain(time.Second)

	assert.Equal(t, int64(3), count.Load())
}

func TestPoller_NoTask(t *testing.T) {
	var count atomic.Int64
	handler := func(_ context.Context, _ *runnerv1.Task) {
		count.Add(1)
	}

	mock := &mockPollerClient{
		interval:  10 * time.Millisecond,
		responses: []*runnerv1.FetchTaskResponse{{}},
	}

	p := NewPoller(mock, handler, 1, time.Second, slog.Default())
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	p.Run(ctx)

	assert.Equal(t, int64(0), count.Load())
}

func TestPoller_FetchError_DeadlineExceeded(t *testing.T) {
	mock := &mockPollerClient{
		interval: 10 * time.Millisecond,
		errs:     []error{connect.NewError(connect.CodeDeadlineExceeded, nil)},
	}

	handler := func(_ context.Context, _ *runnerv1.Task) {}
	p := NewPoller(mock, handler, 1, time.Second, slog.Default())
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	// Should not panic or crash.
	p.Run(ctx)
}

func TestPoller_ContextCancellation(t *testing.T) {
	mock := &mockPollerClient{interval: 10 * time.Millisecond}
	handler := func(_ context.Context, _ *runnerv1.Task) {}
	p := NewPoller(mock, handler, 1, time.Second, slog.Default())

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		p.Run(ctx)
		close(done)
	}()

	cancel()
	select {
	case <-done:
		// Good, returned promptly.
	case <-time.After(time.Second):
		t.Fatal("Run did not return after context cancellation")
	}
}

func TestDrain_WaitsForTasks(t *testing.T) {
	var finished atomic.Bool
	handler := func(_ context.Context, _ *runnerv1.Task) {
		time.Sleep(50 * time.Millisecond)
		finished.Store(true)
	}

	mock := &mockPollerClient{
		interval:  10 * time.Millisecond,
		responses: []*runnerv1.FetchTaskResponse{{Task: &runnerv1.Task{Id: 1}}},
	}

	p := NewPoller(mock, handler, 1, time.Second, slog.Default())
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	p.Run(ctx)
	p.Drain(time.Second)

	assert.True(t, finished.Load())
}

func TestDrain_Timeout(t *testing.T) {
	handler := func(_ context.Context, _ *runnerv1.Task) {
		time.Sleep(5 * time.Second) // very slow
	}

	mock := &mockPollerClient{
		interval:  10 * time.Millisecond,
		responses: []*runnerv1.FetchTaskResponse{{Task: &runnerv1.Task{Id: 1}}},
	}

	p := NewPoller(mock, handler, 1, time.Second, slog.Default())
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	p.Run(ctx)

	start := time.Now()
	p.Drain(100 * time.Millisecond)
	elapsed := time.Since(start)

	assert.Less(t, elapsed, 500*time.Millisecond, "Drain should timeout quickly")
}
