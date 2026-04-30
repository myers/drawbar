# Actions cache: HTTP fetch into emptyDir (bug 001 fix)

**Date:** 2026-04-30
**Status:** Design — pending implementation plan
**Bug:** [`bugs/001-actions-cache-pvc-rwo-prevents-job-pod-mount.md`](../../../bugs/001-actions-cache-pvc-rwo-prevents-job-pod-mount.md)

## Summary

Stop sharing the controller's `drawbar-cache` PVC with job pods. The PVC is `ReadWriteOnce`; on storage backends that enforce RWO strictly (ZFS-LocalPV, most "local" CSI drivers), the controller and a job pod cannot both mount it, leaving job pods stuck in `Init:0/1` with `FailedMount` events.

Replace the PVC mount in job pods with an `emptyDir` populated at init time by an HTTP fetch from the controller's cache server. The controller continues to own the PVC and serves action source contents over a new HTTP route. Job pods become independent of the PVC entirely; bug 001 dissolves structurally.

The workspace snapshot cache (per-job PVCs cloned from `(repo, cache-key)`-labeled `VolumeSnapshot`s) and the GitHub Actions artifact cache (HTTP API on :9300) are unchanged — only the actions *source* cache changes.

## Background

There are three caches in drawbar today, easily conflated:

1. **Actions source cache** — git clones of action repos at `cfg.Cache.Dir/actions-repo-cache/<dir>`. Controller writes (one-time clone per action). Every job needs read access.
2. **Workspace snapshot cache** — per-job ZFS-cloned PVC of `target/`, `node_modules`, etc., keyed by `(repo, cache-key)`. Each job has its own PVC; RWO is correct.
3. **Artifact cache** — `actions/cache@v4` and `actions/upload-artifact` HTTP endpoints on :9300. Backed by SQLite + filesystem inside the controller's PVC. Jobs talk to it over HTTP; no shared volume.

Only #1 has the bug. #2 is correct as designed. #3 is correct as designed.

The current implementation tries to share #1 via a single RWO PVC mounted by both the controller (RW) and every job pod (RO). ZFS-LocalPV's CSI driver rejects the second mount in `verifyMount`; `ReadOnly: true` at the kubelet layer doesn't change this because it's enforced at the CSI driver layer, before kubelet honors the read-only flag.

## Design

### Architecture

- Controller's existing PVC stays as-is. Only the controller mounts it.
- Cache HTTP server on :9300 (already running, today serving the artifact cache protocol) gains a new route family for action sources.
- Job pods get a new `emptyDir` volume mounted at `/actions`.
- The `setup-shim` init container, which today copies the entrypoint binary and writes a manifest via shell heredoc, now also fetches required action tarballs from the controller into `/actions/<dir>/`.
- From the runner container's perspective the `/actions/<dir>/` paths and contents are unchanged; the volume source switches from a PVC subPath to an emptyDir.

### Cache server: new route

Single new route in `pkg/cache/handler.go`:

```
GET /_apis/actions/:dir/tar
```

Handler reads `cfg.Cache.Dir/actions-repo-cache/<dir>`, streams a tar of its contents excluding the top-level `.git/` directory.

Validation: `dir` must match `[a-zA-Z0-9-]+` (the same charset `pkg/actions/resolve.go` `ActionRef.ActionDir()` produces). Defense-in-depth against URL-path traversal even though httprouter handles it at the routing layer.

Implementation:
- New helper `tarDir(w io.Writer, root string, excludes []string) error` in `pkg/cache/tar.go`.
- Handler is one short function added to `Handler.serveAction`.
- No tarball caching — rebuild every request. Action contents are tiny (typically <1 MB unpacked); tar generation is dominated by I/O and ZFS ARC keeps source files hot. Revisit only if profiling shows a hotspot.

The route is reachable at the same URL job pods already use for the artifact cache (the existing `CACHE_SERVICE_NAME` Service). No new Service, no new env var.

### Manifest: new field

`pkg/types/Manifest` (the JSON blob the controller injects via the setup-shim heredoc and the entrypoint reads) gains:

```go
type Manifest struct {
    Steps   []ManifestStep
    BaseEnv map[string]string
    Context *EvalContext
    Actions []ActionFetch  // NEW
}

type ActionFetch struct {
    Dir string  // e.g. "actions-checkout-v4" — the existing ActionDir() value
    URL string  // full URL: "http://drawbar-cache:9300/_apis/actions/actions-checkout-v4/tar"
}
```

The controller already computes the equivalent of `Actions[]` while building the job (`actionsToClone` in `cmd/controller/main.go:425`). Populating the new field is a small additional step at the same site.

### Init container: setup subcommand

Add a `setup` subcommand to `cmd/entrypoint/main.go`. The init container shell command becomes:

```sh
cp /entrypoint /shim/entrypoint && chmod +x /shim/entrypoint && \
cat > /shim/manifest.json << 'DELIM'
<manifest JSON>
DELIM
exec /shim/entrypoint setup /shim/manifest.json
```

The `setup` subcommand:

