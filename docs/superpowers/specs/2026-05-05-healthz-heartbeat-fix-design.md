# `/healthz` heartbeat fix: bugs 014 and 015

**Date:** 2026-05-05
**Status:** design
**Closes:** [bug 014](../../../bugs/014-healthz-successful-fetch-false-positive-during-long-task.md), [bug 015](../../../bugs/015-healthz-poll-loop-wedge-during-long-backoff.md)

## Problem

`/healthz` has two staleness checks. Both produce false positives that cause
kubelet to restart the controller mid-work, orphaning jobs and leaving the
gitea-side run stuck in `in_progress`.

- **Bug 014.** `lastSuccessfulFetch` goes stale during any handler that runs
  longer than `successFetchStaleness` (default 120s) when `capacity == 1`,
  because the poll loop is structurally unable to issue another `FetchTask`
  while the slot is held. Hits roughly every cargo build.

- **Bug 015.** `lastPoll` does not advance during `waitBackoff`'s sleep
  (up to `backoffMax = 60s`). With `pollStaleness` floored at 30s, any
  sustained-error backoff cycle trips `/healthz`. The pod restart-loops
  while gitea has stuck task state — restarting the recovery loop instead
  of letting it recover.

Both bugs share a root cause: the heartbeats record "time since last RPC
return," but the failure mode `/healthz` is supposed to detect is "the
poll goroutine is wedged and will never recover." A goroutine in a healthy
`select` (timer or held semaphore) is alive.

## Goals

- Eliminate the two false-positive zones so `/healthz` no longer triggers
  kubelet restarts during normal long-running tasks or normal backoff.
- Keep detection of real wedges — transport stuck mid-RPC, ticker dead,
  goroutine deadlocked.
- Match the shape of bug 013's existing fix (`InFlight()` predicate +
  guarded staleness check) so the controller's healthz semantics stay
  understandable in one read.
- No new config knobs. `pollStaleness` and `successFetchStaleness` keep
  their current defaults.

## Non-goals

- Generalizing `lastSuccessfulFetch` to advance on any successful gitea
  round-trip (e.g. `UpdateLog`, `UpdateTask`). That is the option-B
  refactor in bug 014's fix sketch and remains a future improvement;
  this design does not block it.
- Making `backoffMax` configurable. The predicate-based fix obsoletes the
  "tune the threshold past backoff" workaround.
- Changing `/readyz` or `/metrics/active-jobs`.

## Design

Rewrite `/healthz`'s wedge predicate.

Today:

```
wedge := (lastPoll stale && inFlight == 0) || (lastSuccessfulFetch stale)
```

After:

```
wedge := (lastPoll stale && inFlight == 0 && !inBackoff)
     || (lastSuccessfulFetch stale && inFlight < capacity)
```

The semantic guarantee: `/healthz` returns 503 only when the poll
goroutine is structurally unable to make progress and there is no
in-flight handler that would drive progress on resumption. Every
false-positive zone in bugs 014 and 015 maps to one of the two new
guards.

### Components

**`pkg/server/poller.go`:**

