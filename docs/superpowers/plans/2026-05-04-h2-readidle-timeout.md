# HTTP/2 ReadIdleTimeout + per-RPC heartbeat Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Stop the controller's poll loop from wedging for hours on a half-dead HTTP/2 connection by enabling h2 PINGs on the connect-go RPC client, plus add a per-RPC heartbeat so `/healthz` actually catches the wedge if PINGs are insufficient.

**Architecture:** Two independent fixes in two files. (1) `pkg/server/client.go::newHTTPClient` — set `ForceAttemptHTTP2`, sane stdlib `http.Transport` timeouts, and call `http2.ConfigureTransports` to enable `ReadIdleTimeout` / `PingTimeout` / `WriteByteTimeout`. After 15s of no inbound h2 frames a PING goes out; if not ACKed in 10s the conn is torn and the in-flight RPC returns a real error, so the existing fetch-error path retries on the next tick. (2) `pkg/server/poller.go` — move `lastPollNs.Store(...)` to *after* the RPC returns and add a separate `lastSuccessfulFetchNs` heartbeat stored only when the RPC produced a real response (success or `CodeDeadlineExceeded`); `/healthz` reports stale if either heartbeat is too old. Belt-and-suspenders: if h2 PINGs ever fail to detect a wedge, the per-RPC heartbeat will.

**Tech Stack:** Go stdlib `net/http`, `golang.org/x/net/http2` (already an indirect dep, will be promoted to direct), connect-go, `log/slog`, `testify`.

**Reference:** `bugs/010-h2-transport-no-readidle-timeout.md` is the spec.

---

## File Structure

Files modified:

- `pkg/server/client.go` — `newHTTPClient` gains `ForceAttemptHTTP2`, stdlib `http.Transport` timeouts, and an `http2.ConfigureTransports` call that sets `ReadIdleTimeout`, `PingTimeout`, `WriteByteTimeout`. The function still returns `*http.Client`, signature unchanged.
- `pkg/server/client_test.go` — extend existing `newHTTPClient` tests so they assert the new transport knobs are configured. No new test file.
- `pkg/server/poller.go` — `lastPollNs` is stored *after* the RPC returns; new `lastSuccessfulFetchNs atomic.Int64` is stored only when `err == nil` or `connect.CodeOf(err) == connect.CodeDeadlineExceeded`. New `LastSuccessfulFetchAt() time.Time` accessor mirrors `LastPollAt`.
- `pkg/server/poller_test.go` — extend `TestPoller_LastPollAt` invariant (still set after Run) and add `TestPoller_LastSuccessfulFetchAt` covering the success-path and error-path cases.
- `cmd/controller/main.go` — `healthzHandler` accepts a second `lastSuccessfulFetch` getter and a separate, longer staleness threshold for it. `/healthz` returns 503 if *either* heartbeat is too stale. `startHealthServer` is updated to pass both. `pollStalenessThreshold` is unchanged; a new `successFetchStalenessThreshold` is added (longer floor, scales with `FetchInterval`).
- `cmd/controller/main_test.go` — add tests covering: success-fetch fresh + poll fresh => 200; success-fetch stale + poll fresh => 503; both fresh => 200; both stale => 503; zero success-fetch (pre-first-success) => 200.
- `go.mod` — promote `golang.org/x/net` from indirect to direct (`go mod tidy` after the import is added).

No new files. No new packages.

---

## Task 1: Enable HTTP/2 ping/timeout knobs on the connect-go transport

**Files:**
- Modify: `pkg/server/client.go:84-101`
- Modify: `pkg/server/client_test.go:40-80`
- Modify: `go.mod` (via `go mod tidy`)

- [ ] **Step 1: Write failing tests for the new transport configuration**

Open `pkg/server/client_test.go` and replace the existing `TestNewHTTPClient_*` functions (lines ~40-80) with the expanded set below. Keep the file's existing imports and add `"golang.org/x/net/http2"`.

