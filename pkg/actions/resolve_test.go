package actions

import (
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
	ref := &ActionRef{Org: "actions", Repo: "cache", Ref: "v4"}
	assert.Equal(t, "actions-cache-v4", ref.ActionDir())

	ref2 := &ActionRef{Org: "Swatinem", Repo: "rust-cache", Ref: "v2.7.0"}
	assert.Equal(t, "Swatinem-rust-cache-v2-7-0", ref2.ActionDir())
}

func TestActionRef_String(t *testing.T) {
	assert.Equal(t, "actions/cache@v4",
		(&ActionRef{Org: "actions", Repo: "cache", Ref: "v4"}).String())
	assert.Equal(t, "org/repo/sub@v1",
		(&ActionRef{Org: "org", Repo: "repo", Path: "sub", Ref: "v1"}).String())
}
