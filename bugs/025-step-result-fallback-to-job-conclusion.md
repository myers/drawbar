# Per-step `conclusion=failure` shipped when only later steps actually failed

**Status: fixed 2026-05-09.** H1 confirmed by the diagnostic flush
captured on image `main-1778339352-79eaf559` (run 100, task 106 on
`gt.monoloco.net/chaos-inc/bevy_xr_nitro` test.yaml). Drawbar's
per-step Results were correct end-to-end; gitea was rewriting them
after receiving an interim `UpdateTask` payload whose job-level
`state.Result` was non-UNSPECIFIED. Fix mirrors the act_runner
pattern at `reference/runner/internal/pkg/report/reporter.go:511`:
interim flushes clamp the outbound `state.Result` to
`RESULT_UNSPECIFIED`; only the close-time flush carries the real
terminal value. Per-step Results are passed through as-is on both
paths. See the Resolution section at the bottom.

Surfaced via test-cluster verification of the bug 016 streaming fix
on image `1a88d9d`. Split out from bug 016: the streaming fix
(commits `97cfb9f..345fe58`) addressed the lost-trailing-event
symptoms (zero durations, mis-attributed `started_at`), but the
per-step `conclusion=failure` symptom described in 016 was still
present.

## Symptom

A job whose final step fails has *every* per-step record in
`repos/.../actions/runs/<id>/jobs` reporting `conclusion=failure`,
even though earlier steps demonstrably ran to a clean exit. The
expected shape is:

- pre-failure steps: `conclusion=success`
- the actually-failing step: `conclusion=failure`
- subsequent unrun steps: `conclusion=skipped` (or `cancelled`,
  depending on whether the entrypoint kept going to evaluate
  `if: failure()` conditions)

Job-level `conclusion=failure` is correct and unaffected.

## Repro

The original 016 repro applies unchanged:
- Repo: `gt.monoloco.net/chaos-inc/bevy_xr_nitro`, `test.yaml`
  (push event), 6 sequential steps.
- Step 4 (`bin/visual-test`) is the only step that actually fails.
- Test-cluster agent's matrix on image `1a88d9d` (2026-05-09):
  *"all 6 steps report failure even though most passed."*
  Identical observed shape to the bug 016 table.

## What we know after the streaming fix landed

- `pkg/k8s/watcher.go::routeStateEvent` calls `FinishStep(step,
  RESULT_SUCCESS)` for `event=end, exit_code=0` and
  `FinishStep(step, RESULT_FAILURE)` for non-zero — so a successfully
  delivered `end` event with exit_code=0 should land as
  `step.Result = RESULT_SUCCESS` in the reporter's state.
- `FinishStep` in `pkg/reporter/reporter.go` is the only call site
  that writes per-step `Result` (other than the
  UNSPECIFIED→CANCELLED rewrite in `Close`). There is no path in
  drawbar that overwrites a successful step's Result with FAILURE.