- New field `inBackoff atomic.Bool` on `Poller`.
- New method `InBackoff() bool`, parallel to `InFlight()`.
- `waitBackoff` flips the flag true before its `select`, deferred reset
  to false on return. The flag is set on every backoff call, not just
  long ones — keeps semantics simple ("in backoff sleep" = "not in
  active fetch"). Even short backoffs (2s base) briefly toggle it,
  which is fine because the staleness windows are seconds-to-minutes.

**`cmd/controller/main.go`:**

- `healthzHandler` gains two parameters: `inBackoff func() bool` and
  `capacity int64`. Both required (no zero-value sentinels).
- The poll-staleness branch's existing guard `if inFlight() == 0`
  becomes `if inFlight() == 0 && !inBackoff()`.
- The successful-fetch branch gains a guard:
  `if inFlight() < capacity` wrapping its check.
- `startHealthServer` picks up the same two params and threads them
  through.
- `run()` passes `poller.InBackoff` and `int64(cfg.Runner.Capacity)`
  to `startHealthServer`.

No types changes. No config changes. No Helm or values.yaml changes.

### Capacity boundary

The successful-fetch guard uses strictly less-than: `inFlight < capacity`.

- At `inFlight == capacity` the poller is blocked on `p.sem <- struct{}{}`
  and physically cannot issue a `FetchTask`. A stalled successful-fetch
  heartbeat is not a transport signal in this state.
- At `inFlight == capacity - 1` (capacity > 1) one slot is free, the
  poller can fetch in parallel, and a stalled successful-fetch heartbeat
  is again a real signal.
- At capacity 1, `inFlight < 1` is equivalent to `inFlight == 0`. Both
  heartbeats are suppressed equally while a handler runs. Using the
  same `< capacity` form everywhere keeps the code readable for the
  capacity > 1 case.

### Accepted trade-off

When `inFlight == capacity` and the transport is genuinely wedged,
`/healthz` will not 503 until at least one handler completes. The
ceiling on time-to-detection becomes "longest in-flight handler
runtime + `successFetchStaleness`."

This is strictly better than the current behavior. Kubelet restarting
mid-handler does not help — it orphans the work — so deferring the 503
is the operational win bug 014 calls out. The next `FetchTask` after
the handler completes either succeeds (clearing staleness) or fails
and advances toward staleness from there.

## Data flow

### Bug 014 (long handler at capacity 1)

1. Task dispatches; handler goroutine starts. Poller loop returns to
   step 1 of `Run`, blocks on `p.sem <- struct{}{}`.
2. `inFlight == 1`, `inBackoff == false`.
3. `/healthz` at t+120s: poll-staleness gated by `inFlight == 0` →
   skipped. Successful-fetch gated by `inFlight < capacity` → `1 < 1`
   is false → skipped. 200.
4. Handler completes. Slot frees, poller fetches, both heartbeats
   advance, `/healthz` returns to its normal logic.

### Bug 015 (long backoff during sustained errors)

1. `FetchTask` returns error, `consecutiveErrors` climbs to 5, backoff
   reaches the 60s cap.
2. `waitBackoff` flips `inBackoff` true, enters `select` on the timer.
3. `inFlight == 0`, `inBackoff == true`.
4. `/healthz` at t+30s into the sleep: poll-staleness gated by
   `!inBackoff` → skipped. Successful-fetch's new guard
   (`inFlight < capacity`) is satisfied (`0 < 1`), so that branch is
   evaluated normally; its heartbeat last advanced when the most
   recent erroring `FetchTask` returned. If errors persist past
   `successFetchStaleness`, this branch will legitimately 503 — that
   is a real "transport not getting through" signal.
5. Timer fires, `inBackoff` flips false, next `FetchTask` begins.
   On success both heartbeats reset.

## Edge cases

**`inBackoff` race with `lastPoll` advance.** `lastPoll` is updated
inside `fetchTask`, before `waitBackoff` is called. The sequence is
`fetchTask returns → lastPoll updated → waitBackoff sets inBackoff=true
→ sleep → inBackoff=false → loop continues`. There is no window where
`inBackoff` is true and `lastPoll` is about to be updated by the same
iteration. The worst case for a `/healthz` probe is reading both flags
during the few-nanosecond loop turnover; well within the 30s threshold
floor.

**`inBackoff` true on first iteration.** Cannot happen. `waitBackoff`
runs only after a failed `fetchTask`.

**Shutdown.** `waitBackoff` returns false if its context is cancelled,
exiting the loop. The deferred reset still fires. After `Run` returns,
`inBackoff` is false; `/healthz` keeps reflecting reality.

**`onWedge` once-semantics.** Unchanged. Still fires once per process
lifetime on the first 503. The new guards correctly suppress 503s that
are not real wedges, so the dump fires on a real wedge instead of a
false alarm.

**Capacity passed by value.** `cfg.Runner.Capacity` is set once at
startup and immutable; an `int64` is simpler than `func() int64`. If
capacity ever becomes dynamic, this changes — not now.

**Backwards compatibility.** `healthzHandler`'s signature changes.
All call sites are inside `cmd/controller/`. The existing healthz tests
break and need updating; that is the only ripple.

## Testing

### `cmd/controller/main_test.go` — extend `TestHealthzHandler_*`

- `TestHealthzHandler_BackoffSuppressesPollStaleness` — `inFlight=0`,
  `inBackoff=true`, stale `lastPoll`, fresh `lastSuccessfulFetch`.
  Expect 200, `onWedge` not called.
- `TestHealthzHandler_BackoffEndsExposesStaleness` — same setup with
  `inBackoff=false`. Expect 503, `kind="poll loop"`.
- `TestHealthzHandler_AtCapacitySuppressesSuccessfulFetch` —
  `inFlight==capacity`, stale `lastSuccessfulFetch`, fresh `lastPoll`.
  Expect 200. Run for capacity=1 and capacity=2.
- `TestHealthzHandler_BelowCapacityExposesSuccessfulFetch` —
  `inFlight<capacity` (1 of 2), stale `lastSuccessfulFetch`. Expect
  503, `kind="successful fetch"`.
- `TestHealthzHandler_BothBranchesIndependent` — `inFlight==0`,
  `inBackoff==false`, both heartbeats stale. Expect 503 for poll loop
  (first branch wins, matches existing `dumpOnce` behavior).
- Update existing `TestHealthzHandler_*` tests to pass the new
  `inBackoff` and `capacity` parameters. Default `inBackoff` to a
  `func() bool { return false }` and capacity to `1` where the value
  does not matter to the assertion.

### `pkg/server/poller_test.go`

- `TestPoller_InBackoff_FlagDuringWait` — drive the poller against a
  stub `PollerClient` that returns errors. With a short `FetchInterval`,
  observe `InBackoff()` flips true during `waitBackoff` and false
  outside. Synchronize via channel signaling or bounded polling of
  `InBackoff()`.
- `TestPoller_InBackoff_FalseOnStartup` — false before the loop starts
  and false after `ctx` cancellation.
- `TestPoller_InBackoff_ResetAfterShutdown` — enter backoff, cancel
  context, confirm `InBackoff() == false`.

### Manual smoke test on the dev cluster

- Rebuild image, redeploy, push to `chaos-inc/bevy_xr_nitro` to trigger
  the same `test.yaml` workload that surfaced bug 014.
- Confirm `/healthz` stays 200 throughout the 13-minute `cargo test`.
- Confirm `/healthz` stays 200 through any subsequent backoff cycle
  (e.g. by stopping gitea briefly to force errors).

## Open questions

None.

## Out of scope (future)

- Option B from bug 014's fix sketch: have the reporter advance
  `lastSuccessfulFetch` on every `UpdateLog`/`UpdateTask` round trip.
  This makes the heartbeat genuinely tied to "any successful server
  round trip" rather than just `FetchTask`. The current design does
  not block this — the `inFlight < capacity` guard would simply become
  redundant once the heartbeat advances during handlers.
- A pluggable `Healthz` predicate set, exposing `inBackoff` /
  `inFlight` via a single struct rather than three function arguments.
  Worth considering if a third predicate appears.
