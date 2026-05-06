package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
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
