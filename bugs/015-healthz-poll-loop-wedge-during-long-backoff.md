# `/healthz` poll-loop heartbeat fires false positive during normal backoff

**Status: fixed 2026-05-05** in commit `94d0732` (`controller: gate
/healthz heartbeats on inBackoff and capacity`). Took the
"`inBackoff()` predicate" approach from the fix sketch: the poller
exposes an `inBackoff` accessor, and the poll-staleness check at
`cmd/controller/main.go:461` is now suppressed when it returns true.
A wedged sleep timer would still surface, but a normal `waitBackoff`
phase no longer kicks the kubelet. Tests:
`TestHealthzHandler_BackoffSuppressesPollStaleness` and friends.

Original report follows.

---

Surfaced 2026-05-05 immediately after bug 014, on
image `main-1778008209-cfda4091`. The `lastPoll` heartbeat is updated
only when a `FetchTask` call RETURNS (success/error), but the poller's
own `waitBackoff` can sleep for up to `backoffMax` (60s default)
between consecutive errors. With `pollStaleness` defaulting to 30s,
any backoff phase ≥ 30s trips `/healthz` and kubelet restarts the
controller — restarting the *recovery loop* instead of letting it
recover.

## Repro

Date: 2026-05-05. Image: `ghcr.io/myers/drawbar:main-1778008209-cfda4091`.
Trigger sequence:

1. Bug 014 fires (successful-fetch wedge, kubelet restart, run 84
   orphaned).
2. On restart, drawbar `cleanup orphaned job` deletes `drawbar-run-90`,
   then begins polling.
3. Gitea still has run 84 / task 90 in `in_progress` state with this
   runner ID. Either:
   - FetchTask returns 409/conflict, or
   - FetchTask returns success with no work (consecutiveEmpty climbs)
4. `Poller.waitBackoff` (`pkg/server/poller.go:206`) doubles the sleep:
   2s → 4s → 8s → 16s → 32s → 60s (cap).
5. After ~60s into the 60s backoff, `lastPoll` is now 60s old.
   `/healthz` (cmd/controller/main.go) fires:
   ```
   POLL LOOP WEDGED — dumping goroutines to stderr
   kind=poll loop since=37344635881 threshold=30000000000
   ```
   (Goroutine dump shows `Poller.Run` in select at `poller.go:89`
   waiting on the backoff timer — i.e. the loop is healthy, just sleeping.)
6. kubelet SIGTERMs the pod. On restart, the cycle repeats — so the
   pod restart-loops at ~90s intervals (60s backoff + ~30s wedge
   detection + restart) until something external clears the gitea-side
   stuck task state or the consecutive errors reset.

## Why this is bug 013's reasoning falling apart

Bug 013's fix was: "while a handler is in-flight, suppress the poll
staleness check." The premise was that `lastPoll` only goes stale
during one of two conditions:

- The poll goroutine genuinely deadlocked (real wedge).
- The poll goroutine is legitimately blocked on capacity acquisition
  (false positive — that's bug 013).

But `lastPoll` *also* doesn't advance during a normal `waitBackoff`
sleep, which can legitimately exceed `pollStaleness` whenever
`backoffMax > pollStaleness`. The current defaults
(`backoffMax = 60s`, `pollStaleness = 30s`) make this guaranteed every
time the runner hits sustained errors — exactly when it's *trying to
back off and let the server recover*.

In other words, the heartbeat semantics are wrong: `lastPoll` is "time
since last RPC return," but the failure mode the heartbeat wants to
detect is "the goroutine is stuck and will never recover." A goroutine
in a `time.NewTimer` select is alive and recovering.

## Fix sketch

Update `lastPoll` (or a sibling heartbeat) at the *start* of the
backoff sleep too, or — equivalently — define the wedge as "the poll
goroutine is not making forward progress through its state machine."
Simpler: record `lastPoll` whenever the timer ticks past inside
`waitBackoff` (or just before `select { case <-timer.C ... }`).

Or: make the threshold `pollStaleness` larger than `backoffMax`
unconditionally. With `pollStaleness = backoffMax * 2 + slack`, a
normal backoff cycle won't trip the wedge. But this couples two
unrelated values and gives slow wedge detection in normal operation.

The cleanest fix mirrors bug 013's approach: introduce a `inBackoff()`
predicate the poller exposes, and have `/healthz` suppress the
poll-staleness check while it's true. Combined with bug 014's fix
(suppress successful-fetch staleness while a handler is in flight),
the healthz semantics become:

- Wedge IFF (no in-flight handler AND not in backoff AND poll stale)
  OR (no in-flight handler AND fetch stale).

## Operational impact

- Pod restart-loops while gitea has stuck task state. Each restart
  triggers the orphaned-job cleanup which is fine, but the pod never
  reaches a steady "polling normally" state long enough to drain the
  next queued task — so the runner is *gone* until manual intervention
  (delete the stuck gitea-side task, or wait for it to time out
  server-side).
- During the restart-loop, no work runs — both the cargo-build queue
  and any new pushes pile up.
- Together with bug 014, this turns "single long task" into "runner is
  dead until human intervenes."

## Workarounds

- Bump `pollStaleness` past `backoffMax`: e.g. set the staleness
  threshold to ≥120s when `backoffMax` is 60s. Currently neither value
  is configurable, so this is a code change too.
- Reduce `backoffMax` below `pollStaleness`. Hurts polite-backoff
  behavior, helps wedge detection.
- Don't use this runner for any workload that produces sustained
  error returns from FetchTask (e.g., gitea task-state inconsistency).

## Evidence

- Drawbar pod restarts: 3 in 9m38s after image bump cfda4091.
- Logs show backoff sequence 4s → 8s → 16s → 32s → 60s, then
  POLL LOOP WEDGED at 37s into the 60s phase, then SIGTERM, restart.
- Goroutine dump confirms `Poller.Run` is alive in the backoff timer
  select.
