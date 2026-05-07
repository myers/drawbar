# `watchJobWith` reports false failure on transient log-stream errors, and orphaned-job cleanup leaks the snapshot-cache PVC

**Status: fixed 2026-05-07.** Surfaced 2026-05-06 via `/ultrareview` run on PR #1.
Two findings on the controller-side k8s lifecycle path that bug 016
just touched. Same domain (watcher / cleanup), same diagnostic toolkit
(kube event timing, pod state, PVC ownership).

**Resolution.**
- Finding A: `watchJobWith` now inspects the streamLogs error. On a non-EOF
  error it calls a new `waitForContainerTerminated` helper (bounded 30s)
  before reading exit code, so a still-running container doesn't get
  reported as `runner container status not found` / `RESULT_FAILURE`.
- Finding B: snapshot-cache PVCs are now labeled with `drawbar.dev/task-id`
  at creation. `cleanupOrphanedJobs` deletes any matching PVC alongside
  each orphaned Job, so controller restarts don't leak cache PVCs.

## Finding A — `watchJobWith` swallows non-EOF log-stream errors and reports the job failed

**Location:** `pkg/k8s/watcher.go:124` (the `<-logDone` discard) and
`:404-407` (`getContainerResult` "runner container status not found"
error).

**The bug.** After bug 016's work, `watchJobWith` does:

```go
logDone := make(chan error, 1)
go func() {
    logDone <- streamLogs(ctx, streamer, namespace, podName, "runner", rep, cfg.CommandProc)
}()
...
<-logDone                          // ← error is discarded
cancelStream()
res := <-stateDone
drainStateFile(ctx, executor, namespace, podName, rep, res.offset)
result, err := getContainerResult(ctx, client, namespace, podName)
if err != nil {
    return runnerv1.Result_RESULT_FAILURE, err
}
return result, nil
```

`streamLogs` returns `nil` on EOF (normal container exit) but a
non-EOF error on any read failure mid-stream (kube-apiserver GOAWAY,
network reset, watch closed). The outer code discards both equally.

When `streamLogs` returned an error mid-job, the runner container
likely is **still running** (the error came from the apiserver-side
streaming connection, not from the container). The controller then
calls `getContainerResult`:

```go
func getContainerResult(ctx context.Context, client kubernetes.Interface, namespace, podName string) (runnerv1.Result, error) {
    pod, err := client.CoreV1().Pods(namespace).Get(...)
    ...
    for _, cs := range pod.Status.ContainerStatuses {
        if cs.Name == "runner" {
            if cs.State.Terminated != nil {
                if cs.State.Terminated.ExitCode == 0 {
                    return runnerv1.Result_RESULT_SUCCESS, nil
                }
                return runnerv1.Result_RESULT_FAILURE, nil
            }
        }
    }
    return runnerv1.Result_RESULT_FAILURE, fmt.Errorf("runner container status not found")
}
```

If the container is `Running` (not `Terminated`), no terminated state
exists, the loop exits, and we hit `runner container status not found`
— a job that's still happily executing is reported as `FAILURE` to
the forge.

**Concrete consequence.** Under apiserver stress (rolling restart,
network blip, kubelet hiccup), a normally-passing job is reported
as failed. The pod keeps running until its own timeout but the forge
already considers the run finished. Worse: drawbar releases the
runner slot and may pick up another task while the orphaned pod is
still consuming cluster resources — and bug 016's drain code already
ran with a wrong offset, so log/state reporting after the false
failure is also broken.

**Fix sketch.** Inspect the streamLogs error and act accordingly:

```go
logErr := <-logDone
cancelStream()
res := <-stateDone
drainStateFile(ctx, executor, namespace, podName, rep, res.offset)

if logErr != nil && !errors.Is(logErr, io.EOF) {
    // Stream broke mid-job. The container may still be running.
    // Wait for terminal pod state with a bounded timeout instead of
    // relying on the broken log stream as a "container exited" signal.
    if waitErr := waitForContainerTerminated(ctx, client, namespace, podName, "runner", 30*time.Second); waitErr != nil {
        return runnerv1.Result_RESULT_FAILURE, fmt.Errorf("waiting for container after log stream failure: %w", waitErr)
    }
}

result, err := getContainerResult(ctx, client, namespace, podName)
...
```

`waitForContainerTerminated` is a new helper analogous to the existing
`waitForContainerRunning`. Its absence is the underlying gap.

## Finding B — `cleanupOrphanedJobs` deletes the Job but leaks the snapshot-cache PVC

**Location:** `cmd/controller/main.go:215` (or thereabouts —
`cleanupOrphanedJobs`).

**The bug.** `cleanupOrphanedJobs` walks Jobs labeled with the
controller's identity and deletes any whose state indicates abandonment
(controller restart left a Job that no current poll loop owns). It
does not also delete the associated snapshot-cache PVC, which
`SnapshotManager` creates with a name derived from the Job's
identifying labels.

Every controller restart that occurs *while a job is running* with the
snapshot-cache feature enabled leaves a PVC behind. The PVC is bound
to a PV; the PV consumes underlying storage (ZFS-backed in the
intended deployment shape). Over time you accumulate dead PVCs that
nothing else cleans up.

**Fix sketch.** In `cleanupOrphanedJobs`, after deleting the Job and
confirming the pod is gone, also delete the matching PVC by label
selector. Use the same labels that `SnapshotManager` uses to create
the PVC. Check with the snapshot package's docs/comments for the
exact label keys — likely `drawbar.io/job` or similar.

A safer variant: have `SnapshotManager` set the Job as the PVC's
`OwnerReference` so kube garbage collection handles the cascade. That
way Job-delete cascades to PVC-delete via standard kube semantics.
Worth checking whether owner refs across namespaces / kinds are
allowed for PVCs (they are, within the same namespace).

## Why these belong together

Both are about **the controller-side k8s lifecycle being incomplete
after bug 016**. The watcher landing was the trigger to look at this
area; both findings extend that work. Same files, same mental model,
same kube-watching diagnostic flow.

## Test plan sketch

- A: a unit-style test for `watchJobWith` where the LogStreamer mock
  returns a non-EOF error after some delay while the pod-status mock
  shows `Running`. Assert the function does not return
  `runner container status not found`.
- A integration: simulate apiserver-flap by making the streamer
  return `connection reset` mid-stream; assert the job result still
  gets reported correctly when the container actually exits.
- B: an integration test where snapshots are enabled, controller
  restarts mid-job, and cleanupOrphanedJobs runs. Assert no orphan
  PVCs remain after cleanup completes.

## Source

Filed via `/ultrareview` run on PR #1, 2026-05-06.

## Related

- Bug 002 (orphaned-job-after-restart): adjacent — same theme of
  "controller restart leaves cluster state behind." Finding B is
  effectively a missing piece of bug 002's resolution.
- Bug 016 (per-step reporting): finding A is in the same function bug
  016 rewrote. Carrying through the same `<-logDone` pattern without
  inspecting the error was a bug-016-era oversight.
