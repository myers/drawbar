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

## Remaining Gaps — Production Blockers

### 1. Container builds (BuildKit sidecar)

**Impact**: Can't build Docker images in CI without this.

Docker actions (`docker/build-push-action`, `docker/login-action`) require a Docker daemon, which we don't have. The solution is not DinD but a **BuildKit sidecar** — drawbar already supports service containers as init-container sidecars.

A workflow would look like:
```yaml
services:
  buildkit:
    image: moby/buildkit:latest
    ports:
      - 1234

steps:
  - run: buildctl --addr tcp://localhost:1234 build ...
```

**Work needed**:
- Verify BuildKit works as a service container (TCP port, no privileged mode needed if using rootless BuildKit)
- Document the pattern
- Possibly create a `drawbar/build-push` action that wraps `buildctl`

### 2. Artifact upload/download verification

**Impact**: Multi-job workflows that pass data between jobs won't work if broken.

We set `ACTIONS_RUNTIME_URL` and `ACTIONS_RESULTS_URL` but have never tested `actions/upload-artifact` / `actions/download-artifact` end-to-end. These actions talk to the Gitea server's artifact API, not our cache server.

**Work needed**:
- E2E test: workflow with two jobs, first uploads artifact, second downloads it
- Verify the URLs resolve correctly through the k8s service mesh
- Test with both small (text) and large (binary) artifacts

### 3. GITHUB_STEP_SUMMARY

**Impact**: Workflows that generate markdown summaries (test reports, coverage) won't display them.

We create the `GITHUB_STEP_SUMMARY` file for each step but never send the content to the server. Gitea/Forgejo may support this via the UpdateTask API.

**Work needed**:
- Check if Gitea's UpdateTask proto has a field for step summaries
- If yes, read the summary file after each step and include it in the report
- If no, this is a server-side limitation (skip)

### 4. OIDC token support

**Impact**: Can't use `aws-actions/configure-aws-credentials`, `google-github-actions/auth`, or similar cloud auth actions.

These actions request an OIDC token from the runner via `ACTIONS_ID_TOKEN_REQUEST_URL` + `ACTIONS_ID_TOKEN_REQUEST_TOKEN`. We don't set these env vars.

**Work needed**:
- Check if Gitea provides OIDC token fields in the task context
- If yes, inject `ACTIONS_ID_TOKEN_REQUEST_URL` and `ACTIONS_ID_TOKEN_REQUEST_TOKEN` into the job environment
- If no, this is a server-side limitation

### 5. Production hardening

**Impact**: Unknown failure modes in real workloads.

Zero production users means zero real-world bug reports. Need sustained use on a real project.

**Work needed**:
- Deploy on a real project (Rust CI with BuildKit for container builds)
- Run for 2-4 weeks, fix issues as they arise
- Monitor: job success rate, cache hit rate, pod scheduling latency

### 6. Documentation

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
