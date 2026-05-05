# Controller's RWO cache PVC blocks rolling update on image bump

**Status: fixed.** Landed in commit `64c85c8` — `deploy/helm/drawbar/templates/deployment.yaml`
sets `strategy.type: Recreate` (Option A). Any consumer using drawbar's
Helm chart picks this up automatically. Verification against upstream
`act_runner` on 2026-05-05: their `examples/kubernetes/{dind-docker,rootless-docker}.yaml`
both ship RWO PVC + default (rolling) Deployment strategy and have the
same wedge — drawbar's chart is now ahead of theirs.

Note: consumers running off a hand-rolled Deployment (e.g. fluxing's
`apps/base/gitea/drawbar/deployment.yaml`) still need their own
`strategy: Recreate` patch; the chart fix only covers chart users.

## Summary

The drawbar controller's `cache` PVC (mounted at `/cache` for the
HTTP-served actions-source cache + ZFS snapshot infrastructure) uses an
RWO StorageClass. The Deployment's default `RollingUpdate` strategy
spawns a new pod **before** terminating the old one (`maxSurge: 25%`,
`maxUnavailable: 25%` rounded up = at least 1 surge pod), so the new
pod tries to mount the same PVC the old pod still holds and gets stuck
in `ContainerCreating` with `FailedMount: device already mounted` for
the entire `terminationGracePeriodSeconds` window — typically several
minutes — before kubelet finally evicts the old pod and the new one
can attach.

Every drawbar image bump therefore requires the operator to manually
`kubectl delete pod` the old replica to break the deadlock. This was
documented as a "known dance" in the May 1 handoff and bit us again on
2026-05-04 (image bump from `b1b9999` → `df969d6b` — surge pod sat in
`ContainerCreating` for 4+ minutes until manual delete) and again on
2026-05-05 (`b1b9999e` → `65709098`, same shape).

## Why it happens

- `apps/base/gitea/drawbar/pvc.yaml` requests RWO (it has to — the
  StorageClass `zfs-nvme` is `WaitForFirstConsumer` and ZFS-backed
  hostPath volumes are inherently single-node single-mount).
- `apps/base/gitea/drawbar/deployment.yaml` has no
  `strategy.type: Recreate` override, so it uses the default
  `RollingUpdate`. K8s scheduler tries to start the new replica
  while the old one is still Running.
- The cache PVC is the controller's own state, not a job pod's
  workspace — bug 001 fixed the RWO problem for *job* pods (HTTP
  served actions cache, no shared mount). The controller's own mount
  was never in scope for that fix.

## Reproduction

1. Bump `image:` in `apps/base/gitea/drawbar/deployment.yaml`.
2. `flux reconcile kustomization apps`.
3. `kubectl -n gitea get pods -l app=drawbar` shows 2 pods, one Running
   and one stuck:
   ```
   NAME                       READY   STATUS              RESTARTS
   drawbar-OLD                1/1     Running             0
   drawbar-NEW                0/1     ContainerCreating   0
   ```
4. `kubectl describe pod drawbar-NEW` shows:
   ```
   Warning  FailedMount  ... MountVolume.SetUp failed for volume "pvc-..." :
   verifyMount: device already mounted at [/var/lib/kubelet/pods/<old>/...]
   ```
5. Manual recovery: `kubectl delete pod drawbar-OLD`. New pod
   immediately attaches the PVC and goes Ready.

## Fix space

### Option A — `strategy: Recreate`

Add to `apps/base/gitea/drawbar/deployment.yaml`:

```yaml
spec:
  strategy:
    type: Recreate
```

This tells K8s to terminate the old replica before starting the new
one, avoiding the surge altogether. Cost: a few seconds of "no
controller running" between pods, during which any in-flight job-pod
loses its log-stream sink (the reporter daemon flushes from inside the
job pod itself, but the controller is what watches the job and surfaces
results). The window is short and the controller currently can't
continue a task across restarts anyway, so this is mostly free.

This is the cleanest fix for the current single-replica deployment.

### Option B — split the PVC concerns

The `/cache` PVC currently holds two distinct things:

1. **Actions-source cache** (compiled tarballs of fetched actions like
   `actions/checkout@v4`) — read-mostly, regenerable, not load-bearing.
2. **ZFS snapshot metadata** (the per-task snapshot/clone bookkeeping
   that survives across tasks).

If (1) moved to an `emptyDir` or a separate small PVC, and (2) stayed
on the RWO PVC but with a `Recreate` strategy applied, you'd lose
nothing meaningful. Currently they're commingled at `/cache`.

This is more work than it's worth unless you also want to support
controller HA, which would need (2) to live somewhere shared (a
ReadWriteMany volume or external state).

### Option C — `accessModes: [ReadWriteOncePod]`

K8s 1.27+ supports `ReadWriteOncePod`, which is even stricter than
RWO — but the ZFS-LocalPV CSI driver on this cluster does not yet
implement it (returns RWO regardless). Not viable today.

## Recommendation

**Option A.** One-line patch to fluxing's
`apps/base/gitea/drawbar/deployment.yaml`. Solves the immediate
operational annoyance, doesn't preclude Option B if controller HA
ever becomes a goal.

## Related

- Bug 001 (`actions-cache PVC RWO prevents job pod mount`) — already
  fixed by switching the actions-source cache to HTTP. That bug was
  about *job pods* sharing the controller's PVC. This bug is about
  *controller pods* sharing it across rolling updates — different
  failure mode, same RWO root cause.
- Mentioned in `2026-05-01-handoff-drawbar-eval.md` "Things that look
  weird but are intentional" → "drawbar runner pod has to be manually
  deleted after image bumps until drawbar bug 003 (or just splitting
  the cache PVCs) lands." That memory entry can be removed once this
  bug is fixed.
- Memory entry to remove after fix lands:
  `~/.claude/projects/-Users-myers-p-fluxing/memory/MEMORY.md` — the
  "manually delete after image bumps" note in the project drawbar
  evaluation memory.
