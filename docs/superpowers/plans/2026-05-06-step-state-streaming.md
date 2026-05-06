# Step State Streaming Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace the 500 ms `cat /shim/state.jsonl` poll loop with a long-lived `entrypoint tail` exec plus a post-exit one-shot drain so per-step `started_at`, `completed_at`, and `conclusion` reach the forge correctly.

**Architecture:** Add a `tail` subcommand to the entrypoint binary (`entrypoint tail [--once] [--skip N] <path>`). On the controller side, swap `PodExecutor.Exec` (one-shot string) for `PodExecutor.ExecStream` (long-lived `io.ReadCloser`), replace `pollStateFileWith` with a streaming `streamStateFileWith` that reconnects via `--skip lastOffset`, and add a `drainStateFile` call after log EOF but before `Reporter.Close`.

**Tech Stack:** Go, `bufio.Reader`, `os/exec`, k8s `client-go` (`remotecommand.NewSPDYExecutor`), `code.gitea.io/actions-proto-go`, `connectrpc.com/connect`, `testify`.

**Spec:** [`docs/superpowers/specs/2026-05-06-step-state-streaming-design.md`](../specs/2026-05-06-step-state-streaming-design.md)

**Bug:** [`bugs/016-step-reporter-misattributes-times-and-conclusions.md`](../../../bugs/016-step-reporter-misattributes-times-and-conclusions.md)

---

## File Map

**Entrypoint (in-pod runner):**
- Create: `cmd/entrypoint/tail.go` — `tail` subcommand implementation.
- Create: `cmd/entrypoint/tail_test.go` — unit tests for the tailer.
- Modify: `cmd/entrypoint/main.go` — wire up the new `tail` case in `main`'s arg dispatch.

**Controller-side k8s glue:**
- Modify: `pkg/k8s/watcher.go` — change `PodExecutor` interface to streaming, replace `pollStateFileWith` with `streamStateFileWith`, add `drainStateFile`, rewrite the end-of-job tail of `watchJobWith`.
- Modify: `pkg/k8s/watcher_test.go` — update `mockPodExecutor` to the new streaming interface, add streaming/reconnect/drain tests, fix call sites of any tests that used the old string-based mock.

**Note:** `pkg/types/step.go`, `pkg/reporter/*`, `cmd/controller/*` are intentionally untouched. Confirmed in spec: the `PodExecutor` interface change is internal to `pkg/k8s` because `WatchConfig.Executor` is left at the zero value by the controller.

---

## Build commands

The Go toolchain is not on PATH in non-interactive shells; prepend it. Use these forms throughout:

- Build: `PATH=/Users/myers/.local/share/mise/installs/go/1.25.7/bin:$PATH make build`
- Test single package: `PATH=/Users/myers/.local/share/mise/installs/go/1.25.7/bin:$PATH go test ./pkg/k8s/...`
- Test single test: `PATH=/Users/myers/.local/share/mise/installs/go/1.25.7/bin:$PATH go test -run '^TestName$' ./pkg/k8s/`
- All tests: `PATH=/Users/myers/.local/share/mise/installs/go/1.25.7/bin:$PATH make test`
- Lint: `PATH=/Users/myers/.local/share/mise/installs/go/1.25.7/bin:$PATH make lint`

---

### Task 1: Add `tail` subcommand skeleton

**Files:**
- Create: `cmd/entrypoint/tail.go`
- Modify: `cmd/entrypoint/main.go` (the `switch os.Args[1]` block at line 28)

**Goal:** Wire the new subcommand into the entrypoint binary so it parses flags (`--once`, `--skip N`) and prints them to stderr, no actual tail logic yet. We start with the dispatch and flag-parsing because the tests in subsequent tasks invoke the function directly and need its signature locked in.

- [ ] **Step 1: Write the failing test**

Create `cmd/entrypoint/tail_test.go`:

```go
package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func TestParseTailArgs_Defaults(t *testing.T) {
	args, err := parseTailArgs([]string{"/some/path"})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if args.path != "/some/path" {
		t.Errorf("path = %q, want /some/path", args.path)
	}
	if args.once {
		t.Errorf("once = true, want false")
	}
	if args.skip != 0 {
		t.Errorf("skip = %d, want 0", args.skip)
	}
}

func TestParseTailArgs_Once(t *testing.T) {
	args, err := parseTailArgs([]string{"--once", "/p"})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if !args.once {
		t.Errorf("once = false, want true")
	}
}

func TestParseTailArgs_Skip(t *testing.T) {
	args, err := parseTailArgs([]string{"--skip", "7", "/p"})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if args.skip != 7 {
		t.Errorf("skip = %d, want 7", args.skip)
	}
}

func TestParseTailArgs_Combined(t *testing.T) {
	args, err := parseTailArgs([]string{"--once", "--skip", "3", "/p"})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if !args.once || args.skip != 3 || args.path != "/p" {
		t.Errorf("got %+v", args)
	}
}

func TestParseTailArgs_MissingPath(t *testing.T) {
	_, err := parseTailArgs([]string{"--once"})
	if err == nil {
		t.Errorf("expected error for missing path")
	}
}

func TestParseTailArgs_UnknownFlag(t *testing.T) {
	_, err := parseTailArgs([]string{"--bogus", "/p"})
	if err == nil {
		t.Errorf("expected error for unknown flag")
	}
}

// Compile-time presence check for the runTail function we'll fill in
// in a later task. Keeping the reference here ensures the signature
// is stable.
var _ = func() {
	_ = runTail(context.Background(), tailArgs{}, &bytes.Buffer{})
	_ = strings.TrimSpace("")
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `PATH=/Users/myers/.local/share/mise/installs/go/1.25.7/bin:$PATH go test ./cmd/entrypoint/ -run '^TestParseTailArgs'`

Expected: build failure — `parseTailArgs` and `tailArgs` and `runTail` undefined.

- [ ] **Step 3: Create `cmd/entrypoint/tail.go` with arg parser and stub `runTail`**

Create `cmd/entrypoint/tail.go`:

```go
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
)

