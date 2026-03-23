package actions

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoadAction_CloneAndCache(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}

	// Set up a bare repo at {baseDir}/myorg/myrepo.git so CloneURL resolves correctly.
	baseDir, org, repo := setupActionRepo(t, "myorg", "myrepo", map[string]string{
		"action.yml": `
name: test-action
description: A test action
inputs:
  path:
    default: "."
runs:
  using: node20
  main: index.js
`,
	})

	cacheDir := t.TempDir()
	cache := NewActionCache(cacheDir)

	ref := &ActionRef{Org: org, Repo: repo, Ref: "main"}

	// First load — clones.
	meta, err := cache.LoadAction(ref, baseDir, "")
	require.NoError(t, err)
	require.NotNil(t, meta)
	assert.Equal(t, "test-action", meta.Action.Name)
	assert.Equal(t, "index.js", meta.Action.Runs.Main)

	// Verify cache dir exists.
	cachedPath := filepath.Join(cacheDir, "actions-repo-cache", ref.ActionDir())
	_, err = os.Stat(filepath.Join(cachedPath, ".git"))
	require.NoError(t, err, "cached .git dir should exist")

	// Second load — uses cache (no clone).
	meta2, err := cache.LoadAction(ref, baseDir, "")
	require.NoError(t, err)
	assert.Equal(t, "test-action", meta2.Action.Name)
}

func TestLoadAction_PathTraversalRejected(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}

	baseDir, org, repo := setupActionRepo(t, "evil", "action", map[string]string{
		"action.yml": `
name: evil-action
runs:
  using: node20
  main: ../../../etc/passwd
`,
	})

	cache := NewActionCache(t.TempDir())
	ref := &ActionRef{Org: org, Repo: repo, Ref: "main"}

	_, err := cache.LoadAction(ref, baseDir, "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "path traversal")
}

func TestLoadAction_MissingActionYml(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}

	baseDir, org, repo := setupActionRepo(t, "test", "noaction", map[string]string{
		"README.md": "# Not an action",
	})

	cache := NewActionCache(t.TempDir())
	ref := &ActionRef{Org: org, Repo: repo, Ref: "main"}

	_, err := cache.LoadAction(ref, baseDir, "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no action.yml")
}

// setupActionRepo creates a bare git repo at {baseDir}/{org}/{repo}.git
// matching the URL layout that CloneURL expects. Returns (baseDir, org, repo).
func setupActionRepo(t *testing.T, org, repo string, files map[string]string) (string, string, string) {
	t.Helper()

	baseDir := t.TempDir()
	repoDir := filepath.Join(baseDir, org, repo+".git")

	// Create a working repo first.
	workDir := filepath.Join(t.TempDir(), "work")
	require.NoError(t, os.MkdirAll(workDir, 0o755))
	run(t, workDir, "git", "init", "-b", "main")
	run(t, workDir, "git", "config", "user.email", "test@test.com")
	run(t, workDir, "git", "config", "user.name", "Test")

	for name, content := range files {
		path := filepath.Join(workDir, name)
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
	}

	run(t, workDir, "git", "add", ".")
	run(t, workDir, "git", "commit", "-m", "init")

	// Clone to bare repo at the expected path.
	require.NoError(t, os.MkdirAll(filepath.Dir(repoDir), 0o755))
	run(t, "", "git", "clone", "--bare", workDir, repoDir)

	return baseDir, org, repo
}

func run(t *testing.T, dir string, name string, args ...string) {
	t.Helper()
	cmd := exec.Command(name, args...)
	if dir != "" {
		cmd.Dir = dir
	}
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "command %s %v failed:\n%s", name, args, string(out))
}
