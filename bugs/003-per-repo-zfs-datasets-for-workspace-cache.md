# Per-repo ZFS datasets for the workspace cache

## Summary

Drawbar's snapshot cache currently puts all repos' workspace state into one pool/dataset namespace. ZFS-LocalPV on top of ZFS supports dataset hierarchies natively; drawbar should use them. Each repo (or `<owner>/<repo>/<workflow>`) gets its own ZFS dataset, which unlocks per-repo quotas, retention, compression tuning, encryption, and cheap "blow away this repo's CI state" semantics. Cross-repo storage contention disappears.

This is the natural next step after bug 001's Option E (split actions cache from workspace cache). That fix gives "two caches, one per shape." This bug pushes further: "many workspace caches, one per repo (or workflow)."

## What you get

- **Per-repo quotas.** `zfs set quota=20G tank/drawbar/workspaces/chaos-inc/zarn`. A runaway zarn build can't starve zfs-workspace.
- **Per-repo retention.** Today drawbar has one global `snapshot.retentionDays`. With per-repo datasets, retention can be a repo-level (or workflow-level) setting. Zarn's huge `target/` snapshots want a few days of history; the blog's Zola-output snapshots want one.
- **Per-repo / per-workflow compression and recordsize.** Rust `target/` is many small object files (smaller recordsize wins). Container build caches are big layered tarballs (bigger recordsize wins). ZFS lets you set both on the same pool, per dataset.
- **Per-repo destroy.** `zfs destroy -r tank/drawbar/workspaces/chaos-inc/zarn` wipes all snapshots and clones for one repo in O(1). Today the operator hunts down VolumeSnapshots by label.
- **Per-workflow snapshot namespacing.** Zarn has `x86_64-linux` and `riscv64-linux-qemu` workflows. They build into the same `target/` directory but with different cargo target triples. They should never share a snapshot. Today the snapshot key (`hashFiles('Cargo.lock')`) is workflow-agnostic, so a warm restore from one might write into the other's expected layout. Per-workflow datasets remove the foot-gun by construction.
- **Encryption keys per dataset** (if you care). ZFS native encryption is per-dataset.

## What it costs

- ZFS-LocalPV's `poolname` is a single value today. Drawbar would need to drive dataset creation itself (call `zfs create tank/drawbar/workspaces/<owner>/<repo>` out-of-band on first use), or work with the openebs-zfs-localpv folks to add a "parent dataset" feature.
- More PVCs flying around. Each per-repo workspace becomes its own PVC bound to its own dataset. Many short-lived clones per CI run.
- More controller logic: "compute the dataset path for `<owner>/<repo>/<workflow>`, ensure it exists, request a PVC against it."

## Suggested taxonomy

```
tank/drawbar/                        — root for all drawbar state
  actions                            — global actions cache (see bug 001 Option E)
  workspaces/                        — per-repo workspace caches
    chaos-inc/
      zarn/
        x86_64-linux/                — per-workflow datasets, one per .gitea/workflows/*.yml
          @<cache-key-1>             — ZFS snapshots, named after drawbar/cache key
          @<cache-key-2>
        riscv64-linux-qemu/
          @<cache-key>
      zfs-workspace/
        blog/
          @<cache-key>
```

Each `@<cache-key>` is a ZFS snapshot. A job restoring from cache:

1. Compute dataset path: `tank/drawbar/workspaces/<owner>/<repo>/<workflow>`.
2. Find newest matching snapshot via the existing `key` + `restore-keys` logic.
3. `zfs clone tank/.../@<key> tank/.../job-<run-id>` (O(1)).
4. Bind-mount the clone into the job pod.
5. On success, `zfs snapshot tank/.../job-<run-id>@<new-key>` and `zfs destroy tank/.../job-<run-id>`.

## Granularity question

Three choices for "what does one dataset cover":

- **Per-repo** (`<owner>/<repo>`): simple, but mixes target triples / cargo profiles / etc.
- **Per-workflow** (`<owner>/<repo>/<workflow>`): right for most cases. Each `.gitea/workflows/*.yml` gets its own dataset. Different builds of the same repo (debug vs release, x86 vs riscv) get isolated workspaces.
- **Per-cache-key**: one dataset per `drawbar/cache@v1.with.key=` value. Maximum isolation, but explodes the dataset count if cache keys aren't stable (e.g. include build timestamps).

Recommended: **per-workflow** as the dataset boundary. Within a workflow, multiple snapshots represent multiple cache keys (`zarn-cargo-<hash>`). This matches how `actions/cache` works on GitHub.

## Implementation sketch

`pkg/k8s/builder.go` job-pod construction:

1. Read repo + workflow from the task context.
2. Resolve the workspace dataset path via a configurable template (default: `{poolName}/drawbar/workspaces/{owner}/{repo}/{workflow}`).
3. Ensure the dataset exists. If openebs-zfs-localpv supports this directly, use that. Otherwise the controller has a small "ensure dataset" helper that calls `kubectl exec` into a tools pod or the host node.
4. The existing snapshot-key restore logic operates within that dataset's namespace.

Helm config:

```yaml
workspaceCache:
  enabled: true
  poolName: tank
  parentDataset: drawbar/workspaces  # under poolName
  granularity: per-workflow          # per-repo | per-workflow | per-cache-key
  defaultQuota: 50Gi                 # applied to each dataset on creation
  defaultRetentionDays: 7
  perRepoOverrides:
    chaos-inc/zarn:
      quota: 100Gi
      retentionDays: 14
```

## Related

- Bug 001 (Option E) is the prerequisite: actions cache must move out of the workspace PVC first. Then this bug splits the workspace PVC into many.
- ZFS-LocalPV upstream issue tracker: there may already be feature requests for "parent dataset" or "dynamic dataset creation"; worth searching before implementing the workaround.

## Acceptance

- A workflow run for `chaos-inc/zarn`'s `x86_64-linux` workflow uses dataset `tank/drawbar/workspaces/chaos-inc/zarn/x86_64-linux/`.
- A workflow run for `chaos-inc/zfs-workspace`'s `blog` workflow uses a different dataset and cannot see zarn's snapshots.
- `zfs list -r tank/drawbar/workspaces` shows one dataset per repo+workflow combination.
- `zfs destroy -r tank/drawbar/workspaces/chaos-inc/zarn` wipes all of zarn's CI state and does not affect other repos.
- Per-repo quotas in helm values are respected.