```go
func TestNewHTTPClient_Default(t *testing.T) {
	hc := newHTTPClient("http://localhost", false, 10*time.Second)
	require.NotNil(t, hc)
	assert.Equal(t, 10*time.Second, hc.Timeout)

	transport, ok := hc.Transport.(*http.Transport)
	require.True(t, ok)
	assert.Nil(t, transport.TLSClientConfig)

	// Stdlib transport timeouts should be set so a single dead conn cannot
	// wedge the client forever.
	assert.True(t, transport.ForceAttemptHTTP2, "ForceAttemptHTTP2 should be true so http2.ConfigureTransports applies")
	assert.Equal(t, 90*time.Second, transport.IdleConnTimeout)
	assert.Equal(t, 10*time.Second, transport.TLSHandshakeTimeout)
	assert.Equal(t, 30*time.Second, transport.ResponseHeaderTimeout)
	assert.Equal(t, 1*time.Second, transport.ExpectContinueTimeout)
}

func TestNewHTTPClient_HTTP2PingsConfigured(t *testing.T) {
	hc := newHTTPClient("https://localhost", false, 10*time.Second)
	transport, ok := hc.Transport.(*http.Transport)
	require.True(t, ok)

	// http2.ConfigureTransports stores the *http2.Transport on
	// transport.TLSNextProto["h2"] indirectly. We can't read that map
	// entry's settings from outside the http2 package, so instead verify
	// the configuration is idempotent: calling ConfigureTransports again
	// returns the SAME *http2.Transport pointer with our values still on it.
	h2, err := http2.ConfigureTransports(transport)
	require.NoError(t, err)
	require.NotNil(t, h2)
	assert.Equal(t, 15*time.Second, h2.ReadIdleTimeout, "ReadIdleTimeout should send a PING after 15s of no inbound frames")
	assert.Equal(t, 10*time.Second, h2.PingTimeout, "PingTimeout should tear the conn if PING not ACKed in 10s")
	assert.Equal(t, 30*time.Second, h2.WriteByteTimeout, "WriteByteTimeout should catch half-open writes")
}

func TestNewHTTPClient_InsecureHTTPS(t *testing.T) {
	hc := newHTTPClient("https://localhost", true, 10*time.Second)
	transport, ok := hc.Transport.(*http.Transport)
	require.True(t, ok)
	require.NotNil(t, transport.TLSClientConfig)
	assert.True(t, transport.TLSClientConfig.InsecureSkipVerify)
}

func TestNewHTTPClient_InsecureHTTP_NoTLS(t *testing.T) {
	// Insecure on HTTP should NOT set TLS config.
	hc := newHTTPClient("http://localhost", true, 10*time.Second)
	transport, ok := hc.Transport.(*http.Transport)
	require.True(t, ok)
	if transport.TLSClientConfig != nil {
		assert.False(t, transport.TLSClientConfig.InsecureSkipVerify)
	}
}

func TestNewHTTPClient_DefaultTimeout(t *testing.T) {
	c := NewClient("http://localhost", false, "", "", time.Second, 0)
	_ = c // Just verify no panic; timeout defaulting is in NewClient, not newHTTPClient.
}

func TestNewHTTPClient_TLSConfigType(t *testing.T) {
	hc := newHTTPClient("https://example.com", true, time.Second)
	transport := hc.Transport.(*http.Transport)
	var _ *tls.Config = transport.TLSClientConfig // type assertion
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `cd /Users/myers/p/drawbar && /Users/myers/.local/share/mise/installs/go/1.25.7/bin/go test ./pkg/server/ -run TestNewHTTPClient -v`

Expected: build error first because `golang.org/x/net/http2` is not yet imported by the test file *or* the production file. After running `go mod tidy` you should see test failures (not build errors) on `TestNewHTTPClient_Default` (`ForceAttemptHTTP2 should be true`) and `TestNewHTTPClient_HTTP2PingsConfigured` (`ReadIdleTimeout` is 0, not 15s). That's the desired pre-implementation state.

If `go mod tidy` complains, run it once to pull `golang.org/x/net` as a direct dep:

Run: `cd /Users/myers/p/drawbar && /Users/myers/.local/share/mise/installs/go/1.25.7/bin/go mod tidy`

- [ ] **Step 3: Implement the transport configuration**

Edit `pkg/server/client.go`. Replace the import block (lines 7-20) with:

```go
import (
	"context"
	"crypto/tls"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"code.gitea.io/actions-proto-go/ping/v1/pingv1connect"
	"code.gitea.io/actions-proto-go/runner/v1/runnerv1connect"
	"connectrpc.com/connect"
	gouuid "github.com/google/uuid"
	"golang.org/x/net/http2"
)
```

Replace `newHTTPClient` (lines 84-101) with:

```go
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
		slog.Warn("TLS certificate verification disabled — connections are vulnerable to MITM attacks",
			"endpoint", endpoint)
		transport.TLSClientConfig = &tls.Config{
			InsecureSkipVerify: true,
		}
	}

	// Enable HTTP/2 PINGs so a half-dead conn (NAT/LB drop, server crash without
	// FIN) is detected and torn within ~25s instead of waiting for OS TCP
	// keepalive (default 2h on Linux). See bugs/010.
	if h2, err := http2.ConfigureTransports(transport); err != nil {
		slog.Error("http2 configure failed", "error", err)
	} else {
		h2.ReadIdleTimeout = 15 * time.Second
		h2.PingTimeout = 10 * time.Second
		h2.WriteByteTimeout = 30 * time.Second
	}

	return &http.Client{
		Transport: transport,
		Timeout:   timeout,
	}
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `cd /Users/myers/p/drawbar && /Users/myers/.local/share/mise/installs/go/1.25.7/bin/go mod tidy && /Users/myers/.local/share/mise/installs/go/1.25.7/bin/go test ./pkg/server/ -run TestNewHTTPClient -v`

