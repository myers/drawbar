# Actions cache HTTP fetch implementation plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace the shared actions-cache PVC mount in job pods with an HTTP fetch from the controller's cache server into an emptyDir, fixing bug 001 (RWO storage rejects the second mount).

**Architecture:** The cache server gains a single new route `GET /_apis/actions/:dir/tar` that streams a tar of `cfg.Cache.Dir/actions-repo-cache/<dir>` (excluding `.git/`). The job manifest gains an `Actions []ActionFetch` field. A new `entrypoint setup` subcommand reads the manifest and HTTP-fetches each tarball into `/actions/<dir>/` before the runner container starts. Job pods stop referencing the controller's cache PVC entirely.

**Tech Stack:** Go 1.x, `archive/tar` stdlib, `net/http` stdlib, `httprouter` (already in pkg/cache), `client-go` for k8s types (already in pkg/k8s), Kubernetes Job/Pod specs, Helm.

**Spec:** [`docs/superpowers/specs/2026-04-30-actions-cache-http-fetch-design.md`](../specs/2026-04-30-actions-cache-http-fetch-design.md)

**Bug:** [`bugs/001-actions-cache-pvc-rwo-prevents-job-pod-mount.md`](../../../bugs/001-actions-cache-pvc-rwo-prevents-job-pod-mount.md)

---

## File map

**Create:**
- `pkg/cache/tar.go` — `tarDir` helper that streams a tar of a directory excluding named top-level entries.
- `pkg/cache/tar_test.go` — table-driven tests for `tarDir`.
- `cmd/entrypoint/setup.go` — `runSetup` function (the `setup` subcommand body).
- `cmd/entrypoint/setup_test.go` — tests using `httptest.Server` for the fetch loop.

**Modify:**
- `pkg/types/step.go` — add `ActionFetch` type and `Actions` field to `Manifest`.
- `pkg/cache/handler.go` — register new route and add `serveAction` handler.
- `pkg/cache/handler_test.go` — add tests for the new route.
- `pkg/k8s/builder.go` — drop the actions-cache PVC volume + subPath mounts; add `actions` emptyDir + mounts on setup-shim and runner; rewrite `setupCmd` to invoke `entrypoint setup`; delete `JobConfig.CachePVCName`.
- `pkg/k8s/builder_test.go` — rewrite the PVC-mount assertion (~lines 102-125 today) to assert the new emptyDir shape and manifest contents.
- `cmd/entrypoint/main.go` — add subcommand dispatch; existing single-arg form is removed.
- `cmd/entrypoint/main_test.go` — update test invocations to use the `run` subcommand form.
- `cmd/controller/main.go` — populate `Manifest.Actions` (or `JobConfig.Actions`) at job-build time; drop `jobCachePVCName` plumbing; build the per-action URLs from the existing `CACHE_SERVICE_NAME`-derived URL.
- `pkg/config/config.go` — delete `CacheConfig.PVCName` field and the `CACHE_PVC_NAME` env override.
- `pkg/config/config_test.go` — drop any tests that reference `PVCName`.
- `deploy/helm/drawbar/templates/deployment.yaml` — drop the `CACHE_PVC_NAME` env var (lines 45-46).

**No changes:**
- `deploy/helm/drawbar/values.yaml` — `cache.*` block is correct as-is.
- `deploy/helm/drawbar/templates/pvc.yaml` — single RWO PVC, mounted only by controller, correct as-is.
- `pkg/snapshot/*` — workspace snapshot cache untouched.
- `pkg/cache/db.go`, `pkg/cache/storage.go` — artifact cache (SQLite + filesystem) untouched.

---

## Task 1: Add `ActionFetch` type to manifest schema

**Files:**
- Modify: `pkg/types/step.go:25-30`

- [ ] **Step 1: Add the `ActionFetch` type and `Actions` field**

Replace the current `Manifest` definition (lines 25-30) with:

```go
// Manifest is the JSON structure the entrypoint binary reads.
type Manifest struct {
	Steps   []ManifestStep    `json:"steps"`
	BaseEnv map[string]string `json:"baseEnv"`
	Context *EvalContext      `json:"context,omitempty"`
	Actions []ActionFetch     `json:"actions,omitempty"`
}

// ActionFetch tells the setup subcommand to fetch an action tarball from the
// cache server and unpack it into /actions/<Dir>/ before the runner starts.
type ActionFetch struct {
	Dir string `json:"dir"` // sanitized action dir name, e.g. "actions-checkout-v4"
	URL string `json:"url"` // full URL, e.g. "http://drawbar-cache:9300/_apis/actions/actions-checkout-v4/tar"
}
```

- [ ] **Step 2: Verify the package still compiles**

Run: `go build ./pkg/types/...`
Expected: success, no output.

- [ ] **Step 3: Commit**

```bash
git add pkg/types/step.go
git commit -m "types: add ActionFetch and Manifest.Actions for HTTP-fetched actions"
```

---

## Task 2: `tarDir` helper — failing test

**Files:**
- Create: `pkg/cache/tar_test.go`

- [ ] **Step 1: Write the failing tests**

Create `pkg/cache/tar_test.go`:

