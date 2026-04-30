package main

import (
	"archive/tar"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
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
func fetchAction(a types.ActionFetch, actionsDir string) error {
	target := filepath.Join(actionsDir, a.Dir)
	if err := os.MkdirAll(target, 0o755); err != nil {
		return fmt.Errorf("mkdir target: %w", err)
	}

	client := &http.Client{Timeout: setupHTTPTimeout}

	var lastErr error
	for attempt := 1; attempt <= setupMaxAttempts; attempt++ {
		err := tryFetch(client, a.URL, target)
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
			f, err := os.OpenFile(dest, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, os.FileMode(hdr.Mode)&0o777)
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
