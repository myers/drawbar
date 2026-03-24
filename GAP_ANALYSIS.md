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

## Remaining Gaps — Production Blockers

### 1. Artifact upload/download verification

**Impact**: Multi-job workflows that pass data between jobs won't work if broken.

**Status**: Upload verified, download needs second-job dispatch testing.
- All env vars injected correctly: `ACTIONS_RUNTIME_URL`, `ACTIONS_RESULTS_URL`, `ACTIONS_RUNTIME_TOKEN`, `GITHUB_*`
- `actions/upload-artifact@v4` does NOT work — throws `GHESNotSupportedError` for non-github.com servers
- `actions/upload-artifact@v3` works — successfully uploads artifacts via the pipeline API
- Gitea's `ROOT_URL` must match the k8s service URL for signed upload URLs to be reachable
- Node action INPUT_ env vars with hyphens now preserved (direct exec, no shell)

**Work needed**:
- Test download in a real multi-job workflow (needs Gitea to dispatch the dependent job)
- Test with large binary artifacts
- Document that v3 artifacts must be used (v4 blocks GHES/Gitea)

### 2. Production hardening

**Impact**: Unknown failure modes in real workloads.

Zero production users means zero real-world bug reports. Need sustained use on a real project.

**Work needed**:
- Deploy on a real project (Rust CI with BuildKit for container builds)
- Run for 2-4 weeks, fix issues as they arise
- Monitor: job success rate, cache hit rate, pod scheduling latency

### 3. Documentation

**Impact**: No one can use it without docs.

**Work needed**:
- README with quick start, architecture diagram, configuration reference
- Helm chart values documentation
- BuildKit sidecar pattern guide
- ZFS snapshot cache setup guide (OpenEBS + loopback for dev, real ZFS for prod)

## Architecture Advantages (vs act_runner)

These are permanent differentiators, not gaps to close:

| Feature | drawbar | act_runner |
|---------|---------|------------|
| Security | Drop ALL caps, no Docker socket | Requires privileged DinD |
| Build cache | ZFS snapshot bind mounts (instant) | HTTP tar.gz (minutes) |
| K8s integration | Native Jobs, RBAC, health probes | Pod with DinD sidecar |
| Scaling | Ephemeral mode + KEDA | Process-level only |
| Checkout | Built-in, structured args (no injection) | External action (node.js) |
| Container builds | BuildKit sidecar (rootless) | DinD (privileged) |

## Won't Fix

- **Docker actions** — replaced by BuildKit sidecar pattern, which is more secure
- **Host execution mode** — we're K8s-native, that's the value prop
- **Per-step containers** — single container is simpler and sufficient
- **GITHUB_STEP_SUMMARY** — Gitea's runner proto (`StepState`, `UpdateTaskRequest`) has no summary field; server-side limitation