```go
package cache

import (
	"archive/tar"
	"bytes"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// readTarEntries returns a map of name -> body for every regular-file entry in the tar stream.
// Symlinks are recorded as name -> "@" + linkname.
func readTarEntries(t *testing.T, r io.Reader) map[string]string {
	t.Helper()
	tr := tar.NewReader(r)
	out := map[string]string{}
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		require.NoError(t, err)
		switch hdr.Typeflag {
		case tar.TypeReg:
			body, err := io.ReadAll(tr)
			require.NoError(t, err)
			out[hdr.Name] = string(body)
		case tar.TypeSymlink:
			out[hdr.Name] = "@" + hdr.Linkname
		}
	}
	return out
}

func TestTarDir_HappyPath(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "dist"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(root, "action.yml"), []byte("name: x"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(root, "dist", "index.js"), []byte("console.log(1)"), 0o644))

	var buf bytes.Buffer
	require.NoError(t, tarDir(&buf, root, nil))

	entries := readTarEntries(t, &buf)
	assert.Equal(t, "name: x", entries["action.yml"])
	assert.Equal(t, "console.log(1)", entries["dist/index.js"])
}

func TestTarDir_ExcludesTopLevelGit(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, ".git"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(root, ".git", "HEAD"), []byte("ref: x"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(root, "action.yml"), []byte("name: x"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(root, ".gitignore"), []byte("node_modules/"), 0o644))

	var buf bytes.Buffer
	require.NoError(t, tarDir(&buf, root, []string{".git"}))

	entries := readTarEntries(t, &buf)
	_, hasGitHead := entries[".git/HEAD"]
	assert.False(t, hasGitHead, ".git/ contents must be excluded")
	assert.Equal(t, "name: x", entries["action.yml"])
	assert.Equal(t, "node_modules/", entries[".gitignore"], ".gitignore must NOT be excluded (only top-level .git matches)")
}

func TestTarDir_SymlinksTarredAsSymlinks(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "real.txt"), []byte("hi"), 0o644))
	require.NoError(t, os.Symlink("real.txt", filepath.Join(root, "link.txt")))

	var buf bytes.Buffer
	require.NoError(t, tarDir(&buf, root, nil))

	entries := readTarEntries(t, &buf)
	assert.Equal(t, "@real.txt", entries["link.txt"], "symlinks should be tarred as TypeSymlink, not followed")
}

func TestTarDir_EmptyDir(t *testing.T) {
	root := t.TempDir()

	var buf bytes.Buffer
	require.NoError(t, tarDir(&buf, root, nil))

	entries := readTarEntries(t, &buf)
	assert.Empty(t, entries)
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./pkg/cache/ -run TestTarDir -v`
Expected: FAIL with "undefined: tarDir" (or similar build error).

---

## Task 3: `tarDir` helper — implementation

**Files:**
- Create: `pkg/cache/tar.go`

- [ ] **Step 1: Implement `tarDir`**

Create `pkg/cache/tar.go`:

```go
package cache

import (
	"archive/tar"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// tarDir streams a tar archive of root into w. Top-level entries whose name is
// in excludes (e.g. ".git") are skipped entirely along with their contents.
// Symbolic links are emitted as tar.TypeSymlink (not followed). The archive
// uses paths relative to root, with forward slashes.
func tarDir(w io.Writer, root string, excludes []string) error {
	tw := tar.NewWriter(w)
	defer tw.Close()

	excludeSet := map[string]struct{}{}
	for _, e := range excludes {
		excludeSet[e] = struct{}{}
	}

	root = filepath.Clean(root)

	return filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path == root {
			return nil // skip the root itself
		}

		rel, err := filepath.Rel(root, path)
		if err != nil {
			return fmt.Errorf("rel %s: %w", path, err)
		}
		// Match excludes only against the top-level component.
		top := rel
		if i := strings.IndexByte(rel, filepath.Separator); i >= 0 {
			top = rel[:i]
		}
		if _, skip := excludeSet[top]; skip {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}

		info, err := d.Info()
		if err != nil {
			return fmt.Errorf("stat %s: %w", path, err)
		}

		var linkTarget string
		if info.Mode()&os.ModeSymlink != 0 {
			linkTarget, err = os.Readlink(path)
			if err != nil {
				return fmt.Errorf("readlink %s: %w", path, err)
			}
		}

		hdr, err := tar.FileInfoHeader(info, linkTarget)
		if err != nil {
			return fmt.Errorf("header %s: %w", path, err)
		}
		hdr.Name = filepath.ToSlash(rel)
		if d.IsDir() {
			hdr.Name += "/"
		}

		if err := tw.WriteHeader(hdr); err != nil {
			return fmt.Errorf("write header %s: %w", path, err)
		}

		if info.Mode().IsRegular() {
			f, err := os.Open(path)
			if err != nil {
				return fmt.Errorf("open %s: %w", path, err)
			}
			_, copyErr := io.Copy(tw, f)
			f.Close()
			if copyErr != nil {
				return fmt.Errorf("copy %s: %w", path, copyErr)
			}
		}
		return nil
	})
}
```

- [ ] **Step 2: Run the tests to verify they pass**

Run: `go test ./pkg/cache/ -run TestTarDir -v`
Expected: PASS for all four subtests.

- [ ] **Step 3: Commit**

```bash
git add pkg/cache/tar.go pkg/cache/tar_test.go
git commit -m "cache: add tarDir helper streaming a directory as tar with top-level excludes"
```

---

## Task 4: New cache route `serveAction` — failing test

**Files:**
- Modify: `pkg/cache/handler_test.go`

- [ ] **Step 1: Inspect the existing test file to match its style**

Run: `head -40 /Users/myers/p/drawbar/pkg/cache/handler_test.go`

Note the helpers and imports it uses; mirror them.

- [ ] **Step 2: Append failing tests for the new route**

Add to the bottom of `pkg/cache/handler_test.go`:

```go
func TestServeAction_HappyPath(t *testing.T) {
	dir := t.TempDir()
	actionRoot := filepath.Join(dir, "actions-repo-cache", "actions-checkout-v4")
	require.NoError(t, os.MkdirAll(filepath.Join(actionRoot, "dist"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(actionRoot, "action.yml"), []byte("name: checkout"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(actionRoot, "dist", "index.js"), []byte("body"), 0o644))
	require.NoError(t, os.MkdirAll(filepath.Join(actionRoot, ".git"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(actionRoot, ".git", "HEAD"), []byte("ref"), 0o644))

	h, err := StartHandler(dir, "127.0.0.1", 0)
	require.NoError(t, err)
	defer h.Close()

	url := h.ExternalURL() + "/_apis/actions/actions-checkout-v4/tar"
	resp, err := http.Get(url)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, "application/x-tar", resp.Header.Get("Content-Type"))

	entries := readTarEntries(t, resp.Body)
	assert.Equal(t, "name: checkout", entries["action.yml"])
	assert.Equal(t, "body", entries["dist/index.js"])
	_, hasGitHead := entries[".git/HEAD"]
	assert.False(t, hasGitHead, ".git/ must be excluded from the response")
}

func TestServeAction_NotFound(t *testing.T) {
	dir := t.TempDir()
	h, err := StartHandler(dir, "127.0.0.1", 0)
	require.NoError(t, err)
	defer h.Close()

	resp, err := http.Get(h.ExternalURL() + "/_apis/actions/does-not-exist/tar")
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
}

func TestServeAction_RejectsInvalidName(t *testing.T) {
	dir := t.TempDir()
	h, err := StartHandler(dir, "127.0.0.1", 0)
	require.NoError(t, err)
	defer h.Close()

	// Names containing slashes won't match :dir but routing variants might;
	// dot/underscore should be rejected by the validation.
	for _, name := range []string{"foo.bar", "foo_bar", "foo bar"} {
		resp, err := http.Get(h.ExternalURL() + "/_apis/actions/" + name + "/tar")
		require.NoError(t, err, "name=%q", name)
		resp.Body.Close()
		assert.Equal(t, http.StatusBadRequest, resp.StatusCode, "name=%q should be rejected", name)
	}
}
```