// tailArgs holds parsed flags for the `entrypoint tail` subcommand.
type tailArgs struct {
	path string
	once bool
	skip int
}

// parseTailArgs parses the argv tail (everything after `tail`) into tailArgs.
// Flag forms recognized: --once, --skip N. Positional argument: path.
func parseTailArgs(argv []string) (tailArgs, error) {
	var args tailArgs
	i := 0
	for i < len(argv) {
		a := argv[i]
		switch a {
		case "--once":
			args.once = true
			i++
		case "--skip":
			if i+1 >= len(argv) {
				return tailArgs{}, errors.New("--skip requires a value")
			}
			n, err := strconv.Atoi(argv[i+1])
			if err != nil {
				return tailArgs{}, fmt.Errorf("--skip: %w", err)
			}
			if n < 0 {
				return tailArgs{}, errors.New("--skip must be non-negative")
			}
			args.skip = n
			i += 2
		default:
			if len(a) > 2 && a[:2] == "--" {
				return tailArgs{}, fmt.Errorf("unknown flag: %s", a)
			}
			if args.path != "" {
				return tailArgs{}, fmt.Errorf("unexpected argument: %s", a)
			}
			args.path = a
			i++
		}
	}
	if args.path == "" {
		return tailArgs{}, errors.New("missing path argument")
	}
	return args, nil
}

// runTail is the stub for the tail behavior. Implemented in later tasks.
func runTail(ctx context.Context, args tailArgs, out io.Writer) error {
	return errors.New("not implemented")
}
```

- [ ] **Step 4: Run tests to verify parser tests pass**

Run: `PATH=/Users/myers/.local/share/mise/installs/go/1.25.7/bin:$PATH go test ./cmd/entrypoint/ -run '^TestParseTailArgs'`

Expected: PASS.

- [ ] **Step 5: Wire `tail` into `main`'s dispatch**

In `cmd/entrypoint/main.go`, modify the `switch os.Args[1]` block (around line 28). Add a `case "tail":` that parses args and calls `runTail` with stdout. Update `usage()` to mention the new subcommand.

Replace this block:

```go
switch os.Args[1] {
case "setup":
	if len(os.Args) < 3 {
		usage()
	}
	if err := runSetup(os.Args[2], actionsDir, shimDir); err != nil {
		fmt.Fprintf(os.Stderr, "setup error: %v\n", err)
		os.Exit(1)
	}
case "run":
	if len(os.Args) < 3 {
		usage()
	}
	if !runEntrypoint(os.Args[2], shimDir) {
		os.Exit(1)
	}
default:
	usage()
}
```

With:

```go
switch os.Args[1] {
case "setup":
	if len(os.Args) < 3 {
		usage()
	}
	if err := runSetup(os.Args[2], actionsDir, shimDir); err != nil {
		fmt.Fprintf(os.Stderr, "setup error: %v\n", err)
		os.Exit(1)
	}
case "run":
	if len(os.Args) < 3 {
		usage()
	}
	if !runEntrypoint(os.Args[2], shimDir) {
		os.Exit(1)
	}
case "tail":
	args, err := parseTailArgs(os.Args[2:])
	if err != nil {
		fmt.Fprintf(os.Stderr, "tail: %v\n", err)
		usage()
	}
	if err := runTail(context.Background(), args, os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "tail: %v\n", err)
		os.Exit(1)
	}
default:
	usage()
}
```

Update `usage()`:

```go
func usage() {
	fmt.Fprintf(os.Stderr, "usage:\n")
	fmt.Fprintf(os.Stderr, "  entrypoint setup <manifest.json>             # init: fetch action sources into /actions/\n")
	fmt.Fprintf(os.Stderr, "  entrypoint run <manifest.json>               # runner: execute steps\n")
	fmt.Fprintf(os.Stderr, "  entrypoint tail [--once] [--skip N] <path>   # stream JSONL lines from a file to stdout\n")
	os.Exit(1)
}
```

The `context` import is already needed for `runTail`. If not present in `cmd/entrypoint/main.go`, add it to the import block.

- [ ] **Step 6: Build to confirm it compiles**

Run: `PATH=/Users/myers/.local/share/mise/installs/go/1.25.7/bin:$PATH make build-entrypoint`

Expected: builds cleanly. The new subcommand is dispatched but always returns "not implemented" — that's fine for now.

- [ ] **Step 7: Commit**

```bash
git add cmd/entrypoint/tail.go cmd/entrypoint/tail_test.go cmd/entrypoint/main.go
git commit -m "entrypoint: scaffold tail subcommand with arg parsing"
```

---

### Task 2: Implement `runTail` for one-shot mode

**Files:**
- Modify: `cmd/entrypoint/tail.go`
- Modify: `cmd/entrypoint/tail_test.go`

**Goal:** Read a JSONL file, optionally skip the first N lines, write the rest to stdout, return on EOF. This is the simpler `--once` path; the follow path lands in Task 3.

- [ ] **Step 1: Write the failing test**

Append to `cmd/entrypoint/tail_test.go`:

```go
import (
	"context"
	"os"
	"path/filepath"
)

func writeFile(t *testing.T, path string, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func TestRunTailOnce_AllLines(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "state.jsonl")
	writeFile(t, p, "line1\nline2\nline3\n")

	var buf bytes.Buffer
	err := runTail(context.Background(), tailArgs{path: p, once: true}, &buf)
	if err != nil {
		t.Fatalf("runTail: %v", err)
	}
	got := buf.String()
	want := "line1\nline2\nline3\n"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestRunTailOnce_Skip(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "state.jsonl")
	writeFile(t, p, "a\nb\nc\nd\n")

	var buf bytes.Buffer
	err := runTail(context.Background(), tailArgs{path: p, once: true, skip: 2}, &buf)
	if err != nil {
		t.Fatalf("runTail: %v", err)
	}
	if buf.String() != "c\nd\n" {
		t.Errorf("got %q, want \"c\\nd\\n\"", buf.String())
	}
}

