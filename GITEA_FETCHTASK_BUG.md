# Gitea Bug: FetchTask Loses Tasks on Network Failure (No Idempotency)

## Summary

Gitea's `FetchTask` RPC has no idempotency mechanism. If a network error occurs after the server assigns a task to a runner but before the runner receives the response, the task is permanently lost. The server believes the task is assigned; the runner never received it. The job appears stuck at "Set up job" forever.

A secondary complication makes recovery impossible: Gitea stores the task's runtime token (`ACTIONS_RUNTIME_TOKEN`) as a **one-way hash** in the database. Even if the server could detect the retry, it cannot return the original token — making re-assignment break cache and artifact authentication.

Forgejo fixed both problems in [commit `0ae6235386`](https://codeberg.org/forgejo/forgejo/commit/0ae6235386) (PR [#11401](https://codeberg.org/forgejo/forgejo/pulls/11401)). Gitea has not adopted this fix.

## The Problem

### Failure Sequence

```
Runner                          Gitea Server                    Database
  |                                  |                              |
  |--- FetchTask(request_key=A) ---->|                              |
  |                                  |--- UPDATE action_task        |
  |                                  |    SET runner_id=R,          |
  |                                  |        status=running        |
  |                                  |<-- OK (1 row affected) ------|
  |                                  |                              |
  |                                  |--- Generate runtime token ---|
  |                                  |--- Hash token, store hash ---|
  |                                  |                              |
  |        *** NETWORK ERROR ***     |                              |
  |<--X-- Response lost -------------|                              |
  |                                  |                              |
  |--- FetchTask(request_key=A) ---->|  (retry with same key)      |
  |                                  |                              |
  |                                  |--- SELECT available tasks ---|
  |                                  |<-- No available tasks -------|
  |                                  |    (task already assigned)   |
  |                                  |                              |
  |<-- "no task available" ----------|                              |
  |                                  |                              |
  |  Runner moves on.               |  Task stuck forever.         |
  |  Job shows "Set up job" in UI.  |  Token hash in DB, original  |
  |                                  |  token value is gone.        |
```

### Why Recovery Is Impossible Without the Fix

Even if Gitea added retry detection, the runtime token is stored as a one-way hash:

1. Server generates token `T` for the task
2. Server stores `hash(T)` in the database
3. Server returns `T` in the FetchTask response
4. Network drops the response
5. On retry, server cannot reconstruct `T` from `hash(T)`
6. The token `T` is used as `ACTIONS_RUNTIME_TOKEN` — required for cache server auth, artifact uploads, and OIDC token requests
7. Even if the server re-assigned the task, the runner would get a broken token

### Under Concurrent Load

Gitea issue [#33492](https://github.com/go-gitea/gitea/issues/33492) documents a related problem: when multiple runners call `FetchTask` concurrently, the `UPDATE action_task SET runner_id=? WHERE status=waiting` query can return 0 rows affected due to DB transaction conflicts. The code treats this as "no task available" (`nil, false, nil`) instead of a retryable conflict. Combined with the missing idempotency, this causes the majority of queued tasks to be lost under load.

## Forgejo's Fix

Forgejo commit [`0ae6235386`](https://codeberg.org/forgejo/forgejo/commit/0ae6235386) (Feb 2026) implements:

1. **`x-runner-request-key` header**: The runner generates a UUID for each FetchTask call and retains it until a successful response. On retry, the same UUID is sent.

2. **Server-side recovery**: On receiving a FetchTask with a known request key, the server returns the previously-assigned task(s) instead of looking for new ones.

3. **Token regeneration**: Since the original token is hashed, the server generates a **fresh** runtime token when recovering a task. This is safe because the original token was never used (the response was lost).

4. **Database schema**: New `runner_request_key` column on `action_task` with a compound index on `(runner_id, runner_request_key)`.

5. **`RecoverTasks()` function**: `services/actions/task.go` — looks up tasks by runner ID + request key, regenerates tokens, returns them.

### Files Changed (Forgejo)

- `models/actions/task.go` — `RunnerRequestKey` field, `GetTasksByRunnerRequestKey()` query
- `models/forgejo_migrations/v15b_add-runner_request_key.go` — migration
- `routers/api/actions/runner/interceptor.go` — extracts `x-runner-request-key` from header
- `routers/api/actions/runner/runner.go` — calls `RecoverTasks()` before assigning new tasks
- `services/actions/task.go` — `RecoverTasks()` with token regeneration

## Gitea's Current Status

- **Issue [#33492](https://github.com/go-gitea/gitea/issues/33492)** (open): Reports FetchTask unreliability under concurrent load. No mention of Forgejo's idempotency fix or the token hashing problem.
- **Draft PR [#35960](https://github.com/go-gitea/gitea/pull/35960)**: Addresses DB performance (pagination, jitter, reduced writes) but does NOT implement request-key idempotency or token regeneration.
- No Gitea issue specifically about the one-way hash preventing task recovery.

## Drawbar's Client-Side Implementation

Drawbar already implements the runner side of the fix:

- `pkg/server/client.go:25` — `RequestKeyHeader = "x-runner-request-key"`
- `pkg/server/client.go:65-70` — interceptor attaches the key to every RPC call
- `pkg/server/poller.go:58` — generates a UUID before polling starts
- `pkg/server/poller.go:90` — sets the key before each FetchTask call
- `pkg/server/poller.go:111` — on error, retains the same key (idempotent retry)
- `pkg/server/poller.go:117` — on success, rotates to a new key

**With Forgejo**: Full idempotency — lost tasks are recovered on retry.
**With Gitea**: The header is silently ignored. Lost tasks remain stuck.

## Impact

- Single-runner setups: Low probability but still possible on flaky networks
- Multi-runner setups: High probability under concurrent load (issue #33492)
- Any setup: A single lost task causes a permanently stuck job visible in the UI

## Recommendation

Port Forgejo's fix to Gitea. The implementation is ~340 lines across 9 files, with integration tests. The client-side protocol (the `x-runner-request-key` header) is already implemented in both drawbar and the Forgejo runner, so no runner changes are needed — only server-side.
