# `/healthz` flags spurious wedge while poller blocks on capacity slot

**Status: fixed.** Landed in the poller-loop-reshape change (see
`docs/superpowers/specs/2026-05-05-poller-loop-reshape-design.md` and
`docs/superpowers/plans/2026-05-05-poller-loop-reshape.md`). The bug is
now structurally unrepresentable — the poll loop acquires its capacity
slot before calling `FetchTask`, so it can never block inside dispatch
with a stale `lastPoll` heartbeat. `/healthz` also suppresses the
poll-staleness 503 while a handler is in flight (`Poller.InFlight()`),
as a belt-and-suspenders measure. The single-context `Drain(timeout)`
was replaced with `Shutdown(ctx)` so kubelet SIGTERM gives in-flight
reporter flushes a graceful window before forced cancellation.

## Summary

The poll-loop staleness check added by the bug-007 round-1 fix
(`/healthz` returns 503 if `time.Since(LastPollAt) > stalenessThreshold`)
fires false positives when `dispatchTask` blocks waiting for a
capacity-semaphore slot. The threshold is 30s by default; with
`capacity: 1` and any task whose dispatch handshake (FetchTask reply
arrival → goroutine spawn → previous task's `defer <-p.sem` release)
takes >30s, kubelet liveness restarts the controller mid-task. The
in-flight job pod is orphaned, its log/state flush fails with
`context canceled`, and gitea sees the run as completed-failure with
truncated logs.

## Repro

Date: 2026-05-05. Image: `main-1777984724-64c85c8a`. capacity:1.

1. Run #75 (test workflow) processes a long-running task (cargo test
   --lib + bin/visual-test, ~13 minutes total).
2. Run #76 (cargo-build, dispatched right after #75) waits in queue.
3. After #75 completes, the poller's `<-p.sem` release inside the
   handler's `defer` runs. The next poll tick fires `FetchTask`,
   gets task 84 (run #77), calls `dispatchTask(task84)`. `dispatchTask`
   blocks at `pkg/server/poller.go:147` (`case p.sem <- struct{}{}:`)
   because the previous semaphore slot hasn't actually been released
   yet — the previous goroutine is mid-`defer` chain finishing
   reporter.Close (`flushState` retry loop, ~tens of seconds).
4. While the poller goroutine sits in that select, `lastPollNs` does
   NOT advance — `poll()` doesn't return until after `dispatchTask`
   returns.
5. 34s after the last RETURNED poll, `/healthz` (cmd/controller/main.go:401)
   logs `POLL LOOP WEDGED — dumping goroutines to stderr` and returns 503.
6. kubelet's livenessProbe sees the 503, SIGTERMs the controller.
7. Drawbar logs:
   - `context cancelled while waiting for capacity task_id=84`
   - 9× `final log flush failed, retrying ... canceled: context canceled`
   - `drain timed out, some tasks may still be running timeout=30s`
   - `runner shut down`
8. New pod schedules. The orphaned job pod for task 84 keeps running
   (it's a separate k8s Job, kubelet didn't touch it). Run #77 (job 82)
   eventually completes when the Job finishes, but its step-level state
   never gets reported back — the API shows all steps with
   `conclusion: failure` even though some succeeded, log retrieval is
   truncated, and artifacts are not uploaded.

## Why it happens

`pkg/server/poller.go:92-141` — `poll()` calls `dispatchTask` which can
block:

```go
func (p *Poller) dispatchTask(ctx context.Context, task *runnerv1.Task) {
    select {
    case p.sem <- struct{}{}:
    case <-ctx.Done():
        p.log.Warn("context cancelled while waiting for capacity", ...)
        return
    }
    p.wg.Add(1)
    go func() {
        defer p.wg.Done()
        defer func() { <-p.sem }()
        p.handler(ctx, task)
    }()
}
```

The `lastPollNs.Store(now)` at poller.go:110 runs only when the
**previous** `FetchTask` returns — i.e. AT THE TOP of the next poll
iteration. If `dispatchTask` sits waiting for a slot for >30s,
`poll()` doesn't return, the next tick can't fire (poll loop is
serial), and `lastPollNs` stays at its pre-task value.

`/healthz` (cmd/controller/main.go:373-415) sees the staleness and
correctly reports it — but the poller isn't *wedged*, it's *blocked
on legitimate backpressure*.

## Why this is a regression

Before commit 2e81477 (round-1 bug-007 fix), `/healthz` always returned
200. The wedge-detection-via-heartbeat was added specifically to catch
the silent poller-died case. It does that — but it also catches
legitimate capacity-wait, which previously was just "the poller is
busy doing its job."

The bug-010 h2-readidle fix is unaffected by this — it operates at the
TLS/h2 layer, independent of the poller's heartbeat.

## Fix space

Three options, ranked by least to most invasive:

1. **Update `lastPollNs` inside `dispatchTask` while blocking on the
   semaphore.** A small heartbeat goroutine, e.g.:

   ```go
   func (p *Poller) dispatchTask(ctx context.Context, task *runnerv1.Task) {
       heartbeat := time.NewTicker(5 * time.Second)
       defer heartbeat.Stop()
       for {
           select {
           case p.sem <- struct{}{}:
               goto acquired
           case <-heartbeat.C:
               p.lastPollNs.Store(time.Now().UnixNano())
           case <-ctx.Done():
               p.log.Warn("context cancelled while waiting for capacity", ...)
               return
           }
       }
       acquired:
       ...
   }
   ```

   Conceptually: "the poller is alive, it's just doing useful work."
   Doesn't touch `/healthz` logic or thresholds. ~10 lines.

2. **Track `dispatching` state separately and exclude it from staleness.**
   Add `p.dispatching atomic.Bool`; set true around `dispatchTask`,
   false after. `/healthz` ignores `lastPollNs` staleness while
   `dispatching` is true. More state, but the semantic is cleanest:
   "wedge = poller can't reach the server" not "wedge = no poll
   completion in N seconds".

3. **Raise the threshold.** Default `pollStaleness` is 30s; bump it to
   e.g. 5 minutes. Hides this bug but masks real wedges for that long.
   Don't pick.

Option 1 is the minimum-diff fix. Option 2 is more conceptually
correct but bigger.

## Severity

Today this fired during a run involving the visual-test pipeline
(>10 min total), restarting drawbar mid-task, losing the logs and
artifacts we were trying to capture. **It also creates ghost runs**
in gitea (job 82 above ended as `failure` with all steps failed even
though some likely succeeded — gitea was getting orphaned partial
state). For a CI runner this is a confidence-shaking failure mode:
"I push a commit, the controller restarts, my test run is unreliable."

This was the root cause of the run #77 / job 82 result we observed
when verifying `actions/upload-artifact@v3.2.2`. The artifact upload
step likely worked but its log + state flush was lost in the SIGTERM
storm.

## Related

- Bug 007 round-1 fix (commit 2e81477) introduced the `/healthz`
  wedge detection. This bug is the operational cost of that fix
  not accounting for legitimate capacity-wait.
- Bug 010 (h2 readidle) catches a different, real wedge mode.
- Bug 012 (cursor latches) — also surfaced this session — would have
  prevented the queue stacking that triggered today's repro by not
  letting runs pile up. Both bugs together produced the spectacular
  failure mode of "run completes, drawbar restarts, run reports
  failure with no useful logs."