func TestRunTailOnce_SkipBeyondFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "state.jsonl")
	writeFile(t, p, "a\nb\n")

	var buf bytes.Buffer
	err := runTail(context.Background(), tailArgs{path: p, once: true, skip: 10}, &buf)
	if err != nil {
		t.Fatalf("runTail: %v", err)
	}
	if buf.String() != "" {
		t.Errorf("got %q, want empty", buf.String())
	}
}

func TestRunTailOnce_PartialTrailingLine(t *testing.T) {
	// A trailing partial line (no \n) is not emitted in --once mode.
	dir := t.TempDir()
	p := filepath.Join(dir, "state.jsonl")
	writeFile(t, p, "complete\npartial-no-newline")

	var buf bytes.Buffer
	err := runTail(context.Background(), tailArgs{path: p, once: true}, &buf)
	if err != nil {
		t.Fatalf("runTail: %v", err)
	}
	if buf.String() != "complete\n" {
		t.Errorf("got %q, want \"complete\\n\"", buf.String())
	}
}

func TestRunTailOnce_NonexistentFile(t *testing.T) {
	var buf bytes.Buffer
	err := runTail(context.Background(), tailArgs{path: "/no/such/path", once: true}, &buf)
	if err == nil {
		t.Errorf("expected error for nonexistent file")
	}
}
```

- [ ] **Step 2: Run tests to confirm failure**

Run: `PATH=/Users/myers/.local/share/mise/installs/go/1.25.7/bin:$PATH go test ./cmd/entrypoint/ -run '^TestRunTailOnce'`

Expected: tests fail with "not implemented" or panics.

- [ ] **Step 3: Implement one-shot mode in `runTail`**

Replace the stub `runTail` in `cmd/entrypoint/tail.go` with:

```go
import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"time"
)

// runTail follows or one-shot reads the JSONL file at args.path, dropping the
// first args.skip newline-terminated lines and writing the remainder verbatim
// to out. Partial trailing lines (no \n) are never emitted.
func runTail(ctx context.Context, args tailArgs, out io.Writer) error {
	f, err := os.Open(args.path)
	if err != nil {
		return fmt.Errorf("opening %s: %w", args.path, err)
	}
	defer f.Close()

	reader := bufio.NewReader(f)
	skipped := 0

	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		line, rErr := reader.ReadString('\n')
		hasFullLine := rErr == nil && line != ""
		if hasFullLine {
			if skipped < args.skip {
				skipped++
				continue
			}
			if _, wErr := io.WriteString(out, line); wErr != nil {
				return fmt.Errorf("writing line: %w", wErr)
			}
			continue
		}
		// Either EOF or another error.
		if errors.Is(rErr, io.EOF) {
			if args.once {
				return nil
			}
			// Follow mode: wait for more data. Implemented in Task 3;
			// for now, treat as EOF return so existing once-mode tests pass.
			return nil
		}
		return fmt.Errorf("reading: %w", rErr)
	}
}

// silence unused imports during partial implementation; removed later.
var _ = strconv.Atoi
var _ = time.Second
```

(The `strconv` and `time` imports stay because Task 1 already used `strconv` for parsing and Task 3 will need `time` for follow-mode sleep — keeping both avoids unused-import noise across tasks.)

- [ ] **Step 4: Run tests to verify pass**

Run: `PATH=/Users/myers/.local/share/mise/installs/go/1.25.7/bin:$PATH go test ./cmd/entrypoint/ -run '^TestRunTailOnce'`

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add cmd/entrypoint/tail.go cmd/entrypoint/tail_test.go
git commit -m "entrypoint: implement tail one-shot mode"
```

---

### Task 3: Implement follow mode

**Files:**
- Modify: `cmd/entrypoint/tail.go`
- Modify: `cmd/entrypoint/tail_test.go`

**Goal:** In follow mode (no `--once`), tail keeps running. On EOF it sleeps 50 ms and retries the read; new appended lines flow out as they arrive. Exits cleanly on context cancel.

- [ ] **Step 1: Write the failing tests**

Append to `cmd/entrypoint/tail_test.go`:

```go
import (
	"sync"
)

func TestRunTailFollow_AppendsArriveLive(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "state.jsonl")
	writeFile(t, p, "first\n")

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	var buf safeBuffer
	done := make(chan error, 1)
	go func() { done <- runTail(ctx, tailArgs{path: p}, &buf) }()

	// Wait until we see "first\n".
	waitFor(t, 1*time.Second, func() bool { return buf.String() == "first\n" })

	// Append more lines while the tailer is running.
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatalf("open append: %v", err)
	}
	if _, err := f.WriteString("second\n"); err != nil {
		t.Fatalf("write: %v", err)
	}
	f.Close()

	waitFor(t, 1*time.Second, func() bool { return buf.String() == "first\nsecond\n" })

	cancel()
	<-done
}

func TestRunTailFollow_PartialLineCompletes(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "state.jsonl")
	writeFile(t, p, "")

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	var buf safeBuffer
	done := make(chan error, 1)
	go func() { done <- runTail(ctx, tailArgs{path: p}, &buf) }()

	// Write a partial line (no \n).
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatalf("open append: %v", err)
	}
	if _, err := f.WriteString("partial"); err != nil {
		t.Fatalf("write: %v", err)
	}

	// Confirm nothing emitted yet.
	time.Sleep(150 * time.Millisecond)
	if got := buf.String(); got != "" {
		t.Errorf("buf = %q before newline; want empty", got)
	}

	// Complete the line.
	if _, err := f.WriteString("-rest\n"); err != nil {
		t.Fatalf("write rest: %v", err)
	}
	f.Close()

	waitFor(t, 1*time.Second, func() bool { return buf.String() == "partial-rest\n" })

	cancel()
	<-done
}

func TestRunTailFollow_ContextCancel(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "state.jsonl")
	writeFile(t, p, "")

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() { done <- runTail(ctx, tailArgs{path: p}, &bytes.Buffer{}) }()

	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("got err = %v, want context.Canceled", err)
		}
	case <-time.After(1 * time.Second):
		t.Fatalf("runTail did not exit after cancel")
	}
}

// safeBuffer is bytes.Buffer guarded by a mutex; tests need to read while
// runTail writes from another goroutine.
type safeBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *safeBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *safeBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

func waitFor(t *testing.T, timeout time.Duration, pred func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if pred() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("waitFor: predicate did not become true within %s", timeout)
}
```

