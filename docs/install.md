# Installing c4-operator

This is c4-operator v0.1; the API group `c4.avalanche.io/v1alpha1` may change
before 1.0. To try it locally, follow the [quickstart](quickstart.md); this
page is for a cluster of your own.

## Requirements

- **Kubernetes.** Tested on 1.36. The operator relies on features that are
  on by default from 1.32: ValidatingAdmissionPolicy, CSI inline ephemeral
  volumes, and bound service-account tokens that name their node.
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
history), then mount them with a `Materialization` and a csi volume;
[a trial cluster on kind](#a-trial-cluster-on-kind-by-hand) shows each step. [DESIGN.md](../DESIGN.md)
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

To place nodes on the HUD's map, label them with
`topology.kubernetes.io/region` and `topology.kubernetes.io/zone` and give
the manager `--hud-sites` with a JSON file of the farm's sites:

```json
[
  {"name": "burbank", "label": "Studio, Burbank", "region": "burbank", "lat": 34.18, "lon": -118.31,
   "kind": "onprem", "localities": ["rack r1", "rack r2"], "tz": "America/Los_Angeles"},
  {"name": "aws-us-west-2", "label": "AWS us-west-2", "region": "us-west-2", "lat": 45.84, "lon": -119.70,
   "kind": "cloud", "localities": ["us-west-2a"], "tz": "America/Los_Angeles"}
]
```

Nodes join a site by region; `tz` (an IANA time zone) sets the site clock's
local time; `localities` are the values of the nodes'
locality label (`topology.kubernetes.io/zone` unless a project sets another)
in display order.

The HUD has three more faces besides the desk: the wall, `#/wall`, for a
machine room or a visitor gallery (add `?tour=1` to cycle the shots); the
side widget, `#/widget/<namespace>/<job>`, sized for the edge of an
artist's second monitor (`?compact=1` for a corner); and a phone layout
below 720 px. Component previews for the design system are served at
`/design/previews/`.

Agents use the same API through MCP:

```sh
c4-operator mcp --hud https://hud.example.com --token-file ~/.c4/token
```

See [docs/agents/tools.md](agents/tools.md) and the
[`c4-farm` skill](../skills/c4-farm/SKILL.md).

## A trial cluster on kind, by hand

`c4 farm up` builds a local farm in one command ([the quickstart](quickstart.md)).
To see each piece go in yourself, the same farm by hand:

```sh
kind create cluster --name c4 --config config/samples/quickstart/kind.yaml
kubectl apply -k config/release
kubectl -n c4-system rollout status deploy/c4-manager
kubectl apply -f config/samples/quickstart/store.yaml
kubectl -n c4-store wait --for=condition=complete job/bucket --timeout=300s
kubectl -n c4-system create secret generic quickstart-store \
  --from-literal=AWS_ACCESS_KEY_ID=GKc4c4c4c4c4c4c4c4c4c4c4c4 \
  --from-literal=AWS_SECRET_ACCESS_KEY=c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4
kubectl apply -f config/samples/quickstart/project.yaml
```

Two workers in the `cpu` pool, the operator, a one-node
[Garage](https://garagehq.deuxfleurs.fr) store with a bucket named
`quickstart` (these keys are for a local trial only), and the project `demo`.
Put a tree in the store and mount it in a pod:

```sh
mkdir -p shot/plates
for f in 1001 1002 1003 1004; do head -c 1048576 /dev/urandom > shot/plates/sh010.$f.exr; done
export AWS_ACCESS_KEY_ID=GKc4c4c4c4c4c4c4c4c4c4c4c4
export AWS_SECRET_ACCESS_KEY=c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4
VIEW=$(c4-operator ingest -q --shard 's0=s3://quickstart/c4/?endpoint=http://127.0.0.1:30900' shot)
kubectl apply -f - <<EOF
apiVersion: c4.avalanche.io/v1alpha1
kind: Materialization
metadata: {name: shot, namespace: demo}
spec:
  source: {id: "$VIEW"}
---
apiVersion: v1
kind: Pod
metadata: {name: reader, namespace: demo}
spec:
  restartPolicy: Never
  securityContext: {runAsNonRoot: true, runAsUser: 65532, seccompProfile: {type: RuntimeDefault}}
  containers:
  - name: reader
    image: busybox:1.37
    command: [sh, -c, "ls -l /show/plates && echo 'looks good' > /show/plates/notes.txt"]
    securityContext: {allowPrivilegeEscalation: false, capabilities: {drop: [ALL]}}
    volumeMounts: [{name: show, mountPath: /show}]
  volumes:
  - name: show
    csi:
      driver: c4.avalanche.io
      volumeAttributes: {materialization: shot}
EOF
kubectl -n demo wait --for=jsonpath='{.status.phase}'=Succeeded pod/reader --timeout=120s
kubectl -n demo get pod reader -o jsonpath='{.metadata.annotations}' | tr ',' '\n' | grep output
```

The Materialization resolves the view against the project's store; the
pod's volume is an overlay of a layer built in the node's cache, each object
fetched once. When the pod ends, the node daemon captures what it wrote as a
changeset over the view (`output-tree`, `output-changeset`) and signs a
record of the task (`output-provenance`). A WorkJob is the same pod template
with the queue in charge: see [Run work](#run-work). `kind delete cluster
--name c4` removes it all.

## Hardening for production

- **Encrypt traffic between cache tiers.** Every tier request is
  authenticated with a service-account token and authorized by project;
  run the tiers in a mesh with mTLS so the bytes are encrypted as well.
- **Keep the signing root in a KMS or HSM.** The provenance signing CA is a
  Secret in `c4-signing` that only the manager reads; in production the
  root key lives in a KMS or HSM and signs an intermediate for the manager.
- **Sign in through your SSO.** Put an SSO proxy in front of the HUD and
  start the manager with `--hud-trust-proxy`; never set `--hud-dev-viewers`.

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

## Tracker sync (experimental)

To keep GitHub, Flow Production Tracking or MLflow in step with the farm,
install c4-sync from the release's `c4-sync.yaml`: see [c4-sync](c4-sync.md).
