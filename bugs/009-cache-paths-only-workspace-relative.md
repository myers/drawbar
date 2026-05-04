# `drawbar/cache@v1` silently no-ops on `~/`-relative or absolute paths

## Summary

`drawbar/cache@v1`'s `path:` declarations are documented to mirror the
GitHub `actions/cache@v4` shape, where `~/.cargo/registry`,
`~/.npm`, `/var/cache/apt` etc. are common and load-bearing. drawbar's
entrypoint (`cmd/entrypoint/cache_mirror.go`) treats every declared
path as `/workspace`-relative — `~` is not expanded, absolute paths are
rejected. Workflows that follow the obvious idiom of caching the *real*
location of a tool's cache (`~/.cargo/...`, `~/.m2/...`, etc.) get a
silent miss: the snapshot stores empty directories at
`/cache/~/.cargo/...`, the symlink at `/workspace/~/.cargo/...` points
nowhere useful, and the tool itself reads/writes its actual location
which never participates in the snapshot. **Cold and warm runs are
indistinguishable.**

## Reproduction

Workflow `chaos-inc/bevy_xr_nitro` `.gitea/workflows/cargo-build.yaml`,
gt run #58 (commit `61ac595`), exact hit on `snap-63`:

```yaml
- uses: drawbar/cache@v1
  with:
    key: bevy-xr-nitro-cargo-full
    path: |
      ~/.cargo/registry
      ~/.cargo/git
      target
```

drawbar logs (correct shape, wrong content):

```
snapshot cache hit (exact)  snapshot=snap-63  key=bevy-xr-nitro-cargo-full
snapshot cache PVC ready    paths=["~/.cargo/registry","~/.cargo/git","target"]  restored=true
```

But cargo's actual `$HOME/.cargo` lives at `/root/.cargo` (the runner
runs as root inside its userns). `/workspace/~/.cargo` is a literal
directory named `~`. So warm run #58:

```
+ cargo build --workspace --tests
    Updating git repository `https://fj.monoloco.net/chaos-inc/bevy.git`     # full re-fetch
    Updating git repository `https://fj.monoloco.net/chaos-inc/bevy_oxr.git` # ...
     Locking 90 packages to latest compatible versions                       # full re-resolve
   Compiling proc-macro2 v1.0.106                                            # rebuild from base
   Compiling unicode-ident v1.0.24
   ... (all 600+ deps recompiled)
```

Cargo build time: 4m 21s warm, vs 4m 5s on the previous (also-uncached
because of the same bug) run. Effectively zero speedup.

## Workaround proven to work

gt run #61 (commit `70308bb`) sets:

```yaml
env:
  CARGO_HOME: /workspace/.cargo
  CARGO_TARGET_DIR: /workspace/target
# ...
- uses: drawbar/cache@v1
  with:
    key: bevy-xr-nitro-cargo-full-v2
    path: |
      .cargo/registry
      .cargo/git
      target
```

Warm run #61 result: cargo build **29s** vs cold #60 **4m 32s**. 89%
reduction. Cache works as designed once paths land inside `/workspace`.

## Fix space

The minimum fix is to **document** the workspace-relative-only constraint
prominently — the README example uses `~/.cargo` so users naturally
copy that idiom from `actions/cache`. But a silent miss is hostile;
the entrypoint should at least:

1. **Reject absolute paths and `~`-prefixed paths at config time**
   (controller side), not silently treat them as relative. Surface the
   error in the controller log and fail the job. Or:

2. **Expand `~` against `$HOME` of the runner container** and mount
   that absolute path under `/cache/<absolute-without-leading-slash>/`,
   then symlink the real `$HOME/.cargo` etc. The controller doesn't
   know `$HOME` ahead of pod start, but the entrypoint runs inside the
   container and can read its own `$HOME` before symlinking. This makes
   `~/.cargo/registry` Just Work and matches user expectation.

3. **Accept absolute paths** with a sandbox check (must be under one of
   a few allowed prefixes: `$HOME`, `/var/cache/...`, `/usr/local/...`)
   so workflows can cache `/var/cache/apt/archives` and similar without
   smuggling them through `$HOME`.

Option 2 is the pit-of-success choice: existing `actions/cache`
workflows port directly. The cost is one `os.UserHomeDir()` call in
`mirrorCachePaths` and a small filepath rewrite.

## Related

- Bug 008 (`drawbar/cache@v1` vs `actions/checkout@v4` EBUSY) was the
  prior step — the cache mount now lives at `/cache` and is symlinked
  into `/workspace` per-step. This bug is the natural follow-up: the
  symlink target machinery only handles workspace-relative paths.
- Verified end-to-end against the bevy_xr_nitro full-workspace build
  (18 members, 600+ deps including bevy_bigfoot, nitro_audio,
  nitro_video, video-demo*) — drawbar's first eval workload that
  exercises a non-trivial cache hit, and it surfaced this issue
  immediately.

## Resolution

Fixed in commits b03f5c4..55f0552 (2026-05-03). `extractCacheInfo`
now accepts workspace-relative, `~/`-prefixed, and absolute paths
verbatim and rejects `..` traversal as a job failure. `mirrorOne`
classifies via a new `resolvePath` helper: `~/.cargo/registry`
expands against `$HOME` and mirrors to
`/cache/<HOME-stripped>/.cargo/registry`; `/var/cache/apt` mirrors
to `/cache/var/cache/apt`. `$HOME` unset on a `~/...` path fails
the job loudly via stderr + non-zero exit. See spec at
`docs/superpowers/specs/2026-05-03-cache-paths-home-and-absolute-design.md`
and plan at
`docs/superpowers/plans/2026-05-03-cache-paths-home-and-absolute.md`.