The `errors` import is already needed in tests now; ensure it's in the import block.

- [ ] **Step 2: Run tests to confirm failure**

Run: `PATH=/Users/myers/.local/share/mise/installs/go/1.25.7/bin:$PATH go test ./cmd/entrypoint/ -run '^TestRunTailFollow' -v`

Expected: failures — without follow logic, `runTail` returns nil at EOF and the buffer never picks up appended bytes.

- [ ] **Step 3: Replace `runTail` with follow-aware implementation**

Replace the body of `runTail` in `cmd/entrypoint/tail.go`:

```go
func runTail(ctx context.Context, args tailArgs, out io.Writer) error {
	f, err := os.Open(args.path)
	if err != nil {
		return fmt.Errorf("opening %s: %w", args.path, err)
	}
	defer f.Close()

	reader := bufio.NewReader(f)
	skipped := 0
	const followSleep = 50 * time.Millisecond

	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		line, rErr := reader.ReadString('\n')

		// Handle a complete line (rErr == nil and line ends in \n).
		if rErr == nil && len(line) > 0 {
			if skipped < args.skip {
				skipped++
				continue
			}
			if _, wErr := io.WriteString(out, line); wErr != nil {
				return fmt.Errorf("writing line: %w", wErr)
			}
			continue
		}

		// rErr != nil. Two interesting subcases: EOF (with possible partial
		// line in `line`) and any other error.
		if errors.Is(rErr, io.EOF) {
			// If we got a partial line at EOF, ReadString returned the bytes
			// in `line` without a trailing newline. We must NOT consume them;
			// they belong to the next iteration so the line completes.
			//
			// bufio.Reader holds those bytes in its internal buffer too — a
			// subsequent ReadString call after more data arrives will return
			// the full line including the partial bytes already returned to
			// us here. So we discard `line` and loop.
			if args.once {
				return nil
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(followSleep):
			}
			continue
		}
		return fmt.Errorf("reading: %w", rErr)
	}
}
```

**Sanity check:** The "discard partial line at EOF" claim hinges on `bufio.Reader.ReadString` returning the partial bytes and `EOF` *without* advancing past them on subsequent reads when more data arrives. That is the documented behavior — `ReadString` returns the data read so far plus the error; the next call continues from where the buffer left off. The partial bytes are still in the internal buffer and will be returned again as part of the completed line.

- [ ] **Step 4: Remove the placeholder unused-import shims**

Delete the `var _ = strconv.Atoi` and `var _ = time.Second` lines from Task 2 — `strconv` is now unused by `tail.go` (only the parser uses it; that lives in the same file but the variables are gone), and `time` is now genuinely used. Adjust the import block:

```go
import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"time"
)
```

Keep `strconv` because `parseTailArgs` (in this same file) uses it. Drop the `_ = ...` shims.

- [ ] **Step 5: Run tests to verify pass**

Run: `PATH=/Users/myers/.local/share/mise/installs/go/1.25.7/bin:$PATH go test ./cmd/entrypoint/ -run '^TestRunTail' -v`

Expected: all `TestRunTailOnce*` and `TestRunTailFollow*` pass.

- [ ] **Step 6: Run full entrypoint package tests**

Run: `PATH=/Users/myers/.local/share/mise/installs/go/1.25.7/bin:$PATH go test ./cmd/entrypoint/...`

Expected: PASS for the whole package — this catches any regressions in setup/run/manifest tests.

- [ ] **Step 7: Commit**

```bash
git add cmd/entrypoint/tail.go cmd/entrypoint/tail_test.go
git commit -m "entrypoint: tail follow mode with partial-line and cancel handling"
```

---

### Task 4: Switch `PodExecutor` to streaming interface

**Files:**
- Modify: `pkg/k8s/watcher.go` (interface, `SPDYExecutor`, default executor wiring at lines 25–56 + 234–261)
- Modify: `pkg/k8s/watcher_test.go` (mockPodExecutor + any callers)

**Goal:** Replace `Exec(...) (string, error)` with `ExecStream(...) (io.ReadCloser, error)` so a long-lived exec session is possible. The current `pollStateFileWith` is left in place using the new interface for now (one breaking change at a time); Task 5 removes it.

- [ ] **Step 1: Update `mockPodExecutor` in tests**

In `pkg/k8s/watcher_test.go`, replace the existing `mockPodExecutor` (lines 23–43) with one that implements `ExecStream`:

```go
// mockPodExecutor implements PodExecutor for testing.
type mockPodExecutor struct {
	mu      sync.Mutex
	outputs []string // sequential outputs for each ExecStream call
	errs    []error
	idx     int
}

func (m *mockPodExecutor) ExecStream(_ context.Context, _, _, _ string, _ []string) (io.ReadCloser, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	i := m.idx
	m.idx++
	if i < len(m.errs) && m.errs[i] != nil {
		return nil, m.errs[i]
	}
	if i < len(m.outputs) {
		return io.NopCloser(strings.NewReader(m.outputs[i])), nil
	}
	return nil, fmt.Errorf("terminated")
}
```

- [ ] **Step 2: Update `PodExecutor` interface and `SPDYExecutor` in `watcher.go`**

In `pkg/k8s/watcher.go`, change the interface (line 25–28):

```go
// PodExecutor opens a streaming exec session against a pod container. The
// returned reader streams stdout from the command. The caller MUST Close it.
type PodExecutor interface {
	ExecStream(ctx context.Context, namespace, pod, container string, cmd []string) (io.ReadCloser, error)
}
```

Replace `SPDYExecutor.Exec` (lines 53–56) with `ExecStream`:

