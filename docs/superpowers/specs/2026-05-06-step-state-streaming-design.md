# Step state streaming: fix mis-attributed step times and conclusions

**Status:** design.
**Bug:** [`bugs/016-step-reporter-misattributes-times-and-conclusions.md`](../../../bugs/016-step-reporter-misattributes-times-and-conclusions.md).

## Summary

Drawbar reports incorrect per-step `started_at` / `completed_at` /
`conclusion` to the forge when a job has multiple sequential steps and a
later step fails. Earlier successful steps appear as `failure`, durations
collapse to `0s`, and the failing step's reported window absorbs the
runtime of preceding steps. The data shown by the gitea Actions UI and
its API is therefore unreliable for debugging.

The root cause is a race at the boundary between the runner container
exiting (which closes the log stream and triggers the controller to
finalize) and the controller's 500 ms `cat /shim/state.jsonl` poll loop.
Trailing `end` events written by the entrypoint within the last poll
interval are never read by the controller before `Reporter.Close` runs,
so those step records reach gitea with no `stopped_at`/`result`. Gitea
then infers them from the terminal task result (`failure`) and stamps
every still-open step accordingly.

This design replaces the periodic `cat`-poll with a **persistent line-by-line
tail** of `/shim/state.jsonl`, plus a **post-exit authoritative drain**
that runs after the runner container exits but before the reporter
closes. Together they eliminate the race and make per-step timing
deliverable in real time, which also improves UI freshness for live runs.

## Goals

- Per-step `started_at` / `completed_at` / `conclusion` reported to the
  forge match what actually happened in the pod.
- Step state events reach the reporter with sub-second latency (down
  from the current 500 ms floor).
- No regressions in log streaming, secret masking, workflow command
  parsing, or `Reporter.Close` semantics.
- No protocol-level change vs. gitea/forgejo. The fix is internal to the
  controller/entrypoint boundary.

## Non-goals

- Changing the on-disk format of `state.jsonl`.
- Restructuring the reporter, the workflow command processor, or the
  log-streaming path.
- Handling pod restart mid-job (covered by bug 002) or controller
  restart mid-job (covered by `runShutdownRecovery`).
- Backwards compatibility with shim images that lack the new
  `entrypoint tail` subcommand. Drawbar is early alpha; the controller
  and shim ship as a unit.

## Architecture

Three additive changes on the existing controller/entrypoint boundary.

1. **New entrypoint subcommand** `entrypoint tail [--once] [--skip N] <path>`.
   Streams an append-only file's lines to stdout. In follow mode it sleeps
   briefly on `EOF` and retries, so it sees writes as they land. In
   `--once` mode it reads the current contents and exits. `--skip N`
   drops the first N newline-terminated lines, used by the controller to
   resume after a reconnect without replaying events.

2. **Replace `pollStateFileWith` with `streamStateFileWith`** in
   `pkg/k8s/watcher.go`. Opens one long-lived `executor.ExecStream(ctx, …,
   ["/shim/entrypoint", "tail", "/shim/state.jsonl"])`, reads stdout via
   `bufio.Reader.ReadString('\n')`, JSON-decodes each line into
   `types.StateEvent`, and routes through the existing `routeStateEvent`.
   Reconnects with `--skip <lastOffset>` on transient SPDY drops, with
   bounded exponential backoff.

3. **Post-exit authoritative drain.** After log streaming returns and
   before `watchJobWith` returns, do a single one-shot
   `executor.ExecStream(ctx, …, ["/shim/entrypoint", "tail", "--once",
   "--skip", "<lastOffset>", "/shim/state.jsonl"])`. Decode and route any
   trailing events the persistent tail missed because of the
   container-exit race. Best-effort: if the exec fails (e.g. container
   already terminated and gone), log a warning and proceed.

The 2 × `cfg.PollInterval` `time.Sleep` at the end of `watchJobWith` is
removed. The reporter, the on-wire `state.jsonl` format, and the
controller's task-handling code in `cmd/controller/main.go` are unchanged.

## Components

### `cmd/entrypoint/main.go` — `tail` subcommand

New top-level case alongside `setup` and `run`.

- CLI shape: `entrypoint tail [--once] [--skip N] <path>`.
- Open `<path>` `O_RDONLY`. Wrap in `bufio.Reader`.
- If `--skip N` is given, drop the first N newline-terminated lines
  before emitting anything. Lines that do not end in `\n` (e.g. partial
  trailing line at EOF) do not count toward N and are not consumed.
