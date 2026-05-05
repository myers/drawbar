# Poller Loop Reshape Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Restructure `pkg/server/poller.go` to acquire its capacity semaphore before calling `FetchTask`, split shutdown into two contexts, and have `/healthz` distinguish "wedged" from "busy with a long handler" — eliminating bug 013 structurally.

**Architecture:** Replace the current `Run` → `poll()` → `dispatchTask()` chain with upstream `act_runner`'s acquire-before-fetch loop and `Shutdown(ctx)` graceful-drain pattern. Adds a per-Run `workerState` struct, an `inFlight` atomic counter exposed via `InFlight()`, and a `stopJobs` context cancellation path. `cmd/controller`'s `/healthz` handler grows an `inFlight func() int64` predicate that suppresses the staleness 503 while any handler is running. Tests are added for the new structural invariants and shutdown semantics; the bug-012 cursor regression test continues to pass unchanged.

**Tech Stack:** Go (`pkg/server/poller.go`, `pkg/server/poller_test.go`, `cmd/controller/main.go`, `cmd/controller/main_test.go`). `connectrpc.com/connect`, `code.gitea.io/actions-proto-go/runner/v1`, `github.com/google/uuid`, `log/slog`, `sync/atomic`. `golangci-lint` and `go test` are run via the project's `Makefile`.

**Repo conventions to honor:**
- Develop on `main`. No feature branches, no worktrees (per `~/.claude/CLAUDE.md` and project `CLAUDE.md`).
- `go` is at `/Users/myers/.local/share/mise/installs/go/1.25.7/bin/go` — prepend to `PATH`.
- Errors: `fmt.Errorf("doing X: %w", err)`. Logging: `log/slog` structured.
- `git add` with explicit filenames; never `git add .`.
- Don't mention test counts in commit messages.

---

## File Structure

| File | Role |
|------|------|
| `pkg/server/poller.go` | The poll loop. Rewritten: acquire-before-fetch, two contexts, `workerState`, `Shutdown`, `InFlight`. |
| `pkg/server/poller_test.go` | Existing tests adjusted; 4 new tests for the new invariants. |
| `cmd/controller/main.go` | `healthzHandler` gains `inFlight` predicate; caller swaps `Drain` for `Shutdown(ctxWithTimeout)`. |
| `cmd/controller/main_test.go` | If existing tests cover `healthzHandler`, update them to pass the new `inFlight` argument. |

The poller is one focused file; no decomposition needed beyond what the spec already calls for. Helpers `fetchTask` and `waitBackoff` live alongside `Run` in `poller.go`.

---

### Task 1: Add `inFlight` counter and `InFlight()` accessor on Poller (no behavior change yet)

**Files:**
- Modify: `pkg/server/poller.go`
- Test: `pkg/server/poller_test.go`

This task adds the new `inFlight` field and accessor without yet wiring it into the loop, so `/healthz` work can land alongside it. The increment happens in this same task by adjusting the existing `dispatchTask` goroutine — non-invasive.

- [ ] **Step 1: Read the current Poller struct definition**

Run: `grep -n "type Poller struct" pkg/server/poller.go`
Expected: line ~19 with the struct definition.

- [ ] **Step 2: Write a failing test for `InFlight()`**

In `pkg/server/poller_test.go`, after the existing `TestPoller_LastSuccessfulFetchAt_DeadlineExceededCounts` test, add:

```go
func TestPoller_InFlight_TracksRunningHandlers(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	handler := func(_ context.Context, _ *runnerv1.Task) {
		close(started)
		<-release
	}
	mock := &mockPollerClient{
		interval:  10 * time.Millisecond,
		responses: []*runnerv1.FetchTaskResponse{{Task: &runnerv1.Task{Id: 1}}},
	}
	p := NewPoller(mock, handler, 1, time.Second, false, slog.Default())

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	go p.Run(ctx)

	<-started
	assert.Equal(t, int64(1), p.InFlight(), "handler running -> InFlight==1")
	close(release)

	// Wait for the handler to finish.
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) && p.InFlight() != 0 {
		time.Sleep(5 * time.Millisecond)
	}
	assert.Equal(t, int64(0), p.InFlight(), "handler returned -> InFlight==0")
	cancel()
}
```

- [ ] **Step 3: Run the test to verify it fails**

Run: `PATH=/Users/myers/.local/share/mise/installs/go/1.25.7/bin:$PATH go test ./pkg/server/ -run TestPoller_InFlight_TracksRunningHandlers -v`
Expected: FAIL with "p.InFlight undefined" (compile error) or similar.

- [ ] **Step 4: Add `inFlight` field and `InFlight()` to Poller**

In `pkg/server/poller.go`, add `inFlight atomic.Int64` to the `Poller` struct alongside the existing atomics. The struct currently looks like:

```go
type Poller struct {
	client                PollerClient
	handler               TaskHandler
	fetchTimeout          time.Duration
	capacity              int64
	ephemeral             bool
	log                   *slog.Logger
	sem                   chan struct{}
	wg                    sync.WaitGroup
	backoff               time.Duration
	stopPoll              context.CancelFunc
	lastPollNs            atomic.Int64
	lastSuccessfulFetchNs atomic.Int64
}
```

Add `inFlight atomic.Int64` right under `lastSuccessfulFetchNs`:

```go
	lastSuccessfulFetchNs atomic.Int64
	inFlight              atomic.Int64 // number of handler goroutines currently running
```

Then add the accessor near `LastSuccessfulFetchAt`:

```go
// InFlight returns the number of handler goroutines currently running.
// /healthz uses this to suppress the poll-staleness 503 while the runner
// is legitimately busy with a long-running task (bug 013).
func (p *Poller) InFlight() int64 {
	return p.inFlight.Load()
}
```

Update `dispatchTask`'s goroutine to bump the counter. Find:

```go
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		defer func() { <-p.sem }()
		p.handler(ctx, task)
	}()
```

Replace with:

```go
	p.wg.Add(1)
	p.inFlight.Add(1)
	go func() {
		defer p.wg.Done()
		defer p.inFlight.Add(-1)
		defer func() { <-p.sem }()
		p.handler(ctx, task)
	}()
```

- [ ] **Step 5: Run the test to verify it passes**

Run: `PATH=/Users/myers/.local/share/mise/installs/go/1.25.7/bin:$PATH go test ./pkg/server/ -run TestPoller_InFlight -v`
Expected: PASS.

- [ ] **Step 6: Run the full server test suite**

Run: `PATH=/Users/myers/.local/share/mise/installs/go/1.25.7/bin:$PATH go test ./pkg/server/ -count=1`
Expected: ok.

- [ ] **Step 7: Commit**

```bash
git add pkg/server/poller.go pkg/server/poller_test.go
git commit -m "$(cat <<'EOF'
poller: add InFlight() handler-count accessor

Tracks the number of currently-running handler goroutines via an atomic
counter incremented before goroutine spawn and decremented in the same
defer chain that releases the capacity semaphore. /healthz will use this
to suppress the poll-staleness 503 while the runner is busy with a
long-running task (bug 013).
EOF
)"
```

---

### Task 2: Wire `InFlight()` into `/healthz` so a busy runner stays healthy

**Files:**
- Modify: `cmd/controller/main.go`

`healthzHandler` gains an `inFlight func() int64` parameter. The poll-staleness check (the 503 path that fires `onWedge`) is suppressed when `inFlight() > 0`. The `lastSuccessfulFetch` check stays unconditional — bug 010's transport wedge can happen during a long handler.

- [ ] **Step 1: Inspect current `healthzHandler` signature and call sites**

Run: `grep -n "healthzHandler\|startHealthServer" cmd/controller/main.go`
Expected: handler defined at ~389; called from `startHealthServer` at ~359; `startHealthServer` called from `Start` at ~282.

- [ ] **Step 2: Update `healthzHandler` to accept `inFlight`**

In `cmd/controller/main.go`, change the signature:

```go
func healthzHandler(
	lastPoll func() time.Time,
	lastSuccessfulFetch func() time.Time,
	inFlight func() int64,
	pollStaleness time.Duration,
	successFetchStaleness time.Duration,
	onWedge func(kind string, since, threshold time.Duration),
) http.HandlerFunc {
```

Inside the handler, gate the `lastPoll` staleness branch on `inFlight() == 0`. Find:

```go
		if t := lastPoll(); !t.IsZero() {
			if since := time.Since(t); since > pollStaleness {
				if onWedge != nil {
					dumpOnce.Do(func() { onWedge("poll loop", since, pollStaleness) })
				}
				w.WriteHeader(http.StatusServiceUnavailable)
				fmt.Fprintf(w, "poll loop stale: last poll %s ago (threshold %s)", since, pollStaleness)
				return
			}
		}
```

Replace with:

```go
		// While any handler is in flight, the poll loop is legitimately blocked
		// on capacity and lastPoll won't advance. Suppress the staleness 503 in
		// that case — see bug 013.
		if inFlight() == 0 {
			if t := lastPoll(); !t.IsZero() {
				if since := time.Since(t); since > pollStaleness {
					if onWedge != nil {
						dumpOnce.Do(func() { onWedge("poll loop", since, pollStaleness) })
					}
					w.WriteHeader(http.StatusServiceUnavailable)
					fmt.Fprintf(w, "poll loop stale: last poll %s ago (threshold %s)", since, pollStaleness)
					return
				}
			}
		}
```

Update the doc comment above `healthzHandler` to mention the new behavior:

```go
// healthzHandler returns OK while the poll loop is making progress and 503
// once either heartbeat is too stale:
//
//   - lastPoll catches "ticker dead" (the poll goroutine is not running). The
//     zero value is treated as "starting up" and is always OK. The check is
//     suppressed while inFlight() > 0 because the loop legitimately blocks on
//     capacity acquisition while a handler runs (bug 013).
//   - lastSuccessfulFetch catches "transport dead but goroutine alive" — RPCs
//     are returning errors but no real server response in a long time
//     (h2 conn half-dead, server-side throttling, ...). The zero value is
//     ignored on purpose: if the server has been down since the runner
//     started, /readyz handles that — /healthz should not page. This check
//     is NOT suppressed during in-flight handlers: a transport wedge while
//     a long handler runs is still a real problem (bug 010).
//
// onWedge fires exactly once across the lifetime of the handler the first
// time either condition trips, so callers can capture diagnostics (a goroutine
// dump) before the kubelet restarts the pod and destroys the evidence.
// The kind parameter identifies which check tripped ("poll loop" or
// "successful fetch").
```

