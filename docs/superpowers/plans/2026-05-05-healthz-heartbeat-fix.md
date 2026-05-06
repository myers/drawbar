# `/healthz` Heartbeat Fix Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Eliminate two false-positive zones in `/healthz` (bugs 014 and 015) so kubelet stops restarting the controller during long handlers and during normal backoff sleeps.

**Architecture:** Add an `InBackoff()` predicate to the poller (atomic flag flipped around `waitBackoff`'s sleep). Update `/healthz` so the poll-staleness branch is gated by `inFlight==0 && !inBackoff` and the successful-fetch branch is gated by `inFlight < capacity`. No config or types changes; all edits live inside `cmd/controller/` and `pkg/server/`.

**Tech Stack:** Go (`log/slog`, `sync/atomic`, `net/http`, `testing`, `testify`).

**Spec:** [`docs/superpowers/specs/2026-05-05-healthz-heartbeat-fix-design.md`](../specs/2026-05-05-healthz-heartbeat-fix-design.md)

**Bugs:** [014](../../../bugs/014-healthz-successful-fetch-false-positive-during-long-task.md), [015](../../../bugs/015-healthz-poll-loop-wedge-during-long-backoff.md)

---

## File Map

- **Modify** `pkg/server/poller.go` — add `inBackoff` field, `InBackoff()` method, flip the flag inside `waitBackoff`.
- **Modify** `pkg/server/poller_test.go` — add `TestPoller_InBackoff_*` tests.
- **Modify** `cmd/controller/main.go` — extend `healthzHandler` and `startHealthServer` signatures with `inBackoff func() bool` and `capacity int64`; rewire `run()` to pass them; update the two staleness guards.
- **Modify** `cmd/controller/main_test.go` — update existing healthz tests to pass the new params; add new tests for backoff suppression and capacity-boundary suppression.

No new files. No config or Helm changes.

---

## Task 1: Add `InBackoff()` predicate to `Poller` (TDD)

**Files:**
- Modify: `pkg/server/poller.go`
- Test: `pkg/server/poller_test.go`

This task adds the predicate the controller needs. Follows the same shape as the existing `InFlight()`.

- [ ] **Step 1: Write the failing tests**

Append to `pkg/server/poller_test.go`:

```go
func TestPoller_InBackoff_FalseOnStartup(t *testing.T) {
	mock := &mockPollerClient{interval: 10 * time.Millisecond}
	p := NewPoller(mock, func(context.Context, *runnerv1.Task) {}, 1, time.Second, false, slog.Default())

	assert.False(t, p.InBackoff(), "InBackoff must be false before Run starts")
}

func TestPoller_InBackoff_FlagDuringWait(t *testing.T) {
	// First call returns an error so the poller enters waitBackoff;
	// subsequent calls block on a channel we control so the goroutine
	// stays inside the backoff sleep when we observe InBackoff().
	gate := make(chan struct{})
	mock := &errGatedClient{
		interval: 50 * time.Millisecond,
		errOnce:  errors.New("transport boom"),
		gate:     gate,
	}

	p := NewPoller(mock, func(context.Context, *runnerv1.Task) {}, 1, time.Second, false, slog.Default())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})
	go func() {
		p.Run(ctx)
		close(done)
	}()

	// The first FetchTask returns an error; waitBackoff is entered with
	// d == base interval (50ms). Wait for the flag to go true.
	deadline := time.Now().Add(2 * time.Second)
	for !p.InBackoff() {
		if time.Now().After(deadline) {
			t.Fatal("InBackoff never went true")
		}
		time.Sleep(2 * time.Millisecond)
	}

	cancel()
	close(gate) // unblock any pending FetchTask call
	<-done

	assert.False(t, p.InBackoff(), "InBackoff must be false after Run returns")
}

// errGatedClient is a PollerClient that returns errOnce on its first call,
// then blocks all subsequent calls on gate until the test closes it.
type errGatedClient struct {
	mu       sync.Mutex
	interval time.Duration
	errOnce  error
	gate     chan struct{}
	calls    int
}

func (c *errGatedClient) FetchTask(ctx context.Context, _ *connect.Request[runnerv1.FetchTaskRequest]) (*connect.Response[runnerv1.FetchTaskResponse], error) {
	c.mu.Lock()
	c.calls++
	n := c.calls
	c.mu.Unlock()
	if n == 1 {
		return nil, c.errOnce
	}
	select {
	case <-c.gate:
		return connect.NewResponse(&runnerv1.FetchTaskResponse{}), nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (c *errGatedClient) Endpoint() string             { return "" }
func (c *errGatedClient) FetchInterval() time.Duration { return c.interval }
func (c *errGatedClient) SetRequestKey(_ gouuid.UUID) func() {
	return func() {}
}
```

- [ ] **Step 2: Run tests to verify they fail**

```
cd /Users/myers/p/drawbar && /Users/myers/.local/share/mise/installs/go/1.25.7/bin/go test ./pkg/server/ -run TestPoller_InBackoff -count=1
```

Expected: compile error (`p.InBackoff undefined`).

- [ ] **Step 3: Add the field, method, and flag flip in `poller.go`**

In `pkg/server/poller.go`, add a field to the `Poller` struct (next to `inFlight`):

```go
inBackoff             atomic.Bool        // true while the poll loop is sleeping inside waitBackoff
```

After the existing `InFlight()` method, add:

```go
// InBackoff reports whether the poll loop is currently sleeping inside
// waitBackoff. /healthz uses this to suppress the poll-staleness 503
// during a normal backoff cycle (bug 015): the goroutine is alive and
// will resume on its own when the timer fires.
func (p *Poller) InBackoff() bool {
	return p.inBackoff.Load()
}
```

In `waitBackoff` (currently `pkg/server/poller.go:190`), set the flag at the very top and reset it on return. The function body becomes:

```go
func (p *Poller) waitBackoff(ctx context.Context, s *workerState) bool {
	p.inBackoff.Store(true)
	defer p.inBackoff.Store(false)

	base := p.client.FetchInterval()
	n := s.consecutiveErrors
	if s.consecutiveEmpty > n {
		n = s.consecutiveEmpty
	}
	d := base
	if n > 1 {
		shift := n - 1
		if shift > 5 {
			shift = 5
		}
		d = base * time.Duration(int64(1)<<shift)
		if d > backoffMax {
			d = backoffMax
		}
		p.log.Warn("backing off", "duration", d)
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-ctx.Done():
		return false
	}
}
```

- [ ] **Step 4: Run tests to verify they pass**

```
cd /Users/myers/p/drawbar && /Users/myers/.local/share/mise/installs/go/1.25.7/bin/go test ./pkg/server/ -run TestPoller_InBackoff -count=1 -v
```

Expected: `--- PASS: TestPoller_InBackoff_FalseOnStartup` and `--- PASS: TestPoller_InBackoff_FlagDuringWait`.

- [ ] **Step 5: Run the full poller test package to confirm nothing else regressed**

```
cd /Users/myers/p/drawbar && /Users/myers/.local/share/mise/installs/go/1.25.7/bin/go test ./pkg/server/... -count=1
```

Expected: `ok  github.com/myers/drawbar/pkg/server` with no failures.

- [ ] **Step 6: Commit**

```bash
git add pkg/server/poller.go pkg/server/poller_test.go
git commit -m "$(cat <<'EOF'
poller: add InBackoff() predicate for /healthz wedge detection

waitBackoff flips an atomic flag around its sleep so /healthz can tell
"goroutine wedged" from "goroutine sleeping in a normal backoff cycle"
(bug 015 setup).
EOF
)"
```

---

## Task 2: Update `healthzHandler` to take `inBackoff` + `capacity` (TDD)

**Files:**
- Modify: `cmd/controller/main.go`
- Test: `cmd/controller/main_test.go`

The signature change touches every existing healthz test and a few new ones. Do the test rewiring as part of the failing-test step so we don't have a compile-broken tree between commits.

- [ ] **Step 1: Write the failing tests**

In `cmd/controller/main_test.go`, replace the existing healthz tests (`TestHealthzHandler_BothFresh`, `TestHealthzHandler_NoPollYet`, `TestHealthzHandler_StalePoll`, `TestHealthzHandler_StaleSuccessfulFetch`, `TestHealthzHandler_StaleSuccessfulFetch_ZeroIgnored`, `TestHealthzHandler_OnWedgeFiresOnce`, `TestHealthzHandler_OnWedgeFiresForSuccessfulFetchStaleness`, `TestHealthzHandler_OnWedgePassesKind`, `TestHealthzHandler_BusySuppressesStaleness`) with the block below. The signature gains `inBackoff func() bool` and `capacity int64`; existing assertions are preserved by passing a constant-`false` `inBackoff` and a high enough `capacity` that the new guard never fires when the existing tests don't intend it to.

```go
// --- healthzHandler ---
//
// Signature reminder for these tests:
//
//   healthzHandler(lastPoll, lastSuccessfulFetch, inFlight, inBackoff,
//                  pollStaleness, successFetchStaleness, capacity, onWedge)
//
// The "always idle" defaults below: inFlight=0, inBackoff=false, capacity=1.
// Tests that exercise the new guards override these explicitly.

func neverInBackoff() bool { return false }

func TestHealthzHandler_BothFresh(t *testing.T) {
	now := time.Now()
	handler := healthzHandler(
		func() time.Time { return now },
		func() time.Time { return now },
		func() int64 { return 0 },
		neverInBackoff,
		30*time.Second, 5*time.Minute, 1, nil,
	)
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	w := httptest.NewRecorder()
	handler(w, req)
	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "ok", w.Body.String())
}

func TestHealthzHandler_NoPollYet(t *testing.T) {
	handler := healthzHandler(
		func() time.Time { return time.Time{} },
		func() time.Time { return time.Time{} },
		func() int64 { return 0 },
		neverInBackoff,
		30*time.Second, 5*time.Minute, 1, nil,
	)
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	w := httptest.NewRecorder()
	handler(w, req)
	assert.Equal(t, http.StatusOK, w.Code)
}

func TestHealthzHandler_StalePoll(t *testing.T) {
	stale := time.Now().Add(-time.Hour)
	now := time.Now()
	handler := healthzHandler(
		func() time.Time { return stale },
		func() time.Time { return now },
		func() int64 { return 0 },
		neverInBackoff,
		30*time.Second, 5*time.Minute, 1, nil,
	)
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	w := httptest.NewRecorder()
	handler(w, req)
	assert.Equal(t, http.StatusServiceUnavailable, w.Code)
	assert.Contains(t, w.Body.String(), "poll loop stale")
}

func TestHealthzHandler_StaleSuccessfulFetch(t *testing.T) {
	now := time.Now()
	stale := time.Now().Add(-time.Hour)
	handler := healthzHandler(
		func() time.Time { return now },
		func() time.Time { return stale },
		func() int64 { return 0 },
		neverInBackoff,
		30*time.Second, 5*time.Minute, 1, nil,
	)
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	w := httptest.NewRecorder()
	handler(w, req)
	assert.Equal(t, http.StatusServiceUnavailable, w.Code)
	assert.Contains(t, w.Body.String(), "successful fetch stale")
}

func TestHealthzHandler_StaleSuccessfulFetch_ZeroIgnored(t *testing.T) {
	now := time.Now()
	handler := healthzHandler(
		func() time.Time { return now },
		func() time.Time { return time.Time{} },
		func() int64 { return 0 },
		neverInBackoff,
		30*time.Second, 5*time.Minute, 1, nil,
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
		func() int64 { return 0 },
		neverInBackoff,
		30*time.Second, 5*time.Minute, 1,
		func(_ string, _, _ time.Duration) { calls.Add(1) },
	)
	for range 5 {
		req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
		w := httptest.NewRecorder()
		handler(w, req)
		assert.Equal(t, http.StatusServiceUnavailable, w.Code)
	}
	assert.Equal(t, int32(1), calls.Load())
}

func TestHealthzHandler_OnWedgeFiresForSuccessfulFetchStaleness(t *testing.T) {
	now := time.Now()
	stale := time.Now().Add(-time.Hour)
	var calls atomic.Int32
	handler := healthzHandler(
		func() time.Time { return now },
		func() time.Time { return stale },
		func() int64 { return 0 },
		neverInBackoff,
		30*time.Second, 5*time.Minute, 1,
		func(_ string, _, _ time.Duration) { calls.Add(1) },
	)
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	w := httptest.NewRecorder()
	handler(w, req)
	assert.Equal(t, http.StatusServiceUnavailable, w.Code)
	assert.Equal(t, int32(1), calls.Load())
}

func TestHealthzHandler_OnWedgePassesKind(t *testing.T) {
	t.Run("poll", func(t *testing.T) {
		stale := time.Now().Add(-time.Hour)
		now := time.Now()
		var gotKind string
		handler := healthzHandler(
			func() time.Time { return stale },
			func() time.Time { return now },
			func() int64 { return 0 },
			neverInBackoff,
			30*time.Second, 5*time.Minute, 1,
			func(kind string, _, _ time.Duration) { gotKind = kind },
		)
		req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
		w := httptest.NewRecorder()
		handler(w, req)
		assert.Equal(t, "poll loop", gotKind)
	})

	t.Run("successful fetch", func(t *testing.T) {
		now := time.Now()
		stale := time.Now().Add(-time.Hour)
		var gotKind string
		handler := healthzHandler(
			func() time.Time { return now },
			func() time.Time { return stale },
			func() int64 { return 0 },
			neverInBackoff,
			30*time.Second, 5*time.Minute, 1,
			func(kind string, _, _ time.Duration) { gotKind = kind },
		)
		req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
		w := httptest.NewRecorder()
		handler(w, req)
		assert.Equal(t, "successful fetch", gotKind)
	})
}

// TestHealthzHandler_BusySuppressesStaleness covers bug 013's existing
// behavior: while a handler is in flight, the poll-staleness 503 is
// suppressed. With the bug 014 fix layered on, the same in-flight state
// also satisfies the inFlight==capacity successful-fetch suppression at
// capacity 1, so this test runs against capacity=1 and confirms BOTH
// branches stay 200 while busy and the poll-stale branch trips when idle.
func TestHealthzHandler_BusySuppressesStaleness(t *testing.T) {
	pollStale := 50 * time.Millisecond
	succStale := time.Hour
	pollT := time.Now().Add(-time.Hour)
	var inFlight atomic.Int64

	var wedgeKind string
	onWedge := func(kind string, _, _ time.Duration) { wedgeKind = kind }

	h := healthzHandler(
		func() time.Time { return pollT },
		func() time.Time { return time.Now() },
		inFlight.Load,
		neverInBackoff,
		pollStale, succStale, 1,
		onWedge,
	)

	inFlight.Store(1)
	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("busy poll-stale: want 200, got %d (%q)", rec.Code, rec.Body.String())
	}
	if wedgeKind != "" {
		t.Fatalf("busy: onWedge must not fire, got %q", wedgeKind)
	}

	inFlight.Store(0)
	rec = httptest.NewRecorder()
	h(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("idle poll-stale: want 503, got %d (%q)", rec.Code, rec.Body.String())
	}
	if wedgeKind != "poll loop" {
		t.Fatalf("idle: want wedgeKind=poll loop, got %q", wedgeKind)
	}
}

// --- New tests for bugs 014 and 015 ---

func TestHealthzHandler_BackoffSuppressesPollStaleness(t *testing.T) {
	stale := time.Now().Add(-time.Hour)
	now := time.Now()
	var inBackoff atomic.Bool
	inBackoff.Store(true)

	var wedgeKind string
	h := healthzHandler(
		func() time.Time { return stale },
		func() time.Time { return now },
		func() int64 { return 0 },
		inBackoff.Load,
		30*time.Second, 5*time.Minute, 1,
		func(kind string, _, _ time.Duration) { wedgeKind = kind },
	)
	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	assert.Equal(t, http.StatusOK, rec.Code, "in-backoff must suppress poll-stale 503")
	assert.Equal(t, "", wedgeKind, "onWedge must not fire while in backoff")
}

func TestHealthzHandler_BackoffEndsExposesStaleness(t *testing.T) {
	stale := time.Now().Add(-time.Hour)
	now := time.Now()
	var inBackoff atomic.Bool

	var wedgeKind string
	h := healthzHandler(
		func() time.Time { return stale },
		func() time.Time { return now },
		func() int64 { return 0 },
		inBackoff.Load,
		30*time.Second, 5*time.Minute, 1,
		func(kind string, _, _ time.Duration) { wedgeKind = kind },
	)

	inBackoff.Store(true)
	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	assert.Equal(t, http.StatusOK, rec.Code)

	inBackoff.Store(false)
	rec = httptest.NewRecorder()
	h(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
	assert.Equal(t, "poll loop", wedgeKind)
}

func TestHealthzHandler_AtCapacitySuppressesSuccessfulFetch(t *testing.T) {
	cases := []struct {
		name     string
		capacity int64
	}{
		{"cap1", 1},
		{"cap2", 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Now()
			stale := time.Now().Add(-time.Hour)

			var wedgeKind string
			h := healthzHandler(
				func() time.Time { return now },
				func() time.Time { return stale },
				func() int64 { return tc.capacity }, // inFlight == capacity
				neverInBackoff,
				30*time.Second, 5*time.Minute, tc.capacity,
				func(kind string, _, _ time.Duration) { wedgeKind = kind },
			)
			rec := httptest.NewRecorder()
			h(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
			assert.Equal(t, http.StatusOK, rec.Code, "at-capacity must suppress successful-fetch 503")
			assert.Equal(t, "", wedgeKind)
		})
	}
}

func TestHealthzHandler_BelowCapacityExposesSuccessfulFetch(t *testing.T) {
	now := time.Now()
	stale := time.Now().Add(-time.Hour)

	var wedgeKind string
	h := healthzHandler(
		func() time.Time { return now },
		func() time.Time { return stale },
		func() int64 { return 1 }, // inFlight < capacity (capacity 2)
		neverInBackoff,
		30*time.Second, 5*time.Minute, 2,
		func(kind string, _, _ time.Duration) { wedgeKind = kind },
	)
	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
	assert.Equal(t, "successful fetch", wedgeKind)
}

func TestHealthzHandler_BothBranchesStale_PollLoopWins(t *testing.T) {
	// inFlight=0, inBackoff=false, both heartbeats stale. Poll-loop branch
	// is evaluated first and wins (matches existing dumpOnce semantics).
	stale := time.Now().Add(-time.Hour)
	var wedgeKind string
	h := healthzHandler(
		func() time.Time { return stale },
		func() time.Time { return stale },
		func() int64 { return 0 },
		neverInBackoff,
		30*time.Second, 5*time.Minute, 1,
		func(kind string, _, _ time.Duration) { wedgeKind = kind },
	)
	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
	assert.Equal(t, "poll loop", wedgeKind)
}
```

- [ ] **Step 2: Run tests to verify they fail**

```
cd /Users/myers/p/drawbar && /Users/myers/.local/share/mise/installs/go/1.25.7/bin/go test ./cmd/controller/ -run TestHealthzHandler -count=1
```

Expected: compile error (`too few arguments in call to healthzHandler` / `undefined: neverInBackoff` is not — that's defined inline).

- [ ] **Step 3: Update `healthzHandler` and `startHealthServer` signatures**

In `cmd/controller/main.go`, edit the `startHealthServer` function to thread the new params through:

```go
func startHealthServer(
	registered *atomic.Bool,
	activeJobs *atomic.Int64,
	capacity int64,
	lastPoll func() time.Time,
	lastSuccessfulFetch func() time.Time,
	inFlight func() int64,
	inBackoff func() bool,
	pollStaleness time.Duration,
	successFetchStaleness time.Duration,
) {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", healthzHandler(
		lastPoll, lastSuccessfulFetch,
		inFlight, inBackoff,
		pollStaleness, successFetchStaleness, capacity,
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

Replace the `healthzHandler` doc comment and signature block with:

```go
// healthzHandler returns OK while the poll loop is making progress and 503
// once either heartbeat is too stale:
//
//   - lastPoll catches "ticker dead" (the poll goroutine is not running). The
//     zero value is treated as "starting up" and is always OK. The check is
//     suppressed while inFlight() > 0 (the loop is legitimately blocked on
//     capacity acquisition while a handler runs — bug 013) or while
//     inBackoff() is true (the loop is sleeping inside waitBackoff and will
//     resume when the timer fires — bug 015).
//   - lastSuccessfulFetch catches "transport dead but goroutine alive" — RPCs
//     are returning errors but no real server response in a long time
//     (h2 conn half-dead, server-side throttling, ...). The zero value is
//     ignored on purpose: if the server has been down since the runner
//     started, /readyz handles that — /healthz should not page. The check is
//     suppressed when inFlight() == capacity: every slot is held, so the
//     poller is structurally unable to issue a new FetchTask and
//     lastSuccessfulFetch cannot advance through no fault of the transport
//     (bug 014). Below capacity (one slot free) the check is live again.
//
// onWedge fires exactly once across the lifetime of the handler the first
// time either condition trips, so callers can capture diagnostics (a goroutine
// dump) before the kubelet restarts the pod and destroys the evidence.
// The kind parameter identifies which check tripped ("poll loop" or
// "successful fetch").
func healthzHandler(
	lastPoll func() time.Time,
	lastSuccessfulFetch func() time.Time,
	inFlight func() int64,
	inBackoff func() bool,
	pollStaleness time.Duration,
	successFetchStaleness time.Duration,
	capacity int64,
	onWedge func(kind string, since, threshold time.Duration),
) http.HandlerFunc {
	var dumpOnce sync.Once
	return func(w http.ResponseWriter, _ *http.Request) {
		// Poll-staleness: only meaningful when the loop is neither holding a
		// slot for an in-flight handler nor sleeping inside waitBackoff.
		if inFlight() == 0 && !inBackoff() {
			if t := lastPoll(); !t.IsZero() {
				if since := time.Since(t); since > pollStaleness {
					if onWedge != nil {
						dumpOnce.Do(func() { onWedge("poll loop", since, pollStaleness) })
					}
					w.WriteHeader(http.StatusServiceUnavailable)
					fmt.Fprintf(w, "poll loop stale: last poll %s ago (threshold %s)", since, pollStaleness)
					return
				}
			}
		}
		// Successful-fetch: only meaningful when at least one slot is free for
		// the poller to actually attempt a new FetchTask. At inFlight ==
		// capacity, the loop is blocked on the semaphore and the heartbeat
		// cannot advance through any path that "transport health" would help.
		if inFlight() < capacity {
			if t := lastSuccessfulFetch(); !t.IsZero() {
				if since := time.Since(t); since > successFetchStaleness {
					if onWedge != nil {
						dumpOnce.Do(func() { onWedge("successful fetch", since, successFetchStaleness) })
					}
					w.WriteHeader(http.StatusServiceUnavailable)
					fmt.Fprintf(w, "successful fetch stale: last successful fetch %s ago (threshold %s)", since, successFetchStaleness)
					return
				}
			}
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	}
}
```

- [ ] **Step 4: Update the `run()` call site**

In `cmd/controller/main.go`, find the `go startHealthServer(...)` call (currently around line 282) and update it to pass the two new arguments:

```go
go startHealthServer(
	&registered, &activeJobs, int64(cfg.Runner.Capacity),
	poller.LastPollAt, poller.LastSuccessfulFetchAt,
	poller.InFlight, poller.InBackoff,
	pollStaleness, successFetchStaleness,
)
```

- [ ] **Step 5: Run tests to verify they pass**

```
cd /Users/myers/p/drawbar && /Users/myers/.local/share/mise/installs/go/1.25.7/bin/go test ./cmd/controller/ -run TestHealthzHandler -count=1 -v
```

Expected: all `TestHealthzHandler_*` cases PASS, including the new `TestHealthzHandler_BackoffSuppressesPollStaleness`, `TestHealthzHandler_BackoffEndsExposesStaleness`, `TestHealthzHandler_AtCapacitySuppressesSuccessfulFetch/cap1`, `TestHealthzHandler_AtCapacitySuppressesSuccessfulFetch/cap2`, `TestHealthzHandler_BelowCapacityExposesSuccessfulFetch`, `TestHealthzHandler_BothBranchesStale_PollLoopWins`.

- [ ] **Step 6: Run the full controller test package**

```
cd /Users/myers/p/drawbar && /Users/myers/.local/share/mise/installs/go/1.25.7/bin/go test ./cmd/controller/... -count=1
```

Expected: `ok  github.com/myers/drawbar/cmd/controller` with no failures.

- [ ] **Step 7: Run the full repo test suite**

```
cd /Users/myers/p/drawbar && /Users/myers/.local/share/mise/installs/go/1.25.7/bin/go test ./... -count=1
```

Expected: all packages PASS.

- [ ] **Step 8: Run the linter**

```
cd /Users/myers/p/drawbar && make lint
```

Expected: no issues. (If `make lint` is not available locally, run `go vet ./...` as a fallback.)

- [ ] **Step 9: Build both binaries**

```
cd /Users/myers/p/drawbar && make build
```

Expected: `bin/controller` and `bin/entrypoint` produced with no errors.

- [ ] **Step 10: Commit**

```bash
git add cmd/controller/main.go cmd/controller/main_test.go
git commit -m "$(cat <<'EOF'
controller: gate /healthz heartbeats on inBackoff and capacity

Closes bugs 014 and 015. The poll-staleness 503 is now suppressed when
the loop is sleeping in waitBackoff, and the successful-fetch 503 is
suppressed when every slot is held — both were structural false
positives that caused kubelet to restart the controller mid-work.
EOF
)"
```

---

## Task 3: Manual smoke test on the dev cluster

**Files:** none. Verification only.

This task is the spec's "Manual smoke test" requirement. Cannot be automated cheaply.

- [ ] **Step 1: Rebuild and redeploy the dev runner**

```
cd /Users/myers/p/drawbar && ./hack/dev-env.sh rebuild
```

Expected: image rebuilt and runner pod redeployed; `kubectl -n gitea get pod` shows the new drawbar pod `Running`, `1/1 Ready`.

- [ ] **Step 2: Confirm `/healthz` is healthy at startup**

In one terminal:

```
kubectl -n gitea port-forward deploy/drawbar 8081:8081
```

In another:

```
curl -i localhost:8081/healthz
```

Expected: `HTTP/1.1 200 OK`, body `ok`.

- [ ] **Step 3: Trigger the bug 014 repro**

Push to `gt.monoloco.net/chaos-inc/bevy_xr_nitro` to fire the `test.yaml` workflow (cargo test --lib + bin/visual-test, ~13 min). While the task runs:

```
while sleep 30; do date; curl -s -o /dev/null -w "%{http_code}\n" localhost:8081/healthz; done
```

Expected: every probe returns `200`. No `SUCCESSFUL FETCH WEDGED` lines in `kubectl -n gitea logs deploy/drawbar`.

- [ ] **Step 4: Confirm the runner finished the task naturally**

Expected: gitea-side run completes (success or workflow-defined failure — either way, the runner reports a final status rather than being SIGTERMed). Drawbar pod has *not* restarted (`kubectl -n gitea get pod -l app.kubernetes.io/name=drawbar` shows `RESTARTS 0`).

- [ ] **Step 5: Trigger the bug 015 repro (optional but recommended)**

Force sustained errors by stopping gitea briefly:

```
kubectl -n gitea scale deploy gitea --replicas=0
```

Wait ~3 minutes (long enough for the poller to backoff several times and reach the 60s cap), polling `/healthz`:

```
while sleep 15; do date; curl -s -o /dev/null -w "%{http_code}\n" localhost:8081/healthz; done
```

Expected: every probe during the backoff sleeps returns `200`. After ~2 minutes of sustained errors, `lastSuccessfulFetch` legitimately ages past `successFetchStaleness` and `/healthz` may return `503`. That is a true positive (transport really is not getting through) — a real wedge would behave the same way and a kubelet restart at that point is appropriate.

Restore gitea:

```
kubectl -n gitea scale deploy gitea --replicas=1
```

Expected: once gitea is back, `FetchTask` succeeds, both heartbeats reset, `/healthz` returns to `200`.

- [ ] **Step 6: Mark the bugs closed**

Update the `**Status:**` line at the top of both bug docs to reflect the fix landed.

For `bugs/014-healthz-successful-fetch-false-positive-during-long-task.md`, change:
```
**Status: filed.** Surfaced 2026-05-05 ...
```
to:
```
**Status: fixed.** Closed by the heartbeat predicate fix
(`docs/superpowers/specs/2026-05-05-healthz-heartbeat-fix-design.md`).
Surfaced 2026-05-05 ...
```

For `bugs/015-healthz-poll-loop-wedge-during-long-backoff.md`, do the same — change the leading `**Status: filed.**` line analogously.

- [ ] **Step 7: Commit**

```bash
git add bugs/014-healthz-successful-fetch-false-positive-during-long-task.md bugs/015-healthz-poll-loop-wedge-during-long-backoff.md
git commit -m "docs: mark bugs 014 and 015 fixed"
```

---

## Self-review notes

- **Spec coverage.** Every section of `2026-05-05-healthz-heartbeat-fix-design.md` is mapped to a task: the `Poller.InBackoff()` predicate (Task 1), the two new healthz guards (Task 2), the test plan (Task 1 + Task 2), and the manual smoke test (Task 3). No spec requirement is left without a task.
- **Type consistency.** `InBackoff()` is the method name everywhere (poller, healthz signature, `run()` wiring, tests). `inBackoff` is the field name and the test-side function variable. `capacity int64` matches the existing `int64(cfg.Runner.Capacity)` cast already used by `startHealthServer`.
- **Placeholder scan.** No TBDs, no "implement later," every code block is concrete. Each test asserts a specific status code or wedge kind, not a vague "verify behavior."
