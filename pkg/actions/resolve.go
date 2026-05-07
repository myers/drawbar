package actions

import (
	"fmt"
	"strings"
)

// ActionRef represents a parsed action reference like "actions/cache@v4".
type ActionRef struct {
	Org  string // "actions"
	Repo string // "cache"
	Path string // "" or "subdir/path"
	Ref  string // "v4"
	URL  string // optional base URL prefix (e.g., "https://github.com")
}

// ParseActionRef parses a `uses:` string into its components.
// Supports: "actions/cache@v4", "org/repo/subdir@ref", "https://example.com/org/repo@ref"
func ParseActionRef(uses string) (*ActionRef, error) {
	uses = strings.TrimSpace(uses)
	if uses == "" {
		return nil, fmt.Errorf("empty action reference")
	}

	// Skip local actions and docker URLs.
	if strings.HasPrefix(uses, "./") || strings.HasPrefix(uses, "docker://") {
		return nil, fmt.Errorf("not a remote action: %s", uses)
	}

	// Try simple "org/repo@ref" format first.
	parts := strings.SplitN(uses, "@", 2)
	if len(parts) != 2 || parts[1] == "" {
		return nil, fmt.Errorf("invalid action reference %q: missing @ref", uses)
	}

	ref := parts[1]
	repoPath := parts[0]

	// Check for URL prefix (e.g., "https://code.forgejo.org/actions/cache")
	var url string
	if strings.Contains(repoPath, "://") {
		// Has URL prefix. Extract it.
		// Find the org/repo part after the URL.
		idx := strings.Index(repoPath, "://")
		rest := repoPath[idx+3:] // after "://"
		// rest is "host/org/repo/path" — find first /org/repo
		slashParts := strings.SplitN(rest, "/", 4) // host, org, repo, [path]
		if len(slashParts) < 3 {
			return nil, fmt.Errorf("invalid action URL %q", uses)
		}
		url = repoPath[:idx+3] + slashParts[0] // "https://host"
		ref := &ActionRef{
			URL:  url,
			Org:  slashParts[1],
			Repo: slashParts[2],
			Ref:  ref,
		}
		if len(slashParts) > 3 {
			ref.Path = slashParts[3]
		}
		if err := validateActionPath(ref.Path); err != nil {
			return nil, err
		}
		return ref, nil
	}

	// Simple "org/repo(/path)@ref" format.
	pathParts := strings.SplitN(repoPath, "/", 3)
	if len(pathParts) < 2 {
		return nil, fmt.Errorf("invalid action reference %q: expected org/repo@ref", uses)
	}

	result := &ActionRef{
		Org:  pathParts[0],
		Repo: pathParts[1],
		Ref:  ref,
	}
	if len(pathParts) > 2 {
		result.Path = pathParts[2]
	}
	if err := validateActionPath(result.Path); err != nil {
		return nil, err
	}

	return result, nil
}

func validateActionPath(path string) error {
	if path == "" {
		return nil
	}
	for _, part := range strings.Split(path, "/") {
		if part == ".." {
			return fmt.Errorf("action path contains traversal: %q", path)
		}
	}
	return nil
}

// CloneURL returns the git clone URL for this action.
func (a *ActionRef) CloneURL(defaultActionsURL string) string {
	base := defaultActionsURL
	if a.URL != "" {
		base = a.URL
	}
	base = strings.TrimRight(base, "/")
	return fmt.Sprintf("%s/%s/%s.git", base, a.Org, a.Repo)
}

// ActionDir returns a unique directory name for this action within /actions.
func (a *ActionRef) ActionDir() string {
	// Sanitize for filesystem: "actions/cache@v4" → "actions-cache-v4"
	name := fmt.Sprintf("%s-%s-%s", a.Org, a.Repo, a.Ref)
	name = strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			return r
		}
		return '-'
	}, name)
	return name
}

// String returns a human-readable representation.
func (a *ActionRef) String() string {
	s := fmt.Sprintf("%s/%s", a.Org, a.Repo)
	if a.Path != "" {
		s += "/" + a.Path
	}
	s += "@" + a.Ref
	return s
}
