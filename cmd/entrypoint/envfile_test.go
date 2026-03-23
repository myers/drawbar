package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseEnvFile_SimpleKeyValue(t *testing.T) {
	f := writeTemp(t, "FOO=bar\nBAZ=qux\n")
	got, err := parseEnvFile(f)
	if err != nil {
		t.Fatal(err)
	}
	assertEq(t, got["FOO"], "bar")
	assertEq(t, got["BAZ"], "qux")
}

func TestParseEnvFile_Heredoc(t *testing.T) {
	f := writeTemp(t, "CERT<<EOF\nline1\nline2\nEOF\n")
	got, err := parseEnvFile(f)
	if err != nil {
		t.Fatal(err)
	}
	assertEq(t, got["CERT"], "line1\nline2")
}

func TestParseEnvFile_UnclosedHeredoc(t *testing.T) {
	f := writeTemp(t, "CERT<<EOF\nline1\nline2\n")
	_, err := parseEnvFile(f)
	if err == nil {
		t.Fatal("expected error for unclosed heredoc, got nil")
	}
}

func TestParseEnvFile_EmptyFile(t *testing.T) {
	f := writeTemp(t, "")
	got, err := parseEnvFile(f)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("expected empty map, got %v", got)
	}
}

func TestParseEnvFile_NonExistent(t *testing.T) {
	got, err := parseEnvFile("/nonexistent/path")
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Fatalf("expected nil for nonexistent file, got %v", got)
	}
}

func TestParseEnvFile_Mixed(t *testing.T) {
	f := writeTemp(t, "A=1\nB<<DELIM\nhello\nworld\nDELIM\nC=3\n")
	got, err := parseEnvFile(f)
	if err != nil {
		t.Fatal(err)
	}
	assertEq(t, got["A"], "1")
	assertEq(t, got["B"], "hello\nworld")
	assertEq(t, got["C"], "3")
}

func TestParseEnvFile_ValueWithEquals(t *testing.T) {
	f := writeTemp(t, "URL=https://example.com?a=1&b=2\n")
	got, err := parseEnvFile(f)
	if err != nil {
		t.Fatal(err)
	}
	assertEq(t, got["URL"], "https://example.com?a=1&b=2")
}

func TestParsePaths(t *testing.T) {
	f := writeTemp(t, "/usr/local/bin\n/opt/bin\n\n")
	got, err := parsePaths(f)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 paths, got %d", len(got))
	}
	assertEq(t, got[0], "/usr/local/bin")
	assertEq(t, got[1], "/opt/bin")
}

func TestParsePaths_NonExistent(t *testing.T) {
	got, err := parsePaths("/nonexistent/path")
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Fatalf("expected nil, got %v", got)
	}
}

func writeTemp(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "envfile")
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func assertEq(t *testing.T, got, want string) {
	t.Helper()
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}
