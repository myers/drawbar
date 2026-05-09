# `/healthz` successful-fetch heartbeat fires false positive while a long task runs

**Status: fixed 2026-05-05** in commit `94d0732` (`controller: gate
/healthz heartbeats on inBackoff and capacity`). Took fix-sketch
option 1: the successful-fetch staleness check at
`cmd/controller/main.go:477` is now suppressed while
`inFlight() == capacity` (no slot is free for the poller to issue a
new FetchTask). A real transport wedge surfaces the moment the
handler returns. Test coverage added in
`TestHealthzHandler_AtCapacitySuppressesSuccessfulFetch` and
`TestHealthzHandler_BelowCapacityExposesSuccessfulFetch`.

Original report follows.

---

Surfaced 2026-05-05 while shaking down image
`main-1778008209-cfda4091` (the bug 013 fix). Bug 013's healthz fix
suppresses the *poll-staleness* check while a handler is in flight,
but explicitly leaves the *successful-fetch* check unconditional. With
`capacity: 1` and any single task whose handler runs longer than the
`successFetchStaleness` window (default 120s — including all real cargo
build/test workloads), the poll loop legitimately can't fetch a *new*
task while the slot is held, so `lastSuccessfulFetchAt` never advances.
At 120s the controller logs `SUCCESSFUL FETCH WEDGED`, returns 503,
kubelet livenessProbe restarts it, and the in-flight job is orphaned
with the same SIGTERM-storm symptom as bug 013.

## Repro

Date: 2026-05-05. Image: `ghcr.io/myers/drawbar:main-1778008209-cfda4091`.
Config: capacity 1, `pollStaleness=30s`, `successFetchStaleness=120s`.

1. Push to `gt.monoloco.net/chaos-inc/bevy_xr_nitro` triggering both
   `cargo-build.yaml` (cargo build, ~1.5min) and `test.yaml` (cargo test
   --lib + bin/visual-test, ~13min). Two runs queued from one push (88+89).
2. Drawbar drains the cargo-build runs (tasks 87, 88, 89) back-to-back.
   Bug 012 is fixed — both runs pick up cleanly without manual dispatch.
