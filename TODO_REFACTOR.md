# drawbar — refactor north star

This doc captures the architectural direction surfaced while
diagnosing bug 026 (step end-event not reaching the reporter). The
specific bug has a small fix; this doc is about the larger shape
the codebase wants to grow into so we stop hitting variants of the
same class of problem.

**Status: design notes, not a plan.** Nothing here is committed to.
Use this as a reference when sizing the next refactor, deciding
whether a given fix should be tactical or strategic, or when a
new "controller couldn't see X after Y exited" bug surfaces and
we want to ask "is this the moment?"

## The class of bugs we keep hitting

- **Bug 016** — step reporter mis-attributes times/conclusions:
  trailing per-step events were lost when the runner container
  exited. Streaming refactor reduced the symptom but didn't
  eliminate the lifecycle dependency.
- **Bug 023** — post-exit drain race: same shape, different
  symptom. Drain runs against a dead container.
- **Bug 026** — step end-event not routed: same shape again.
  `entrypoint tail` running inside the runner container gets
  SIGKILL'd when the container exits, before the trailing bytes
  of `state.jsonl` are read. The post-exit drain runs against a
  dead container and fails silently.

The common pattern: **drawbar uses the runner container as the
locus of truth** (stdout for logs, `state.jsonl` for lifecycle,
exit code for outcome). The runner container is the most fragile
thing in the system — it's the one that exits, the one that can
be OOM-killed mid-step, the one whose filesystem disappears. We
keep trying to read state from a thing that's dying.

Anything the controller wants to read reliably needs to live
somewhere that **outlives** the runner container. The current
architecture violates this and we pay a recurring bug tax.

## The architectural direction

### 1. A state-plane sidecar owns the truth

Add a second container in the same pod — call it `state-agent`.
The runner shell sends events to it (Unix socket, named pipe,
HTTP on localhost — TBD). The agent holds the canonical state
and serves it to the controller on demand. The controller
doesn't race the runner's exit because the thing it's talking
to is alive until the controller releases it.

This is a place to put other "lifecycle-sensitive" responsibilities
that today live in the runner or the controller:

- **Log streaming.** Today the controller reads logs from the
  kubelet log endpoint. Move it: the agent reads the runner's
  stdout (shared FIFO / `tee`) and emits a single ordered stream
  of `(timestamp, kind=log|state, payload)` records. One source
  of truth instead of correlating two.
- **Workflow command parsing** (`::add-mask::`, `::set-output::`,
  `::error file=...::`). Move from controller to agent. Controller
  receives structured events; doesn't need to know workflow command
  syntax.
- **Secret masking.** Belongs closest to where the secret might
  leak. Today the controller masks after the apiserver round-trip;
  move it into the agent so secrets never leave the pod unmasked.

### 2. The runner container shrinks

Once the agent exists, the entrypoint binary drops:

- Buffering and `fsync`'ing `state.jsonl` — send events to the
  agent instead.
- Owning `results.json` — agent holds it.
- Caring about graceful shutdown — agent handles drain; runner
  just exits.

The runner becomes a simpler "execute steps and report to my
supervisor" process. Closer to how Kubernetes thinks about
workloads.

### 3. The kubelet is not for control flow

Today drawbar uses `kubectl exec` (apiserver SPDY round-trip) to
drain `state.jsonl`. That's a control-plane operation, billed
against apiserver QPS, with reconnect logic for GOAWAY, and
dependent on the container being alive. None of that should be
in the hot path of "did step 4 finish?"

Alternatives, ranked by my current preference:

