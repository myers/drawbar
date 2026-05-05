# Upstream `act_runner` recon — bugs/ vs `reference/runner/`

Date: 2026-05-05. Source: `reference/runner/` (gitea/gitea-runner).
Purpose: spot-check whether each filed bug's fix matches, beats, or
trails the upstream implementation.

## Coverage at a glance

| Bug | Area | Upstream has it? | Drawbar position |
|-----|------|------------------|-------------------|
| 001 | Actions cache PVC RWO | N/A (no shared PVC pattern) | ahead — HTTP-fetch design |
| 002 | Registration recovery | same problem, no recovery | tied / unfixed |
| 003 | Per-repo ZFS datasets | N/A (no k8s storage layer) | drawbar-specific roadmap |
| 007 | Silent poller wedge | no instrumentation at all | ahead (added heartbeat) |
| 008 | `drawbar/cache@v1` mount EBUSY | N/A (no magic cache action) | drawbar-specific |
| 009 | `~/`-relative cache paths | N/A (no magic cache action) | drawbar-specific |
| 010 | h2 ReadIdleTimeout | **not set** (`http.go:18`) | ahead |
| 011 | RWO PVC blocks rolling update | **same bug** in their k8s example | ahead |
| 012 | Poller cursor latches | upstream has the correct fix already | landed match |
| 013 | `/healthz` false positive on capacity wait | structurally avoided by their loop shape | drawbar regressed and re-fixed |

## Detail

### 001 — actions-cache PVC RWO blocks job-pod mount
Drawbar-specific. Upstream `act_runner` runs jobs in Docker containers
on the same host, not as separate K8s pods, so the "two pods, one PVC"
problem doesn't arise for them. Drawbar's HTTP-fetch design (cache
server serves `/actions/<dir>/tar`, no shared mount) is a clean
solution to a problem upstream simply doesn't have.

### 002 — runner registration recovery
Filed: drawbar can't self-recover when its credentials become invalid.
Upstream check: `internal/app/cmd/daemon.go:130-150` loads
`reg.Token` once, calls `Declare`, then enters the poll loop. There
is no re-registration path on `connect.CodeUnauthenticated`. Same
failure mode upstream — they just don't run on Kubernetes so an
operator usually notices via the systemd unit log.

**Fix space is open.** Nothing to copy from upstream.

### 003 — per-repo ZFS datasets
Drawbar-specific roadmap. Upstream has no analogue.

### 007 — silent poller wedge
Filed: poller stopped calling `FetchTask`, no observability signal.

Upstream check: `internal/pkg/metrics/server.go:21-24` —
`/healthz` is a hard-coded `200 OK` with zero internal state checks.
They have **no liveness logic at all**. Their poll loop has no
heartbeat, no `lastPollAt`, no staleness threshold.

Drawbar's round-1 (`LastPollAt`) and round-2 (`LastSuccessfulFetchAt`)
heartbeats are entirely net-new instrumentation. There's no upstream
reference to consult; the choice of where to place the `Store(...)`
calls and what threshold to use is all drawbar's own design.

### 008 / 009 — `drawbar/cache@v1` issues
Drawbar-specific magic action. Upstream uses `actions/cache@v4`
unmodified, served by their cache HTTP server (port 9300). They don't
have a "bind-mount the cache PVC over /workspace" feature because
they don't have a per-task PVC concept. Both bugs are about drawbar
emulating snapshot caching with K8s primitives — no upstream
reference applies.

### 010 — h2 ReadIdleTimeout
Filed: poller wedges on half-dead h2 conn, kubelet doesn't notice.

Upstream check: `internal/pkg/client/http.go:18-30` —
```go
transport := &http.Transport{
    MaxIdleConns:        10,
    MaxIdleConnsPerHost: 10,
    IdleConnTimeout:     90 * time.Second,
}
```
**No `ForceAttemptHTTP2`. No `http2.ConfigureTransports`. No PING
knobs.** Upstream is exposed to the same wedge mode, just hasn't
been hit hard enough to file it (probably because their typical
deployments are docker-compose alongside gitea, no LB/NAT in
between).

Drawbar's `pkg/server/client.go::buildTransport` is ahead of
upstream and worth proposing back.

### 011 — RWO PVC blocks rolling update
Filed: drawbar Deployment uses default rolling strategy; new pod
can't mount the RWO PVC the old pod still holds.

Upstream check: `examples/kubernetes/{dind-docker,rootless-docker}.yaml`
both ship:
- `accessModes: [ReadWriteOnce]`
- `kind: Deployment` with `strategy: {}` (default rolling)

**Same bug, ships in their examples.** Drawbar's chart fix
(`strategy.type: Recreate`) is ahead.

### 012 — poller cursor latches and misses queued tasks
Filed: drawbar latched cursor to gitea's `latestVersion` on every
response, wedging when two tasks queued at the same version.

Upstream check: `internal/app/poll/poller.go:231-285` —
```go
if resp.Msg.TasksVersion > v {
    p.tasksVersion.CompareAndSwap(v, resp.Msg.TasksVersion)
}
...
// got a task, set `tasksVersion` to zero to force query db in next request.
p.tasksVersion.CompareAndSwap(resp.Msg.TasksVersion, 0)
```
Upstream has the correct pattern with an explicit comment about why.
Drawbar matched it in commit `c82b373`. The bug doc's Option 2 was
wrong; reading the OG code corrected the fix.

### 013 — `/healthz` false positive during capacity wait
Filed: poller blocks at `dispatchTask`'s semaphore acquire while a
task drains; `lastPollNs` doesn't advance; `/healthz` 503s; kubelet
restarts mid-task.

Upstream check: `internal/app/poll/poller.go:79-113` — semaphore
acquire happens **at the top of the loop**, *before* `fetchTask`:
```go
for {
    select {
    case sem <- struct{}{}:           // ← acquire first
    case <-p.pollingCtx.Done():
        return
    }
    task, ok := p.fetchTask(...)      // ← then fetch
    ...
}
```
Drawbar's poller acquires the semaphore inside `dispatchTask`,
*after* `FetchTask` returns. That inversion is the structural cause
of bug 013 — when capacity is full, drawbar's `poll()` blocks
holding `lastPollNs` stale, while upstream's loop just blocks at the
semaphore acquire and never enters `fetchTask` at all.

**This is the most actionable upstream lesson uncovered.** Switching
to upstream's loop shape would make bug 013 unrepresentable instead
of just shortening its window. Worth filing as a follow-up to bug 013.

Bonus: upstream's `Shutdown` (`poll/poller.go:133-160`) uses two
separate contexts — `pollingCtx` (stop accepting work) and `jobsCtx`
(kill in-flight tasks) — so graceful shutdown can drain without
killing in-flight jobs until a hard timeout. Drawbar's `Drain` shares
one context; on kubelet SIGTERM, in-flight jobs die immediately.
Adopting the two-context pattern would also help bug 013's
"kubelet kills mid-task" behaviour.

## Suggested follow-ups

1. **Re-shape the poll loop to match upstream** (acquire-before-fetch
   + two shutdown contexts). Closes bug 013 structurally; improves
   graceful drain. Larger diff than the heartbeat tweak we already
   landed.

2. **Propose `ReadIdleTimeout` upstream.** Their HTTP client is
   exposed to the same wedge; we already wrote the code.

3. **Bug 002 stays open.** Upstream isn't a reference; designs are
   on us. Likely directions: surface auth state via `/readyz`,
   document the recovery dance, or implement re-registration
   on `Unauthenticated`.
