# Future Work

## Per-Job Cache Isolation (Multi-Tenant Security)

**Problem**: The HTTP cache server (`actions/cache` protocol) is currently open — any job pod that knows the URL can read/write any repo's cache entries. In a single-tenant setup (one org, trusted repos) this is fine. In a multi-tenant setup (untrusted repos sharing a runner), a malicious workflow in repo A could poison repo B's cache.

**Solution**: Add a reverse proxy between job pods and the cache server that enforces per-repo isolation via HMAC authentication.

### How It Would Work

1. Runner generates a random HMAC secret on startup, stored in the k8s credentials Secret
2. When a job starts, the controller creates a signed token: `HMAC(secret, repo + runNumber + timestamp)`
3. Job gets a scoped cache URL: `http://cache:9300/{runID}/_apis/artifactcache/...`
4. The proxy intercepts each request, looks up the run ID, injects repo identity headers, and validates the HMAC
5. The cache server scopes all operations to that repo's namespace

### What It Prevents

- Job in repo A cannot read or write repo B's cache entries
- Expired/replayed requests rejected via timestamp validation
- PR branches get write isolation (can read main's cache, writes scoped to the PR ref)

### Implementation Notes

- ~200 lines: HTTP reverse proxy + HMAC validation + run registry
- Must be clean-room (the Forgejo implementation is GPLv3, we're MIT)
- The proxy is a `net/http/httputil.ReverseProxy` with a `Rewrite` func that injects auth headers
- Run data stored in a `sync.Map` keyed by random run ID

### When to Prioritize

Only needed when running untrusted workflows from external contributors. Not needed for private repos or single-org deployments.

---

## Kubernetes User Namespaces for Job Pods

**Problem**: CI job pods currently run without `RunAsNonRoot` because popular images like `node:22-bookworm` run as root. While we drop all capabilities and disallow privilege escalation, the container still has UID 0 inside.

**Idea**: Use Kubernetes User Namespaces (`hostUsers: false` on the pod spec) to map container root (UID 0) to an unprivileged UID on the host.

### Prerequisites

- Kubernetes 1.30+ (User Namespaces GA)
- Linux kernel 6.3+ on nodes
- containerd 1.7+

### Implementation

One-line change: set `hostUsers: false` in the job pod's `PodSecurityContext`. Make configurable via Helm values since not all clusters support it.

---

## Action Cache Expiration

The controller caches cloned action repos on the PVC at `/cache/actions-repo-cache/`. Currently cached forever.

- **Problem**: Actions pinned to branches (`@main`) serve stale code forever
- **Solution**: TTL-based expiration (default: 7 days). Check clone age on each LoadAction call, re-clone if stale. Tags (`@v4`) could cache longer since they're immutable.
- **Manual invalidation**: CLI command or API endpoint to flush the action cache

---

## Recreate Deployment Strategy

**Problem**: The cache server uses BoltDB which takes an exclusive file lock. During rolling updates, the new pod can't open the database until the old pod fully terminates, causing "Open(bolt.db): timeout" crashes.

**Solution**: Use `strategy: Recreate` in the Helm deployment template instead of `RollingUpdate`. This kills the old pod before starting the new one, avoiding the lock conflict. Trade-off: brief downtime during updates (acceptable for a CI runner).

Alternative: retry BoltDB open with backoff instead of failing immediately.

---

## Standalone Cache Server

act_runner has a `cache-server` subcommand that runs the HTTP cache as a standalone process. Useful for multiple runner replicas sharing one cache. Currently each drawbar instance runs its own embedded cache server.

Low priority — BoltDB's exclusive lock means you can't share the cache PVC between replicas anyway. Would need a different storage backend (SQLite WAL, or S3-compatible) to make this useful.
