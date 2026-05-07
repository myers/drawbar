package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
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
}

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
