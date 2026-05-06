# Graceful drain on SIGTERM — Design

**Status:** draft, not yet implemented.
**Scope:** option B from the brainstorm — bounded drain driven by config, hard-kill the orphan Job after the budget expires, post a clean failure to the forge. Resumable state / cross-restart adoption (option C) is explicitly out of scope.

## Problem

When the controller pod terminates (image bump, eviction, node drain), the in-flight k8s Job pods it was watching are orphaned: the runner container keeps executing, the controller goroutine that was streaming logs and reporting state has gone away, and the forge sees the run as in-flight until its server-side timeout fires. Logs from the last segment never arrive.

Two contributing pieces:

- `cmd/controller/main.go:296` hardcodes a 30s drain budget for `poller.Shutdown(shutCtx)`. There is no way to tune this for clusters with longer-running jobs.
- `deploy/helm/drawbar/templates/deployment.yaml` sets no `terminationGracePeriodSeconds`, so the kubelet defaults to 30s and SIGKILLs the controller before any non-trivial drain can complete.
- Even with a long drain, k8s Jobs are peer objects, not children of the controller pod. They survive controller exit and keep producing output nobody reads.

The poller-side drain (cancel handler ctx on timeout, wait for handlers to return) already exists from the recent `Shutdown(ctx)` work. What's missing is the config plumbing, the chart's grace period, and the *handler-side* recovery: report a clean failure to the forge and delete the Job before the controller exits.

## Non-goals

- **Resumable state.** A successor controller does not adopt orphaned Jobs. Doing this requires persisting `(taskID, jobName, log cursor, step index)` to a PVC, idempotent reporting, dedup against double-reports, and a lease to prevent two controllers briefly overlapping. Not worth the complexity until capacity > 1 with routinely long jobs is a real workload.
- **Catching SIGKILL.** If the kubelet sends SIGKILL before drain completes (oversubscribed node, OOM), the orphan condition reverts to today's behavior. Mitigation is "raise both knobs in tandem."

## Reference: upstream `act_runner`

Upstream (`reference/runner/internal/app/cmd/daemon.go:182`) reads `cfg.Runner.ShutdownTimeout` from YAML and passes it to `poller.Shutdown(ctx, timeout)`. Drawbar's `Poller` already has a near-identical `Shutdown(ctx)` (the bug-013 reshape closed the gap), but the controller hardcodes the timeout. Adding the config knob is straight upstream parity.

Upstream has *no orphan problem* because their jobs run as Docker containers in the runner's process tree — when the runner dies, the containers die with it. Drawbar is structurally different (k8s Jobs are peer objects), so the orphan handling is drawbar-native; there is no upstream pattern to mirror.

## Design

Three independent changes.

### 1. Config knob

Add `Runner.ShutdownTimeout time.Duration` (yaml: `shutdown_timeout`) to `pkg/config/config.go`.

