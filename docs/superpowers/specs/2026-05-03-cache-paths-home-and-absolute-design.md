# `drawbar/cache@v1`: support `~/` and absolute paths

Bug: `bugs/009-cache-paths-only-workspace-relative.md`

## Problem

`drawbar/cache@v1` documents itself as a drop-in for `actions/cache@v4`,
where `path:` entries like `~/.cargo/registry`, `~/.npm`, and
`/var/cache/apt` are common. drawbar's entrypoint (`mirrorCachePaths`)
treats every declared path as `/workspace`-relative: `~` is not
expanded, absolute paths are rejected per-path. The controller
(`extractCacheInfo`) silently drops anything starting with `/` or
containing `..`, but `~/.cargo/registry` slips through (no leading
slash, no `..`) and ends up as a literal `~` directory under
`/workspace/~/.cargo/...`. The tool's real cache at `$HOME/.cargo`
never participates in the snapshot and warm runs are
indistinguishable from cold runs.

## Goals

- Existing `actions/cache@v4` workflows port to `drawbar/cache@v1` without
  rewriting `path:` entries.
- No silent misses. A path that can't be cached produces a visible
  failure (job-level), not a quiet no-op.
- No new config knobs. Behavior is determined by the path string.

## Non-goals

- Changing the snapshot scope (still the whole `/cache` PVC).
- Changing the snapshot key (still `(repo, cache-key)`).
- Changing the manifest schema (`Manifest.CachePaths` stays a `[]string`
  carrying the unresolved entries).
- Operator-tunable allow-lists for absolute paths. The workflow author
  knows where their tool's cache lives; we don't second-guess.

## Path semantics

Three accepted shapes, classified by the entrypoint at runtime:

| Shape              | Example                | Workspace location          | Cache location                            |
| ------------------ | ---------------------- | --------------------------- | ----------------------------------------- |
| Workspace-relative | `target`               | `/workspace/target`         | `/cache/target`                           |
| Home-relative      | `~/.cargo/registry`    | `$HOME/.cargo/registry`     | `/cache/<HOME-stripped>/.cargo/registry`  |
| Absolute           | `/var/cache/apt`       | `/var/cache/apt`            | `/cache/var/cache/apt`                    |

Layout is verbatim: `$HOME=/root` produces `/cache/root/.cargo/...`. A
base-image change that moves `$HOME` will silently miss the prior
warm cache; this is honest behavior — the files aren't where the tool
looks for them either.

Rejected at controller-build time (job fails before pod starts):

