# Gap Analysis: drawbar vs Gitea act_runner

Date: 2026-03-23

## Architecture Differences

| Aspect | drawbar | act_runner |
|--------|---------|------------|
| **Execution model** | K8s Jobs — one pod per job, no DinD | Docker containers via nektos/act |
| **Step isolation** | Single container, all steps sequential | Per-step containers possible |
| **Privileges** | Drops ALL capabilities, unprivileged | Requires Docker socket or DinD (privileged) |
| **Scaling** | K8s-native (HPA, node pools) | Process-level (run more instances) |
| **Workflow engine** | Custom (parser + entrypoint binary) | nektos/act (full GitHub Actions compat) |

## Where act_runner is better

### 1. Workflow compatibility — the biggest gap
- act_runner delegates to nektos/act which has years of GitHub Actions compat work
- Handles edge cases we don't: `::add-mask::`, `::group::`, `::warning::`, `::error::` workflow commands
- Our entrypoint doesn't process any workflow commands at all

### 2. Docker actions (`runs.using: docker`)
- act_runner runs Docker actions natively (it has a Docker daemon)
- We can't — single-container architecture means no container spawning
- This blocks popular actions like `docker/build-push-action`

### 3. Retry/resilience
- act_runner uses `avast/retry-go` with exponential backoff for reporter.Close()
- Our reporter retries 10x with linear 100ms backoff — less robust
- act_runner has rate limiting on polling (`golang.org/x/time/rate`)
- Our poller has no backoff on connection failures

### 4. Task versioning optimization
- act_runner tracks `tasksVersion` — server can skip DB query if nothing changed
- Our poller always does a full fetch (more server load)

### 5. Workflow commands
- act_runner processes `::add-mask::`, `::debug::`, `::group::`, `::stop-commands::`
- We process none — secrets are only masked from the initial task payload

### 6. Ephemeral mode
- act_runner supports `ephemeral: true` — pick up one job then exit
- Useful for K8s autoscaling patterns (scale-to-zero)
- We don't have this

### 7. Host execution mode
- act_runner can run directly on the host (no container)
- We're K8s-only — can't run bare-metal CI

### 8. Volume validation
- act_runner has glob-pattern validation for Docker volume mounts
- Security feature we don't need (no Docker) but worth noting

## Where drawbar is better

### 1. Security — significantly ahead
- We drop ALL capabilities, disallow privilege escalation
- act_runner requires privileged DinD or Docker socket mount — both major attack vectors
- Our `GIT_ASKPASS` approach never embeds tokens in clone URLs
- Structured args for checkout commands prevent shell injection
- No Docker socket = no container escape risk

### 2. Kubernetes-native design
- Native K8s Jobs with proper labeling, RBAC, resource limits
- Service containers as proper init-container sidecars (not Docker network hacks)
- VolumeSnapshot-based workspace caching (ZFS/LVM instant clones)
- PVC-backed action cache (shared across jobs)
- Health endpoints (`/healthz`, `/readyz`) for K8s probes

### 3. ZFS snapshot caching
- Instant workspace restoration from VolumeSnapshot
- act_runner has no equivalent — every job starts from scratch
- Can save minutes on large monorepos

### 4. Per-job cache auth (cacheproxy)
- HMAC-based per-job cache authentication
- act_runner's cache server has no per-job isolation

### 5. Built-in checkout
- No dependency on external `actions/checkout` action
- Faster (no node.js startup), more secure (structured args)
- Works offline — no need to fetch action from registry

### 6. Observability
- Structured JSON logging (slog)
- act_runner uses logrus (less structured)

## Gaps to close (priority order)

### Critical — blocks real-world adoption

1. **Workflow commands** (`::add-mask::`, `::group::`, `::error::`, `::warning::`)
   - Without `::add-mask::`, dynamically generated secrets leak in logs
   - Location: `cmd/entrypoint/main.go` — parse stdout lines for `::` prefix
   - Scope: entrypoint stdout/stderr processing, reporter mask list

2. **Exponential backoff on connection failures**
   - If Gitea goes down, we hammer it every 2s forever
   - Location: `pkg/server/poller.go` — add backoff after errors

3. **Task versioning**
   - Reduces polling load — important at scale
   - act_runner sends `tasksVersion` in FetchTask, server skips DB query if unchanged

### Important — improves reliability

4. **Ephemeral mode**
   - Pick up one job, exit. K8s Job recreates the runner pod.
   - Enables scale-to-zero with KEDA or similar

5. **Retry with exponential backoff in reporter**
   - Our 10x100ms linear retry is weak for network blips
   - Should use exponential backoff (100ms, 200ms, 400ms, 800ms...)

6. **`::add-mask::` at minimum**
   - Even without full workflow command support, mask handling is critical
   - Many actions dynamically generate tokens and use `::add-mask::`

### Nice to have

7. **Standalone cache server mode** (act_runner has `cache-server` subcommand)
8. **Config file validation** (JSON schema or similar)
9. **GitHub mirror support** (for actions hosted on github.com)
10. **Nested composite action expansion**

## Features we should NOT try to match

- **Docker execution mode** — this is our architectural differentiator. DinD-free is the whole point.
- **Host execution mode** — we're K8s-native, that's our value prop.
- **Per-step containers** — would require fundamental redesign for marginal benefit.
