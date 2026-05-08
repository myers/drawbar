# Entrypoint shell and action-result semantics diverge from GitHub Actions

**Status: fixed 2026-05-07.** Surfaced 2026-05-06 via `/ultrareview` run on PR #1.
Three findings about places where drawbar's behavior subtly differs
from documented GitHub/Forgejo Actions behavior. All diagnosable by
reading the actions-runner spec and our entrypoint side-by-side.

## Finding A — bash steps run without `-o pipefail`, masking errors in piped commands

**Location:** `cmd/entrypoint/main.go:320-322`.

**The bug.** `executeStep` invokes bash as:

```go
case "bash":
    cmd = exec.CommandContext(ctx, "/bin/bash", "-e", "-c", step.Command)
```

Just `-e`, no `-o pipefail`. GitHub Actions explicitly documents that
bash steps run with **`set -eo pipefail`** (see GitHub docs:
"jobs.<job_id>.steps[*].shell" → bash uses `-eo pipefail`). Forgejo
Actions matches this.

**Concrete consequence.** A pipeline like
`curl -sSL bad-url | tar xz` returns the exit code of `tar xz` (which
sees an empty input and "succeeds") rather than `curl`'s failure.
Workflows author-tested against GitHub will appear to pass on drawbar
but actually swallow upstream failures.

**Fix sketch.** Two equivalent forms:

```go
cmd = exec.CommandContext(ctx, "/bin/bash", "-eo", "pipefail", "-c", step.Command)
```

or:

```go
cmd = exec.CommandContext(ctx, "/bin/bash", "--noprofile", "--norc", "-eo", "pipefail", "-c", step.Command)
```

GitHub uses the latter. `--noprofile --norc` is small additional
hardening (skips `/etc/profile`, `~/.bashrc`) but makes drawbar
behavior more reproducible across base images.

**Caveat.** Workflows that *rely* on lack of pipefail (rare — usually
this is a bug, not a deliberate choice) will start failing. Worth a
release note; the fix is correct.

## Finding B — `SetStepResult` conflates `outcome` and `conclusion`, ignores `continue-on-error`

**Location:** `pkg/expressions/eval.go:89-99` (and probably a few lines
below).

**The bug.** `SetStepResult` sets BOTH `Outcome` and `Conclusion` to
the same value derived from one `outcome` parameter:

```go
func (e *Evaluator) SetStepResult(stepID string, outcome string, outputs map[string]string) {
    if e.env.Steps == nil {
        e.env.Steps = make(map[string]*model.StepResult)
    }
    e.env.Steps[stepID] = &model.StepResult{
        Conclusion: model.StepStatusSuccess,
        Outcome:    model.StepStatusSuccess,
        Outputs:    outputs,
    }
    if outcome == "failure" {
        // ... presumably flips both to failure
    }
}
```

Per GitHub Actions spec, **Outcome** is the raw step exit status, and
**Conclusion** applies the `continue-on-error` modifier:

| Step exited | continue-on-error | Outcome | Conclusion |
|---|---|---|---|
| 0 | n/a | success | success |
| ≠0 | false | failure | failure |
| ≠0 | true | failure | success |

`${{ steps.foo.outcome }}` and `${{ steps.foo.conclusion }}` are
distinct context variables. `${{ failure() }}` and `success()` use
**conclusion** (so a `continue-on-error: true` failure does NOT trip
`failure()` for subsequent steps). Drawbar conflating them means
those two contexts return the same value, and `continue-on-error`
doesn't behave per spec.

The entrypoint's `runEntrypoint` does set job status correctly (it
checks `step.ContinueOnError` and avoids tripping `eval.SetJobStatus("failure")`),
but the per-step Outcome/Conclusion split is lost inside `SetStepResult`.

**Fix sketch.** Pass continue-on-error through to `SetStepResult`:

```go
func (e *Evaluator) SetStepResult(stepID string, outcome string, continueOnError bool, outputs map[string]string) {
    if e.env.Steps == nil {
        e.env.Steps = make(map[string]*model.StepResult)
    }
    var conclusion string
    if outcome == "failure" && continueOnError {
        conclusion = "success"
    } else {
        conclusion = outcome
    }
    e.env.Steps[stepID] = &model.StepResult{
        Outcome:    model.StepStatus(outcome),
        Conclusion: model.StepStatus(conclusion),
        Outputs:    outputs,
    }
}
```

And update the entrypoint call site (around `cmd/entrypoint/main.go:221`)
to pass `step.ContinueOnError`.

## Finding C — `parseEnvFile` mis-parses key=value lines containing `<<` as heredoc

**Location:** `cmd/entrypoint/envfile.go:26-50`.

**The bug.** Order-of-checks issue:

```go
for scanner.Scan() {
    line := scanner.Text()
    // Check for heredoc: key<<DELIMITER
    if idx := strings.Index(line, "<<"); idx > 0 {
        key := line[:idx]
        delim := line[idx+2:]
        // ... heredoc parsing ...
        continue
    }
    // Simple key=value
    if idx := strings.IndexByte(line, '='); idx > 0 {
        key := line[:idx]
        val := line[idx+1:]
        result[key] = val
    }
}
```