You will need these imports at the top of `handler_test.go` (add any missing): `"net/http"`, `"os"`, `"path/filepath"`, `"testing"`, `"github.com/stretchr/testify/assert"`, `"github.com/stretchr/testify/require"`.

- [ ] **Step 3: Run the tests to verify they fail**

Run: `go test ./pkg/cache/ -run TestServeAction -v`
Expected: FAIL — happy path returns 404 (route not registered) and validation test gets 404 from the router.

---

## Task 5: New cache route `serveAction` — implementation

**Files:**
- Modify: `pkg/cache/handler.go:85-92` (router setup), and append a new handler method.

- [ ] **Step 1: Register the new route**

In `pkg/cache/handler.go`, find the router-setup block (currently around lines 85-92) and add the new route:

```go
	router := httprouter.New()
	router.GET(urlBase+"/cache", h.middleware(h.find))
	router.POST(urlBase+"/caches", h.middleware(h.reserve))
	router.PATCH(urlBase+"/caches/:id", h.middleware(h.upload))
	router.POST(urlBase+"/caches/:id", h.middleware(h.commit))
	router.GET(urlBase+"/artifacts/:id", h.middleware(h.get))
	router.POST(urlBase+"/clean", h.middleware(h.clean))
	router.GET("/_apis/actions/:dir/tar", h.middleware(h.serveAction))  // NEW
	h.router = router
```

- [ ] **Step 2: Add the `serveAction` handler and validator**

Append to `pkg/cache/handler.go`:

```go
// GET /_apis/actions/:dir/tar
func (h *Handler) serveAction(w http.ResponseWriter, r *http.Request, params httprouter.Params) {
	dir := params.ByName("dir")
	if !isSafeActionDir(dir) {
		http.Error(w, "invalid action dir", http.StatusBadRequest)
		return
	}
	actionPath := filepath.Join(h.dir, "actions-repo-cache", dir)
	info, err := os.Stat(actionPath)
	if err != nil || !info.IsDir() {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/x-tar")
	if err := tarDir(w, actionPath, []string{".git"}); err != nil {
		// Headers are already written; we can't switch to JSON error here.
		// Log and let the connection drop with a truncated tar — the client
		// will see archive/tar.ErrHeader on parse and retry.
		slog.Error("serve action tar", "dir", dir, "error", err)
	}
}

// isSafeActionDir validates that dir is a single component matching the charset
// produced by ActionRef.ActionDir(): lowercase letters, digits, and dash. No
// slashes, dots, underscores, or other punctuation.
func isSafeActionDir(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= '0' && r <= '9':
		case r == '-':
		default:
			return false
		}
	}
	return true
}
```

- [ ] **Step 3: Run the tests to verify they pass**

Run: `go test ./pkg/cache/ -run TestServeAction -v`
Expected: PASS for all three subtests.

- [ ] **Step 4: Run the full cache package tests to confirm nothing else broke**

Run: `go test ./pkg/cache/ -v`
Expected: PASS for all tests.

- [ ] **Step 5: Commit**

```bash
git add pkg/cache/handler.go pkg/cache/handler_test.go
git commit -m "cache: add GET /_apis/actions/:dir/tar route serving action sources"
```

---

## Task 6: `entrypoint setup` subcommand — failing test

**Files:**
- Create: `cmd/entrypoint/setup_test.go`

- [ ] **Step 1: Write the failing tests**

Create `cmd/entrypoint/setup_test.go`:

```go
package main

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/myers/drawbar/pkg/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// makeTar returns the bytes of a tar containing the given name -> body pairs.
func makeTar(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for name, body := range files {
		require.NoError(t, tw.WriteHeader(&tar.Header{
			Name:     name,
			Size:     int64(len(body)),
			Mode:     0o644,
			Typeflag: tar.TypeReg,
		}))
		_, err := tw.Write([]byte(body))
		require.NoError(t, err)
	}
	require.NoError(t, tw.Close())
	return buf.Bytes()
}

// writeManifest writes a manifest with the given Actions and returns its path.
func writeManifest(t *testing.T, dir string, actions []types.ActionFetch) string {
	t.Helper()
	m := types.Manifest{Actions: actions}
	data, err := json.Marshal(m)
	require.NoError(t, err)
	path := filepath.Join(dir, "manifest.json")
	require.NoError(t, os.WriteFile(path, data, 0o644))
	return path
}

func TestRunSetup_HappyPath(t *testing.T) {
	tarBytes := makeTar(t, map[string]string{
		"action.yml":    "name: foo",
		"dist/index.js": "console.log(1)",
	})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-tar")
		w.Write(tarBytes)
	}))
	defer srv.Close()

	tmp := t.TempDir()
	actionsDir := filepath.Join(tmp, "actions")
	manifestPath := writeManifest(t, tmp, []types.ActionFetch{
		{Dir: "foo-bar", URL: srv.URL + "/_apis/actions/foo-bar/tar"},
	})

	err := runSetup(manifestPath, actionsDir)
	require.NoError(t, err)

	body, err := os.ReadFile(filepath.Join(actionsDir, "foo-bar", "action.yml"))
	require.NoError(t, err)
	assert.Equal(t, "name: foo", string(body))

	body, err = os.ReadFile(filepath.Join(actionsDir, "foo-bar", "dist", "index.js"))
	require.NoError(t, err)
	assert.Equal(t, "console.log(1)", string(body))
}

func TestRunSetup_RetriesOn5xx(t *testing.T) {
	tarBytes := makeTar(t, map[string]string{"action.yml": "ok"})
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) < 2 {
			http.Error(w, "boom", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/x-tar")
		w.Write(tarBytes)
	}))
	defer srv.Close()

	// Disable backoff for tests via the package-level hook.
	oldBackoff := setupRetryBackoff
	setupRetryBackoff = 0
	defer func() { setupRetryBackoff = oldBackoff }()

	tmp := t.TempDir()
	actionsDir := filepath.Join(tmp, "actions")
	manifestPath := writeManifest(t, tmp, []types.ActionFetch{
		{Dir: "foo", URL: srv.URL},
	})

	err := runSetup(manifestPath, actionsDir)
	require.NoError(t, err)
	assert.Equal(t, int32(2), calls.Load(), "should have retried once after the 503")
}

func TestRunSetup_FailsAfterMaxRetries(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	oldBackoff := setupRetryBackoff
	setupRetryBackoff = 0
	defer func() { setupRetryBackoff = oldBackoff }()

	tmp := t.TempDir()
	actionsDir := filepath.Join(tmp, "actions")
	manifestPath := writeManifest(t, tmp, []types.ActionFetch{
		{Dir: "foo", URL: srv.URL},
	})

	err := runSetup(manifestPath, actionsDir)
	assert.Error(t, err)
}

func TestRunSetup_FailsFastOn404(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.Error(w, "no", http.StatusNotFound)
	}))
	defer srv.Close()

	tmp := t.TempDir()
	actionsDir := filepath.Join(tmp, "actions")
	manifestPath := writeManifest(t, tmp, []types.ActionFetch{
		{Dir: "foo", URL: srv.URL},
	})

	err := runSetup(manifestPath, actionsDir)
	assert.Error(t, err)
	assert.Equal(t, int32(1), calls.Load(), "404 must not be retried")
}

func TestRunSetup_RejectsTarTraversal(t *testing.T) {
	tarBytes := makeTar(t, map[string]string{
		"../escape.txt": "evil",
	})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-tar")
		w.Write(tarBytes)
	}))
	defer srv.Close()

	tmp := t.TempDir()
	actionsDir := filepath.Join(tmp, "actions")
	manifestPath := writeManifest(t, tmp, []types.ActionFetch{
		{Dir: "foo", URL: srv.URL},
	})

	err := runSetup(manifestPath, actionsDir)
	assert.Error(t, err, "path traversal must be rejected")

	// The escape file must not have been written.
	parent := filepath.Dir(actionsDir)
	_, statErr := os.Stat(filepath.Join(parent, "escape.txt"))
	assert.True(t, os.IsNotExist(statErr), "no file should escape the actions dir")
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./cmd/entrypoint/ -run TestRunSetup -v`
Expected: FAIL with "undefined: runSetup" (and `setupRetryBackoff`).

