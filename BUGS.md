# Bugs Found During E2E Testing — All Fixed

## 1. Checkout fails: `git fetch` doesn't include specific commit SHA

**Status:** Fixed — removed built-in checkout, use `actions/checkout@v4` instead.

The built-in checkout reimplemented `actions/checkout` with structured git args but had a bug: `git fetch --depth=1 origin -- <SHA>` doesn't work for non-tip SHAs. Rather than fixing the edge case, we removed the built-in entirely. `actions/checkout@v4` is now loaded as a normal Node.js action and handles all checkout scenarios (force-pushed SHAs, submodules, LFS, etc.).

## 2. `continue-on-error` doesn't prevent job failure

**Status:** Fixed — the logic was already correct by the time of final testing.

When `ContinueOnError` is true and a step fails: `hadFailure` is not set, `overallSuccess` stays true, and `eval.SetJobStatus("failure")` is skipped. Subsequent steps without `if:` conditions continue running. Verified by `TestRunEntrypoint_ContinueOnError`.

## 3. Job-level `env:` not propagated to steps

**Status:** Fixed — `parsed.Env` is now merged into `baseEnv` in `makeTaskHandler`.

Job-level env vars are available as real environment variables in every step via the manifest's `BaseEnv` field.
