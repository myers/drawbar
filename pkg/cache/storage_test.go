package cache

import (
	"fmt"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestStorageFilename_BucketIsIdMod256 pins the contract: each id lands
// in bucket fmt.Sprintf("%02x", id%256). This catches the bug-019 mod-255
// typo (which left bucket `ff` unreachable) and any future drift in the
// modulo or format.
func TestStorageFilename_BucketIsIdMod256(t *testing.T) {
	s, err := NewStorage(t.TempDir())
	require.NoError(t, err)

	for id := uint64(0); id < 256; id++ {
		bucket := filepath.Base(filepath.Dir(s.filename(id)))
		assert.Equal(t, fmt.Sprintf("%02x", id), bucket, "id=%d", id)
	}

	// Spot-check a few ids past the first 256 to confirm the modulo
	// (not just the prefix-zero-padded format) is what's being asserted.
	assert.Equal(t, "00", filepath.Base(filepath.Dir(s.filename(256))))
	assert.Equal(t, "ff", filepath.Base(filepath.Dir(s.filename(511))))
}
