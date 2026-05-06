# Graceful drain on SIGTERM Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace the controller's hardcoded 30 s shutdown drain with a configurable `runner.shutdown_timeout`, raise the Helm chart's `terminationGracePeriodSeconds` in tandem, and on hard-kill report `RESULT_FAILURE` to the forge plus delete the surviving k8s Job before the controller exits.

**Architecture:** Three independent in-tree changes. (1) `pkg/config/config.go` gains a `Runner.ShutdownTimeout` Duration with default `clamp(min(Runner.Timeout, 10*Runner.FetchTimeout), 30s, 5m)`, validated and overridable via `RUNNER_SHUTDOWN_TIMEOUT`. (2) `cmd/controller/main.go` reads that knob for the existing `poller.Shutdown(shutCtx)` call, and the per-task handler grows a recovery branch: when handler ctx is cancelled (i.e. drain timed out and the poller cancelled `jobsCtx`), the handler uses a fresh Background-rooted 5 s `shutdownReportCtx` to push a "controller restart, results may be incomplete" log line, call `rep.Close(_, RESULT_FAILURE)`, and delete the k8s Job with foreground propagation. (3) The Helm chart adds `runner.shutdownTimeout: 60s` and computes `terminationGracePeriodSeconds = shutdown_timeout + 5s` via a `_helpers.tpl` template. No new packages, no resumable cross-restart adoption.

**Tech Stack:** Go stdlib `context`, `time`, `os`, `log/slog`; `k8s.io/api/batch/v1`, `k8s.io/apimachinery/pkg/apis/meta/v1`, `k8s.io/client-go/kubernetes/fake`; `connect.NewResponse` mocks; Helm/Sprig templating; testify.

**Reference:** `docs/superpowers/specs/2026-05-05-graceful-drain-sigterm-design.md` is the design.

---

## File Structure

Files modified:

- `pkg/config/config.go` — `RunnerConfig` gains a `ShutdownTimeout time.Duration` field (yaml `shutdown_timeout`); `Default()` computes the clamped default; `Load` re-applies the default for zero values; `Validate()` enforces `>= 1s` and `<= Runner.Timeout`; `applyEnvOverrides()` reads `RUNNER_SHUTDOWN_TIMEOUT`.
- `pkg/config/config_test.go` — tests for the default math, env override, and validation rules.
- `cmd/controller/main.go` — `run()` swaps the literal `30 * time.Second` for `cfg.Runner.ShutdownTimeout` and adds a startup log line; `makeTaskHandler` derives a `shutdownReportCtx`, branches on `ctx.Err() != nil` after `WatchJob` returns, and on that branch calls `rep.AddLog` + `rep.Close(shutdownReportCtx, …)` + `Jobs().Delete(shutdownReportCtx, …, foreground)` instead of the existing post-watch path. The recovery branch is wrapped in `defer recover()` so a panic doesn't crash sibling handlers.
- `cmd/controller/handler_test.go` — new test exercising the recovery branch with a fake watcher that blocks until ctx is cancelled.
- `deploy/helm/drawbar/values.yaml` — `runner.shutdownTimeout: 60s`.
- `deploy/helm/drawbar/templates/configmap.yaml` — emit `runner.shutdown_timeout: {{ .Values.runner.shutdownTimeout | quote }}`.
- `deploy/helm/drawbar/templates/_helpers.tpl` — `drawbar.shutdownGraceSeconds` template that parses single-unit durations (`Ns`/`Nm`/`Nh`) and adds 5.
- `deploy/helm/drawbar/templates/deployment.yaml` — `terminationGracePeriodSeconds: {{ include "drawbar.shutdownGraceSeconds" . }}` on the pod spec.

No new files. No new packages.

---

## Task 1: Add `Runner.ShutdownTimeout` config field, default, env override, validation

**Files:**
- Modify: `pkg/config/config.go:27-40` (`RunnerConfig` struct), `:69-100` (`Default`), `:120-149` (`Load` zero-value re-apply block), `:158-173` (`Validate` runner block), `:204-275` (`applyEnvOverrides`).
- Modify: `pkg/config/config_test.go` (append new tests at end of file).

