# forgejo-k8s-runner

A Kubernetes-native runner for [Forgejo Actions](https://forgejo.org/docs/latest/user/actions/). Executes workflow jobs as unprivileged Kubernetes pods — no Docker-in-Docker required.

## Why

The existing Forgejo runner (`act_runner`) requires Docker-in-Docker to run on Kubernetes. DinD means privileged containers, broken resource accounting, overlay-on-overlay filesystem overhead, and no k8s scheduling benefits.

This runner replaces DinD with native Kubernetes primitives:

- Each **workflow job** becomes a Kubernetes **Job**
- Each **step** runs sequentially inside a **single container** via an entrypoint shim
- **Service containers** (postgres, redis) run as **native k8s sidecars**
- **Image builds** pair with an existing **BuildKit** daemon — no Docker socket needed
- The **workspace** is a shared **emptyDir** volume
- **Zero privileged containers**, ever

## Features

- **Single container per job** — entrypoint shim orchestrates steps sequentially
- **`$GITHUB_ENV` / `$GITHUB_PATH` / `$GITHUB_OUTPUT`** — full inter-step state propagation
- **Service containers** — postgres, redis, etc. as k8s native sidecars (1.28+)
- **Expression evaluation** — `${{ github.* }}`, `${{ env.* }}`, `${{ secrets.* }}`, step `if:` conditions
- **Action support** — Node.js, composite, Go, and shell actions
- **Built-in checkout** — `actions/checkout` as native git clone
- **Action cache server** — GitHub Actions cache protocol (`_apis/artifactcache`) with BoltDB + PVC storage
- **Action repo caching** — actions cloned once to PVC, mounted via k8s subPath (zero copy per build). Note: the cache is **not cleaned up automatically** — see [FUTURE.md](FUTURE.md) for planned eviction. Operators should monitor PVC usage and clean up `actions-repo-cache/` if needed.
- **Image builds** — pair with a BuildKit daemon for `docker build` without Docker (use `buildctl` in `run:` steps or a BuildKit action)
- **Helm chart** — RBAC, ServiceAccount, PVC, health probes, cache Service
- **Log masking** — secret values replaced with `***` in streamed logs
- **Crash recovery** — orphaned jobs cleaned up on controller restart

## Limitations

- **Docker actions** (`runs.using: docker`) are not supported. Use Node.js, composite, Go, or shell actions instead. This is a deliberate design decision — the single-container architecture is incompatible with per-step image switching.

## Quick Start

```bash
helm install runner oci://fj.monoloco.net/chaos-inc/forgejo-k8s-runner \
  --namespace forgejo-runner --create-namespace \
  --set forgejo.url=https://your-forgejo-instance.example.com \
  --set forgejo.registrationToken=<token> \
  --set runner.gitCloneUrl=https://your-forgejo-instance.example.com
```

Get a registration token from Forgejo: **Site Administration → Runners → Create Registration Token**.

## Configuration

Configuration is via Helm values (recommended) or a YAML config file with environment variable overrides.

### Helm Values

| Value | Description | Default |
|---|---|---|
| `image.repository` | Controller image | `fj.monoloco.net/chaos-inc/forgejo-k8s-runner` |
| `image.tag` | Image tag | `latest` |
| `forgejo.url` | Forgejo instance URL | (required) |
| `forgejo.registrationToken` | Runner registration token | (required for first install) |
| `runner.name` | Runner name in Forgejo | `k8s-runner` |
| `runner.labels` | Runner labels (image mapping) | `["ubuntu-latest:docker://node:24-trixie"]` |
| `runner.capacity` | Max concurrent jobs | `1` |
| `runner.timeout` | Job timeout | `30m` |
| `runner.gitCloneUrl` | Override URL for git clone | (uses `forgejo.url`) |
| `runner.actionsUrl` | Override URL for cloning actions | (uses task context `default_actions_url`) |
| `cache.enabled` | Enable action cache server | `true` |
| `cache.storage` | PVC size for cache | `5Gi` |
| `cache.port` | Cache proxy port | `9300` |

### YAML Config File

The controller reads `config.yaml` (path configurable via `--config` flag). All values can be overridden with environment variables:

| Env Var | Config Field |
|---|---|
| `FORGEJO_URL` | `forgejo.url` |
| `FORGEJO_REGISTRATION_TOKEN` | `forgejo.registration_token` |
| `RUNNER_NAME` | `runner.name` |
| `RUNNER_LABELS` | `runner.labels` (comma-separated) |
| `RUNNER_CAPACITY` | `runner.capacity` |
| `RUNNER_GIT_CLONE_URL` | `runner.git_clone_url` |
| `RUNNER_ACTIONS_URL` | `runner.actions_url` |
| `CONTROLLER_IMAGE` | `runner.controller_image` |
| `CACHE_ENABLED` | `cache.enabled` |
| `CACHE_DIR` | `cache.dir` |
| `CACHE_PORT` | `cache.port` |
| `LOG_LEVEL` | `log.level` |

## Architecture

```
Controller Pod
├── Poller — FetchTask from Forgejo via Connect RPC
├── Cache server — artifactcache + cacheproxy on PVC
├── Health server — /healthz + /readyz on :8081
└── Task handler:
    ├── Parse workflow YAML (act/model)
    ├── Evaluate expressions (act/exprparser)
    ├── Resolve actions (clone to PVC cache)
    ├── Build step manifest JSON
    └── Create k8s Job:
        initContainers:
          - svc-* (sidecars, restartPolicy: Always)
          - wait-for-services (TCP readiness check)
          - setup-shim (copies entrypoint binary + manifest)
        containers:
          - runner (entrypoint executes all steps sequentially)
```

The entrypoint binary runs inside the job container, executing each step as a child process. Between steps it reads `$GITHUB_ENV`/`$GITHUB_PATH`/`$GITHUB_OUTPUT` files and propagates state to subsequent steps. Step lifecycle events are written to `/shim/state.jsonl` (not parseable from stdout — prevents spoofing by user steps).

## Building Images Without Docker

This runner has no Docker daemon. For workflows that build container images, pair it with a [BuildKit](https://github.com/moby/buildkit) daemon running in your cluster:

```yaml
steps:
  - uses: actions/checkout@v4
  - name: Build and push
    run: |
      buildctl \
        --addr tcp://buildkitd.your-namespace.svc:1234 \
        build \
        --frontend dockerfile.v0 \
        --local context=. \
        --local dockerfile=. \
        --output type=image,name=registry.example.com/myapp:latest,push=true
```

BuildKit runs unprivileged and supports layer caching, multi-stage builds, and cross-compilation — everything Docker can do, without Docker.

## Development

```bash
# Build
make build          # controller + entrypoint binaries
make test           # run all tests
make image          # build Docker image
make push           # push to registry

# Local testing with k3d
k3d cluster create myrunner --registry-use k3d-myregistry:5001
make image VERSION=dev
docker push localhost:5001/forgejo-k8s-runner:dev
helm install runner ./deploy/helm/forgejo-k8s-runner \
  --namespace forgejo-runner --create-namespace \
  --set image.repository=k3d-myregistry:5001/forgejo-k8s-runner \
  --set image.tag=dev \
  --set forgejo.url=http://forgejo.forgejo.svc:80 \
  --set forgejo.registrationToken=<token>
```

## License

MIT
