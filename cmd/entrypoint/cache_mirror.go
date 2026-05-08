package main

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// cacheMirrorRoot is where the snapshot-cache PVC is mounted inside the
// runner container. /workspace/<path> is kept as a symlink to
// /cache/<path>. Mounting directly into /workspace would EBUSY when
// actions/checkout deletes the workspace at job start.
const cacheMirrorRoot = "/cache"

// workspaceRoot is the runner's working directory.
const workspaceRoot = "/workspace"

// errHomeRequired is returned when a ~/-prefixed path is encountered
// but $HOME is empty in the runner container. mirrorCachePaths treats
// this as a job-fatal error; everything else is best-effort.
var errHomeRequired = errors.New("$HOME required but unset")

// resolvePath classifies a declared cache path and returns where the
// symlink lives (wsPath) and where the symlink points (cachePath).
//
//	"target"            -> (workspace+/target,            cache+/target)
//	"~/.cargo/registry" -> (home+/.cargo/registry,        cache+/<home>/.cargo/registry)
//	"/var/cache/apt"    -> (/var/cache/apt,               cache+/var/cache/apt)
//
// home is the runner's $HOME at call time; an empty home with a ~/-
// prefixed path returns errHomeRequired.
func resolvePath(workspace, cache, home, rel string) (wsPath, cachePath string, err error) {
	switch {
	case rel == "~" || strings.HasPrefix(rel, "~/"):
		if home == "" {
			return "", "", fmt.Errorf("path %q: %w", rel, errHomeRequired)
		}
		rest := strings.TrimPrefix(strings.TrimPrefix(rel, "~"), "/")
		wsPath = filepath.Join(home, rest)
		cachePath = filepath.Join(cache, strings.TrimPrefix(home, "/"), rest)
	case filepath.IsAbs(rel):
		wsPath = rel
		cachePath = filepath.Join(cache, strings.TrimPrefix(rel, "/"))
	default:
		wsPath = filepath.Join(workspace, rel)
		cachePath = filepath.Join(cache, rel)
	}
	return wsPath, cachePath, nil
}

// mirrorCachePaths makes the configured paths under /workspace, $HOME,
// or absolute roots into symlinks pointing at /cache/<location>. Called
// before each step. Returns a non-nil error only for fatal conditions
// (currently: a ~/-prefixed path with $HOME unset). Per-path filesystem
// errors are logged and skipped — caching just doesn't kick in for that
// path this run.
func mirrorCachePaths(workspace, cache string, paths []string) error {
	home := os.Getenv("HOME")
	for _, p := range paths {
		if err := mirrorOne(workspace, cache, home, p); err != nil {
			if errors.Is(err, errHomeRequired) {
				return err
			}
			fmt.Fprintf(os.Stderr, "cache mirror %q: %v\n", p, err)
		}
	}
	return nil
}

func mirrorOne(workspace, cache, home, rel string) error {
	if rel == "" {
		return fmt.Errorf("empty cache path")
	}
	wsPath, cachePath, err := resolvePath(workspace, cache, home, rel)
	if err != nil {
		return err
	}

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
		if err := os.Remove(wsPath); err != nil {
			return fmt.Errorf("removing stale symlink: %w", err)
		}
		return os.Symlink(cachePath, wsPath)
	}

	if !info.IsDir() {
		return nil
	}

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

// copyFile writes src's contents to dst, creating dst if missing and
// truncating it otherwise. O_NOFOLLOW refuses to follow a symlink at
// the final path component — this matters because dst lives in a tree
// (the snapshot cache) that prior workflow steps can plant symlinks
// into; without O_NOFOLLOW a checkout that wrote dst/foo -> /etc/passwd
// would have us writing into /etc/passwd on the next mirror pass.
//
// On ELOOP we treat the existing entry as a stale/adversarial symlink,
// unlink it, and retry the open once.
func copyFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := openNoFollowForWrite(dst, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// openNoFollowForWrite opens dst for writing with O_NOFOLLOW. If the
// open fails because the final path component is a symlink, the
// symlink is removed and the open is retried once.
func openNoFollowForWrite(dst string, mode os.FileMode) (*os.File, error) {
	flags := os.O_WRONLY | os.O_CREATE | os.O_TRUNC | syscall.O_NOFOLLOW
	out, err := os.OpenFile(dst, flags, mode)
	if err == nil {
		return out, nil
	}
	if !errors.Is(err, syscall.ELOOP) {
		return nil, err
	}
	if rmErr := os.Remove(dst); rmErr != nil {
		return nil, fmt.Errorf("removing symlink %q before write: %w", dst, rmErr)
	}
	return os.OpenFile(dst, flags, mode)
}
