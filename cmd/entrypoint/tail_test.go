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
