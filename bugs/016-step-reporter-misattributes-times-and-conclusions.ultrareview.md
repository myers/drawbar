# Ultrareview findings — bug 016 fix

Cloud-run review of the bug 016 fix (commits `97cfb9f..881fa8c` on
`main`). Scope reported as 10 files changed, 2913 insertions, 162
deletions. One bug filed.

Review session: <https://claude.ai/code/session_017ryztAYmiqZPuVbf4HPvHY>
(may expire; the cloud sandbox itself is ephemeral and was not
preserved on this machine).

---

## bug_001 — `streamStateFileWith` tight reconnect loop on immediate stream EOF

**Severity:** normal.
**File:** `pkg/k8s/watcher.go`, lines 188–192 (against commit `881fa8c`).

### Summary

`streamStateFileWith` resets the attempt counter and backoff on every
successful `ExecStream`, so the `maxAttempts=5` cap only protects
against the connect-fails path. When connect succeeds but the stream
EOFs immediately (e.g. the startup race where `waitForContainerRunning`
returns before `runEntrypoint` has created `/shim/state.jsonl`, so
`entrypoint tail` hits ENOENT and exits 1), the inner loop breaks and
the outer loop reconnects with no sleep — producing a tight reconnect
loop that hammers the kube API at kubelet round-trip speed until
`state.jsonl` exists.

**Fix:** only reset attempt/backoff after at least one event has been
routed on this stream, or always sleep the initial backoff after EOF
before reconnecting.

### Reasoning (verbatim from the cloud reviewer)

#### What the bug is

`streamStateFileWith` (`pkg/k8s/watcher.go:156-224`) is a reconnect
loop with a 5-attempt cap and exponential backoff. The cap is
implemented two layers deep: the outer loop calls
`executor.ExecStream`; if that returns an error, `attempt` is
incremented and we sleep `backoff` before retrying (lines 175-188). On
success, `attempt = 0; backoff = initialBackoff` (lines 190-192) and
we drop into the inner read loop.

The protection is asymmetric. `maxAttempts=5` only catches errors
*from `ExecStream` itself* — i.e. the SPDY connect failed before the
kubelet exec'd anything. It does **not** catch the case where
`ExecStream` returns `(stream, nil)` successfully but the stream EOFs
immediately. In that case we hit the inner loop, `ReadString` returns
`("", io.EOF)`, the `errors.Is(rErr, context.Canceled)` branch is
false, we fall through to `break` (line 221), and control returns to
the top of the outer `for {}`. `ctx.Err()` is nil, so we re-invoke
`ExecStream` with no sleep. And because `attempt` was reset to 0 on
the prior connect, even if this immediate-EOF cycle keeps repeating,
`attempt` never accumulates — the maxAttempts cap can never trigger.

#### The trigger: startup race

This is exactly the shape of the startup race: `waitForContainerRunning`
(`pkg/k8s/watcher.go:577-608`) returns as soon as the runner
container's status is `Running` or `Terminated`. `Running` only
requires PID 1 — the entrypoint binary — to have started. But
`runEntrypoint` (`cmd/entrypoint/main.go:71-78`) must execute Go
runtime init, then `loadManifest` (file I/O), then call
`os.OpenFile(state.jsonl, O_CREATE|O_WRONLY|O_APPEND, 0o644)`. There
is a window of tens to hundreds of milliseconds where
`Running == true` but `state.jsonl` does not yet exist.

During that window, the controller's `streamStateFileWith` invokes
`executor.ExecStream(ctx, ..., "/shim/entrypoint", "tail", "--skip",
"0", "/shim/state.jsonl")`. The kubelet exec succeeds — the entrypoint
binary exists at `/shim/entrypoint` (copied by `setup-shim`), and
`tail` starts. Inside `runTail` (`cmd/entrypoint/tail.go:65-68`),
`os.Open(args.path)` returns `ENOENT` because `state.jsonl` doesn't
exist yet (the path-only `os.Open` does not include `O_CREATE`).
`runTail` returns a wrapped error; `main.go:51` prints to stderr and
calls `os.Exit(1)`. The SPDY exec stream EOFs.