```go
// SPDYExecutor implements PodExecutor using the k8s SPDY protocol.
type SPDYExecutor struct {
	Client  kubernetes.Interface
	RestCfg *rest.Config
}

func (s *SPDYExecutor) ExecStream(ctx context.Context, namespace, pod, container string, cmd []string) (io.ReadCloser, error) {
	return execStream(ctx, s.Client, s.RestCfg, namespace, pod, container, cmd)
}
```

Replace `execInPod` (around line 235) with `execStream`:

```go
// execStream runs a command in a running container and returns a reader that
// streams stdout. The command keeps running until it exits or the caller
// closes the reader. Stderr is discarded.
func execStream(ctx context.Context, client kubernetes.Interface, restCfg *rest.Config, namespace, podName, container string, cmd []string) (io.ReadCloser, error) {
	req := client.CoreV1().RESTClient().Post().
		Resource("pods").
		Name(podName).
		Namespace(namespace).
		SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: container,
			Command:   cmd,
			Stdout:    true,
			Stderr:    false,
		}, scheme.ParameterCodec)

	exec, err := remotecommand.NewSPDYExecutor(restCfg, "POST", req.URL())
	if err != nil {
		return nil, fmt.Errorf("creating SPDY executor: %w", err)
	}

	pr, pw := io.Pipe()
	go func() {
		err := exec.StreamWithContext(ctx, remotecommand.StreamOptions{
			Stdout: pw,
		})
		// Close the writer so the reader sees EOF (or the error).
		pw.CloseWithError(err)
	}()
	return pr, nil
}
```

Note: `bytes` import (used by old `execInPod`) is no longer needed by `execStream`; check if anything else still uses it before removing.

- [ ] **Step 3: Update `pollStateFileWith` to consume the stream API**

Update `pollStateFileWith` (around line 129) so it still works with the new interface. Each poll opens a stream, reads everything via `io.ReadAll`, then closes. Same observed behavior as before, just adapted to the new interface — this is interim glue that gets deleted in Task 5.

```go
// pollStateFileWith reads the entrypoint's state.jsonl file to track step lifecycle.
// Interim implementation against the new streaming PodExecutor; will be replaced
// by streamStateFileWith in a follow-up task.
func pollStateFileWith(ctx context.Context, executor PodExecutor, namespace, podName string, rep *reporter.Reporter, poll time.Duration) error {
	var lastOffset int

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(poll):
		}

		stream, err := executor.ExecStream(ctx, namespace, podName, "runner",
			[]string{"cat", "/shim/state.jsonl"})
		if err != nil {
			if strings.Contains(err.Error(), "terminated") || strings.Contains(err.Error(), "not found") {
				return nil
			}
			continue
		}
		body, rErr := io.ReadAll(stream)
		stream.Close()
		if rErr != nil {
			continue
		}
		events, newOffset := parseStateEvents(string(body), lastOffset)
		for _, event := range events {
			routeStateEvent(event, rep)
		}
		lastOffset = newOffset
	}
}
```

- [ ] **Step 4: Run tests to confirm green**

Run: `PATH=/Users/myers/.local/share/mise/installs/go/1.25.7/bin:$PATH go test ./pkg/k8s/...`

Expected: all existing tests pass. The mock returns `io.ReadCloser` now; the consumers (`pollStateFileWith` via `io.ReadAll`) flatten it back to a string for `parseStateEvents`.

- [ ] **Step 5: Build to confirm full project compiles**

Run: `PATH=/Users/myers/.local/share/mise/installs/go/1.25.7/bin:$PATH make build`

Expected: both binaries build cleanly.

- [ ] **Step 6: Commit**

```bash
git add pkg/k8s/watcher.go pkg/k8s/watcher_test.go
git commit -m "k8s: switch PodExecutor to streaming ExecStream interface"
```

---

### Task 5: Implement `streamStateFileWith` and `drainStateFile`

**Files:**
- Modify: `pkg/k8s/watcher.go`
- Modify: `pkg/k8s/watcher_test.go`

**Goal:** Replace the `cat`-poll with a long-lived `entrypoint tail` exec stream + reconnect, plus a one-shot post-exit drain. The end-of-job rewiring of `watchJobWith` happens in Task 6 — this task just adds the new functions with full test coverage so they're trusted before we cut over.

- [ ] **Step 1: Write tests for `streamStateFileWith`**

Append to `pkg/k8s/watcher_test.go`:

```go
import (
	"strconv"
)

// findFlagValue scans cmd for `flag` and returns the next argument's value.
// Returns "" if not found.
func findFlagValue(cmd []string, flag string) string {
	for i := 0; i < len(cmd)-1; i++ {
		if cmd[i] == flag {
			return cmd[i+1]
		}
	}
	return ""
}

// recordingExecutor records every ExecStream invocation and serves outputs
// from a programmable script.
type recordingExecutor struct {
	mu       sync.Mutex
	calls    [][]string
	outputs  []string
	errs     []error
	idx      int
}

func (r *recordingExecutor) ExecStream(_ context.Context, _, _, _ string, cmd []string) (io.ReadCloser, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, append([]string(nil), cmd...))
	i := r.idx
	r.idx++
	if i < len(r.errs) && r.errs[i] != nil {
		return nil, r.errs[i]
	}
	if i < len(r.outputs) {
		return io.NopCloser(strings.NewReader(r.outputs[i])), nil
	}
	return nil, fmt.Errorf("terminated")
}

func (r *recordingExecutor) callCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.calls)
}

func (r *recordingExecutor) callAt(i int) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls[i]
}

func TestStreamStateFile_RoutesAllEvents(t *testing.T) {
	rep := newTestReporter(1, 3)
	jsonl := `{"event":"start","step":0,"name":"checkout","time":"2026-05-06T14:35:54Z"}
{"event":"end","step":0,"name":"checkout","exit_code":0,"time":"2026-05-06T14:35:58Z"}
{"event":"start","step":1,"name":"build","time":"2026-05-06T14:35:58Z"}
`
	exec := &recordingExecutor{outputs: []string{jsonl}}

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		// Once exec returns the stream EOF, the function will try to reconnect.
		// Cancel ctx after a short pause to break it out of the retry loop.
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()

	off, _ := streamStateFileWith(ctx, exec, "ns", "pod", rep)
	if off != 3 {
		t.Errorf("offset = %d, want 3", off)
	}
	// The first call should NOT carry --skip (skip=0).
	if got := findFlagValue(exec.callAt(0), "--skip"); got != "0" && got != "" {
		t.Errorf("first call --skip = %q, want \"0\" or absent", got)
	}
}

func TestStreamStateFile_ReconnectAfterError(t *testing.T) {
	rep := newTestReporter(1, 5)
	first := `{"event":"start","step":0,"name":"a","time":"t"}
{"event":"end","step":0,"name":"a","exit_code":0,"time":"t"}
{"event":"start","step":1,"name":"b","time":"t"}
`
	// After 3 routed events, EOF triggers reconnect. Second call returns more.
	second := `{"event":"end","step":1,"name":"b","exit_code":0,"time":"t"}
