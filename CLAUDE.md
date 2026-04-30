# drawbar

A Forgejo/Gitea Actions runner that executes workflow jobs as **native Kubernetes
Jobs** — one Job per workflow job, with services as real sidecars, secrets as
real Secrets, and no docker-in-docker anywhere. It is an alternative to
`gitea/act-runner` aimed at clusters that already speak Kubernetes.

Status: **early alpha**. APIs, on-disk layout, and Helm values are all subject
to change. There are no in-flight users to protect from breakage — develop
directly on `main`, no feature branches, no worktrees.

## Architecture

Three top-level pieces:

- **Controller** (`cmd/controller/`) — long-running pod. Registers as a runner
  with the forge, polls for tasks via the Forgejo runner gRPC (Connect) API,
  and per task: parses the workflow, resolves any `uses:` actions, builds a
  `batchv1.Job`, creates it, watches the pod, streams logs back to the forge,
  reports final status, and (if enabled) snapshots the workspace.
- **Entrypoint** (`cmd/entrypoint/`) — the in-pod runner. The controller
  injects this binary into each job pod via the `setup-shim` init container.
  It reads `/shim/manifest.json`, executes each step inside the runner
  container, evaluates runtime `if:` expressions, accumulates `$GITHUB_ENV` /
  `$GITHUB_PATH` / `$GITHUB_OUTPUT` between steps, and writes a
  `/shim/state.jsonl` lifecycle stream the controller parses out-of-band
  (so steps can't fake their state via stdout).
- **Cache server** (`pkg/cache/`) — HTTP server on `:9300` implementing the
  GitHub Actions artifact-cache protocol. Backed by SQLite (WAL) + filesystem
  on a PVC. Used by `actions/cache@v4`, `actions/upload-artifact`, etc.

End-to-end task flow:

```
poll  ->  parse workflow  ->  resolve actions (clone to PVC)  ->
build Job spec  ->  kubectl create  ->  watch pod / tail logs  ->
parse state.jsonl + workflow commands  ->  UpdateLog/UpdateTask  ->
on success: snapshot workspace (optional)  ->  TTL cleanup
```

## Package map

`cmd/`

- `cmd/controller/` — controller entrypoint, top-level handler/run loop. The
  shim binary is **not** here — it lives in `cmd/entrypoint/`.
- `cmd/entrypoint/` — the binary copied into each job pod by `setup-shim`.
  Reads the manifest and runs steps in-container.

`pkg/`

- `pkg/actions/` — clones `uses:` action repos into the action cache,
  parses `action.yml`, resolves Docker/Node/composite/shell action types.
- `pkg/cache/` — GitHub Actions artifact-cache HTTP server (SQLite + FS).
- `pkg/config/` — YAML config + env overrides + validation.
- `pkg/expressions/` — wraps `nektos/act/pkg/exprparser` for `${{ ... }}`.
  Used both controller-side (build-time) and shim-side (runtime `if:`).
- `pkg/k8s/` — `client.go` (in/out-of-cluster client), `builder.go` (the
  canonical `BuildJob` that turns a parsed workflow into a `batchv1.Job`),
  `watcher.go` (pod watch + log stream).
- `pkg/labels/` — parsing of `runs-on` label specs like
  `ubuntu-latest:docker://node:24-trixie`.
- `pkg/reporter/` — batches log rows + step state and pushes them to the
  forge via `UpdateLog` / `UpdateTask`. Also parses workflow commands
  (`::set-output::`, `::add-mask::`, etc.) and masks secrets in logs.
- `pkg/server/` — Forgejo gRPC client, registration, credential store
  (`FileStore` for local dev, `SecretStore` for in-cluster), poll loop.
- `pkg/snapshot/` — VolumeSnapshot / PVC-from-snapshot lifecycle for the
  ZFS-backed workspace cache. GC by retention.
- `pkg/types/` — shared step/manifest types crossing the
  controller/entrypoint boundary (serialized as JSON in `manifest.json`).
- `pkg/version/` — version + git commit, set via ldflags.
- `pkg/workflow/` — minimal wrapper around `act/model.ReadWorkflow` that
  enforces "exactly one job per task" (Forgejo pre-selects the job).

`actions/` — internal/bundled actions used by some flows
(`build-push/action.yml`, `cache/action.yml` + `main.sh`).

## The three caches (don't confuse them)

drawbar has three independent caches. Mixing them up is a common source of
confusion:

1. **Actions source cache** — git clones of action repos themselves, e.g.
   `actions/checkout@v4`. Lives at `cfg.Cache.Dir/actions-repo-cache/<dir>`.
   Populated by the controller (`pkg/actions`), read by job pods via a
   `ReadOnly` PVC mount with `subPath` per action. **Bug 001** documents
   that this currently breaks on RWO storage because the controller pod
   already has the PVC mounted RW when a job pod tries to mount it RO.

2. **Workspace snapshot cache** (`pkg/snapshot`) — per-job PVCs created
   from `VolumeSnapshot` objects, keyed by `(repo, cache-key)`. Designed
   for ZFS-backed CSI drivers where snapshot/clone is O(1). Bind-mounts
   declared paths (e.g. `target/`, `node_modules/`) into `/workspace`.
   Disabled by default; requires a snapshot-capable CSI driver.

3. **Artifact cache** (`pkg/cache`) — the GitHub Actions cache HTTP API
   on `:9300`, backed by SQLite + filesystem. Consumed by
   `actions/cache@v4`, `actions/upload-artifact`, etc. Independent of
   the other two.

## Build / test / run

From the `Makefile`:

- `make build` — builds both `bin/controller` and `bin/entrypoint`
  (CGO disabled, ldflags inject version).
- `make build-controller` / `make build-entrypoint` — individual binaries.
- `make test` — `go test ./...`.
- `make lint` — `golangci-lint run`.
- `make image` — `docker build` with version build args.
- `make push` / `make push-k3d` — push to `IMAGE` (default `localhost:5001/drawbar`).
- `make clean` — remove `bin/`.

For a local end-to-end loop there is `hack/dev-env.sh`:

- `./hack/dev-env.sh up` — k3d cluster + Gitea (or Forgejo via `SERVER=forgejo`)
  + drawbar runner.
- `./hack/dev-env.sh rebuild` — fast iteration (rebuild image + redeploy runner).
- `./hack/dev-env.sh down | status | logs | token`.

## Deployment

- `deploy/helm/drawbar/` — the canonical deployment artifact. `values.yaml`
  is the source of truth for what is configurable (image, server URL,
  registration token, runner labels/capacity, cache, snapshot, jobSecrets,
  RBAC, resources).
- `deploy/forgejo-test.yaml` and `deploy/gitea-test.yaml` — reference
  manifests for the forge side; the dev-env script uses them.

Config flow: `pkg/config.Load(path)` reads YAML, applies defaults, then
overlays env vars (`SERVER_URL`, `RUNNER_LABELS`, `CACHE_*`, `SNAPSHOT_*`,
etc. — see `applyEnvOverrides` for the full list). Helm renders the YAML
into a ConfigMap; secrets come in via env from a Secret.

## Bugs and active work

Before assuming something is broken or unconsidered, check:

- `bugs/` — numbered bug docs with reproduction + design. Currently:
  `001-actions-cache-pvc-rwo-prevents-job-pod-mount.md`,
  `002-better-runner-registration-recovery.md`,
  `003-per-repo-zfs-datasets-for-workspace-cache.md`.
- `BUGS.md` — older / lighter bug list.
- `PLAN.md` — phased implementation plan, with phases 0-9 marked complete.
- `NEXT_STEPS.md`, `FUTURE.md` — roadmap notes.
- `GAP_ANALYSIS.md` — what's missing vs. `act_runner` / GitHub Actions.
- `PRD.md` — product requirements / scope.
- `GITEA_FETCHTASK_BUG.md` — known upstream issue worth knowing about.

New design specs land under `docs/superpowers/specs/` when that directory
exists.

## Conventions worth knowing

- **Logging**: `log/slog` everywhere. Structured key/value, not formatted
  strings. The level comes from `cfg.Log.Level`.
- **Errors**: `fmt.Errorf("doing X: %w", err)` — wrap with a verb-phrase
  prefix, no trailing punctuation. Match this style when adding new errors.
- **Config**: yaml-tagged structs in `pkg/config`, with both
  `Default()` + `applyEnvOverrides()`. When adding a knob, update Default,
  Validate, applyEnvOverrides, the Helm `values.yaml`, and the relevant
  template — all four.
- **Building k8s objects**: go through `pkg/k8s/builder.go::BuildJob`. It
  is the single place that knows the pod shape (init containers in order:
  service sidecars -> wait-for-services -> setup-shim, then the runner
  container). All containers get a hardened SecurityContext (drop ALL caps,
  no privilege escalation; `RunAsNonRoot` is intentionally **not** set
  because common CI images run as root).
- **Controller/entrypoint boundary**: anything crossing it goes through
  `pkg/types` (`Manifest`, `ManifestStep`, `EvalContext`, `StateEvent`,
  `StepResult`) and is JSON. Don't pass Go structs across by other means.
- **Runner protocol**: types from `code.gitea.io/actions-proto-go` over
  `connectrpc.com/connect`. Tests use the `PollerClient` / `ReporterClient`
  interfaces in `pkg/server` and `pkg/reporter` — prefer those over the
  concrete client when adding tests.
- **Workflow parsing**: one job per task is enforced in
  `pkg/workflow.ParseTask`. The forge selects the job; matrix expansion
  happens server-side.
- **Tests**: most packages have `_test.go` next to the file. Integration
  tests that touch real git/k8s use a `_integration_test.go` suffix.