Any line containing `<<` is treated as heredoc start, even when `=`
appears earlier. Concrete failure: a perfectly valid `KEY=foo<<bar`
becomes a heredoc with key=`KEY=foo` and delimiter `bar`.

The GitHub Actions docs are explicit: heredoc form is
`KEY<<DELIMITER` (no `=` between key and `<<`); the `=` form is
`KEY=VALUE`. The two are mutually exclusive on a single line, and
the `=` form is the more common.

**Fix sketch.** Check the `=` position first. Only treat as heredoc
if `<<` appears in the part of the line BEFORE any `=`:

```go
eqIdx := strings.IndexByte(line, '=')
hereIdx := strings.Index(line, "<<")

// Heredoc only if << appears at the start (key) and before any =, OR
// there is no = on the line at all.
if hereIdx > 0 && (eqIdx < 0 || hereIdx < eqIdx) {
    key := line[:hereIdx]
    delim := line[hereIdx+2:]
    // ... heredoc parsing ...
    continue
}
if eqIdx > 0 {
    key := line[:eqIdx]
    val := line[eqIdx+1:]
    result[key] = val
}
```

## Why these belong together

All three are "drawbar's behavior diverges from documented GH/Forgejo
Actions behavior." Same diagnostic flow: read the spec, read our
implementation, find the mismatch. Fix is small in each case but the
test surface is real (each fix needs at least one test verifying spec
compliance).

## Test plan sketch

- A: a step with `run: false | true` should fail under pipefail.
  Add a test that runs that command and asserts non-zero exit.
  Existing passes-without-pipefail tests should still pass.
- B: a step with `continue-on-error: true` that fails. Assert
  `${{ steps.foo.outcome }}` evaluates to `failure` and
  `${{ steps.foo.conclusion }}` evaluates to `success`. Assert
  `${{ failure() }}` in a subsequent step does NOT trip.
- C: extend `envfile_test.go` with a `KEY=value<<weird` case. Assert
  it parses as `KEY=value<<weird` (single `=` line), not as heredoc.

## Source

Filed via `/ultrareview` run on PR #1, 2026-05-06.

## Related

- Bug 016 area: per-step reporting. Finding B affects how
  `${{ steps.X.outcome }}` references resolve in later steps' `if:`
  conditions, which the entrypoint evaluates at runtime.

## Resolution

All three findings fixed.

**A:** `cmd/entrypoint/main.go::executeStep` now invokes
`/bin/bash --noprofile --norc -eo pipefail -c <command>` for `bash`
shell steps, matching documented GitHub Actions behavior.
Regression tests in `cmd/entrypoint/main_test.go` assert both the
flag set (`TestExecuteStep_BashUsesPipefail`) and the actual
end-to-end pipefail behavior against the real `/bin/bash`
(`TestExecuteStep_BashPipefailRealShell`, runs `false | true` and
expects non-zero exit).

**B:** `Evaluator.SetStepResult` (`pkg/expressions/eval.go`) now
takes `continueOnError bool` and records distinct values for
`Outcome` (raw exit status) and `Conclusion` (continue-on-error
masked). The entrypoint passes `step.ContinueOnError`. Tests:
`TestSetStepResult_OutcomeAndConclusionTable` covers the four
cells of the spec table, and
`TestSetStepResult_FailureFunctionRespectsConclusion` locks in
that `${{ failure() }}` doesn't trip after a failed step with
`continue-on-error: true` (since `failure()` consults conclusion).

**C:** `parseEnvFile` (`cmd/entrypoint/envfile.go`) now compares
the index of `=` and `<<` and only treats a line as a heredoc
start if `<<` appears in the key portion (i.e. before any `=`).
A line like `KEY=value<<weird` parses as the obvious key=value.
Test: `TestParseEnvFile_ValueContainingDoubleAngle`.

### Review follow-ups

`/review` of the fix commit raised three minor follow-ups, addressed
in a small follow-up commit:

- **`--noprofile --norc` workflow risk:** verified — ripgrep across
  `actions/`, `hack/`, `deploy/`, and all repo `.yml`/`.yaml` files
  found no workflows that `source ~/.bashrc` or `source /etc/profile`.
  The repo doesn't ship example workflows; the deploy YAMLs are k8s
  manifests, not Actions workflows. Matches the GitHub Actions runner
  default — no action needed.
- **Test rename:** `TestParseEnvFile_HeredocAfterFixStillParses` →
  `TestParseEnvFile_HeredocWithoutEquals`. The "AfterFix" qualifier
  was temporal and would rot; the new name describes the syntax under
  test (heredoc with no `=` before `<<`).
- **Outcome-string contract:** `SetStepResult` now uses an explicit
  `switch` over the documented outcome values (`success`, `failure`)
  with a default arm that warn-logs the offending value and falls
  back to success. Today's only callers pass `"success"` or
  `"failure"`; the guard makes the contract obvious in the function
  body so a future "skipped" path either extends the switch or calls
  a new method instead of silently mapping to success. Test:
  `TestSetStepResult_UnknownOutcomeWarnsAndDefaultsToSuccess`.
