package cache

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestStorageFilename_AllBucketsReachable asserts that ids in [0, 256)
// land in 256 distinct buckets — i.e. the modulo really is mod-256, not
// mod-255 (which would leave bucket `ff` unreachable and double-up `00`).
func TestStorageFilename_AllBucketsReachable(t *testing.T) {
	s, err := NewStorage(t.TempDir())
	require.NoError(t, err)

	buckets := make(map[string]struct{}, 256)
	for id := uint64(0); id < 256; id++ {
		buckets[filepath.Base(filepath.Dir(s.filename(id)))] = struct{}{}
	}
	assert.Len(t, buckets, 256, "ids 0..255 must land in 256 distinct buckets")

	// Bucket `ff` specifically must be reachable.
	assert.Equal(t, "ff", filepath.Base(filepath.Dir(s.filename(0xff))))
}
