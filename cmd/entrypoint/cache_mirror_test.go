package main

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestMirrorOne_CreatesSymlinkWhenWorkspaceMissing(t *testing.T) {
	ws, cache := tmpWorkspaceAndCache(t)

	if err := mirrorOne(ws, cache, "/root", "target"); err != nil {
		t.Fatalf("mirrorOne: %v", err)
	}

	wsTarget := filepath.Join(ws, "target")
	cacheTarget := filepath.Join(cache, "target")

	got, err := os.Readlink(wsTarget)
	if err != nil {
		t.Fatalf("expected symlink at %s: %v", wsTarget, err)
	}
	if got != cacheTarget {
		t.Errorf("symlink target: got %q, want %q", got, cacheTarget)
	}
	if _, err := os.Stat(cacheTarget); err != nil {
		t.Errorf("expected cache dir to exist: %v", err)
	}
}

func TestMirrorOne_LeavesCorrectSymlinkAlone(t *testing.T) {
	ws, cache := tmpWorkspaceAndCache(t)
	wsTarget := filepath.Join(ws, "target")
	cacheTarget := filepath.Join(cache, "target")
	mustMkdir(t, cacheTarget)
	mustWriteFile(t, filepath.Join(cacheTarget, "marker"), "keep")
	mustSymlink(t, cacheTarget, wsTarget)

	if err := mirrorOne(ws, cache, "/root", "target"); err != nil {
		t.Fatalf("mirrorOne: %v", err)
	}

	got, err := os.Readlink(wsTarget)
	if err != nil || got != cacheTarget {
		t.Fatalf("symlink should be unchanged: got=%q err=%v", got, err)
	}
	// Marker still there.
	if _, err := os.Stat(filepath.Join(cacheTarget, "marker")); err != nil {
		t.Errorf("marker file should be untouched: %v", err)
	}
}

func TestMirrorOne_ReplacesWrongPointingSymlink(t *testing.T) {
	ws, cache := tmpWorkspaceAndCache(t)
	wsTarget := filepath.Join(ws, "target")
	bogus := filepath.Join(t.TempDir(), "bogus")
	mustMkdir(t, bogus)
	mustSymlink(t, bogus, wsTarget)

	if err := mirrorOne(ws, cache, "/root", "target"); err != nil {
		t.Fatalf("mirrorOne: %v", err)
	}

	got, err := os.Readlink(wsTarget)
	if err != nil {
		t.Fatalf("readlink: %v", err)
	}
	want := filepath.Join(cache, "target")
	if got != want {
		t.Errorf("symlink target: got %q, want %q", got, want)
	}
}

func TestMirrorOne_MergesRealDirIntoCache(t *testing.T) {
	ws, cache := tmpWorkspaceAndCache(t)
	wsTarget := filepath.Join(ws, "target")
	mustMkdir(t, wsTarget)
	mustWriteFile(t, filepath.Join(wsTarget, "a.txt"), "from-workspace")
	mustMkdir(t, filepath.Join(wsTarget, "sub"))
	mustWriteFile(t, filepath.Join(wsTarget, "sub", "b.txt"), "nested")

	// Pre-existing cache content; collisions: workspace wins.
	cacheTarget := filepath.Join(cache, "target")
	mustMkdir(t, cacheTarget)
	mustWriteFile(t, filepath.Join(cacheTarget, "a.txt"), "from-cache")
	mustWriteFile(t, filepath.Join(cacheTarget, "c.txt"), "only-in-cache")

	if err := mirrorOne(ws, cache, "/root", "target"); err != nil {
		t.Fatalf("mirrorOne: %v", err)
	}

	// /workspace/target is now a symlink to /cache/target.
	got, err := os.Readlink(wsTarget)
	if err != nil || got != cacheTarget {
		t.Fatalf("symlink: got=%q err=%v", got, err)
	}

	assertFile(t, filepath.Join(cacheTarget, "a.txt"), "from-workspace")
	assertFile(t, filepath.Join(cacheTarget, "c.txt"), "only-in-cache")
	assertFile(t, filepath.Join(cacheTarget, "sub", "b.txt"), "nested")
}

func TestMirrorOne_PreservesSymlinkDuringMerge(t *testing.T) {
	ws, cache := tmpWorkspaceAndCache(t)
	wsTarget := filepath.Join(ws, "target")
	mustMkdir(t, wsTarget)
	mustSymlink(t, "../elsewhere", filepath.Join(wsTarget, "rel-link"))

	if err := mirrorOne(ws, cache, "/root", "target"); err != nil {
		t.Fatalf("mirrorOne: %v", err)
	}

	cacheLink := filepath.Join(cache, "target", "rel-link")
	tgt, err := os.Readlink(cacheLink)
	if err != nil {
		t.Fatalf("symlink should be preserved: %v", err)
	}
	if tgt != "../elsewhere" {
		t.Errorf("symlink target: got %q want %q", tgt, "../elsewhere")
	}
}

