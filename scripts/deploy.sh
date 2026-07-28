#!/bin/bash
set -euo pipefail

# ============================================
# LogHawk K8s deployment script (hardened).
# Usage: bash scripts/deploy.sh
# ============================================

NS="loghawk"
MASTER_IP="${MASTER_IP:-192.168.56.10}"
REGISTRY="${REGISTRY:-loghawk}"
IMAGE_TAG="${IMAGE_TAG:-v1.0.0}"

RED='\033[0;31m'; GREEN='\033[0;32m'; YELLOW='\033[1;33m'; NC='\033[0m'
info()  { echo -e "${GREEN}[INFO]${NC} $*"; }
warn()  { echo -e "${YELLOW}[WARN]${NC} $*"; }
error() { echo -e "${RED}[ERROR]${NC} $*" >&2; }

apply() {
  local desc="$1"; shift
  if kubectl apply -f "$@"; then
    info "  ✓ $desc"
  else
    error "  ✗ $desc failed (kubectl apply returned $?)"
    exit 1
  fi
}

apply_multi() {
  local desc="$1"; shift
  local success=0
  local failed=0
  for f in "$@"; do
    if kubectl apply -f "$f"; then
      success=$((success+1))
    else
      failed=$((failed+1))
    fi
  done
  if [ $failed -eq 0 ]; then
    info "  ✓ $desc"
  elif [ $success -eq 0 ]; then
    error "  ✗ $desc failed"
    exit 1
  else
    warn "  ! $desc: $success succeeded, $failed failed"
  fi
}

wait_for() {
  local label="$1"; shift
  local timeout="$1"; shift
  if kubectl -n "$NS" wait --for=condition=ready pod -l "$label" --timeout="$timeout"; then
    info "  ✓ $label ready"
  else
    warn "  ! $label not ready after ${timeout}; continuing (check: kubectl -n $NS get pods -l $label)"
  fi
}

info "🚀 LogHawk K8s deployment starting"

# ---- Step 1: check prerequisites ----
for cmd in kubectl ctr; do
  if ! command -v "$cmd" >/dev/null 2>&1; then
    error "missing required command: $cmd"
    exit 1
  fi
done

RUNTIME=""
if command -v docker >/dev/null 2>&1; then
  RUNTIME="docker"
elif command -v nerdctl >/dev/null 2>&1; then
  RUNTIME="nerdctl"
else
  warn "no docker/nerdctl found; skipping image build/import (images must already exist)"
fi

SERVICES="ingest ai-proxy alerter chaos log-collector"

# ---- Step 2: build Go services (host) ----
if [ -n "$RUNTIME" ]; then
  info "📦 Building Go services..."
  for svc in $SERVICES; do
    if [ -f "services/$svc/go.mod" ]; then
      info "  → building $svc"
      ( cd "services/$svc" && CGO_ENABLED=0 go build -ldflags="-s -w" -o "$svc" . ) \
        || { error "build failed: $svc"; exit 1; }
    fi
  done
fi

# ---- Step 3: build & import Docker images ----
if [ -n "$RUNTIME" ]; then
  info "🐳 Building Docker images..."
  for svc in $SERVICES frontend; do
    info "  → $svc"
    if ! "$RUNTIME" build -t "$REGISTRY/$svc:$IMAGE_TAG" "services/$svc"; then
      error "docker build failed: $svc"; exit 1
    fi
  done

  info "📥 Importing images into containerd (k8s.io namespace)..."
  for svc in $SERVICES frontend; do
    if ! "$RUNTIME" save "$REGISTRY/$svc:$IMAGE_TAG" -o "/tmp/loghawk-$svc.tar"; then
      error "save failed: $svc"; exit 1
    fi
    if ! ctr -n k8s.io images import "/tmp/loghawk-$svc.tar"; then
      error "import failed: $svc"; exit 1
    fi
    info "  ✓ $svc"
  done

  if [ "$REGISTRY" != "loghawk" ]; then
    info "🏷️  Pushing to registry $REGISTRY..."
    for svc in $SERVICES frontend; do
      if ! "$RUNTIME" push "$REGISTRY/$svc:$IMAGE_TAG"; then
        warn "  ! push failed: $svc (offline? images already imported locally)"
      fi
    done
  fi

  info "📡 Distributing to worker nodes..."
  for worker in k8s-worker1 k8s-worker2; do
    info "  → $worker"
    ping -c 1 -W 2 "$worker" >/dev/null 2>&1 || { warn "  ! $worker unreachable"; continue; }
    for svc in $SERVICES frontend; do
      if scp "/tmp/loghawk-$svc.tar" "root@$worker:/tmp/"; then
        ssh "root@$worker" "ctr -n k8s.io images import /tmp/loghawk-$svc.tar" \
          || warn "  ! import on $worker failed: $svc"
      else
        warn "  ! scp to $worker failed: $svc"
      fi
    done
  done
