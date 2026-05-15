# Step reporter mis-attributes start/end times and conclusion across sequential steps

**Status: fully resolved 2026-05-15.** The two symptoms in this
doc split into two independent root causes, both now closed:

1. **Lost trailing events → zero durations, mis-attributed
   `started_at`.** First addressed by the 2026-05-06 streaming
   refactor (`97cfb9f..345fe58`), which reduced but did not
   eliminate the lifecycle dependency — see **bug 026**. The
   streaming-tail-via-exec model still raced the runner
   container's SIGKILL. **Bug 026's fix** (state-agent native
   sidecar streaming via the kubelet log endpoint, commit on
   2026-05-15) is the actual cure: the kubelet buffers the
   trailing events post-exit, so durations and timestamps are
   now correct end-to-end. Verified against patched gitea.

2. **Every step reporting `conclusion=failure` when only later
   steps failed.** Split out to **bug 025**. Root cause was
   *not* drawbar — it was upstream gitea bug
   [#37592](https://github.com/go-gitea/gitea/pull/37592):
   `ToActionWorkflowJob` rendered every step's conclusion from
   `job.Status` instead of `step.Status`. Fixed upstream
   2026-05-07 (commit `601c6eb`), not yet in a gitea release.
   Drawbar's per-step Results were correct on the wire the
   whole time.

So the original "drawbar's reporting layer is loose" framing
was half right: the timing/duration half was a real drawbar
lifecycle bug (026), the conclusion half was a gitea renderer
bug (025). See those docs for specifics.

Historical detail below preserved for context. The Resolution
section at the bottom describes the 2026-05-06 partial fix;
`docs/superpowers/specs/2026-05-06-step-state-streaming-design.md`
has that design. The definitive fix is in bug 026.

Surfaced 2026-05-06 while shaking down image
`main-1778069066-4cb729d9` (the bug 014/015 fix). Drawbar is reporting
per-step `started_at` / `completed_at` / `conclusion` to gitea
incorrectly when a job has multiple sequential steps where a *later*
step fails. Earlier steps that actually passed are reported as
`conclusion=failure`, their durations collapse to 0s, and the failing
step's window absorbs the runtime of preceding steps. The data shown
in the gitea Actions UI and `gt api .../jobs/<id>/jobs` is therefore
unreliable for debugging — you can't tell from the API which step
actually failed or how long any individual step really took.

## Repro

Image: `ghcr.io/myers/drawbar:main-1778069066-4cb729d9`. Capacity 1.
Repo: `gt.monoloco.net/chaos-inc/bevy_xr_nitro`, run 86, job 91
(`test.yaml`, push event). Workflow has 6 sequential steps; step 4
(visual-test pytest) is the only one that actually fails.

What gitea reports via `repos/.../actions/runs/86/jobs`:

| # | name                                  | status    | conclusion | started_at           | completed_at         |
|---|---------------------------------------|-----------|------------|----------------------|----------------------|
| 0 | actions/checkout@v4                   | completed | failure    | 2026-05-06T14:35:54Z | 2026-05-06T14:35:58Z |
| 1 | configure netrc for fj git deps       | completed | failure    | 14:35:58Z            | 14:36:00Z            |
| 2 | drawbar/cache@v1                      | completed | failure    | 14:36:00Z            | 14:36:00Z            |
| 3 | cargo test --lib                      | completed | failure    | 14:36:00Z            | 14:36:00Z            |
| 4 | visual regression suite (bin/visual-test) | completed | failure | 14:36:00Z            | 14:39:45Z            |
| 5 | upload visual-test artifacts on failure  | completed | failure | 14:39:45Z            | 14:42:34Z            |

What the *log stream* shows actually happened:

| # | name                                  | actual start  | actual end    | actual duration | actual outcome |
|---|---------------------------------------|---------------|---------------|-----------------|----------------|
| 0 | actions/checkout                      | 14:35:54Z     | 14:35:58Z     | 4s              | **success**    |
| 1 | configure netrc                       | 14:35:58Z     | 14:36:00Z     | 2s              | **success**    |
| 2 | drawbar/cache@v1                      | 14:36:00Z     | 14:36:00Z     | <1s (cache hit) | **success**    |
| 3 | cargo test --lib                      | 14:35:59Z*    | 14:39:45Z     | ~3m46s          | **success** (160+ tests pass) |
| 4 | bin/visual-test                       | 14:39:45Z     | 14:42:34Z     | 2m49s           | failure (real) |
| 5 | upload-artifact                       | 14:42:34Z     | 14:43:59Z     | 1m25s           | failure (real) |

(*step 3's first log line is at 14:35:59, one second before the API
says step 2 ended at 14:36:00 — an off-by-one within the same second,
not material.)

Concrete bugs in that table:

- **Steps 0–3 report `conclusion=failure`** despite all four executing
  successfully. The `failure` is propagating from job-level
  `conclusion=failure` down onto every step.
- **Step 3 reports duration 0s** despite running for ~3m46s. The
  cargo test compilation + 160+ test invocations across 17 packages
  all happened inside this step (visible in the log between lines
  148 and 1310).
- **Step 4 reports `started_at=14:36:00Z`** — the moment step 3
  *began*, not ended. So step 4's reported window
  (`14:36:00Z → 14:39:45Z`, 3m45s) actually contains *all of step 3*
  plus none of step 4's real runtime. Step 4 actually began at
  14:39:45Z (when `+ bin/visual-test` is logged) and ran until
  ~14:42:34Z, ~2m49s.
- **Net effect**: from the API alone, you cannot determine which
  step failed (every step says it did), how long the failing step
  actually took (step 4's duration is wrong), or whether any earlier
  step succeeded (everything says failure).

## Root cause hypothesis (unverified)

I have not read the reporter code yet. The shape of the data
suggests one of:

1. The reporter only emits a final `UpdateTask` payload at job end
   that overwrites all step records with terminal `conclusion`,
   instead of streaming per-step `UpdateTask` calls as each step
   transitions. If gitea's actor protocol expects per-step transitions
   and they're being collapsed into one final flush, gitea will
   reasonably stamp the job's terminal conclusion onto every step.
2. Step boundary timestamps come from a different source than step
   conclusions (e.g., the actor entrypoint reports start times eagerly
   on dispatch but never reports per-step end times until job
   completion), so `completed_at` and `started_at` end up with the
   wrong relationship.
3. There's an off-by-one in how step indices are matched between
   drawbar's reporter view and gitea's expected schema (note: the
   log stream prints `Step 5 (visual regression suite ...)` and `Step 6
   (upload-artifact ...)` — 1-indexed including a hidden setup step —
   while the API reports those same steps as 4 and 5 — 0-indexed
   from the workflow's `steps:` array). If the reporter uses one
   indexing and gitea uses the other, step records may be written
   onto wrong slots.

The 1-vs-0 indexing is itself worth checking — even if it's not
the cause of this bug, it suggests the reporter and gitea disagree
on what "step N" identifies.

## Operational impact

- **Debugging CI is meaningfully harder.** When a job fails, the
  API tells you nothing about *which* step actually broke. You have
  to fetch the raw log and grep for `Step N ... failed with exit code`.
  The runner UI in gitea is presumably similarly affected (haven't
  cross-checked the web view, only the API).
- **Step duration metrics are unreliable.** Anyone trying to monitor
  drawbar performance via per-step durations (e.g. "how long does
  cargo test take on this branch over time") gets noise — sometimes
  zero, sometimes the previous step's runtime.
- **Doesn't affect job-level outcomes.** The job correctly reports
  `failure`, runs the right post-failure cleanup. Per-task accounting
  toward gitea is unaffected.
- **Also doesn't affect drawbar's own behavior** — runner cache
  decisions, slot accounting, lifecycle all use drawbar-internal state
  that is correct.

## Fix sketch

1. **Stream per-step `UpdateTask` calls as each step transitions**,
   not a single final flush. The actor knows when each step begins
   and ends — those transitions are what's being logged to stderr.
   Pipe them into a `UpdateTask{step: N, status: completed,
   conclusion: success|failure|cancelled, started_at, completed_at}`
   call when each step finishes.
2. **Verify step indexing parity with gitea.** Looking at the log
   header lines (`Step 5 (visual regression suite ...)` vs. API
   index 4), there's a 1-step offset between what the actor logs and
   what the API records. Establish which is canonical and align both
   sides.
3. **Until (1) ships, at minimum stop propagating job-level
   `conclusion` onto step records.** Even if step durations stay
   inaccurate, the per-step `conclusion` fields should reflect what
   that step actually returned, not the job's terminal state. This
   alone makes "which step failed?" answerable from the API.

(1) is the real fix. (3) is a stopgap that restores the most
important debugging signal.

## Related

- Bug 002 (orphaned-job-after-restart): tangential — that's about
  *job-level* status not propagating during restart. This is about
  *step-level* status not propagating during a normal job lifecycle.
- Bug 007 (poller-dies-silently): same theme of "the controller
  knows the right answer but doesn't tell anyone." Different symptom.
- Bugs 014/015 (just shipped in `4cb729d9`): no functional overlap,
  but were the trigger for noticing this — debugging run 86 to verify
  014 forced a close read of the per-step reporting and the
  zero-duration cargo test step is what surfaced it.

## Workarounds

- **Read the log, not the API.** `gt api repos/.../actions/jobs/<id>/logs`
  contains accurate step boundaries (`+ cargo test --lib`,
  `+ bin/visual-test`, `Step N ... failed with exit code N`). Anything
  built on per-step timing data should derive from log parsing until
  the reporter is fixed.
- **Don't trust `conclusion` on individual steps in dashboards or
  alerts.** Treat job-level `conclusion` as authoritative for outcome,
  and ignore step-level `conclusion` until this is fixed.

## Evidence

- API call:
  `gt api 'repos/chaos-inc/bevy_xr_nitro/actions/runs/86/jobs'` —
  shows the misattributed step records in the table above.
- Log stream:
  `gt api 'repos/chaos-inc/bevy_xr_nitro/actions/jobs/91/logs'` — shows
  real step boundaries. Key lines:
  - L148: `2026-05-06T14:35:59.9734394Z + cargo test --lib`
  - L1048–L1307: `test result: ok. ...` — 17+ packages, all green
  - L1310: `2026-05-06T14:39:45.4694244Z + bin/visual-test`
  - L1528: `2026-05-06T14:42:34.9500672Z Step 5 (visual regression
    suite (bin/visual-test)) failed with exit code 1`
- Drawbar pod (during this run):
  `drawbar-766f56c567-vh6qx`, ran clean for 8m13s job duration with
  0 restarts (validating bugs 014/015 fixes in the same run).

## Resolution

Fixed in commits `97cfb9f..345fe58` on `main`. Replaced the
500 ms `cat /shim/state.jsonl` poll loop with a long-lived
`entrypoint tail` exec stream plus a one-shot post-exit drain so
trailing per-step state events are no longer lost when the runner
container exits.

- Spec: [`docs/superpowers/specs/2026-05-06-step-state-streaming-design.md`](../docs/superpowers/specs/2026-05-06-step-state-streaming-design.md)
- Plan: [`docs/superpowers/plans/2026-05-06-step-state-streaming.md`](../docs/superpowers/plans/2026-05-06-step-state-streaming.md)

Live verification against the bug 016 repro shape (multi-step job
with a late-step failure) is pending — handed off to the
test-cluster agent. Fill in run/job IDs and observed per-step
records here once verified.

The 1-vs-0 step-indexing discrepancy noted in this doc (log lines
say "Step 5", API says step 4) was deliberately *not* touched in
this fix — it's a separate human-facing stderr formatting issue.
File a follow-up if it remains a concern.