Expected: all six `TestNewHTTPClient_*` tests PASS.

- [ ] **Step 5: Run the full server package tests**

Run: `cd /Users/myers/p/drawbar && /Users/myers/.local/share/mise/installs/go/1.25.7/bin/go test ./pkg/server/...`

Expected: PASS. No regressions.

- [ ] **Step 6: Commit**

```bash
cd /Users/myers/p/drawbar
git add pkg/server/client.go pkg/server/client_test.go go.mod go.sum
git commit -m "$(cat <<'EOF'
server: enable http2 PINGs on connect-go transport

Set ReadIdleTimeout=15s, PingTimeout=10s, WriteByteTimeout=30s via
http2.ConfigureTransports so a half-dead h2 conn is torn within ~25s
instead of waiting hours for OS TCP keepalive. Also set sane stdlib
http.Transport timeouts (IdleConnTimeout, TLSHandshakeTimeout,
ResponseHeaderTimeout, ExpectContinueTimeout) and ForceAttemptHTTP2.

Fixes the root cause of bug 007 / addresses bug 010.
EOF
)"
```

---

## Task 2: Move per-RPC heartbeat to AFTER the call and add successful-fetch heartbeat

**Files:**
- Modify: `pkg/server/poller.go:30,91-92,104-118`
- Modify: `pkg/server/poller_test.go` (add new test, leave existing tests intact)

- [ ] **Step 1: Write failing tests for the new heartbeat behavior**

