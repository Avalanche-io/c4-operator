# Quickstart: c4-operator on kind in five minutes

This runs the operator on a three-node kind cluster on your machine, puts a
small directory tree into a store, mounts it in a pod as a C4 view, captures
what the pod writes, and queues a small render you can watch in the HUD.

You need Docker, [kind](https://kind.sigs.k8s.io) 0.33 or later, kubectl,
and Go 1.26 for the command-line tool.

## 1. The source and the CLI

```sh
git clone https://github.com/Avalanche-io/c4-operator
cd c4-operator
git checkout v0.1.0
go install ./cmd/c4-operator        # or take a binary from the release page
```

## 2. A cluster

```sh
kind create cluster --name c4 --config config/samples/quickstart/kind.yaml
```

Two workers in one zone, both in the `cpu` pool. The control plane maps the
store to `127.0.0.1:30900` and the HUD to `127.0.0.1:30080`.

## 3. The operator

```sh
kubectl apply -k config/release
kubectl -n c4-system rollout status deploy/c4-manager
kubectl -n c4-system rollout status ds/c4-node
```

The images come from `ghcr.io/avalanche-io/c4-operator`.

## 4. A store and a project

```sh
kubectl apply -f config/samples/quickstart/store.yaml
kubectl -n c4-store wait --for=condition=complete job/bucket --timeout=300s
kubectl -n c4-system create secret generic quickstart-store \
  --from-literal=AWS_ACCESS_KEY_ID=GKc4c4c4c4c4c4c4c4c4c4c4c4 \
  --from-literal=AWS_SECRET_ACCESS_KEY=c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4
kubectl apply -f config/samples/quickstart/project.yaml
```

The store is one [Garage](https://garagehq.deuxfleurs.fr) node with a
bucket named `quickstart`; the project `demo` owns the namespace `demo`
and reads and writes that store. Garage key IDs are `GK` and 24 hex digits,
and secrets 64 hex digits; these are for a local trial only.

## 5. Put a tree in the store

```sh
mkdir -p shot/plates
for f in 1001 1002 1003 1004; do head -c 1048576 /dev/urandom > shot/plates/sh010.$f.exr; done
export AWS_ACCESS_KEY_ID=GKc4c4c4c4c4c4c4c4c4c4c4c4
export AWS_SECRET_ACCESS_KEY=c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4
VIEW=$(c4-operator ingest -q --shard 's0=s3://quickstart/c4/?endpoint=http://127.0.0.1:30900' shot)
echo "$VIEW"
```

`ingest` hashes each file once, uploads what the store lacks and prints the
view's ID: the C4 ID of the whole tree.

## 6. Mount it in a pod

```sh
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
kubectl -n demo wait --for=jsonpath='{.status.phase}'=Ready materialization/shot
kubectl -n demo wait --for=jsonpath='{.status.phase}'=Succeeded pod/reader --timeout=120s
kubectl -n demo logs reader
```

The Materialization resolved the view against the project's store and
checked every object is there. The pod's volume is an overlay of a layer
built in the node's cache; the node fetched each object once, from the
store or from a peer.

When the pod ended, the node daemon captured what it wrote as a changeset
over the view and signed a record of the task:

```sh
kubectl -n demo get pod reader -o jsonpath='{.metadata.annotations}' | tr ',' '\n' | grep output
```

`c4.avalanche.io/output-tree` is the ID of the tree the pod left behind (the
plates plus `notes.txt`), `output-changeset` the ID of the change, and
`output-provenance` the signed task record.

## 7. Queue a render and watch it in the HUD

```sh
kubectl apply -f - <<EOF
apiVersion: c4.avalanche.io/v1alpha1
kind: WorkJob
metadata: {name: sh010-comp-v001, namespace: demo}
spec:
  class: render
  params: {frames: "1001-1024", order: binary, estimateSeconds: "4"}
  inputs: [{materialization: shot, mountPath: /show}]
  template:
    spec:
      securityContext: {runAsNonRoot: true, runAsUser: 65532, seccompProfile: {type: RuntimeDefault}}
      containers:
      - name: comp
        image: busybox:1.37
        command: [sh, -c, "mkdir -p /show/comp && for f in \$(seq \$C4_FRAME_START \$C4_FRAME_END); do sleep 3; echo frame \$f > /show/comp/sh010.\$f.exr; done"]
        securityContext: {allowPrivilegeEscalation: false, capabilities: {drop: [ALL]}}
EOF
```

The HUD needs a signed-in viewer. For this local trial only, let the
manager accept a development viewer:

```sh
kubectl -n c4-system patch deploy c4-manager --type=json \
  -p='[{"op":"add","path":"/spec/template/spec/containers/0/args/-","value":"--hud-dev-viewers=wrangler=*;artist=demo"}]'
kubectl -n c4-system rollout status deploy/c4-manager
open "http://127.0.0.1:30080/?as=wrangler"      # or xdg-open, or paste it into a browser
```

`wrangler` sees every project; `artist` sees only `demo`, and the top
bar's **View as** switches between them.

The queue releases the job's units in binary order (1001, 1024, 1012, ...)
two slots per node, adds each unit's captured frames to the job's output
view, and shows its estimate converge as frames finish. Set a deadline and
watch the queue plan for it:

```sh
kubectl -n demo patch workjob sh010-comp-v001 --type=merge -p "{\"spec\":{\"deadline\":\"$(date -u -v+10M +%Y-%m-%dT%H:%M:%SZ 2>/dev/null || date -u -d '+10 min' +%Y-%m-%dT%H:%M:%SZ)\"}}"
```

## 8. Ask an agent

```sh
claude mcp add c4-farm -- c4-operator mcp --hud http://127.0.0.1:30080 --as wrangler
```

Then ask, for example, "which frames of sh010-comp-v001 are done, and will
it make its deadline?" The tools are listed in
[docs/agents/tools.md](agents/tools.md).

## Clean up

```sh
kind delete cluster --name c4
```

## Next

- [docs/install.md](install.md): a cluster of your own.
- [DESIGN.md](../DESIGN.md): how views, tiers, isolation, the queue and
  provenance work.
- [REPORT-v2.md](../REPORT-v2.md): what was measured, and what is not built.