func TestMirrorOne_LeavesRegularFileAlone(t *testing.T) {
	ws, cache := tmpWorkspaceAndCache(t)
	wsTarget := filepath.Join(ws, "target")
	mustWriteFile(t, wsTarget, "i am a file")

	if err := mirrorOne(ws, cache, "/root", "target"); err != nil {
		t.Fatalf("mirrorOne: %v", err)
	}

	// File should still be there as a regular file.
	info, err := os.Lstat(wsTarget)
	if err != nil {
		t.Fatalf("lstat: %v", err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		t.Errorf("expected regular file, got symlink")
	}
	if info.IsDir() {
		t.Errorf("expected regular file, got dir")
	}
}

func TestMirrorOne_NestedRelativePath(t *testing.T) {
	ws, cache := tmpWorkspaceAndCache(t)

	if err := mirrorOne(ws, cache, "/root", "deep/nested/dir"); err != nil {
		t.Fatalf("mirrorOne: %v", err)
	}

	wsPath := filepath.Join(ws, "deep/nested/dir")
	cachePath := filepath.Join(cache, "deep/nested/dir")
	got, err := os.Readlink(wsPath)
	if err != nil || got != cachePath {
		t.Fatalf("symlink: got=%q err=%v", got, err)
	}
}

func TestMirrorCachePaths_ContinuesAfterFailure(t *testing.T) {
	ws, cache := tmpWorkspaceAndCache(t)
	// Empty path produces a non-fatal error; valid relative path should
	// then be mirrored successfully.
	if err := mirrorCachePaths(ws, cache, []string{"", "valid-rel"}); err != nil {
		t.Fatalf("mirrorCachePaths should not return fatal error: %v", err)
	}
	// Confirm the second path was actually processed: symlink at ws/valid-rel.
	wsPath := filepath.Join(ws, "valid-rel")
	cachePath := filepath.Join(cache, "valid-rel")
	got, err := os.Readlink(wsPath)
	if err != nil {
		t.Fatalf("expected symlink at %s: %v", wsPath, err)
	}
	if got != cachePath {
		t.Errorf("symlink target: got %q, want %q", got, cachePath)
	}
}

func TestResolvePath_WorkspaceRelative(t *testing.T) {
	ws, cache := tmpWorkspaceAndCache(t)
	wsPath, cachePath, err := resolvePath(ws, cache, "/root", "target")
	if err != nil {
		t.Fatalf("resolvePath: %v", err)
	}
	if wsPath != filepath.Join(ws, "target") {
		t.Errorf("wsPath: got %q, want %q", wsPath, filepath.Join(ws, "target"))
	}
	if cachePath != filepath.Join(cache, "target") {
		t.Errorf("cachePath: got %q, want %q", cachePath, filepath.Join(cache, "target"))
	}
}

func TestResolvePath_HomeRelative(t *testing.T) {
	ws, cache := tmpWorkspaceAndCache(t)
	wsPath, cachePath, err := resolvePath(ws, cache, "/root", "~/.cargo/registry")
	if err != nil {
		t.Fatalf("resolvePath: %v", err)
	}
	if wsPath != "/root/.cargo/registry" {
		t.Errorf("wsPath: got %q, want %q", wsPath, "/root/.cargo/registry")
	}
	want := filepath.Join(cache, "root/.cargo/registry")
	if cachePath != want {
		t.Errorf("cachePath: got %q, want %q", cachePath, want)
	}
}

func TestResolvePath_HomeRelativeBareTilde(t *testing.T) {
	ws, cache := tmpWorkspaceAndCache(t)
	wsPath, cachePath, err := resolvePath(ws, cache, "/root", "~")
	if err != nil {
		t.Fatalf("resolvePath: %v", err)
	}
	if wsPath != "/root" {
		t.Errorf("wsPath: got %q, want %q", wsPath, "/root")
	}
	if cachePath != filepath.Join(cache, "root") {
		t.Errorf("cachePath: got %q, want %q", cachePath, filepath.Join(cache, "root"))
	}
}

func TestResolvePath_Absolute(t *testing.T) {
	ws, cache := tmpWorkspaceAndCache(t)
	wsPath, cachePath, err := resolvePath(ws, cache, "/root", "/var/cache/apt")
	if err != nil {
		t.Fatalf("resolvePath: %v", err)
	}
	if wsPath != "/var/cache/apt" {
		t.Errorf("wsPath: got %q, want %q", wsPath, "/var/cache/apt")
	}
	if cachePath != filepath.Join(cache, "var/cache/apt") {
		t.Errorf("cachePath: got %q, want %q", cachePath, filepath.Join(cache, "var/cache/apt"))
	}
}

func TestResolvePath_HomeRelativeFailsWhenHomeUnset(t *testing.T) {
	ws, cache := tmpWorkspaceAndCache(t)
	_, _, err := resolvePath(ws, cache, "", "~/.cargo")
	if err == nil {
		t.Fatal("expected error for ~/ with empty $HOME")
	}
	if !errors.Is(err, errHomeRequired) {
		t.Errorf("expected errHomeRequired, got %v", err)
	}
}

// Helpers.

func tmpWorkspaceAndCache(t *testing.T) (string, string) {
	t.Helper()
	root := t.TempDir()
	ws := filepath.Join(root, "workspace")
	cache := filepath.Join(root, "cache")
	mustMkdir(t, ws)
	mustMkdir(t, cache)
	return ws, cache
}

func mustMkdir(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", path, err)
	}
}

func mustWriteFile(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir parent: %v", err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func mustSymlink(t *testing.T, target, link string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		t.Fatalf("mkdir parent: %v", err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("symlink %s -> %s: %v", link, target, err)
	}
}

func assertFile(t *testing.T, path, want string) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if string(got) != want {
		t.Errorf("%s: got %q, want %q", path, string(got), want)
	}
}
