# Gitea Bug: FetchTask Loses Tasks on Network Failure (No Idempotency)

## Summary

Gitea's `FetchTask` RPC has no idempotency mechanism. If a network error occurs after the server assigns a task to a runner but before the runner receives the response, the task is permanently lost. The server believes the task is assigned; the runner never received it. The job appears stuck at "Set up job" forever.

A secondary complication makes recovery impossible: Gitea stores the task's runtime token (`ACTIONS_RUNTIME_TOKEN`) as a **one-way hash** in the database. Even if the server detected the retry, it cannot return the original token — breaking cache and artifact authentication for any re-assigned task.

Related: [#33492](https://github.com/go-gitea/gitea/issues/33492) (FetchTask not reliable under concurrent load), draft PR [#35960](https://github.com/go-gitea/gitea/pull/35960) (addresses DB performance but not idempotency).

## The Problem

### Failure Sequence

```
Runner                          Gitea Server                    Database
  |                                  |                              |
  |--- FetchTask ------------------>|                              |
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
  |--- FetchTask ------------------>|  (retry)                     |
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

### Why Recovery Is Impossible

The runtime token is stored as a one-way hash:

1. Server generates token `T` for the task
2. Server stores `hash(T)` in the database
3. Server returns `T` in the FetchTask response
4. Network drops the response
5. On retry, server cannot reconstruct `T` from `hash(T)`
6. `T` is used as `ACTIONS_RUNTIME_TOKEN` — required for cache server auth, artifact uploads, and OIDC token requests
7. Even if the server re-assigned the task, the runner would get a broken token

### Under Concurrent Load

Issue [#33492](https://github.com/go-gitea/gitea/issues/33492) documents a related problem: when multiple runners call `FetchTask` concurrently, the `UPDATE action_task SET runner_id=? WHERE status=waiting` query can return 0 rows affected due to DB transaction conflicts. The code treats this as "no task available" (`nil, false, nil`) instead of a retryable conflict. Combined with the missing idempotency, this causes the majority of queued tasks to be lost under load.

## Proposed Fix

The `act_runner` protocol already includes an `x-runner-request-key` header — a UUID sent with each `FetchTask` call. The runner retains the same key until a successful response, then rotates to a new one. Gitea currently ignores this header.

The fix requires two server-side changes:

### 1. Idempotent FetchTask via request key

The server should:

- Record the `x-runner-request-key` when assigning a task to a runner
- On receiving a `FetchTask` with a previously-seen request key for the same runner, return the already-assigned task instead of looking for new ones
- Store the key in a new `runner_request_key` column on `action_task` with a compound index on `(runner_id, runner_request_key)`

### 2. Token regeneration on recovery

Since the original token is hashed and unrecoverable, the server must generate a **fresh** runtime token when returning a recovered task. This is safe because the original token was never used (the response was lost).

## Impact

- Single-runner setups: low probability but possible on flaky networks
- Multi-runner setups: high probability under concurrent load (issue #33492)
- Any setup: a single lost task causes a permanently stuck job visible in the UI

## Reproducer

See `pkg/server/fetchtask_idempotency_test.go` — a test that demonstrates the bug:

1. Creates a mock server that simulates Gitea's current FetchTask behavior
2. Calls FetchTask to receive a task (simulating the first call succeeding server-side)
3. Calls FetchTask again with the **same** request key (simulating a retry after network failure)
4. Verifies the task is NOT returned — demonstrating the bug

A companion test (`TestFetchTask_WithIdempotency`) shows what correct behavior looks like: the server detects the duplicate request key and returns the same task with a regenerated token.
