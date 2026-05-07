# Reporter has a data race on `r.logOffset` and ignores ctx during Close retries

**Status: filed.** Surfaced 2026-05-06 via `/ultrareview` run on PR #1
(synthetic whole-codebase audit). Two findings in
`pkg/reporter/reporter.go`, both about how the reporter behaves when
things go wrong (concurrent flush, retry-on-shutdown).

## Finding A — race on `r.logOffset` between read and write

**Location:** `pkg/reporter/reporter.go:191` (read) and `:210` (write).

**The bug.** `flushLogs` reads `r.logOffset` outside `r.mu` (line 193,
inside the `connect.NewRequest` argument: `Index: int64(r.logOffset)`)
while line 210 writes `r.logOffset` under `r.mu`. Per Go's memory
model this is a data race. `go test -race` would catch it on a hot
day.

The pattern in the file:

```go
func (r *Reporter) flushLogs(ctx context.Context, noMore bool) error {
    r.mu.Lock()
    rows := make([]*runnerv1.LogRow, len(r.logRows))
    copy(rows, r.logRows)
    r.mu.Unlock()                       // ← unlock before reading logOffset

    if len(rows) == 0 && !noMore {
        return nil
    }

    resp, err := r.client.UpdateLog(ctx, connect.NewRequest(&runnerv1.UpdateLogRequest{
        TaskId: r.taskID,
        Index:  int64(r.logOffset),     // ← read of logOffset, no lock held
        Rows:   rows,
        NoMore: noMore,
    }))
    ...
    r.mu.Lock()
    if ack >= r.logOffset {
        ...
        r.logOffset = ack               // ← write under lock
    }
    r.mu.Unlock()
}
```

**Concrete consequence.** Two concurrent `Flush` calls (e.g., the
periodic daemon and an explicit Flush from the controller) can both
read the *same* `logOffset` value, then both write rows starting at
that index. The second `UpdateLog` request arrives with a stale
`Index` and the server may either reject the duplicate range or
re-record the same rows under a new offset, depending on how Forgejo's
log accumulator handles it. Either way the reporter's bookkeeping has
diverged from the server's.

**Fix sketch.** Snapshot `logOffset` under the same lock that copies
`rows`:

```go
r.mu.Lock()
rows := make([]*runnerv1.LogRow, len(r.logRows))
copy(rows, r.logRows)
offset := r.logOffset
r.mu.Unlock()
...
resp, err := r.client.UpdateLog(ctx, connect.NewRequest(&runnerv1.UpdateLogRequest{
    TaskId: r.taskID,
    Index:  int64(offset),
    ...
}))
```

The trim-on-ack block at the bottom already takes the lock; leave it
alone.

## Finding B — `Reporter.Close` ignores ctx during retry sleeps

**Location:** `pkg/reporter/reporter.go:294-315`.

**The bug.** `Close` retries the final UpdateLog/UpdateTask up to 10
times with exponential backoff (100ms doubling each time, up to
~51.2s) using `time.Sleep(backoff)`. `time.Sleep` does NOT honor ctx
cancellation. Total cumulative sleep across all 10 attempts is
~102.3s of pure sleeping, plus the request RTTs.

If the controller is shutting down (SIGTERM) while a task is in
its final flush, Close will keep sleeping for up to ~100s past the
shutdown signal even though `ctx` was canceled. Bug 014/015 just
landed graceful-drain machinery; this regresses that work.

The relevant block:

```go
backoff := 100 * time.Millisecond
for attempt := range 10 {
    if err := r.flushLogs(ctx, true); err != nil {
        slog.Warn("final log flush failed, retrying", "attempt", attempt+1, "error", err)
        lastErr = err
        time.Sleep(backoff)             // ← ctx-blind sleep
        backoff *= 2
        continue
    }
    if err := r.flushState(ctx); err != nil {
        slog.Warn("final state flush failed, retrying", ...)
        lastErr = err
        time.Sleep(backoff)             // ← same here
        backoff *= 2
        continue
    }
    return nil
}
```

**Fix sketch.** Replace each `time.Sleep(backoff)` with a select on
`ctx.Done()` and `time.After(backoff)`. Return early on ctx cancel
with the last error wrapped — Close becomes responsive to shutdown
within milliseconds.

```go
select {
case <-ctx.Done():
    return fmt.Errorf("close interrupted: %w", ctx.Err())
case <-time.After(backoff):
}
backoff *= 2
```

## Why these belong together

Both are reporter-internal correctness bugs. Both are about graceful
behavior under stress (concurrent flush, shutdown). Both diagnosable
by reading `pkg/reporter/reporter.go` cold. One session.

## Test plan sketch

- Race: `go test -race ./pkg/reporter/...` with a test that fires
  concurrent `Flush` calls. Should fail today; pass after fix.
- Close-on-cancel: existing test pattern with a fake client that
  always errors; cancel ctx mid-retry; assert Close returns within
  ~50ms of cancel rather than ~100s.

## Source

Filed via `/ultrareview` run on PR #1 (synthetic codebase audit),
2026-05-06. Cloud session reported as "Review crashed before
producing findings" but the user retrieved a partial findings list
via the web UI.

## Related

- Bug 014/015: graceful-drain SIGTERM work — finding B regresses that
  story for the per-task close path.
- Bug 016: also a reporter-adjacent concern; finding A's UpdateLog
  semantics matter to per-step reporting accuracy.