1. Reads the manifest from the path argv.
2. Writes `/shim/askpass.sh` (the askpass shim that today is built by shell `printf`). Move into Go.
3. For each `Actions[]` entry: HTTP GET the URL with a 30s timeout; on 5xx or network error, retry 3 times with 1s/2s/4s backoff; on 404, fail immediately. Stream the response body through `archive/tar.Reader` directly into `/actions/<Dir>/`. Reject any tar entry whose cleaned path escapes the target dir.
4. Exit 0 on success; non-zero with a clear error message on failure (kubelet's init-container retry handles the rest).

Sequential fetch. Actions are typically 2-5 per job and small; concurrency is not warranted in v1.

The current main entrypoint behavior (running the steps) becomes the `run` subcommand. The old single-arg form is removed — drawbar is pre-production, no in-flight jobs to keep working.

### Volumes and mounts (`pkg/k8s/builder.go`)

**Pod-level:**
- Drop the `actions-cache` PVC volume (current `:113-122`).
- Add: `{Name: "actions", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}}`.

**Setup-shim init container** gains a mount at `/actions` (write target).

**Runner container:**
- Drop the `actions-cache` PVC subPath mounts (current `:217-228`).
- Add: `{Name: "actions", MountPath: "/actions", ReadOnly: true}`.

The runner sees the same paths and files at `/actions/<dir>/` — only the volume source changes.

### Config and helm

- **`pkg/config/config.go`:** delete `CacheConfig.PVCName` field and the `CACHE_PVC_NAME` env override. The field is no longer used.
- **`pkg/k8s/builder.go`:** delete `JobConfig.CachePVCName` and the `jobCachePVCName` plumbing in `cmd/controller/main.go:595-599`.
- **Helm chart `values.yaml`:** no schema change. The `cache.*` block already correctly describes the controller's persistent cache.
- **Helm chart `deployment.yaml`:** drop `CACHE_PVC_NAME` env var. Keep `CACHE_SERVICE_NAME` (jobs still need it for the HTTP fetch URL).
- **Helm chart `pvc.yaml`:** unchanged — single `<release>-cache` PVC, RWO, mounted only by the controller.

No deprecation handling. Drawbar is pre-production (the bug was caught during the shakedown eval); no real deployment relies on `cache.pvc_name` or `CACHE_PVC_NAME`. Delete the field and the env override outright.

## Failure modes

**Cache server unreachable from job pod.** Setup-shim retries 3× (1s/2s/4s), then fails with a network error. Job stays `Init:0/1` until init backoff gives up; error is visible in `kubectl logs ... -c setup-shim`. Operator fixes DNS/Service/NetworkPolicy.

**Cache server has lost the action source** (e.g. controller PVC corruption). Setup-shim 404s, fails immediately. Operator restarts the controller; next job triggers `LoadAction` to re-clone from upstream. Self-healing not in v1 — making the cache server re-clone on miss expands its security surface (outbound git calls).

**Disk pressure from emptyDir.** Job pod's `/actions` adds single-digit MB to ephemeral storage per pod. The workspace emptyDir is already much larger. If ever a problem, set `volumes[].emptyDir.sizeLimit` and document.

## Tests

### Unit

- `pkg/cache/handler_test.go`: happy/404/validation/streaming for the new route; `.git/` excluded from output.
- `pkg/cache/tar_test.go` (new): exclusions only match top-level (`.gitignore` not excluded); symlinks tarred as symlinks; empty dir tars cleanly.
- `cmd/entrypoint/setup_test.go` (new): happy path against `httptest.Server`; retry-then-success; retry-then-fail; fail-fast on 404; reject path-traversal in tar entries.
- `pkg/k8s/builder_test.go`: rewrite the existing PVC-mount assertion (`:102-118`). New asserts: `actions` emptyDir exists; setup-shim and runner both mount it; manifest JSON contains `Actions[]` with correct URLs.

### Integration

No CI fixture for real ZFS-LocalPV. The acceptance test is manual against `gt.monoloco.net` (the bug-repro cluster):

1. Build & push image, `helm upgrade`.
2. Push a workflow using `actions/checkout@v4`.
3. Verify: no FailedMount events on the job pod; setup-shim logs show successful action fetch; workflow runs to completion.
4. Verify a no-actions workflow still runs (the no-actions path).
5. Verify `actions/cache@v4` still works (the artifact cache is independent and unaffected).

## Out of scope

- **Per-repo workspace ZFS datasets.** Discussed during design and deferred. The current label-based per-repo isolation gives per-repo visibility (snapshot listing, GC); per-repo *enforcement* (quota, retention) is the only thing per-repo datasets would add, and there is no operator complaint pointing at it yet. Open a follow-up issue if/when one arises.
- **Self-heal on action-source cache miss.** Operator-restart for v1; revisit only if it becomes a recurring operational burden.
- **Concurrent action fetches in setup.** Sequential is fine for typical 2-5 actions/job.
- **Tarball caching in the cache server.** Rebuild-on-request is cheap; revisit on measured load.

## Acceptance

From `bugs/001-...md`:

- A workflow using `actions/checkout@v4` runs to completion on a single-node cluster with RWO-only storage.
- `kubectl describe pod drawbar-run-N-XXXXX` shows no FailedMount events.
- `pkg/k8s/builder_test.go:102-118` is rewritten to match the new architecture.

Plus:

- `actions/cache@v4` still works (artifact cache unaffected).
- A workflow with no actions still works (the no-actions path).
- `JobConfig.CachePVCName` and `CacheConfig.PVCName` are deleted (compile-time guarantee that no future job-build code can reintroduce a job-pod mount of the controller's PVC).
