# Step `end` event from entrypoint not reaching the reporter

**Status: fixed 2026-05-11** by introducing a `state-agent` native
sidecar that owns `entrypoint tail` and stream the events via the
kubelet log endpoint (which buffers post-exit). See Resolution
section at the bottom.

**Earlier status: open, surfaced 2026-05-11.** Split out from bug 025
investigation. Drawbar reports a successfully-completed step as
`Result=CANCELLED` to the forge because `Close()`'s defensive
UNSPECIFIED→CANCELLED rewrite fires — i.e. the watcher never
called `FinishStep(stepIdx, SUCCESS)` for the step before Close.

## Symptom

A minimal `echo "hello"` workflow ran to completion on a local
gitea 1.26.1 + drawbar `e239d63` dev-env. The job pod's container
exited cleanly (`exit 0`) and the controller log shows
`task completed result=1` (RESULT_SUCCESS). But the reporter's
state, observed via the bug 025 diagnostic, is:

```
reporter flushState (terminal): task_id=2 final=true job_result=RESULT_SUCCESS steps="0=CANCELLED"
```

This is masked on gitea ≤1.26.1 due to upstream bug #37592
(per-step API conclusion synthesized from job conclusion). With
the gitea fix applied, the step would render as `cancelled`
instead of `success` — wrong for a step that actually succeeded.

## Repro

1. `./hack/dev-env.sh up` (gitea 1.26.1, image `e239d63`).
2. Push a minimal workflow:

   ```yaml
   name: level1
   on: [push]
   jobs:
     test:
       runs-on: ubuntu-latest
       steps:
         - name: only-step
           run: echo "hello"
   ```

3. Observe the runner log around the `task completed` line. The
   `reporter flushState (terminal)` entry will show
   `steps="0=CANCELLED"` despite the job succeeding.

4. Query the gitea DB directly to confirm persistence (the API
   masks it on 1.26.1):

   ```
   sqlite3 /data/gitea/gitea.db \
     "SELECT id, task_id, \"index\", name, status FROM action_task_step;"
   ```

   `status=3` is `StatusCancelled`.

## Localized to

The watcher's `routeStateEvent` path
(`pkg/k8s/watcher.go:397`). For a `type=end` event with
`exit_code=0`, the code calls `rep.FinishStep(step,
RESULT_SUCCESS)`. So either:

- The entrypoint never wrote the `end` event to
  `/shim/state.jsonl` for step 0, or
- The watcher's streaming tail / post-exit drain didn't pick it
  up before the container's stdout closed, or
- The event was picked up but `Close()` had already snapshotted
  the state.

The streaming refactor (bug 016 resolution, spec
`docs/superpowers/specs/2026-05-06-step-state-streaming-design.md`)
was supposed to make this race impossible by long-lived
`entrypoint tail` + a one-shot post-exit drain. Either the drain
isn't running, isn't reading the trailing events, or there's a
window where `Close()` fires between the streamer's tail closing
and the drain starting.

## Why this is the right thing to fix next

Even though bug 025 is upstream-gitea and not us, this bug is
real and our problem:

- Once gitea ships the #37592 fix, `cancelled` is what users will
  see for every successful step on a one-step job — a regression
  even relative to today's masked behavior.
- It also affects multi-step jobs: every step that runs to clean
  exit but whose `end` event misses the streamer becomes
  `cancelled`. Could explain misleading durations in mixed runs.
- Bug 023 (post-exit drain race) was a previous attempt at this
  class of issue. Worth re-reading.

## Diagnostic next steps

1. Enable debug logging (`log.level: debug`) and check the
   watcher logs for `state event` and `routing state event`
   entries. Are end-events for step 0 logged but not routed? Or
   not logged at all?
2. `kubectl exec` into the job pod after task completion and
   inspect `/shim/state.jsonl` — does it contain the `end` event
   for step 0?
3. If the file has the event, the bug is in the drain path. If
   not, the bug is in the entrypoint.