- [ ] **Step 1: Write failing tests for the new config knob**

Open `pkg/config/config_test.go` and append:

```go
func TestDefault_ShutdownTimeout(t *testing.T) {
	cfg := Default()
	// With default Runner.Timeout=3h and FetchTimeout=30s,
	// min(3h, 10*30s)=5m, clamped to [30s, 5m] = 5m.
	assert.Equal(t, 5*time.Minute, cfg.Runner.ShutdownTimeout)
}

func TestLoad_ShutdownTimeout_FromYAML(t *testing.T) {
	t.Setenv("RUNNER_NAME", "")
	content := `
server:
  url: http://localhost:3000
runner:
  labels: ["x:docker://alpine"]
  shutdown_timeout: 90s
`
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))

	cfg, err := Load(path)
	require.NoError(t, err)
	assert.Equal(t, 90*time.Second, cfg.Runner.ShutdownTimeout)
}

func TestLoad_ShutdownTimeout_DefaultAppliedForZero(t *testing.T) {
	// YAML omits shutdown_timeout entirely — Load should fall back to Default.
	t.Setenv("RUNNER_NAME", "")
	content := `
server:
  url: http://localhost:3000
runner:
  labels: ["x:docker://alpine"]
`
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))

	cfg, err := Load(path)
	require.NoError(t, err)
	assert.Equal(t, 5*time.Minute, cfg.Runner.ShutdownTimeout)
}

func TestLoad_ShutdownTimeout_FromEnv(t *testing.T) {
	t.Setenv("SERVER_URL", "http://server:3000")
	t.Setenv("RUNNER_SHUTDOWN_TIMEOUT", "2m")
	cfg, err := Load("/nonexistent/config.yaml")
	require.NoError(t, err)
	assert.Equal(t, 2*time.Minute, cfg.Runner.ShutdownTimeout)
}

func TestValidate_ShutdownTimeout_TooSmall(t *testing.T) {
	cfg := validConfig()
	cfg.Runner.ShutdownTimeout = 500 * time.Millisecond
	assert.ErrorContains(t, cfg.Validate(), "shutdown_timeout")
}

func TestValidate_ShutdownTimeout_LargerThanTimeout(t *testing.T) {
	cfg := validConfig()
	cfg.Runner.Timeout = 2 * time.Minute
	cfg.Runner.ShutdownTimeout = 5 * time.Minute
	assert.ErrorContains(t, cfg.Validate(), "shutdown_timeout")
}

func TestDefault_ShutdownTimeout_ClampLow(t *testing.T) {
	// Construct a config where 10*FetchTimeout would be below 30s and verify
	// the clamp floor kicks in. Since Default() uses the global defaults, we
	// exercise the helper directly rather than via Default().
	got := computeShutdownTimeoutDefault(1*time.Hour, 1*time.Second)
	assert.Equal(t, 30*time.Second, got)
}

func TestDefault_ShutdownTimeout_ClampHigh(t *testing.T) {
	got := computeShutdownTimeoutDefault(3*time.Hour, 10*time.Minute)
	assert.Equal(t, 5*time.Minute, got)
}

func TestDefault_ShutdownTimeout_PicksMin(t *testing.T) {
	// Runner.Timeout is the lower bound — short jobs get short drain.
	got := computeShutdownTimeoutDefault(45*time.Second, 30*time.Second)
	// min(45s, 10*30s) = 45s, clamped to [30s, 5m] = 45s.
	assert.Equal(t, 45*time.Second, got)
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `/Users/myers/.local/share/mise/installs/go/1.25.7/bin/go test ./pkg/config/ -run 'TestDefault_ShutdownTimeout|TestLoad_ShutdownTimeout|TestValidate_ShutdownTimeout' -v`

Expected: compile error / FAIL on `cfg.Runner.ShutdownTimeout` (field doesn't exist) and `computeShutdownTimeoutDefault` (function doesn't exist).

- [ ] **Step 3: Add the field, default helper, and validation**

In `pkg/config/config.go`, modify `RunnerConfig` (after the `Timeout` field, around line 34) to add:

```go
	Timeout         time.Duration `yaml:"timeout"`
	ShutdownTimeout time.Duration `yaml:"shutdown_timeout"`
