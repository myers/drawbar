# Implementation Plan

## Completed Phases

### Phase 0: Skeleton + Protocol ✅
- Connect RPC client, registration, credential persistence (File + k8s Secret)
- Poll loop with idempotency keys, graceful shutdown

### Phase 1: Minimum Viable Execution ✅
- Workflow YAML parsing via `act/model.ReadWorkflow`
- k8s Job builder: one init container per `run:` step, shared workspace emptyDir
- Pod watcher: stream init container logs → Forgejo via UpdateLog/UpdateTask
- E2E: `run: echo hello` → green check

### Phase 2a: Test Hardening ✅
- Extracted `ReporterClient` + `PollerClient` interfaces for testability
- Injectable `WatchConfig` for poll intervals
- 40 → 67 tests (reporter, watcher, builder edge cases)

### Phase 2b: actions/checkout ✅
- Built-in git clone (no Node.js dependency)
- Token auth from task context, `git_clone_url` config override
- E2E: checkout + cat README.md → green check

### Phase 2c: Secrets — Skipped
- Forgejo resolves `${{ secrets.* }}` server-side before sending payload
- Log masking deferred

### Phase 2d: Expression Evaluation ✅
- Imported `act/exprparser` for full expression support
- `${{ github.* }}`, `${{ env.* }}`, `${{ secrets.* }}`, `${{ vars.* }}`
- Step `if:` conditions with `success()`, `failure()`, `always()`, `contains()`, etc.
- E2E: expressions resolve, conditional steps skip when false

### Phase 2e: Service Containers ✅
- k8s 1.28+ native sidecars (`restartPolicy: Always`)
- Docker-style port parsing, TCP readiness check
- E2E: postgres:16 + psql SELECT 1 → green check

### Phase 2f: Matrix Builds — Free (Server-Side) ✅
### Phase 2g: Job Dependencies — Free (Server-Side) ✅

### Phase 3: Helm Chart ✅
- Controller runs as k8s pod (no host binary, no port-forward)
- ServiceAccount + RBAC, ConfigMap, Secret, PVC
- E2E: helm install → workflow → green check (all inside k3d)

### Phase 4: Action Cache ✅
- `act/artifactcache` + `act/cacheproxy` for GitHub cache protocol
- BoltDB + filesystem on PVC, HMAC-signed per-job proxy, k8s Service
- `ACTIONS_CACHE_URL` + `ACTIONS_RUNTIME_TOKEN` injected into steps

### Phase 5: Generic Action Support ✅
- All action types: Node.js, Docker, composite, Go, shell
- Action repo caching on PVC (clone once, reuse via subPath bind mounts)
- E2E: actions/cache@v4 runs end-to-end (pre + main + post steps)

### Phase 6: CI/CD ✅
- Repo at fj.monoloco.net/chaos-inc/forgejo-k8s-runner
- Woodpecker pipeline: BuildKit build + push image + push Helm OCI
- First CI run: SUCCESS

---

### Phase 7: Entrypoint Shim ✅
- Single-container architecture with entrypoint binary
- $GITHUB_ENV, $GITHUB_PATH, $GITHUB_OUTPUT propagation between steps
- Step lifecycle tracked via /shim/state.jsonl (not spoofable via stdout)
- E2E: GITHUB_ENV propagation verified

### Phase 8: Failure Hardening ✅
- Log masking: secret values + tokens → `***`
- Crash recovery: orphaned jobs cleaned up on controller startup
- E2E failure testing: deferred (needs manual verification)

### Phase 9: Health, Probes, Docs ✅
- Health server: /healthz + /readyz on :8081
- Helm probes: liveness + readiness configured
- README: full rewrite with features, config, architecture, dev guide

### Phase 6: CI/CD ✅
- Repo at fj.monoloco.net/chaos-inc/forgejo-k8s-runner
- Woodpecker pipeline: BuildKit build + push image + Helm OCI push

### Deferred

- **Prometheus metrics** — jobs_total, active, duration, errors. Important for monitoring but not blocking functionality.
- **Multi-node support** — pod affinity for cache PVC sharing. Only needed when scaling beyond single-node k3s.
- **KEDA autoscaling** — ScaledObject for scaling runner pods. Forgejo already supports the KEDA API.
- **NetworkPolicy / ResourceQuota** — Helm chart security hardening.

### Not Needed (removed)

- ~~BuildKit integration~~ — This is just a custom action (`forgejo-actions/buildkit-build`), not a runner feature. Users write it themselves.

---

## Project Stats

| Metric | Value |
|---|---|
| Commits | 21 |
| Source lines | ~8000 |
| Tests | 64 |
| Packages | 9 (actions, config, expressions, forgejo, k8s, labels, reporter, version, workflow) |
| Docker | Zero DinD, zero privileged containers |
| CI/CD | Woodpecker on fj.monoloco.net, BuildKit image builds, Helm OCI publishing |

## Resolved Design Questions

1. **Workflow parser**: Import `act/model` directly (public, under `act/`)
2. **Node.js actions**: Use the same job image (most CI images have Node)
3. **Label mapping**: Reuse act_runner's `name:docker://image` format
4. **Cache storage**: PVC with BoltDB + filesystem (act/artifactcache)
5. **Action caching**: Clone once to PVC, mount via k8s subPath (zero copy)
6. **Relationship to act**: Import public packages, accept transitive deps. Propose upstream module extraction later.
7. **Default actions URL**: Configurable, falls back to task context `gitea_default_actions_url`
