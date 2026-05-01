# `drawbar/cache@v1` mount-into-/workspace conflicts with `actions/checkout@v4`

## Summary

When a workflow uses `drawbar/cache@v1` with a `path:` that lives inside
`/workspace` (e.g. `target`, `node_modules`, `dist`), `actions/checkout@v4`
fails on its very first run with:

```
Deleting the contents of '/workspace'
::error::File was unable to be removed Error: EBUSY: resource busy or locked, rmdir '/workspace/target'
```

The cache integration itself works — the controller correctly extracts
`key`, `path`, and `restore-keys` from the magic action's `with:` values,
provisions a `cache-<task>` PVC, and emits the `snapshot cache PVC ready`
log line. But the workspace bind-mount is in place *before* the runner
container starts, so when checkout runs its built-in workspace clear it
hits the live PVC mount and EBUSYs.

The result: with this bug, `drawbar/cache@v1` is unusable for the most
common case — caching build artefacts that live under the repository
working tree (Cargo's `target/`, npm's `node_modules/`, etc.). Acceptance
criterion #4 of the gt evaluation plan ("Snapshot caching speedup ≤ 50%
of cold") is blocked on this.

## Repro

Date: 2026-05-01. Image: `ghcr.io/myers/drawbar:main-1777645990-2e81477b`.
Workflow: `chaos-inc/zarn` `.gitea/workflows/x86_64-linux.yml` at commit
`ad77374` (gt run #26).

Workflow excerpt:

```yaml
- uses: drawbar/cache@v1
  with:
    key: cargo-x86_64-${{ hashFiles('**/Cargo.lock') }}
    path: |
      ~/.cargo
      target
    restore-keys: |
      cargo-x86_64-

- uses: actions/checkout@v4
  with:
    lfs: true
    clean: false
```

Drawbar controller log over the run:

```
{"msg":"created empty workspace PVC (cache miss)","pvc":"cache-32"}
{"msg":"snapshot cache PVC ready","paths":["~/.cargo","target"],"restored":false}
```

Then the runner container starts, checkout runs, and immediately:

```
Deleting the contents of '/workspace'
::error::File was unable to be removed Error: EBUSY: resource busy or locked, rmdir '/workspace/target'
```

Reproduces 100% on cold runs. `clean: false` on checkout does **not**
help — that option controls the *post*-checkout `git clean -ffdx`, not
the initial workspace clear which always runs.

## Why it happens

In `pkg/k8s/builder.go:202-211`, drawbar adds a `VolumeMount` for each
declared cache path directly inside `/workspace`:

```go
for _, cachePath := range cfg.SnapshotPaths {
    if cfg.SnapshotPVCName != "" {
        runnerMounts = append(runnerMounts, corev1.VolumeMount{
            Name:      "snapshot-cache",
            MountPath: "/workspace/" + cachePath,
            SubPath:   cachePath,
        })
    }
}
```

This is a kernel-level bind mount on top of the runner container's
filesystem. The mount is established by the kubelet before the runner
container's entrypoint runs, so by the time `actions/checkout@v4` is
invoked, `/workspace/target` is already a busy mountpoint. `rmdir` on a
busy mountpoint fails with EBUSY — there is no way for an in-container
process to dismiss this without `umount`, which would require
`CAP_SYS_ADMIN` (drawbar's containers drop ALL capabilities — see
`builder.go:65-70`).

There is also a related but separate bug in path handling: `~/.cargo`
passes the `extractCacheInfo` filter at `cmd/controller/main.go:998`
(no leading `/`, no `..`), then becomes `MountPath: "/workspace/~/.cargo"`
— a literal `~` directory rather than the user's home. That's tracked
separately; this bug is specifically about paths that conflict with the
workspace clear.

## Sibling bug — the `~/.cargo` path doesn't expand

Out of scope here but worth noting: the same workflow's `~/.cargo`
entry doesn't conflict with checkout (it's outside `/workspace`), but
it also doesn't *cache* `~/.cargo` — it caches a directory called
`~`/.cargo *under* `/workspace`. That's user-confusing and also makes
the cache speedup useless for cargo's registry. File a separate bug
once 008 is fixed.

