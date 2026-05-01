package main

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
)

// cacheMirrorRoot is where the snapshot-cache PVC is mounted inside the
// runner container. /workspace/<path> is kept as a symlink to
// /cache/<path>. Mounting directly into /workspace would EBUSY when
// actions/checkout deletes the workspace at job start.
const cacheMirrorRoot = "/cache"

// workspaceRoot is the runner's working directory.
const workspaceRoot = "/workspace"

// mirrorCachePaths makes /workspace/<path> a symlink to /cache/<path>
// for each declared cache path. Idempotent: safe to call between every
// step. Cases handled:
//
//   - /workspace/<path> missing: create symlink directly.
//   - /workspace/<path> already a symlink to the right /cache target: noop.
//   - /workspace/<path> is a real directory: move its contents into
//     /cache/<path> (merging on collision, /workspace wins) and replace
//     the directory with a symlink. This is the warm-run case where
//     actions/checkout recreated the dir, or the case where step 1 was
//     checkout itself and produced files we want cached.
//   - /workspace/<path> is anything else (regular file, broken symlink
//     to elsewhere): leave it alone and skip — surfacing this as an
//     error would be more disruptive than helpful, and it cannot happen
//     for the cache use case we care about.
//
// Each call is best-effort: if mirroring one path fails, log and
// continue — caching just doesn't kick in for that path this run.
func mirrorCachePaths(paths []string) {
	for _, p := range paths {
		if err := mirrorOne(workspaceRoot, cacheMirrorRoot, p); err != nil {
			fmt.Fprintf(os.Stderr, "cache mirror %q: %v\n", p, err)
		}
	}
}

func mirrorOne(workspace, cache, rel string) error {
	if rel == "" || filepath.IsAbs(rel) {
		return fmt.Errorf("invalid relative path %q", rel)
	}
	wsPath := filepath.Join(workspace, rel)
	cachePath := filepath.Join(cache, rel)

	if err := os.MkdirAll(filepath.Dir(cachePath), 0o755); err != nil {
		return fmt.Errorf("mkdir cache parent: %w", err)
	}
	if err := os.MkdirAll(cachePath, 0o755); err != nil {
		return fmt.Errorf("mkdir cache: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(wsPath), 0o755); err != nil {
		return fmt.Errorf("mkdir workspace parent: %w", err)
	}

	info, err := os.Lstat(wsPath)
	if errors.Is(err, fs.ErrNotExist) {
		return os.Symlink(cachePath, wsPath)
	}
	if err != nil {
		return fmt.Errorf("lstat workspace: %w", err)
	}

	if info.Mode()&os.ModeSymlink != 0 {
		target, err := os.Readlink(wsPath)
		if err != nil {
			return fmt.Errorf("readlink workspace: %w", err)
		}
		if target == cachePath {
			return nil
		}
		// Wrong-pointing symlink — replace it.
		if err := os.Remove(wsPath); err != nil {
			return fmt.Errorf("removing stale symlink: %w", err)
		}
		return os.Symlink(cachePath, wsPath)
	}

	if !info.IsDir() {
		// Regular file or other; leave alone.
		return nil
	}

	// Real directory — merge into cache and replace with symlink.
	if err := mergeDir(wsPath, cachePath); err != nil {
		return fmt.Errorf("merging into cache: %w", err)
	}
	if err := os.RemoveAll(wsPath); err != nil {
		return fmt.Errorf("removing workspace dir: %w", err)
	}
	return os.Symlink(cachePath, wsPath)
}

// mergeDir copies entries from src into dst, recursively. /workspace
// wins on conflict (the freshly-checked-out file is more authoritative
// than whatever was cached). Symlinks are preserved as symlinks.
func mergeDir(src, dst string) error {
	entries, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	for _, e := range entries {
		srcPath := filepath.Join(src, e.Name())
		dstPath := filepath.Join(dst, e.Name())
		info, err := os.Lstat(srcPath)
		if err != nil {
			return err
		}

		switch {
		case info.Mode()&os.ModeSymlink != 0:
			target, err := os.Readlink(srcPath)
			if err != nil {
				return err
			}
			_ = os.Remove(dstPath)
			if err := os.Symlink(target, dstPath); err != nil {
				return err
			}
		case info.IsDir():
			if err := os.MkdirAll(dstPath, info.Mode().Perm()); err != nil {
				return err
			}
			if err := mergeDir(srcPath, dstPath); err != nil {
				return err
			}
		default:
			if err := copyFile(srcPath, dstPath, info.Mode().Perm()); err != nil {
				return err
			}
		}
	}
	return nil
}

func copyFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
