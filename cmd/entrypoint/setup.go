package main

import (
	"archive/tar"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/myers/drawbar/pkg/types"
)

// setupRetryBackoff is the base backoff between retries. Tests set this to 0.
var setupRetryBackoff = 1 * time.Second

const (
	setupHTTPTimeout = 30 * time.Second
	setupMaxAttempts = 3
)

// runSetup reads a manifest from manifestPath, writes the askpass shim into
// shimDir, and downloads each manifest.Actions tarball into actionsDir/<Dir>/.
func runSetup(manifestPath, actionsDir, shimDir string) error {
	manifest, err := loadManifest(manifestPath)
	if err != nil {
		return err
	}

	if err := writeAskpassShim(filepath.Join(shimDir, "askpass.sh")); err != nil {
		return fmt.Errorf("writing askpass shim: %w", err)
	}

	if err := os.MkdirAll(actionsDir, 0o755); err != nil {
		return fmt.Errorf("creating actions dir: %w", err)
	}

	for _, a := range manifest.Actions {
		if err := fetchAction(a, actionsDir); err != nil {
			return fmt.Errorf("action %s: %w", a.Dir, err)
		}
	}
	return nil
}

// writeAskpassShim writes a tiny shell script git uses for HTTP basic auth.
// The contents echo $GIT_AUTH_TOKEN — same shape that was previously written
// from the setup-shim heredoc.
func writeAskpassShim(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte("#!/bin/sh\necho \"$GIT_AUTH_TOKEN\"\n"), 0o755)
}

// errFetchNotFound is the sentinel returned when the server returned 404.
// It is not retried.
var errFetchNotFound = errors.New("not found")

// fetchAction downloads a single action tarball into actionsDir/<a.Dir>/.
// Retries on 5xx and network errors; bails immediately on 404.
//
// Each attempt extracts into a fresh sibling temp dir
// (target.partial.<rand>) and atomically renames it to target on
// success. This makes retries idempotent against partial extractions
// (a network blip used to leave EEXIST-tripping debris) and means
// concurrent readers of target either see no extraction or a complete
// one, never a half-finished tree.
func fetchAction(a types.ActionFetch, actionsDir string) error {
	if err := os.MkdirAll(actionsDir, 0o755); err != nil {
		return fmt.Errorf("mkdir actions dir: %w", err)
	}
	target := filepath.Join(actionsDir, a.Dir)

	client := &http.Client{Timeout: setupHTTPTimeout}

	var lastErr error
	for attempt := 1; attempt <= setupMaxAttempts; attempt++ {
		err := tryFetchAndPromote(client, a.URL, target)
		if err == nil {
			return nil
		}
		if errors.Is(err, errFetchNotFound) {
			return err // do not retry
		}
		lastErr = err
		if attempt < setupMaxAttempts {
			time.Sleep(time.Duration(attempt) * setupRetryBackoff)
		}
	}
	return fmt.Errorf("after %d attempts: %w", setupMaxAttempts, lastErr)
}

// tryFetchAndPromote performs one fetch attempt: extract to a sibling
// temp dir, then rename it over target. The temp dir is cleaned up on
// every error path. On success the rename consumes the temp dir, so
// the deferred RemoveAll is a no-op.
func tryFetchAndPromote(client *http.Client, url, target string) error {
	tmp, err := makePartialDir(target)
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)

	if err := tryFetch(client, url, tmp); err != nil {
		return err
	}

	// Promote tmp -> target. Common case: target is missing (this is
	// the first install for this action), so a single Rename suffices
	// and the directory is never observed in an absent state. Only if
	// a prior run left target behind do we RemoveAll first.
	if _, statErr := os.Lstat(target); statErr == nil {
		if err := os.RemoveAll(target); err != nil {
			return fmt.Errorf("removing prior target %q: %w", target, err)
		}
	} else if !errors.Is(statErr, fs.ErrNotExist) {
		return fmt.Errorf("stat target %q: %w", target, statErr)
	}
	if err := os.Rename(tmp, target); err != nil {
		return fmt.Errorf("promoting %q to %q: %w", tmp, target, err)
	}
	return nil
}

