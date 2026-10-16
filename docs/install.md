# Installing c4-operator

c4-operator is alpha software (v0.1). The API group is `c4.avalanche.io/v1alpha1`
and will change before 1.0. To try it on a laptop, follow the
[quickstart](quickstart.md); this page is for a cluster of your own.

## Requirements

- **Kubernetes.** Tested on 1.36 only (kind v0.33). The operator relies on
  features that are on by default from 1.32: ValidatingAdmissionPolicy, CSI
  inline ephemeral volumes, and bound service-account tokens that name their
  node.
- **Linux nodes with overlayfs.** The node daemon (a DaemonSet) runs
  privileged with host networking: it is the CSI node plugin, mounts the
  overlay views and owns the node's object cache under `/var/lib/c4`. Pods
  that use views need nothing beyond Pod Security `restricted`.
- **A zone label on every node that runs pods with views.** The node daemon
  runs where `topology.kubernetes.io/zone` is set, and nodes in the same zone
  serve each other's cache misses.
- **A store per project.** S3-compatible object storage with path-style
  addressing (tested with MinIO) or c4d, as one or more shards. Credentials
  are a Secret in `c4-system`.

## Install a release

Each release attaches one manifest with the CRDs, the operator and its
RBAC, pointing at the release's image on GHCR:

```sh
kubectl apply -f https://github.com/Avalanche-io/c4-operator/releases/download/v0.1.0/install.yaml
kubectl -n c4-system rollout status deploy/c4-manager
kubectl -n c4-system rollout status ds/c4-node
kubectl -n c4-system rollout status deploy/c4-regional
```

or, from a checkout of the release tag, with kustomize:

```sh
kubectl apply -k config/release
```

The image is `ghcr.io/avalanche-io/c4-operator:v0.1.0`, one image for every
component, for `linux/amd64` and `linux/arm64`.

What it installs:

| object                                            | role                                                                                                   |
| ------------------------------------------------- | ------------------------------------------------------------------------------------------------------ |
| CRDs `Project`, `Materialization`, `WorkJob`, `Pool`, `CapacityRequest` | the API (`c4.avalanche.io/v1alpha1`)                                                                   |
| Deployment `c4-manager` (namespace `c4-system`)   | controllers, the work queue, the HUD (port 8090), the pod webhook, the provenance certificate issuer   |
| DaemonSet `c4-node`                               | the CSI driver `c4.avalanche.io`, the node cache and the peer tier, capture, task records             |
| Deployment `c4-regional`                          | the regional cache tier                                                                                |
| Namespace `c4-signing`                            | the provenance signing CA, readable only by the manager                                                |
| ValidatingAdmissionPolicy `c4-node-writes`        | node daemons may change only output annotations of pods on their own node                              |
| Services `c4-hud`, `c4-signer`, `c4-regional`     | the HUD, the certificate issuer, the regional tier                                                     |

## Set up a project

A project is a set of namespaces, a store, and how its cached bytes are
shared with other projects:

```sh
kubectl -n c4-system create secret generic show-a-store \
  --from-literal=AWS_ACCESS_KEY_ID=... --from-literal=AWS_SECRET_ACCESS_KEY=...
```

```yaml
apiVersion: c4.avalanche.io/v1alpha1
kind: Project
metadata: {name: show-a}
spec:
  namespaces: [show-a]
  isolation: Strict          # or Shared: identical bytes cached once across projects
  store:
    access: Tiered           # node daemons never hold the credentials
    credentialsSecretRef: {name: show-a-store}
    shards:
    - {name: s0, url: "s3://show-a-bucket/c4/"}
    - {name: s1, url: "s3://show-a-bucket-2/c4/?endpoint=https://s3.example.com"}
```

Put trees in the store with `c4-operator ingest` (or `import-git` for a git
history), then mount them with a `Materialization` and a csi volume; the
[quickstart](quickstart.md) shows each step. [DESIGN.md](../DESIGN.md)
section 6 covers isolation, grants for vendors, and revocation.

## Run work

The work queue releases units of `WorkJob`s onto nodes of `Pool`s:

```yaml
apiVersion: c4.avalanche.io/v1alpha1
kind: Pool
metadata: {name: cpu}
spec:
  nodeSelector: {c4.avalanche.io/pool: cpu}
  slotsPerNode: 4
  costPerSlotHour: "0.40"
```

Classes are `render`, `ci` and `ml` (DESIGN.md section 10). Projects can
carry quotas per pool and a burst budget (`spec.work`).

## The HUD and the MCP server

The HUD is served by the manager on port 8090 (Service `c4-hud`). Put an SSO
proxy in front of it and start the manager with `--hud-trust-proxy`, or call
it with Kubernetes bearer tokens; every viewer sees the projects whose
namespaces they may list WorkJobs in, and every control is checked against
their permissions. `--hud-dev-viewers` skips sign-in and is for local trials
only.

Agents use the same API through MCP:

```sh
c4-operator mcp --hud https://hud.example.com --token-file ~/.c4/token
```

See [docs/agents/tools.md](agents/tools.md) and the
[`c4-farm` skill](../skills/c4-farm/SKILL.md).

## Before production

v0.1 is not ready for production. Known limits, from
[REPORT-v2.md](../REPORT-v2.md):

- Cache tiers authenticate every request with service-account tokens but
  talk plain HTTP inside the cluster; use a mesh with mTLS.
- The provenance signing CA key is a Secret; in production it belongs in a
  KMS or HSM.
- Measured on kind on one laptop only; nothing here has run at production
  scale.

## Uninstall

```sh
kubectl delete -f https://github.com/Avalanche-io/c4-operator/releases/download/v0.1.0/install.yaml
```

Deleting the CRDs deletes every Project, Materialization and WorkJob. The
stores are not touched.

## Build your own images

```sh
make images REGISTRY=registry.example.com/you VERSION=v0.1.0 PUSH=1
make install-yaml REGISTRY=registry.example.com/you VERSION=v0.1.0   # bin/install.yaml
```

`make images` cross-compiles each platform's binary and builds one
multi-arch image with buildx; without `PUSH=1` it writes an OCI archive to
`bin/` instead of pushing.