---

## Task 7: `entrypoint setup` subcommand — implementation

**Files:**
- Create: `cmd/entrypoint/setup.go`

- [ ] **Step 1: Implement `runSetup`**

Create `cmd/entrypoint/setup.go` with the following content:

```go
package main

import (
	"archive/tar"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/myers/drawbar/pkg/types"
)

// setupRetryBackoff is the base backoff between retries. Tests set this to 0.
var setupRetryBackoff = 1 * time.Second

const (
	setupHTTPTimeout = 30 * time.Second
	setupMaxAttempts = 3
)

// runSetup reads a manifest from manifestPath and downloads each
// manifest.Actions tarball into actionsDir/<Dir>/.
func runSetup(manifestPath, actionsDir string) error {
	manifest, err := loadManifest(manifestPath)
	if err != nil {
		return err
	}

	if err := os.MkdirAll(actionsDir, 0o755); err != nil {
		return fmt.Errorf("creating actions dir: %w", err)
	}

	for _, a := range manifest.Actions {
		if err := fetchAction(a, actionsDir); err != nil {
			return fmt.Errorf("action %s: %w", a.Dir, err)
		}
	}
	return nil
}

// errFetchNotFound is the sentinel returned when the server returned 404.
// It is not retried.
var errFetchNotFound = errors.New("not found")

// fetchAction downloads a single action tarball into actionsDir/<a.Dir>/.
// Retries on 5xx and network errors; bails immediately on 404.
func fetchAction(a types.ActionFetch, actionsDir string) error {
	target := filepath.Join(actionsDir, a.Dir)
	if err := os.MkdirAll(target, 0o755); err != nil {
		return fmt.Errorf("mkdir target: %w", err)
	}

	client := &http.Client{Timeout: setupHTTPTimeout}

	var lastErr error
	for attempt := 1; attempt <= setupMaxAttempts; attempt++ {
		err := tryFetch(client, a.URL, target)
		if err == nil {
			return nil
		}
		if errors.Is(err, errFetchNotFound) {
			return err // do not retry
		}
		lastErr = err
		if attempt < setupMaxAttempts {
			time.Sleep(time.Duration(attempt) * setupRetryBackoff)
		}
	}
	return fmt.Errorf("after %d attempts: %w", setupMaxAttempts, lastErr)
}

func tryFetch(client *http.Client, url, target string) error {
	resp, err := client.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusOK:
		return untarInto(resp.Body, target)
	case resp.StatusCode == http.StatusNotFound:
		return errFetchNotFound
	default:
		return fmt.Errorf("http status %d", resp.StatusCode)
	}
}

// untarInto extracts a tar stream into target, rejecting any entry whose
// cleaned path would escape target.
func untarInto(r io.Reader, target string) error {
	target = filepath.Clean(target) + string(filepath.Separator)
	tr := tar.NewReader(r)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("tar next: %w", err)
		}

		clean := filepath.Clean("/" + hdr.Name) // anchored at root, removes ../
		dest := filepath.Join(target, strings.TrimPrefix(clean, "/"))
		// Defense in depth: ensure dest is still inside target.
		if !strings.HasPrefix(dest+string(filepath.Separator), target) && dest != strings.TrimSuffix(target, string(filepath.Separator)) {
			return fmt.Errorf("tar entry escapes target: %q", hdr.Name)
		}

		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(dest, 0o755); err != nil {
				return fmt.Errorf("mkdir %s: %w", dest, err)
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
				return fmt.Errorf("mkdir parent %s: %w", dest, err)
			}
			f, err := os.OpenFile(dest, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, os.FileMode(hdr.Mode)&0o777)
			if err != nil {
				return fmt.Errorf("create %s: %w", dest, err)
			}
			if _, err := io.Copy(f, tr); err != nil {
				f.Close()
				return fmt.Errorf("write %s: %w", dest, err)
			}
			f.Close()
		case tar.TypeSymlink:
			if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
				return fmt.Errorf("mkdir parent %s: %w", dest, err)
			}
			// Reject absolute or escape-y symlink targets.
			if filepath.IsAbs(hdr.Linkname) || strings.Contains(hdr.Linkname, "..") {
				return fmt.Errorf("symlink target unsafe: %q -> %q", hdr.Name, hdr.Linkname)
			}
			if err := os.Symlink(hdr.Linkname, dest); err != nil {
				return fmt.Errorf("symlink %s: %w", dest, err)
			}
		default:
			// Skip pax headers, devices, fifos, etc.
		}
	}
}
```

