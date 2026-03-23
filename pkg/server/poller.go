package server

import (
	"context"
	"log/slog"
	"sync"
	"time"

	runnerv1 "code.forgejo.org/forgejo/actions-proto/runner/v1"
	"connectrpc.com/connect"
	gouuid "github.com/google/uuid"
)

// TaskHandler is called when a task is received from Forgejo.
type TaskHandler func(ctx context.Context, task *runnerv1.Task)

// Poller continuously fetches tasks from Forgejo.
type Poller struct {
	client       PollerClient
	handler      TaskHandler
	fetchTimeout time.Duration
	capacity     int64
	log          *slog.Logger
	sem          chan struct{} // concurrency semaphore
	wg           sync.WaitGroup
}

// NewPoller creates a poller that calls handler for each received task.
func NewPoller(client PollerClient, handler TaskHandler, capacity int64, fetchTimeout time.Duration, log *slog.Logger) *Poller {
	return &Poller{
		client:       client,
		handler:      handler,
		fetchTimeout: fetchTimeout,
		capacity:     capacity,
		log:          log,
		sem:          make(chan struct{}, capacity),
	}
}

// Run starts the poll loop. Blocks until ctx is cancelled.
func (p *Poller) Run(ctx context.Context) {
	var tasksVersion int64
	requestKey := gouuid.New()

	ticker := time.NewTicker(p.client.FetchInterval())
	defer ticker.Stop()

	p.log.Info("poller started",
		"interval", p.client.FetchInterval(),
		"capacity", p.capacity,
		"endpoint", p.client.Endpoint(),
	)

	for {
		select {
		case <-ctx.Done():
			p.log.Info("poller stopping")
			return
		case <-ticker.C:
			p.poll(ctx, &tasksVersion, &requestKey)
		}
	}
}

func (p *Poller) poll(ctx context.Context, tasksVersion *int64, requestKey *gouuid.UUID) {
	cleanup := p.client.SetRequestKey(*requestKey)
	defer cleanup()

	fetchCtx, cancel := context.WithTimeout(ctx, p.fetchTimeout)
	defer cancel()

	resp, err := p.client.FetchTask(fetchCtx, connect.NewRequest(&runnerv1.FetchTaskRequest{
		TasksVersion: *tasksVersion,
	}))
	if err != nil {
		if ctx.Err() != nil {
			return // Context cancelled, shutting down.
		}
		// deadline_exceeded is normal when no tasks are available (server holds connection).
		if connect.CodeOf(err) == connect.CodeDeadlineExceeded {
			p.log.Debug("no tasks available", "error", err)
		} else {
			p.log.Error("fetch task failed", "error", err)
		}
		// Keep the same request key for retry (idempotency).
		return
	}

	// Successful response — rotate request key and update version.
	*requestKey = gouuid.New()
	*tasksVersion = resp.Msg.GetTasksVersion()

	// Handle primary task.
	if task := resp.Msg.GetTask(); task != nil && task.GetId() != 0 {
		p.log.Info("received task", "id", task.GetId())
		p.dispatchTask(ctx, task)
	}

	// Handle additional tasks (multi-capacity).
	for _, task := range resp.Msg.GetAdditionalTasks() {
		if task != nil && task.GetId() != 0 {
			p.log.Info("received additional task", "id", task.GetId())
			p.dispatchTask(ctx, task)
		}
	}
}

// dispatchTask runs the handler in a goroutine after acquiring a semaphore slot.
// Blocks until a slot is available or the context is cancelled.
func (p *Poller) dispatchTask(ctx context.Context, task *runnerv1.Task) {
	select {
	case p.sem <- struct{}{}:
	case <-ctx.Done():
		p.log.Warn("context cancelled while waiting for capacity", "task_id", task.GetId())
		return
	}
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		defer func() { <-p.sem }()
		p.handler(ctx, task)
	}()
}

// Drain waits for all in-flight tasks to complete, up to the given timeout.
func (p *Poller) Drain(timeout time.Duration) {
	done := make(chan struct{})
	go func() {
		p.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		p.log.Info("all tasks drained")
	case <-time.After(timeout):
		p.log.Warn("drain timed out, some tasks may still be running", "timeout", timeout)
	}
}
