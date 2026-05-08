# Post-exit log drain races container teardown, reports success as failure

**Status: fixed 2026-05-07.** Surfaced 2026-05-07 during capacity-2 shakedown of
image `main-1778113050-2e31ce18`. When a runner pod's container exits
cleanly (exitCode 0), drawbar's post-exit log-drain path attempts to
upgrade an exec connection to the (now-terminated) container, gets
`container not found` from the kubelet, and reports the task as
`result: 2` (failure) to gitea — even though the workflow itself
succeeded. The gitea-side log shows a successful build followed by
`Job execution error: runner container status not found` as the final
line, and the run is marked `failure`.

This bug is independent of capacity, but parallel runs at capacity 2
make it more visible: the snapshot-cache asymmetry means one of two
parallel jobs typically gets a hot cache and finishes very fast (~24s
in this repro), shrinking the window in which the post-exit drain
needs to complete before the kubelet GCs the container.

## Repro

Image: `ghcr.io/myers/drawbar:main-1778113050-2e31ce18`. Capacity 2.
Repo: `gt.monoloco.net/chaos-inc/bevy_xr_nitro`, run 93, job 98
(cargo-build), task id 99 — fired in parallel with run 94 / task 100
from a single push.

Drawbar controller log timeline (`drawbar-5d99d97c58-lb9cl`):

```
10:53:32.610 received task id=99
10:53:32.610 executing task id=99
10:53:32.611 parsed workflow task_id=99 job_id=build steps=3
10:53:32.634 created workspace PVC from snapshot pvc=cache-99 snapshot=snap-97
10:53:32.634 snapshot cache PVC ready pvc=cache-99 ... restored=true
10:53:32.647 created k8s job drawbar-run-99
10:53:32.661 pod created drawbar-run-99-4npp9
10:53:38.212 runner container started pod=drawbar-run-99-4npp9
        ... cargo build runs to completion ...
10:54:06.798 WARN  post-exit state drain stream error
                err="unable to upgrade connection: container not found (\"runner\")"
                lastOffset=5
10:54:06.810 ERROR job watch error
                error="runner container status not found"
10:54:07.058 task completed task_id=99 result=2
```

Pod's actual container state at the time drawbar reported failure
(`kubectl get pod drawbar-run-99-4npp9 -o jsonpath='{.status.containerStatuses[0]}'`):

```json
{
  "name": "runner",
  "state": {
    "terminated": {
      "exitCode": 0,
      "reason": "Completed",
      "startedAt": "2026-05-07T10:53:37Z",
      "finishedAt": "2026-05-07T10:54:05Z"
    }
  }
}
```

Gitea-side log tail (job 98):

```
2026-05-07T10:54:05.78  Finished `dev` profile [optimized + debuginfo] target(s) in 23.57s
2026-05-07T10:54:06.81  Job execution error: runner container status not found
```

Net: container exited clean at 10:54:05, drawbar saw `container not
found` at 10:54:06.798 (≈1s later), reported failure at 10:54:07.058.
The user-visible run on gitea is `failure` despite cargo emitting
`Finished 'dev' profile`.

## Root cause hypothesis (unverified)

Drawbar appears to have two independent post-exit pipelines watching
the runner container:

1. A **log/state drain stream** that uses `kubectl exec`-style
   connection upgrade (or the kubelet's exec endpoint directly) to
   read final stdout/stderr. The `unable to upgrade connection:
   container not found ("runner")` error is the canonical kubelet
   response when an exec request lands after the container has
   terminated and been removed from the pod's running container set
   (the pod itself is still around in `Phase=Succeeded`, but the
   exec target no longer exists).
2. A **job watcher** that reads `pod.status.containerStatuses[0]` to
   determine the exit code and translates it to a task `result`.

The race appears to be that path (1) sees `container not found` from
its drain attempt, propagates that error up, and path (2) maps that
error to `result: 2` (failure) instead of falling back to reading
the pod's `containerStatuses[0].state.terminated.exitCode` which is
already populated and equals 0.

A clean implementation of post-exit drain has to handle the inherent
race between "container exits" and "drain attempt": once the
container has terminated, the source of truth is
`status.containerStatuses[*].state.terminated.exitCode`, not an
exec/log stream. The kubelet does not promise to keep the container
process accessible for any window after exit.

## Why this is a real bug, not just noise

- It misreports successful builds as failures on the gitea-visible
  run status. PR gating, automation, and dashboards all read that
  status; a green run that drawbar reports as failure is
  indistinguishable from a real failure to consumers.
- It does NOT affect the actual workflow output — the cargo build
  artifacts are intact, the cache snapshot is created, the
  workspace PVC reflects success. The damage is purely at the
  reporting layer.
- It compounds bug 016: bug 016 makes per-step status unreliable
  but the overall job conclusion still tracks reality. This bug
  inverts the overall job conclusion, which is the *one* signal
  bug 016 didn't damage. Together they make drawbar's status
  reporting close to useless without parsing logs.

## Operational impact

- Most visible at capacity > 1 with mixed cache states, because
  the cache-warm parallel job exits in tens of seconds vs. the
  cache-cold job's many minutes. The fast exit shrinks the post-exit
  drain window.
- At capacity 1 with consistently warm caches it can also fire on
  any quick build (any task < ~30s wall time). At capacity 1 with
  cold caches, builds take several minutes and the drain window is
  comfortably wide; the race effectively never fires there. This
  matches our observation: bugs 014/015 verifications across many
  ~3min jobs at capacity 1 never tripped this bug, but the very
  first capacity-2 dispatch with a warm cache did.
- Doesn't affect bug 014/015 fixes — the controller stays up,
  `lastSuccessfulFetch` advances, no kubelet kill.
- The companion run 94 (test workflow, cold cache, takes minutes)
  was unaffected — its container had not terminated when drawbar
  finished its setup, so the drain attempt landed on a live
  container.

## Fix sketch

Two viable shapes:

1. **Trust `containerStatuses[*].state.terminated.exitCode` as the
   source of truth for task result.** When the post-exit drain
   stream errors with `container not found`, the controller should
   re-read the pod status, and if `terminated.exitCode == 0`, report
   `result: success`. The drain error means "we couldn't capture
   the very last bytes of stdout"; it does not mean the workflow
   failed. At most, log a warning that some terminal log tail may
   be missing.

2. **Drain pre-exit, not post-exit.** Use the streaming `kubectl
   logs --follow` API (which works on terminated containers from
   the kubelet's log buffer until kubelet GC) instead of an exec
   upgrade. `kubectl logs` survives container termination as long
   as the pod is around; exec/attach do not. This removes the race
   entirely.

(2) is the larger refactor; (1) is the smaller fix that restores
correctness immediately. Both are worth, in that order — (1) so
nobody is misled today, (2) so the log tail isn't lost on fast jobs.

A combined fix: the drain layer can use `kubectl logs --follow`
*and* the result-reporter can fall back to `containerStatuses` on
any drain error. Either alone closes the bug.

## Related

- Bug 016 (filed): step reporter mis-attributes start/end times
  and conclusion across sequential steps. Same general theme of
  "drawbar's reporting layer is loose" — different mechanism (per-
  step transitions) but same outcome (gitea sees the wrong story).
- Bug 014/015 (closed): controller-side wedge handling. Not
  related — those concern the controller staying alive; this one
  is about how the controller reports results.

## Workarounds

- **Capacity 1 with cold caches** sidesteps this bug by accident.
  Any non-trivial cargo build runs many minutes, far exceeding the
  post-exit drain window. The 014/015 shakedown completed at
  capacity 1 with this bug latent.
- **Re-trigger** is the user-side workaround once you notice
  drawbar reported failure on a build that obviously succeeded
  (the log shows `Finished` near the end). This is bad UX though
  — it requires reading logs to disbelieve the status.
- **No drawbar-config knob** mitigates this; the path is in the
  Go code.

## Evidence

- Drawbar controller logs:
  `kubectl logs -n gitea drawbar-5d99d97c58-lb9cl --since=10m` —
  contains the `post-exit state drain stream error` and
  `runner container status not found` lines pasted above.
- Pod final status:
  `kubectl get pod drawbar-run-99-4npp9 -n gitea -o
  jsonpath='{.status.containerStatuses[0].state}'` — shows
  `terminated.exitCode: 0`, `reason: Completed`.
- Gitea job log:
  `gt api 'repos/chaos-inc/bevy_xr_nitro/actions/jobs/98/logs'` —
  contains `Finished 'dev' profile [optimized + debuginfo]
  target(s) in 23.57s` followed by `Job execution error: runner
  container status not found`.
- Run 93 status (gitea): `completed / failure`.
- Run 94 (companion task 100, cold cache, slower): unaffected;
  ran to completion under the same controller without tripping
  this bug — confirming the race is sensitive to fast container
  exit, not to capacity-2 itself.

## Resolution

`pkg/k8s.watchJobWith` now waits for the runner container to reach
`State.Terminated` on BOTH the EOF and non-EOF log-stream exit paths,
not just non-EOF. On EOF the kubelet has GC'd the running-container
set (so post-exit drain exec returns "container not found") but the
pod-status update with `terminated.exitCode` lags slightly — without
the wait, `getContainerResult` saw `State.Running` and returned
`runner container status not found`. With the wait,
`getContainerResult` reliably reads the populated exit code.

Drain errors continue to be swallowed at WARN — they signal "we may
have lost the very last bytes of stdout/state.jsonl," which is a
reporting-loss concern, not a job-result concern. Fix sketch (2)
from above (drain pre-exit using `kubectl logs --follow` semantics)
is left as future work; the `kubectl logs` path here already uses
follow, so most of stdout is captured live — only the trailing few
state events written after log EOF can be missed.

Regression test: `TestWatchJobWith_EOFWaitsForTerminationStatus` in
`pkg/k8s/watcher_test.go` reproduces the kubelet-status-update race
with a fake clientset and exec executor that returns the literal
"unable to upgrade connection: container not found" string.
