# forgejo-k8s-runner — Product Requirements Document

## Problem

Forgejo Actions has no production-grade Kubernetes-native runner. The current `act_runner` requires Docker-in-Docker (DinD) sidecars to run on Kubernetes, which means:

- **Privileged containers** — DinD needs `--privileged`, effectively disabling container isolation. A container escape compromises the node.
- **Broken resource accounting** — the inner Docker daemon ignores k8s resource limits, causing unpredictable OOMs.
- **No k8s scheduling** — all jobs run inside one pod instead of being distributed across the cluster.
- **Double filesystem overhead** — overlay-on-overlay degrades I/O performance.
- **No image cache persistence** — ephemeral DinD starts cold every time.
- **Operational misery** — nested daemons, nested logs, nested networking.

Gitea solved this with a proprietary enterprise-only Actions Runner Controller. The open-source Forgejo ecosystem has nothing equivalent. The only existing attempt is infinoid's homelab PoC (`code.forgejo.org/infinoid/k8s-runner`), which validates the approach but lacks services, caching, artifacts, secrets, and most workflow features.

## Solution

A standalone Kubernetes-native controller that:

1. **Registers with Forgejo** as a standard runner using the existing Connect RPC protocol (actions-proto v0.6.0)
2. **Polls for jobs** via `FetchTask`
3. **Creates a Kubernetes Job per workflow job** — each step is an init container, services are sidecars, workspace is a shared emptyDir
4. **Streams logs** back to Forgejo via `UpdateLog`
5. **Reports status** via `UpdateTask`
6. **Requires zero changes to Forgejo server** — from the server's perspective, this is just another runner

### Non-Goals (v1)

- Replacing Woodpecker CI or other CI systems
- Full GitHub Actions feature parity (composite actions, reusable workflows — these can come later)
- Running on non-Kubernetes platforms
- Managing the Forgejo server itself

## Architecture

```
Forgejo Server
     │
     │  Connect RPC (HTTP/1.1 + protobuf)
     │  Poll: FetchTask / UpdateTask / UpdateLog
     ▼
┌──────────────────────────────────────────────┐
│  forgejo-k8s-runner controller               │
│  (single unprivileged pod)                   │
│                                              │
│  • Registers as runner on startup            │
│  • Polls for pending jobs                    │
│  • Parses workflow YAML → k8s Job spec       │
│  • Creates Jobs via client-go                │
│  • Watches pod logs, streams to Forgejo      │
│  • Handles cancellation (deletes Job)        │
│  • Manages artifact/cache storage            │
└────────────────┬─────────────────────────────┘
                 │  client-go (k8s API)
                 ▼
┌──────────────────────────────────────────────┐
│  Kubernetes Job: "run-42-build"              │
│  ┌─────────────────────────────────────────┐ │
│  │ Pod                                     │ │
│  │                                         │ │
│  │  init-0: workspace-setup (clone, cache) │ │
│  │  init-1: step "checkout"                │ │
│  │  init-2: step "setup-node"              │ │
│  │  init-3: step "npm test"                │ │
│  │                                         │ │
│  │  sidecars:                              │ │
│  │    postgres (from services: block)      │ │
│  │    redis    (from services: block)      │ │
│  │                                         │ │
│  │  volumes:                               │ │
│  │    workspace: emptyDir                  │ │
│  │    outputs:   emptyDir                  │ │
│  │    tool-cache: PVC (optional)           │ │
│  └─────────────────────────────────────────┘ │
└──────────────────────────────────────────────┘
```

### Why Init Containers (Not Separate Pods)

- Steps share filesystem naturally — no copying between pods
- Steps share network namespace — `localhost` services just work
- Kubernetes sequences init containers automatically — no orchestration code
- One pod = one schedulable unit with aggregate resource requests
- This is the same design GitHub's ARC and GitLab's k8s executor use

### Step Execution Model

Each workflow step becomes an init container:

1. Controller resolves the step's container image (from `uses:` action metadata or `container:` block)
2. Creates init container spec with:
   - Image from the action or workflow
   - Command/entrypoint overridden to run through the **step entrypoint shim**
   - Workspace volume mounted at `/workspace`
   - Outputs volume mounted at `/outputs`
   - Environment variables from workflow + secrets
3. The entrypoint shim handles:
   - `$GITHUB_ENV`, `$GITHUB_OUTPUT`, `$GITHUB_PATH` file-based communication
   - `::set-output::`, `::add-mask::`, `::error::` workflow commands (parsed from stdout)
   - Exit code capture and reporting
   - Step-level `if:` conditional evaluation