- [ ] **Step 3: Update `startHealthServer` to thread `inFlight` through**

Find `startHealthServer` (around line 349). Add `inFlight func() int64` to its parameter list and to the `healthzHandler(...)` call:

```go
func startHealthServer(
	registered *atomic.Bool,
	activeJobs *atomic.Int64,
	capacity int64,
	lastPoll func() time.Time,
	lastSuccessfulFetch func() time.Time,
	inFlight func() int64,
	pollStaleness time.Duration,
	successFetchStaleness time.Duration,
) {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", healthzHandler(
		lastPoll, lastSuccessfulFetch,
		inFlight,
		pollStaleness, successFetchStaleness,
		dumpGoroutinesToStderr,
	))
	mux.HandleFunc("/readyz", readyzHandler(registered))
	mux.HandleFunc("/metrics/active-jobs", metricsHandler(activeJobs, capacity))
	mux.Handle("/debug/pprof/", http.DefaultServeMux)
	slog.Info("health server listening", "port", 8081)
	if err := http.ListenAndServe(":8081", mux); err != nil {
		slog.Error("health server error", "error", err)
	}
}
```

- [ ] **Step 4: Update the `startHealthServer` caller to pass `poller.InFlight`**

Find the `go startHealthServer(...)` call (around line 282). Update:

```go
	go startHealthServer(
		&registered, &activeJobs, int64(cfg.Runner.Capacity),
		poller.LastPollAt, poller.LastSuccessfulFetchAt,
		poller.InFlight,
		pollStaleness, successFetchStaleness,
	)
```

- [ ] **Step 5: Build and run existing tests**

Run: `PATH=/Users/myers/.local/share/mise/installs/go/1.25.7/bin:$PATH go build ./... && PATH=/Users/myers/.local/share/mise/installs/go/1.25.7/bin:$PATH go test ./... -count=1`
Expected: build succeeds, all tests pass. If `cmd/controller/main_test.go` had a `healthzHandler` test, it will fail to compile because of the new argument — fix it next.

- [ ] **Step 6: If `cmd/controller/main_test.go` fails to compile, update test calls**

Run: `grep -n "healthzHandler" cmd/controller/main_test.go 2>/dev/null || echo "no main_test.go calls"`

If there are calls to `healthzHandler(...)` in tests, add a new `inFlight` arg. For tests that don't care about in-flight state, pass `func() int64 { return 0 }`. For any test that exercises the busy-suppression path, add a controllable counter:

```go
var inFlight atomic.Int64
h := healthzHandler(
	func() time.Time { return lastPoll },
	func() time.Time { return lastSucc },
	inFlight.Load,
	pollThresh, succThresh,
	nil,
)
```

Run tests again to confirm.

- [ ] **Step 7: Commit**

```bash
git add cmd/controller/main.go
git commit -m "$(cat <<'EOF'
controller: suppress /healthz poll-staleness while a handler runs

healthzHandler accepts an inFlight func() int64 predicate. While at
least one handler goroutine is in flight, the lastPoll staleness check
is skipped — the loop is legitimately blocked on capacity acquisition,
not wedged. The lastSuccessfulFetch check remains unconditional so a
transport wedge during a long handler is still surfaced.
EOF
)"
```

If `cmd/controller/main_test.go` was updated, include it in the same commit:

```bash
git add cmd/controller/main.go cmd/controller/main_test.go
```

---

### Task 3: Add the `TestPoller_HealthyDuringLongHandler` regression test

This test pins down the bug 013 behavior using the now-wired `InFlight()` accessor. It calls `healthzHandler` directly so the test does not need to spin up an HTTP server.