`
	exec := &recordingExecutor{outputs: []string{first, second}}

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(150 * time.Millisecond)
		cancel()
	}()

	off, _ := streamStateFileWith(ctx, exec, "ns", "pod", rep)
	if off != 4 {
		t.Errorf("offset = %d, want 4", off)
	}
	if exec.callCount() < 2 {
		t.Fatalf("expected >= 2 calls, got %d", exec.callCount())
	}
	if got := findFlagValue(exec.callAt(1), "--skip"); got != "3" {
		t.Errorf("second call --skip = %q, want \"3\"", got)
	}
}

func TestStreamStateFile_MalformedLineSkipped(t *testing.T) {
	rep := newTestReporter(1, 2)
	jsonl := `{not json}
{"event":"start","step":0,"name":"a","time":"t"}
`
	exec := &recordingExecutor{outputs: []string{jsonl}}

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()

	off, _ := streamStateFileWith(ctx, exec, "ns", "pod", rep)
	if off != 2 {
		t.Errorf("offset = %d, want 2 (both lines counted)", off)
	}
}

func TestStreamStateFile_MaxRetries(t *testing.T) {
	rep := newTestReporter(1, 0)
	exec := &recordingExecutor{
		errs: []error{
			fmt.Errorf("boom1"),
			fmt.Errorf("boom2"),
			fmt.Errorf("boom3"),
			fmt.Errorf("boom4"),
			fmt.Errorf("boom5"),
		},
	}

	ctx := context.Background()
	off, err := streamStateFileWith(ctx, exec, "ns", "pod", rep)
	if err == nil {
		t.Errorf("expected error after max retries")
	}
	if off != 0 {
		t.Errorf("offset = %d, want 0", off)
	}
	if exec.callCount() != 5 {
		t.Errorf("call count = %d, want 5", exec.callCount())
	}
}

func TestDrainStateFile_RoutesEvents(t *testing.T) {
	rep := newTestReporter(1, 2)
	jsonl := `{"event":"end","step":0,"name":"a","exit_code":0,"time":"t"}
{"event":"end","step":1,"name":"b","exit_code":1,"time":"t"}
`
	exec := &recordingExecutor{outputs: []string{jsonl}}

	drainStateFile(context.Background(), exec, "ns", "pod", rep, 3)

	if exec.callCount() != 1 {
		t.Errorf("call count = %d, want 1", exec.callCount())
	}
	cmd := exec.callAt(0)
	if findFlagValue(cmd, "--skip") != "3" {
		t.Errorf("--skip = %q, want \"3\"", findFlagValue(cmd, "--skip"))
	}
	hasOnce := false
	for _, a := range cmd {
		if a == "--once" {
			hasOnce = true
		}
	}
	if !hasOnce {
		t.Errorf("drain command missing --once flag: %v", cmd)
	}
}

func TestDrainStateFile_TerminatedContainer(t *testing.T) {
	rep := newTestReporter(1, 1)
	exec := &recordingExecutor{errs: []error{fmt.Errorf("container terminated")}}

	// Should not panic, should not block; just log a warning and return.
	drainStateFile(context.Background(), exec, "ns", "pod", rep, 0)
}

// Compile-time silencing for strconv import which we'll exercise via
// findFlagValue once strconv is needed in tests; otherwise drop the import.
var _ = strconv.Itoa
```

- [ ] **Step 2: Run the new tests to confirm failure**

Run: `PATH=/Users/myers/.local/share/mise/installs/go/1.25.7/bin:$PATH go test ./pkg/k8s/ -run '^TestStreamStateFile|^TestDrainStateFile' -v`

Expected: build failures — `streamStateFileWith` and `drainStateFile` undefined.

- [ ] **Step 3: Implement `streamStateFileWith` and `drainStateFile`**

Add to `pkg/k8s/watcher.go` (after `pollStateFileWith`):