```

Add the helper function (place it after `Default()`, around line 100):

```go
// computeShutdownTimeoutDefault returns the default Runner.ShutdownTimeout:
// min(Runner.Timeout, 10*Runner.FetchTimeout), clamped to [30s, 5m]. Long
// enough to flush the final UpdateTask + Job delete; short enough that pod
// eviction during a node drain doesn't take hours.
func computeShutdownTimeoutDefault(timeout, fetchTimeout time.Duration) time.Duration {
	const (
		floor   = 30 * time.Second
		ceiling = 5 * time.Minute
	)
	d := timeout
	if t := 10 * fetchTimeout; t < d {
		d = t
	}
	if d < floor {
		return floor
	}
	if d > ceiling {
		return ceiling
	}
	return d
}
```

In `Default()` (the `RunnerConfig` literal, around line 72), add `ShutdownTimeout` after `Timeout`:

```go
		Runner: RunnerConfig{
			Name:            hostname,
			Labels:          []string{"ubuntu-latest:docker://node:22-bookworm"},
			Capacity:        1,
			FetchInterval:   2 * time.Second,
			FetchTimeout:    30 * time.Second,
			Timeout:         3 * time.Hour,
			ShutdownTimeout: computeShutdownTimeoutDefault(3*time.Hour, 30*time.Second),
			GitCloneURL:     "",
			ActionsURL:      "",
		},
```

(Existing `Default()` doesn't list every field — keep its current shape and just add the `ShutdownTimeout` line in the same style as the other timeouts.)

In `Load()`'s zero-value re-apply block (around line 134, right after `if cfg.Runner.Timeout == 0 { ... }`), add:

```go
	if cfg.Runner.ShutdownTimeout == 0 {
		cfg.Runner.ShutdownTimeout = computeShutdownTimeoutDefault(cfg.Runner.Timeout, cfg.Runner.FetchTimeout)
	}
```

In `Validate()` (after the existing `Runner.Timeout` check at line 173), add:

```go
	if c.Runner.ShutdownTimeout < 1*time.Second {
		return fmt.Errorf("runner.shutdown_timeout must be >= 1s (got %s)", c.Runner.ShutdownTimeout)
	}
	if c.Runner.ShutdownTimeout > c.Runner.Timeout {
		return fmt.Errorf("runner.shutdown_timeout (%s) must be <= runner.timeout (%s)", c.Runner.ShutdownTimeout, c.Runner.Timeout)
	}
```

In `applyEnvOverrides()` (after the existing `RUNNER_*` blocks, around line 230), add:

```go
	if v := os.Getenv("RUNNER_SHUTDOWN_TIMEOUT"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			c.Runner.ShutdownTimeout = d
		}
	}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `/Users/myers/.local/share/mise/installs/go/1.25.7/bin/go test ./pkg/config/ -v`

Expected: all PASS, including the new tests and the existing suite.

- [ ] **Step 5: Commit**

```bash
git add pkg/config/config.go pkg/config/config_test.go
git commit -m "config: add Runner.ShutdownTimeout knob

Plumbs cfg.Runner.ShutdownTimeout (yaml: shutdown_timeout, env:
RUNNER_SHUTDOWN_TIMEOUT) with a clamped default of
min(Runner.Timeout, 10*Runner.FetchTimeout) clamped to [30s, 5m].
Validates >= 1s and <= Runner.Timeout. Replaces the controller's
hardcoded 30s drain budget in a follow-up commit."
```

---

## Task 2: Wire `cfg.Runner.ShutdownTimeout` into `poller.Shutdown` call

**Files:**
- Modify: `cmd/controller/main.go:289-302` (the post-`Run` shutdown block).

- [ ] **Step 1: Replace the literal 30s with the config value**

In `cmd/controller/main.go`, find the block at line 289-302:

