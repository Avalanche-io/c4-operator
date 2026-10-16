#!/usr/bin/env bash
# Bring up the e2e cluster: kind with eight workers in two zones, three
# MinIO shards, the operator, and the projects the phases use. Everything
# stays on this machine: images are built locally and loaded into kind.
#
#   hack/e2e/up.sh             create or update
#   FRESH=1 hack/e2e/up.sh     recreate the cluster
source "$(dirname "$0")/lib.sh"

if [ "${FRESH:-}" = 1 ]; then
  "$KIND" delete cluster --name "$CLUSTER" >/dev/null 2>&1 || true
fi
log "cluster"
if ! "$KIND" get clusters | grep -qx "$CLUSTER"; then
  "$KIND" create cluster --name "$CLUSTER" --config "$ROOT/hack/e2e/kind.yaml" --wait 120s
fi

log "images"
make -C "$ROOT" image >/dev/null
CGO_ENABLED=0 GOOS=linux GOARCH="$ARCH" go build -trimpath -o "$ROOT/bin/linux_$ARCH/c4e2e" "$ROOT/hack/e2e/tool"
go build -o "$TOOL" "$ROOT/hack/e2e/tool"
go build -o "$OP" "$ROOT/cmd/c4-operator"
docker build -q --build-arg TARGETARCH="$ARCH" -t c4-e2e:dev -f "$ROOT/hack/e2e/Dockerfile.tool" "$ROOT" >/dev/null
for img in c4-operator:dev c4-e2e:dev minio/minio:latest minio/mc:latest registry.k8s.io/sig-storage/csi-node-driver-registrar:v2.15.0; do
  docker image inspect "$img" >/dev/null 2>&1 || docker pull -q "$img" >/dev/null
  "$KIND" load docker-image "$img" --name "$CLUSTER" >/dev/null
done

log "store: three MinIO shards"
k apply -f "$ROOT/hack/e2e/minio.yaml" >/dev/null
for i in 0 1 2; do k -n c4-store rollout status "deploy/minio-$i" --timeout=180s >/dev/null; done
for i in 0 1 2; do until curl -sf "http://127.0.0.1:3090$i/minio/health/ready" >/dev/null; do sleep 1; done; done
mc_run 'for i in 0 1 2; do for b in show-a show-b herd-tiered herd-direct mono; do mc mb --ignore-existing m$i/$b >/dev/null; done; done; echo buckets ready'

log "operator"
k apply -k "$ROOT/config/default" >/dev/null
k -n c4-system rollout restart deploy/c4-manager deploy/c4-regional ds/c4-node >/dev/null
k -n c4-system rollout status deploy/c4-manager --timeout=180s
k -n c4-system rollout status deploy/c4-regional --timeout=180s
k -n c4-system rollout status ds/c4-node --timeout=300s

# Signing needs no setup: the manager makes the farm's signing CA in the
# c4-signing namespace on first start and publishes its certificate in the
# c4-provenance ConfigMap (DESIGN.md section 9.2).

log "projects"
k -n c4-system create secret generic store-root \
  --from-literal=AWS_ACCESS_KEY_ID="$AWS_ACCESS_KEY_ID" --from-literal=AWS_SECRET_ACCESS_KEY="$AWS_SECRET_ACCESS_KEY" \
  --dry-run=client -o yaml | k apply -f - >/dev/null
for ns in show-a show-b herd-tiered herd-direct mono; do
  k create ns "$ns" --dry-run=client -o yaml | k apply -f - >/dev/null
  # User namespaces enforce Pod Security restricted: the csi volume is all
  # a pod needs.
  k label ns "$ns" pod-security.kubernetes.io/enforce=restricted --overwrite >/dev/null
done
project() { # NAME BUCKET ACCESS NAMESPACE
  k apply -f - >/dev/null <<EOF
apiVersion: c4.avalanche.io/v1alpha1
kind: Project
metadata: {name: $1}
spec:
  namespaces: [$4]
  isolation: Strict
  store:
    access: $3
    credentialsSecretRef: {name: store-root}
    shards:
$(shards_yaml "$2")
EOF
}
project show-a show-a Tiered show-a
project show-b show-b Tiered show-b
project herd-tiered herd-tiered Tiered herd-tiered
project herd-direct herd-direct Direct herd-direct
project mono mono Tiered mono
k wait --for=condition=Ready project --all --timeout=120s >/dev/null
k get projects
echo "cluster up"