**Files:**
- Modify: `cmd/controller/main_test.go` (or create if it doesn't exist)

- [ ] **Step 1: Check whether main_test.go exists and what it covers**

Run: `ls cmd/controller/ && grep -n "healthzHandler\|TestHealthz" cmd/controller/main_test.go 2>/dev/null || echo "no test file or no relevant tests"`

- [ ] **Step 2: Write the regression test**

Add to `cmd/controller/main_test.go` (creating with `package main` + necessary imports if missing):

```go
func TestHealthzHandler_BusySuppressesStaleness(t *testing.T) {
	pollStale := 50 * time.Millisecond
	succStale := time.Hour
	now := time.Now().Add(-time.Hour) // last poll: ages ago
	var inFlight atomic.Int64

	var wedgeKind string
	onWedge := func(kind string, _, _ time.Duration) { wedgeKind = kind }

	h := healthzHandler(
		func() time.Time { return now },
		func() time.Time { return time.Now() }, // succ fetch: fresh
		inFlight.Load,
		pollStale, succStale,
		onWedge,
	)

	// While in-flight, poll-staleness must be suppressed.
	inFlight.Store(1)
	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("busy poll-stale: want 200, got %d (%q)", rec.Code, rec.Body.String())
	}
	if wedgeKind != "" {
		t.Fatalf("busy: onWedge must not fire, got %q", wedgeKind)
	}

	// When idle, the same staleness must trip the 503.
	inFlight.Store(0)
	rec = httptest.NewRecorder()
	h(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("idle poll-stale: want 503, got %d (%q)", rec.Code, rec.Body.String())
	}
	if wedgeKind != "poll loop" {
		t.Fatalf("idle: want wedgeKind=poll loop, got %q", wedgeKind)
	}
}
```

If the file doesn't exist, create it with:

```go
package main

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)
```

If the file already exists, add only the missing imports.

- [ ] **Step 3: Run the test**

Run: `PATH=/Users/myers/.local/share/mise/installs/go/1.25.7/bin:$PATH go test ./cmd/controller/ -run TestHealthzHandler_BusySuppressesStaleness -v`
Expected: PASS.

- [ ] **Step 4: Commit**

```bash
git add cmd/controller/main_test.go
git commit -m "$(cat <<'EOF'
controller: cover /healthz busy-suppression for bug 013

Asserts healthzHandler returns 200 (and does not fire onWedge) when the
poll heartbeat is stale but inFlight() > 0, then 503 with the "poll
loop" wedge kind once inFlight drops to 0 with the same heartbeat.
EOF
)"
```

---

### Task 4: Add `Shutdown(ctx)` alongside the existing `Drain(timeout)`

This task adds the new method without removing the old one yet. `Shutdown` cancels `pollingCtx` (which `Run` is already structured around) and waits for `wg`, falling back to cancelling `jobsCtx` on ctx timeout. We add a `stopJobs` field plumbed through `Run`.

**Files:**
- Modify: `pkg/server/poller.go`
- Test: `pkg/server/poller_test.go`

- [ ] **Step 1: Read the current `Run` structure to identify cancellation plumbing**

Run: `grep -n "stopPoll\|context.WithCancel\|Drain" pkg/server/poller.go`
Expected: `stopPoll` field; one `context.WithCancel` in `Run`; `Drain` near the bottom.

- [ ] **Step 2: Add `stopJobs` field, derive `jobsCtx` in `Run`, pass it to handler**

In `pkg/server/poller.go`, add to the `Poller` struct (next to `stopPoll`):

```go
	stopPoll context.CancelFunc
	stopJobs context.CancelFunc
```

In `Run`, derive `jobsCtx` and store its cancel func. Find:

```go
func (p *Poller) Run(ctx context.Context) {
	pollCtx, stopPoll := context.WithCancel(ctx)
	defer stopPoll()
	p.stopPoll = stopPoll
```

Replace with:

```go
func (p *Poller) Run(ctx context.Context) {
	pollCtx, stopPoll := context.WithCancel(ctx)
	defer stopPoll()
	p.stopPoll = stopPoll

	jobsCtx, stopJobs := context.WithCancel(ctx)
	defer stopJobs()
	p.stopJobs = stopJobs
```

Update the dispatch goroutine to use `jobsCtx` instead of the outer `ctx`. Find inside `dispatchTask`:

```go
	go func() {
		defer p.wg.Done()
		defer p.inFlight.Add(-1)
		defer func() { <-p.sem }()
		p.handler(ctx, task)
	}()
```

It currently uses the `ctx` argument of `dispatchTask`. Two options:
  - **Pass `jobsCtx` explicitly** — change `dispatchTask` signature to `dispatchTask(jobsCtx context.Context, task *runnerv1.Task)`.
  - **Pass via Poller field** — `dispatchTask` reads `p.stopJobs`'s ctx.

Go with the first (explicit). Change `dispatchTask` to accept `jobsCtx context.Context`. Update the call site in `poll(...)` — but `poll` doesn't currently see `jobsCtx`. Easier: `Run` passes `jobsCtx` into `poll`, which passes it to `dispatchTask`.

Update `poll` signature and call. Find:

```go
		case <-ticker.C:
			p.poll(ctx, &tasksVersion, &requestKey)
```

Replace with:

```go
		case <-ticker.C:
			p.poll(ctx, jobsCtx, &tasksVersion, &requestKey)
```

Find `func (p *Poller) poll(ctx context.Context, ...)` and update its signature:

```go
func (p *Poller) poll(ctx context.Context, jobsCtx context.Context, tasksVersion *int64, requestKey *gouuid.UUID) {
```

In `poll`, change the dispatch call:

```go
	if task := resp.Msg.GetTask(); task != nil && task.GetId() != 0 {
		p.log.Info("received task", "id", task.GetId())
		*tasksVersion = 0
		p.dispatchTask(jobsCtx, task)
	}
```

Update `dispatchTask`'s parameter name and the goroutine body:

```go
func (p *Poller) dispatchTask(jobsCtx context.Context, task *runnerv1.Task) {
	select {
	case p.sem <- struct{}{}:
	case <-jobsCtx.Done():
		p.log.Warn("context cancelled while waiting for capacity", "task_id", task.GetId())
		return
	}
	p.wg.Add(1)
	p.inFlight.Add(1)
	go func() {
		defer p.wg.Done()
		defer p.inFlight.Add(-1)
		defer func() { <-p.sem }()
		p.handler(jobsCtx, task)
	}()

	if p.ephemeral && p.stopPoll != nil {
		p.log.Info("ephemeral mode: task dispatched, stopping poller")
		p.stopPoll()
	}
}
```

- [ ] **Step 3: Add `Shutdown(ctx)` method**

Add near `Drain` (do not remove `Drain` yet):

```go
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
```

- [ ] **Step 4: Run all server tests to confirm nothing broke**

Run: `PATH=/Users/myers/.local/share/mise/installs/go/1.25.7/bin:$PATH go test ./pkg/server/ -count=1`
Expected: ok. The existing `TestDrain_*` tests pass unchanged because we kept `Drain`.

- [ ] **Step 5: Commit**

```bash
git add pkg/server/poller.go
git commit -m "$(cat <<'EOF'
poller: add Shutdown(ctx) with two-context drain

Splits the existing single-context shutdown into pollingCtx (stop
accepting new work) and jobsCtx (kill in-flight handlers). Shutdown
cancels polling, waits for the handler waitgroup, and only cancels
jobsCtx if its argument context expires first. This is the structural
piece that lets a kubelet SIGTERM give in-flight reporter flushes time
to finish before being force-cancelled.

Drain stays for now to keep the controller working; the next commit
swaps the call site over.
EOF
)"
```

---

### Task 5: Test `Shutdown_GracefulDrain` and `Shutdown_HardTimeout`

**Files:**
- Modify: `pkg/server/poller_test.go`

- [ ] **Step 1: Write the graceful-drain test**

Add after the existing `TestPoller_InFlight_TracksRunningHandlers`:

```go
func TestPoller_Shutdown_GracefulDrain(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	var sawJobsCancel atomic.Bool
	handler := func(ctx context.Context, _ *runnerv1.Task) {
		close(started)
		select {
		case <-release:
			// Graceful path: ctx should still be alive.
			if ctx.Err() != nil {
				sawJobsCancel.Store(true)
			}
		case <-ctx.Done():
			sawJobsCancel.Store(true)
		}
	}

	mock := &mockPollerClient{
		interval:  10 * time.Millisecond,
		responses: []*runnerv1.FetchTaskResponse{{Task: &runnerv1.Task{Id: 1}}},
	}
	p := NewPoller(mock, handler, 1, time.Second, false, slog.Default())

	runDone := make(chan struct{})
	go func() {
		p.Run(context.Background())
		close(runDone)
	}()

	<-started
	close(release)

	shutCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := p.Shutdown(shutCtx); err != nil {
		t.Fatalf("Shutdown: want nil, got %v", err)
	}
	if sawJobsCancel.Load() {
		t.Fatal("graceful path: handler must not see jobsCtx cancellation")
	}
	<-runDone
}
```

- [ ] **Step 2: Write the hard-timeout test**

Append:

```go
func TestPoller_Shutdown_HardTimeout(t *testing.T) {
	started := make(chan struct{})
	var sawJobsCancel atomic.Bool
	handler := func(ctx context.Context, _ *runnerv1.Task) {
		close(started)
		<-ctx.Done()
		sawJobsCancel.Store(true)
	}

	mock := &mockPollerClient{
		interval:  10 * time.Millisecond,
		responses: []*runnerv1.FetchTaskResponse{{Task: &runnerv1.Task{Id: 1}}},
	}
	p := NewPoller(mock, handler, 1, time.Second, false, slog.Default())

	runDone := make(chan struct{})
	go func() {
		p.Run(context.Background())
		close(runDone)
	}()

	<-started

	shutCtx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	err := p.Shutdown(shutCtx)
	if err == nil {
		t.Fatal("Shutdown: want non-nil err on timeout, got nil")
	}
	if !sawJobsCancel.Load() {
		t.Fatal("hard path: handler must see jobsCtx cancellation")
	}
	<-runDone
}
```

- [ ] **Step 3: Run both tests**

Run: `PATH=/Users/myers/.local/share/mise/installs/go/1.25.7/bin:$PATH go test ./pkg/server/ -run TestPoller_Shutdown -v -count=1`
Expected: both PASS.

- [ ] **Step 4: Commit**

```bash
git add pkg/server/poller_test.go
git commit -m "$(cat <<'EOF'
poller: cover Shutdown graceful drain and hard timeout

Two tests pinning down the new behavior: a handler that completes
inside the Shutdown deadline must not observe jobsCtx cancellation; a
handler that ignores the deadline does observe it once the deadline
expires.
EOF
)"
```

---

### Task 6: Reshape `Run` to acquire-before-fetch with `workerState`

This is the structural change. After this task, bug 013 is unrepresentable: the loop blocks at the semaphore acquire, not in a separate `dispatchTask` after `FetchTask` returned a task.

**Files:**
- Modify: `pkg/server/poller.go`
- Test: `pkg/server/poller_test.go` (add the structural-invariant test).

- [ ] **Step 1: Add the structural-invariant test (will fail against the current code's loop shape — but that loop shape doesn't actually trigger the bug in a single-task test, so we'll write a stronger test)**

Append to `pkg/server/poller_test.go`:

```go
// fetchCountingClient records every FetchTask call so a test can assert
// the loop does NOT call FetchTask while at capacity.
type fetchCountingClient struct {
	mu        sync.Mutex
	calls     int
	responses []*runnerv1.FetchTaskResponse
	idx       int
	interval  time.Duration
}

func (c *fetchCountingClient) FetchTask(_ context.Context, _ *connect.Request[runnerv1.FetchTaskRequest]) (*connect.Response[runnerv1.FetchTaskResponse], error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	if c.idx < len(c.responses) {
		r := c.responses[c.idx]
		c.idx++
		return connect.NewResponse(r), nil
	}
	return connect.NewResponse(&runnerv1.FetchTaskResponse{}), nil
}
func (c *fetchCountingClient) Endpoint() string             { return "" }
func (c *fetchCountingClient) FetchInterval() time.Duration { return c.interval }
func (c *fetchCountingClient) SetRequestKey(_ gouuid.UUID) func() {
	return func() {}
}
func (c *fetchCountingClient) Calls() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}

// TestPoller_AcquireBeforeFetch pins down the structural invariant: while
// at capacity, the loop must NOT call FetchTask. Today's code violates
// this — the test is expected to fail against pre-reshape code and pass
// after Task 6.
func TestPoller_AcquireBeforeFetch(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	handler := func(_ context.Context, _ *runnerv1.Task) {
		close(started)
		<-release
	}

	c := &fetchCountingClient{
		interval: 10 * time.Millisecond,
		responses: []*runnerv1.FetchTaskResponse{
			{Task: &runnerv1.Task{Id: 1}},
		},
	}
	p := NewPoller(c, handler, 1, time.Second, false, slog.Default())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go p.Run(ctx)

	<-started

	// Hold the handler busy for ~5 ticker intervals. After 50ms there
	// should still be exactly 1 FetchTask call: the one that delivered
	// the task.
	time.Sleep(50 * time.Millisecond)
	if calls := c.Calls(); calls != 1 {
		t.Fatalf("at capacity: want exactly 1 FetchTask call, got %d", calls)
	}

	close(release)
	cancel()
}
```

- [ ] **Step 2: Run the test against the current code**

Run: `PATH=/Users/myers/.local/share/mise/installs/go/1.25.7/bin:$PATH go test ./pkg/server/ -run TestPoller_AcquireBeforeFetch -v -count=1`
Expected: FAIL — current code calls `FetchTask` on every tick regardless of capacity (and blocks at `dispatchTask` after each call). The call count should be >1.

- [ ] **Step 3: Add `workerState`, `fetchTask`, `waitBackoff`; rewrite `Run`**

Replace the current `Run` and `poll` in `pkg/server/poller.go` with the new structure. The full set of changes:

**Add the workerState type** (above `Run`):

```go
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
```

**Replace `Run`**:

```go
// Run starts the poll loop. Blocks until ctx is cancelled (or until the
// first task is dispatched in ephemeral mode). Acquires a capacity slot
// BEFORE calling FetchTask so the loop blocks on capacity rather than
// in a separate dispatch step (matches upstream act_runner; see bug 013).
func (p *Poller) Run(ctx context.Context) {
	pollingCtx, stopPoll := context.WithCancel(ctx)
	defer stopPoll()
	p.stopPoll = stopPoll

	jobsCtx, stopJobs := context.WithCancel(ctx)
	defer stopJobs()
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
```

**Replace `poll` with `fetchTask`** (delete the old `poll` function entirely):

```go
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
```

**Add `waitBackoff`** (replaces the old `increaseBackoff` helper):

```go
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
```

**Delete the obsolete code:** the old `poll` function, the `increaseBackoff` method, the `backoff time.Duration` field on `Poller`, and `dispatchTask`. After deletion the file has only: package, imports, types (Poller + workerState + TaskHandler), constants (backoffMax), `NewPoller`, `Run`, `fetchTask`, `waitBackoff`, `LastPollAt`, `LastSuccessfulFetchAt`, `InFlight`, `Drain`, `Shutdown`.

`backoffMin` constant was used by `increaseBackoff`; since we now scale from `FetchInterval` directly, `backoffMin` is no longer needed. Delete the constant too.

- [ ] **Step 4: Run the structural-invariant test**

Run: `PATH=/Users/myers/.local/share/mise/installs/go/1.25.7/bin:$PATH go test ./pkg/server/ -run TestPoller_AcquireBeforeFetch -v -count=1`
Expected: PASS.

- [ ] **Step 5: Run the full server test suite**

Run: `PATH=/Users/myers/.local/share/mise/installs/go/1.25.7/bin:$PATH go test ./pkg/server/ -count=1`
Expected: ok. The existing `TestPoller_*` tests should continue to pass:
  - `TestPoller_DispatchesTask` — single task, completes, exits.
  - `TestPoller_NoTask` — empty response, no dispatch.
  - `TestPoller_FetchError_DeadlineExceeded` — deadline-exceeded path doesn't crash.
  - `TestPoller_ContextCancellation` — cancellable.
  - `TestPoller_Ephemeral` — ephemeral exits after one task.
  - `TestPoller_LastPollAt` — heartbeat sets.
  - `TestPoller_LastSuccessfulFetchAt_*` — three variants.
  - `TestPoller_DoesNotLatchCursorOnEmptyResponse` (bug 012) — still passes.
  - `TestPoller_InFlight_TracksRunningHandlers` (Task 1) — still passes.
  - `TestPoller_Shutdown_GracefulDrain`, `TestPoller_Shutdown_HardTimeout` (Task 5) — still pass.

If any test fails, debug:
  - `TestPoller_NoTask`: now backs off after consecutive empties. If the test runs for 50ms and the first response is empty, `waitBackoff` sleeps `FetchInterval` (10ms in tests) → next call. Should still be fine.
  - `TestPoller_Ephemeral`: stopPoll is called via captured local `stopPoll`, not `p.stopPoll` field. Verify the variable name in the new `Run` matches.

- [ ] **Step 6: Run all package tests**

Run: `PATH=/Users/myers/.local/share/mise/installs/go/1.25.7/bin:$PATH go test ./... -count=1`
Expected: ok across all packages.

- [ ] **Step 7: Commit**

```bash
git add pkg/server/poller.go pkg/server/poller_test.go
git commit -m "$(cat <<'EOF'
poller: acquire capacity before FetchTask (closes bug 013)

The poll loop now blocks at the semaphore acquire at the top of each
iteration instead of after FetchTask returns. While a handler runs, the
loop never enters FetchTask — the previous structure called FetchTask
on every tick and then blocked at dispatch, dragging the lastPoll
heartbeat stale and tripping /healthz mid-task.

Also folds the per-Run cursor / requestKey / consecutive-empty/error
counters into a workerState struct (matches upstream act_runner) and
replaces the old increaseBackoff field-mutating helper with a pure
waitBackoff function that sleeps via the polling context.

backoff and backoffMin are removed. Backoff now scales directly from
FetchInterval × 2^(n-1), capped at backoffMax.
EOF
)"
```

---

### Task 7: Swap `cmd/controller`'s `Drain` call for `Shutdown` and remove `Drain`

`Drain` is now redundant with `Shutdown(ctxWithTimeout)`. The single caller in `cmd/controller/main.go` swaps over.

**Files:**
- Modify: `cmd/controller/main.go`
- Modify: `pkg/server/poller.go` — remove `Drain`.
- Modify: `pkg/server/poller_test.go` — convert remaining `TestDrain_*` tests.

- [ ] **Step 1: Replace `Drain` with `Shutdown` in main.go**

Find:

```go
	poller.Run(ctx)
	slog.Info("poller stopped, draining in-flight tasks")
	poller.Drain(30 * time.Second)
	slog.Info("runner shut down")
	return nil
```

Replace with:

```go
	poller.Run(ctx)
	slog.Info("poller stopped, draining in-flight tasks")
	shutCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := poller.Shutdown(shutCtx); err != nil {
		slog.Warn("shutdown drain ended early", "error", err)
	}
	slog.Info("runner shut down")
	return nil
```

- [ ] **Step 2: Convert `TestDrain_WaitsForTasks` and `TestDrain_Timeout`**

In `pkg/server/poller_test.go`, find `TestDrain_WaitsForTasks`. Replace its body:

```go
func TestShutdown_WaitsForTasks(t *testing.T) {
	var finished atomic.Bool
	handler := func(_ context.Context, _ *runnerv1.Task) {
		time.Sleep(50 * time.Millisecond)
		finished.Store(true)
	}

	mock := &mockPollerClient{
		interval:  10 * time.Millisecond,
		responses: []*runnerv1.FetchTaskResponse{{Task: &runnerv1.Task{Id: 1}}},
	}

	p := NewPoller(mock, handler, 1, time.Second, false, slog.Default())
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	p.Run(ctx)
	shutCtx, shutCancel := context.WithTimeout(context.Background(), time.Second)
	defer shutCancel()
	if err := p.Shutdown(shutCtx); err != nil {
		t.Fatalf("Shutdown: want nil, got %v", err)
	}

	assert.True(t, finished.Load())
}
```

Find `TestDrain_Timeout`. Replace with a Shutdown-shaped test that proves the hard-timeout *cancels* the handler (the new semantics):

```go
func TestShutdown_Timeout(t *testing.T) {
	handler := func(ctx context.Context, _ *runnerv1.Task) {
		// Respect ctx so Shutdown's hard cancel can return.
		select {
		case <-ctx.Done():
		case <-time.After(5 * time.Second):
		}
	}

	mock := &mockPollerClient{
		interval:  10 * time.Millisecond,
		responses: []*runnerv1.FetchTaskResponse{{Task: &runnerv1.Task{Id: 1}}},
	}

	p := NewPoller(mock, handler, 1, time.Second, false, slog.Default())
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	p.Run(ctx)

	shutCtx, shutCancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer shutCancel()
	start := time.Now()
	err := p.Shutdown(shutCtx)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("Shutdown: want non-nil err on timeout, got nil")
	}
	assert.Less(t, elapsed, 500*time.Millisecond, "Shutdown should return promptly after cancelling handler")
}
```

- [ ] **Step 3: Remove `Drain` from `pkg/server/poller.go`**

Find and delete:

```go
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
```

- [ ] **Step 4: Build everything**

Run: `PATH=/Users/myers/.local/share/mise/installs/go/1.25.7/bin:$PATH go build ./...`
Expected: success.

If `pkg/server/poller_test.go` still has older tests that call `p.Drain(...)` directly (e.g. `TestPoller_DispatchesTask`, `TestPoller_Ephemeral`, `TestDrain_WaitsForTasks` if you preserved its name), update each call site:

  - `p.Drain(time.Second)` → `func() { ctx, cancel := context.WithTimeout(context.Background(), time.Second); defer cancel(); _ = p.Shutdown(ctx) }()`

Or, more readably, factor a test helper at the top of the file:

```go
func shutdown(t *testing.T, p *Poller, timeout time.Duration) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	_ = p.Shutdown(ctx)
}
```

Then replace `p.Drain(time.Second)` with `shutdown(t, p, time.Second)` everywhere.

- [ ] **Step 5: Run the full server test suite**

Run: `PATH=/Users/myers/.local/share/mise/installs/go/1.25.7/bin:$PATH go test ./pkg/server/ -count=1 -v`
Expected: all tests pass.

- [ ] **Step 6: Run all package tests**

Run: `PATH=/Users/myers/.local/share/mise/installs/go/1.25.7/bin:$PATH go test ./... -count=1`
Expected: ok across all packages.

- [ ] **Step 7: Commit**

```bash
git add cmd/controller/main.go pkg/server/poller.go pkg/server/poller_test.go
git commit -m "$(cat <<'EOF'
controller, poller: replace Drain(timeout) with Shutdown(ctx)

The cmd/controller drain step now uses a timeout-bound context and
poller.Shutdown, which cancels in-flight handlers if the timeout
elapses (instead of returning while leaving them running). Drain is
removed; tests that exercised it are converted to use Shutdown via a
small helper.
EOF
)"
```

---

### Task 8: Verification — run vet, lint, and the full suite once more

**Files:** none (verification only).

- [ ] **Step 1: `go vet`**

Run: `PATH=/Users/myers/.local/share/mise/installs/go/1.25.7/bin:$PATH go vet ./...`
Expected: clean (the proto.Clone fix from earlier in the session keeps `pkg/server/fetchtask_idempotency_test.go` quiet).

- [ ] **Step 2: `go test` race detector on the package**

Run: `PATH=/Users/myers/.local/share/mise/installs/go/1.25.7/bin:$PATH go test -race ./pkg/server/ -count=1`
Expected: ok. The new `inFlight` counter and the two-context shutdown introduce concurrency that's worth race-checking.

- [ ] **Step 3: Full test suite**

Run: `PATH=/Users/myers/.local/share/mise/installs/go/1.25.7/bin:$PATH go test ./... -count=1`
Expected: ok everywhere.

- [ ] **Step 4 (optional but recommended): lint**

Run: `make lint`
Expected: clean. If `make lint` complains about unused imports left over from the refactor, fix and amend. Common candidates:
  - `time.Duration` import was kept but `backoffMin` was removed (still used elsewhere).
  - `gouuid` still used by `workerState.requestKey` and `client.SetRequestKey` — keep.

- [ ] **Step 5: Update bug 013 doc to mark it fixed**

Append at the top of `bugs/013-healthz-false-positive-during-capacity-wait.md`, under the title:

```markdown
**Status: fixed.** Landed in the poller-loop-reshape change (see
`docs/superpowers/specs/2026-05-05-poller-loop-reshape-design.md`).
The bug is now structurally unrepresentable — the poll loop acquires
its capacity slot before calling FetchTask, so it can never block
inside dispatch with a stale lastPoll heartbeat. /healthz also
suppresses the poll-staleness 503 while a handler is in flight, as a
belt-and-suspenders measure.
```

- [ ] **Step 6: Final commit**

```bash
git add bugs/013-healthz-false-positive-during-capacity-wait.md
git commit -m "$(cat <<'EOF'
docs: mark bug 013 fixed by poller loop reshape
EOF
)"
```

---

## Self-Review

**Spec coverage:**
- Acquire-before-fetch loop shape → Task 6.
- Two-context shutdown → Tasks 4 & 7.
- `workerState` struct → Task 6.
- `InFlight()` accessor → Task 1.
- `/healthz` busy-suppression → Tasks 2 & 3.
- Cursor forward-only + reset on task → preserved in `fetchTask` (Task 6).
- Ephemeral via `stopPoll` → preserved in `Run` (Task 6).
- `Shutdown(ctx)` graceful + hard timeout → Tasks 4, 5, 7.
- All tests in spec covered: `TestPoller_AcquireBeforeFetch` (Task 6), `TestPoller_HealthyDuringLongHandler` (Task 3, named `TestHealthzHandler_BusySuppressesStaleness` — better name since it's testing the handler not the poller, but covers the same invariant), `TestPoller_Shutdown_GracefulDrain` (Task 5), `TestPoller_Shutdown_HardTimeout` (Task 5). Existing tests preserved/updated.

**Placeholder scan:** No "TBD/TODO/handle later." Each step shows the actual code or the exact command.

**Type consistency:**
- `workerState` fields: `tasksVersion int64`, `requestKey gouuid.UUID`, `consecutiveEmpty int`, `consecutiveErrors int` — used consistently in Tasks 6.
- `Shutdown(ctx context.Context) error` — same in Tasks 4 (added), 5 (tested), 7 (called).
- `InFlight() int64` — same in Tasks 1 (added), 2 (passed), 3 (tested).
- `healthzHandler` parameter order: `lastPoll, lastSuccessfulFetch, inFlight, pollStaleness, successFetchStaleness, onWedge` — consistent across Tasks 2 and 3.
- `stopPoll` and `stopJobs` field names — consistent between Tasks 4 and 6.