- Loop: `ReadString('\n')`. On full line, write verbatim to `os.Stdout`
  (including the trailing newline) and call `os.Stdout.Sync()` (or use
  unbuffered output). On `io.EOF`:
  - If `--once`: return cleanly.
  - Else: `time.Sleep(50 * time.Millisecond)`, retry.
- Exit on stdout write error (parent disconnected) or `SIGPIPE`. The
  controller is the only intended reader; when it closes the SPDY
  exec connection, the kernel signals the tail process and it exits.
- No JSON parsing — the tail subcommand is byte-faithful. The controller
  decodes.
- New file: `cmd/entrypoint/tail.go`. Tests: `cmd/entrypoint/tail_test.go`.

### `pkg/k8s` — `PodExecutor` interface change

Today's `PodExecutor.Exec(ctx, ns, pod, container, cmd) (string, error)`
runs a command and returns its full stdout once it exits. The new
streaming use case needs a long-lived exec whose stdout is read
incrementally.

Replace the interface:

```go
type PodExecutor interface {
    ExecStream(ctx context.Context, namespace, pod, container string,
        cmd []string) (io.ReadCloser, error)
}
```

`SPDYExecutor` already uses `remotecommand.NewSPDYExecutor` and
`StreamWithContext`. Update its `Exec` method to return a reader rather
than a buffered string. Existing `Exec`-style call sites (none outside
`pkg/k8s` after this change) are converted to read from the reader, or
collected into a string via `io.ReadAll` if a one-shot result is what
they want.

### `pkg/k8s/watcher.go` — `streamStateFileWith`

Replaces `pollStateFileWith`. Same caller (`watchJobWith` line ~110).

Pseudocode:

```go
func streamStateFileWith(ctx context.Context, executor PodExecutor,
    namespace, podName string, rep *reporter.Reporter) (lastOffset int, err error) {

    backoff := 50 * time.Millisecond
    const (
        maxBackoff  = 2 * time.Second
        maxAttempts = 5
    )
    attempt := 0

    for {
        if ctx.Err() != nil {
            return lastOffset, ctx.Err()
        }

        cmd := []string{"/shim/entrypoint", "tail",
            "--skip", strconv.Itoa(lastOffset), "/shim/state.jsonl"}
        stream, exErr := executor.ExecStream(ctx, namespace, podName, "runner", cmd)
        if exErr != nil {
            attempt++
            if attempt >= maxAttempts {
                slog.Error("state stream gave up after retries",
                    "lastOffset", lastOffset, "err", exErr)
                return lastOffset, exErr
            }
            time.Sleep(backoff)
            if backoff < maxBackoff {
                backoff *= 2
            }
            continue
        }
        // Successful connect resets backoff.
        attempt = 0
        backoff = 50 * time.Millisecond

        reader := bufio.NewReader(stream)
        for {
            line, rErr := reader.ReadString('\n')
            if line != "" {
                trimmed := strings.TrimRight(line, "\n\r")
                if trimmed == "" {
                    // entrypoint shouldn't emit blank lines, but tolerate.
                    lastOffset++
                    continue
                }
                var ev types.StateEvent
                if jErr := json.Unmarshal([]byte(trimmed), &ev); jErr != nil {
                    slog.Debug("skipping malformed state event",
                        "line", trimmed, "err", jErr)
                    lastOffset++
                    continue
                }
                routeStateEvent(ev, rep)
                lastOffset++
            }
            if rErr != nil {
                stream.Close()
                if rErr == io.EOF {
                    // Stream ended (container exit, kubelet drop, etc.).
                    // Try to reconnect; the post-exit drain is the safety net.
                    break
                }
                if errors.Is(rErr, context.Canceled) ||
                   errors.Is(rErr, context.DeadlineExceeded) {
                    return lastOffset, rErr
                }
                break
            }
        }
    }
}
```

The function returns the final `lastOffset` so `watchJobWith` can pass it
to `drainStateFile`.

### `pkg/k8s/watcher.go` — `drainStateFile`

Best-effort one-shot read of any events written between the last
streaming line we received and the runner container's exit.