Open `pkg/server/poller_test.go`. Look at the existing `mockPollerClient` near the top of the file to understand its shape (it returns canned responses or errors via the `responses` / `errors` slices — read the file once if you're unsure). Append the following two tests at the end of the file:

```go
func TestPoller_LastSuccessfulFetchAt_SuccessUpdatesBoth(t *testing.T) {
	mock := &mockPollerClient{
		interval:  10 * time.Millisecond,
		responses: []*runnerv1.FetchTaskResponse{{}, {}, {}},
	}
	handler := func(_ context.Context, _ *runnerv1.Task) {}
	p := NewPoller(mock, handler, 1, time.Second, false, slog.Default())

	// Both heartbeats are zero before any poll.
	assert.True(t, p.LastPollAt().IsZero())
	assert.True(t, p.LastSuccessfulFetchAt().IsZero())

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	p.Run(ctx)

	// After at least one successful fetch both timestamps should be set.
	assert.False(t, p.LastPollAt().IsZero(), "LastPollAt should be set after a successful poll")
	assert.False(t, p.LastSuccessfulFetchAt().IsZero(), "LastSuccessfulFetchAt should be set after a successful poll")
}

func TestPoller_LastSuccessfulFetchAt_TransportErrorOnlyUpdatesPoll(t *testing.T) {
	// All FetchTask calls return a non-deadline error. lastPollNs should
	// advance (the goroutine is alive and attempting RPCs); however
	// lastSuccessfulFetchNs must NOT advance — that's what catches the wedge
	// if h2 PINGs ever fail to detect a dead conn.
	mock := &mockPollerClient{
		interval: 10 * time.Millisecond,
		errors:   []error{connect.NewError(connect.CodeUnavailable, errors.New("transport error"))},
		repeatLastError: true, // see note below
	}
	handler := func(_ context.Context, _ *runnerv1.Task) {}
	p := NewPoller(mock, handler, 1, time.Second, false, slog.Default())

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	p.Run(ctx)

	assert.False(t, p.LastPollAt().IsZero(), "poll heartbeat should advance even on errors")
	assert.True(t, p.LastSuccessfulFetchAt().IsZero(), "successful-fetch heartbeat must not advance on transport errors")
}

func TestPoller_LastSuccessfulFetchAt_DeadlineExceededCounts(t *testing.T) {
	// Long-poll's "no work" response is CodeDeadlineExceeded — that's still
	// a healthy round trip and must update lastSuccessfulFetchNs.
	mock := &mockPollerClient{
		interval:        10 * time.Millisecond,
		errors:          []error{connect.NewError(connect.CodeDeadlineExceeded, errors.New("no tasks"))},
		repeatLastError: true,
	}
	handler := func(_ context.Context, _ *runnerv1.Task) {}
	p := NewPoller(mock, handler, 1, time.Second, false, slog.Default())

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	p.Run(ctx)

	assert.False(t, p.LastPollAt().IsZero())
	assert.False(t, p.LastSuccessfulFetchAt().IsZero(), "DeadlineExceeded is a successful round trip; should update")
}
```

You will likely need to add `"errors"` to the test file's imports if it's not already present.

**Note about `repeatLastError`:** `mockPollerClient` in this file may or may not already support repeating its last error/response forever. Inspect the existing struct (look near the top of the file or in `client_test.go`/`fetchtask_idempotency_test.go` for its definition — it is shared in this package). If it doesn't have a "repeat" mode, add one:

```go
// In mockPollerClient struct:
repeatLastError    bool
repeatLastResponse bool

// In FetchTask:
if i := m.errIdx; i < len(m.errors) {
    m.errIdx++
    return nil, m.errors[i]
}
if m.repeatLastError && len(m.errors) > 0 {
    return nil, m.errors[len(m.errors)-1]
}
```

Mirror the same idea for responses if needed. Keep changes minimal — do not rewrite the mock.

- [ ] **Step 2: Run tests to verify they fail**

Run: `cd /Users/myers/p/drawbar && /Users/myers/.local/share/mise/installs/go/1.25.7/bin/go test ./pkg/server/ -run TestPoller_LastSuccessfulFetchAt -v`

Expected: build error — `p.LastSuccessfulFetchAt undefined`. That's the desired pre-implementation state.

- [ ] **Step 3: Implement the heartbeat changes in `poller.go`**

Edit `pkg/server/poller.go`.

Replace the `Poller` struct field block (lines 19-31). Add `lastSuccessfulFetchNs` next to `lastPollNs`:

```go
type Poller struct {
	client       PollerClient
	handler      TaskHandler
	fetchTimeout time.Duration
	capacity     int64
	ephemeral    bool
	log          *slog.Logger
	sem          chan struct{}
	wg           sync.WaitGroup
	backoff      time.Duration
	stopPoll     context.CancelFunc
	lastPollNs            atomic.Int64 // unix-nanos of the most recent FetchTask attempt to RETURN; 0 until first poll completes
	lastSuccessfulFetchNs atomic.Int64 // unix-nanos of the most recent FetchTask that produced a real response (success or DeadlineExceeded); 0 until first such response
}
```

Replace the body of `poll` (lines 91-130). The two changes are: (a) drop the `p.lastPollNs.Store(...)` at the very top; (b) at the end of the function — both error and success paths — store the heartbeats. Result:

```go
func (p *Poller) poll(ctx context.Context, tasksVersion *int64, requestKey *gouuid.UUID) {
	p.log.Debug("polling", "tasks_version", *tasksVersion)

	cleanup := p.client.SetRequestKey(*requestKey)
	defer cleanup()

	fetchCtx, cancel := context.WithTimeout(ctx, p.fetchTimeout)
	defer cancel()

	resp, err := p.client.FetchTask(fetchCtx, connect.NewRequest(&runnerv1.FetchTaskRequest{
		TasksVersion: *tasksVersion,
	}))

	// Heartbeat: lastPollNs records that the RPC RETURNED (success or error,
	// but not a context cancellation due to shutdown). If the RPC wedges on
	// a half-dead h2 conn, this heartbeat goes stale and /healthz reports it.
	if ctx.Err() == nil {
		now := time.Now().UnixNano()
		p.lastPollNs.Store(now)
		// lastSuccessfulFetchNs is stricter: only advances when the server
		// actually responded. CodeDeadlineExceeded is the long-poll "no work"
		// signal and counts as a successful round trip.
		if err == nil || connect.CodeOf(err) == connect.CodeDeadlineExceeded {
			p.lastSuccessfulFetchNs.Store(now)
		}
	}

	if err != nil {
		if ctx.Err() != nil {
			return // Context cancelled, shutting down.
		}
		if connect.CodeOf(err) == connect.CodeDeadlineExceeded {
			p.log.Debug("no tasks available", "error", err)
			p.backoff = 0
		} else {
			p.log.Error("fetch task failed", "error", err)
			p.increaseBackoff()
		}
		return
	}

	p.backoff = 0
	*requestKey = gouuid.New()
	*tasksVersion = resp.Msg.GetTasksVersion()

	if task := resp.Msg.GetTask(); task != nil && task.GetId() != 0 {
		p.log.Info("received task", "id", task.GetId())
		p.dispatchTask(ctx, task)
	}
}
```

Replace `LastPollAt` (lines 166-175) and add `LastSuccessfulFetchAt` next to it:

```go
// LastPollAt returns the wall-clock time of the most recent FetchTask call
// to RETURN (success or error). Returns the zero Time before the first
// completed RPC; callers should treat that as "never polled."
func (p *Poller) LastPollAt() time.Time {
	ns := p.lastPollNs.Load()
	if ns == 0 {
		return time.Time{}
	}
	return time.Unix(0, ns)
}

// LastSuccessfulFetchAt returns the wall-clock time of the most recent
// FetchTask that produced a real server response — either nil error or
// connect.CodeDeadlineExceeded (long-poll's "no work" signal). Returns the
// zero Time before the first such response. Used by /healthz to detect a
// transport that is alive at the syscall level but not actually talking
// to the server (h2 conn half-dead, server-side throttling, etc.).
func (p *Poller) LastSuccessfulFetchAt() time.Time {
	ns := p.lastSuccessfulFetchNs.Load()
	if ns == 0 {
		return time.Time{}
	}
	return time.Unix(0, ns)
}
```

- [ ] **Step 4: Run the new tests to verify they pass**

Run: `cd /Users/myers/p/drawbar && /Users/myers/.local/share/mise/installs/go/1.25.7/bin/go test ./pkg/server/ -run "TestPoller_LastPollAt|TestPoller_LastSuccessfulFetchAt" -v`

Expected: all four tests PASS. The pre-existing `TestPoller_LastPollAt` should still pass — it only asserts `LastPollAt` is set after `Run`, which is still true (just a tick later than before).

- [ ] **Step 5: Run the full server package tests**

Run: `cd /Users/myers/p/drawbar && /Users/myers/.local/share/mise/installs/go/1.25.7/bin/go test ./pkg/server/...`

Expected: PASS.

- [ ] **Step 6: Commit**

```bash
cd /Users/myers/p/drawbar
git add pkg/server/poller.go pkg/server/poller_test.go
git commit -m "$(cat <<'EOF'
server: store poll heartbeat AFTER FetchTask returns; add successful-fetch heartbeat

Round-1 fix stored lastPollNs before the RPC, so /healthz kept returning
200 even when FetchTask was wedged on a dead h2 read. Move the store to
after the call. Add lastSuccessfulFetchNs that only advances when the
server actually responded (nil err or CodeDeadlineExceeded — the long-poll
"no work" signal). /healthz consumer in cmd/controller wired up next.
EOF
)"
```

---

## Task 3: `/healthz` checks both heartbeats

**Files:**
- Modify: `cmd/controller/main.go:343-412`
- Modify: `cmd/controller/main_test.go` (extend healthz tests)

- [ ] **Step 1: Write failing tests for the two-heartbeat healthzHandler**

Edit `cmd/controller/main_test.go`. Replace the existing healthz tests (the block starting at `// --- healthzHandler ---` near line 283 through `TestPollStalenessThreshold`) with this expanded set. Note the new signature `healthzHandler(lastPoll, lastSuccessFetch, pollStaleness, successFetchStaleness, onWedge)`:

```go
// --- healthzHandler ---

func TestHealthzHandler_BothFresh(t *testing.T) {
	now := time.Now()
	handler := healthzHandler(
		func() time.Time { return now },
		func() time.Time { return now },
		30*time.Second, 5*time.Minute, nil,
	)
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	w := httptest.NewRecorder()
	handler(w, req)
	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "ok", w.Body.String())
}

func TestHealthzHandler_NoPollYet(t *testing.T) {
	// Both heartbeats zero = startup, before first poll. Always healthy.
	handler := healthzHandler(
		func() time.Time { return time.Time{} },
		func() time.Time { return time.Time{} },
		30*time.Second, 5*time.Minute, nil,
	)
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	w := httptest.NewRecorder()
	handler(w, req)
	assert.Equal(t, http.StatusOK, w.Code)
}

func TestHealthzHandler_StalePoll(t *testing.T) {
	// Poll heartbeat stale = ticker dead. 503.
	stale := time.Now().Add(-time.Hour)
	now := time.Now()
	handler := healthzHandler(
		func() time.Time { return stale },
		func() time.Time { return now },
		30*time.Second, 5*time.Minute, nil,
	)
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	w := httptest.NewRecorder()
	handler(w, req)
	assert.Equal(t, http.StatusServiceUnavailable, w.Code)
	assert.Contains(t, w.Body.String(), "poll loop stale")
}

func TestHealthzHandler_StaleSuccessfulFetch(t *testing.T) {
	// Poll heartbeat is fresh (goroutine is alive, RPCs are returning) but
	// no successful response in a long time = transport wedged. 503.
	now := time.Now()
	stale := time.Now().Add(-time.Hour)
	handler := healthzHandler(
		func() time.Time { return now },
		func() time.Time { return stale },
		30*time.Second, 5*time.Minute, nil,
	)
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	w := httptest.NewRecorder()
	handler(w, req)
	assert.Equal(t, http.StatusServiceUnavailable, w.Code)
	assert.Contains(t, w.Body.String(), "successful fetch stale")
}

func TestHealthzHandler_StaleSuccessfulFetch_ZeroIgnored(t *testing.T) {
	// Successful-fetch zero = no successful fetch yet (e.g. server still down
	// since startup). Treat as "starting up" — do NOT 503 on this alone. The
	// poll heartbeat handles ticker-dead; readyz handles registration.
	now := time.Now()
	handler := healthzHandler(
		func() time.Time { return now },
		func() time.Time { return time.Time{} },
		30*time.Second, 5*time.Minute, nil,
	)
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	w := httptest.NewRecorder()
	handler(w, req)
	assert.Equal(t, http.StatusOK, w.Code)
}

func TestHealthzHandler_OnWedgeFiresOnce(t *testing.T) {
	stale := time.Now().Add(-time.Hour)
	now := time.Now()
	var calls atomic.Int32
	handler := healthzHandler(
		func() time.Time { return stale },
		func() time.Time { return now },
		30*time.Second, 5*time.Minute,
		func(_, _ time.Duration) { calls.Add(1) },
	)
	for range 5 {
		req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
		w := httptest.NewRecorder()
		handler(w, req)
		assert.Equal(t, http.StatusServiceUnavailable, w.Code)
	}
	assert.Equal(t, int32(1), calls.Load(), "onWedge should fire exactly once across multiple stale probes")
}

func TestHealthzHandler_OnWedgeFiresForSuccessfulFetchStaleness(t *testing.T) {
	now := time.Now()
	stale := time.Now().Add(-time.Hour)
	var calls atomic.Int32
	handler := healthzHandler(
		func() time.Time { return now },
		func() time.Time { return stale },
		30*time.Second, 5*time.Minute,
		func(_, _ time.Duration) { calls.Add(1) },
	)
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	w := httptest.NewRecorder()
	handler(w, req)
	assert.Equal(t, http.StatusServiceUnavailable, w.Code)
	assert.Equal(t, int32(1), calls.Load())
}

func TestPollStalenessThreshold(t *testing.T) {
	assert.Equal(t, 30*time.Second, pollStalenessThreshold(2*time.Second))
	assert.Equal(t, 30*time.Second, pollStalenessThreshold(time.Second))
	assert.Equal(t, 100*time.Second, pollStalenessThreshold(10*time.Second))
	assert.Equal(t, 5*time.Minute, pollStalenessThreshold(30*time.Second))
}

func TestSuccessFetchStalenessThreshold(t *testing.T) {
	// Floor of 2 minutes — successful-fetch staleness is the slower detector;
	// it should tolerate a brief outage before tripping the probe.
	assert.Equal(t, 2*time.Minute, successFetchStalenessThreshold(2*time.Second))
	assert.Equal(t, 2*time.Minute, successFetchStalenessThreshold(10*time.Second))
	// 10x interval applies above the floor.
	assert.Equal(t, 5*time.Minute, successFetchStalenessThreshold(30*time.Second))
	assert.Equal(t, 10*time.Minute, successFetchStalenessThreshold(time.Minute))
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `cd /Users/myers/p/drawbar && /Users/myers/.local/share/mise/installs/go/1.25.7/bin/go test ./cmd/controller/ -run "TestHealthzHandler|TestPollStalenessThreshold|TestSuccessFetchStalenessThreshold" -v`

Expected: build error — `successFetchStalenessThreshold undefined` and the `healthzHandler` argument count mismatch. That's the desired pre-implementation state.

- [ ] **Step 3: Implement the new `healthzHandler` signature and `successFetchStalenessThreshold`**

Edit `cmd/controller/main.go`.

Replace `startHealthServer` (line 343) and the wiring at line 281 to pass the new accessor. First, change the call site at line 281 from:

```go
go startHealthServer(&registered, &activeJobs, int64(cfg.Runner.Capacity), poller.LastPollAt, pollStaleness)
```

to:

```go
pollStaleness := pollStalenessThreshold(cfg.Runner.FetchInterval)
successFetchStaleness := successFetchStalenessThreshold(cfg.Runner.FetchInterval)
go startHealthServer(
	&registered, &activeJobs, int64(cfg.Runner.Capacity),
	poller.LastPollAt, poller.LastSuccessfulFetchAt,
	pollStaleness, successFetchStaleness,
)
```

(remove the now-duplicate `pollStaleness :=` line above the goroutine if you moved it — keep only one declaration.)

Update the `slog.Info` "runner is online" log statement nearby to also include the new threshold (helpful operational signal):

```go
slog.Info("runner is online, polling for tasks",
	"job_namespace", deps.namespace,
	"poll_staleness_threshold", pollStaleness,
	"success_fetch_staleness_threshold", successFetchStaleness,
)
```

Replace `startHealthServer` with:

```go
func startHealthServer(
	registered *atomic.Bool,
	activeJobs *atomic.Int64,
	capacity int64,
	lastPoll func() time.Time,
	lastSuccessfulFetch func() time.Time,
	pollStaleness time.Duration,
	successFetchStaleness time.Duration,
) {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", healthzHandler(
		lastPoll, lastSuccessfulFetch,
		pollStaleness, successFetchStaleness,
		dumpGoroutinesToStderr,
	))
	mux.HandleFunc("/readyz", readyzHandler(registered))
	mux.HandleFunc("/metrics/active-jobs", metricsHandler(activeJobs, capacity))
	mux.Handle("/debug/pprof/", http.DefaultServeMux)
	slog.Info("health server listening", "port", 8081)
	if err := http.ListenAndServe(":8081", mux); err != nil {
		slog.Error("health server error", "error", err)
	}
}
```

Replace `healthzHandler` with:

```go
// healthzHandler returns OK while the poll loop is making progress and 503
// once either heartbeat is too stale:
//
//   - lastPoll catches "ticker dead" (the poll goroutine is not running). The
//     zero value is treated as "starting up" and is always OK.
//   - lastSuccessfulFetch catches "transport dead but goroutine alive" — RPCs
//     are returning errors but no real server response in a long time
//     (h2 conn half-dead, server-side throttling, ...). The zero value is
//     ignored on purpose: if the server has been down since the runner
//     started, /readyz handles that — /healthz should not page.
//
// onWedge fires exactly once across the lifetime of the handler the first
// time either condition trips, so callers can capture diagnostics (a goroutine
// dump) before the kubelet restarts the pod and destroys the evidence.
func healthzHandler(
	lastPoll func() time.Time,
	lastSuccessfulFetch func() time.Time,
	pollStaleness time.Duration,
	successFetchStaleness time.Duration,
	onWedge func(since, threshold time.Duration),
) http.HandlerFunc {
	var dumpOnce sync.Once
	return func(w http.ResponseWriter, _ *http.Request) {
		if t := lastPoll(); !t.IsZero() {
			if since := time.Since(t); since > pollStaleness {
				if onWedge != nil {
					dumpOnce.Do(func() { onWedge(since, pollStaleness) })
				}
				w.WriteHeader(http.StatusServiceUnavailable)
				fmt.Fprintf(w, "poll loop stale: last poll %s ago (threshold %s)", since, pollStaleness)
				return
			}
		}
		if t := lastSuccessfulFetch(); !t.IsZero() {
			if since := time.Since(t); since > successFetchStaleness {
				if onWedge != nil {
					dumpOnce.Do(func() { onWedge(since, successFetchStaleness) })
				}
				w.WriteHeader(http.StatusServiceUnavailable)
				fmt.Fprintf(w, "successful fetch stale: last successful fetch %s ago (threshold %s)", since, successFetchStaleness)
				return
			}
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	}
}
```

Add `successFetchStalenessThreshold` next to the existing `pollStalenessThreshold`:

```go
// successFetchStalenessThreshold returns how long the poller is allowed to
// go without a real server response before /healthz starts failing. Slower
// than pollStalenessThreshold because brief server outages are normal — we
// only want to restart the pod when the transport itself is wedged. At
// least 2 minutes; otherwise 10× the configured fetch interval.
func successFetchStalenessThreshold(fetchInterval time.Duration) time.Duration {
	const floor = 2 * time.Minute
	t := fetchInterval * 10
	if t < floor {
		return floor
	}
	return t
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `cd /Users/myers/p/drawbar && /Users/myers/.local/share/mise/installs/go/1.25.7/bin/go test ./cmd/controller/ -run "TestHealthzHandler|TestPollStalenessThreshold|TestSuccessFetchStalenessThreshold" -v`

Expected: all eight tests PASS.

- [ ] **Step 5: Run the full controller package tests**

Run: `cd /Users/myers/p/drawbar && /Users/myers/.local/share/mise/installs/go/1.25.7/bin/go test ./cmd/controller/...`

Expected: PASS.

- [ ] **Step 6: Commit**

```bash
cd /Users/myers/p/drawbar
git add cmd/controller/main.go cmd/controller/main_test.go
git commit -m "$(cat <<'EOF'
controller: /healthz also checks lastSuccessfulFetch heartbeat

Two staleness checks: pollStaleness (ticker dead) and
successFetchStaleness (transport dead but goroutine alive).
Catches the wedge mode where FetchTask returns errors quickly
(e.g. fast retries from a half-broken connection) but no real
server response makes it through.
EOF
)"
```

---

## Task 4: Whole-tree build, lint, and test

**Files:** none (verification only)

- [ ] **Step 1: Build both binaries**

Run: `cd /Users/myers/p/drawbar && make build`

Expected: builds `bin/controller` and `bin/entrypoint` with no errors.

- [ ] **Step 2: Run the full test suite**

Run: `cd /Users/myers/p/drawbar && make test`

Expected: PASS across all packages.

- [ ] **Step 3: Run the linter**

Run: `cd /Users/myers/p/drawbar && make lint`

Expected: no lint errors. If `golangci-lint` isn't installed, skip and note it.

- [ ] **Step 4: Verify `go.mod` looks right**

Run: `cd /Users/myers/p/drawbar && grep golang.org/x/net go.mod`

Expected: a single line `golang.org/x/net v0.47.0` (no `// indirect` comment) — it's a direct dep now that `pkg/server/client.go` imports `golang.org/x/net/http2`.

- [ ] **Step 5: Commit any leftover go.mod/go.sum churn**

If `go.mod` / `go.sum` weren't already committed in Task 1, commit them now:

```bash
cd /Users/myers/p/drawbar
git status
# If go.mod/go.sum are dirty:
git add go.mod go.sum
git commit -m "deps: promote golang.org/x/net to direct dependency"
```

If they're clean, skip the commit.

---

## Self-review notes

**Spec coverage:**
- Bug 010 "Fix (recommended)" → Task 1.
- Bug 010 "Belt-and-suspenders companion" (move heartbeat to after RPC, add `lastSuccessfulFetchNs`, `/healthz` checks both) → Tasks 2 + 3.
- Round-1 fix references in `commit 2e81477` (`LastPollAt`, `pollStalenessThreshold`) — preserved, not removed; the new check is additive.

**Things deliberately NOT in scope:**
- The "round-2 plan" from bug 007 (tear down + rebuild the connect-go HTTP client). Bug 010 explicitly says ship `ReadIdleTimeout` first; only do the rebuild logic if wedges still happen. So this plan does not implement it.
- Closing the bug doc / updating `BUGS.md` / writing a postmortem — out of scope for the plan; the user will do this once they've verified the fix in their cluster.

**Risks / things to watch:**
- `http2.ConfigureTransports` is idempotent and safe to call again; the test in Task 1 step 1 exploits that to read back the configured values. If the http2 package ever changes that behavior the test would need an alternative (e.g. wrapping the dialer). Documented in the test's comment.
- `mockPollerClient` shape may not exactly match what's described in Task 2 step 1; if the existing mock has a different repeat semantics, adapt the test setup rather than rewriting the mock.