## Findings 2026-05-11 (dev-env, image `e239d63`, gitea 1.26.1)

Triggered three runs (tasks 1, 2, 3) of the level-1 workflow.
All three showed identical symptom: `0=CANCELLED` on the
terminal flush. With `log.level: debug` on task 3:

- `step started` logged at `15:12:54.722` (the `start` event
  was routed via the live streamer).
- `step completed` was **never logged**.
- Three interim flushes all showed `0=UNSPECIFIED`. The third
  flush is `req_bytes=38` (vs prior 6) — `StartedAt` got set
  by the `start` event being routed, but `Result` stayed
  UNSPECIFIED.
- Terminal flush at `15:12:56.622` shipped `0=CANCELLED` (the
  defensive rewrite in `Close()` UNSPECIFIED→CANCELLED).

So the `start` event reaches the reporter live, but the `end`
event does not — neither via the live streamer nor via the
post-exit drain.

## Likely root cause

The streamer (`streamStateFileWith`) runs
`/shim/entrypoint tail /shim/state.jsonl` as an `exec` inside
the runner container. When the runner shell exits (step 0
finishes, container has no more commands to run), the container
terminates — SIGKILL goes to **every** process inside it,
including the `entrypoint tail` we're exec'd against. Our exec
stream EOFs at that moment, regardless of whether the tail
process has finished reading the trailing lines of
`state.jsonl`.

Then `drainStateFile` runs a fresh `exec` against the same
container, but the container is now in Terminated state — the
kubelet refuses the exec, `executor.ExecStream` returns
"container not running" (or similar), `drainStateFile` logs a
warn and returns. The trailing `end` event sits unread in the
container's `/shim/state.jsonl` filesystem forever (and the
filesystem itself is GC'd a moment later when the pod is
reaped).

The streaming refactor's premise — "post-exit drain catches
trailing events the live stream missed" — is broken because the
container is gone by the time the drain tries to exec. The
fix needs to either:

1. **Make the entrypoint tail process outlive the runner
   shell.** Put it in a separate container in the same pod so
   it stays alive until explicitly killed. Then the drain can
   exec against a still-live container and read the trailing
   bytes. This is the cleanest model but adds a container slot.

2. **Read state.jsonl out-of-band.** Instead of execing into
   the pod, have the entrypoint write `state.jsonl` to a path
   that's also visible to the controller (PVC, sidecar that
   tails over the network). Adds infrastructure.

3. **Flush trailing events before the runner shell exits.**
   The entrypoint already writes `f.Sync()` after each event,
   so the bytes are on disk. The issue is reading them, not
   writing them. A pre-exit sleep in the entrypoint would race
   the kubelet — not a fix.

4. **Capture the lifecycle log out of the container's stdout.**
   Drawbar already streams the runner container's stdout in
   real time. The entrypoint could write a sentinel
   `::drawbar-state::{...}` line on stdout in addition to
   `state.jsonl`. The log streamer reads stdout via the
   apiserver log endpoint, which doesn't require the container
   to be alive (logs are buffered by the kubelet). This is the
   smallest delta and works for both pre-exit and post-exit
   events.

(4) looks cheapest. It also doesn't require adding a sidecar or
PVC. Risk: stdout interleaving — workflow command parsing
already needs careful handling, and adding a second sentinel
syntax is one more thing to mask in logs to users.

## Confirmation on patched gitea (2026-05-11)

Rebuilt gitea locally at commit `601c6eb` (which contains
upstream PR #37592 — the bug 025 renderer fix), swapped the
dev-env's gitea pod to that image, and re-queried run 3 (the
same run as the dev-env Level 1 retry above, no new task ran —
only the renderer changed):

