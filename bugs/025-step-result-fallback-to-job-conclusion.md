# Per-step `conclusion=failure` shipped when only later steps actually failed

**Status: ROOT CAUSE LOCALIZED TO GITEA, NOT DRAWBAR — 2026-05-11.**
Bug 025 is **gitea bug [#37592](https://github.com/go-gitea/gitea/pull/37592)**,
fixed upstream by commit `601c6eb1` on 2026-05-07, not yet in any
released gitea version (latest release is 1.26.1 from before the fix).
`services/convert/convert.go::ToActionWorkflowJob` called
`ToActionsStatus(job.Status)` for every step instead of
`ToActionsStatus(step.Status)`, so the API response synthesized
each step's conclusion from the job's overall conclusion. The
per-step `step.Status` rows in the `action_task_step` table were
correct; only the API renderer was wrong.

The test-cluster agent saw "all 6 steps = FAILURE" because the
job failed and the API rendered the job's conclusion onto every
step record. Drawbar's outbound payload was fine.

Reproduced locally on gitea 1.26.1: drawbar shipped a step record
with `Result=CANCELLED` (separate bug, see below), the DB stored
`StatusCancelled`, and the API rendered `conclusion=success`
because the job succeeded.

The agent's `gt.monoloco.net` test cluster runs a home-built gitea
from main. If their build predates `601c6eb1` (2026-05-07), they
will see the all-FAILURE shape. If they pull post-`601c6eb1`, the
bug will disappear from their view — though see "Drawbar-side
finding" below for what *will* surface once gitea is fixed.

## Drawbar-side finding (Level 1 dev-env repro, 2026-05-11)

A separate drawbar bug surfaced during the gitea-side debugging.
On a one-step `echo "hello"` workflow that ran cleanly:

```
reporter flushState (terminal): job_result=RESULT_SUCCESS steps="0=CANCELLED"
```

`Close()` saw step 0 still in `Result=UNSPECIFIED` and applied its
defensive UNSPECIFIED→CANCELLED rewrite. That means the watcher's
`FinishStep(0, SUCCESS)` call was never made — the entrypoint's
`state.jsonl` `end` event for step 0 did not reach the reporter.

This is masked on gitea ≤1.26.1 (API renders job conclusion over
step conclusion), but will surface as soon as gitea ships the
`601c6eb1` fix. Filed separately — see bug 026.

## Original investigation (resolved by upstream fix)

Below is the original analysis preserved for history.

---

**Earlier status: NOT FIXED — re-opening 2026-05-09.** First fix attempt on
image `main-1778348893-750a497c` (commit `750a497`, "bugs/025: clamp
interim flushState job_result to UNSPECIFIED") clamped interim flushes
correctly but did **not** restore per-step conclusions on the gitea
side. Verified on `gt.monoloco.net/chaos-inc/bevy_xr_nitro` run 102,
task 108. drawbar's outbound payload sequence was textbook:

- All interim flushes: `final=false job_result=RESULT_UNSPECIFIED`
  with per-step Results progressing 0=SUCCESS → ... →
  `0=SUCCESS,1=SUCCESS,2=SUCCESS,3=SUCCESS,4=SUCCESS,5=FAILURE,6=FAILURE`.
- Close-time flush: `final=true job_result=RESULT_FAILURE` with the
  same per-step `0=SUCCESS,1=SUCCESS,2=SUCCESS,3=SUCCESS,4=SUCCESS,5=FAILURE,6=FAILURE`.

Gitea then displayed all 6 user-visible steps as `failure` anyway.
This rules out hypothesis H1(a) (gitea-rewrites-on-interim) and
confirms H1(b): **gitea performs the per-step conclusion overwrite at
the moment it receives a terminal `state.Result` (non-UNSPECIFIED),
regardless of what per-step Results sit alongside it in the same
payload.** The reconcile lives on gitea's side; clamping interim job
results doesn't help, because the offending overwrite happens during
the *close-time* call which by definition has to carry the real
terminal `state.Result`.

The original "fix" notes below describe what was tried; the actual
fix needs a different shape. Candidate approaches under "Re-opened
fix sketch" near the bottom.

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

## Re-verification on image `750a497c` — fix incomplete

**Run 102 / task 108** on `gt.monoloco.net/chaos-inc/bevy_xr_nitro`,
2026-05-09. Test workflow with the same 6 user-visible steps. The
clamp from `750a497` is doing exactly what it's supposed to:

| time     | final | job_result          | steps |
|----------|-------|---------------------|-------|
| 23:10:16 | false | RESULT_UNSPECIFIED  | 0=UNSPECIFIED,1=UNSPECIFIED,2=UNSPECIFIED,3=UNSPECIFIED,4=UNSPECIFIED,5=UNSPECIFIED,6=UNSPECIFIED |
| 23:10:21 | false | RESULT_UNSPECIFIED  | 0=SUCCESS,1=UNSPECIFIED,2=UNSPECIFIED,3=UNSPECIFIED,4=UNSPECIFIED,5=UNSPECIFIED,6=UNSPECIFIED |
| 23:10:22 | false | RESULT_UNSPECIFIED  | 0=SUCCESS,1=SUCCESS,2=SUCCESS,3=SUCCESS,4=UNSPECIFIED,5=UNSPECIFIED,6=UNSPECIFIED |
| 23:14:13 | false | RESULT_UNSPECIFIED  | 0=SUCCESS,1=SUCCESS,2=SUCCESS,3=SUCCESS,4=SUCCESS,5=UNSPECIFIED,6=UNSPECIFIED |
| 23:22:14 | false | RESULT_UNSPECIFIED  | 0=SUCCESS,1=SUCCESS,2=SUCCESS,3=SUCCESS,4=SUCCESS,5=FAILURE,6=UNSPECIFIED |
| 23:23:35 | false | RESULT_UNSPECIFIED  | 0=SUCCESS,1=SUCCESS,2=SUCCESS,3=SUCCESS,4=SUCCESS,5=FAILURE,6=FAILURE |
| **23:23:36** | **true** | **RESULT_FAILURE**  | **0=SUCCESS,1=SUCCESS,2=SUCCESS,3=SUCCESS,4=SUCCESS,5=FAILURE,6=FAILURE** |

Followed by `task completed task_id=108 result=2` at 23:23:36.182.

Gitea's view of run 102 immediately after terminal:

```
job 107: completed/failure
  step 0: Run actions/checkout@v4                            completed  failure
  step 1: configure netrc for fj git deps                    completed  failure
  step 2: Run drawbar/cache@v1                               completed  failure
  step 3: cargo test --lib                                   completed  failure
  step 4: visual regression suite (bin/visual-test)          completed  failure
  step 5: upload visual-test artifacts on failure            completed  failure
```

Per-step conclusions: still all `failure`. The interim clamp is
working; the bug lives in gitea's processing of the close-time
payload, which carries `state.Result = RESULT_FAILURE` (correctly,
because it has to). Whatever path on gitea synthesizes per-step
conclusions from the job-level Result fires unconditionally on
that terminal call — and there's no in-payload way drawbar can
distinguish "this is the truth, please don't rewrite my steps" from
the same payload act_runner sends.

## Re-opened fix sketch

The clamp-interim approach is not enough on its own. Possible next
shapes, ranked roughly by effort vs. likelihood:

1. **Two-call close.** First close-time call ships
   `state.Result = RESULT_UNSPECIFIED` with the real per-step
   Results — gitea persists steps but doesn't terminate the job.
   Then a second call ships `state.Result = RESULT_FAILURE` with
   per-step Results unchanged (or omitted entirely if the schema
   permits). Gitea terminates the job; if the second call's
   per-step rewrite still happens, the first call has at least
   left an audit trail. This is brittle (depends on gitea's persist
   semantics not coalescing back-to-back updates).

2. **Server-side fix in gitea.** The truly correct fix:
   `actions.UpdateTask` shouldn't synthesize per-step conclusions
   from job-level Result when the payload already supplies them.
   Confirms by reading `gitea/services/actions/update_task.go`
   (or similar) — we don't have the source in-tree but the URL is
   `https://gt.monoloco.net/myers/gt` to clone for a patch. This
   is the right fix; drawbar should still emit defensible payloads
   regardless.

3. **Skip the close-time UpdateTask, rely on a separate "task done"
   signal.** Read the gitea/runner protocol spec to see if there's
   a dedicated end-of-task RPC that doesn't go through the same
   reconcile path. (act_runner doesn't appear to use one — it
   sends the terminal UpdateTask, and act_runner's per-step
   reporting suffers the same bug, just less visibly because most
   pipelines fail on the last step where the rewrite is correct.)

4. **Send terminal twice in opposite order.** First terminal call
   ships per-step Results with `state.Result = UNSPECIFIED`; second
   ships `state.Result = FAILURE` with per-step Results scrubbed to
   UNSPECIFIED. If gitea's persist doesn't overwrite UNSPECIFIED
   step entries, the per-step record from call 1 survives. Same
   brittleness concern as (1).

The right next move is probably (2): clone gitea, find the
UpdateTask handler, identify exactly when/where step conclusions
get synthesized from job conclusion. Once the gitea-side path is
known, the drawbar workaround (if any) becomes obvious.

In the meantime: bug 025 stays open. Bug 016's "read the log,
don't trust the API" workaround remains the only mitigation.

## Source review 2026-05-10 — forgejo (not gitea)

The test cluster runs **forgejo**, not gitea (the `gt` CLI is the
forgejo-cli). Forgejo's `services/actions/task.go::UpdateTaskByState`
is the code that ingests `UpdateTask` payloads. Reviewed against
forgejo `main` at the time (`~/c/reference/forgejo`):

```go
for _, step := range task.Steps {
    var result runnerv1.Result
    if v, ok := stepStates[step.Index]; ok {
        result = v.Result
        step.LogIndex = v.LogIndex
        step.LogLength = v.LogLength
        step.Started = convertTimestamp(v.StartedAt)
        step.Stopped = convertTimestamp(v.StoppedAt)
    }
    if result != runnerv1.Result_RESULT_UNSPECIFIED {
        step.Status = actions_model.Status(result)
    } else if step.Started != 0 {
        step.Status = actions_model.StatusRunning
    }
    if _, err := e.ID(step.ID).Update(step); err != nil {
        return nil, err
    }
}
```

Findings:

1. **The lookup is correct in shape.** `stepStates[step.Index]`
   keyed off the runner's `state.Steps[*].Id`. There is no path
   in forgejo's own code that synthesizes per-step `Status` from
   `task.Status` after the runner ships per-step Results. Modulo
   the off-by-one note below, the persisted per-step `Status`
   should match what drawbar shipped.

2. **Off-by-one between drawbar and forgejo (separate from the
   conclusion-rewrite symptom).** Drawbar's payload is sized by
   the post-expansion StepSpec count (7 in the bevy_xr_nitro
   case — likely a `pre`/`main`/`post` split for one of the node
   actions). Forgejo's task is sized by the workflow YAML's
   `steps:` array (6). Drawbar's `Id 0..5` lands on forgejo's
   `Index 0..5`; drawbar's `Id 6` is silently dropped. So even
   when the rewrite bug is fixed, the *labelling* of forgejo's
   step 5 ("upload-artifact") will reflect drawbar's step 5
   (visual-test) — wrong. This is a drawbar-side bug we should
   fix anyway.

3. **`StopTask` (the bulk per-step rewrite path) is gated on
   `task.Status.IsDone()`.** The relevant callers are
   `CancelRun` (user cancellation) and `stopTasks` (zombie/
   endless cron, default `ZombieTaskTimeout = 10m`,
   `EndlessTaskTimeout = 3h`). Drawbar's daemon flushes every
   1s and `flushState` always invokes `UpdateTask`, which
   refreshes `task.Updated`. So zombie cron should not fire
   on a healthy 13-min run.

4. **`FullSteps` (`modules/actions/task_state.go`) is web-view
   only.** It wraps the persisted steps with virtual pre/post
   "Set up job" and "Complete job" but doesn't mutate the
   middle entries. The API endpoint `repos/.../actions/runs/<id>/jobs`
   serves a flat `[]ActionRunJob` — it doesn't even include
   per-step records.

**Where the source review can't conclude:** the agent's report
of "step 0 ... step 5 ... all failure" doesn't match the
output shape of forgejo's API endpoint, which doesn't return
per-step records. The agent must be reading either:

- the **web view** payload (which does include steps via
  `FullSteps`), in which case the data ultimately reads from
  `task.Steps` — and per finding 1, those should be correct, so
  there's a gap I can't yet explain;
- a **forgejo branch with custom patches** (`gt.monoloco.net/myers/gt`
  is the test cluster's forgejo, possibly a fork);
- or the diagnostic happened on a *prior* run / *cached* page,
  not the run-102 close-time state.

**Action item for the next round:** the agent should capture and
paste:

- The exact URL/CLI used to read the per-step results
  ("step 0 ... failure"). E.g. `gt api repos/.../runs/102/jobs`?
  the web UI? a different `/jobs/<id>` endpoint?
- The raw JSON response from that URL.
- The forgejo version `gt.monoloco.net` is running
  (`gt api version` or the deploy manifest).

With those three signals, the off-by-one + the rewrite path can
be definitively localized to either (a) drawbar bug, (b) a known
forgejo upstream bug, or (c) a fork-specific patch. Until then,
fix sketches (1)/(3)/(4) are guesswork.

The off-by-one (finding 2) **is** worth fixing on the drawbar side
independently — it's a real misalignment between drawbar's
post-expansion step count and forgejo's workflow-YAML step count.
The fix is: cap `state.Steps` to `len(workflow.Steps)` rather than
`len(steps)` (post-expansion), and either drop or merge per-spec
records that don't have a forgejo slot. This may also be the
actual cause of bug 025: if drawbar overshoots and forgejo's
update loop never sees a SUCCESS for one of its slots, that slot
keeps `Status = StatusWaiting` (the initial value at task creation)
— and a *separate* code path turns lingering Waiting steps into
Failure when the parent task goes terminal. Worth grepping
forgejo for `StatusWaiting` reconcile paths in the next pass.

## Source review 2026-05-11 — gitea cross-check

Re-read against `~/c/reference/gitea` (commit `0a3aaea`, 2026-05-09
`main`) to rule out a forgejo-vs-gitea divergence in
`UpdateTaskByState`. They are **structurally identical**:
- Lookup keyed on `stepStates[step.Index]` (workflow-YAML index).
- Per-step `step.Status = Status(result)` only when result is
  non-UNSPECIFIED; falls back to `StatusRunning` when started,
  otherwise unchanged.
- No path inside `UpdateTaskByState` stamps the job conclusion
  onto step records.

Grepped for any path that could rewrite per-step Status with
`task.Status` after `UpdateTaskByState` returns:

- **`models/actions/task.go:463` (`StopTask`)** rewrites all
  not-done steps with the passed-in status. Called from:
  - `models/actions/run.go:329` — user-cancel path (`CancelRun`),
    passes `StatusCancelled`.
  - `services/actions/clear_tasks.go:126` (`stopTasks`) — used by
    `StopZombieTasks` and `StopEndlessTasks`, passes
    `StatusFailure`. Both filter for `Status: StatusRunning` —
    a task already finalized via `UpdateTaskByState` has
    `Status = StatusFailure`, which is `IsDone()` → these crons
    cannot fire on it.
- **`modules/actions/task_state.go::FullSteps`** is web-view-only
  decoration. Mutates the synthetic pre/post records but never
  the middle `task.Steps`.
- **`services/convert/convert.go:392` (`ToActionWorkflowJob`)** is
  the API path. It reads `task.Steps[*].Status` directly through
  `ToActionsStatus`, no synthesis from `job.Status`.

So gitea/forgejo source predicts the run-102 final flush
(`[SUCCESS,SUCCESS,SUCCESS,SUCCESS,SUCCESS,FAILURE,FAILURE]`
shipped, with the off-by-one truncating to slot 5) should persist
as `[SUCCESS,SUCCESS,SUCCESS,SUCCESS,SUCCESS,FAILURE]` in the
six task.Steps slots — and `ToActionWorkflowJob` should return
those verbatim. **The all-FAILURE shape the agent observed is not
predicted by reading the source.**

That gap means one of three things:

1. **The agent's read source is different from what we think.**
   E.g. the agent's `gt api` is hitting a forgejo endpoint not in
   `~/c/reference/forgejo`, the test cluster forgejo is a custom
   fork, or the agent quoted the *web view* (which renders the
   synthetic `FullSteps` wrapping plus the persisted middle —
   but the persisted middle is still the per-step Result, not the
   job conclusion).
2. **drawbar's payload is not what the diagnostic table says.**
   The slog line ships post-clone, but reads of `r.state.Result`
   happen under the mutex — there's no obvious way to ship one
   thing and log another, but it's worth checking the slog field
   formatter (`formatStepResults`) for a string/value mismatch.
3. **A race in gitea/forgejo between `UpdateTaskByState`'s step
   write loop and a concurrent path I haven't found.** Less
   likely — the loop runs inside `WithTx2`, holding the row lock.

The cheapest disambiguation is still data from the agent.
Ranked by signal value:

- **Re-read `task.Steps` directly out of the gitea/forgejo DB
  immediately after run terminal.** A `gt admin db query` (or
  forgejo equivalent) hitting `SELECT id, index, name, status FROM
  action_task_step WHERE task_id=108`. This bypasses every layer
  above the row store and tells us what was actually persisted.
- **Raw HTTP response** from whatever `gt api` invocation the agent
  used to get the "all 6 failure" table — URL, headers, and body
  JSON.
- **Forgejo version** running on `gt.monoloco.net`. If it's a
  fork, knowing the patch set rules in or out path (1).

Until we have at least the DB read, the agent's "all 6 failure"
observation is unfalsifiable against the source.

### Updated next action

Hold further drawbar-side fixes. Write back to the test cluster
agent with the three asks above. If the DB shows
`[SUCCESS,SUCCESS,SUCCESS,SUCCESS,SUCCESS,FAILURE]`, the bug is
display-only on the gitea/forgejo side and drawbar is fine. If
the DB shows `[FAILURE * 6]`, there's a write path we haven't
found and the source review needs to redo with a different
grep angle.

The off-by-one (drawbar slot 6 = upload-artifact = FAILURE) is
still a real drawbar bug to fix independently — gitea/forgejo
silently drops it. But it cannot turn earlier SUCCESS slots into
FAILURE.

## Instrumentation 2026-05-11 — drawbar self-diagnostics

Rather than wait on manual data from the agent, the reporter now
self-instruments on every terminal flush and a post-terminal
read-back probe.

In `pkg/reporter/reporter.go::flushState`:

1. **Wire-level digest.** Every flush (debug-level on interim,
   info-level on terminal) logs `req_bytes` and `req_sha256`
   (first 8 bytes hex of `proto.Marshal(req)`). If a future
   diagnostic shows we shipped one thing per the slog and gitea
   persisted another, the forge-side log can prove byte-equal
   receive vs. drift in transit. The terminal log is unconditional
   so it lands in every operator's controller log without needing
   `LOG_LEVEL=debug`.
2. **Response logging.** Each flush logs `resp_task_id` and
   `resp_job_result` (the only fields gitea/forgejo echo back via
   `UpdateTaskResponse.State`). Catches forge-side cancellations
   and confirms forge's view of the task status.
3. **Readback probe (Close-only).** After the terminal
   `flushState` succeeds, `Close` issues a final no-op
   `UpdateTask{State: {Id: taskID, Result: UNSPECIFIED}}`. Gitea's
   `UpdateTaskByState` early-returns when `task.Status.IsDone()`
   (gitea models/actions/task.go:370), so this is side-effect-free
   on the forge side. The response echoes what the forge currently
   thinks the task status is. If the forge says `RESULT_SUCCESS`
   when we sent `RESULT_FAILURE`, that's a smoking-gun for a
   wrong-task-id race or forge-side rewrite.

What the operator will see in the controller log after the next
bevy_xr_nitro run:

```
reporter flushState (terminal)         task_id=N final=true job_result=RESULT_FAILURE steps="0=SUCCESS,...,5=FAILURE,6=FAILURE" req_bytes=... req_sha256=...
reporter flushState response (terminal) task_id=N resp_task_id=N resp_job_result=RESULT_FAILURE
reporter readback probe                task_id=N resp_task_id=N resp_job_result=RESULT_FAILURE
```

Three signals to compare against the agent's gitea API view:

| Signal | Says | Conclusion |
|---|---|---|
| `flushState (terminal)` `steps` | what drawbar sent | wire-side truth |
| `response (terminal)` `resp_job_result` | what forge persisted (job-level) | forge ack |
| `readback probe` `resp_job_result` | what forge now thinks (post-commit) | forge state after settle |

Combined with the agent's screenshot of the API per-step view, this
should disambiguate (a) drawbar sent wrong, (b) forge persisted
right but UI renders wrong, (c) forge has a delayed-rewrite path
that flips state between the response and the read-back.

Tests:
- `TestReporter_CloseSendsReadbackProbe` pins the probe contract
  (UNSPECIFIED + zero steps).
- Existing terminal/clamp tests still pass; the fake forgejo
  servers in `cmd/controller/{handler,run}_test.go` now ignore
  UNSPECIFIED results when latching `lastResult` so the probe
  doesn't clobber the terminal observation.