```go
// streamStateFileWith maintains a long-lived `entrypoint tail` exec session
// against /shim/state.jsonl and routes each newline-terminated state event
// into rep. On transient stream failure it reconnects with --skip <lastOffset>
// so events are never replayed. After maxStreamAttempts failures in a row it
// returns. The returned offset is the number of newline-terminated lines
// successfully processed (whether routed or skipped as malformed).
func streamStateFileWith(ctx context.Context, executor PodExecutor, namespace, podName string, rep *reporter.Reporter) (int, error) {
	const (
		initialBackoff = 50 * time.Millisecond
		maxBackoff     = 2 * time.Second
		maxAttempts    = 5
	)

	lastOffset := 0
	backoff := initialBackoff
	attempt := 0

	for {
		if err := ctx.Err(); err != nil {
			return lastOffset, err
		}

		cmd := []string{"/shim/entrypoint", "tail",
			"--skip", strconv.Itoa(lastOffset),
			"/shim/state.jsonl"}
		stream, err := executor.ExecStream(ctx, namespace, podName, "runner", cmd)
		if err != nil {
			attempt++
			if attempt >= maxAttempts {
				slog.Error("state stream gave up after retries",
					"lastOffset", lastOffset, "err", err)
				return lastOffset, err
			}
			select {
			case <-ctx.Done():
				return lastOffset, ctx.Err()
			case <-time.After(backoff):
			}
			if backoff < maxBackoff {
				backoff *= 2
				if backoff > maxBackoff {
					backoff = maxBackoff
				}
			}
			continue
		}
		// Successful connect: reset attempt counter and backoff.
		attempt = 0
		backoff = initialBackoff

		reader := bufio.NewReader(stream)
		for {
			line, rErr := reader.ReadString('\n')
			if rErr == nil && len(line) > 0 {
				trimmed := strings.TrimRight(line, "\n\r")
				if trimmed == "" {
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
				continue
			}
			// rErr != nil: close the stream and decide whether to reconnect.
			stream.Close()
			if errors.Is(rErr, context.Canceled) ||
				errors.Is(rErr, context.DeadlineExceeded) {
				return lastOffset, rErr
			}
			// Any other error (including io.EOF) → break inner loop and
			// reconnect via the outer loop.
			break
		}
	}
}

// drainStateFile performs a single best-effort one-shot read of any state
// events written to /shim/state.jsonl after the streaming tail's last
// observed line. Errors are logged at warn and not returned: by the time we
// drain, the runner container may already be terminated.
func drainStateFile(ctx context.Context, executor PodExecutor, namespace, podName string, rep *reporter.Reporter, lastOffset int) {
	if err := ctx.Err(); err != nil {
		slog.Warn("post-exit state drain skipped: ctx already canceled",
			"err", err, "lastOffset", lastOffset)
		return
	}

	drainCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	cmd := []string{"/shim/entrypoint", "tail", "--once",
		"--skip", strconv.Itoa(lastOffset),
		"/shim/state.jsonl"}
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
		if rErr == nil && len(line) > 0 {
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
			continue
		}
		if rErr != io.EOF {
			slog.Warn("post-exit state drain stream error",
				"err", rErr, "lastOffset", lastOffset)
		}
		return
	}
}
```

Add the `errors` and `strconv` imports if missing:

```go
import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strconv"
	"strings"
	"time"
	...
)
```

- [ ] **Step 4: Run the tests to verify pass**

Run: `PATH=/Users/myers/.local/share/mise/installs/go/1.25.7/bin:$PATH go test ./pkg/k8s/ -run '^TestStreamStateFile|^TestDrainStateFile' -v`

Expected: PASS for all six tests.

- [ ] **Step 5: Run the full k8s package tests**

Run: `PATH=/Users/myers/.local/share/mise/installs/go/1.25.7/bin:$PATH go test ./pkg/k8s/...`

Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add pkg/k8s/watcher.go pkg/k8s/watcher_test.go
git commit -m "k8s: add streamStateFileWith and drainStateFile"
```

---

### Task 6: Cut `watchJobWith` over to streaming + drain

**Files:**
- Modify: `pkg/k8s/watcher.go` (the body of `watchJobWith`, around lines 100–125)
- Modify: `pkg/k8s/watcher_test.go` (any tests that exercised `pollStateFileWith` directly — drop them since the function will be deleted)

**Goal:** Switch `watchJobWith` to use `streamStateFileWith` and `drainStateFile`. Delete the now-unused `pollStateFileWith` and `parseStateEvents` is kept (still exported-ish? — actually it's package-local; check). Remove the `time.Sleep(cfg.PollInterval * 2)` line.

- [ ] **Step 1: Inspect what tests still depend on the old code**

Run: `PATH=/Users/myers/.local/share/mise/installs/go/1.25.7/bin:$PATH grep -n "pollStateFileWith\|parseStateEvents" pkg/k8s/`

Expected output: list of references. Note them. We are removing `pollStateFileWith`; `parseStateEvents` is fine to keep (a helper that may have its own unit tests).

- [ ] **Step 2: Update `watchJobWith` body**

In `pkg/k8s/watcher.go`, replace the block from "Stream logs and poll state in parallel" through "Determine result from container exit code" (around lines 102–123) with:

```go
	// Stream logs from the runner container.
	logDone := make(chan error, 1)
	go func() {
		logDone <- streamLogs(ctx, streamer, namespace, podName, "runner", rep, cfg.CommandProc)
	}()

	// Stream step-state events via the entrypoint tail subcommand.
	type streamResult struct {
		offset int
		err    error
	}
	streamCtx, cancelStream := context.WithCancel(ctx)
	defer cancelStream()
	stateDone := make(chan streamResult, 1)
	go func() {
		off, sErr := streamStateFileWith(streamCtx, executor, namespace, podName, rep)
		stateDone <- streamResult{offset: off, err: sErr}
	}()

	// Wait for log streaming to finish (container exits).
	<-logDone

	// Stop the live state streamer and pick up its final offset.
	cancelStream()
	res := <-stateDone

	// Authoritatively drain anything written after the last streamed line.
	drainStateFile(ctx, executor, namespace, podName, rep, res.offset)

	// Determine result from container exit code.
	result, err := getContainerResult(ctx, client, namespace, podName)
	if err != nil {
		return runnerv1.Result_RESULT_FAILURE, err
	}

	return result, nil
