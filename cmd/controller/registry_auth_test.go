package main

import (
	"encoding/base64"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRegistryAuthHeader_NoLineWrap proves that the canonical bash snippet
// used to build a registry-auth header strips newlines from base64 output.
// GNU base64 wraps at 76 chars by default; without `tr -d '\n'`, a long
// USER:TOKEN string produces a multi-line header that corrupts the docker
// config JSON. Bug 022 / Finding C.
func TestRegistryAuthHeader_NoLineWrap(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	if _, err := exec.LookPath("base64"); err != nil {
		t.Skip("base64 not available")
	}

	// 60 + 60 + 1 colon = 121 bytes → base64 grows ~33% to 164 chars.
	// That comfortably exceeds GNU base64's default 76-char wrap point,
	// so the unwrapped reference output would contain at least one
	// newline if the `tr -d` filter were ever removed.
	user := strings.Repeat("u", 60)
	token := strings.Repeat("t", 60)

	// Same shape as actions/build-push/action.yml and .gitea/workflows/build.yml.
	script := `printf '%s:%s' "$USER_VAL" "$TOKEN_VAL" | base64 | tr -d '\n'`

	cmd := exec.Command("bash", "-c", script)
	cmd.Env = append(os.Environ(), "USER_VAL="+user, "TOKEN_VAL="+token)
	out, err := cmd.Output()
	require.NoError(t, err)

	auth := string(out)
	assert.NotContains(t, auth, "\n", "auth header must not contain newline")
	assert.NotEmpty(t, auth)

	// And it must round-trip back to USER:TOKEN.
	decoded, err := base64.StdEncoding.DecodeString(auth)
	require.NoError(t, err)
	assert.Equal(t, user+":"+token, string(decoded))
}

// TestRegistryAuthSnippets_HaveTrFilter is a guard against silent
// regression-by-edit. Each of the four shell snippets touched by bug
// 022 / Finding C must keep piping the base64 output through `tr -d`
// (or some equivalent newline-stripper). If a future edit removes the
// filter from one of them, the test should fail loudly even if no one
// remembers why it's there.
func TestRegistryAuthSnippets_HaveTrFilter(t *testing.T) {
	repoRoot := findRepoRoot(t)
	files := []struct {
		path    string
		needles []string
	}{
		{
			path:    ".gitea/workflows/build.yml",
			needles: []string{`base64 | tr -d '\n'`},
		},
		{
			// .woodpecker/build.yaml has *two* base64 invocations,
			// both inside awk-built shell pipelines. The escapes
			// land as `tr -d "\n"` in the awk source.
			path:    ".woodpecker/build.yaml",
			needles: []string{`base64 | tr -d \"\\n\"`, `base64 | tr -d \"\\n\"`},
		},
		{
			path:    "actions/build-push/action.yml",
			needles: []string{`base64 | tr -d '\n'`},
		},
	}

	for _, f := range files {
		full := filepath.Join(repoRoot, f.path)
		body, err := os.ReadFile(full)
		require.NoError(t, err, "reading %s", f.path)
		text := string(body)
		// Count substring matches. needles is a slice (not set) because
		// .woodpecker/build.yaml needs the filter to appear twice.
		want := len(f.needles)
		got := strings.Count(text, f.needles[0])
		assert.GreaterOrEqual(t, got, want,
			"%s: expected `%s` to appear at least %d time(s); got %d",
			f.path, f.needles[0], want, got)
	}
}

// findRepoRoot walks up from the test's working directory until it finds
// a go.mod, returning the absolute repo path.
func findRepoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	require.NoError(t, err)
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("go.mod not found above %s", dir)
		}
		dir = parent
	}
}
