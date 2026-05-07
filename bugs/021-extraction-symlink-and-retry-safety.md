# Cache mirror and action fetch follow symlinks / fail on retry

**Status: filed.** Surfaced 2026-05-06 via `/ultrareview` run on PR #1.
Two findings about file extraction into a tree we don't own. Same
toolkit: `O_NOFOLLOW`, atomic-rename-then-promote, idempotent
extraction.

## Finding A — `mergeDir.copyFile` follows symlinks in destination, allowing writes outside the cache

**Location:** `cmd/entrypoint/cache_mirror.go:131-168` (around
`mergeDir`) and the `copyFile` helper.

**The bug.** `mergeDir` iterates entries from `src` and copies them
to `dst`. For regular files it calls
`copyFile(srcPath, dstPath, mode)`, which uses `os.OpenFile(dst, ...)`
under the hood. `os.OpenFile` follows symlinks in the destination
path. So if `dst` already contains a symlink (e.g. `dst/foo →
/etc/passwd`), `copyFile` opens `/etc/passwd` and writes the source
file's contents into it.

The cache directory is filled from prior workflow steps. A step that
ran `actions/checkout` of a malicious repo could plant a symlink
inside the cached tree pointing at any path the runner pod can
write. The next cache-mirror pass writes outside the cache, into
whatever the symlink targets.

**Threat model.** This is a confused-deputy / sandbox-escape vector.
The runner pod runs as root in most CI base images (see CLAUDE.md
note about RunAsNonRoot intentionally not set). The pod's filesystem
includes `/workspace`, `/cache`, `/shim`, `/actions`, plus standard
container directories. A malicious workflow that gets to the
mergeDir step could overwrite `/etc/resolv.conf`, `/shim/manifest.json`,
or other security-relevant files inside the pod.

It does NOT escape the pod (the kernel still enforces container
boundaries), but it lets a malicious workflow tamper with files
outside the cache directory it should be confined to.

**Fix sketch.** `O_NOFOLLOW` on the destination open. If the open
returns ELOOP, treat the symlink as a stale entry and remove it
before retrying:

```go
fd, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC|syscall.O_NOFOLLOW, mode)
if err != nil {
    if errors.Is(err, syscall.ELOOP) {
        if rmErr := os.Remove(dst); rmErr != nil {
            return fmt.Errorf("removing symlink %q before write: %w", dst, rmErr)
        }
        fd, err = os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC|syscall.O_NOFOLLOW, mode)
        if err != nil {
            return err
        }
    } else {
        return err
    }
}
```

Belt-and-braces: also stat the destination tree first and refuse to
proceed if any path component is a symlink to outside `dst`'s root.
That's a bigger change; the `O_NOFOLLOW` minimum is sufficient for
the immediate vulnerability.

**Note.** The reviewer's HTML output also flagged `untarInto` in
`setup.go` for a similar issue (struck through, dismissed). That
dismissal may be wrong — `untarInto` likely has the same
`O_NOFOLLOW` gap. Worth verifying as part of the same fix session.

## Finding B — `fetchAction` retries fail on partial extraction

**Location:** `cmd/entrypoint/setup.go:65-88`, the `fetchAction`
function.

**The bug.** Each retry of `fetchAction` calls
`tryFetch -> untarInto` on the *same* `target` directory without
cleanup. If attempt 1 succeeds in extracting some entries but fails
mid-stream (network drop, server EOF, partial response), the target
directory contains a partial extraction. Attempt 2 tries to recreate
the same files and:

- For symlinks: `os.Symlink(...)` returns `EEXIST`.
- For directories: `os.Mkdir(...)` returns `EEXIST` if a
  same-named regular file was created earlier, or harmlessly if a
  same-named directory exists. Mode bits may differ.
- For regular files: `os.OpenFile(O_CREATE|O_WRONLY|O_TRUNC, ...)`
  truncates and rewrites. Generally OK.

So attempts 2+ reliably fail on the symlink/dir cases, even though
the only thing that went wrong was a network blip.

**Concrete consequence.** Flaky network → workflow always fails
permanently after the first partial fetch, even though the retry
loop *exists* to handle exactly this case.

**Fix sketch.** Two options:

1. **Wipe-and-retry.** Before each attempt, delete `target`
   recursively. Simple, slow on large action sources, correct.

2. **Atomic-rename-then-promote.** Extract to a sibling temp dir
   (`target.partial.<rand>`), and on full success rename it to
   `target`. On failure, delete the temp and retry. The promotion is
   `atomic` per filesystem semantics, so concurrent readers never
   see a partial state.

(2) is the standard pattern for cache populators and is what
`actions/cache@v4` does on the GitHub side. It also pairs nicely with
the cache-mirror code: any subsequent reader of `target` is
guaranteed to see a complete extraction or no extraction at all.

```go
tmp := target + ".partial." + randSuffix()
defer os.RemoveAll(tmp)        // cleanup on any error path
if err := untarInto(tmp, ...); err != nil {
    return err
}
if err := os.Rename(tmp, target); err != nil {
    return fmt.Errorf("promoting %s to %s: %w", tmp, target, err)
}
return nil
```

The `defer os.RemoveAll(tmp)` is no-op on the success path because
`os.Rename` removed `tmp`. Good.

## Why these belong together

Both about extracting untrusted content into a directory tree. Both
need the same lemma: "the destination tree may already contain
adversarial content (symlinks, partial state) and we must defend
against it." Fixing in one PR with one shared helper for safe-write
makes sense; the test patterns are identical (extract into a dir
that contains a symlink / partial state, assert correct behavior).

## Test plan sketch

- A: a unit test for `mergeDir` against a destination tree that
  contains a pre-planted symlink `dst/escape → /tmp/escape-target`.
  Source has `src/escape` regular file with content `pwned`. Assert
  after merge: `/tmp/escape-target` is unchanged AND `dst/escape` is
  a regular file with content `pwned` (or returns an error,
  depending on the chosen fix shape — both are acceptable).
- A bonus: same test against `untarInto` (the dismissed-but-suspect
  case from the reviewer).
- B: a unit test for `fetchAction` where the first call's
  `tryFetch` is mocked to extract two files then return an error,
  and the second call succeeds. Assert the final tree has all
  expected files.

## Source

Filed via `/ultrareview` run on PR #1, 2026-05-06.

## Related

- The "three caches" section in CLAUDE.md: this concerns the
  actions-source cache, NOT the artifact cache or workspace
  snapshot. Fix should not touch the other two.
- Bug 008 (cache-mount-vs-checkout-init-ebusy) is in adjacent
  territory: cache mount semantics. Different mechanism though.
