# drawbar — TODO

Items deferred from active work, with enough context that picking one
up cold is straightforward. Each links to the bug doc or session note
that surfaced it.

## Workflow / cache hygiene

### Share cargo cache between workflows in the same repo

Surfaced 2026-05-04 during bevy_xr_nitro eval. The repo has two
workflows (`cargo-build.yaml` and `test.yaml`) that compile the same
workspace. Today they declare different cache keys
(`bevy-xr-nitro-cargo-tilde-v1` vs `bevy-xr-nitro-test-v1`), so the
first warm run of each is cold even though the dep set is identical.

Not a drawbar bug — workflows can share keys today, the fix is on the
workflow author. Mentioning here because:

- A nice docs/README example showing "use the same key across
  workflows that build the same artifacts" would prevent the next
  user from making the same mistake.
- Drawbar could surface a friendlier log when a key collision is
  detected, e.g. `snapshot cache hit (exact)  shared_with=[<other workflows>]`,
  to make the sharing visible.

Action: add a section to the README/PRD on cache key strategy.

## Operations

### Bug 002 — runner registration recovery

See `bugs/002-better-runner-registration-recovery.md`. The shape:

- After Gitea restart / token rotation / runner record deletion, the
  pod stays Running + Ready but logs `unregistered runner` forever.
- `/readyz` does not reflect auth state; only `/healthz` would, and
  even then the existing heartbeat only catches "no recent FetchTask
  attempt", not "FetchTask returns Unauthenticated".
- Recovery is a 5-step manual dance involving admin Gitea
  credentials, kubectl-patching the registration secret, deleting
  the credentials secret, restarting the pod, and (separately) the
  RWO PVC dance from bug 011.

Open ideas in the bug doc:
- (A) `/readyz` fails when `unregistered runner` errors persist.
- (B) Auto-re-register if the deployment has a fresh registration
      token mounted; don't reuse the consumed one.
- (C) Operator/CRD pattern that mints registration tokens from a
      higher-trust principal.

Has not bitten in active workloads since 2026-04-30. Worth picking
up before the next Gitea upgrade or runner-record cleanup.
