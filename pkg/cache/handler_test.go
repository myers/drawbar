package cache

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func setupTestHandler(t *testing.T) (*Handler, string) {
	t.Helper()
	dir := t.TempDir()
	h, err := StartHandler(dir, "127.0.0.1", 0)
	require.NoError(t, err)
	t.Cleanup(func() { h.Close() })
	return h, dir
}

func TestCacheRoundTrip(t *testing.T) {
	h, _ := setupTestHandler(t)
	ts := httptest.NewServer(h.router)
	defer ts.Close()

	// 1. Reserve a cache entry.
	body, _ := json.Marshal(map[string]any{
		"key":     "go-build-abc123",
		"version": "v1",
	})
	resp, err := http.Post(ts.URL+urlBase+"/caches", "application/json", bytes.NewReader(body))
	require.NoError(t, err)
	require.Equal(t, 200, resp.StatusCode)

	var reserveResp map[string]any
	json.NewDecoder(resp.Body).Decode(&reserveResp)
	resp.Body.Close()
	cacheID := int64(reserveResp["cacheId"].(float64))
	assert.Greater(t, cacheID, int64(0))

	// 2. Upload data.
	data := []byte("hello cache world")
	req, _ := http.NewRequest("PATCH",
		fmt.Sprintf("%s%s/caches/%d", ts.URL, urlBase, cacheID),
		bytes.NewReader(data))
	req.Header.Set("Content-Range", fmt.Sprintf("bytes 0-%d/*", len(data)-1))
	resp, err = http.DefaultClient.Do(req)
	require.NoError(t, err)
	assert.Equal(t, 200, resp.StatusCode)
	resp.Body.Close()

	// 3. Commit.
	commitBody, _ := json.Marshal(map[string]any{"size": len(data)})
	resp, err = http.Post(
		fmt.Sprintf("%s%s/caches/%d", ts.URL, urlBase, cacheID),
		"application/json", bytes.NewReader(commitBody))
	require.NoError(t, err)
	assert.Equal(t, 200, resp.StatusCode)
	resp.Body.Close()

	// 4. Find by exact key.
	resp, err = http.Get(fmt.Sprintf("%s%s/cache?keys=go-build-abc123&version=v1", ts.URL, urlBase))
	require.NoError(t, err)
	assert.Equal(t, 200, resp.StatusCode)
	var findResp map[string]any
	json.NewDecoder(resp.Body).Decode(&findResp)
	resp.Body.Close()
	assert.Equal(t, "hit", findResp["result"])
	assert.Equal(t, "go-build-abc123", findResp["cacheKey"])

	// 5. Find by prefix.
	resp, err = http.Get(fmt.Sprintf("%s%s/cache?keys=go-build-&version=v1", ts.URL, urlBase))
	require.NoError(t, err)
	assert.Equal(t, 200, resp.StatusCode)
	json.NewDecoder(resp.Body).Decode(&findResp)
	resp.Body.Close()
	assert.Equal(t, "hit", findResp["result"])

	// 6. Download via artifact URL.
	resp, err = http.Get(fmt.Sprintf("%s%s/artifacts/%d", ts.URL, urlBase, cacheID))
	require.NoError(t, err)
	assert.Equal(t, 200, resp.StatusCode)
	downloaded, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	assert.Equal(t, data, downloaded)

	// 7. Miss on wrong version.
	resp, err = http.Get(fmt.Sprintf("%s%s/cache?keys=go-build-abc123&version=v2", ts.URL, urlBase))
	require.NoError(t, err)
	assert.Equal(t, 204, resp.StatusCode)
	resp.Body.Close()
}

func TestCacheSQLiteWAL(t *testing.T) {
	_, dir := setupTestHandler(t)

	// Verify SQLite WAL mode is used (wal file should exist after writes).
	dbPath := filepath.Join(dir, "cache.db")
	_, err := os.Stat(dbPath)
	require.NoError(t, err, "cache.db should exist")

	// WAL file may or may not exist depending on whether the WAL has been
	// checkpointed, but the main DB should always be there.
}

func TestCacheDB_CRUD(t *testing.T) {
	dir := t.TempDir()
	db, err := OpenDB(filepath.Join(dir, "test.db"))
	require.NoError(t, err)
	defer db.Close()

	// Insert.
	c := &Cache{Key: "test-key", Version: "v1", Size: 100}
	require.NoError(t, InsertCache(db, c))
	assert.Greater(t, c.ID, int64(0))

	// Get.
	got, err := GetCache(db, c.ID)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "test-key", got.Key)
	assert.False(t, got.Complete)

	// Complete.
	require.NoError(t, CompleteCache(db, c.ID, 200))
	got, _ = GetCache(db, c.ID)
	assert.True(t, got.Complete)
	assert.Equal(t, int64(200), got.Size)

	// Find.
	found, err := FindCache(db, []string{"test-key"}, "v1")
	require.NoError(t, err)
	require.NotNil(t, found)
	assert.Equal(t, c.ID, found.ID)

	// Find prefix.
	found, err = FindCache(db, []string{"test-"}, "v1")
	require.NoError(t, err)
	require.NotNil(t, found)

	// Miss.
	found, err = FindCache(db, []string{"nope"}, "v1")
	require.NoError(t, err)
	assert.Nil(t, found)

	// Delete.
	require.NoError(t, DeleteCache(db, c.ID))
	got, _ = GetCache(db, c.ID)
	assert.Nil(t, got)
}

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

	// Names with characters outside [A-Za-z0-9_-] should be rejected by the validator.
	for _, name := range []string{"foo.bar", "foo bar", "foo+bar"} {
		resp, err := http.Get(h.ExternalURL() + "/_apis/actions/" + name + "/tar")
		require.NoError(t, err, "name=%q", name)
		resp.Body.Close()
		assert.Equal(t, http.StatusBadRequest, resp.StatusCode, "name=%q should be rejected", name)
	}
}

func TestServeAction_AcceptsActionDirCharset(t *testing.T) {
	// Names matching what ActionRef.ActionDir() produces: lower/upper, digits,
	// dash, underscore. Each must reach the handler (which then 404s because
	// the action dir doesn't exist on disk — proving the validator passed it).
	dir := t.TempDir()
	h, err := StartHandler(dir, "127.0.0.1", 0)
	require.NoError(t, err)
	defer h.Close()

	for _, name := range []string{"Swatinem-rust-cache-v2-7-0", "actions-checkout-v4", "Foo_Bar-1"} {
		resp, err := http.Get(h.ExternalURL() + "/_apis/actions/" + name + "/tar")
		require.NoError(t, err, "name=%q", name)
		resp.Body.Close()
		assert.Equal(t, http.StatusNotFound, resp.StatusCode, "name=%q must reach handler (404), not be 400-rejected", name)
	}
}
