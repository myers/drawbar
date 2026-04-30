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
