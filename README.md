# drawbar

A Kubernetes-native runner for [Gitea Actions](https://docs.gitea.com/usage/actions/overview) and [Forgejo Actions](https://forgejo.org/docs/latest/user/actions/). Executes workflow jobs as unprivileged Kubernetes pods — no Docker-in-Docker required.

## Why

The existing runner (`act_runner`) requires Docker-in-Docker to run on Kubernetes. DinD means privileged containers, broken resource accounting, overlay-on-overlay filesystem overhead, and no k8s scheduling benefits.

drawbar replaces DinD with native Kubernetes primitives:

- Each **workflow job** becomes a Kubernetes **Job**
- Each **step** runs sequentially inside a **single container** via an entrypoint shim
- **Service containers** (postgres, redis, BuildKit) run as **native k8s sidecars**
- **Image builds** use a **BuildKit sidecar** — no Docker socket needed
- The **workspace** is a shared **emptyDir** volume
- **Zero privileged containers**, ever

## Features

- **Single container per job** — entrypoint shim orchestrates steps sequentially
- **`$GITHUB_ENV` / `$GITHUB_PATH` / `$GITHUB_OUTPUT`** — full inter-step state propagation
- **Service containers** — postgres, redis, etc. as k8s native sidecars (k8s 1.28+)
- **Expression evaluation** — `${{ github.* }}`, `${{ env.* }}`, `${{ secrets.* }}`, step `if:` conditions
- **Action support** — Node.js, composite, Go, and shell actions via direct exec
- **`actions/checkout@v4`** — works natively (loaded as a Node.js action, auth via `GITHUB_TOKEN`)
- **Action cache server** — GitHub Actions cache protocol with BoltDB + PVC storage
- **Action repo caching** — actions cloned once to PVC, mounted via k8s subPath (zero copy per build)
- **BuildKit sidecar** — auto-detected, rootless, unprivileged container builds
- **ZFS snapshot cache** — O(1) workspace restore via VolumeSnapshot (optional)
- **Artifact upload/download** — via `actions/upload-artifact@v3` / `actions/download-artifact@v3`
- **OIDC tokens** — cloud auth actions (`aws-actions`, `google-github-actions`) supported
- **Helm chart** — RBAC, ServiceAccount, PVC, health probes, cache Service
- **Log masking** — secret values replaced with `***` in streamed logs
- **Crash recovery** — orphaned jobs cleaned up on controller restart

## Limitations

- **Docker actions** (`runs.using: docker`) are not supported. Use Node.js, composite, Go, or shell actions instead. This is by design — the single-container architecture is incompatible with per-step image switching.
- **`actions/upload-artifact@v4`** and **`actions/download-artifact@v4`** do not work with Gitea/Forgejo — they throw `GHESNotSupportedError` for non-github.com servers. Use **v3** instead.
- **`GITHUB_STEP_SUMMARY`** — the file is created for each step but the content is not sent to the server (Gitea's runner protocol has no summary field).

## Quick Start

```bash
helm install runner oci://fj.monoloco.net/chaos-inc/drawbar \
  --namespace drawbar --create-namespace \
  --set server.url=https://your-gitea-instance.example.com \
  --set server.registrationToken=<token>
```

Get a registration token: **Site Administration > Runners > Create Registration Token**.

## Architecture

```
Controller Pod
+-- Poller: FetchTask from Gitea/Forgejo via Connect RPC
+-- Cache server: GitHub Actions cache protocol on PVC
+-- Health server: /healthz + /readyz on :8081
+-- Task handler:
    +-- Parse workflow YAML
    +-- Evaluate expressions
    +-- Resolve actions (clone to PVC cache)
    +-- Build step manifest JSON
    +-- Create k8s Job:
        initContainers:
          - svc-* (sidecars, restartPolicy: Always)
          - wait-for-services (TCP readiness probes)
          - setup-shim (copies entrypoint binary + manifest)
        containers:
          - runner (entrypoint executes all steps sequentially)
```

The entrypoint binary runs inside the job container, executing each step as a child process. Between steps it reads `$GITHUB_ENV` / `$GITHUB_PATH` / `$GITHUB_OUTPUT` files and propagates state. Step lifecycle events are written to `/shim/state.jsonl` (not parseable from stdout — prevents spoofing by user steps).

## BuildKit Sidecar

drawbar has no Docker daemon. For container image builds, use a [BuildKit](https://github.com/moby/buildkit) sidecar:

```yaml
services:
  buildkit:
    image: moby/buildkit:rootless
    ports:
      - 1234

steps:
  - uses: actions/checkout@v4
  - run: |
      buildctl --addr tcp://localhost:1234 build \
        --frontend dockerfile.v0 \
        --local context=. \
        --local dockerfile=. \
        --output type=image,name=registry.example.com/myapp:latest,push=true
```

**How it works**: drawbar auto-detects `moby/buildkit` images in service containers and configures them for rootless operation:

- Sets `seccompProfile: Unconfined` (required for user namespace creation)
- Adds `SETUID` + `SETGID` capabilities (required by `newuidmap`/`newgidmap`)
- Injects `--oci-worker-no-process-sandbox` (avoids needing `SYS_ADMIN`)
- Injects `--addr=tcp://0.0.0.0:{port}` for each declared port (BuildKit defaults to Unix socket only)

No workflow-level annotations needed — the auto-detection keeps your YAML clean.

**Note**: The job container image must include `buildctl`. Either use an image that has it pre-installed, or download it in a prior step.

### Registry Authentication

Pass credentials via workflow secrets and create a docker config inline:

```yaml
- run: |
    mkdir -p $HOME/.docker
    AUTH=$(printf '%s:%s' "${{ secrets.REGISTRY_USER }}" "${{ secrets.REGISTRY_PASS }}" | base64)
    printf '{"auths":{"%s":{"auth":"%s"}}}\n' "registry.example.com" "$AUTH" > $HOME/.docker/config.json
    buildctl --addr tcp://localhost:1234 build ...
```

## ZFS Snapshot Cache

Optional feature that restores workspace dependencies (e.g., `target/`, `node_modules/`) from a ZFS VolumeSnapshot in O(1) time — compared to minutes for tar.gz over HTTP.

### How It Works

1. Workflow declares cache paths via the `drawbar/cache` action
2. Controller looks up an existing VolumeSnapshot matching the cache key
3. If found: creates a PVC from the snapshot (ZFS clone — instant, copy-on-write)
4. Bind-mounts the cached paths into `/workspace` before the job starts
5. On job success: snapshots the workspace PVC for future reuse
6. Snapshots older than `retentionDays` are garbage-collected hourly

### Prerequisites

- A CSI driver with snapshot support. Recommended: [OpenEBS ZFS LocalPV](https://github.com/openebs/zfs-localpv)
- A `VolumeSnapshotClass` and `StorageClass` configured for the driver

### Enabling

```yaml
# Helm values
snapshot:
  enabled: true
  class: openebs-zfs-snapshot      # VolumeSnapshotClass name
  storageClass: openebs-zfs        # StorageClass for PVCs
  size: 10Gi                       # Workspace PVC size
  retentionDays: 7                 # GC snapshots older than this
```

### Workflow Usage

```yaml
steps:
  - uses: drawbar/cache@v1
    with:
      key: rust-${{ hashFiles('Cargo.lock') }}
      path: target
      restore-keys: |
        rust-
  - run: cargo build
```

The `drawbar/cache` action is a no-op at runtime — the controller reads `key`, `path`, and `restore-keys` from the step environment at job-build time and sets up bind mounts before the pod starts.

## Artifacts

Use `actions/upload-artifact@v3` and `actions/download-artifact@v3` for passing data between jobs:

```yaml
jobs:
  build:
    steps:
      - run: echo "result" > output.txt
      - uses: actions/upload-artifact@v3
        with:
          name: my-output
          path: output.txt

  test:
    needs: build
    steps:
      - uses: actions/download-artifact@v3
        with:
          name: my-output
```

**Important**: `@v4` does NOT work with Gitea/Forgejo — it throws `GHESNotSupportedError`. Use `@v3`.

**Gitea configuration**: The server's `ROOT_URL` must be set to a URL reachable from job pods (e.g., `http://gitea.gitea.svc:80/`). Signed artifact upload URLs are derived from `ROOT_URL`.

## Configuration

### Helm Values

| Value | Description | Default |
|---|---|---|
| `image.repository` | Controller image | `fj.monoloco.net/chaos-inc/drawbar` |
| `image.tag` | Image tag | `latest` |
| `image.pullPolicy` | Image pull policy | `IfNotPresent` |
| `server.url` | Gitea/Forgejo instance URL | (required) |
| `server.registrationToken` | Runner registration token | (required for first install) |
| `server.insecure` | Skip TLS certificate verification | `false` |
| `runner.name` | Runner name | `k8s-runner` |
| `runner.labels` | Runner labels (image mapping) | `["ubuntu-latest:docker://node:24-trixie"]` |
| `runner.capacity` | Max concurrent jobs | `1` |
| `runner.timeout` | Job timeout | `30m` |
| `runner.fetchInterval` | Task poll interval | `2s` |
| `runner.fetchTimeout` | FetchTask RPC timeout | `30s` |
| `runner.gitCloneUrl` | Override URL for git clone | (uses `server.url`) |
| `runner.jobNamespace` | Namespace for job pods | (release namespace) |
| `runner.jobSecrets` | K8s Secrets to mount into job pods | `[]` |
| `cache.enabled` | Enable action cache server | `true` |
| `cache.storage` | PVC size for cache | `5Gi` |
| `cache.port` | Cache proxy port | `9300` |
| `snapshot.enabled` | Enable ZFS snapshot cache | `false` |
| `snapshot.class` | VolumeSnapshotClass name | `""` |
| `snapshot.storageClass` | StorageClass for snapshot PVCs | `""` |
| `snapshot.size` | Workspace PVC size | `10Gi` |
| `snapshot.retentionDays` | GC snapshots older than this | `7` |
| `log.level` | Log level (debug, info, warn, error) | `info` |
| `resources` | Controller pod resource requests/limits | 100m CPU, 128Mi-256Mi memory |
| `serviceAccount.create` | Create a ServiceAccount | `true` |
| `rbac.create` | Create RBAC roles | `true` |

#### Job Secrets

Mount Kubernetes Secrets into job pods (e.g., registry credentials, TLS certs):

```yaml
runner:
  jobSecrets:
    - name: registry-creds          # inject as env vars (no mountPath)
    - name: buildkit-client-certs   # mount as files
      mountPath: /certs
```

### Environment Variable Overrides

All config values can be overridden with environment variables:

| Env Var | Config Field |
|---|---|
| `SERVER_URL` | `server.url` |
| `RUNNER_NAME` | `runner.name` |
| `RUNNER_LABELS` | `runner.labels` (comma-separated) |
| `RUNNER_CAPACITY` | `runner.capacity` |
| `RUNNER_EPHEMERAL` | `runner.ephemeral` |
| `RUNNER_GIT_CLONE_URL` | `runner.git_clone_url` |
| `RUNNER_ACTIONS_URL` | `runner.actions_url` |
| `CONTROLLER_IMAGE` | `runner.controller_image` |
| `CACHE_ENABLED` | `cache.enabled` |
| `CACHE_DIR` | `cache.dir` |
| `CACHE_PORT` | `cache.port` |
| `CACHE_SERVICE_NAME` | `cache.service_name` |
| `CACHE_PVC_NAME` | `cache.pvc_name` |
| `SNAPSHOT_ENABLED` | `snapshot.enabled` |
| `SNAPSHOT_CLASS` | `snapshot.class` |
| `SNAPSHOT_STORAGE_CLASS` | `snapshot.storage_class` |
| `SNAPSHOT_SIZE` | `snapshot.size` |
| `SNAPSHOT_RETENTION_DAYS` | `snapshot.retention_days` |
| `LOG_LEVEL` | `log.level` |

## Autoscaling with KEDA

drawbar exposes a `/metrics/active-jobs` JSON endpoint on port 8081 that reports active job count and capacity:

```json
{"active": 1, "capacity": 1}
```

[KEDA](https://keda.sh) can use this to scale the drawbar deployment based on load.

### Setup

1. Install KEDA:

```bash
kubectl apply --server-side -f https://github.com/kedacore/keda/releases/download/v2.19.0/keda-2.19.0.yaml
```

2. Create a Service for the metrics endpoint:

```yaml
apiVersion: v1
kind: Service
metadata:
  name: runner-metrics
  namespace: drawbar
spec:
  selector:
    app: drawbar                    # match your deployment pod labels
  ports:
  - port: 8081
    targetPort: 8081
```

3. Create a ScaledObject:

```yaml
apiVersion: keda.sh/v1alpha1
kind: ScaledObject
metadata:
  name: drawbar-scaler
  namespace: drawbar
spec:
  scaleTargetRef:
    name: runner-runner             # your drawbar Deployment name
  minReplicaCount: 1
  maxReplicaCount: 5
  pollingInterval: 5                # check every 5 seconds
  cooldownPeriod: 30                # scale down 30s after jobs finish
  triggers:
  - type: metrics-api
    metadata:
      targetValue: "1"
      url: "http://runner-metrics.drawbar.svc:8081/metrics/active-jobs"
      valueLocation: "active"
```

When active jobs reach the threshold, KEDA scales up additional replicas. When jobs finish, it scales back down after the cooldown period. Multiple replicas can share the same cache PVC (SQLite WAL mode allows concurrent access).

**Note**: If using Forgejo, you can alternatively use KEDA's native `forgejo-runner` scaler which polls the Forgejo API directly for pending jobs. This provides better scaling signals (pending queue depth vs active count) but requires Forgejo-specific API endpoints that Gitea doesn't have.

## Known Issues

See [BUGS.md](BUGS.md) for known bugs and [GITEA_FETCHTASK_BUG.md](GITEA_FETCHTASK_BUG.md) for a Gitea server-side issue affecting task reliability under concurrent load (fixed in Forgejo, not yet in Gitea).

## Development

The project includes a fully automated dev environment:

```bash
# Full setup: k3d cluster + Gitea + runner
./hack/dev-env.sh up

# Fast iteration: rebuild + redeploy runner only
./hack/dev-env.sh rebuild

# Other commands
./hack/dev-env.sh status   # show pod status
./hack/dev-env.sh logs     # tail runner logs
./hack/dev-env.sh token    # print registration token
./hack/dev-env.sh down     # tear down cluster

# Use Forgejo instead of Gitea
SERVER=forgejo ./hack/dev-env.sh up
```

### Building

```bash
make build    # controller + entrypoint binaries
make test     # run all tests
make image    # build Docker image
make push     # push to registry
```

## License

MIT
