#!/usr/bin/env bash
# Rebuild the images, load them into the running e2e cluster, apply the
# manifests and restart the operator's components.
source "$(dirname "$0")/lib.sh"

# quiet runs a step without its chatter, and on failure shows what it said
# and stops: under set -e a step sent to /dev/null would end the script
# with no word of why.
quiet() {
  local out
  if ! out=$("$@" 2>&1); then
    echo "redeploy: failed: $*" >&2
    echo "$out" >&2
    exit 1
  fi
}

quiet make -C "$ROOT" image
CGO_ENABLED=0 GOOS=linux GOARCH="$ARCH" go build -trimpath -o "$ROOT/bin/linux_$ARCH/c4e2e" "$ROOT/hack/e2e/tool"
go build -o "$TOOL" "$ROOT/hack/e2e/tool"
go build -o "$OP" "$ROOT/cmd/c4-operator"
quiet docker build -q --build-arg TARGETARCH="$ARCH" -t c4-e2e:dev -f "$ROOT/hack/e2e/Dockerfile.tool" "$ROOT"
for img in c4-operator:dev c4-e2e:dev; do
  quiet "$KIND" load docker-image "$img" --name "$CLUSTER"
done
# The CRDs first, established before anything that uses them; then the
# rest, once more if the first pass meets a webhook or CRD not ready yet.
quiet k apply -k "$ROOT/config/crd"
quiet k wait --for condition=established --timeout=60s crd --all
if ! out=$(k apply -k "$ROOT/config/default" 2>&1); then
  echo "redeploy: apply failed once, retrying in 5 s:" >&2
  echo "$out" >&2
  sleep 5
  quiet k apply -k "$ROOT/config/default"
fi
quiet k -n c4-system rollout restart deploy/c4-manager deploy/c4-regional deploy/c4-egress ds/c4-node
quiet k -n c4-system rollout status deploy/c4-manager --timeout=180s
quiet k -n c4-system rollout status deploy/c4-regional --timeout=180s
quiet k -n c4-system rollout status deploy/c4-egress --timeout=180s
quiet k -n c4-system rollout status ds/c4-node --timeout=300s
echo redeployed