```go
	slog.Info("runner is online, polling for tasks",
		"job_namespace", deps.namespace,
		"poll_staleness_threshold", pollStaleness,
		"success_fetch_staleness_threshold", successFetchStaleness,
	)
	poller.Run(ctx)
	slog.Info("poller stopped, draining in-flight tasks")
	shutCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := poller.Shutdown(shutCtx); err != nil {
		slog.Warn("shutdown drain ended early", "error", err)
	}
	slog.Info("runner shut down")
	return nil
```

Change to:

```go
	slog.Info("runner is online, polling for tasks",
		"job_namespace", deps.namespace,
		"poll_staleness_threshold", pollStaleness,
		"success_fetch_staleness_threshold", successFetchStaleness,
		"shutdown_timeout", cfg.Runner.ShutdownTimeout,
	)
	poller.Run(ctx)
	slog.Info("poller stopped, draining in-flight tasks", "timeout", cfg.Runner.ShutdownTimeout)
	shutCtx, cancel := context.WithTimeout(context.Background(), cfg.Runner.ShutdownTimeout)
	defer cancel()
	if err := poller.Shutdown(shutCtx); err != nil {
		slog.Warn("shutdown drain ended early", "error", err)
	}
	slog.Info("runner shut down")
	return nil
```

- [ ] **Step 2: Build to verify compile**

Run: `/Users/myers/.local/share/mise/installs/go/1.25.7/bin/go build ./...`

Expected: clean build.

- [ ] **Step 3: Run existing tests for regression**

Run: `/Users/myers/.local/share/mise/installs/go/1.25.7/bin/go test ./cmd/controller/ -run 'TestRun' -v`

Expected: existing tests still pass (this is a literal-to-config swap; no semantics changed).

- [ ] **Step 4: Commit**

```bash
git add cmd/controller/main.go
git commit -m "controller: drive shutdown drain budget from cfg.Runner.ShutdownTimeout

Replaces the hardcoded 30s shutCtx timeout with the new config
knob. Logs the resolved value at startup and at drain so a
misconfiguration is visible in 'kubectl logs'."
```

---

## Task 3: Add handler-side shutdown recovery branch (report failure + delete Job)

**Files:**
- Modify: `cmd/controller/main.go:533-869` (`makeTaskHandler` body).

The recovery branch fires when `ctx.Err() != nil` after `WatchJob` returns — i.e., the poller's `Shutdown` cancelled `jobsCtx` because the drain budget expired. It must use a Background-rooted context (because every derivative of the cancelled handler ctx is also cancelled) to push the failure to the forge and delete the surviving k8s Job.

- [ ] **Step 1: Write failing test for the recovery branch**

Append to `cmd/controller/handler_test.go`:

```go
// blockingExecutor blocks Exec calls until ctx.Done fires, then returns
// ctx.Err. Used to simulate a long-running pod that's still mid-execution
// when the controller decides to shut down.
type blockingExecutor struct{}

func (blockingExecutor) Exec(ctx context.Context, _, _, _ string, _ []string) (string, error) {
	<-ctx.Done()
	return "", ctx.Err()
}

// blockingStreamer returns a reader that blocks on Read until ctx is cancelled.
type blockingStreamer struct{}

func (blockingStreamer) StreamLogs(ctx context.Context, _, _, _ string) (io.ReadCloser, error) {
	pr, pw := io.Pipe()
	go func() {
		<-ctx.Done()
		pw.CloseWithError(ctx.Err())
	}()
	return pr, nil
}

func TestMakeTaskHandler_ShutdownRecovery_ReportsFailureAndDeletesJob(t *testing.T) {
	taskID := int64(200)
	jobName := fmt.Sprintf("server-run-%d", taskID)

	// 1. Fake Forgejo server.
	fjs := &fakeForgejoServer{}
	server := httptest.NewServer(fjs.serveMux("/api/actions"))
	t.Cleanup(server.Close)
	forgejoClient := forgeserver.NewClient(server.URL, false, "uuid", "token", time.Second, 5*time.Second)

	// 2. Fake k8s client; create a pod for the job once it appears so
	//    waitForContainerRunning unblocks and we get into the streaming phase
	//    where the blocking executor/streamer can be cancelled.
	k8sClient := fake.NewSimpleClientset()
	go func() {
		for i := 0; i < 100; i++ {
			time.Sleep(10 * time.Millisecond)
			jobs, _ := k8sClient.BatchV1().Jobs("test-ns").List(context.Background(), metav1.ListOptions{})
			if len(jobs.Items) > 0 {
				pod := &corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:      jobName + "-pod",
						Namespace: "test-ns",
						Labels:    map[string]string{"job-name": jobs.Items[0].Name},
					},
					Status: corev1.PodStatus{
						ContainerStatuses: []corev1.ContainerStatus{
							{
								Name:  "runner",
								State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
							},
						},
					},
				}
				k8sClient.CoreV1().Pods("test-ns").Create(context.Background(), pod, metav1.CreateOptions{})
				return
			}
		}
	}()

	// 3. Build task with a single step.
	task := &runnerv1.Task{
		Id: taskID,
		WorkflowPayload: []byte(`name: Test
