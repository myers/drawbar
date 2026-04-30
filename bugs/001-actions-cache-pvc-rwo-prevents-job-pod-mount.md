# `actions-cache` PVC is RWO; controller and job pod cannot share it

## Summary

`pkg/k8s/builder.go:115` adds the controller's cache PVC to job pods as `actions-cache` (read-only). On a cluster where that PVC's StorageClass is `ReadWriteOnce` (typical for local/ZFS storage), the job pod's mount fails because the controller already has the PVC mounted. Result: job pods are stuck in `Init:0/1` with `FailedMount` events; the workflow run sits at "Set up job" indefinitely with no logs.

## Repro

Cluster: single-node k3s with OpenEBS ZFS-LocalPV (`zfs-nvme` storage class, RWO).

Drawbar config:
```yaml
cache:
  enabled: true
  dir: /cache
  port: 9300
snapshot:
  enabled: true
  class: openebs-zfs-snapshot
  storage_class: zfs-nvme
  size: 10Gi
  retention_days: 7
```

Helm `cache.pvc-name` resolves to `drawbar-cache` (the deployment's PVC). The controller mounts it RW; jobs are scheduled to mount it RO.

Push a workflow that uses `actions/checkout@v4`. Drawbar:

1. Clones `actions/checkout` into the cache PVC. ✓
2. Creates the job pod with `actions-cache` volume = same PVC, ReadOnly: true.
3. Job pod schedules; kubelet calls CSI Mount.
4. CSI returns:
   ```
   verifyMount: device already mounted at [...]/<controller-pod-uuid>/...
   ```
5. Job pod stays `Init:0/1` (stuck on `setup-shim` init container).
6. Controller log shows `received task / executing task / created k8s job / pod created`, then no further activity for the run.
7. From the user's perspective: `gt run watch <id>` reports "Set up job" with no logs, indefinitely.

Reproduced 2026-04-30 on `gt.monoloco.net`, drawbar `main-c804f22e`. Symptom looks identical to "FetchTask lost the task" but is a different root cause — verify by `kubectl describe pod drawbar-run-N-XXXXX` and look for FailedMount events.

## Why it can't work as currently designed

`ReadWriteOnce` in CSI semantics means a single node, mounted by a single pod (the lenient interpretation: single node with multiple pods is supported by some drivers; ZFS-LocalPV via OpenEBS is **not** one of them). Even with both pods on the same node, ZFS-LocalPV's CSI driver checks `verifyMount` and rejects a second mount of the same device.

`ReadWriteMany` would lift the restriction but ZFS-LocalPV (and most "local" storage classes — hostPath, local PV, OpenEBS LocalPV variants) don't support RWX. So the design implicitly requires either (a) network storage (NFS/CephFS) or (b) a CSI driver that's lax about same-node multi-mount. Neither is documented.

## Possible fixes (sketches, not committing to one)

### Option A — actions cache as `hostPath` rather than PVC

The actions cache is read-mostly, recoverable (re-cloneable from upstream), per-node-fine. Move it to a `hostPath` mount on the controller's own node. Both controller and job pods mount the same host path; multiple readers on the same FS path "just work."

Trade-off: drawbar becomes node-sticky (controller and jobs must run on the same node), or each node runs its own drawbar instance with a per-node cache.

This is probably the right answer for the deployment shape drawbar targets (single-node k3s with ZFS LocalPV).

### Option B — copy actions into job pod via init container

Drawbar already runs an init container (`setup-shim`). Have it pull the action tarballs from the controller (over its existing service, e.g. via the same cache server already serving on `:9300`) and unpack into an emptyDir. Job pod doesn't mount the cache PVC at all.

Trade-off: per-job re-extraction overhead (~MB-scale, fast). No cross-cutting infrastructure changes.

### Option C — pre-populate cache via a per-job clone PVC

Each job gets its own short-lived PVC populated from a snapshot of the controller's cache. ZFS-LocalPV snapshot-clone is O(1), so this would actually be cheap given the snapshot infrastructure already in place for the workspace cache. Feels symmetric with the existing snapshot-cache story.

Trade-off: more PVCs/snapshots flying around, more CSI ops per job.

### Option D — make `actions-cache` mount conditional on a `--cache-mode` flag

Default to "embed via init container" (Option B), allow `--cache-mode=pvc` for users on RWX-capable storage. Documents the constraint at the config layer.

### Option E — actually two different caches (recommended)

The actions cache and the workspace (target/) cache have completely different access patterns and should not share a backing PVC:

| | actions cache | workspace cache |
|---|---|---|
| Size | small (~MB per action) | large (cargo target/, GB) |
| Writers | controller only | each job |
| Readers | every job (RO) | the job that wrote it |
| Lifetime | persistent | per-job, snapshotted on success |
| Recoverable | yes (re-clone from upstream) | no (would re-build) |
| Mount-mode needed | shared RO from many pods | exclusive RW from one pod |

Today the helm chart's `cache.pvc-name` resolves to a single PVC (`drawbar-cache`) used for both. That's the root mismatch — the actions cache wants RWX-or-equivalent semantics, the workspace cache wants RWO-and-snapshottable.

Concrete shape:

- **`drawbar-actions-cache`** — small, hostPath or RWX. Controller-only writer; jobs mount RO. The fix for *this* bug is "use a hostPath" or "use an emptyDir populated by an init container" specifically for this cache.
- **`drawbar-workspace-cache-<key>`** — what the existing snapshot infrastructure produces. Each job clones from the keyed snapshot, mounts RW, snapshots on success. RWO is fine because each job has its own PVC.

This is Option E because it subsumes A and B: the actions cache uses one of those simpler approaches, and the workspace cache keeps its existing well-fitting design. Helm chart split: `actionsCache.path` (hostPath default `/var/lib/drawbar/actions`) and `workspaceCache.storageClass` (the existing snapshot config).

Recommended for the immediate fix. See bug 003 for the natural next step (per-repo workspace datasets).

## Workaround for the eval (no code change)

Until this is fixed, the job pod cannot start. To unblock the drawbar evaluation on this cluster, either:

- Disable the actions cache in the drawbar config (if that path exists — currently it looks like the code unconditionally mounts the cache when any step uses an action). Worth grepping for a kill switch.
- Restructure the smoke workflow to not use any actions (replace `actions/checkout@v4` with a `run:` git clone). Useful for the smoke test specifically; not viable for real workflows.

## References

- Code: `pkg/k8s/builder.go:113-123` (volume add), `pkg/k8s/builder.go:220-225` (mount add).
- Test that asserts current behavior (and would need to change with any of A/B/D): `pkg/k8s/builder_test.go:102-118`.
- ZFS-LocalPV CSI source of `verifyMount`: openebs/zfs-localpv repo, `pkg/mgmt/volume/volume.go`.

## Acceptance

- A workflow using `actions/checkout@v4` runs to completion on a single-node cluster with RWO-only storage.
- `kubectl describe pod drawbar-run-N-XXXXX` shows no FailedMount events.
- Existing test in `builder_test.go:102` is updated or deleted to match the new architecture.
