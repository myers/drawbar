# Future Ideas

## ZFS Snapshot Cache

**Problem**: The current HTTP cache (act/artifactcache) transfers tar.gz archives over HTTP. For large Rust `target/` directories (1-2 GB), this adds minutes to every CI run even on local networking.

**Idea**: Replace HTTP tar.gz cache with ZFS snapshot/clone via the k8s VolumeSnapshot API.

### How It Would Work

- **Cache save**: `zfs snapshot pool/cache/workspace@rust-{hash}` — O(1) regardless of size
- **Cache restore**: `zfs clone pool/cache/workspace@rust-{hash} pool/jobs/job-42` — instant, copy-on-write
- **Deduplication**: Two branches with 95% identical `target/` dirs share blocks via CoW
- **No network I/O**: The workspace PVC *is* the cache — no tar, no HTTP, no compress

### k8s Integration

- Use [OpenEBS ZFS LocalPV](https://github.com/openebs/zfs-localpv) as the CSI driver
- Create `VolumeSnapshot` from job workspace PVC after successful build
- Create new PVC from `VolumeSnapshot` for the next job (ZFS clone underneath)
- Controller maintains a map of `cache-key → VolumeSnapshot name`
- All through standard k8s APIs (VolumeSnapshot CRD, PVC from snapshot)

### Prerequisites

- ZFS-backed k8s nodes
- OpenEBS ZFS LocalPV CSI driver installed
- VolumeSnapshot CRD (GA since k8s 1.20)
- `actions/cache` action becomes unnecessary — the whole workspace is cached

### Benefits

| | HTTP Cache | ZFS Snapshot |
|---|---|---|
| Save | tar.gz + HTTP upload (minutes) | snapshot (milliseconds) |
| Restore | HTTP download + extract (minutes) | clone (milliseconds) |
| Storage | Full copy per key | CoW, shared blocks |
| Network | All data over HTTP | Zero |

### Implementation Sketch

1. After job completes successfully, create a `VolumeSnapshot` of the workspace PVC
2. Tag the snapshot with the cache key (label or annotation)
3. Before next job starts, check if a matching snapshot exists
4. If yes, create the workspace PVC from the snapshot (instant clone)
5. If no, create a fresh empty PVC (cache miss)
6. GC: delete snapshots older than retention period

This would be the killer feature that makes forgejo-k8s-runner worth choosing over every other runner for Rust, C++, or any language with large build artifacts.

## Kubernetes User Namespaces for Job Pods

**Problem**: CI job pods currently run without `RunAsNonRoot` because popular images like `node:22-bookworm` run as root. While we drop all capabilities and disallow privilege escalation, the container still has UID 0 inside.

**Idea**: Use Kubernetes User Namespaces (`hostUsers: false` on the pod spec) to map container root (UID 0) to an unprivileged UID on the host. The container thinks it's root and can install packages, but has zero real host privileges.

### Prerequisites

- Kubernetes 1.30+ (User Namespaces GA)
- Linux kernel 6.3+ on nodes (for idmap mounts)
- Container runtime support (containerd 1.7+)

### Implementation

Set `hostUsers: false` in the job pod's `PodSecurityContext` (in `BuildJob`). This is a one-line change once the cluster supports it. Consider making it configurable via Helm values since not all clusters will have the prerequisite kernel version (e.g., Ubuntu 22.04 ships Linux 5.15).

## Action Cache Expiration

The controller caches cloned action repos on the PVC at `/cache/actions-repo-cache/`. Currently these are cached forever (clone once, reuse forever). This needs a TTL-based expiration system:

- **Problem**: Actions get updated (security fixes, new features). Pinned versions (`@v4`) point to tags that don't change, but `@main` or `@latest` references would serve stale code forever.
- **Solution**: Add a configurable TTL (default: 7 days). On each LoadAction call, check the clone's age. If older than TTL, re-clone. Store last-fetched timestamp in a sidecar file (e.g., `.cache-timestamp`).
- **Tag vs branch**: Tags are immutable — could cache forever. Branches should have shorter TTL.
- **Manual invalidation**: Add a CLI command or API endpoint to flush the action cache.
- **Multi-node**: If using shared storage (NFS/CephFS), need file-level locking to prevent concurrent re-clones.