on: [push]
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - run: sleep 60
`),
		Context: &structpb.Struct{
			Fields: map[string]*structpb.Value{
				"server_url": structpb.NewStringValue("https://server.example.com"),
				"token":      structpb.NewStringValue("test-token"),
			},
		},
		Secrets: map[string]string{},
	}

	// 4. Run handler with a ctx that gets cancelled mid-execution to
	//    simulate poller.Shutdown's jobsCtx cancellation.
	handler := makeTaskHandler(TaskHandlerConfig{
		K8sClient:     k8sClient,
		ServerClient:  forgejoClient,
		Labels:        labels.Labels{labels.MustParse("ubuntu-latest:docker://node:24")},
		Namespace:     "test-ns",
		Timeout:       5 * time.Minute,
		WatchConfig: k8s.WatchConfig{
			PollInterval: 20 * time.Millisecond,
			Executor:     blockingExecutor{},
			Streamer:     blockingStreamer{},
		},
	})

	ctx, cancel := context.WithCancel(context.Background())
	// Cancel after a brief delay to let the handler get into WatchJob.
	go func() {
		time.Sleep(150 * time.Millisecond)
		cancel()
	}()

	done := make(chan struct{})
	go func() {
		handler(ctx, task)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("handler did not return after ctx cancel")
	}

	// 5. Verify: forge received a final UpdateTask with RESULT_FAILURE.
	fjs.mu.Lock()
	assert.Greater(t, fjs.taskCalls, 0, "should have sent task updates")
	assert.Equal(t, runnerv1.Result_RESULT_FAILURE, fjs.lastResult,
		"recovery branch should report RESULT_FAILURE")
	fjs.mu.Unlock()

	// 6. Verify: k8s Job was deleted.
	jobs, err := k8sClient.BatchV1().Jobs("test-ns").List(context.Background(), metav1.ListOptions{})
	require.NoError(t, err)
	assert.Empty(t, jobs.Items, "recovery branch should delete the k8s Job")
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `/Users/myers/.local/share/mise/installs/go/1.25.7/bin/go test ./cmd/controller/ -run 'TestMakeTaskHandler_ShutdownRecovery' -v`

Expected: FAIL. The current handler calls `rep.Close(ctx, RESULT_FAILURE)` with the *cancelled* ctx, so the UpdateTask request returns `ctx.Canceled` and never reaches the forge — `fjs.taskCalls` stays 0 (or the assertion on `RESULT_FAILURE` fails). The k8s Job also isn't deleted.

- [ ] **Step 3: Add the recovery branch in `makeTaskHandler`**

In `cmd/controller/main.go`, find the existing post-watch block (lines 837-867):

```go
		result, err := k8s.WatchJob(ctx, cfg.K8sClient, cfg.RestConfig, cfg.Namespace, created.Name, rep, watchCfg)
		if err != nil {
			slog.Error("job watch error", "error", err)
			rep.AddLog(fmt.Sprintf("Job execution error: %v", err))
			result = runnerv1.Result_RESULT_FAILURE
		}

		// Report final result.
		if err := rep.Close(ctx, result); err != nil {
			slog.Error("failed to report final result", "error", err)
		}

		// ZFS snapshot cache: snapshot on success, always delete PVC.
		if snapshotPVCName != "" && cfg.SnapshotManager != nil {
			...
		}

		slog.Info("task completed", "task_id", task.GetId(), "result", result)