- Any path containing `..` as a segment.
- Empty entries (today's behavior, kept).

Rejected at runtime (entrypoint exits non-zero before any step):

- A `~/...` path when `$HOME` is unset or empty in the runner
  container. Rare; usually means a misconfigured base image.

## Architecture

Two-binary fix along the existing controller/entrypoint boundary.

```
workflow YAML  ->  step.Env["INPUT_PATH"]
            ->  controller: extractCacheInfo (validate shape)
                  - reject `..` segments  -> fail job
                  - accept ws-rel | ~/-prefixed | absolute
            ->  Manifest.CachePaths (unchanged schema, []string)
            ->  entrypoint: mirrorCachePaths (resolve + symlink)
                  - resolvePath(rel) -> (wsPath, cachePath)
                  - $HOME unset on ~/... -> emit state error, exit 1
                  - per-path mirror failure -> log, continue
            ->  /cache/... ends up in snapshot at job end
```

## Components

### Controller: `extractCacheInfo` (`cmd/controller/main.go`)

Signature change:

```go
func extractCacheInfo(steps []types.StepSpec) (
    key string, paths []string, restoreKeys []string, err error,
)
```

Validation per `path:` entry:

1. Trim whitespace, skip empty.
2. Reject if any segment of the path is `..` →
   `fmt.Errorf("cache path %q contains '..'", entry)`.
3. Otherwise pass through unchanged. Workspace-relative,
   `~/`-prefixed, and absolute all flow through to the manifest as-is.

The caller in the controller's task handler logs
`slog.Error("invalid cache path", "error", err)` and reports the task
as failed via the existing `UpdateTask` failure path. No PVC is
created, no pod is started.

### Entrypoint: `mirrorCachePaths` / `mirrorOne` (`cmd/entrypoint/cache_mirror.go`)

New helper and sentinel error:

```go
// errHomeRequired is returned when a ~/-prefixed path is encountered
// but $HOME is empty. Caller (mirrorCachePaths) treats it as fatal.
var errHomeRequired = errors.New("$HOME required but unset")

// resolvePath classifies the declared path and returns the workspace
// location (where the symlink lives) and the cache location (the
// symlink's target, where files are actually stored). home is the
// runner's $HOME at call time; pass "" only when the path is known
// not to need it (caller checks classification).
func resolvePath(workspace, cache, home, rel string) (wsPath, cachePath string, err error) {
    switch {
    case strings.HasPrefix(rel, "~/") || rel == "~":
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
```

`mirrorOne` becomes:

```go
func mirrorOne(workspace, cache, home, rel string) error {
    if rel == "" {
        return fmt.Errorf("empty path")
    }
    wsPath, cachePath, err := resolvePath(workspace, cache, home, rel)
    if err != nil {
        return err
    }
    // ... existing mkdir + lstat + symlink + merge logic, unchanged ...
}
```

`mirrorCachePaths` becomes:

```go
func mirrorCachePaths(paths []string) error {
    home := os.Getenv("HOME")
    var fatal error
    for _, p := range paths {
        if err := mirrorOne(workspaceRoot, cacheMirrorRoot, home, p); err != nil {
            // $HOME-related failures are job-fatal; per-path filesystem
            // errors are best-effort.
            if errors.Is(err, errHomeRequired) {
                fatal = err
                break
            }
            fmt.Fprintf(os.Stderr, "cache mirror %q: %v\n", p, err)
        }
    }
    return fatal
}
```

Caller (`cmd/entrypoint/main.go`) treats a non-nil return as a
job-level error: emit a `StateEvent{Event: "error", ...}` and exit 1
before the step loop continues.

### k8s builder + types: no schema change

`Manifest.CachePaths` stays `[]string`. The strings are unresolved
entries from the workflow; resolution is the entrypoint's job because
only it knows `$HOME`.

### `actions/cache/action.yml` description

Update the `path:` input description from "Paths to cache, relative
to workspace. One per line." to call out the three accepted shapes
and re-mention `actions/cache@v4` parity.

## Error handling summary

| Condition                              | Where      | Outcome                                        |
| -------------------------------------- | ---------- | ---------------------------------------------- |
| `..` segment in a path                 | Controller | Job fails before pod starts; clear log.        |
| `~/...` path, `$HOME` unset            | Entrypoint | Emit state error; exit 1 before step loop.     |
| Per-path filesystem error (mkdir, etc) | Entrypoint | Log to stderr, skip path, continue. (today)    |
| Workspace path is a real file          | Entrypoint | Leave alone, continue. (today)                 |

Every path either ends up mirrored or causes a visible failure. No
silent drops anywhere in the pipeline.

## Testing

New tests in `cmd/entrypoint/cache_mirror_test.go`:

- `TestMirrorOne_HomeRelativePath` — `$HOME=<tempdir>`, mirror
  `~/.cargo/registry`, assert symlink at `<HOME>/.cargo/registry` →
  cache path with `<HOME-stripped>` prefix.
- `TestMirrorOne_HomeRelativePathBareTilde` — mirror `~`, asserts
  the boundary case.
- `TestMirrorOne_AbsolutePath` — mirror an absolute path under a
  tempdir (for hermeticity), assert symlink at the absolute path →
  `/<cache>/<path-stripped>`.
- `TestMirrorOne_AbsolutePathWithExistingDir` — pre-create content
  at the absolute path, assert merge + symlink works the same as the
  workspace-relative case.
- `TestMirrorCachePaths_FailsOnUnsetHome` — `$HOME=""`, pass
  `~/.cargo`, assert the error return.
- Existing `TestMirrorOne_RejectsAbsolutePath` is **removed** —
  absolute paths are now valid input.

The existing workspace-relative tests are updated to pass the new
`home` argument (any non-empty string works) and otherwise stay
unchanged.

New tests for the controller's `extractCacheInfo`:

- Rejects `..` traversal with a clear error.
- Accepts `~/...`, absolute, and workspace-relative shapes; returns
  them verbatim in the `paths` slice.

End-to-end validation is the `bevy_xr_nitro` workflow from the bug:
warm runs after the fix should produce the ~89% speedup the
workaround already demonstrated, this time without rewriting the
workflow.

## Migration

None required. Workflows currently using the workspace-relative
workaround (`CARGO_HOME=/workspace/.cargo` + `path: .cargo/registry`)
keep working; they can be reverted to the natural `actions/cache`
idiom on the author's schedule.
