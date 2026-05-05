package server

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	runnerv1 "code.gitea.io/actions-proto-go/runner/v1"
	"connectrpc.com/connect"
	gouuid "github.com/google/uuid"
)

// TaskHandler is called when a task is received from Forgejo.
type TaskHandler func(ctx context.Context, task *runnerv1.Task)

// Poller continuously fetches tasks from the server.
type Poller struct {
	client                PollerClient
	handler               TaskHandler
	fetchTimeout          time.Duration
	capacity              int64
	ephemeral             bool // if true, stop polling after first task completes
	log                   *slog.Logger
	sem                   chan struct{} // concurrency semaphore
	wg                    sync.WaitGroup
	backoff               time.Duration      // current backoff duration (0 = no backoff)
	stopPoll              context.CancelFunc // set by Run(), called in ephemeral mode after dispatch
	lastPollNs            atomic.Int64       // unix-nanos of the most recent FetchTask attempt to RETURN; 0 until first poll completes
	lastSuccessfulFetchNs atomic.Int64       // unix-nanos of the most recent FetchTask that produced a real response (success or DeadlineExceeded); 0 until first such response
}

const (
	backoffMin = 2 * time.Second
	backoffMax = 60 * time.Second
)

// NewPoller creates a poller that calls handler for each received task.
// If ephemeral is true, the poller stops after the first task completes.
func NewPoller(client PollerClient, handler TaskHandler, capacity int64, fetchTimeout time.Duration, ephemeral bool, log *slog.Logger) *Poller {
	return &Poller{
		client:       client,
		handler:      handler,
		fetchTimeout: fetchTimeout,
		capacity:     capacity,
		ephemeral:    ephemeral,
		log:          log,
		sem:          make(chan struct{}, capacity),
	}
}

// Run starts the poll loop. Blocks until ctx is cancelled (or until the first
// task completes in ephemeral mode).
func (p *Poller) Run(ctx context.Context) {
	pollCtx, stopPoll := context.WithCancel(ctx)
	defer stopPoll()
	p.stopPoll = stopPoll

	var tasksVersion int64
	requestKey := gouuid.New()

	interval := p.client.FetchInterval()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	p.log.Info("poller started",
		"interval", interval,
		"capacity", p.capacity,
		"ephemeral", p.ephemeral,
		"endpoint", p.client.Endpoint(),
	)

	for {
		select {
		case <-pollCtx.Done():
			p.log.Info("poller stopping")
			return
		case <-ticker.C:
			p.poll(ctx, &tasksVersion, &requestKey)

			// Adjust ticker: use backoff duration if set, otherwise normal interval.
			if p.backoff > 0 {
				ticker.Reset(p.backoff)
			} else {
				ticker.Reset(interval)
			}
		}
	}
}

func (p *Poller) poll(ctx context.Context, tasksVersion *int64, requestKey *gouuid.UUID) {
	p.log.Debug("polling", "tasks_version", *tasksVersion)

	cleanup := p.client.SetRequestKey(*requestKey)
	defer cleanup()

	fetchCtx, cancel := context.WithTimeout(ctx, p.fetchTimeout)
	defer cancel()

	resp, err := p.client.FetchTask(fetchCtx, connect.NewRequest(&runnerv1.FetchTaskRequest{
		TasksVersion: *tasksVersion,
	}))

	// Heartbeat: lastPollNs records that the RPC RETURNED (success or error,
	// but not a context cancellation due to shutdown). If the RPC wedges on
	// a half-dead h2 conn, this heartbeat goes stale and /healthz reports it.
	if ctx.Err() == nil {
		now := time.Now().UnixNano()
		p.lastPollNs.Store(now)
		// lastSuccessfulFetchNs is stricter: only advances when the server
		// actually responded. CodeDeadlineExceeded is the long-poll "no work"
		// signal and counts as a successful round trip.
		if err == nil || connect.CodeOf(err) == connect.CodeDeadlineExceeded {
			p.lastSuccessfulFetchNs.Store(now)
		}
	}

	if err != nil {
		if ctx.Err() != nil {
			return // Context cancelled, shutting down.
		}
		if connect.CodeOf(err) == connect.CodeDeadlineExceeded {
			p.log.Debug("no tasks available", "error", err)
			p.backoff = 0
		} else {
			p.log.Error("fetch task failed", "error", err)
			p.increaseBackoff()
		}
		return
	}

	p.backoff = 0
	*requestKey = gouuid.New()

	// Advance the cursor on every successful response so that idle polls reach
	// gitea's "version unchanged" fast path. Only ever move it forward — a
	// stale response carrying an older version must not roll us back.
	if v := resp.Msg.GetTasksVersion(); v > *tasksVersion {
		*tasksVersion = v
	}

	if task := resp.Msg.GetTask(); task != nil && task.GetId() != 0 {
		p.log.Info("received task", "id", task.GetId())
		// Reset the cursor so the next poll forces gitea to run PickTask.
		// Otherwise we wedge: gitea bumps latestVersion on run-insert /
		// job-reset, not on PickTask or UpdateTask, so a second task queued
		// at the same version (e.g. two runs from one push, capacity=1)
		// would sit indefinitely while gitea returned empty responses with
		// the same version. Matches gitea's act_runner behaviour. See bugs/012.
		*tasksVersion = 0
		p.dispatchTask(ctx, task)
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

	if p.ephemeral && p.stopPoll != nil {
		p.log.Info("ephemeral mode: task dispatched, stopping poller")
		p.stopPoll()
	}
}

func (p *Poller) increaseBackoff() {
	if p.backoff == 0 {
		p.backoff = backoffMin
	} else {
		p.backoff *= 2
		if p.backoff > backoffMax {
			p.backoff = backoffMax
		}
	}
	p.log.Warn("backing off", "duration", p.backoff)
}

// LastPollAt returns the wall-clock time of the most recent FetchTask call
// to RETURN (success or error). Returns the zero Time before the first
// completed RPC; callers should treat that as "never polled."
func (p *Poller) LastPollAt() time.Time {
	ns := p.lastPollNs.Load()
	if ns == 0 {
		return time.Time{}
	}
	return time.Unix(0, ns)
}

// LastSuccessfulFetchAt returns the wall-clock time of the most recent
// FetchTask that produced a real server response — either nil error or
// connect.CodeDeadlineExceeded (long-poll's "no work" signal). Returns the
// zero Time before the first such response. Used by /healthz to detect a
// transport that is alive at the syscall level but not actually talking
// to the server (h2 conn half-dead, server-side throttling, etc.).
func (p *Poller) LastSuccessfulFetchAt() time.Time {
	ns := p.lastSuccessfulFetchNs.Load()
	if ns == 0 {
		return time.Time{}
	}
	return time.Unix(0, ns)
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
