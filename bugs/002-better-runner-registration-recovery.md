# Runner registration recovery is awkward

## Summary

When the drawbar runner's saved credentials become invalid (server-side runner record deleted, server restarted in a way that broke session state, token rotated), the runner cannot self-recover: the `SERVER_REGISTRATION_TOKEN` env var is one-shot and the operator has to know that the recovery sequence is "mint a new registration token, kubectl-patch the secret, kubectl-delete the credentials secret, restart the pod."

This is fragile: the registration token is committed to git as a SealedSecret (in fluxing's case, `apps/base/gitea/drawbar/registration-secret.yaml`), but registration tokens are *one-time-use*. After a successful registration, the secret holds a token that's already been consumed. Recovery requires manual intervention every time.

## Symptom

After a Gitea restart on 2026-04-30, drawbar logged:

```
ERROR fetch task failed error="unknown: rpc error: code = Unauthenticated desc = unregistered runner"
WARN  backing off duration=60s
```

…repeated for an hour with no other log lines, no health-check failures, no clear next step for the operator.

The runner pod stayed `Running` and `Ready` (the `/readyz` handler reports ready as long as the runner *registered at boot*, regardless of whether it's currently authenticated to the server). So Kubernetes-level monitoring did not flag anything wrong.

## Why this is bad

Three failure modes converge:

1. **No external indicator of "runner is broken."** The pod is healthy by k8s metrics; the only signal is the log line, which is not surfaced anywhere.
2. **Recovery requires admin Gitea credentials** to mint a new registration token. The drawbar runner itself doesn't have these. So the runner can't self-heal even if it detected the problem.
3. **Recovery sequence is undocumented.** An operator coming back to a broken cluster has to:
   1. Mint a new registration token via `POST /api/v1/admin/actions/runners/registration-token` (admin scope).
   2. `kubectl patch secret drawbar-registration ... --type=merge -p '{"data":{"token":"<base64>"}}'` — bypasses the SealedSecret since re-sealing per recovery is impractical.
   3. `kubectl delete secret drawbar-credentials` — force re-registration on next start.
   4. `kubectl rollout restart deployment/drawbar` — bounce the pod.
   5. Wait for FailedMount on the new pod due to the cache PVC standoff (separate bug); manually delete the old pod.

There is no documentation for any of this in the README or PRD.

## Ideas

### A. `/readyz` should fail when the runner is unauthenticated

Right now `/readyz` returns ready iff `registered.Store(true)` was called at startup (`cmd/controller/main.go:273-275`). Change the semantic: `/readyz` should reflect the *current* poll state. If the last N task fetches have all returned `unregistered runner`, mark unready.

This single change would surface the problem to k8s-level alerting and let kustomization/PrometheusRule observers fire before someone notices via CI lag.

### B. Self-heal on `unregistered runner` if a registration token is available

When the poller gets `Unauthenticated: unregistered runner`, drawbar could:

1. Discard the in-memory client.
2. Wipe `drawbar-credentials` (the SecretStore).
3. Re-call `EnsureRegistered` with the `RegistrationToken` env var.
4. If the env-var token also fails (already-used), surface a clear error.

This requires the operator to refresh `drawbar-registration` *before* restart, which is still manual — but it removes the explicit secret-delete + rollout dance, and the operator only needs one kubectl command.

### C. "Self-service" registration via admin API

If drawbar were configured with an admin token (separate scope, separate secret), it could mint its own registration tokens on demand. This couples drawbar to admin-level access, which is a non-trivial security tradeoff — but it would make the runner fully self-healing.

Probably not the right answer for the same reason GitHub Actions self-hosted runners don't have it: principle of least privilege.

### D. Document the recovery sequence in `BUGS.md` or `README.md`

Lowest-effort fix. Even if the code stays as is, an operator with the documented sequence resolves it in 5 minutes instead of 30.

Suggested doc location: a new section in `README.md` titled "Recovery: runner is unregistered after server restart" with the exact kubectl commands, including how to mint a new registration token via the Gitea API (or note that `gt runner registration-token` would do it once that exists — see `~/p/gt/bugs/002`).

### E. Move the registration token out of GitOps entirely

Stop storing the registration token as a SealedSecret. Treat it as out-of-band ops state. The deployment YAML still references `drawbar-registration` from a secret reference, but the secret is created/updated by an out-of-band process (the recovery script, an `inv` task, or a `gt runner setup` command if one is built).

The fluxing Kustomization would no longer manage `drawbar-registration`. The current pattern of "commit a SealedSecret with a token that's stale the moment it's used" is honestly worse than no GitOps for that one secret.

## Recommendation order

1. **D (document)** — costs nothing, immediately useful.
2. **A (readyz reflects auth state)** — small change, big operational win.
3. **B (self-heal on unauth)** — modest change, fully automates the in-band part of recovery.
4. **E (move reg token out of GitOps)** — orthogonal cleanup; do it when convenient.
5. **C (self-service via admin token)** — explicit non-goal unless we change our minds about the security tradeoff.

## Acceptance

For (A): a runner with bad credentials marks `/readyz` failed; k8s reports the pod NotReady; the standard "deployment unhealthy" alerting fires.

For (B): a runner that gets `unregistered runner` and has a fresh `SERVER_REGISTRATION_TOKEN` re-registers automatically and resumes work; no manual `kubectl delete secret` needed.

For (D): a concrete recovery section in README that another operator (or a future agent) can follow without inventing the sequence.
