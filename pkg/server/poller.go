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
	ephemeral             bool // if true, stop polling after first task dispatched
	log                   *slog.Logger
	sem                   chan struct{} // concurrency semaphore
	wg                    sync.WaitGroup
	stopPoll              context.CancelFunc // set by Run(), called in ephemeral mode after dispatch
	stopJobs              context.CancelFunc // set by Run(), called by Shutdown when its ctx expires
	lastPollNs            atomic.Int64       // unix-nanos of the most recent FetchTask attempt to RETURN; 0 until first poll completes
	lastSuccessfulFetchNs atomic.Int64       // unix-nanos of the most recent FetchTask that produced a real response (success or DeadlineExceeded); 0 until first such response
	inFlight              atomic.Int64       // number of handler goroutines currently running
}

// workerState is per-Run scratch state: the cursor we send to the server,
// the idempotency key for the next request, and consecutive empty/error
// counters that drive backoff. Local to Run so a fresh Run starts clean.
type workerState struct {
	tasksVersion      int64
	requestKey        gouuid.UUID
	consecutiveEmpty  int
	consecutiveErrors int
}

func (s *workerState) resetBackoff() {
	s.consecutiveEmpty = 0
	s.consecutiveErrors = 0
}

const backoffMax = 60 * time.Second

// NewPoller creates a poller that calls handler for each received task.
// If ephemeral is true, the poller stops after the first task is dispatched.
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

// Run starts the poll loop. Blocks until ctx is cancelled (or until the
// first task is dispatched in ephemeral mode). Acquires a capacity slot
// BEFORE calling FetchTask so the loop blocks on capacity rather than
// in a separate dispatch step (matches upstream act_runner; see bug 013).
func (p *Poller) Run(ctx context.Context) {
	pollingCtx, stopPoll := context.WithCancel(ctx)
	defer stopPoll()
	p.stopPoll = stopPoll

	jobsCtx, stopJobs := context.WithCancel(ctx)
	p.stopJobs = stopJobs

	s := &workerState{requestKey: gouuid.New()}

	p.log.Info("poller started",
		"interval", p.client.FetchInterval(),
		"capacity", p.capacity,
		"ephemeral", p.ephemeral,
		"endpoint", p.client.Endpoint(),
	)

	for {
		// 1. Acquire capacity, or stop.
		select {
		case p.sem <- struct{}{}:
		case <-pollingCtx.Done():
			p.log.Info("poller stopping")
			return
		}

		// 2. Fetch (we hold a slot to handle a task if one comes back).
		task, ok := p.fetchTask(pollingCtx, s)
		if !ok {
			<-p.sem
			if !p.waitBackoff(pollingCtx, s) {
				p.log.Info("poller stopping")
				return
			}
			continue
		}
		s.resetBackoff()

		// 3. Spawn handler. Goroutine releases the slot when done.
		p.wg.Add(1)
		p.inFlight.Add(1)
		go func(t *runnerv1.Task) {
			defer p.wg.Done()
			defer p.inFlight.Add(-1)
			defer func() { <-p.sem }()
			p.handler(jobsCtx, t)
		}(task)

		if p.ephemeral {
			p.log.Info("ephemeral mode: task dispatched, stopping poller")
			stopPoll()
		}
	}
}

