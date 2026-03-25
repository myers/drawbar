# Gap Analysis: drawbar — Production Readiness

Updated: 2026-03-23

## Completed

All items from the initial gap analysis (vs act_runner) are done:

- Workflow commands (`::add-mask::`, `::group::`, `::debug::`, etc.)
- Exponential backoff on connection failures
- Task versioning (was already implemented)
- Ephemeral mode (`RUNNER_EPHEMERAL=true`)
- Reporter exponential backoff
- Nested composite action expansion
- Config validation
- Dependencies swapped from Forgejo GPLv3 to Gitea MIT
- ZFS snapshot cache with bind mounts + restore-keys
- BuildKit sidecar for container builds (auto-detected, rootless, `drawbar/build-push` action)
- OIDC token support (`ACTIONS_ID_TOKEN_REQUEST_*` from Gitea task context)
- GITHUB_* env vars injected into all steps
- Node actions run via direct exec (preserves hyphenated INPUT_* env vars)
- Artifact upload/download verified E2E (v3 actions, text + 100KB binary, multi-job with `needs:`)
- Documentation (README, Helm values, BuildKit guide, ZFS cache guide, artifact guidance)

## Remaining Gaps — Production Blockers

### 1. Production hardening

**Impact**: Unknown failure modes in real workloads.

Zero production users means zero real-world bug reports. Need sustained use on a real project.

**Work needed**:
- Deploy on a real project (Rust CI with BuildKit for container builds)
- Run for 2-4 weeks, fix issues as they arise
- Monitor: job success rate, cache hit rate, pod scheduling latency

## Architecture Advantages (vs act_runner)

These are permanent differentiators, not gaps to close:

| Feature | drawbar | act_runner |
|---------|---------|------------|
| Security | Drop ALL caps, no Docker socket | Requires privileged DinD |
| Build cache | ZFS snapshot bind mounts (instant) | HTTP tar.gz (minutes) |
| K8s integration | Native Jobs, RBAC, health probes | Pod with DinD sidecar |
| Scaling | Ephemeral mode + KEDA | Process-level only |
| Checkout | actions/checkout@v4 via direct exec | External action (node.js) |
| Container builds | BuildKit sidecar (rootless) | DinD (privileged) |

## Won't Fix

- **Docker actions** — replaced by BuildKit sidecar pattern, which is more secure
- **Host execution mode** — we're K8s-native, that's the value prop
- **Per-step containers** — single container is simpler and sufficient
- **GITHUB_STEP_SUMMARY** — Gitea's runner proto (`StepState`, `UpdateTaskRequest`) has no summary field; server-side limitation
