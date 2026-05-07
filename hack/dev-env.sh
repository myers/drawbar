#!/usr/bin/env bash
# Dev environment: k3d cluster + Gitea/Forgejo + runner
#
# Usage:
#   ./hack/dev-env.sh up       # Create cluster, build image, deploy everything
#   ./hack/dev-env.sh down     # Tear down cluster
#   ./hack/dev-env.sh rebuild  # Rebuild + redeploy runner only (fast iteration)
#   ./hack/dev-env.sh status   # Show pod status
#   ./hack/dev-env.sh logs     # Tail runner logs
#   ./hack/dev-env.sh token    # Print registration token
#
# Environment:
#   SERVER=gitea|forgejo  # Which forge to deploy (default: gitea)
#
set -euo pipefail

# --- Server selection ---

SERVER="${SERVER:-gitea}"

case "$SERVER" in
  gitea)
    SERVER_NS="gitea"
    SERVER_LABEL="app=gitea"
    SERVER_MANIFEST="deploy/gitea-test.yaml"
    SERVER_CLI="gitea"
    ;;
  forgejo)
    SERVER_NS="forgejo"
    SERVER_LABEL="app=forgejo"
    SERVER_MANIFEST="deploy/forgejo-test.yaml"
    SERVER_CLI="forgejo"
    ;;
  *)
    echo "ERROR: SERVER must be 'gitea' or 'forgejo' (got: $SERVER)"
    exit 1
    ;;
esac

CLUSTER_NAME="runner-dev"
REGISTRY_NAME="k3d-registry"
REGISTRY_PORT="5111"
IMAGE="localhost:${REGISTRY_PORT}/drawbar"
VERSION="dev"
RUNNER_NS="drawbar"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_DIR="$(dirname "$SCRIPT_DIR")"

cd "$PROJECT_DIR"

# --- Helpers ---

log() { echo "==> $*"; }

wait_for_pod() {
  local ns="$1" label="$2" timeout="${3:-120}"
  log "Waiting for pod $label in $ns (${timeout}s timeout)..."
  kubectl wait --for=condition=ready pod -l "$label" -n "$ns" --timeout="${timeout}s"
}

wait_for_url() {
  local url="$1" timeout="${2:-60}"
  log "Waiting for $url..."
  for i in $(seq 1 "$timeout"); do
    if curl -sf "$url" >/dev/null 2>&1; then
      return 0
    fi
    sleep 1
  done
  echo "ERROR: $url not reachable after ${timeout}s"
  return 1
}

# --- Commands ---

cmd_up() {
  log "Creating k3d cluster '$CLUSTER_NAME' with registry..."
  k3d registry create "$REGISTRY_NAME" --port "$REGISTRY_PORT" 2>/dev/null || true
  # Build k3d create args.
  local k3d_args=(
    --registry-use "k3d-${REGISTRY_NAME}:${REGISTRY_PORT}"
    --api-port 6550
    -p "${SERVER_PORT:-8090}:80@loadbalancer"
    --wait
  )

  # If a MITM proxy CA cert exists, mount it into the k3s node so containerd
  # can pull images through the proxy.
  MITM_CERT="/usr/local/share/ca-certificates/mitmproxy-ca-cert.crt"
  if [ -f "$MITM_CERT" ]; then
    log "MITM proxy cert detected, injecting into k3s node..."
    k3d_args+=(--volume "${MITM_CERT}:/etc/ssl/certs/mitmproxy-ca-cert.crt")
  fi

  k3d cluster create "$CLUSTER_NAME" "${k3d_args[@]}"

  log "Building runner image..."
  make image IMAGE="$IMAGE" VERSION="$VERSION"
  docker push "$IMAGE:$VERSION"

  log "Deploying $SERVER..."
  kubectl create namespace "$SERVER_NS" --dry-run=client -o yaml | kubectl apply -f -
  kubectl apply -f "$SERVER_MANIFEST"
  wait_for_pod "$SERVER_NS" "$SERVER_LABEL" 120

  SERVER_POD=$(kubectl get pod -n "$SERVER_NS" -l "$SERVER_LABEL" -o jsonpath='{.items[0].metadata.name}')
  SERVER_URL="http://localhost:${SERVER_PORT:-8090}"
  ADMIN_USER="devadmin"
  ADMIN_PASS="admin123"

  log "Creating admin user..."
  kubectl exec -n "$SERVER_NS" "$SERVER_POD" -- \
    su -s /bin/sh git -c \
    "$SERVER_CLI admin user create --username $ADMIN_USER --password $ADMIN_PASS --email ${ADMIN_USER}@localhost --admin --must-change-password=false" \
    2>/dev/null || true

  log "Creating registration token via API..."
  wait_for_url "${SERVER_URL}/api/v1/version" 30
  REG_TOKEN=$(curl -sf "${SERVER_URL}/api/v1/admin/runners/registration-token" \
    -u "${ADMIN_USER}:${ADMIN_PASS}" | jq -r '.token' 2>/dev/null || echo "")

  if [ -z "$REG_TOKEN" ]; then
    echo "ERROR: Failed to get registration token."
    echo "  Try: curl -u ${ADMIN_USER}:${ADMIN_PASS} ${SERVER_URL}/api/v1/admin/runners/registration-token"
    return 1
  fi

  log "Registration token: $REG_TOKEN"

  log "Deploying runner..."
  kubectl create namespace "$RUNNER_NS" --dry-run=client -o yaml | kubectl apply -f -
  helm upgrade --install runner ./deploy/helm/drawbar \
    --namespace "$RUNNER_NS" \
    --set image.repository="k3d-${REGISTRY_NAME}:${REGISTRY_PORT}/drawbar" \
    --set image.tag="$VERSION" \
    --set server.url="http://${SERVER_NS}.${SERVER_NS}.svc:80" \
    --set server.registrationToken="$REG_TOKEN" \
    --set runner.gitCloneUrl="http://${SERVER_NS}.${SERVER_NS}.svc:80" \
    --set runner.timeout="10m" \
    --set-json 'runner.labels=["ubuntu-latest:docker://node:24-trixie"]' \
    --set cache.enabled=true \
    --wait --timeout 120s

  log "Done! Dev environment is ready."
  echo ""
  echo "  ${SERVER^} UI:  http://localhost:${SERVER_PORT:-8090}  (devadmin / admin123)"
  echo "  Runner logs: ./hack/dev-env.sh logs"
  echo "  Tear down:   ./hack/dev-env.sh down"
  echo ""
  cmd_status
}

