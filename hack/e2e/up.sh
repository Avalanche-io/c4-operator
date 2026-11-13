#!/usr/bin/env bash
# Bring up the e2e cluster: kind with eight workers in two zones, three
# Garage shards, the operator, and the projects the phases use. Everything
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
  # the host ports, moved by E2E_PORT_OFFSET (lib.sh)
  cfg=$(mktemp)
  awk -v po="$PO" '{ if (match($0, /hostPort: [0-9]+/)) { p = substr($0, RSTART + 10, RLENGTH - 10); sub(/hostPort: [0-9]+/, "hostPort: " (p + po)) } print }' \
    "$ROOT/hack/e2e/kind.yaml" >"$cfg"
  "$KIND" create cluster --name "$CLUSTER" --config "$cfg" --wait 120s
  rm -f "$cfg"
fi

log "images"
make -C "$ROOT" image >/dev/null
CGO_ENABLED=0 GOOS=linux GOARCH="$ARCH" go build -trimpath -o "$ROOT/bin/linux_$ARCH/c4e2e" "$ROOT/hack/e2e/tool"
go build -o "$TOOL" "$ROOT/hack/e2e/tool"
go build -o "$OP" "$ROOT/cmd/c4-operator"
docker build -q --build-arg TARGETARCH="$ARCH" -t c4-e2e:dev -f "$ROOT/hack/e2e/Dockerfile.tool" "$ROOT" >/dev/null
# pulled IMAGE: the image here, as one platform. A pulled image can be a
# multi-platform index naming layers of other platforms it never fetched,
# which `kind load` refuses ("content digest ... not found", Docker
# Desktop's containerd store); built FROM itself it is this machine's
# platform alone.
pulled() {
  docker image inspect "$1" >/dev/null 2>&1 || docker pull -q "$1" >/dev/null
  echo "FROM $1" | docker build -q -t "$1" - >/dev/null
}
for img in "$GARAGE_IMAGE" registry.k8s.io/sig-storage/csi-node-driver-registrar:v2.15.0; do pulled "$img"; done
for img in c4-operator:dev c4-e2e:dev "$GARAGE_IMAGE" registry.k8s.io/sig-storage/csi-node-driver-registrar:v2.15.0; do
  "$KIND" load docker-image "$img" --name "$CLUSTER" >/dev/null
done

log "store: three Garage shards"
garage_up garage-0 garage-1 garage-2
for i in 0 1 2; do
  for b in show-a show-b herd-tiered herd-direct mono; do garage_bucket "garage-$i" "$b"; done
done
echo "buckets ready"

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