```

Replace with:

```go
		result, err := k8s.WatchJob(ctx, cfg.K8sClient, cfg.RestConfig, cfg.Namespace, created.Name, rep, watchCfg)
		if err != nil {
			slog.Error("job watch error", "error", err)
			rep.AddLog(fmt.Sprintf("Job execution error: %v", err))
			result = runnerv1.Result_RESULT_FAILURE
		}

		// If the handler ctx was cancelled, this is the controller's shutdown
		// drain timing out. The handler ctx (and every derivative) is dead, so
		// the final UpdateTask and Job delete need a fresh Background-rooted
		// context. The 5s budget matches the chart's terminationGracePeriodSeconds
		// buffer (shutdown_timeout + 5s).
		if ctx.Err() != nil {
			runShutdownRecovery(cfg, task.GetId(), created.Name, rep)
			slog.Info("task completed", "task_id", task.GetId(), "result", "shutdown")
			return
		}

		// Report final result.
		if err := rep.Close(ctx, result); err != nil {
			slog.Error("failed to report final result", "error", err)
		}

		// ZFS snapshot cache: snapshot on success, always delete PVC.
		if snapshotPVCName != "" && cfg.SnapshotManager != nil {
			...
		}

		slog.Info("task completed", "task_id", task.GetId(), "result", result)
```

(Keep the existing snapshot block exactly as it was in the original — only the new `if ctx.Err() != nil` check above it is new.)

Add the `runShutdownRecovery` helper. Place it just below `reportFailure` (around line 879):

```go
// runShutdownRecovery is invoked from the task handler when the handler
// context has been cancelled by poller.Shutdown's drain timeout. It pushes
// a final RESULT_FAILURE to the forge and deletes the surviving k8s Job
// using a fresh Background-rooted context (the handler ctx is dead, so
// any derivative would be cancelled too). The defer recover() prevents a
// panic here from crashing sibling handlers' drain.
func runShutdownRecovery(cfg TaskHandlerConfig, taskID int64, jobName string, rep *reporter.Reporter) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("panic in shutdown recovery", "task_id", taskID, "panic", r)
		}
	}()

	shutdownReportCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	rep.AddLog("controller restart, results may be incomplete")
	if err := rep.Close(shutdownReportCtx, runnerv1.Result_RESULT_FAILURE); err != nil {
		slog.Warn("shutdown recovery: final report failed",
			"task_id", taskID, "job", jobName, "error", err)
	}

	propagation := metav1.DeletePropagationForeground
	if err := cfg.K8sClient.BatchV1().Jobs(cfg.Namespace).Delete(
		shutdownReportCtx, jobName,
		metav1.DeleteOptions{PropagationPolicy: &propagation},
	); err != nil {
		slog.Warn("shutdown recovery: job delete failed",
			"task_id", taskID, "job", jobName, "namespace", cfg.Namespace, "error", err)
	}
}
```

`metav1` is already imported in `cmd/controller/main.go` (line 44); `context` is imported (line 4); `reporter` is imported.

- [ ] **Step 4: Run the test to verify it passes**

Run: `/Users/myers/.local/share/mise/installs/go/1.25.7/bin/go test ./cmd/controller/ -run 'TestMakeTaskHandler_ShutdownRecovery' -v`

Expected: PASS. The handler enters the recovery branch on ctx cancellation, pushes RESULT_FAILURE to the fake forge (`taskCalls > 0` and `lastResult == RESULT_FAILURE`), and deletes the k8s Job (the list returns empty).

- [ ] **Step 5: Run the full handler test suite for regression**

Run: `/Users/myers/.local/share/mise/installs/go/1.25.7/bin/go test ./cmd/controller/ -v`

Expected: all PASS, including `TestMakeTaskHandler_RunStep_Success` (the success path is unchanged).

- [ ] **Step 6: Commit**

```bash
git add cmd/controller/main.go cmd/controller/handler_test.go
git commit -m "controller: report failure + delete Job on shutdown drain timeout