cmd_down() {
  log "Tearing down cluster '$CLUSTER_NAME'..."
  k3d cluster delete "$CLUSTER_NAME" 2>/dev/null || true
  k3d registry delete "k3d-${REGISTRY_NAME}" 2>/dev/null || true
  log "Done."
}

cmd_rebuild() {
  log "Rebuilding runner image..."
  make image IMAGE="$IMAGE" VERSION="$VERSION"
  docker push "$IMAGE:$VERSION"

  log "Restarting runner deployment..."
  kubectl rollout restart deployment -n "$RUNNER_NS" -l app.kubernetes.io/name=drawbar
  kubectl rollout status deployment -n "$RUNNER_NS" -l app.kubernetes.io/name=drawbar --timeout=60s

  log "Done."
}

cmd_status() {
  echo "--- ${SERVER^} ---"
  kubectl get pods -n "$SERVER_NS" -o wide 2>/dev/null || echo "(not deployed)"
  echo ""
  echo "--- Runner ---"
  kubectl get pods -n "$RUNNER_NS" -o wide 2>/dev/null || echo "(not deployed)"
  echo ""
  echo "--- Jobs ---"
  kubectl get jobs -n "$RUNNER_NS" -o wide 2>/dev/null || echo "(none)"
}

cmd_logs() {
  kubectl logs -n "$RUNNER_NS" -l app.kubernetes.io/name=drawbar -f --tail=100
}

cmd_token() {
  SERVER_POD=$(kubectl get pod -n "$SERVER_NS" -l "$SERVER_LABEL" -o jsonpath='{.items[0].metadata.name}')
  case "$SERVER" in
    gitea)
      kubectl exec -n "$SERVER_NS" "$SERVER_POD" -- \
        su -s /bin/sh git -c 'gitea actions generate-runner-token --scope ""'
      ;;
    forgejo)
      kubectl exec -n "$SERVER_NS" "$SERVER_POD" -- \
        su -s /bin/sh git -c 'forgejo admin runner create-runner-token --scope ""'
      ;;
  esac
}

# --- Main ---

case "${1:-help}" in
  up)      cmd_up ;;
  down)    cmd_down ;;
  rebuild) cmd_rebuild ;;
  status)  cmd_status ;;
  logs)    cmd_logs ;;
  token)   cmd_token ;;
  *)
    echo "Usage: $0 {up|down|rebuild|status|logs|token}"
    echo ""
    echo "Environment:"
    echo "  SERVER=gitea|forgejo  (default: gitea)"
    exit 1
    ;;
esac
