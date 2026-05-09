# Per-step `conclusion=failure` shipped when only later steps actually failed

**Status: filed 2026-05-09.** Surfaced via test-cluster verification
of the bug 016 streaming fix on image `1a88d9d`. Split out from
bug 016: the streaming fix (commits `97cfb9f..345fe58`) addressed
the lost-trailing-event symptoms (zero durations, mis-attributed
`started_at`), but the per-step `conclusion=failure` symptom
described in 016 is still present.

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