3. Task 90 (test.yaml's `build` job) starts at 19:33:28; `cargo test --lib`
   begins at 19:33:40.
4. At 19:35:34 (≈114s of pure handler runtime, ≈126s since the last
   successful FetchTask reply) `/healthz` logs:
   ```
   SUCCESSFUL FETCH WEDGED — dumping goroutines to stderr
   kind=successful fetch since=126308480610 threshold=120000000000
   ```
   and returns 503.
5. Goroutine dump shows `Poller.Run` in `select [2 minutes]` at
   `poller.go:89` — the loop is healthy, just blocked on the (held)
   capacity slot. The TLS read on the gitea API connection is also
   IO-waiting normally; transport is fine.
6. kubelet SIGTERMs the controller. Logs:
   - `job watch error: client rate limiter Wait returned an error: context canceled`
   - 9× `final log flush failed, retrying ... canceled: context canceled`
   - On restart: `cleaning up orphaned job drawbar-run-90`
7. Gitea-side run 84 is left in `in_progress` with no further runner
   updates; controller restart can't recover it (bug 002 territory).

## Root cause

In `cmd/controller/main.go` (commit d4f8b77), bug 013's fix made the
`lastPoll` staleness 503 conditional on `inFlight() == 0`:

```go
if inFlight() == 0 {
    if t := lastPoll(); !t.IsZero() {
        if since := time.Since(t); since > pollStaleness {
            ...
        }
    }
}
if t := lastSuccessfulFetch(); !t.IsZero() {
    if since := time.Since(t); since > successFetchStaleness {
        // <-- unconditionally fires while a long handler holds the slot
    }
}
```

The commit message rationalizes leaving the second check unconditional:
> "This check is NOT suppressed during in-flight handlers: a transport
>  wedge while a long handler runs is still a real problem (bug 010)."

That reasoning conflates two things. With capacity > 1, the poller
*could* keep fetching new tasks while a long one runs and a transport
wedge would still surface as `lastSuccessfulFetch` going stale. With
capacity == 1 (the live config), the loop is *structurally unable* to
issue another FetchTask until the slot frees — so `lastSuccessfulFetch`
stays frozen *exactly when the controller is healthiest*. The
"unconditional" check has a real false-positive zone tied to handler
runtime, not transport health.

The 120s default lands roughly at "any cargo build". This is not edge
case — it fires on essentially every test run.

## Operational impact

- Same blast radius as bug 013: orphaned job pod, truncated logs,
  gitea-side run stuck in_progress, no automatic recovery.
- More likely to fire than bug 013 ever was, because 120s is hit by
  any non-trivial CI workload. Bug 013 needed a >30s dispatch handshake;
  this needs only a >120s task.
- Acceptance status of the drawbar plan is unaffected — caching/build
  speedups hit on short tasks where `lastSuccessfulFetch` does advance.
  But any "drawbar runs production workloads under load" claim is
  blocked until this is addressed.

## Fix sketch

Two viable approaches:

1. **Symmetrize with bug 013's fix.** Suppress the successful-fetch
   503 while `inFlight() > 0 && capacity == 1`, or more simply when
   `inFlight() == capacity` (no slot is free for the poller to use).
   Real transport wedges still surface the moment the handler returns
   — the next FetchTask either succeeds (clearing staleness) or fails
   and `lastSuccessfulFetch` advances toward staleness from there.
   Trade-off: a transport wedge during a handler is invisible to
   /healthz until the handler completes. Argument: kubelet restarting
   mid-handler doesn't help — it orphans the work — so deferring the
   503 is strictly better.

2. **Have the handler advance `lastSuccessfulFetch` on each successful
   reporter ping** (`UpdateLog`, `UpdateTask`). Those are the same
   transport; if they succeed, transport is alive. This generalizes
   the heartbeat: any successful gitea round-trip resets it, not just
   FetchTask. Bigger refactor but no false positive zone at all.

Option 1 is the smaller, safer change to ship now; option 2 is the
"correct" long-term shape.

## Related

- Bug 013 (closed): poll-staleness false positive during capacity wait.
  Same overall failure mode (kubelet kill mid-task) but a different
  heartbeat. Fix in d4f8b77 was scoped to `lastPoll`; this bug is the
  remaining hole.
- Bug 010 (closed): h2 ReadIdleTimeout. The motivating reason given
  for keeping `lastSuccessfulFetch` unconditional. This bug argues the
  reasoning doesn't hold at capacity == 1.

## Workarounds

- **Raise `success_fetch_staleness_threshold`** to comfortably exceed
  the longest expected single-task runtime. For our workloads
  (visual-test ≈ 13 minutes), `successFetchStaleness: 30m` would
  eliminate the false positive without giving up transport-wedge
  detection on idle controllers. This is a config change in
  `apps/base/gitea/drawbar/configmap.yaml`-equivalent.
- **Capacity > 1** (e.g. 2): the poller can fetch new tasks
  in parallel; `lastSuccessfulFetch` keeps advancing. Not appropriate
  for our RWO-cache setup but worth noting.

## Evidence

- Drawbar pod logs (previous instance):
  `kubectl -n gitea logs drawbar-7dcd89b4ff-z5lfp --previous` — shows
  task 87/88/89 (cargo-build) success in ~30s each, task 90 (cargo test)
  starts, 114s later the wedge fires.
- Goroutine dump: `Poller.Run` in `select [2 minutes]` at
  `poller.go:89`. Transport TLS read is IO-waiting normally.
- Gitea: run 84 (push event) stuck `in_progress` after restart;
  cleanup-on-startup deleted `drawbar-run-90` job.