// makePartialDir creates an empty sibling of target named
// target.partial.<8 hex bytes>. The random suffix avoids collisions
// when a previous run left a stale partial dir behind. On the
// vanishingly unlikely EEXIST (same 64-bit suffix as a leftover dir)
// we re-roll up to a few times before giving up.
func makePartialDir(target string) (string, error) {
	for attempt := 0; attempt < 5; attempt++ {
		var buf [8]byte
		if _, err := rand.Read(buf[:]); err != nil {
			return "", fmt.Errorf("rand: %w", err)
		}
		tmp := target + ".partial." + hex.EncodeToString(buf[:])
		err := os.Mkdir(tmp, 0o755)
		if err == nil {
			return tmp, nil
		}
		if !errors.Is(err, fs.ErrExist) {
			return "", fmt.Errorf("mkdir partial: %w", err)
		}
	}
	return "", fmt.Errorf("mkdir partial: exhausted random suffixes for %q", target)
}

func tryFetch(client *http.Client, url, target string) error {
	resp, err := client.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusOK:
		return untarInto(resp.Body, target)
	case resp.StatusCode == http.StatusNotFound:
		return errFetchNotFound
	default:
		return fmt.Errorf("http status %d", resp.StatusCode)
	}
}

// untarInto extracts a tar stream into target, rejecting any entry whose
// cleaned path would escape target.
func untarInto(r io.Reader, target string) error {
	target = filepath.Clean(target) + string(filepath.Separator)
	tr := tar.NewReader(r)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("tar next: %w", err)
		}

		// Reject any entry whose name contains ".." components — do not silently
		// neutralize them, because the intent is clearly malicious.
		for _, part := range strings.Split(filepath.ToSlash(hdr.Name), "/") {
			if part == ".." {
				return fmt.Errorf("tar entry escapes target: %q", hdr.Name)
			}
		}

		clean := filepath.Clean("/" + hdr.Name) // anchored at root
		dest := filepath.Join(target, strings.TrimPrefix(clean, "/"))
		// Defense in depth: ensure dest is still inside target.
		if !strings.HasPrefix(dest+string(filepath.Separator), target) && dest != strings.TrimSuffix(target, string(filepath.Separator)) {
			return fmt.Errorf("tar entry escapes target: %q", hdr.Name)
		}

		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(dest, 0o755); err != nil {
				return fmt.Errorf("mkdir %s: %w", dest, err)
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
				return fmt.Errorf("mkdir parent %s: %w", dest, err)
			}
			// O_NOFOLLOW: defense-in-depth against an earlier-in-stream
			// symlink redirecting our write outside target. Symlink
			// linknames are already validated above (no abs, no ".."),
			// so this should not trigger in practice — but if it does,
			// fail loudly rather than write through.
			f, err := os.OpenFile(dest, os.O_CREATE|os.O_WRONLY|os.O_TRUNC|syscall.O_NOFOLLOW, os.FileMode(hdr.Mode)&0o777)
			if err != nil {
				return fmt.Errorf("create %s: %w", dest, err)
			}
			if _, err := io.Copy(f, tr); err != nil {
				f.Close()
				return fmt.Errorf("write %s: %w", dest, err)
			}
			f.Close()
		case tar.TypeSymlink:
			if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
				return fmt.Errorf("mkdir parent %s: %w", dest, err)
			}
			// Reject absolute or escape-y symlink targets.
			if filepath.IsAbs(hdr.Linkname) || strings.Contains(hdr.Linkname, "..") {
				return fmt.Errorf("symlink target unsafe: %q -> %q", hdr.Name, hdr.Linkname)
			}
			if err := os.Symlink(hdr.Linkname, dest); err != nil {
				return fmt.Errorf("symlink %s: %w", dest, err)
			}
		default:
			// Skip pax headers, devices, fifos, etc.
		}
	}
}