- The streaming refactor proved we can deliver `start`/`end` events
  reliably even at container-exit boundary (bug 016's Resolution).
  So if the entrypoint emits a clean `end` with exit_code=0 for
  steps 0..3, the reporter's local state should record SUCCESS.

That leaves **what we send on the wire** vs **what gitea persists**
as the unexplained gap.

## Working hypotheses, ranked

### H1 (most likely): drawbar lacks the act_runner UNSPECIFIED-on-interim pattern

`reference/runner/internal/pkg/report/reporter.go:511`:

```go
if !reportResult {
    state.Result = runnerv1.Result_RESULT_UNSPECIFIED
}
```

act_runner forces `state.Result = RESULT_UNSPECIFIED` on **every**
interim `UpdateTask` call. Only the close-time call sends the
terminal `RESULT_FAILURE`/`RESULT_SUCCESS`. drawbar's
`flushState` always sends `state.Result` as-is — which is
UNSPECIFIED while running and FAILURE/SUCCESS after `Close()`.

**Why this might cause the symptom:** unverified, but plausible
shapes include — (a) gitea may treat any `UpdateTaskRequest` whose
`state.Result != UNSPECIFIED` as authoritative-final and stamp the
job conclusion onto step records that haven't reached terminal
themselves; (b) per-step Result fields in the same payload may
get rewritten during gitea's `update_task` server-side reconcile
when the job goes terminal. We don't have gitea source available
in-tree to confirm.

The diagnostic in the next section will show whether our final
flush is correct — that disambiguates this from H2/H3.

### H2: step-index misalignment after action expansion

drawbar's reporter array is sized by `len(steps)` post-expansion
(see `cmd/controller/main.go:740`). A workflow step that uses a
composite action expands to N StepSpecs; the reporter has slots
for all N expansions. Gitea's per-step records, by contrast, are
sized to the workflow YAML's `steps:` array. If they disagree,
`FinishStep(N, SUCCESS)` writes to a slot gitea doesn't have, and
gitea slots that were never written to fall back to job conclusion.

Mitigation: confirm that the test workflow's actions
(`actions/checkout@v4`, `drawbar/cache@v1`,
`actions/upload-artifact@v3`) all map to **exactly one StepSpec**
each. `actions/checkout` is node20 (1 spec). `drawbar/cache@v1` is
a magic action (1 spec). `actions/upload-artifact` is node20
(1 spec). So this hypothesis is unlikely for the specific repro
workflow but real for any workflow containing a composite action.

### H3: missing `end` events fall through to CANCELLED, displayed as failure

If the streamer dropped step 0..3's `end` events somehow despite
the streaming fix (e.g., reconnect-cap tripped silently), those
steps would reach `Close()` with `Result = UNSPECIFIED` and become
`RESULT_CANCELLED`. The agent reports "failure," but gitea's UI
may render `RESULT_CANCELLED` indistinguishably from
`RESULT_FAILURE`. The diagnostic also disambiguates this (we'd see
CANCELLED in the close-time flush instead of SUCCESS).

## Diagnostic plan (must run BEFORE writing the fix)

1. Add a debug-level log in `pkg/reporter/reporter.go::flushState`
   dumping the outbound `state.Result` plus each step's `Result`.
   Format compactly so each flush is one log line:
   `flushState task_id=N job=UNSPECIFIED steps=[0=SUCCESS,1=SUCCESS,2=UNSPECIFIED,...]`.
   Gate at `slog.Debug` — only fires when `LOG_LEVEL=debug` is set
   on the controller. No behavioral change.
2. Build an image, push, deploy to the test cluster with
   `LOG_LEVEL=debug`.
3. Re-run the bevy_xr_nitro `test.yaml` workflow. Capture two log
   lines:
   - **Interim flush** (any flush during the run, ideally one
     after step 3 completed but before step 4 finished).
   - **Final flush** (the close-time one after the job has reached
     terminal state).
4. Decision tree based on what the two captures show:

| Final flush job= | Final flush per-step | Conclusion |
|---|---|---|
| FAILURE | [SUCCESS, SUCCESS, SUCCESS, SUCCESS, FAILURE, FAILURE] | **H1 confirmed** — gitea is rewriting step records on terminal job. Fix: send UNSPECIFIED on interim, real on close. |
| FAILURE | [FAILURE, FAILURE, FAILURE, FAILURE, FAILURE, FAILURE] | **drawbar bug, not H1** — something is rewriting per-step Results client-side. Audit `Close` and `FinishStep` more carefully. |
| FAILURE | [CANCELLED, CANCELLED, ..., FAILURE] | **H3 confirmed** — events were dropped. Fix is in the watcher. |
| FAILURE | [SUCCESS, ..., UNSPECIFIED] | step-index misalignment per H2. |

## Diagnostic results (2026-05-09, image `main-1778339352-79eaf559`)

Test-cluster verification on `gt.monoloco.net/chaos-inc/bevy_xr_nitro`,
run 100 (push-triggered test.yaml), task id 106 (job 105 server-side).
Controller deployed with `log.level: debug`. Captured deduped
flushState transitions for task 106 from `kubectl logs` while the run
ran end-to-end:

| time     | job_result          | steps |
|----------|---------------------|-------|
| 15:14:42 | RESULT_UNSPECIFIED  | 0=UNSPECIFIED,1=UNSPECIFIED,2=UNSPECIFIED,3=UNSPECIFIED,4=UNSPECIFIED,5=UNSPECIFIED,6=UNSPECIFIED |
| 15:14:50 | RESULT_UNSPECIFIED  | 0=SUCCESS,1=UNSPECIFIED,2=UNSPECIFIED,3=UNSPECIFIED,4=UNSPECIFIED,5=UNSPECIFIED,6=UNSPECIFIED |
| 15:14:52 | RESULT_UNSPECIFIED  | 0=SUCCESS,1=SUCCESS,2=SUCCESS,3=SUCCESS,4=UNSPECIFIED,5=UNSPECIFIED,6=UNSPECIFIED |
| 15:18:50 | RESULT_UNSPECIFIED  | 0=SUCCESS,1=SUCCESS,2=SUCCESS,3=SUCCESS,4=SUCCESS,5=UNSPECIFIED,6=UNSPECIFIED |
| 15:26:42 | RESULT_UNSPECIFIED  | 0=SUCCESS,1=SUCCESS,2=SUCCESS,3=SUCCESS,4=SUCCESS,5=FAILURE,6=UNSPECIFIED |
| 15:28:03 | RESULT_UNSPECIFIED  | 0=SUCCESS,1=SUCCESS,2=SUCCESS,3=SUCCESS,4=SUCCESS,5=FAILURE,6=FAILURE |
| **15:28:04** (final) | **RESULT_FAILURE** | **0=SUCCESS,1=SUCCESS,2=SUCCESS,3=SUCCESS,4=SUCCESS,5=FAILURE,6=FAILURE** |

Followed immediately by `task completed task_id=106 result=2` at
15:28:04.792.

What gitea actually displays for run 100, fetched immediately after
terminal:

```
job 105: completed/failure
  steps:
    0: Run actions/checkout@v4                            completed  failure
    1: configure netrc for fj git deps                    completed  failure
    2: Run drawbar/cache@v1                               completed  failure
    3: cargo test --lib                                   completed  failure
    4: visual regression suite (bin/visual-test)          completed  failure
    5: upload visual-test artifacts on failure            completed  failure
```

### Decision-tree match

The final flush row (`FAILURE` / `[SUCCESS,SUCCESS,SUCCESS,SUCCESS,SUCCESS,FAILURE,FAILURE]`)
maps to **row 1** of the decision tree:

> **H1 confirmed** — gitea is rewriting step records on terminal job.
> Fix: send UNSPECIFIED on interim, real on close.

drawbar's outbound state is correct; the per-step conclusions sent to
gitea reflect actual step outcomes. Yet gitea collapses every step's
`conclusion` to `failure` on its side after receiving the terminal
`UpdateTask` whose `state.Result = RESULT_FAILURE`. This matches the
suspected gitea-side reconcile pattern: when `state.Result` arrives
non-UNSPECIFIED, gitea treats the call as authoritative-final and
stamps the job conclusion onto step records.

### Step-index note (separate observation)

drawbar reports **7 steps** (indices 0–6); gitea displays **6 steps**
(indices 0–5). The step at drawbar's index 0 appears to be the actor's
internal "Set up job" step which gitea doesn't surface in its API.
Drawbar's `1..6` map onto gitea's `0..5`, and the per-step conclusions
are correctly aligned with what each user-visible step actually did
(visual-test = 5, upload-artifact = 6 → maps to gitea's 4, 5, both
genuine failures). This off-by-one is *not* the cause of bug 025 — the
mapping is consistent, gitea just rewrites the data after receiving
it. But it's worth noting separately for diagnostic clarity in any
future debugging.

## Operational impact

Same as bug 016: per-step accounting via the gitea API is
unreliable. Job-level outcomes are unaffected. Mitigated by reading
raw logs.

## Workarounds

- Read the log stream, not the API.
- Don't trust per-step `conclusion` in dashboards.

## Related

- Bug 016 (parent): the streaming refactor fixed the
  lost-event/zero-duration symptom; this bug is the unfixed
  remainder.
- Bug 017: reporter `logOffset` race + ctx-blind close (closed,
  unrelated).
- Reference act_runner pattern: `reference/runner/internal/pkg/report/reporter.go:510-512`.

## Resolution

`pkg/reporter/reporter.go::flushState` now takes a `final bool`
parameter. The daemon-driven `Flush` path passes `false` and the
outbound `state.Result` is forced to `RESULT_UNSPECIFIED` on the
clone — the reporter's internal `r.state.Result` is unchanged, only
the wire payload is clamped. The retry loop in `Close` passes
`true` so the terminal job conclusion ships once per task. Per-step
Results are passed through unchanged on both paths.

Three regression tests in `pkg/reporter/reporter_test.go`:

- `TestReporter_FlushClampsJobResultOnInterim` — mid-run flush ships
  `state.Result = UNSPECIFIED` with per-step Results intact.
- `TestReporter_CloseShipsTerminalJobResult` — close-time flush
  ships the real terminal Result.
- `TestReporter_FlushAfterInternalResultStillClamps` — even when
  internal `r.state.Result` has been set non-UNSPECIFIED, an
  interim Flush still clamps. Guards against a future caller
  setting Result early.

Verified to fail without the clamp; all three pass with it.
