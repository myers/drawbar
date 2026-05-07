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