```go
func drainStateFile(ctx context.Context, executor PodExecutor,
    namespace, podName string, rep *reporter.Reporter, lastOffset int) {

    drainCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
    defer cancel()

    cmd := []string{"/shim/entrypoint", "tail", "--once",
        "--skip", strconv.Itoa(lastOffset), "/shim/state.jsonl"}
    stream, err := executor.ExecStream(drainCtx, namespace, podName, "runner", cmd)
    if err != nil {
        slog.Warn("post-exit state drain exec failed",
            "err", err, "lastOffset", lastOffset, "pod", podName)
        return
    }
    defer stream.Close()

    reader := bufio.NewReader(stream)
    for {
        line, rErr := reader.ReadString('\n')
        if line != "" {
            trimmed := strings.TrimRight(line, "\n\r")
            if trimmed == "" {
                continue
            }
            var ev types.StateEvent
            if jErr := json.Unmarshal([]byte(trimmed), &ev); jErr != nil {
                slog.Debug("skipping malformed state event",
                    "line", trimmed, "err", jErr)
                continue
            }
            routeStateEvent(ev, rep)
        }
        if rErr != nil {
            if rErr != io.EOF {
                slog.Warn("post-exit state drain stream error",
                    "err", rErr, "lastOffset", lastOffset)
            }
            return
        }
    }
}
```

### `pkg/k8s/watcher.go` — `watchJobWith`

Replace the existing block:

```go
// Stream logs and poll state in parallel.
logDone := make(chan error, 1)
go func() { logDone <- streamLogs(...) }()

stateDone := make(chan error, 1)
go func() { stateDone <- pollStateFileWith(...) }()

<-logDone
time.Sleep(cfg.PollInterval * 2)
result, err := getContainerResult(...)
return result, err
```

with:

```go
logDone := make(chan error, 1)
go func() { logDone <- streamLogs(...) }()

type streamResult struct{ offset int; err error }
stateDone := make(chan streamResult, 1)
streamCtx, cancelStream := context.WithCancel(ctx)
defer cancelStream()
go func() {
    off, err := streamStateFileWith(streamCtx, executor, namespace, podName, rep)
    stateDone <- streamResult{off, err}
}()

<-logDone
cancelStream()
res := <-stateDone

drainStateFile(ctx, executor, namespace, podName, rep, res.offset)

result, err := getContainerResult(ctx, client, namespace, podName)
if err != nil {
    return runnerv1.Result_RESULT_FAILURE, err
}
return result, nil
```

The `time.Sleep` is gone. The post-exit drain replaces it with bounded
work that targets the actual problem.

### `pkg/types/step.go`, `pkg/reporter/*`, `cmd/controller/*`

No changes. The `StateEvent` struct, the reporter's `StartStep`/
`FinishStep`/`Close` semantics, and the controller's task-handling
code are unchanged. The `PodExecutor` interface change is internal to
`pkg/k8s` — `WatchConfig.Executor` is left at the zero value by the
controller, so the interface swap does not propagate outward.

## Data flow

### Mid-job, steady state

```
runner step proc       entrypoint run                   entrypoint tail              controller
    │                       │                                 │                            │
    │                       │ writeState(start) → state.jsonl │                            │
    │                       │                                 │ ReadString('\n') → stdout │
    │                       │                                 │                            │ bufio.ReadString
    │                       │                                 │                            │ json.Unmarshal
    │                       │                                 │                            │ routeStateEvent
    │                       │                                 │                            │   → rep.StartStep
```

`Reporter`'s 1 s daemon picks up the change on its next tick and pushes
to the forge via `UpdateTask`.

### End of job (the case bug 016 breaks today)

```
T0: step N fails, runner container exits

streamLogs            ──► returns (EOF on log stream)
streamStateFileWith   ──► ctx canceled, exits with lastOffset=K
drainStateFile        ──► one-shot exec: tail --once --skip K state.jsonl
                          - 0 lines: we already had everything, no-op
                          - M lines: trailing end events the live tail missed,
                            routed into rep before Close
getContainerResult    ──► reads pod status for exit code
return result         ──►
controller Close      ──► daemon canceled, terminal flush:
                          - per-step state correct
                          - any still-UNSPECIFIED step → CANCELLED
                          - UpdateTask with task result + final step state
```

### Reconnect during run

```
streamStateFileWith reading line 7 ──► exec returns error: connection reset
                                       │ backoff 50ms (then 100, 200, 400, 800, cap 2s)
                                ──►    re-exec: tail --skip 7 state.jsonl
                                       entrypoint drops first 7 lines, then streams
                                       │ controller resumes; lastOffset=7, no dupes
```

## Error handling

