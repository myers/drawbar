# Step `end` event from entrypoint not reaching the reporter

**Status: open, surfaced 2026-05-11.** Split out from bug 025
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

## Related

- Bug 016: streaming refactor that this was supposed to fix.
- Bug 023: post-exit drain race (different framing, may be the
  same root cause).
- Bug 025: where this was found. Upstream gitea bug, separate.