### Image Building (No Docker Daemon)

For workflows that build container images, the controller integrates with **BuildKit**:

- Configurable BuildKit daemon endpoint(s)
- First-party `forgejo-actions/buildkit-build` action wraps `buildctl`
- Shares cache with existing BuildKit infrastructure (e.g., Woodpecker CI builds)
- No DinD, no privileged containers

### Security Model

| Component | Security Posture |
|---|---|
| Controller pod | Unprivileged. ServiceAccount with scoped RBAC (create/delete Jobs, Pods, Secrets in runner namespace only). Network policy: egress to Forgejo + k8s API only. |
| Job pods | Run as non-root (configurable). No privilege escalation. ResourceQuota + LimitRange enforced. Ephemeral — deleted on completion. |
| Secrets | Mounted as tmpfs volumes, not env vars. Scoped per job. Cleaned up with the pod. |
| Network | Configurable NetworkPolicy per job. Default: allow egress for package downloads, deny ingress. |
| RBAC | Controller cannot exec into job pods, only create/delete/watch. |

### Runner Protocol

Uses the existing Forgejo runner protocol with zero server-side changes:

| RPC | Purpose |
|---|---|
| `Ping` | Health check on startup |
| `Register` | One-time registration with registration token |
| `Declare` | Update labels/version on each startup |
| `FetchTask` | Poll for pending jobs (with idempotency key) |
| `UpdateTask` | Report job/step state and outputs |
| `UpdateLog` | Batch upload log rows |

Authentication: `x-runner-uuid` + `x-runner-token` headers on every request (except Register). Connect RPC over HTTP/1.1 with protobuf serialization.

## Target Users

- **Self-hosters** running Forgejo on Kubernetes who want Actions without DinD
- **Organizations** with security policies that prohibit privileged containers
- **Homelab operators** on k3s/k0s who want efficient resource usage
- **Teams migrating from GitHub Actions** who need workflow compatibility on k8s

## Success Criteria

### Phase 1 — Minimum Viable Runner
- [ ] Register with Forgejo, poll for jobs, execute simple workflows (checkout + run shell commands)
- [ ] Steps execute as init containers in a k8s Job
- [ ] Logs stream back to Forgejo UI in real time
- [ ] Job status reported correctly (success/failure/cancelled)
- [ ] Zero privileged containers

### Phase 2 — Real Workloads
- [ ] Service containers (postgres, redis, etc.) as pod sidecars
- [ ] Secrets injection
- [ ] `actions/cache` support (S3 or PVC backend)
- [ ] `actions/upload-artifact` / `download-artifact` support
- [ ] Expression evaluation (`${{ }}` syntax)
- [ ] Step conditionals (`if:`)
- [ ] Matrix builds
- [ ] Job `needs:` dependencies

### Phase 3 — Production Grade
- [ ] BuildKit integration for image builds
- [ ] KEDA autoscaling (controller scales to zero when idle)
- [ ] Helm chart with RBAC, NetworkPolicy, ResourceQuota
- [ ] Multi-instance Forgejo support
- [ ] Comprehensive error handling and retry logic
- [ ] Metrics (Prometheus) and structured logging
- [ ] Documentation and getting-started guide

### Phase 4 — Feature Complete
- [ ] Composite actions
- [ ] Reusable workflows
- [ ] Container actions (custom Dockerfile in action repo)
- [ ] OIDC token support for cloud provider auth
- [ ] Job concurrency controls
- [ ] Workflow `timeout-minutes`

## Technology

- **Language**: Go (same as Forgejo, act_runner, and the k8s ecosystem)
- **k8s client**: `client-go`
- **Protocol**: `code.forgejo.org/forgejo/actions-proto` v0.6.0 + `connectrpc.com/connect`
- **Workflow parsing**: Fork/extract from `nektos/act/pkg/model` or reimplement
- **Build**: Standard Go module, multi-arch container images
- **Deploy**: Helm chart, with raw manifests as alternative

## Prior Art

| Project | Relationship |
|---|---|
| `code.forgejo.org/infinoid/k8s-runner` | PoC that validates the core approach. Reference for protocol integration. |
| `code.forgejo.org/forgejo/runner` (act_runner) | Source for protocol client code, reporter, config handling. |
| `code.forgejo.org/forgejo/actions-proto` | Protobuf definitions for the runner protocol. |
| GitHub ARC + runner-container-hooks | Reference architecture for init-container-per-step design. |
| GitLab Kubernetes Executor | Reference for k8s job pod lifecycle management. |
| Woodpecker CI k8s backend | Reference for k8s-native CI execution (pod-per-step variant). |
