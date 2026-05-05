# Poller wedge — `http2.Transport.ReadIdleTimeout` not set on connect-go HTTP client

**Status: fixed.** Landed in commits `21d4d93` (transport PING knobs) and
`9659196` (belt-and-suspenders heartbeat). See `pkg/server/client.go::buildTransport`
and `pkg/server/poller.go::poll`. Verification against upstream `act_runner`
on 2026-05-05: their `internal/pkg/client/http.go::getHTTPClient` does **not**
set `ReadIdleTimeout`, so this fix is intentionally ahead of upstream rather
than mirroring it. Drawbar's deployment shape (long-poll through cloud LB /
NAT) hits the wedge mode upstream apparently hasn't yet — they may simply
not have noticed, or they tolerate a 2-hour TCP-keepalive recovery.

## Summary

The connect-go RPC client in `pkg/server/client.go` is constructed with a
default `*http.Transport` that never gets `http2.ConfigureTransports`
called on it. Go's stdlib auto-upgrades to HTTP/2 the first time it sees
an h2 ALPN, and the resulting `http2.Transport` runs with
**`ReadIdleTimeout: 0` (no PINGs)** by default. Long-idle h2 connections
sit forever; when NAT, an L4 LB, or the gitea server silently drops the
conn, neither side notices. The next `FetchTask` re-uses the dead conn,
goes out, never gets a response, and blocks on `crypto/tls.Conn.Read`
until the OS TCP keepalive fires (default 2 hours on Linux). During that
window the poller is wedged — `/healthz` lies that it's fine because
`lastPollNs` is stored *before* the RPC, the kubelet doesn't restart the
pod, and Gitea HTTP logs show zero `FetchTask` requests.

This is the actual root cause of bug 007. The round-1 fix (`commit
2e81477` — heartbeat + `/healthz` 503 if `lastPollAt` is stale) does not
catch this wedge mode because the heartbeat is updated per-tick, not
per-completed-RPC.

## Repro

- `pkg/server/client.go:84-101` — `newHTTPClient` returns
  `&http.Client{Transport: &http.Transport{Proxy: ProxyFromEnvironment, ...}}`.
  No `ForceAttemptHTTP2`, no `IdleConnTimeout`, no `http2.ConfigureTransports`.
- Goroutine dump from a wedge (`bugs/007-goroutine-dump-2026-05-01-1545UTC.txt`,
  lines 138–148): single `http2clientConnReadLoop` parked in
  `crypto/tls.Conn.Read` waiting on a frame header. Zero
  `connect.(*Client).CallUnary` / `http.RoundTrip` frames anywhere.
  Goroutine 1 (`Poller.Run`) is in `select` at `pkg/server/poller.go:74`.
- Most recent reproduction: 2026-05-04 ~01:31 UTC after task 72 of the
  drawbar/gt eval. 95-minute wedge. `/healthz` returned `ok` throughout.
  Forced recovery via `kubectl delete pod`.

## Why the round-1 fix didn't catch it

Round 1 added `lastPollNs atomic.Int64` and a `/healthz` 503 if
`time.Since(LastPollAt) > FetchInterval × 10`. But `lastPollNs.Store(...)`
runs at `poller.go:92` — **before** the RPC. So even when `FetchTask`
has been blocked on a dead read for an hour, the heartbeat is "fresh"
and `/healthz` keeps returning 200.

## Fix (recommended)

`pkg/server/client.go` — set `http2.Transport` ping/timeout knobs on the
underlying transport. `golang.org/x/net/http2` is already a transitive
dep via connect-go; no new module required.