#### Step-by-step proof

```
T+0 ms: waitForContainerRunning returns (runner is Running)
T+1 ms: streamStateFileWith outer loop iter 1: ExecStream → success
        attempt = 0, backoff = 50ms (already)
        ReadString → ("", io.EOF) — file doesn't exist, tail process exited
        Not Canceled/DeadlineExceeded → break inner loop
T+2 ms: outer loop iter 2: ctx.Err() == nil, no sleep
        ExecStream → success
        attempt = 0 (reset!), backoff = 50ms (reset!)
        ReadString → ("", io.EOF) again
T+~10-50 ms (kubelet round-trip): repeat
... continues until state.jsonl is created and tail has lines to stream
```

Each iteration is a fresh SPDY exec call against the kube API server.
The natural rate-limit is kubelet round-trip latency (~10-50 ms), so
this isn't pure CPU-burn, but it can spam dozens-to-hundreds of exec
API calls during the startup race window. client-go QPS limits and
the apiserver's exec rate-limit may throttle this; otherwise,
depending on cluster load, it can:

1. Eat client-go QPS budget that other watches/lists need.
2. Burn kubelet/apiserver resources.
3. Trigger any rate-limit alerting in the kube control plane.

The startup race is unavoidable — it happens at every job startup.
Worse, the same fast-EOF pattern would recur for any other condition
that makes `tail` exit fast (file gets deleted, permissions issue,
race on log truncation, etc.) and there's no upper bound on how long
it could last if `state.jsonl` were never created (e.g. manifest
parse failure in `runEntrypoint`).

#### Why existing protections don't catch this

- `maxAttempts=5` increment lives only in the `if err != nil` branch
  on the `ExecStream` return value (line 175). When connect succeeds
  but the stream EOFs, that branch is never entered.
- The reset `attempt = 0; backoff = initialBackoff` (lines 190-192)
  is unconditional on successful connect, so even repeated
  immediate-EOF cycles never accumulate toward maxAttempts.
- The post-EOF break-and-reconnect path (line 221) has no sleep at all.

#### Fix

Two simple options, either suffices:

1. **Gate the reset on having routed events.** Track whether any line
   was successfully read from the current stream. Only reset
   `attempt`/`backoff` after the first successful read; otherwise
   treat the immediate-EOF as a connection-level failure and keep the
   attempt counter incrementing.

2. **Always sleep initial backoff after a stream ends with no data.**
   Even simpler: detect `linesRead == 0` when the inner loop breaks,
   and unconditionally `time.Sleep(backoff)` (or honor ctx) before the
   outer loop retries. This rate-limits the reconnect cadence to at
   least 50 ms even when attempt is reset, and gives `state.jsonl`
   time to appear.

Option 1 is preferable because it preserves backoff growth across the
entire pre-creation window. The post-exit drain at `watchJobWith`
line 130 is the safety net for events written near container exit;
this loop's job is to track events live, and busy-reconnecting against
an empty file isn't doing that anyway.

---

## Resolution

Fixed in commit `f38f1a2` on `main` (`k8s: rate-limit reconnects when
tail stream produces no lines`). Implementation chose **option 1
combined with option 2**: gate the reset of `attempt`/`backoff` on
having routed at least one full line on the current stream, *and*
treat an unproductive stream the same as a failed connect — push it
through the same `backoffStep` helper so both failure modes share the
cap and rate-limit.

Two regression tests in `pkg/k8s/watcher_test.go`:

- `TestStreamStateFile_ImmediateEOFAppliesBackoff` — five empty streams
  in a row trip the cap and return an error.
- `TestStreamStateFile_ProductiveStreamResetsAttempts` — a productive
  stream resets the counter so subsequent unproductive cycles get
  their own 5-attempt budget.
