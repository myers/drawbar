# Cache Paths: `~/` and Absolute — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make `drawbar/cache@v1` accept `~/`-prefixed and absolute `path:` entries, replacing today's silent miss with either correct mirroring or a visible job failure.

**Architecture:** Two-binary fix along the existing controller/entrypoint boundary. Controller's `extractCacheInfo` validates path shape (rejecting only `..` traversal) and surfaces invalid paths as job failures. Entrypoint's `mirrorOne` learns to classify paths and resolve `~` against `$HOME` at runtime, mirroring `$HOME/.cargo/registry` to `/cache/<HOME-stripped>/.cargo/registry` and `/var/cache/apt` to `/cache/var/cache/apt`. `Manifest.CachePaths` schema is unchanged — strings flow through verbatim.

**Tech Stack:** Go 1.25, `log/slog`, `code.gitea.io/actions-proto-go`, standard library `os`/`filepath`/`strings`/`errors`.

**Spec:** `docs/superpowers/specs/2026-05-03-cache-paths-home-and-absolute-design.md`
**Bug:** `bugs/009-cache-paths-only-workspace-relative.md`

---

## File Structure

Files modified:

- `cmd/entrypoint/cache_mirror.go` — add `errHomeRequired`, add `resolvePath`, change `mirrorOne` signature to accept `home`, change `mirrorCachePaths` to return error and propagate `$HOME`.
- `cmd/entrypoint/cache_mirror_test.go` — drop `TestMirrorOne_RejectsAbsolutePath`; update existing tests to pass `home`; add new tests for `~/` and absolute paths and the `$HOME`-unset failure.
- `cmd/entrypoint/main.go` — handle the new error return from `mirrorCachePaths` (emit state error, exit 1).
- `cmd/controller/main.go` — change `extractCacheInfo` to return an error, validate `..` traversal, accept `~/` and absolute, call `reportFailure` on invalid input.
- `cmd/controller/main_test.go` — add `TestExtractCacheInfo_*` tests.
- `actions/cache/action.yml` — update `path:` description.

Each task below is bite-sized (2–5 minutes), TDD-shaped where it makes sense, and ends with a commit. The Go binary path on this dev machine is `/Users/myers/.local/share/mise/installs/go/1.25.7/bin/go` (per `MEMORY.md`); the plan uses `go` and assumes the runner has it on PATH or aliases it.

---

## Task 1: Add `errHomeRequired` sentinel and `resolvePath` helper (TDD)

**Files:**
- Modify: `cmd/entrypoint/cache_mirror.go`
- Test: `cmd/entrypoint/cache_mirror_test.go`

- [ ] **Step 1: Write failing tests for `resolvePath`**

Append to `cmd/entrypoint/cache_mirror_test.go`:

```go
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
```

Add the `errors` import to the test file if it isn't already there.

- [ ] **Step 2: Run tests to verify they fail**

```
go test ./cmd/entrypoint/ -run TestResolvePath -v
```

Expected: FAIL — `resolvePath` and `errHomeRequired` are undefined.

- [ ] **Step 3: Implement `errHomeRequired` and `resolvePath`**

In `cmd/entrypoint/cache_mirror.go`, add the `errors` import (if not already present) and add at the top of the file (after the existing constants):