- [ ] **Step 2: Run the tests to verify they pass**

Run: `go test ./cmd/entrypoint/ -run TestRunSetup -v`
Expected: PASS for all five subtests.

- [ ] **Step 3: Run the full entrypoint package tests to confirm nothing else broke**

Run: `go test ./cmd/entrypoint/ -v`
Expected: PASS for all tests (the existing ones don't touch `runSetup`).

- [ ] **Step 4: Commit**

```bash
git add cmd/entrypoint/setup.go cmd/entrypoint/setup_test.go
git commit -m "entrypoint: add runSetup that fetches action tarballs into /actions/<dir>/"
```

---

## Task 8: Subcommand dispatch in entrypoint `main`

**Files:**
- Modify: `cmd/entrypoint/main.go:21-30` (the `main` function)
- Modify: `cmd/entrypoint/main_test.go` (callers of the binary, if any)

- [ ] **Step 1: Replace the `main` function**

In `cmd/entrypoint/main.go`, replace the `main` function (currently lines 21-30):

```go
func main() {
	if len(os.Args) < 2 {
		usage()
	}
	switch os.Args[1] {
	case "setup":
		if len(os.Args) < 3 {
			usage()
		}
		if err := runSetup(os.Args[2], "/actions"); err != nil {
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
}

func usage() {
	fmt.Fprintf(os.Stderr, "usage:\n")
	fmt.Fprintf(os.Stderr, "  entrypoint setup <manifest.json>   # init: fetch action sources into /actions/\n")
	fmt.Fprintf(os.Stderr, "  entrypoint run <manifest.json>     # runner: execute steps\n")
	os.Exit(1)
}
```

- [ ] **Step 2: Update `main_test.go` callers (if any)**

Run: `grep -n "runEntrypoint\|os.Args" cmd/entrypoint/main_test.go`

If any test invokes `main()` directly with a manifest path as the only arg, update it to call `runEntrypoint` directly (which already takes the manifest path, so most tests should already do this). Do not add backward-compat to `main` itself.

- [ ] **Step 3: Run all entrypoint tests**

Run: `go test ./cmd/entrypoint/ -v`
Expected: PASS for all tests.

- [ ] **Step 4: Verify the binary builds**

Run: `make build-entrypoint`
Expected: success, `bin/entrypoint` produced.

- [ ] **Step 5: Smoke-test the new CLI**

Run: `./bin/entrypoint`
Expected: usage message, exit 1.

Run: `./bin/entrypoint setup` (no manifest path)
Expected: usage message, exit 1.

- [ ] **Step 6: Commit**

```bash
git add cmd/entrypoint/main.go cmd/entrypoint/main_test.go
git commit -m "entrypoint: dispatch on setup/run subcommands"
```

---

## Task 9: Builder — failing tests for new pod shape

**Files:**
- Modify: `pkg/k8s/builder_test.go:80-125` (rewriting the `actions-cache` PVC test)

- [ ] **Step 1: Replace the existing `actions-cache` PVC assertion test**

The test currently at `pkg/k8s/builder_test.go:80-125` (search for `TestBuildJob` whose body asserts `actions-cache` PVC presence) should be rewritten. Locate it by:

```bash
grep -n "actions-cache" pkg/k8s/builder_test.go
```

Replace the body of the test (and the `JobConfig` it builds) with one that asserts the new shape. The replacement test:

```go
func TestBuildJob_ActionsEmptyDirAndManifestActions(t *testing.T) {
	cfg := JobConfig{
		TaskID:          1,
		Namespace:       "default",
		Image:           "node:24-trixie",
		ControllerImage: "runner:latest",
		Steps: []types.StepSpec{
			{ID: "checkout", Name: "actions/checkout", Args: []string{"node", "/actions/actions-checkout-v4/dist/index.js"}, ActionDir: "actions-checkout-v4"},
		},
		Actions: []types.ActionFetch{
			{Dir: "actions-checkout-v4", URL: "http://drawbar-cache:9300/_apis/actions/actions-checkout-v4/tar"},
		},
	}

	job, err := BuildJob(cfg)
	require.NoError(t, err)

	// Pod should have an `actions` emptyDir volume and NO actions-cache PVC.
	var actionsVol *corev1.Volume
	for i := range job.Spec.Template.Spec.Volumes {
		v := &job.Spec.Template.Spec.Volumes[i]
		if v.Name == "actions" {
			actionsVol = v
		}
		assert.NotEqual(t, "actions-cache", v.Name, "actions-cache PVC must not be present in the pod spec")
		if v.PersistentVolumeClaim != nil {
			// The only PVC the pod may reference is the snapshot cache (only when
			// configured). It must never be the controller's cache PVC.
			assert.NotEqual(t, "runner-cache", v.PersistentVolumeClaim.ClaimName)
		}
	}
	require.NotNil(t, actionsVol, "actions emptyDir volume must be present")
	require.NotNil(t, actionsVol.EmptyDir, "actions volume must be emptyDir, not PVC")

	// Setup-shim init container must mount /actions (write).
	var setupShim *corev1.Container
	for i := range job.Spec.Template.Spec.InitContainers {
		c := &job.Spec.Template.Spec.InitContainers[i]
		if c.Name == "setup-shim" {
			setupShim = c
		}
	}
	require.NotNil(t, setupShim, "setup-shim init container must exist")
	foundShimMount := false
	for _, m := range setupShim.VolumeMounts {
		if m.Name == "actions" && m.MountPath == "/actions" {
			foundShimMount = true
			assert.False(t, m.ReadOnly, "setup-shim must mount /actions read-write")
		}
	}
	assert.True(t, foundShimMount, "setup-shim must mount /actions")

	// Runner must mount /actions read-only.
	runner := job.Spec.Template.Spec.Containers[0]
	foundRunnerMount := false
	for _, m := range runner.VolumeMounts {
		if m.Name == "actions" && m.MountPath == "/actions" {
			foundRunnerMount = true
			assert.True(t, m.ReadOnly, "runner must mount /actions read-only")
		}
		assert.NotEqual(t, "actions-cache", m.Name, "runner must not have actions-cache mounts")
	}
	assert.True(t, foundRunnerMount, "runner must mount /actions")

	// Manifest JSON injected into setup-shim args must contain the Actions field.
	require.NotEmpty(t, setupShim.Args, "setup-shim must have args (the heredoc shell)")
	shellScript := setupShim.Args[0]
	assert.Contains(t, shellScript, `"actions":[`)
	assert.Contains(t, shellScript, "actions-checkout-v4")
	assert.Contains(t, shellScript, "/_apis/actions/actions-checkout-v4/tar")

	// Setup-shim must invoke `entrypoint setup`.
	assert.Contains(t, shellScript, "/shim/entrypoint setup /shim/manifest.json")
}
```

This replaces the old `TestBuildJob_*` whose body asserts the PVC. Keep all other tests in `builder_test.go` untouched. If a test references the now-deleted `JobConfig.CachePVCName`, remove that field from its `JobConfig` literal.

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./pkg/k8s/ -run TestBuildJob_ActionsEmptyDirAndManifestActions -v`
Expected: FAIL — current code produces `actions-cache` PVC, not `actions` emptyDir; `JobConfig.Actions` field doesn't exist.

---

## Task 10: Builder — implement the new pod shape

**Files:**
- Modify: `pkg/k8s/builder.go:36-53` (`JobConfig`)
- Modify: `pkg/k8s/builder.go:87-123` (volumes section)
- Modify: `pkg/k8s/builder.go:194-209` (setup-shim init container)
- Modify: `pkg/k8s/builder.go:211-229` (runner mounts)

- [ ] **Step 1: Update `JobConfig`**

In `pkg/k8s/builder.go`, replace the `JobConfig` struct (currently `:36-53`):

```go
// JobConfig describes how to build a k8s Job.
type JobConfig struct {
	TaskID          int64
	RunID           string
	JobName         string
	Namespace       string
	Image           string // job container image (from label resolution)
	ControllerImage string // controller image (for shim injection)
	Steps           []types.StepSpec
	BaseEnv         map[string]string // env vars injected into all steps
	Services        []ServiceSpec
	Timeout         int64                // ActiveDeadlineSeconds
	Actions         []types.ActionFetch  // actions for setup-shim to fetch into /actions/<Dir>/
	JobSecrets      []JobSecretMount     // k8s Secrets to mount into job pods
	EvalContext     *types.EvalContext   // evaluation context for runtime if: conditions
	SnapshotPVCName string               // PVC for ZFS snapshot cache (empty = disabled)
	SnapshotPaths   []string             // paths to bind-mount from snapshot PVC into /workspace
}
```

Note: `CachePVCName` is removed.

- [ ] **Step 2: Replace the volumes section**

Replace the volumes block (currently `:87-123`) with:

```go
	// Volumes. Workspace is always emptyDir (fresh clone each job).
	// Actions is always emptyDir, populated by setup-shim from the cache server.
	volumes := []corev1.Volume{
		{Name: "workspace", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
		{Name: "shim", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
		{Name: "actions", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
	}

	// ZFS snapshot cache PVC — bind-mount declared paths into /workspace.
	if cfg.SnapshotPVCName != "" && len(cfg.SnapshotPaths) > 0 {
		volumes = append(volumes, corev1.Volume{
			Name: "snapshot-cache",
			VolumeSource: corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
					ClaimName: cfg.SnapshotPVCName,
				},
			},
		})
	}
```

The old `actions-cache` PVC volume block (and its `hasActions` precheck) is deleted.

- [ ] **Step 3: Update `buildManifest` to include Actions**

Find `buildManifest` (currently `:302`). Update it to set `Actions`:

```go
func buildManifest(cfg JobConfig) types.Manifest {
	steps := make([]types.ManifestStep, 0, len(cfg.Steps))
	for _, s := range cfg.Steps {
		steps = append(steps, types.ManifestStep{
			ID:              s.ID,
			Name:            s.Name,
			Command:         s.Script,
			Args:            s.Args,
			Shell:           s.Shell,
			Env:             s.Env,
			WorkDir:         "/workspace",
			ContinueOnError: s.ContinueOnError,
			If:              s.If,
			TimeoutMinutes:  s.TimeoutMinutes,
		})
	}
	return types.Manifest{
		Steps:   steps,
		BaseEnv: cfg.BaseEnv,
		Context: cfg.EvalContext,
		Actions: cfg.Actions,
	}
}
```

- [ ] **Step 4: Replace the setup-shim init container**

Replace the `setupCmd` and `setup-shim` init container blocks (currently `:194-209`) with:

```go
	setupCmd := fmt.Sprintf(
		"cp /entrypoint /shim/entrypoint && chmod +x /shim/entrypoint && "+
			"cat > /shim/manifest.json << '%s'\n%s\n%s\n"+
			"exec /shim/entrypoint setup /shim/manifest.json",
		delimiter, string(manifestJSON), delimiter)

	initContainers = append(initContainers, corev1.Container{
		Name:            "setup-shim",
		Image:           controllerImage,
		Command:         []string{"/bin/sh", "-c"},
		Args:            []string{setupCmd},
		SecurityContext: containerSecurity,
		VolumeMounts: []corev1.VolumeMount{
			{Name: "shim", MountPath: "/shim"},
			{Name: "actions", MountPath: "/actions"},
		},
	})
```

The askpass shim (`/shim/askpass.sh`) is no longer written by the heredoc shell. If any other code path needs `askpass.sh`, write it inside `runSetup` instead. (Check by grepping: `grep -rn askpass.sh pkg/ cmd/`. If it is only referenced for git auth in scripts, add a one-liner in `runSetup` before fetching actions: write `/shim/askpass.sh` with the same content as today.)

To handle that cleanly, in `cmd/entrypoint/setup.go`'s `runSetup`, before the `for _, a := range manifest.Actions` loop, add:

```go
	if err := writeAskpassShim("/shim/askpass.sh"); err != nil {
		return fmt.Errorf("writing askpass shim: %w", err)
	}
```

And in the same file, add:

```go
func writeAskpassShim(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte("#!/bin/sh\necho \"$GIT_AUTH_TOKEN\"\n"), 0o755)
}
```

- [ ] **Step 5: Replace the runner mounts**

Replace the `runnerMounts` setup (currently `:211-229`) with:

```go
	// Runner container: executes all steps via entrypoint.
	runnerMounts := []corev1.VolumeMount{
		{Name: "workspace", MountPath: "/workspace"},
		{Name: "shim", MountPath: "/shim"},
		{Name: "actions", MountPath: "/actions", ReadOnly: true},
	}
```

The `mountedActions` loop and the `actions-cache` PVC subPath mounts are deleted.

- [ ] **Step 6: Update the runner Command**

The runner container currently invokes `/shim/entrypoint /shim/manifest.json`. With subcommand dispatch, it must now invoke `run`. Find the runner container definition (search for `Command: []string{"/shim/entrypoint"`) and change it to:

```go
		Command:         []string{"/shim/entrypoint", "run", "/shim/manifest.json"},
```

- [ ] **Step 7: Run the new test**

Run: `go test ./pkg/k8s/ -run TestBuildJob_ActionsEmptyDirAndManifestActions -v`
Expected: PASS.

- [ ] **Step 8: Run all builder tests**

Run: `go test ./pkg/k8s/ -v`
Expected: PASS for all tests. If others fail because they reference `JobConfig.CachePVCName`, drop the field from those literals.

- [ ] **Step 9: Run full test suite**

Run: `go test ./...`
Expected: PASS or only-fail-in-controller (which we'll fix in Task 11).

- [ ] **Step 10: Commit**

```bash
git add pkg/k8s/builder.go pkg/k8s/builder_test.go cmd/entrypoint/setup.go
git commit -m "k8s/builder: replace actions-cache PVC with emptyDir populated by setup subcommand"
```

---

## Task 11: Controller — populate `Actions` and drop PVC plumbing

**Files:**
- Modify: `cmd/controller/main.go:380-405` (the JobConfig type that the controller passes around — find via grep)
- Modify: `cmd/controller/main.go:425-540` (action collection and step building — `actionsToClone`)
- Modify: `cmd/controller/main.go:594-651` (job-build site that currently sets `CachePVCName`)

- [ ] **Step 1: Locate the controller-side JobConfig type**

Run: `grep -n "CachePVCName" cmd/controller/main.go pkg/k8s/builder.go`

There may be one or two definitions. Drop `CachePVCName` from any controller-side struct/config that holds it. Drop reads of the env var in `main`'s setup (search `CACHE_PVC_NAME`).

- [ ] **Step 2: Find the cache service URL the controller uses for `ACTIONS_CACHE_URL`**

Run: `grep -n "ACTIONS_CACHE_URL\|cacheURL\|CACHE_SERVICE_NAME" cmd/controller/main.go`

The controller already has the cache service URL in scope (used to set `ACTIONS_CACHE_URL` for jobs around line 580). Capture it as a local: `cacheServiceURL` (e.g. `http://drawbar-cache:9300`). It is the same URL that should be used for action fetches; we just append a different path per action.

- [ ] **Step 3: Build `[]types.ActionFetch` from `actionsToClone`**

Right before the `k8s.BuildJob(...)` call (currently around `:635`), add:

```go
	// Build ActionFetch list for setup-shim from the resolved actions.
	var actionFetches []types.ActionFetch
	if cacheServiceURL != "" {
		seen := map[string]bool{}
		for _, m := range actionsToClone {
			if m == nil || m.Dir == "" || seen[m.Dir] {
				continue
			}
			seen[m.Dir] = true
			actionFetches = append(actionFetches, types.ActionFetch{
				Dir: m.Dir,
				URL: fmt.Sprintf("%s/_apis/actions/%s/tar", strings.TrimRight(cacheServiceURL, "/"), m.Dir),
			})
		}
	}
```

(If `cacheServiceURL` is named differently in the file, use that name. If `strings` is not imported, add it.)

- [ ] **Step 4: Pass `Actions` into `BuildJob`**

Update the `k8s.BuildJob(k8s.JobConfig{...})` call (currently `:635-651`):

```go
		k8sJob, err := k8s.BuildJob(k8s.JobConfig{
			TaskID:          task.GetId(),
			RunID:           runID,
			JobName:         parsed.JobID,
			Namespace:       cfg.Namespace,
			Image:           image,
			ControllerImage: cfg.ControllerImage,
			Steps:           steps,
			BaseEnv:         baseEnv,
			Services:        services,
			Timeout:         timeoutSecs,
			Actions:         actionFetches,
			JobSecrets:      secretMounts,
			EvalContext:     evalCtx,
			SnapshotPVCName: snapshotPVCName,
			SnapshotPaths:   snapshotPaths,
		})
```

`CachePVCName` and the `jobCachePVCName` local are removed.

- [ ] **Step 5: Drop the unused `jobCachePVCName` block**

Delete the block that computes `jobCachePVCName` (currently `:594-599`):

```go
		// Determine cache PVC name.
		jobCachePVCName := ""
		if len(actionsToClone) > 0 {
			jobCachePVCName = cfg.CachePVCName
		}
```

- [ ] **Step 6: Run controller package tests**

Run: `go test ./cmd/controller/... -v`
Expected: PASS. If a test references `CachePVCName`, drop it.

- [ ] **Step 7: Run full test suite**

Run: `go test ./...`
Expected: PASS everywhere.

- [ ] **Step 8: Commit**

```bash
git add cmd/controller/main.go cmd/controller/main_test.go cmd/controller/handler_test.go cmd/controller/run_test.go
git commit -m "controller: build Actions list for setup-shim and drop CachePVCName plumbing"
```

(Stage only the files you actually modified.)

---

## Task 12: Drop `CACHE_PVC_NAME` from config and helm

**Files:**
- Modify: `pkg/config/config.go:47-53` (`CacheConfig`)
- Modify: `pkg/config/config.go:244-249` (env override block)
- Modify: `pkg/config/config_test.go` (any test that touches `PVCName`)
- Modify: `deploy/helm/drawbar/templates/deployment.yaml:45-46`

- [ ] **Step 1: Remove `PVCName` from `CacheConfig`**

In `pkg/config/config.go`, replace the `CacheConfig` struct (currently `:47-53`):

```go
type CacheConfig struct {
	Enabled     bool   `yaml:"enabled"`      // default: true
	Dir         string `yaml:"dir"`          // cache storage directory, default: /cache
	Port        uint16 `yaml:"port"`         // cache proxy listen port, default: 9300
	ServiceName string `yaml:"service_name"` // k8s Service name for cache (set via CACHE_SERVICE_NAME)
}
```

- [ ] **Step 2: Remove the `CACHE_PVC_NAME` env override**

In `pkg/config/config.go`, find and delete the block (currently `:247-249`):

```go
	if v := os.Getenv("CACHE_PVC_NAME"); v != "" {
		c.Cache.PVCName = v
	}
```

- [ ] **Step 3: Update config tests**

Run: `grep -n PVCName pkg/config/config_test.go`

Drop any assertion that touches `PVCName`. If a test fixture YAML includes `pvc_name`, leave the YAML (unknown fields are silently ignored) but drop the matching assertion.

- [ ] **Step 4: Drop `CACHE_PVC_NAME` from the Helm deployment template**

In `deploy/helm/drawbar/templates/deployment.yaml`, delete lines 45-46:

```yaml
            - name: CACHE_PVC_NAME
              value: "{{ include "drawbar.fullname" . }}-cache"
```

The `CACHE_SERVICE_NAME` env var (lines 43-44) stays.

- [ ] **Step 5: Verify config still compiles and tests pass**

Run: `go test ./pkg/config/... -v`
Expected: PASS.

- [ ] **Step 6: Verify the helm chart still templates**

Run: `helm template /Users/myers/p/drawbar/deploy/helm/drawbar/ > /dev/null`
Expected: success, no errors.

- [ ] **Step 7: Run full test suite to verify nothing references `PVCName`**

Run: `go test ./...`
Expected: PASS everywhere.

- [ ] **Step 8: Commit**

```bash
git add pkg/config/config.go pkg/config/config_test.go deploy/helm/drawbar/templates/deployment.yaml
git commit -m "config: remove CacheConfig.PVCName and CACHE_PVC_NAME env (unused after job-pod PVC removal)"
```

---

## Task 13: Update CLAUDE.md to reflect the new architecture

**Files:**
- Modify: `CLAUDE.md` (find and update the "three caches" section)

- [ ] **Step 1: Locate the section**

Run: `grep -n -A 15 "Three caches\|three caches\|Actions source cache" CLAUDE.md`

- [ ] **Step 2: Update the actions source cache description**

Update the "actions source cache" bullet (or paragraph) in `CLAUDE.md` to describe the new mechanism:

> **Actions source cache** — git clones of action repos at `cfg.Cache.Dir/actions-repo-cache/<dir>`. Mounted only by the controller. Job pods fetch the action contents at init time via HTTP from the cache server (`GET /_apis/actions/<dir>/tar`) and unpack into the pod-local `/actions` emptyDir. The init-container fetch logic lives in the `entrypoint setup` subcommand.

(Edit the wording to match the existing CLAUDE.md tone — terse, no marketing.)

If CLAUDE.md previously claimed the actions cache is shared via PVC, ensure that claim is gone.

- [ ] **Step 3: Commit**

```bash
git add CLAUDE.md
git commit -m "docs: update CLAUDE.md — actions source cache now served via HTTP, not shared PVC"
```

---

## Task 14: Manual cluster verification (no commit)

**Files:** none (this is a runbook task, not a code change)

This is the bug-001 acceptance test. It is run by hand against `gt.monoloco.net`.

- [ ] **Step 1: Build and push the image**

Run: `make image push`
Expected: image pushed to the registry the cluster pulls from.

- [ ] **Step 2: Upgrade drawbar via Helm**

Run: `helm upgrade drawbar /Users/myers/p/drawbar/deploy/helm/drawbar/ -n <namespace> --set image.tag=<new-tag>`
Expected: rollout succeeds, controller pod is `Running` and `Ready`.

- [ ] **Step 3: Push a workflow that uses `actions/checkout@v4`**

In a test repo with a `.forgejo/workflows/smoke.yml` like:

```yaml
on: push
jobs:
  smoke:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - run: ls
```

Push a commit. Wait for the run to start.

- [ ] **Step 4: Watch the job pod**

Run: `kubectl get pods -n <namespace> -l app.kubernetes.io/managed-by=drawbar -w`
Expected: pod transitions out of `Init:0/1` cleanly within ~10s.

- [ ] **Step 5: Verify no FailedMount events**

Run: `kubectl describe pod drawbar-run-N-XXXXX -n <namespace>`
Expected: no `FailedMount` events. No mention of `actions-cache` volume.

- [ ] **Step 6: Verify setup-shim succeeded**

Run: `kubectl logs drawbar-run-N-XXXXX -c setup-shim -n <namespace>`
Expected: no errors; manifest written; if any actions are in the manifest, fetch lines visible.

- [ ] **Step 7: Verify the workflow ran**

In the Forgejo UI for the run: status `success`, all steps green, the `actions/checkout@v4` step shows the cloned files.

- [ ] **Step 8: Verify a no-actions workflow still works**

Push a workflow with only `run:` steps (no `uses:`). Expected: runs normally; the empty `actions` emptyDir is harmless.

- [ ] **Step 9: Verify `actions/cache@v4` still works**

Push a workflow that uses `actions/cache@v4` to cache something cheap (e.g. an apt list). First run: cache miss, file uploaded. Second run: cache hit, file restored. This validates the artifact cache (the existing :9300 routes) is unaffected.

- [ ] **Step 10: Add a closing note to bug 001**

Edit `bugs/001-actions-cache-pvc-rwo-prevents-job-pod-mount.md` and append:

```markdown
## Resolution

Fixed by [`docs/superpowers/specs/2026-04-30-actions-cache-http-fetch-design.md`](../docs/superpowers/specs/2026-04-30-actions-cache-http-fetch-design.md). Job pods no longer mount the controller's cache PVC. Action sources are fetched over HTTP from the cache server at init time and unpacked into a pod-local emptyDir.

Verified on `gt.monoloco.net` with a workflow using `actions/checkout@v4` on `<date>` against drawbar `<sha>`.
```

- [ ] **Step 11: Commit the bug-doc update**

```bash
git add bugs/001-actions-cache-pvc-rwo-prevents-job-pod-mount.md
git commit -m "bugs: mark 001 resolved — actions cache served via HTTP into emptyDir"
```

---

## Self-review notes (kept for the executor)

- All `JobConfig.CachePVCName`, `CacheConfig.PVCName`, and `CACHE_PVC_NAME` references must be deleted; this is the compile-time regression guarantee.
- The runner container's command line changes from one arg to two (`run <manifest>`); confirm Task 10 step 6 was applied or jobs will fail at startup.
- The askpass shim moved from shell heredoc into `runSetup`; confirm it is still written before `runSteps` need it (it must exist before any step that uses git auth runs, which means before step execution starts — `runSetup` writes it before fetching actions, so timing is correct).
- The new cache route is on the same listener and namespace-reachable Service as the existing artifact cache; no Service or NetworkPolicy edits.
- Helm `values.yaml` is unchanged on purpose — the `cache.*` block correctly describes controller-only cache.