fi

# ---- Step 4: apply K8s resources ----
info "☸️  Applying K8s resources..."

apply "Namespace"                k8s/namespace.yaml
apply "PriorityClass"            k8s/priorityclass.yaml
apply "ConfigMap"                k8s/configmap.yaml
apply "Secret"                   k8s/secret.yaml
apply "Registry secret"          k8s/registry-secret.yaml

apply "PostgreSQL"               k8s/postgres.yaml
apply "RabbitMQ"                 k8s/rabbitmq.yaml
apply "Redis"                    k8s/redis.yaml

info "⏳ Waiting for middleware..."
wait_for "app=postgres"  120s
wait_for "app=rabbitmq"  120s
wait_for "app=redis"     60s

apply "Ingest"                   k8s/ingest.yaml
apply "AI Proxy"                 k8s/ai-proxy.yaml
apply "Alerter"                  k8s/alerter.yaml
apply "Chaos"                    k8s/chaos.yaml
apply "Log Collector (DaemonSet)" k8s/log-collector.yaml
apply "Frontend"                 k8s/frontend.yaml

apply "NetworkPolicy"            k8s/networkpolicy.yaml
apply "ResourceQuota"            k8s/resourcequota.yaml
apply "Ingress"                  k8s/ingress.yaml
apply "Prometheus"               k8s/prometheus.yaml
apply "Grafana"                  k8s/grafana.yaml

# ---- Step 5: ensure image tags match deployment manifests ----
info "🏷️  Pinning images to $REGISTRY/$IMAGE_TAG..."
kubectl -n "$NS" set image deployment/ingest        ingest="$REGISTRY/ingest:$IMAGE_TAG"
kubectl -n "$NS" set image deployment/ai-proxy      ai-proxy="$REGISTRY/ai-proxy:$IMAGE_TAG"
kubectl -n "$NS" set image deployment/alerter       alerter="$REGISTRY/alerter:$IMAGE_TAG"
kubectl -n "$NS" set image deployment/chaos         chaos="$REGISTRY/chaos:$IMAGE_TAG"
kubectl -n "$NS" set image deployment/frontend      frontend="$REGISTRY/frontend:$IMAGE_TAG"
kubectl -n "$NS" set image daemonset/log-collector  collector="$REGISTRY/log-collector:$IMAGE_TAG"
info "⏳ Waiting for rollout..."
for d in ingest ai-proxy alerter chaos frontend; do
  kubectl -n "$NS" rollout status deployment/"$d" --timeout=120s \
    || warn "rollout status check skipped for $d"
done

# ---- Step 6: summary ----
info "⏳ Waiting for pods to settle (5s)..."
sleep 5

echo ""
info "✅ LogHawk deployment complete (tag: $IMAGE_TAG)"
echo ""
echo "  Frontend:  http://${MASTER_IP}:30080"
echo "  Grafana:   http://${MASTER_IP}:30300 (admin/<CHANGE_ME_GRAFANA_PASSWORD>)"
echo ""
echo "  Rollback:  kubectl -n $NS rollout undo deployment/<name>"
echo "  Status:    kubectl -n $NS rollout status deployment/<name>"
echo ""
kubectl -n "$NS" get pods -o wide