// fetchTask runs one FetchTask round trip and updates heartbeats and
// workerState. Returns (task, true) if a task was received; (nil, false)
// otherwise (empty response, error, or context cancellation). The cursor
// is forward-only and reset to 0 after a task receipt (bug 012).
func (p *Poller) fetchTask(ctx context.Context, s *workerState) (*runnerv1.Task, bool) {
	cleanup := p.client.SetRequestKey(s.requestKey)
	defer cleanup()

	fetchCtx, cancel := context.WithTimeout(ctx, p.fetchTimeout)
	defer cancel()

	resp, err := p.client.FetchTask(fetchCtx, connect.NewRequest(&runnerv1.FetchTaskRequest{
		TasksVersion: s.tasksVersion,
	}))

	// Heartbeat: lastPollNs records that the RPC RETURNED. lastSuccessfulFetchNs
	// is stricter — only advances on a real server response. CodeDeadlineExceeded
	// is the long-poll's "no work" signal and counts as a successful round trip.
	if ctx.Err() == nil {
		now := time.Now().UnixNano()
		p.lastPollNs.Store(now)
		if err == nil || connect.CodeOf(err) == connect.CodeDeadlineExceeded {
			p.lastSuccessfulFetchNs.Store(now)
		}
	}

	if err != nil {
		if ctx.Err() != nil {
			return nil, false
		}
		if connect.CodeOf(err) == connect.CodeDeadlineExceeded {
			p.log.Debug("no tasks available", "error", err)
			s.consecutiveEmpty++
			return nil, false
		}
		p.log.Error("fetch task failed", "error", err)
		s.consecutiveErrors++
		return nil, false
	}

	// Reset error counter on any successful RPC. consecutiveEmpty is
	// intentionally NOT reset here — back-to-back empty responses still
	// drive the empty-side backoff to slow polling when the server has
	// no work.
	s.consecutiveErrors = 0
	s.requestKey = gouuid.New()

	// Cursor: forward-only advance + reset to 0 on task receipt (bug 012).
	if v := resp.Msg.GetTasksVersion(); v > s.tasksVersion {
		s.tasksVersion = v
	}

	task := resp.Msg.GetTask()
	if task == nil || task.GetId() == 0 {
		s.consecutiveEmpty++
		return nil, false
	}
	s.tasksVersion = 0
	p.log.Info("received task", "id", task.GetId())
	return task, true
}

// waitBackoff sleeps for the configured FetchInterval, scaled by
// consecutive empty/error counts. Returns false if the polling context
// is cancelled while waiting.
func (p *Poller) waitBackoff(ctx context.Context, s *workerState) bool {
	base := p.client.FetchInterval()
	n := s.consecutiveErrors
	if s.consecutiveEmpty > n {
		n = s.consecutiveEmpty
	}
	d := base
	if n > 1 {
		shift := n - 1
		if shift > 5 {
			shift = 5
		}
		d = base * time.Duration(int64(1)<<shift)
		if d > backoffMax {
			d = backoffMax
		}
		p.log.Warn("backing off", "duration", d)
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-ctx.Done():
		return false
	}
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

// InFlight returns the number of handler goroutines currently running.
// /healthz uses this to suppress the poll-staleness 503 while the runner
// is legitimately busy with a long-running task (bug 013).
func (p *Poller) InFlight() int64 {
	return p.inFlight.Load()
}

// Shutdown stops accepting new work and waits for in-flight handlers to
// complete. If ctx expires before that happens, in-flight handlers are
// cancelled (their handler ctx fires) and Shutdown waits for them to
// return; ctx.Err() is returned in that case. Otherwise nil.
//
// Replaces Drain — the difference is that Shutdown cancels the handler
// context on timeout, rather than returning while leaving handlers
// running. Callers should pass a context with the deadline they're
// willing to wait for graceful drain.
func (p *Poller) Shutdown(ctx context.Context) error {
	if p.stopPoll != nil {
		p.stopPoll()
	}
	if p.stopJobs != nil {
		// Always release jobsCtx on return. CancelFunc is idempotent.
		// This plugs the context-child leak that would otherwise happen
		// in the graceful path (where the timeout branch is not taken).
		defer p.stopJobs()
	}

	done := make(chan struct{})
	go func() {
		p.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		p.log.Info("all tasks drained")
		return nil
	case <-ctx.Done():
		// Race: graceful drain may have completed at the same instant.
		select {
		case <-done:
			return nil
		default:
		}
		p.log.Warn("drain timed out — cancelling in-flight tasks")
		if p.stopJobs != nil {
			p.stopJobs()
		}
		<-done
		return ctx.Err()
	}
}
