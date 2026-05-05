# Poller `tasksVersion` cursor latches to gitea's version, masks subsequent queued tasks

## Summary

`pkg/server/poller.go` updates its `tasksVersion` cursor on **every** successful
`FetchTask` response — including empty "no work" replies. After processing a
task, drawbar's local cursor sits at gitea's current `latestVersion`. Gitea only
calls `PickTask` when the runner-supplied `tasksVersion` differs from
`latestVersion`. So if any task was queued *before* the runner's last poll but
not picked up (e.g. a second workflow run from the same push), it sits there
indefinitely — gitea returns empty responses with the same `TasksVersion`
forever. The wedge breaks only when something independently bumps gitea's
counter (a new run is inserted, or a job is re-set to `waiting`).

This is a different failure mode from bug 007 (dead goroutine) and bug 010
(half-dead h2 conn): the poll loop is alive, transport is healthy, RPCs succeed
— `/healthz` correctly reports green and the kubelet does not restart the pod.

## Repro

Date: 2026-05-05. Image: `main-1777984724-64c85c8a`. Drawbar `capacity: 1`,
~88 min uptime, no restarts.

1. Single git push containing two workflow files
   (`.gitea/workflows/cargo-build.yaml` and `.gitea/workflows/test.yaml`).
   Gitea creates run #71 and run #72 from this push, each with one job
   labeled `ubuntu-latest`. Both runs are inserted in the same DB
   transaction sequence; each insert calls `IncreaseTaskVersion`
   (`models/actions/run.go:344`). Say version goes N → N+2.
2. Drawbar's first poll sends `tasksVersion=0`. Gitea
   (`routers/api/actions/runner/runner.go:158`): `0 != N+2`, calls
   `PickTask`. PickTask picks task 77 from run #71 (oldest waiting,
   ordered by `updated, id`), returns it with `TasksVersion: N+2`.
3. Drawbar (`pkg/server/poller.go:135`): `*tasksVersion = N+2`. Processes
   task 77 to completion.
4. Drawbar resumes polling, sends `tasksVersion=N+2` on every tick.
   Gitea: `N+2 == N+2` → empty response, `PickTask` not called. The
   `waiting` job from run #72 sits in the DB; the runner is idle.
5. 16 minutes elapse, all polls empty. `/healthz` returns 200,
   `LastPollAt` and `LastSuccessfulFetchAt` both fresh. Goroutine dump
   shows ~11 healthy goroutines, poller in `select`, h2 readloop idle.
6. External `workflow_dispatch` triggers run #73 →
   `IncreaseTaskVersion()` → version N+3. Next poll: `N+2 != N+3`,
   PickTask runs, returns task 77 of run #72 (still oldest waiting).
   Wedge resolved within one poll interval (~2 s).

## Why it happens

Drawbar's poll loop:

```go
// pkg/server/poller.go:131-141
p.backoff = 0
*requestKey = gouuid.New()
*tasksVersion = resp.Msg.GetTasksVersion()

if task := resp.Msg.GetTask(); task != nil && task.GetId() != 0 {
    p.log.Info("received task", "id", task.GetId())
    p.dispatchTask(ctx, task)
}
```

`*tasksVersion = resp.Msg.GetTasksVersion()` runs even on empty responses,
latching the local cursor to whatever gitea returned.

Gitea's FetchTask handler:

```go
// routers/api/actions/runner/runner.go:158
if tasksVersion != latestVersion {
    if t, ok, err := actions_service.PickTask(ctx, runner); err != nil { ... }
    ...
}
```

PickTask is gated by `tasksVersion != latestVersion`. The version is only
bumped on:
- new run insert with at least one waiting job (`models/actions/run.go:344`)
- job re-set to waiting on rerun (`models/actions/run_job.go:128-132`)

Neither fires on `PickTask` itself or on `UpdateTask` (task completion). So
once both sides agree on the version, drawbar is "told" there's no work
even when there is.

## Why bug-007 / bug-010 fixes don't catch it

