#!/usr/bin/env bash
# Rebuild the images, load them into the running e2e cluster, apply the
# manifests and restart the operator's components.
source "$(dirname "$0")/lib.sh"

make -C "$ROOT" image >/dev/null 2>&1
CGO_ENABLED=0 GOOS=linux GOARCH="$ARCH" go build -trimpath -o "$ROOT/bin/linux_$ARCH/c4e2e" "$ROOT/hack/e2e/tool"
go build -o "$TOOL" "$ROOT/hack/e2e/tool"
go build -o "$OP" "$ROOT/cmd/c4-operator"
docker build -q --build-arg TARGETARCH="$ARCH" -t c4-e2e:dev -f "$ROOT/hack/e2e/Dockerfile.tool" "$ROOT" >/dev/null
for img in c4-operator:dev c4-e2e:dev; do
  "$KIND" load docker-image "$img" --name "$CLUSTER" >/dev/null 2>&1
done
k apply -k "$ROOT/config/default" >/dev/null 2>&1
k -n c4-system rollout restart deploy/c4-manager deploy/c4-regional ds/c4-node >/dev/null
k -n c4-system rollout status deploy/c4-manager --timeout=180s >/dev/null
k -n c4-system rollout status deploy/c4-regional --timeout=180s >/dev/null
k -n c4-system rollout status ds/c4-node --timeout=300s >/dev/null
echo redeployed