```go
import "golang.org/x/net/http2"

func newHTTPClient(endpoint string, insecure bool, timeout time.Duration) *http.Client {
    transport := &http.Transport{
        Proxy:                 http.ProxyFromEnvironment,
        ForceAttemptHTTP2:     true,
        IdleConnTimeout:       90 * time.Second,
        TLSHandshakeTimeout:   10 * time.Second,
        ResponseHeaderTimeout: 30 * time.Second,
        ExpectContinueTimeout: 1 * time.Second,
    }
    if strings.HasPrefix(endpoint, "https://") && insecure {
        transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
    }
    if h2, err := http2.ConfigureTransports(transport); err != nil {
        slog.Error("http2 configure failed", "error", err)
    } else {
        h2.ReadIdleTimeout = 15 * time.Second // PING when no frame seen for 15s
        h2.PingTimeout      = 10 * time.Second // tear conn if PING not ACKed in 10s
        h2.WriteByteTimeout = 30 * time.Second // catch half-open writes
    }
    return &http.Client{Transport: transport, Timeout: timeout}
}
```

### What this gets you

- After 15s of no inbound h2 frames, the readloop sends a PING.
- If the server doesn't ACK within 10s, the conn is closed *and the
  in-flight `RoundTrip` returns a real error*. The poller's existing
  error path runs, the next tick's FetchTask hits a fresh dial.
- `IdleConnTimeout: 90s` recycles long-idle conns even before they
  go bad — covers cases where PING traffic alone keeps a degraded
  conn alive but it's unhealthy in some other way (e.g. h2 stream-id
  exhaustion, server-side throttling).

This is what `act_runner`, grpc-go (via keepalive), client-go, and the
broader Go HTTP/2 ecosystem have settled on for any long-poll-shaped
client running through a cloud LB or NAT. The Go HTTP/2 issue tracker
has multiple writeups recommending these exact knobs.

### Why I'm filing this instead of the round-2 plan

The round-2 plan in `bugs/007-poller-dies-silently-no-observability.md`
was: track `lastSuccessfulFetchAt` (only stored on real responses),
detect staleness, then tear down + rebuild the connect-go HTTP client.
That works, but:

- It detects the wedge **after** the staleness window (default 30s,
  really `FetchInterval × 10`) — minutes wasted.
- The rebuild needs new concurrency state. Naively swapping the client
  pointer doesn't unblock the wedged read; you'd also have to
  `transport.CloseIdleConnections()` or set up cancellation on the
  in-flight call. Bookkeeping for a problem that the h2 layer already
  knows how to solve.
- `ReadIdleTimeout` causes the in-flight `RoundTrip` to return a
  retryable error, so the existing `slog.Error("fetch task failed")`
  path runs and the next tick retries. **No new state machine, no new
  goroutines, no new races.**

Ship `ReadIdleTimeout` first. If wedges still happen, then write the
rebuild logic.

## Belt-and-suspenders companion (do this too)

`pkg/server/poller.go:92` — move `lastPollNs.Store(...)` to *after* the
RPC returns (success or error). Currently the heartbeat says "the
goroutine reached line 92"; it should say "the goroutine completed an
RPC attempt." Also add `lastSuccessfulFetchNs atomic.Int64` stored only
on `nil` error or `connect.CodeDeadlineExceeded` (long-poll's "no
work" response is still a successful round trip). `/healthz` checks
`max(staleness from lastPoll with short threshold, staleness from
lastSuccessfulFetch with longer threshold)` — fast detect of "ticker
dead", slow detect of "transport dead with PINGs disabled or broken".

3 lines of refactor + 1 new atomic + a `/healthz` predicate change.
Would have caught today's wedge by itself even without the transport
fix. Worth doing as defense-in-depth on a control-plane process.

## Related

- Round-1 partial fix: `commit 2e81477` adds `LastPollAt` heartbeat
  and `/healthz` 503. Helps when the *ticker* is dead but not when the
  *connection* is. This bug is the real root cause.
- Bug 007's "Status" section already noted the round-2 plan but it
  was never implemented; that's still a viable backup if `ReadIdleTimeout`
  alone proves insufficient.
- Code review by superpowers:code-reviewer agent (2026-05-04) ranked
  this fix as #1 ROI in the codebase. Quote: "You're missing one line
  of code... `http2.Transport.ReadIdleTimeout` (and `PingTimeout`) on
  the underlying transport treats the cause and is the canonical Go
  fix for exactly this failure mode."