- Poll goroutine is alive: `lastPollNs` advances every tick (poller.go:110).
- RPCs succeed: empty `FetchTaskResponse` → nil error → `lastSuccessfulFetchNs`
  advances every tick (poller.go:114-116).
- Both `/healthz` thresholds stay satisfied; kubelet has no signal to act on.
- pprof goroutine dump shows nothing wrong: select in `Run`, no in-flight
  RPC, h2 readloop idle but healthy.

The wedge is at the application protocol layer, not the transport layer.

## Fix space

Three options were considered initially:

1. **Send `tasksVersion=0` on every poll.** Forces gitea to call `PickTask`
   on every request. Costs one extra DB query (`PickTask`) per poll
   interval — at 2 s default that's a constant trickle, well within what
   gitea handles for a single runner. The `tasks_version` mechanism was
   designed as a polling-rate optimisation; with one runner and a 2 s
   long-poll, the optimisation is moot.

2. **Update the cursor only when a task was returned.** Move
   `*tasksVersion = resp.Msg.GetTasksVersion()` inside the
   `if task := ...; task != nil` block. Originally pitched as the
   minimum-diff fix, but **does not actually fix this repro**: receiving
   task 77 of run #71 advances the cursor to N+2, which equals gitea's
   `latestVersion`, so the next poll still hits the version-unchanged
   gate and PickTask is not called for run #72.

3. **Periodic cursor reset.** Force `tasksVersion=0` every N seconds
   regardless. Worst of both worlds; do not pick this.

## Fix taken

**Option 4 — match upstream `act_runner`.** Verified against
`reference/runner/internal/app/poll/poller.go:231-285` (gitea's own
runner). The pattern is:

- On any successful response, advance the cursor only **forward** to
  `resp.TasksVersion` (so the idle/no-work fast path keeps working).
- **When a task is delivered, reset the cursor to 0** so the very next
  poll forces gitea to call `PickTask` again — catching the second task
  queued at the same version.

Upstream comments this explicitly: "got a task, set `tasksVersion` to
zero to force query db in next request." This is strictly better than
option 1: idle deployments still hit the fast path, but bursts of two+
tasks at the same version are picked up immediately.

Implemented at `pkg/server/poller.go::poll`. Regression test
`TestPoller_DoesNotLatchCursorOnEmptyResponse` in `poller_test.go` uses
a `giteaLikePollerClient` that mirrors gitea's `tasksVersion !=
latestVersion` gating, enqueues two tasks, and asserts both are
delivered without an external version bump. The test fails against
option 2 and passes against option 4.

## Test sketch

In `pkg/server/poller_test.go`, a test that drives a `mockPollerClient`
returning:

1. First call: `{Task: task42, TasksVersion: 5}`.
2. Subsequent calls: `{TasksVersion: 5}` (empty, same version).

Then assert that the second response with `TasksVersion: 5` does NOT
prevent drawbar from continuing to send polls — and (with the proposed
fix) that the `tasksVersion` field in subsequent `FetchTaskRequest`
calls remains 0 (option 1) or stays at 5 only because no task was
received (option 2). Combine with a third response
`{Task: task43, TasksVersion: 6}` to verify the second task is
delivered and dispatched. Today's code passes the test only because
the mock never bothered to gate on `tasks_version` — the
`giteaFetchHandler` in `fetchtask_idempotency_test.go` returns empty
without checking the request's version. A faithful gitea mock would
expose this bug in unit tests.

## Related

- Bug 007 (poller dies silently): different failure mode (goroutine death),
  same observability gap (silent wedge looks identical from the outside).
- Bug 010 (h2 readidle): different failure mode (half-dead conn), same
  observability gap. Round-2 belt-and-suspenders heartbeat in 010 doesn't
  catch this — `lastSuccessfulFetchNs` advances normally because empty
  responses are healthy round trips.
- `GITEA_FETCHTASK_BUG.md` documents a separate, complementary gitea-side
  bug (no idempotency on `x-runner-request-key`). That one is server-side;
  this one is client-side and fixable in drawbar alone.