When poller.Shutdown's drain budget expires it cancels the
handler ctx mid-WatchJob. The handler now detects this via
ctx.Err() and runs a recovery branch on a fresh Background-rooted
5s context: push 'controller restart, results may be incomplete'
+ RESULT_FAILURE to the forge, then delete the surviving k8s Job
with foreground propagation so the runner pod stops cleanly.

Without this, the forge would see the run hang until its
server-side timeout fired and the orphaned Job pod would keep
consuming cluster resources for no reason."
```

---

## Task 4: Helm chart — `runner.shutdownTimeout` value, ConfigMap, deployment grace period

**Files:**
- Modify: `deploy/helm/drawbar/values.yaml:11-26` (`runner` block).
- Modify: `deploy/helm/drawbar/templates/configmap.yaml:11-28` (runner block).
- Modify: `deploy/helm/drawbar/templates/_helpers.tpl` (append new helper).
- Modify: `deploy/helm/drawbar/templates/deployment.yaml:23` (pod spec).

- [ ] **Step 1: Add `runner.shutdownTimeout` to `values.yaml`**

In `deploy/helm/drawbar/values.yaml`, find the `runner:` block (lines 11-26) and add `shutdownTimeout` after `timeout`:

```yaml
runner:
  name: k8s-runner
  labels:
    - "ubuntu-latest:docker://node:24-trixie"
  capacity: 1
  fetchInterval: 2s
  fetchTimeout: 30s
  timeout: 30m
  shutdownTimeout: 60s   # drain budget on SIGTERM; chart sets terminationGracePeriodSeconds = shutdownTimeout + 5s
  gitCloneUrl: ""  # defaults to server.url
  jobNamespace: ""  # defaults to release namespace
```

- [ ] **Step 2: Emit `shutdown_timeout` in the ConfigMap**

In `deploy/helm/drawbar/templates/configmap.yaml`, find the runner block (line 11+) and add a line after the `timeout:` line:

```yaml
    runner:
      name: {{ .Values.runner.name | quote }}
      labels:
{{ .Values.runner.labels | toYaml | indent 8 }}
      capacity: {{ .Values.runner.capacity }}
      fetch_interval: {{ .Values.runner.fetchInterval | quote }}
      fetch_timeout: {{ .Values.runner.fetchTimeout | quote }}
      timeout: {{ .Values.runner.timeout | quote }}
      shutdown_timeout: {{ .Values.runner.shutdownTimeout | quote }}