```go
// errHomeRequired is returned when a ~/-prefixed path is encountered
// but $HOME is empty in the runner container. mirrorCachePaths treats
// this as a job-fatal error; everything else is best-effort.
var errHomeRequired = errors.New("$HOME required but unset")

// resolvePath classifies a declared cache path and returns where the
// symlink lives (wsPath) and where the symlink points (cachePath).
//
//	"target"            -> (workspace+/target,            cache+/target)
//	"~/.cargo/registry" -> (home+/.cargo/registry,        cache+/<home>/.cargo/registry)
//	"/var/cache/apt"    -> (/var/cache/apt,               cache+/var/cache/apt)
//
// home is the runner's $HOME at call time; an empty home with a ~/-
// prefixed path returns errHomeRequired.
func resolvePath(workspace, cache, home, rel string) (wsPath, cachePath string, err error) {
	switch {
	case rel == "~" || strings.HasPrefix(rel, "~/"):
		if home == "" {
			return "", "", fmt.Errorf("path %q: %w", rel, errHomeRequired)
		}
		rest := strings.TrimPrefix(strings.TrimPrefix(rel, "~"), "/")
		wsPath = filepath.Join(home, rest)
		cachePath = filepath.Join(cache, strings.TrimPrefix(home, "/"), rest)
	case filepath.IsAbs(rel):
		wsPath = rel
		cachePath = filepath.Join(cache, strings.TrimPrefix(rel, "/"))
	default:
		wsPath = filepath.Join(workspace, rel)
		cachePath = filepath.Join(cache, rel)
	}
	return wsPath, cachePath, nil
}
```

Add `"strings"` to the imports.

- [ ] **Step 4: Run tests to verify they pass**

```
go test ./cmd/entrypoint/ -run TestResolvePath -v
```

Expected: PASS for all five `TestResolvePath_*` tests. Existing tests untouched.

- [ ] **Step 5: Commit**

```
git add cmd/entrypoint/cache_mirror.go cmd/entrypoint/cache_mirror_test.go
git commit -m "entrypoint: add resolvePath helper for cache path classification"
```

---

## Task 2: Change `mirrorOne` to accept `home` and use `resolvePath`

**Files:**
- Modify: `cmd/entrypoint/cache_mirror.go:47-100`
- Modify: `cmd/entrypoint/cache_mirror_test.go` (existing tests get a `home` argument; remove `TestMirrorOne_RejectsAbsolutePath`)

- [ ] **Step 1: Update existing tests to pass `home` and remove the obsolete test**

In `cmd/entrypoint/cache_mirror_test.go`:

1. Replace every existing `mirrorOne(ws, cache, "...")` call with `mirrorOne(ws, cache, "/root", "...")` (the home value doesn't matter for workspace-relative paths but the signature now requires it). Tests to update: `TestMirrorOne_CreatesSymlinkWhenWorkspaceMissing`, `TestMirrorOne_LeavesCorrectSymlinkAlone`, `TestMirrorOne_ReplacesWrongPointingSymlink`, `TestMirrorOne_MergesRealDirIntoCache`, `TestMirrorOne_PreservesSymlinkDuringMerge`, `TestMirrorOne_LeavesRegularFileAlone`, `TestMirrorOne_NestedRelativePath`.

2. Delete `TestMirrorOne_RejectsAbsolutePath` entirely (absolute paths are now valid).

3. Update `TestMirrorCachePaths_ContinuesAfterFailure` — the test currently passes `/absolute-bad` expecting failure. Change it so the bad input is `""` (empty path, which still errors) instead of an absolute path:

```go
func TestMirrorCachePaths_ContinuesAfterFailure(t *testing.T) {
	ws, cache := tmpWorkspaceAndCache(t)
	_ = ws
	_ = cache
	// Empty path still fails; valid relative path should still get processed.
	if err := mirrorCachePaths([]string{"", "valid-rel"}); err != nil {
		t.Fatalf("mirrorCachePaths should not return fatal error for empty path: %v", err)
	}
}
```

(The `mirrorCachePaths` signature changes in this task too — see Step 3.)

- [ ] **Step 2: Run tests to verify they fail**

```
go test ./cmd/entrypoint/ -v
```

Expected: FAIL — `mirrorOne` signature mismatch in tests, `mirrorCachePaths` no longer returns nothing.

- [ ] **Step 3: Update `mirrorOne` and `mirrorCachePaths`**

In `cmd/entrypoint/cache_mirror.go`, replace `mirrorOne` and `mirrorCachePaths` with:

```go
// mirrorCachePaths makes the configured paths under /workspace, $HOME,
// or absolute roots into symlinks pointing at /cache/<location>. Called
// before each step. Returns a non-nil error only for fatal conditions
// (currently: a ~/-prefixed path with $HOME unset). Per-path filesystem
// errors are logged and skipped — caching just doesn't kick in for that
// path this run.
func mirrorCachePaths(paths []string) error {
	home := os.Getenv("HOME")
	for _, p := range paths {
		if err := mirrorOne(workspaceRoot, cacheMirrorRoot, home, p); err != nil {
			if errors.Is(err, errHomeRequired) {
				return err
			}
			fmt.Fprintf(os.Stderr, "cache mirror %q: %v\n", p, err)
		}
	}
	return nil
}

func mirrorOne(workspace, cache, home, rel string) error {
	if rel == "" {
		return fmt.Errorf("empty cache path")
	}
	wsPath, cachePath, err := resolvePath(workspace, cache, home, rel)
	if err != nil {
		return err
	}

	if err := os.MkdirAll(filepath.Dir(cachePath), 0o755); err != nil {
		return fmt.Errorf("mkdir cache parent: %w", err)
	}
	if err := os.MkdirAll(cachePath, 0o755); err != nil {
		return fmt.Errorf("mkdir cache: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(wsPath), 0o755); err != nil {
		return fmt.Errorf("mkdir workspace parent: %w", err)
	}

	info, err := os.Lstat(wsPath)
	if errors.Is(err, fs.ErrNotExist) {
		return os.Symlink(cachePath, wsPath)
	}
	if err != nil {
		return fmt.Errorf("lstat workspace: %w", err)
	}

	if info.Mode()&os.ModeSymlink != 0 {
		target, err := os.Readlink(wsPath)
		if err != nil {
			return fmt.Errorf("readlink workspace: %w", err)
		}
		if target == cachePath {
			return nil
		}
		if err := os.Remove(wsPath); err != nil {
			return fmt.Errorf("removing stale symlink: %w", err)
		}
		return os.Symlink(cachePath, wsPath)
	}

	if !info.IsDir() {
		return nil
	}

	if err := mergeDir(wsPath, cachePath); err != nil {
		return fmt.Errorf("merging into cache: %w", err)
	}
	if err := os.RemoveAll(wsPath); err != nil {
		return fmt.Errorf("removing workspace dir: %w", err)
	}
	return os.Symlink(cachePath, wsPath)
}
```

- [ ] **Step 4: Run tests to verify they pass**

```
go test ./cmd/entrypoint/ -v
```

Expected: PASS for all updated tests. (`TestResolvePath_*` from Task 1 still pass.)

- [ ] **Step 5: Commit**

```
git add cmd/entrypoint/cache_mirror.go cmd/entrypoint/cache_mirror_test.go
git commit -m "entrypoint: route mirrorOne through resolvePath, accept ~/ and absolute paths"
```

---

## Task 3: Add behavior tests for `~/` and absolute mirror paths (TDD)

Verifies the end-to-end mirror behavior — not just resolution, but the symlink actually getting created at the right place.

**Files:**
- Test: `cmd/entrypoint/cache_mirror_test.go`

- [ ] **Step 1: Write the new tests**

Append to `cmd/entrypoint/cache_mirror_test.go`:

```go
func TestMirrorOne_HomeRelativePath(t *testing.T) {
	ws, cache := tmpWorkspaceAndCache(t)
	home := filepath.Join(t.TempDir(), "home", "runner")
	mustMkdir(t, home)

	if err := mirrorOne(ws, cache, home, "~/.cargo/registry"); err != nil {
		t.Fatalf("mirrorOne: %v", err)
	}

	wsTarget := filepath.Join(home, ".cargo/registry")
	cacheTarget := filepath.Join(cache, strings.TrimPrefix(home, "/"), ".cargo/registry")

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

func TestMirrorOne_AbsolutePath(t *testing.T) {
	ws, cache := tmpWorkspaceAndCache(t)
	// Use a tempdir-prefixed absolute path so the test is hermetic.
	absDir := filepath.Join(t.TempDir(), "abs", "var", "cache", "apt")

	if err := mirrorOne(ws, cache, "/root", absDir); err != nil {
		t.Fatalf("mirrorOne: %v", err)
	}

	cacheTarget := filepath.Join(cache, strings.TrimPrefix(absDir, "/"))
	got, err := os.Readlink(absDir)
	if err != nil {
		t.Fatalf("expected symlink at %s: %v", absDir, err)
	}
	if got != cacheTarget {
		t.Errorf("symlink target: got %q, want %q", got, cacheTarget)
	}
}

func TestMirrorOne_AbsolutePathMergesExistingDir(t *testing.T) {
	ws, cache := tmpWorkspaceAndCache(t)
	absDir := filepath.Join(t.TempDir(), "abs", "var", "cache", "apt")
	mustMkdir(t, absDir)
	mustWriteFile(t, filepath.Join(absDir, "marker"), "fresh")

	if err := mirrorOne(ws, cache, "/root", absDir); err != nil {
		t.Fatalf("mirrorOne: %v", err)
	}

	cacheTarget := filepath.Join(cache, strings.TrimPrefix(absDir, "/"))
	got, err := os.Readlink(absDir)
	if err != nil || got != cacheTarget {
		t.Fatalf("symlink: got=%q err=%v", got, err)
	}
	assertFile(t, filepath.Join(cacheTarget, "marker"), "fresh")
}
```

Add `"strings"` to the test file imports if it isn't there yet.

- [ ] **Step 2: Run tests**

```
go test ./cmd/entrypoint/ -run "TestMirrorOne_HomeRelativePath|TestMirrorOne_AbsolutePath" -v
```

Expected: PASS. `mirrorOne` already does the right thing thanks to Task 2 — these tests just nail the behavior down.

- [ ] **Step 3: Commit**

```
git add cmd/entrypoint/cache_mirror_test.go
git commit -m "entrypoint: test ~/ and absolute cache path mirroring"
```

---

## Task 4: Add `mirrorCachePaths` fatal-error test for unset `$HOME` (TDD)

**Files:**
- Test: `cmd/entrypoint/cache_mirror_test.go`

- [ ] **Step 1: Write the failing test**

Append to `cmd/entrypoint/cache_mirror_test.go`:

```go
func TestMirrorCachePaths_FailsOnUnsetHome(t *testing.T) {
	t.Setenv("HOME", "")
	err := mirrorCachePaths([]string{"~/.cargo"})
	if err == nil {
		t.Fatal("expected fatal error when $HOME is unset")
	}
	if !errors.Is(err, errHomeRequired) {
		t.Errorf("expected errHomeRequired, got %v", err)
	}
}

func TestMirrorCachePaths_HomeSetSucceeds(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	// valid-rel falls back to /workspace, which doesn't exist as a real
	// dir in the test process — mirrorOne will log a non-fatal error
	// for it. We only care that the function returns nil overall.
	if err := mirrorCachePaths([]string{"valid-rel"}); err != nil {
		t.Fatalf("expected nil, got %v", err)
	}
}
```

- [ ] **Step 2: Run tests**

```
go test ./cmd/entrypoint/ -run TestMirrorCachePaths -v
```

Expected: PASS. The behavior is already in place from Task 2; this just locks it in.

- [ ] **Step 3: Commit**

```
git add cmd/entrypoint/cache_mirror_test.go
git commit -m "entrypoint: test mirrorCachePaths fatal $HOME error"
```

---

## Task 5: Wire the fatal error into the entrypoint step loop

**Files:**
- Modify: `cmd/entrypoint/main.go:99-101`

- [ ] **Step 1: Read the relevant lines**

Find this block in `cmd/entrypoint/main.go` (around lines 94–101):

```go
for i, step := range manifest.Steps {
	// Refresh /workspace/<path> -> /cache/<path> symlinks before every
	// step. The previous step (most often actions/checkout) may have
	// recreated cached directories; this re-establishes the symlinks
	// idempotently and merges any fresh contents into the cache.
	if len(manifest.CachePaths) > 0 {
		mirrorCachePaths(manifest.CachePaths)
	}
```

- [ ] **Step 2: Replace with error-aware version**

```go
for i, step := range manifest.Steps {
	// Refresh /workspace/<path> -> /cache/<path> symlinks before every
	// step. The previous step (most often actions/checkout) may have
	// recreated cached directories; this re-establishes the symlinks
	// idempotently and merges any fresh contents into the cache.
	if len(manifest.CachePaths) > 0 {
		if err := mirrorCachePaths(manifest.CachePaths); err != nil {
			writeState(stateFile, StateEvent{
				Event: "error",
				Step:  i,
				Name:  step.Name,
				Time:  time.Now().UTC().Format(time.RFC3339),
				Error: err.Error(),
			})
			fmt.Fprintf(os.Stderr, "cache mirror fatal: %v\n", err)
			os.Exit(1)
		}
	}
```

If `StateEvent` doesn't already have an `Error` field, check what's available in `pkg/types` (likely `Message` or similar):

```
grep -n "StateEvent" pkg/types/*.go cmd/entrypoint/*.go | head
```

Use the existing field for the error message. If no field exists, use `Name: err.Error()` as a fallback (matches what the file already does for skip events with messages baked into Name).

- [ ] **Step 3: Build to verify**

```
go build ./cmd/entrypoint/
```

Expected: builds cleanly.

- [ ] **Step 4: Run all entrypoint tests**

```
go test ./cmd/entrypoint/ -v
```

Expected: PASS.

- [ ] **Step 5: Commit**

```
git add cmd/entrypoint/main.go
git commit -m "entrypoint: fail job when cache path mirror returns fatal error"
```

---

## Task 6: Change `extractCacheInfo` to return error, accept `~/` and absolute (TDD)

**Files:**
- Modify: `cmd/controller/main.go:1009-1046`
- Modify: `cmd/controller/main.go:670-697` (caller, in same task to keep build green)
- Test: `cmd/controller/main_test.go`

- [ ] **Step 1: Write failing tests for the new `extractCacheInfo`**

Append to `cmd/controller/main_test.go`:

```go
func TestExtractCacheInfo_AcceptsAllShapes(t *testing.T) {
	steps := []types.StepSpec{{
		Env: map[string]string{
			"INPUT_KEY":  "k",
			"INPUT_PATH": "target\n~/.cargo/registry\n/var/cache/apt",
		},
	}}
	key, paths, _, err := extractCacheInfo(steps)
	if err != nil {
		t.Fatalf("extractCacheInfo: %v", err)
	}
	if key != "k" {
		t.Errorf("key: got %q, want %q", key, "k")
	}
	want := []string{"target", "~/.cargo/registry", "/var/cache/apt"}
	if len(paths) != len(want) {
		t.Fatalf("paths len: got %d, want %d", len(paths), len(want))
	}
	for i, p := range want {
		if paths[i] != p {
			t.Errorf("paths[%d]: got %q, want %q", i, paths[i], p)
		}
	}
}

func TestExtractCacheInfo_RejectsTraversal(t *testing.T) {
	steps := []types.StepSpec{{
		Env: map[string]string{
			"INPUT_KEY":  "k",
			"INPUT_PATH": "../escape",
		},
	}}
	_, _, _, err := extractCacheInfo(steps)
	if err == nil {
		t.Fatal("expected error for ../ traversal")
	}
}

func TestExtractCacheInfo_RejectsTraversalInMiddle(t *testing.T) {
	steps := []types.StepSpec{{
		Env: map[string]string{
			"INPUT_KEY":  "k",
			"INPUT_PATH": "good/../bad",
		},
	}}
	_, _, _, err := extractCacheInfo(steps)
	if err == nil {
		t.Fatal("expected error for embedded ../")
	}
}

func TestExtractCacheInfo_DedupesPaths(t *testing.T) {
	steps := []types.StepSpec{{
		Env: map[string]string{
			"INPUT_KEY":  "k",
			"INPUT_PATH": "target\ntarget\n~/.cargo\n~/.cargo",
		},
	}}
	_, paths, _, err := extractCacheInfo(steps)
	if err != nil {
		t.Fatalf("extractCacheInfo: %v", err)
	}
	if len(paths) != 2 {
		t.Errorf("expected dedup to 2 paths, got %d: %v", len(paths), paths)
	}
}

func TestExtractCacheInfo_NoCacheStep(t *testing.T) {
	steps := []types.StepSpec{{Env: map[string]string{}}}
	key, paths, _, err := extractCacheInfo(steps)
	if err != nil {
		t.Fatalf("extractCacheInfo: %v", err)
	}
	if key != "" || len(paths) != 0 {
		t.Errorf("expected empty result, got key=%q paths=%v", key, paths)
	}
}
```

If `types` isn't imported in `main_test.go`, add `"github.com/myers/drawbar/pkg/types"` (verify the actual module path with `head -1 go.mod`).

- [ ] **Step 2: Run tests to verify they fail**

```
go test ./cmd/controller/ -run TestExtractCacheInfo -v
```

Expected: FAIL — signature mismatch (`extractCacheInfo` returns 3 values, not 4) and `~/.cargo` / `/var/cache/apt` get silently dropped today.

- [ ] **Step 3: Replace `extractCacheInfo` in `cmd/controller/main.go`**

Find the existing function (around lines 1009–1046) and replace it with:

```go
// extractCacheInfo finds the cache key, paths, and restore-keys from
// drawbar/cache@v1 steps. Path entries are accepted in three shapes:
// workspace-relative ("target"), home-relative ("~/.cargo/registry"),
// and absolute ("/var/cache/apt"). Returns an error if any entry
// contains a ".." traversal segment — the caller fails the job.
func extractCacheInfo(steps []types.StepSpec) (key string, paths []string, restoreKeys []string, err error) {
	seen := make(map[string]bool)
	for _, step := range steps {
		k, ok := step.Env["INPUT_KEY"]
		if !ok || k == "" {
			continue
		}
		if key == "" {
			key = k
		}
		if p, ok := step.Env["INPUT_PATH"]; ok && p != "" {
			for _, entry := range strings.Split(p, "\n") {
				entry = strings.TrimSpace(entry)
				if entry == "" {
					continue
				}
				if hasTraversal(entry) {
					return "", nil, nil, fmt.Errorf("cache path %q contains '..'", entry)
				}
				if !seen[entry] {
					seen[entry] = true
					paths = append(paths, entry)
				}
			}
		}
		if rk, ok := step.Env["INPUT_RESTORE-KEYS"]; ok && rk != "" {
			for _, entry := range strings.Split(rk, "\n") {
				entry = strings.TrimSpace(entry)
				if entry != "" {
					restoreKeys = append(restoreKeys, entry)
				}
			}
		}
	}
	return key, paths, restoreKeys, nil
}

// hasTraversal reports whether any segment of the path is "..". Uses
// filepath.ToSlash so it behaves the same regardless of host OS — paths
// in workflow YAML always use forward slashes.
func hasTraversal(p string) bool {
	for _, seg := range strings.Split(filepath.ToSlash(p), "/") {
		if seg == ".." {
			return true
		}
	}
	return false
}
```

If `path/filepath` isn't already imported in `main.go`, add it.

- [ ] **Step 4: Update the caller in `cmd/controller/main.go`**

Find (around line 676):

```go
snapshotCacheKey, snapshotPaths, restoreKeys = extractCacheInfo(steps)
```

Replace the surrounding block with error-aware version:

```go
var extractErr error
snapshotCacheKey, snapshotPaths, restoreKeys, extractErr = extractCacheInfo(steps)
if extractErr != nil {
	slog.Error("invalid cache path", "error", extractErr, "task_id", task.GetId())
	reportFailure(ctx, cfg.ServerClient, task, fmt.Sprintf("Invalid cache path: %v", extractErr))
	continue
}
if snapshotCacheKey != "" && len(snapshotPaths) > 0 {
	// ... existing PVC creation block, unchanged ...
}
```

(`continue` returns control to the next iteration of the polling loop. Verify this by reading a few lines above to confirm the surrounding loop variable, and that `continue` is the right keyword — if the function isn't a loop, use `return` or whatever the surrounding pattern uses for "skip this task." Look at how line 472's `reportFailure` for parse errors handles control flow and match it.)

- [ ] **Step 5: Run all controller tests**

```
go test ./cmd/controller/ -v
```

Expected: PASS for the new `TestExtractCacheInfo_*` tests and any existing tests.

- [ ] **Step 6: Build the whole project**

```
go build ./...
```

Expected: clean build.

- [ ] **Step 7: Commit**

```
git add cmd/controller/main.go cmd/controller/main_test.go
git commit -m "controller: extractCacheInfo accepts ~/ and absolute, rejects .. traversal"
```

---

## Task 7: Update the bundled `actions/cache/action.yml` description

**Files:**
- Modify: `actions/cache/action.yml`

- [ ] **Step 1: Update the path: input description**

Replace:

```yaml
  path:
    description: 'Paths to cache, relative to workspace. One per line.'
    required: true
```

with:

```yaml
  path:
    description: |
      Paths to cache, one per line. Three shapes accepted:
        - workspace-relative: "target"
        - home-relative:      "~/.cargo/registry"  (expanded against $HOME in the runner)
        - absolute:           "/var/cache/apt"
      Mirrors the actions/cache@v4 path: shape.
    required: true
```

- [ ] **Step 2: Commit**

```
git add actions/cache/action.yml
git commit -m "actions/cache: document ~/ and absolute path support"
```

---

## Task 8: Full project verification

- [ ] **Step 1: Lint and test everything**

```
go test ./...
```

Expected: all green.

```
make lint
```

Expected: clean (or no new lint errors compared to baseline).

- [ ] **Step 2: Build images for end-to-end test (optional, only if local k3d is up)**

```
make build
```

Expected: `bin/controller` and `bin/entrypoint` produced.

If `./hack/dev-env.sh status` shows the cluster is up, run `./hack/dev-env.sh rebuild` and trigger the bevy_xr_nitro workflow (or any workflow with `~/.cargo/registry` in its cache path) to verify warm runs hit the snapshot. Per the bug report, warm cargo build should drop from ~4m to ~30s — the same speedup the workspace-relative workaround already demonstrated, this time without the workaround.

- [ ] **Step 3: Mark bug 009 resolved**

Append to `bugs/009-cache-paths-only-workspace-relative.md`:

```markdown

## Resolution

Fixed in commit <SHA>. `extractCacheInfo` now accepts `~/`-prefixed
and absolute paths, `mirrorOne` resolves them against `$HOME` /
absolute roots respectively, and bad paths (`..` traversal,
`$HOME` unset on `~/...`) cause visible job failures instead of
silent misses. See spec at
`docs/superpowers/specs/2026-05-03-cache-paths-home-and-absolute-design.md`.
```

```
git add bugs/009-cache-paths-only-workspace-relative.md
git commit -m "bugs: mark 009 resolved (cache paths)"
```

---

## Self-review summary

- **Spec coverage:** Path semantics (Task 1, 2, 3), error handling (Task 4, 5, 6), `actions/cache/action.yml` description (Task 7), no schema change (verified — `Manifest.CachePaths` untouched), snapshot scope unchanged (verified — no snapshot package edits in any task).
- **Placeholder scan:** No TBDs. One semi-soft instruction in Task 5 Step 2 about checking `StateEvent` field name; this is honest research-not-yet-done rather than a placeholder, and the grep command is provided. Task 6 Step 4 has a similar honest note about confirming the surrounding control-flow keyword (`continue` vs `return`) — necessary because the surrounding code wasn't fully read during planning, and the verification is a one-line grep.
- **Type consistency:** `mirrorOne(ws, cache, home, rel)` signature is consistent across Tasks 1–4. `extractCacheInfo` returns `(key, paths, restoreKeys, error)` consistently in Task 6 and tests. `errHomeRequired` referenced by name in Tasks 1, 2, and 4 — defined in Task 1.
