package actions

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseActionRef(t *testing.T) {
	tests := []struct {
		input    string
		expected *ActionRef
		wantErr  bool
	}{
		{
			input:    "actions/checkout@v4",
			expected: &ActionRef{Org: "actions", Repo: "checkout", Ref: "v4"},
		},
		{
			input:    "actions/cache@v4",
			expected: &ActionRef{Org: "actions", Repo: "cache", Ref: "v4"},
		},
		{
			input:    "dtolnay/rust-toolchain@stable",
			expected: &ActionRef{Org: "dtolnay", Repo: "rust-toolchain", Ref: "stable"},
		},
		{
			input:    "org/repo/subdir@v1",
			expected: &ActionRef{Org: "org", Repo: "repo", Path: "subdir", Ref: "v1"},
		},
		{
			input:    "https://code.forgejo.org/actions/cache@v4",
			expected: &ActionRef{URL: "https://code.forgejo.org", Org: "actions", Repo: "cache", Ref: "v4"},
		},
		{
			input:   "",
			wantErr: true,
		},
		{
			input:   "actions/cache",
			wantErr: true, // missing @ref
		},
		{
			input:   "./local-action",
			wantErr: true, // local action
		},
		{
			input:   "docker://alpine",
			wantErr: true, // docker URL
		},
		{
			input:   "org/repo/../etc/passwd@v1",
			wantErr: true, // path traversal
		},
		{
			input:   "https://example.com/org/repo/../../etc@v1",
			wantErr: true, // path traversal via URL
		},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			ref, err := ParseActionRef(tt.input)
			if tt.wantErr {
				assert.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.expected.Org, ref.Org)
			assert.Equal(t, tt.expected.Repo, ref.Repo)
			assert.Equal(t, tt.expected.Path, ref.Path)
			assert.Equal(t, tt.expected.Ref, ref.Ref)
			if tt.expected.URL != "" {
				assert.Equal(t, tt.expected.URL, ref.URL)
			}
		})
	}
}

func TestActionRef_CloneURL(t *testing.T) {
	ref := &ActionRef{Org: "actions", Repo: "cache", Ref: "v4"}
	assert.Equal(t, "https://code.forgejo.org/actions/cache.git",
		ref.CloneURL("https://code.forgejo.org"))

	// With explicit URL override
	ref2 := &ActionRef{URL: "https://github.com", Org: "actions", Repo: "cache", Ref: "v4"}
	assert.Equal(t, "https://github.com/actions/cache.git",
		ref2.CloneURL("https://code.forgejo.org"))
}

func TestActionRef_ActionDir(t *testing.T) {
	// Format: <sanitized org-repo-ref>-<8 hex chars FNV-1a>.
	// We assert structure, not the hash digits, so tests don't break if the
	// hash content (currently "org/repo@ref") is ever tweaked.
	ref := &ActionRef{Org: "actions", Repo: "cache", Ref: "v4"}
	dir := ref.ActionDir()
	assert.True(t, strings.HasPrefix(dir, "actions-cache-v4-"), "got %q", dir)
	assert.Regexp(t, `^actions-cache-v4-[0-9a-f]{8}$`, dir)

	ref2 := &ActionRef{Org: "Swatinem", Repo: "rust-cache", Ref: "v2.7.0"}
	dir2 := ref2.ActionDir()
	assert.True(t, strings.HasPrefix(dir2, "Swatinem-rust-cache-v2-7-0-"), "got %q", dir2)
	assert.Regexp(t, `^Swatinem-rust-cache-v2-7-0-[0-9a-f]{8}$`, dir2)

	// Same input → same dir (deterministic).
	assert.Equal(t, dir, ref.ActionDir())
}

// TestActionRef_ActionDir_NoCollisions guards against bug 019 finding C
// for the case the bug is actually about: distinct git Refs (different
// versions of the same action) that happen to sanitize to the same
// prefix must still produce distinct ActionDir outputs.
//
// We deliberately do NOT vary Path here. Subdir actions like
// `org/repo/sub@v1` and `org/repo/other@v1` share the same on-disk
// action source on purpose — they live in the same git clone, and
// loader.go::actionPath() appends Ref.Path to the dir at use time. So
// two refs differing only in Path are *expected* to produce the same
// ActionDir (they share a clone) and tightening that would just waste
// cache by re-fetching the same git repo per subdir.
func TestActionRef_ActionDir_NoCollisions(t *testing.T) {
	refs := []string{"v4.0.0", "v4-0-0", "v4_0_0", "v4/0/0", "v4 0 0"}
	seen := make(map[string]string, len(refs))
	for _, r := range refs {
		dir := (&ActionRef{Org: "actions", Repo: "cache", Ref: r}).ActionDir()
		if prev, dup := seen[dir]; dup {
			t.Fatalf("collision: ref %q and %q both map to %q", prev, r, dir)
		}
		seen[dir] = r
		// The result must still pass the cache handler's isSafeActionDir
		// charset (lowercase/uppercase letters, digits, `-`, `_`).
		for _, ch := range dir {
			ok := (ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z') ||
				(ch >= '0' && ch <= '9') || ch == '-' || ch == '_'
			assert.True(t, ok, "ref %q produced dir %q with disallowed char %q", r, dir, ch)
		}
	}
}

func TestActionRef_String(t *testing.T) {
	assert.Equal(t, "actions/cache@v4",
		(&ActionRef{Org: "actions", Repo: "cache", Ref: "v4"}).String())
	assert.Equal(t, "org/repo/sub@v1",
		(&ActionRef{Org: "org", Repo: "repo", Path: "sub", Ref: "v1"}).String())
}
