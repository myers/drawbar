package k8s

import (
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestSanitizeLabelValue(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"myorg/myrepo", "myorg-myrepo"},
		{"simple", "simple"},
		// Leading non-alphanumeric must be stripped.
		{"_foo", "foo"},
		{":foo:bar:", "foo-bar"},
		// Refs with embedded slashes/colons.
		{"feature/foo:bar", "feature-foo-bar"},
		{"feat/test:wip", "feat-test-wip"},
		// All non-alphanumeric collapses to empty (allowed by k8s).
		{":::", ""},
		{"_", ""},
		// Already-valid input is preserved (including dots and underscores in the middle).
		{"v1.2.3_rc", "v1.2.3_rc"},
		// Numeric-leading is fine — alphanumerics include digits.
		{"123-foo", "123-foo"},
	}
	for _, c := range cases {
		got := SanitizeLabelValue(c.in)
		assert.Equal(t, c.want, got, "input %q", c.in)
		assertValidLabelValue(t, got)
	}

	// Length cap: 63 chars max, and the result must still be valid (no
	// trailing non-alphanumeric after truncation).
	long := SanitizeLabelValue("a very long string that exceeds the sixty three character kubernetes label value limit by quite a bit")
	assert.LessOrEqual(t, len(long), 63)
	assertValidLabelValue(t, long)

	// Truncation that lands on a non-alnum must trim back to an alnum.
	// 62 alphanums + 64 dashes → after sanitize, 62 alphanums + dashes;
	// after the 63 cut and re-trim, only the 62 alphanums remain.
	tricky := SanitizeLabelValue(strings.Repeat("a", 62) + strings.Repeat("/", 64))
	assert.Equal(t, strings.Repeat("a", 62), tricky)
	assertValidLabelValue(t, tricky)
}

// assertValidLabelValue checks the k8s label-value regex
// (([A-Za-z0-9][-A-Za-z0-9_.]*)?[A-Za-z0-9])? — empty allowed; otherwise
// must start and end with alphanumeric and only contain [-A-Za-z0-9_.].
func assertValidLabelValue(t *testing.T, s string) {
	t.Helper()
	if s == "" {
		return
	}
	assert.LessOrEqual(t, len(s), 63, "label %q exceeds 63 chars", s)
	matched, err := regexp.MatchString(`^[A-Za-z0-9]([-A-Za-z0-9_.]*[A-Za-z0-9])?$`, s)
	assert.NoError(t, err)
	assert.True(t, matched, "label %q does not match k8s regex", s)
}
