# Poller loop reshape (acquire-before-fetch + two-context shutdown)

Date: 2026-05-05. Closes bug 013, structural improvement informed by
`reference/runner/internal/app/poll/poller.go`.

## Goal

Restructure `pkg/server/poller.go` and the controller's `/healthz` so that:

1. The capacity semaphore is acquired **before** `FetchTask` is called,
   not after — eliminating bug 013 (poll-loop heartbeat goes stale while
   `dispatchTask` blocks for capacity, kubelet liveness restarts the
   controller mid-task).
2. Shutdown uses **two contexts** (`pollingCtx` for "stop accepting
   work" and `jobsCtx` for "kill in-flight handlers"), so kubelet
   SIGTERM lets the in-flight reporter flush its final state before
   force-cancellation.
3. `/healthz` distinguishes **wedged** (no progress) from **busy**
   (handler running) — the staleness 503 is suppressed while any
   handler is in flight.

This is the change `bugs/upstream-recon-2026-05-05.md` flagged as the
most actionable upstream lesson, and it makes bug 013 unrepresentable
rather than papering over it with a heartbeat workaround.

## Architecture

### Loop shape

`Run(parentCtx)` becomes (pseudocode):

```go
pollingCtx, stopPolling := context.WithCancel(parentCtx)
jobsCtx,    stopJobs    := context.WithCancel(parentCtx)
defer stopPolling()
defer stopJobs()

s := &workerState{}

for {
    // 1. Acquire a capacity slot, or stop.
    select {
    case p.sem <- struct{}{}:
    case <-pollingCtx.Done():
        return
    }

    // 2. Fetch (only when we have capacity to handle a result).
    task, ok := p.fetchTask(pollingCtx, s)
    if !ok {
        <-p.sem
        if !p.waitBackoff(pollingCtx, s) {
            return
        }
        continue
    }
    s.resetBackoff()

    // 3. Spawn handler. Semaphore is released by the goroutine's defer.
    p.wg.Add(1)
    p.inFlight.Add(1)
    go func(t *runnerv1.Task) {
        defer p.wg.Done()
        defer p.inFlight.Add(-1)
        defer func() { <-p.sem }()
        p.handler(jobsCtx, t)
    }(task)

    if p.ephemeral {
        stopPolling()
    }
}
```

`poll()` and `dispatchTask()` are gone. Their bodies fold into `Run`
and a small `fetchTask()` helper.

### `fetchTask`

```go
func (p *Poller) fetchTask(ctx context.Context, s *workerState) (*runnerv1.Task, bool) {
    fetchCtx, cancel := context.WithTimeout(ctx, p.fetchTimeout)
    defer cancel()

    // SetRequestKey + cleanup as today.
    cleanup := p.client.SetRequestKey(s.requestKey)
    defer cleanup()

    resp, err := p.client.FetchTask(fetchCtx, connect.NewRequest(&runnerv1.FetchTaskRequest{
        TasksVersion: s.tasksVersion,
    }))

    p.recordHeartbeats(ctx, err)  // lastPollNs always; lastSuccessfulFetchNs on nil err or DeadlineExceeded

    if err != nil {
        if ctx.Err() != nil {
            return nil, false
        }
        if connect.CodeOf(err) == connect.CodeDeadlineExceeded {
            s.consecutiveEmpty++
            return nil, false
        }
        s.consecutiveErrors++
        p.log.Error("fetch task failed", "error", err)
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

### `workerState`

Local struct, lives only across one `Run` invocation. Replaces the
`backoff time.Duration` field on `Poller` and the local `tasksVersion` /
`requestKey` vars in `Run`:

```go
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

`p.backoff` field is removed; backoff calculation becomes a function of
`workerState`.

### `waitBackoff`

```go
func (p *Poller) waitBackoff(ctx context.Context, s *workerState) bool {
    base := p.client.FetchInterval()
    n := max(s.consecutiveEmpty, s.consecutiveErrors)
    var d time.Duration
    switch {
    case n <= 1:
        d = base
    default:
        shift := n - 1
        if shift > 5 {
            shift = 5
        }
        d = base * time.Duration(int64(1)<<shift)
        if d > backoffMax {
            d = backoffMax
        }
    }
    if d > base {
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

`backoffMin` (currently 2s, used as the floor) folds into the natural
"first interval = FetchInterval"; if `FetchInterval` is shorter than
2s and we want a 2s floor, the case `n == 1 -> base` becomes
`max(base, 2*time.Second)`. **Decision: keep simple, drop the
`backoffMin` constant.** The current default FetchInterval is already
2s, and the only escape hatch from the cap is misconfiguration. (If we
ever want a separate floor, add it later.)

### `Shutdown`

Replaces `Drain(timeout)`:

```go
func (p *Poller) Shutdown(ctx context.Context) error {
    p.stopPolling()  // captured in Run

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
        // graceful timeout — may have raced with done.
        select {
        case <-done:
            return nil
        default:
        }
        p.log.Warn("drain timed out — cancelling in-flight tasks")
        p.stopJobs()
        <-done
        return ctx.Err()
    }
}
```

`stopPolling` and `stopJobs` are stored on `Poller` from `Run`. The old
`stopPoll` field is renamed `stopPolling`; new field `stopJobs`. The
ephemeral path still calls `stopPolling()` after dispatch (Q2 answer
A): no behavioral change for ephemeral mode.

The caller in `cmd/controller/main.go` swaps `p.Drain(d)` for
`p.Shutdown(ctxWithTimeout(d))`. Net behavior preserved (graceful
window of `d`, then hard cancel).

### `/healthz` change

`Poller` exposes:
- `LastPollAt() time.Time` (unchanged)
- `LastSuccessfulFetchAt() time.Time` (unchanged)
- `InFlight() int64` (new)

`InFlight()` returns the current in-flight handler count. Backed by a
new `atomic.Int64` field on `Poller`, incremented before goroutine
spawn (in `Run`) and decremented in the same `defer` chain that
releases the semaphore.

`healthzHandler` gains a fourth parameter `inFlight func() int64`.
Predicate change:

```go
if inFlight() == 0 {
    // existing lastPoll staleness check
}
// existing lastSuccessfulFetch check (unchanged — independent of busy state)
```

The `lastSuccessfulFetch` check is **not** suppressed during busy: a
handler that runs forever while the transport is dead is still a
problem, and bug 010's wedge is detected by `lastSuccessfulFetch` even
when handlers are running.

The "poll loop" wedge dump fires only when `inFlight() == 0` AND
`lastPoll` is stale — exactly the "real" wedge case bug 007 was about.

## Components touched

| File | Change |
|------|--------|
| `pkg/server/poller.go` | Rewrite `Run`; remove `poll`/`dispatchTask`; add `fetchTask`/`waitBackoff`/`workerState`/`Shutdown`/`InFlight`. ~80 LOC delta. |
| `pkg/server/poller_test.go` | Add 4 new tests (below). Update existing `TestDrain_*` tests to use `Shutdown(ctxWithTimeout)`. The bug-012 regression test continues to pass unchanged. |
| `cmd/controller/main.go` | `healthzHandler` gains `inFlight func() int64` predicate. Replace `p.Drain(d)` with `p.Shutdown(ctxWithTimeout(d))`. Wire `poller.InFlight` into the handler. |

`pkg/server/interfaces.go`, `pkg/server/client.go` — untouched.

## Tests

In `pkg/server/poller_test.go`:

1. **`TestPoller_AcquireBeforeFetch`** — capacity=1, slow handler
   (300ms). Mock counts `FetchTask` calls. After 1st fetch returns a
   task, no further `FetchTask` is called until handler completes.
   Asserts the structural invariant.

2. **`TestPoller_HealthyDuringLongHandler`** — capacity=1, staleness
   threshold 50ms, handler runs 200ms. Drive `healthzHandler`'s
   predicate (or test `InFlight() > 0` directly + simulate the
   `lastPoll-stale-but-busy` case). Assert no 503 fired and
   `onWedge` callback was not invoked.

3. **`TestPoller_Shutdown_GracefulDrain`** — handler signals start,
   waits on a chan. Test calls `Shutdown(1s timeout)`. Test releases
   the chan from outside; handler returns within 100ms. Assert
   `Shutdown` returned nil, handler observed `jobsCtx.Err() == nil`
   throughout.

4. **`TestPoller_Shutdown_HardTimeout`** — handler does
   `<-jobsCtx.Done(); return`. Test calls `Shutdown(50ms timeout)`.
   Assert `Shutdown` returns `context.DeadlineExceeded`; handler saw
   `jobsCtx.Done()` fire.

Existing tests adjusted, not removed:
- `TestPoller_DispatchesTask` — unchanged behavior, should pass.
- `TestPoller_NoTask` — unchanged.
- `TestPoller_FetchError_DeadlineExceeded` — unchanged.
- `TestPoller_ContextCancellation` — unchanged.
- `TestPoller_Ephemeral` — unchanged (ephemeral still cancels polling).
- `TestPoller_LastPollAt` — unchanged.
- `TestPoller_LastSuccessfulFetchAt_*` — unchanged.
- `TestPoller_DoesNotLatchCursorOnEmptyResponse` (bug 012) — unchanged.
- `TestDrain_WaitsForTasks` → renamed `TestShutdown_WaitsForTasks`,
  switched to `Shutdown`.
- `TestDrain_Timeout` → renamed `TestShutdown_Timeout`, switched to
  `Shutdown`. **Note**: under the new design, `Shutdown` cancels
  `jobsCtx` after the timeout, so a handler that sleeps 5s but
  ignores ctx will block `Shutdown` past its timeout. Test handler
  needs to honor `jobsCtx`.

## Risks / non-goals

- **Drain semantics are slightly stricter**: today `Drain(timeout)`
  returns after timeout even if handlers are still running (orphaned
  goroutine survives). New `Shutdown(ctx)` cancels `jobsCtx` and waits
  for the handler to actually return. A handler that ignores
  `jobsCtx` will block `Shutdown` forever. This is correct behavior:
  the existing handler at `cmd/controller/main.go` wraps the runner
  loop with the parent ctx, so it already responds to cancellation.
  Verified by reading `runHandler` (or equivalent).

- **In-flight job orphaning on hard exit**: kubelet SIGKILL after the
  graceful window is unaffected by this work. The k8s Job continues
  running but loses its drawbar log/state pipe. Real fix is "re-attach
  to in-progress jobs on restart" — out of scope, future bug.

- **`InFlight()` mask on transport wedge**: a handler that runs
  forever during a fetch wedge still trips `lastSuccessfulFetch`
  staleness, so the alarm path is preserved.

- **Test for "ephemeral after restart in middle of task"**: ephemeral
  mode is for one-shot CI runners; the existing `TestPoller_Ephemeral`
  proves the cancel flow works. We don't add new ephemeral coverage.

- **`backoffMin` removal**: today's code has a 2s floor that's
  separate from FetchInterval. New code uses FetchInterval as the
  floor. For the default config (FetchInterval=2s) this is identical;
  for shorter FetchInterval configs the new code lets backoff start
  shorter. We're not aware of any deployment using <2s FetchInterval,
  but if found, add an explicit `max(base, 2*time.Second)` line.

## Out of scope (explicitly)

- Reshaping `cmd/controller`'s top-level shutdown signal handling.
  Continues to translate SIGTERM into ctx cancellation; only the
  poller's reaction changes.
- Restart-survival for in-flight Jobs.
- Metrics / observability beyond the existing heartbeats.
- Backoff jitter (upstream has it; drawbar doesn't; out of scope).
