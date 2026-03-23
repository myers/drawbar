# Bugs Found During E2E Testing (Forgejo v14, 2026-03-23)

## 1. Checkout fails: `git fetch` doesn't include specific commit SHA

**Symptom:** `error: pathspec 'a7dc04c...' did not match any file(s) known to git`

**Root cause:** `pkg/workflow/checkout.go` does `git fetch origin <branch>` then `git checkout <sha>`. The fetch only pulls the branch tip. If the task's commit SHA isn't the tip (e.g., a force-push happened between dispatch and execution, or multiple commits were pushed), the SHA isn't in the local repo.

**Fix:** Change the fetch to `git fetch origin <sha>` or `git fetch --depth=1 origin +<sha>:refs/heads/<branch>`. Alternatively, fetch the full branch (`--unshallow` or remove `--depth=1`), then checkout the SHA.

**File:** `pkg/workflow/checkout.go` — `ResolveCheckout` / `ToStepSpecs`

## 2. `continue-on-error` doesn't prevent job failure

**Symptom:** Step with `continue-on-error: true` exits 1, next step never runs, job reports failure.

**Root cause:** `cmd/entrypoint/main.go` in the step loop — when `hadFailure` is set and `hasRuntimeConditions` is false, the entrypoint breaks out of the loop. The `ContinueOnError` flag is checked but doesn't prevent `hadFailure` from being set, so subsequent steps without `if:` conditions are skipped via the `hadFailure && step.If == ""` guard.

The logic should be: if `ContinueOnError` is true, don't set `hadFailure` or `overallSuccess = false`. The step failed but the job should continue as if it succeeded.

**Fix:** In the failure handling block (~line 170), when `ContinueOnError` is true, do NOT set `hadFailure = true` or `overallSuccess = false`. Also do NOT set `eval.SetJobStatus("failure")` — the job status should remain "success" because continue-on-error masks the failure.

**File:** `cmd/entrypoint/main.go` — step loop failure handling

## 3. Job-level `env:` not propagated to steps

**Symptom:** `JOB_VAR=from-job` set at job level is empty inside steps. Step-level `env:` works.

**Root cause:** `cmd/controller/main.go` in `makeTaskHandler` — when building the manifest, job-level env vars from `parsed.Env` are used to create the expression evaluator but are NOT passed into `baseEnv` or the individual step environments. They're only used for expression interpolation context, not as actual environment variables.

In GitHub Actions, job-level `env:` should be available as real environment variables in every step. Currently only `baseEnv` (cache URLs, artifact URLs) is injected as manifest-wide env, and step-level `env:` is per-step.

**Fix:** Merge `parsed.Env` into `baseEnv` in `makeTaskHandler`, before the step-building loop. This makes job-level env vars available to all steps via the manifest's `BaseEnv` field, which the entrypoint layers into every step's environment.

```go
// In makeTaskHandler, after baseEnv is initialized:
for k, v := range parsed.Env {
    baseEnv[k] = v
}
```

**File:** `cmd/controller/main.go` — `makeTaskHandler`, around the `baseEnv` construction