```

The `time.Sleep(cfg.PollInterval * 2)` line goes away. The `cfg.PollInterval` field is no longer referenced in `watchJobWith`; leave the field on `WatchConfig` for now (other callers may set it; check).

- [ ] **Step 3: Delete `pollStateFileWith`**

Remove the `pollStateFileWith` function from `pkg/k8s/watcher.go`. If `parseStateEvents` is now unused, delete it too. Run a grep first:

Run: `PATH=/Users/myers/.local/share/mise/installs/go/1.25.7/bin:$PATH grep -n "pollStateFileWith\|parseStateEvents" pkg/k8s/*.go`

Delete the function and any references. If `parseStateEvents` has remaining test callers, keep it; otherwise delete it and its tests.

- [ ] **Step 4: Remove or update tests that referenced the old function**

For each grep hit in `pkg/k8s/watcher_test.go` referencing `pollStateFileWith` or `parseStateEvents` (if you removed it):
- If the test was specifically about polling behavior, delete it — covered by the new streaming tests in Task 5.
- If the test exercised `parseStateEvents` directly and we kept the helper, leave it.

- [ ] **Step 5: Run the full k8s tests**

Run: `PATH=/Users/myers/.local/share/mise/installs/go/1.25.7/bin:$PATH go test ./pkg/k8s/...`

Expected: PASS. If failures: usually a stale test referencing the deleted function. Delete the test or update it to use the new functions.

- [ ] **Step 6: Run all tests**

Run: `PATH=/Users/myers/.local/share/mise/installs/go/1.25.7/bin:$PATH make test`

Expected: PASS across the whole repo.

- [ ] **Step 7: Build**

Run: `PATH=/Users/myers/.local/share/mise/installs/go/1.25.7/bin:$PATH make build`

Expected: both binaries build.

- [ ] **Step 8: Lint**

Run: `PATH=/Users/myers/.local/share/mise/installs/go/1.25.7/bin:$PATH make lint`

Expected: clean.

- [ ] **Step 9: Commit**

```bash
git add pkg/k8s/watcher.go pkg/k8s/watcher_test.go
git commit -m "k8s: replace state-file poll loop with tail stream + post-exit drain"
```

---

### Task 7: End-to-end smoke test

**Files:**
- None — pure verification.

**Goal:** Confirm the controller and entrypoint binaries built from the new code work together against a live-ish setup. Since the user has another agent running the test cluster, this task hands off to them: bake an image, push it, and let the cluster agent run a multi-step workflow that ends in a late-step failure.

- [ ] **Step 1: Build the docker image**

Run:

```bash
PATH=/Users/myers/.local/share/mise/installs/go/1.25.7/bin:$PATH make image
```

Expected: image built locally.

- [ ] **Step 2: Push to the dev registry**

Run:

```bash
PATH=/Users/myers/.local/share/mise/installs/go/1.25.7/bin:$PATH make push-k3d
```

Expected: image pushed to `localhost:5001/drawbar`. (The Makefile is the source of truth — adjust target if the user keeps pushing under a different tag.)

- [ ] **Step 3: Hand off to the test-cluster agent**

Note the new image tag (e.g. `localhost:5001/drawbar:main-<timestamp>-<sha>`). Pass it to the test-cluster agent along with the bug 016 verification queries:

- `gt api repos/<repo>/actions/runs/<run>/jobs` should now show:
  - Each step's `conclusion` matching its actual outcome (no global `failure` propagation).
  - Each step's `started_at` ≠ the prior step's `started_at`.
  - Step durations matching log-derived ground truth.

- `gt api repos/<repo>/actions/jobs/<job>/logs` (the log stream) — should still show `+ <command>` lines and `Step N (...) failed with exit code` markers in the right places.

- [ ] **Step 4: After the test-cluster agent reports back, decide:**

  - Pass: this implementation is good. Move to Task 8 (final review/cleanup).
  - Fail: triage. The most likely failure modes:
    - `entrypoint tail` not on the runner container's PATH — confirm `setup-shim` actually copies the binary to `/shim/entrypoint` (check `pkg/k8s/builder.go` and `cmd/entrypoint/setup.go`).
    - SPDY exec dropping unexpectedly — bump backoff, increase max attempts, or shorten ticker.
    - Drain timing out against a fast-exiting pod — extend the 5 s drain timeout if needed.

---

### Task 8: Final review

**Files:**
- None.

**Goal:** Walk the diff one last time before considering the bug closed.

- [ ] **Step 1: Diff against main**

Run: `git diff main -- cmd/entrypoint/ pkg/k8s/`

Skim for: leftover debug prints, unused imports, comments that no longer match the code, dead error paths.

- [ ] **Step 2: Verify the bug doc still reads accurately**

Open `bugs/016-step-reporter-misattributes-times-and-conclusions.md`. Add a short "Resolution" section at the bottom pointing at this plan and the spec, with a one-line summary of the fix and the verifying run from Task 7.

```markdown
## Resolution

Fixed by replacing the 500 ms `cat`-poll with a long-lived `entrypoint tail`
exec stream plus a post-exit one-shot drain. Implementation:
- Plan: `docs/superpowers/plans/2026-05-06-step-state-streaming.md`
- Spec: `docs/superpowers/specs/2026-05-06-step-state-streaming-design.md`

Verified against run <run-id> on <date>; per-step conclusions and
durations now match the log stream.
```

- [ ] **Step 3: Commit the resolution note**

```bash
git add bugs/016-step-reporter-misattributes-times-and-conclusions.md
git commit -m "bugs/016: resolution note pointing at the fix"
```

---

## Self-review checklist

- [x] **Spec coverage:**
  - "New entrypoint subcommand" → Tasks 1–3.
  - "Replace `pollStateFileWith` with `streamStateFileWith`" → Task 5.
  - "Post-exit authoritative drain" → Task 5 (impl) + Task 6 (wired into watchJobWith).
  - "PodExecutor interface change" → Task 4.
  - Spec's testing section (unit + integration + manual) → Tasks 1–6 cover unit; manual handed off in Task 7. Integration test under build tag is **explicitly deferred** in this plan because the user has a separate test-cluster agent doing live verification — flag that gap if the test cluster is not reliably available.
- [x] **Placeholder scan:** no TBD/TODO/"add appropriate" left in steps.
- [x] **Type consistency:** `tailArgs`, `runTail`, `streamStateFileWith`, `drainStateFile`, `recordingExecutor`, `mockPodExecutor` — names used identically across tasks.

## Execution Handoff

Plan complete and saved to `docs/superpowers/plans/2026-05-06-step-state-streaming.md`. Two execution options:

1. **Subagent-Driven (recommended)** — I dispatch a fresh subagent per task, review between tasks, fast iteration.
2. **Inline Execution** — Execute tasks in this session using executing-plans, batch execution with checkpoints.

Which approach?
