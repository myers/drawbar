package cache

import (
	"archive/tar"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// tarDir streams a tar archive of root into w. Top-level entries whose name is
// in excludes (e.g. ".git") are skipped entirely along with their contents.
// Symbolic links are emitted as tar.TypeSymlink (not followed). The archive
// uses paths relative to root, with forward slashes.
func tarDir(w io.Writer, root string, excludes []string) error {
	tw := tar.NewWriter(w)
	defer tw.Close()

	excludeSet := map[string]struct{}{}
	for _, e := range excludes {
		excludeSet[e] = struct{}{}
	}

	root = filepath.Clean(root)

	return filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path == root {
			return nil // skip the root itself
		}

		rel, err := filepath.Rel(root, path)
		if err != nil {
			return fmt.Errorf("rel %s: %w", path, err)
		}
		// Match excludes only against the top-level component.
		top := rel
		if i := strings.IndexByte(rel, filepath.Separator); i >= 0 {
			top = rel[:i]
		}
		if _, skip := excludeSet[top]; skip {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}

		info, err := d.Info()
		if err != nil {
			return fmt.Errorf("stat %s: %w", path, err)
		}

		var linkTarget string
		if info.Mode()&os.ModeSymlink != 0 {
			linkTarget, err = os.Readlink(path)
			if err != nil {
				return fmt.Errorf("readlink %s: %w", path, err)
			}
		}

		hdr, err := tar.FileInfoHeader(info, linkTarget)
		if err != nil {
			return fmt.Errorf("header %s: %w", path, err)
		}
		hdr.Name = filepath.ToSlash(rel)
		if d.IsDir() {
			hdr.Name += "/"
		}

		if err := tw.WriteHeader(hdr); err != nil {
			return fmt.Errorf("write header %s: %w", path, err)
		}

		if info.Mode().IsRegular() {
			f, err := os.Open(path)
			if err != nil {
				return fmt.Errorf("open %s: %w", path, err)
			}
			_, copyErr := io.Copy(tw, f)
			f.Close()
			if copyErr != nil {
				return fmt.Errorf("copy %s: %w", path, copyErr)
			}
		}
		return nil
	})
}
