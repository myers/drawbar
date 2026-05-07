package cache

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestFindCache_LIKEWildcardsInPrefix verifies that user-supplied prefixes
// containing SQL LIKE metacharacters (`%`, `_`) are matched literally —
// a workflow that lists `restore-keys: ["foo%"]` must NOT pull caches
// whose key happens to start with `foo` followed by anything.
func TestFindCache_LIKEWildcardsInPrefix(t *testing.T) {
	dir := t.TempDir()
	db, err := OpenDB(filepath.Join(dir, "test.db"))
	require.NoError(t, err)
	defer db.Close()

	// Insert + complete each key so they are all FindCache-eligible.
	for _, key := range []string{"foo", "foobar", "foo%bar", "foo_bar"} {
		c := &Cache{Key: key, Version: "v1"}
		require.NoError(t, InsertCache(db, c))
		require.NoError(t, CompleteCache(db, c.ID, 1))
	}

	// `foo%` must match only the literal-`%` key, not `foobar`.
	got, err := FindCache(db, []string{"foo%"}, "v1")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "foo%bar", got.Key)

	// `foo_` must match only the literal-`_` key, not `foobar`.
	got, err = FindCache(db, []string{"foo_"}, "v1")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "foo_bar", got.Key)

	// Existing prefix semantics still work: `foo` matches some entry whose
	// key starts with `foo` (one of the four).
	got, err = FindCache(db, []string{"foo"}, "v1")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Contains(t, []string{"foo", "foobar", "foo%bar", "foo_bar"}, got.Key)

	// A prefix that hits nothing still misses cleanly.
	got, err = FindCache(db, []string{"nope%"}, "v1")
	require.NoError(t, err)
	assert.Nil(t, got)
}

// TestFindCache_BackslashInPrefix verifies the escape character itself
// is escaped — a `\` in the prefix must not be interpreted by the
// `ESCAPE '\'` clause as starting an escape sequence.
func TestFindCache_BackslashInPrefix(t *testing.T) {
	dir := t.TempDir()
	db, err := OpenDB(filepath.Join(dir, "test.db"))
	require.NoError(t, err)
	defer db.Close()

	c := &Cache{Key: `foo\bar`, Version: "v1"}
	require.NoError(t, InsertCache(db, c))
	require.NoError(t, CompleteCache(db, c.ID, 1))

	got, err := FindCache(db, []string{`foo\`}, "v1")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, `foo\bar`, got.Key)
}