- **Default:** `clamp(min(Runner.Timeout, 10*Runner.FetchTimeout), 30s, 5m)`. With the current defaults (`Runner.Timeout=3h`, `Runner.FetchTimeout=30s`) this evaluates to `clamp(min(3h, 5m), 30s, 5m)` = `5m`. Long enough to flush the final UpdateTask + Job delete; short enough that pod eviction during a node drain doesn't take hours. **Note:** this is the Go config default (used when `runner.shutdown_timeout` is absent from the YAML). The Helm chart sets the key explicitly to `60s` (see §3), so chart-installed runners see `60s` unless overridden.
- **Validation** (`Validate()`):
  - `ShutdownTimeout >= 1s` (anything shorter can't fit one UpdateTask round-trip).
  - `ShutdownTimeout <= Runner.Timeout` (no point waiting longer than a single job can run).
- **Env override:** `RUNNER_SHUTDOWN_TIMEOUT` (parses via `time.ParseDuration`).

### 2. Controller drain + orphan kill

`cmd/controller/main.go::run()`:

- Replace the literal `30 * time.Second` at line 296 with `cfg.Runner.ShutdownTimeout`.
- Log the resolved value at startup alongside the existing `runner is online` line so it's visible to operators.

`cmd/controller/main.go::makeTaskHandler`:

- At handler entry, derive `shutdownReportCtx, cancelReport := context.WithTimeout(context.Background(), 5*time.Second)`. **Rooted at `Background()`, not at handler `ctx`** — handler `ctx` is `jobsCtx`-derived and gets cancelled by `Shutdown`'s timeout branch; a derivative would inherit that cancellation. Defer `cancelReport()`.
- After `WatchJob` returns, branch on `ctx.Err() != nil` (i.e., handler ctx was cancelled mid-job). If true, this is the shutdown-recovery path:
  1. `rep.AddLog("controller restart, results may be incomplete")`
  2. `rep.Close(shutdownReportCtx, runnerv1.Result_RESULT_FAILURE)` — push the failure to the forge using the still-live ctx.
  3. `cfg.K8sClient.BatchV1().Jobs(cfg.Namespace).Delete(shutdownReportCtx, created.Name, metav1.DeleteOptions{PropagationPolicy: &foreground})` — kill the surviving Job pod cleanly. Foreground propagation so the apiserver waits for the pod to terminate before deleting the Job, but the apiserver returns immediately; we don't block on pod death.
  4. Skip snapshot *creation* (no point snapshotting a pod we just killed). Still call `cfg.SnapshotManager.DeletePVC(shutdownReportCtx, snapshotPVCName)` if a PVC was created — PVCs aren't owned by the k8s Job and would otherwise leak across controller restarts.
- Wrap this whole recovery branch in a deferred `recover()` that logs the panic stack and returns cleanly. A panic here would crash the controller process before other handlers finish their drain.
- Otherwise (success path or non-shutdown error), behavior unchanged.

### 3. Helm chart

`deploy/helm/drawbar/values.yaml`:

```yaml
runner:
  # ... existing keys ...
  shutdownTimeout: 60s   # default; raise for clusters running long jobs
```

`deploy/helm/drawbar/templates/configmap.yaml`:

Emit `runner.shutdown_timeout: {{ .Values.runner.shutdownTimeout }}` so the controller reads the same value the chart used.

`deploy/helm/drawbar/templates/deployment.yaml`:

Add to the pod spec:

```yaml
terminationGracePeriodSeconds: {{ include "drawbar.shutdownGraceSeconds" . }}
```

`deploy/helm/drawbar/templates/_helpers.tpl`:

```tpl
{{- /*
Convert runner.shutdownTimeout (Go duration string) to integer seconds and
add a 5-second buffer for the kubelet's SIGKILL after drain completes.
Supports a single-unit suffix: "Ns", "Nm", or "Nh". Mixed forms ("1h30m")
are not supported in the chart and will fail the template render with a
clear message — operators wanting compound durations should set the value
in seconds (e.g. "5400s" for 1.5h).
*/ -}}
{{- define "drawbar.shutdownGraceSeconds" -}}
{{- $d := .Values.runner.shutdownTimeout | default "60s" -}}
{{- $unit := $d | trimAll "0123456789" -}}
{{- $n := $d | trimSuffix $unit | int -}}
{{- $secs := 0 -}}
{{- if eq $unit "s" }}{{- $secs = $n -}}
{{- else if eq $unit "m" }}{{- $secs = mul $n 60 -}}
{{- else if eq $unit "h" }}{{- $secs = mul $n 3600 -}}
{{- else }}{{- fail (printf "runner.shutdownTimeout %q: unsupported unit %q (use Ns, Nm, or Nh)" $d $unit) -}}
{{- end -}}
{{- add $secs 5 -}}
{{- end -}}
```

Compound Go durations (`1h30m`) are accepted by the controller (which uses `time.ParseDuration`) but not by this helper. If an operator sets a compound value, `helm install` fails with a clear error pointing them at single-unit form. The Go side keeps the more permissive parser so a hand-written ConfigMap (non-Helm install) still works.

**Why +5s:** the buffer between `shutdown_timeout` (controller's drain budget) and `terminationGracePeriodSeconds` (kubelet's SIGKILL deadline) is the window in which the *report-failure-and-delete-Job* recovery has to complete. The handler uses a 5s `shutdownReportCtx` for those calls, so the buffer must be ≥ 5s. Larger buffer is fine but wastes time on planned restarts.

## Failure handling

| Scenario | Behavior |
| --- | --- |
| Drain finishes within `shutdown_timeout` | `Shutdown` returns nil, controller exits cleanly. No orphans, no lost logs. |
| Drain exceeds `shutdown_timeout`, recovery succeeds | Handler reports `RESULT_FAILURE`, Job deleted, controller exits. Forge sees the failure within seconds. |
| `rep.Close` fails in recovery branch (forge unreachable) | Log warn with task ID + jobName. Proceed to Job delete. Forge will eventually time the run out server-side — same as today, but the operator has breadcrumbs. |
| `Jobs().Delete` fails in recovery branch | Log warn with jobName + namespace + error. Controller exits anyway; on next start `cleanupOrphanedJobs` (already in tree) sweeps it. |
| Handler panics in recovery branch | Deferred `recover()` logs stack and returns cleanly; other handlers' drain continues. |
| Controller killed by SIGKILL before drain completes | Same orphan condition as today. Documented limitation. |
| Operator sets `RUNNER_SHUTDOWN_TIMEOUT` env to a value larger than chart's `terminationGracePeriodSeconds` | We can't read kubelet's grace period from inside the pod, so no runtime validation. Mitigation: log the resolved `shutdown_timeout` at startup so a misconfiguration is visible in `kubectl logs`. |

## Testing

**`pkg/config/config_test.go`:**
- Default falls within [30s, 5m] for default `Runner.Timeout` and `Runner.FetchTimeout`.
- `RUNNER_SHUTDOWN_TIMEOUT=2m` env override applies.
- `Validate()` rejects `0s` and `> Runner.Timeout`.

**`cmd/controller/main_test.go`:**
- New test: handler is mid-`WatchJob`, shutdown fires, handler observes ctx cancellation, calls `rep.Close(_, RESULT_FAILURE)` with the "controller restart" log line, deletes the Job. Use the existing `fake.NewSimpleClientset` + a fake watcher that blocks on `ctx.Done`. Mock reporter asserts the close call happened with a Background-rooted ctx (i.e., it didn't return `ctx.Canceled`).
- Existing `run()` tests still pass — the literal-to-config swap doesn't change semantics.

**`pkg/server/poller_test.go`:**
No changes. `Shutdown(ctx)` semantics are unchanged.

**Helm:**
- `helm template` with default values produces `terminationGracePeriodSeconds: 65` and ConfigMap `runner.shutdown_timeout: 60s`.
- Override `runner.shutdownTimeout: 5m` produces `terminationGracePeriodSeconds: 305` and `runner.shutdown_timeout: 5m`.
- Manual snapshot check; not blocking on `helm unittest` infrastructure.

**Manual integration check (documented, not automated):**
Via `hack/dev-env.sh`: kick off a long job, `kubectl delete pod` the controller mid-job, observe (a) forge marks the run failed within `shutdown_timeout + a few seconds`, (b) the run's last log line is the "controller restart" annotation, (c) `kubectl get jobs -n gitea` shows the Job gone within ~10s.

## Files touched

- `pkg/config/config.go` — `Runner.ShutdownTimeout` field, default, validation, env override.
- `pkg/config/config_test.go` — coverage for the above.
- `cmd/controller/main.go` — replace literal 30s, add handler-side recovery branch with `shutdownReportCtx` and Job delete.
- `cmd/controller/main_test.go` — recovery-branch test.
- `deploy/helm/drawbar/values.yaml` — `runner.shutdownTimeout: 60s`.
- `deploy/helm/drawbar/templates/configmap.yaml` — emit the new key.
- `deploy/helm/drawbar/templates/deployment.yaml` — `terminationGracePeriodSeconds`.
- `deploy/helm/drawbar/templates/_helpers.tpl` — `drawbar.shutdownGraceSeconds` template helper.

No new packages, no new files.