| Failure | Behavior |
|---|---|
| Tail exec fails to start | Reconnect with backoff up to 5 attempts. Then `slog.Error("state stream gave up …")`, return; post-exit drain is the safety net. |
| Tail exec EOFs mid-job (kubelet drop, SPDY blip) | Reconnect via `--skip lastOffset`. No replay because lastOffset gates the entrypoint side. |
| Malformed JSON on a state line | Increment `lastOffset`, log at `Debug` (matches existing `parseStateEvents`), continue. One bad line cannot wedge the stream. |
| Partial line at EOF | `bufio.Reader.ReadString('\n')` returns the partial line + `EOF`. The controller does not consume partial lines (no trailing `\n` ⇒ no `ReadString` success). The entrypoint's tail subcommand likewise only emits whole lines, so the controller never sees one. |
| Post-exit drain exec fails (terminated container, pod gone) | `slog.Warn("post-exit state drain failed", …)`. Whatever events were already received during the run stand. Steps still UNSPECIFIED at `Close` time get stamped `CANCELLED` — strictly better than today's behavior, and rare. |
| `entrypoint` binary missing from `/shim/entrypoint` | Tail exec fails. After max retries, `slog.Error(…)`. The drain also fails. The job's per-step records will be incomplete but the job-level result is still authoritative. This case should not arise in practice because `setup-shim` copies the entrypoint as part of the init flow. |
| Context cancel from controller (timeout, shutdown) | Propagates through `ExecStream`. Tail subcommand exits via `SIGPIPE` on next write. Drain bails immediately if ctx is already canceled — no 5 s wasted timeout. |

## Testing

### Unit

`cmd/entrypoint/tail_test.go` (new):

- `TestTailOnce` — N lines in, N lines out, in order.
- `TestTailOnceWithSkip` — `--skip 3` drops the first 3 lines.
- `TestTailFollow` — start tail, append lines while it's running, assert each appears within 200 ms.
- `TestTailFollowPartialLine` — write `{"event":"start"` (no newline), assert nothing emitted; complete the line, assert it appears.
- `TestTailNonexistentFile` — returns an error promptly.

`pkg/k8s/watcher_test.go` (extend):

- `TestStreamStateFile` — fake `PodExecutor.ExecStream` returns a reader of pre-canned JSONL; assert each event is routed.
- `TestStreamStateFileReconnect` — first exec yields 3 lines then errors; second exec called with `--skip 3`; no events duplicated.
- `TestStreamStateFileMalformedLine` — malformed line followed by valid line; only the valid line is routed; offset advances on both.
- `TestStreamStateFileMaxRetries` — exec errors 5 times; function returns with the right offset and an error; no panics, no leaks.
- `TestDrainStateFile` — fake exec returns a few lines; all are routed.
- `TestDrainStateFileTerminatedContainer` — fake exec returns a "container terminated" error; warn is logged; function returns nil.

`pkg/reporter/*` — no test changes; reporter behavior is unchanged.

### Integration

`pkg/k8s/watcher_integration_test.go` (new, build-tag-gated per CLAUDE.md
conventions):

- Reproduce the bug 016 scenario end-to-end against a real (or envtest)
  pod: a multi-step run where a later step fails and the runner
  container exits within ~100 ms of writing the trailing `end` events.
  Assert that all per-step `started_at` / `stopped_at` / `result` values
  match the file, with no events lost or duplicated.

This is the test that proves the bug is actually fixed.

### Manual verification

Handed off to a separate test-cluster agent. The bug 016 doc has the
exact `gt api` queries (`repos/.../actions/runs/<N>/jobs`,
`repos/.../actions/jobs/<id>/logs`) that show the misattribution
today; running the same queries against a re-triggered run should show
correct per-step records.

## Out of scope

- Bug 002 (orphaned-job-after-restart) — separate concern.
- Bug 007 (poller-dies-silently) — same theme but different surface.
- Sentinel-prefixed step events on stdout (the alternate "tunnel state
  through the log channel" design). Tail-via-exec is a smaller change
  that addresses the observed bug; the stdout-sentinel idea remains
  available as a future option if we ever want to remove the SPDY exec
  channel entirely.

## References

- Bug filing: [`bugs/016-step-reporter-misattributes-times-and-conclusions.md`](../../../bugs/016-step-reporter-misattributes-times-and-conclusions.md)
- Today's poll loop: `pkg/k8s/watcher.go:128–214`
- State event writer: `cmd/entrypoint/main.go:64,114,128,138,225,348`
- Reporter close semantics: `pkg/reporter/reporter.go:271–318`