```
GET /api/v1/repos/devadmin/bug025/actions/runs/3/jobs
{
  "name": "test",
  "status": "completed",
  "conclusion": "success",            ← job conclusion: correct
  "steps": [{
    "name": "only-step",
    "conclusion": "cancelled",        ← now shows the real step.Status
    "completed_at": "1970-01-01T00:00:00Z"  ← step.Stopped was never set
  }]
}
```

Two facts confirmed in one read:

1. **Upstream gitea fix works.** The renderer now reads
   `step.Status` instead of `job.Status`. Bug 025 closes
   automatically once gitea ships this commit in a release.
2. **Bug 026 is real and serves the wrong data to gitea.** Same
   run data, only renderer changed; the underlying `cancelled`
   is what drawbar wrote. The 1970 `completed_at` is consistent
   with drawbar never sending a stop time for step 0 (which
   ratifies the "end event was never routed" finding).

The patched-gitea image
(`k3d-k3d-registry:5111/gitea-patched:601c6eb`) stays in the
local registry so further drawbar fixes can be validated
end-to-end against the future-gitea renderer rather than the
masking 1.26.1 renderer.

## Related

- Bug 016: streaming refactor that this was supposed to fix.
- Bug 023: post-exit drain race (different framing, may be the
  same root cause).
- Bug 025: where this was found. Upstream gitea bug, separate.

## Resolution (2026-05-11)

Adopted fix option (a) — sidecar — sketched in `TODO_REFACTOR.md`,
plus the realization that we don't need to exec into the sidecar
at all: the kubelet log endpoint already buffers content
post-exit. The implementation:

1. **`pkg/k8s/builder.go::BuildJob`** adds a native sidecar
   (init container with `RestartPolicy: Always`, k8s 1.29+)
   named `state-agent`. It runs the existing
   `/shim/entrypoint tail /shim/state.jsonl` against the shared
   `/shim` emptyDir. `setup-shim` now `touch /shim/state.jsonl`
   so the agent can `os.Open` it without racing the runner's
   first write.
2. **`cmd/entrypoint/main.go`** wires `signal.NotifyContext` for
   SIGINT/SIGTERM in the `tail` subcommand so the kubelet's
   graceful shutdown signal cancels `runTail` cleanly.
3. **`pkg/k8s/watcher.go::streamStateFileWith`** rewritten: no
   more long-lived exec + reconnect loop. It now opens a single
   log-stream against the `state-agent` container (`Follow: true`)
   and routes each line through EOF. The kubelet buffers events
   the agent emitted right before exit, so the live stream
   naturally reads trailing events through EOF.
4. **`drainStateFile` removed** — the live log stream reads
   buffered post-exit content directly, so a separate one-shot
   drain step is no longer needed.
5. **`backoffStep` removed** — only `streamStateFileWith` used
   it, and the log-stream version doesn't need retries.

End-to-end verified against the patched gitea
(`k3d-k3d-registry:5111/gitea-patched:601c6eb`, which contains
the bug 025 renderer fix):

| signal | pre-fix (`909daf1`) | post-fix |
|---|---|---|
| controller log: `step started` | absent | present |
| controller log: `step completed` | absent | present |
| reporter flushState (terminal) `steps` | `0=CANCELLED` | `0=SUCCESS` |
| gitea API step `conclusion` | `cancelled` | `success` |
| gitea API step `started_at` | 1970-01-01 epoch | wall-clock time |
| gitea API step `completed_at` | 1970-01-01 epoch | wall-clock time |

Note this fix also obviates the `Close()` defensive
UNSPECIFIED→CANCELLED rewrite in `pkg/reporter/reporter.go` for
healthy runs — by the time `Close` fires, every step's `Result`
has been routed correctly. The rewrite still matters for genuinely
incomplete steps (job-level failure with later steps unreached)
where the runner exits before emitting those steps' `end` events.

This is the seed of the broader state-plane architecture sketched
in `TODO_REFACTOR.md`. Today the agent does nothing but tail a
file; over time it can absorb log streaming, workflow command
parsing, and secret masking — each migration is decoupled.