- **Pod-IP gRPC.** Agent exposes a gRPC server bound to the pod
  IP. Controller dials it directly (we already have networking
  to the pod for the cache server's perspective). Reconnects
  are cheap TCP; no apiserver in the path.
- **Agent dials out to the controller.** Reverse direction.
  Controller listens on its own service; agent dials when it has
  events. Removes the controller's need to know whether the agent
  is "ready."
- **Persist to PVC.** Embedded BoltDB / SQLite / event log on a
  PVC. Controller reads it even if the pod is fully gone.
  Recoverable but slower.

Pod-IP gRPC is the cleanest for a single-pod-per-job model.

### 4. Lifecycle states get explicit

Drawbar currently derives lifecycle from observations: "container
exited" → "job done"; "exit code 0" → "success". A first-class
state machine would look like:

```
queued → scheduled → running → finalizing → reported → reaped
```

The `running` state ends when the runner container exits. The
`finalizing` state covers everything between runner-exit and
controller-acknowledged-completion. Bug 026 lives entirely in
that window. Today drawbar collapses `running` and `finalizing`
into one — the runner container's exit is the only boundary we
have, and we try to do everything before it. Explicit
`finalizing` is owned by the agent.

### 5. The pod isn't the unit of work

Today one pod = one workflow job. The pod's lifecycle is the
job's lifecycle. That's why exit ordering matters so much: when
the pod ends, our window to inspect it ends.

A resilient design separates them:

- **Workflow job** is a logical entity (key: `task_id`),
  persisted by the controller, with its own state machine.
- **Pod** is the current attempt to execute it. Pods come and go;
  the workflow job persists across them.

This buys: retries on pod eviction without losing per-step state;
the ability to read partial results after pod GC; a home for
"the runner container OOMed at step 3 with this much log
captured."

Bigger architectural shift than the sidecar. Removes a whole
class of "I need to read this thing before the pod disappears"
bugs. Probably needs a small controller-side store (the same
SQLite we use for the cache server could grow a task table).

### 6. Time and ordering get explicit

Bug 016 was, deep down, a time/ordering bug — multiple producers
of "what happened with step N" and the controller had to
reconcile. With the agent emitting a single ordered event stream
timestamped by *its* monotonic clock, that reconciliation goes
away. We get a coherent log even if the runner's clock or the
controller's clock drift.

### 7. Failure modes get clearer

| failure | today | post-refactor |
|---|---|---|
| runner OOMs mid-step | exit code reads as failure, may lose state | agent emits "runner died" with last known state |
| agent OOMs | n/a | runner can keep writing locally; next controller poll detects sick pod |
| kubelet/apiserver flap | exec retries with backoff, events can be lost | agent buffers locally, sends when reconnected |
| controller restart mid-job | task is effectively lost | agent still running; new controller reconnects and picks up |

Bug 002 (better-runner-registration-recovery), bug 023, bug 026
are all instances of "we lose information when something we
depended on goes away." The agent model addresses them as a
class, not one-by-one.

## What this means in code

Rough sketch, not committed:

- `pkg/k8s/builder.go` builds a two-container pod (three counting
  the existing init shim). Both containers get the same security
  hardening; the agent gets a smaller resource footprint.
- `cmd/entrypoint/` splits into `cmd/runner-entrypoint/` (executes
  steps) and `cmd/state-agent/` (the sidecar). They share types
  via `pkg/types`, like today.
- `pkg/k8s/watcher.go` shrinks dramatically. No more "watch pod +
  drain state.jsonl + reconnect tail." The controller dials the
  agent's gRPC and gets a clean event stream until the agent says
  "I'm done, here are the final outputs."
- `pkg/reporter/` keeps its current job (talking to the forge)
  but receives clean events instead of correlating partial
  signals. `Close()`'s defensive UNSPECIFIED→CANCELLED rewrite
  (the thing that's masking bug 026's CANCELLED-for-success step)
  becomes unnecessary — by the time the agent signals "task
  done," every routed event has been applied.

## What we'd preserve

- One pod per job (easy GC, namespace isolation, k8s-native
  security model).
- Init container injecting the entrypoint binaries.
- Cache server on the controller.
- Forgejo runner protocol on the controller.
- Actions resolution (clone `uses:` actions). Same.

The split is **inside the pod**, not in the overall topology.

## Open design questions

- **Transport between runner and agent.** Unix socket on a shared
  emptyDir? Localhost HTTP? Localhost gRPC? Each has tradeoffs
  for startup ordering, error semantics, and what happens when
  the agent isn't yet ready. Picking deliberately matters; we
  inherited "file on shared volume" from the current shape and
  it's not necessarily the right answer.
- **Native sidecars vs. regular containers.** Kubernetes 1.29+
  has `restartPolicy: Always` on init containers, which is the
  right primitive (the sidecar is treated as part of pod
  readiness, not as a workload). Using them constrains the
  cluster version; not using them means manual shutdown
  coordination. Worth deciding up front.
- **What lives on the controller vs. on the agent.** Workflow
  command parsing and secret masking *can* move to the agent;
  doesn't mean they *should*. Each has reasons to stay
  controller-side (single place to update for new commands,
  shared logic across all jobs). Pick per-responsibility.
- **gRPC API surface.** What does the agent serve? A streaming
  `Events()` RPC plus a final `Drain()`? An unary `GetState()`?
  Polling vs streaming has implications for how the controller's
  reporter loop is structured.
- **Failure recovery.** If the agent dies (pod OOM, node
  reboot), what's our story? The runner can keep writing to a
  local file as fallback, but who reads it after the pod is
  gone? Maybe write-through to a PVC is the answer; maybe it's
  "accept the failure and surface it explicitly."

## What to skip when we get to bug 026

Most of this doc is "if we were rebuilding." For bug 026 today,
the smallest fix is: state-agent sidecar that runs the existing
`/shim/entrypoint tail` binary against `state.jsonl`, controller
execs into the sidecar instead of the runner. That's roughly
30 lines of `builder.go` plus a container-name swap in
`watcher.go`. Everything else above is the "if we were rebuilding
from scratch" expansion — useful as a north star, not blocking
026.

The interesting question is whether the minimum 026 fix should
be designed to grow into the larger architecture, or whether
it's a one-time patch we revisit later. Bias: the minimum fix
should at least put us on the path. Naming the sidecar
`state-agent` (not `tail` or `streamer`) signals it's the seed
of the state plane, even if today it does nothing but tail a
file.
