# Drawbar poller can stop silently with no observable signal

## Summary

After a single task completed (success or failure), the drawbar runner
silently stopped calling `FetchTask` against the gt server. The pod stayed
`Ready 1/1`, `/healthz` returned `ok`, and structured logs showed only the
last `task completed` line — followed by 6+ minutes of total silence.
Meanwhile, a new workflow run sat at `status: queued` in the gt UI with
zero indication that no runner was claiming it.

Forced recovery required `kubectl delete pod`. After restart the same
runner immediately picked up the queued task.

## Repro

Date: 2026-05-01 14:00 UTC. Image: `ghcr.io/myers/drawbar:main-1777642724-6229f270`.
Single drawbar pod with `capacity: 1`. Sequence:

1. Push commit A → drawbar receives task 22, fails fast, completes.
2. Push commit B → drawbar receives task 23, fails fast, completes.
3. Push commit C → drawbar receives **nothing**. Task 24 sits queued.

Drawbar log tail (everything since boot relevant to polling):

```
{"level":"INFO","msg":"runner is online, polling for tasks","job_namespace":"gitea"}
{"level":"INFO","msg":"poller started","interval":2000000000,"capacity":1,"ephemeral":false}
{"level":"INFO","msg":"received task","id":22}
... task 22 work ...
{"level":"INFO","msg":"task completed","task_id":22,"result":2}
{"level":"INFO","msg":"received task","id":23}
... task 23 work ...
{"level":"INFO","msg":"task completed","task_id":23,"result":2}
<6 minutes of silence>
```

Gitea HTTP log over the same window: zero
`POST /api/actions/runner.v1.RunnerService/FetchTask` requests after the
last task's `UpdateTask`.

After `kubectl delete pod drawbar-...`:

```
{"level":"INFO","msg":"poller started"...}
{"level":"INFO","msg":"received task","id":24}    # the queued task
```

So the underlying gRPC dispatch worked once a fresh poller existed; the old
poller had stopped without surfacing why.

## Why it's hard to diagnose

Four overlapping observability gaps make this look identical to "polling
but no work to do":

### 1. No periodic poll heartbeat

The poller logs `poller started` once and `received task` only on a hit.
There's no per-poll log line, not even at DEBUG. So a dead poll loop and a
healthy-but-idle poll loop produce the same output (none).

**Suggested fix:** `slog.Debug("polling")` per FetchTask call, or at minimum
a `slog.Warn("no tasks fetched in ${duration}")` if FetchTask hasn't
returned a task in N minutes (e.g. 5).

### 2. `/healthz` doesn't reflect poller liveness

`/healthz` returns `ok` while the poll goroutine is dead. Kubernetes can't
restart the pod because nothing reports the failure.

**Suggested fix:** track `lastPollAt time.Time` in the poller; have
`/healthz` (or a new `/livez`) return non-200 if `time.Since(lastPollAt) >
PollInterval * 5` (or similar). With a `livenessProbe`, k8s restarts the
pod automatically.

### 3. No "0 runners online" signal in the UI

A queued task with no eligible runner is indistinguishable in the gt UI
from a queued task that's about to start. This is partly a gitea
limitation, but drawbar could publish "matching, idle" runners more
visibly via Declare or via labels in the runner list endpoint.

### 4. Job pod artefact left behind

After task 23 failed, `Job/drawbar-run-23` and its pod remained in the
namespace as `Failed`. If the controller crashed during the watch, this
might never clean up. Setting
`spec.ttlSecondsAfterFinished: 600` on the created Job would let kube-controller
GC them automatically and reduce noise.

## Acceptance

- A `slog.Warn` (or non-200 healthz) appears within 5 minutes of the
  poller going silent without a new task.
- A liveness probe configured against drawbar restarts the pod when the
  poll loop dies.
- `Job` artefacts get cleaned up via `ttlSecondsAfterFinished`.
- (Stretch) the gt UI gains a runner-availability indicator.

## Related

This was uncovered during the bug 006 verification. The actual blocker for
bug 006 is solved; this bug is the "we wouldn't have known drawbar was
broken without manually correlating gitea HTTP logs with drawbar logs"
finding. The cause of the poller dying is unknown — would need to add
the heartbeat first to even capture it next time.

## Status

- Heartbeat: `Poller.LastPollAt()` now records the wall-clock time of every
  `FetchTask` attempt, plus a per-poll `slog.Debug("polling")`.
- `/healthz`: returns 503 once `time.Since(LastPollAt) > max(FetchInterval*10, 30s)`.
  At default 2 s interval the threshold is 30 s; combined with the existing
  Helm `livenessProbe` (`periodSeconds: 10`, default `failureThreshold: 3`)
  the kubelet restarts the pod within ~65 s of a wedged poll loop — well
  inside the 5-minute acceptance window.
- Job GC: already covered. `pkg/k8s/builder.go` sets
  `TTLSecondsAfterFinished: 300` on every Job built; finished Jobs are GC'd
  by kube-controller after 5 min.
- UI runner-availability indicator: not addressed (stretch goal).
- Diagnostics on wedge: when `/healthz` first detects staleness it logs
  `POLL LOOP WEDGED` at ERROR and writes a full goroutine dump
  (`runtime.Stack(buf, true)`) to **stderr**, exactly once per stale state.
  Use `kubectl logs --previous <pod>` after the kubelet restart to recover it.
- Live introspection: `net/http/pprof` is mounted at `/debug/pprof/` on the
  same port (8081). Always on (early alpha; port not exposed outside the
  cluster). Grab a live dump with
  `kubectl port-forward <pod> 8081 && curl localhost:8081/debug/pprof/goroutine?debug=2`
  before the liveness probe restarts the pod.