```

- [ ] **Step 3: Add `drawbar.shutdownGraceSeconds` helper**

Append to `deploy/helm/drawbar/templates/_helpers.tpl`:

```tpl
{{- /*
Convert runner.shutdownTimeout (Go duration string) to integer seconds and
add a 5-second buffer for the kubelet's SIGKILL after drain completes.
Supports a single-unit suffix: "Ns", "Nm", or "Nh". Mixed forms ("1h30m")
are accepted by the controller's time.ParseDuration but not by this helper —
use seconds (e.g. "5400s" for 1.5h) for compound durations.
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

- [ ] **Step 4: Set `terminationGracePeriodSeconds` on the pod spec**

In `deploy/helm/drawbar/templates/deployment.yaml`, find the pod spec (line 23, the line `      serviceAccountName: ...`) and add `terminationGracePeriodSeconds` immediately above it:

```yaml
    spec:
      terminationGracePeriodSeconds: {{ include "drawbar.shutdownGraceSeconds" . }}
      serviceAccountName: {{ include "drawbar.serviceAccountName" . }}
```

- [ ] **Step 5: Verify default render**

Run from the repo root:

```bash
helm template test deploy/helm/drawbar/ --set server.url=http://localhost:3000 | grep -E 'terminationGracePeriodSeconds|shutdown_timeout'
```

Expected output:

```
      terminationGracePeriodSeconds: 65
      shutdown_timeout: "60s"
```

- [ ] **Step 6: Verify override render with minutes**

```bash
helm template test deploy/helm/drawbar/ --set server.url=http://localhost:3000 --set runner.shutdownTimeout=5m | grep -E 'terminationGracePeriodSeconds|shutdown_timeout'
```

Expected:

```
      terminationGracePeriodSeconds: 305
      shutdown_timeout: "5m"
```

- [ ] **Step 7: Verify override render with hours**

```bash
helm template test deploy/helm/drawbar/ --set server.url=http://localhost:3000 --set runner.shutdownTimeout=1h | grep -E 'terminationGracePeriodSeconds|shutdown_timeout'
```

Expected:

```
      terminationGracePeriodSeconds: 3605
      shutdown_timeout: "1h"
```

- [ ] **Step 8: Verify the helper rejects compound durations**

```bash
helm template test deploy/helm/drawbar/ --set server.url=http://localhost:3000 --set runner.shutdownTimeout=1h30m 2>&1 | grep -i shutdown
```

Expected: a `helm template` error message containing `runner.shutdownTimeout "1h30m": unsupported unit "h30m" (use Ns, Nm, or Nh)`.

- [ ] **Step 9: Commit**

```bash
git add deploy/helm/drawbar/values.yaml \
        deploy/helm/drawbar/templates/configmap.yaml \
        deploy/helm/drawbar/templates/_helpers.tpl \
        deploy/helm/drawbar/templates/deployment.yaml
git commit -m "helm: terminationGracePeriodSeconds tied to runner.shutdownTimeout

Adds runner.shutdownTimeout (default 60s), emits it as
runner.shutdown_timeout in the ConfigMap so the controller picks
it up, and computes the pod's terminationGracePeriodSeconds as
shutdownTimeout + 5s via a new drawbar.shutdownGraceSeconds
helper. The +5s buffer is the window the controller's recovery
branch uses to push a final RESULT_FAILURE and delete the
surviving k8s Job before the kubelet SIGKILLs the controller pod."
```

---

## Task 5: Final integration verification

**Files:** none modified.

- [ ] **Step 1: Run the full Go test suite**

Run: `/Users/myers/.local/share/mise/installs/go/1.25.7/bin/go test ./...`

Expected: all PASS.

- [ ] **Step 2: Run the linter**

Run: `make lint`

Expected: clean (no new findings introduced by these changes).

- [ ] **Step 3: Render the chart and inspect the deployment**

Run:

```bash
helm template test deploy/helm/drawbar/ --set server.url=http://localhost:3000 > /tmp/drawbar-rendered.yaml
```

Inspect `/tmp/drawbar-rendered.yaml`:

- ConfigMap contains `shutdown_timeout: "60s"` under the `runner:` block.
- Deployment pod spec contains `terminationGracePeriodSeconds: 65`.

(Per CLAUDE.md, don't actually use `/tmp` — render to `./tmp-drawbar-rendered.yaml` in the repo and remove it after inspection. Or pipe directly to `grep`.)

- [ ] **Step 4: Manual end-to-end check (optional, document in PR)**

Using `hack/dev-env.sh`:

```bash
./hack/dev-env.sh up
# Trigger a long-running workflow run on the dev forge.
# Once the runner pod is mid-job:
kubectl delete pod -n drawbar -l app=drawbar
# Within ~60s, the forge should mark the run failed with the
# "controller restart, results may be incomplete" log line.
# kubectl get jobs -n gitea should show the Job gone within ~10s.
```

This is a manual smoke test; not a blocking step, but worth noting in the PR description. (No automated integration test for this — the existing Go unit test covers the recovery branch logic; the end-to-end behavior depends on the kubelet's actual SIGTERM-then-SIGKILL flow, which a unit test can't observe.)