## Fix space

The mount has to either move out of `/workspace` entirely, or be
re-established after checkout finishes. Workflow-side workarounds were
considered and rejected because they defeat the speedup we're trying
to measure, and they push complexity onto every consumer of
`drawbar/cache@v1`.

### Option A — Mount the PVC at a sibling path; symlink after checkout

Mount the cache PVC at `/snapshots/<key>/` (or just `/cache/`) and
either:

1. After checkout completes, have the entrypoint create symlinks
   `/workspace/<path>` → `/cache/<path>`. This works for cargo's
   `target` (cargo follows symlinks fine) and for most build tools.
2. Or, document that workflows must symlink themselves after
   checkout. Brittle; rejected.

(1) requires a hook between steps in the entrypoint. The current
entrypoint in `cmd/entrypoint/main.go` runs steps in a simple `for`
loop and has no notion of "post-checkout" — but it does know which
step is which. A simple rule: after the first step that has
`uses: actions/checkout` (or any step ID that matches a small set),
run the symlink phase.

Pros: clean, no privileged operations, no Kubernetes-level changes.
Cons: a little magic in the entrypoint; symlinks have edge cases
(some tools `realpath` and break, though this is rare for `target/`).

### Option B — Don't mount at all; rsync into /workspace after checkout

Mount the PVC at `/cache/` read-write, and after checkout rsync
the contents into `/workspace/<path>`. On step end, rsync back.

Pros: no symlinks, no mount conflicts, works for tools that
`realpath`.
Cons: doubles the disk usage during the run and adds copy time,
which directly defeats the "ZFS snapshot speedup" claim. Also
adds a "snapshot back to PVC" path that has to be careful about
file ownership and timestamps. Rejected.

### Option C — Make the mount lazy via an init script

Mount at `/cache/`, then have a privileged init container (or
the entrypoint with bind-mount capability) bind-mount
`/cache/<path>` into `/workspace/<path>` after checkout. This is
just option A but using mount(2) instead of symlinks.

Pros: fully transparent to the tool.
Cons: requires `CAP_SYS_ADMIN` in the runner container or a
sidecar that has it. Drawbar's hardening explicitly drops ALL
caps. We'd have to mark a single container or do the mount from
the controller side via `kubectl exec`, which is ugly.
Rejected unless A turns out to have real problems.

### Recommendation

**Option A.** The implementation is small and contained:

- `builder.go`: change the bind-mount target from
  `/workspace/<path>` to `/cache/<path>` (no SubPath), keep the
  PVC mount as-is.
- `entrypoint/main.go`: after each step finishes, if it was a
  checkout-style step, ensure `/workspace/<path>` is a symlink to
  `/cache/<path>`. Idempotent: if the symlink already exists, do
  nothing; if `/workspace/<path>` exists as a real directory
  (e.g. checkout recreated it), `mv` its contents into
  `/cache/<path>` and replace it with a symlink.

Detection of "checkout-style step": match `uses:` against
`actions/checkout@*` and similar (forgejo-actions/checkout,
gitea/checkout). Or, simpler: do the symlink-or-merge step
*before every non-cache step*, idempotently — the cost is one
`lstat` per step.

A simpler alternative is to do the symlink work *once*, just
before the first non-cache step runs. That's enough because the
EBUSY only happens when checkout's "delete /workspace" hits a
live mountpoint; once the mount is at `/cache/` instead, checkout
runs cleanly and we just need to make sure the cached paths are
visible under `/workspace` afterwards.

## Acceptance for the fix

1. zarn x86_64 workflow runs with `drawbar/cache@v1` (`path: target`,
   `path: ~/.cargo`) and `actions/checkout@v4` succeeds end-to-end.
2. Cold run produces a snapshot; warm run mounts that snapshot and
   `cargo build` skips already-built crates (visible from cargo's
   "Compiling" / "Fresh" output).
3. Warm-run total time is ≤ 50 % of cold-run total time on the
   `zarn-dmu` test job (acceptance criterion #4 of the eval plan).
4. Existing drawbar tests pass; new tests cover the symlink/merge
   behaviour in `cmd/entrypoint/`.
