# Projects you could take on

In most open-source projects, what could be built next lives in the
maintainers' heads, or in issues that say what is wanted but not what would
be accepted. Each project here names its goal, the contract it must meet,
where it plugs in, an example to start from and its size, so you can pick
one up and own it.

| project                                                                                    | size                      | start from                              |
| ------------------------------------------------------------------------------------------ | ------------------------- | --------------------------------------- |
| [Check a DCC panel in the real application](#check-a-dcc-panel-in-the-real-application)    | small, good first project | `integrations/dcc/<app>/CHECKLIST.md`   |
| [A farm for the HUD's examples](#a-farm-for-the-huds-examples)                             | small, good first project | c4-hud `farms/homelab.yaml`             |
| [Renderer tool kits: RenderMan, Arnold, V-Ray](#renderer-tool-kits-renderman-arnold-v-ray) | small to medium           | `integrations/dcc/houdini/tools/karma/` |
| [Orchestrator plug-ins](#orchestrator-plug-ins)                                            | small to medium, each     | `integrations/mlflow-c4/`               |
| [A tracker adapter](#a-tracker-adapter)                                                    | medium                    | `internal/tracker/linear/`              |
| [A panel for another DCC](#a-panel-for-another-dcc)                                        | medium                    | `integrations/dcc/nuke/`                |
| [A Deadline job importer](#a-deadline-job-importer)                                        | medium                    | `schema/fixtures/submit/`               |
| [A Karpenter capacity actuator](#a-karpenter-capacity-actuator)                            | medium                    | `internal/work/provreq/`                |
| [A Helm chart](#a-helm-chart)                                                              | medium                    | `config/`                               |
| [An Azure Blob store](#an-azure-blob-store)                                                | medium                    | `internal/storeurl/`                    |

## Taking one on

Open a [project issue](https://github.com/Avalanche-io/c4-operator/issues/new?template=project.yml)
saying which one, what you will build and how you will show it meets the
contract. The issue is yours: you design the project there, and it is
reviewed against its contract and tests, not against how a maintainer would
have built it. One issue per project, so two people do not build the same
thing unknowingly; if one is already open, join it.

Agents are welcome to take one on, under the same rules: start at
[llms.txt](../llms.txt), and ask the code, the design and the tests before
asking a maintainer. [CONTRIBUTING.md](../CONTRIBUTING.md) says how work
lands.

## Check a DCC panel in the real application

**Goal:** confirm that a panel works in the real application, not only
against the fake its tests run on.

**Today:** each panel's tests run against a fake of the application's
Python API. What only the real application can show is a numbered list in
the panel's `CHECKLIST.md`, and steps marked `[guess]` (Houdini's `[API]`
and `[GUESS]`) are assumptions about that API which no test can confirm.
No checklist has been run end to end yet.

**Contract:** in each version the checklist lists, build its setup scene,
point the panel at a farm (a real one, or `c4submit.testing.serve` for a
fake one over HTTP), walk every step and check each **Expect**. Where a
step fails, fix the panel and the fake together, so the tests keep matching
the application.

**Plugs in at:** [`integrations/dcc/`](../integrations/dcc/README.md), one
folder per application:

| panel   | versions         |
| ------- | ---------------- |
| Blender | 4.2 LTS, 4.5 LTS |
| Houdini | 20.5, 21         |
| Maya    | 2024, 2025, 2026 |
| Nuke    | 14, 15, 16       |
| Katana  | 7, 8             |
| Unreal  | 5.4, 5.5, 5.6    |

**Size:** small to medium per application, and a good first project.
Blender and Unreal need no licence; the others need one you already have.

## A farm for the HUD's examples

**Goal:** a farm, and the moments it is shown at, for a kind of studio the
HUD's examples do not have yet.

**Contract:** the farm format in c4-hud's
[`farms/README.md`](https://github.com/Avalanche-io/c4-hud/blob/main/farms/README.md).
A farm is scheduled by the operator's own policy (`sched.Decide`), so what
the HUD shows is what the farm would do.

**Plugs in at:** c4-hud's `farms/`, beside the seven farms there, from a
laptop swarm to a facility on four continents.

**Start from:** `farms/homelab.yaml` or `farms/qc.yaml`.

**Size:** small, and a good first project.

## Renderer tool kits: RenderMan, Arnold, V-Ray

**Goal:** a tool folder for each of `prman`, `arnold` and `vray`, so a
studio adds only its own installer and licence server.

**Contract:** `tool/1` ([`schema/tool-1.schema.json`](../schema/tool-1.schema.json)),
the script conventions in [`tools/README.md`](../tools/README.md), and the
shipped-tool tests (`TestShippedToolsRead` in
[`integrations/dcc/tools_test.go`](../integrations/dcc/tools_test.go)).
Each renders the sample shot's USD stage through its Hydra delegate to
OpenEXR, prints a line per finished frame, and says why a frame failed and
whether a retry could help.

**Plugs in at:** `integrations/dcc/`, with a Dockerfile template the studio
builds with its installer.

**Start from:** Houdini's Karma tools, `integrations/dcc/houdini/tools/karma/`.

**Size:** small to medium. Testing needs the renderer and a licence. The
farm does not count licence seats yet, and refuses a tool that asks for
them, so these tools declare none until it does.

## Orchestrator plug-ins

**Goal:** let Prefect, Airflow, Dagster or Argo hand a task to C4, which runs
its units against a deadline and returns the output's C4 ID.

**Contract:** the HUD's API, v1 ([`internal/hud/API.md`](../internal/hud/API.md)):
send what the farm lacks (`POST /objects/{project}/missing`, then
`PUT /objects`), make the view (`POST /views`), submit (`POST /jobs` or
`POST /submissions`), and follow the job (`GET /jobs/{ns}/{name}`,
`/events`). What fits each orchestrator: a Prefect worker type, an Airflow
deferrable operator, a Dagster Pipes client, an Argo executor or artifact
plugin.

**Plugs in at:** a package of its own under `integrations/`.

**Start from:** [`integrations/mlflow-c4/`](../integrations/mlflow-c4),
MLflow's backend for C4, tested against a fake farm.

**Size:** small to medium, each one a project of its own.

## A tracker adapter

**Goal:** keep another production tracker in step with the farm, as
[c4-sync](c4-sync.md) does for GitHub, Flow Production Tracking, Kitsu,
Linear and MLflow. ftrack, AYON, GitLab and Jira are designed and not built.

**Contract:** the `Adapter` interface in
[`internal/tracker/tracker.go`](../internal/tracker/tracker.go) (`Kind`,
`Capabilities`, `Get`, `List`, `Changes`, `Decisions`, `Receive`, `Apply`,
`Publish`, `Probe`, `Guard`, `Person`), with its errors (`ErrUnsupported`,
`ErrNotFound`, `ErrRefused`, `ErrBadSignature`, `RateLimited`), and the
`Presenter` in [`internal/tracker/present.go`](../internal/tracker/present.go).
An adapter registers itself with `tracker.Register`.

**Plugs in at:** `internal/tracker/<kind>/`, and one import in
`cmd/c4-sync/main.go`.

**Start from:** `internal/tracker/linear/` or `internal/tracker/mlflow/`.
Each adapter brings a fake of its tracker (`<kind>test/server.go`), its own
tests, and the syncer's loop tests (`syncer_<kind>_test.go`), which drive a
job from the tracker to done and back.

**Size:** medium.

## A panel for another DCC

**Goal:** submit to the farm from inside another application, as artists
already can from Nuke, Houdini, Maya, Blender, Katana and Unreal.

**Contract:** a `c4submit.host.Host`
([`integrations/dcc/c4submit/src/c4submit/host.py`](../integrations/dcc/c4submit/src/c4submit/host.py)),
which says what the scene is: its file, frame range, renderables and the
files it reads; and `c4submit.form.Form` drawn in the application's own
widgets. [Writing a panel for another DCC](../integrations/dcc/README.md#writing-a-panel-for-another-dcc)
covers the rest, including what `c4 farm integrate` needs to find it.

**Plugs in at:** `integrations/dcc/<app>/`, with tests against a fake of
the application's Python API (run by `python -m pytest integrations/dcc`)
and a `CHECKLIST.md` for the real one.

**Start from:** `integrations/dcc/nuke/`, the smallest, or
`integrations/dcc/blender/`.

**Size:** medium.

## A Deadline job importer

**Goal:** turn a Deadline 10 job (`job.info` and `plugin.info`) into a C4
submission, with a report of what translated and what needs a person.

**Contract:** the translation rules in [DESIGN.md](../DESIGN.md) section 14:
frames and chunk size become units, pools and groups become pool
requirements, job dependencies become `dependsOn`, the machine limit
becomes the parallel cap. The output is a `submit/1` file
([`schema/submit-1.schema.json`](../schema/submit-1.schema.json)) that
`c4 farm submit FILE` accepts.

**Plugs in at:** a command of its own to start with. Where it lives in the
farm is part of the project.

**Start from:** the submission fixtures in `schema/fixtures/submit/`, and
the panels' shared `c4submit/submission.py`.

**Size:** medium.

## A Karpenter capacity actuator

**Goal:** fulfil a burst pool's capacity requests by setting a Karpenter
NodePool's limits, as the operator already does with Kubernetes
ProvisioningRequests.

**Contract:** `CapacityRequest`, its spec and status, in
[`api/v1alpha1/work_types.go`](../api/v1alpha1/work_types.go), and the
behaviour in [DESIGN.md](../DESIGN.md) section 10.12. A pool's `burst` already
accepts `provider: karpenter`.

**Plugs in at:** a `manager.Runnable` chosen by the manager's
`--capacity-provider` flag (`cmd/c4-operator/manager.go`).

**Start from:** [`internal/work/provreq/`](../internal/work/provreq/), and
its envtest.

**Size:** medium. Exporting the two metrics KEDA would scale on
(`c4_capacity_requested_slots` and `c4_work_required_slots`) is a small
project beside it.

## A Helm chart

**Goal:** install the operator with one Helm chart, published as an OCI
artifact, where today there is kustomize.

**Contract:** what [DESIGN.md](../DESIGN.md) section 17 lists: the CRDs,
the manager, the node daemon, the regional tier, the HUD and MCP. It must
render without a cluster, as `helm template` does, since GitOps tools and
cloud marketplaces render charts that way.

**Plugs in at:** a chart beside `config/`.

**Start from:** the kustomize trees under `config/` (`default`, `release`,
`node`, `regional`).

**Size:** medium.

## An Azure Blob store

**Goal:** let a project's store be Azure Blob Storage, which has no S3 API.

**Contract:** the `Shard` interface in
[`internal/storeurl/storeurl.go`](../internal/storeurl/storeurl.go)
(`Name`, `Get` from an offset, `Head`, `Put`). `Get` must resume at an
offset, and `Head` must tell an object that is not there from a store that
cannot be reached.

**Plugs in at:** `Open`'s switch on the store URL, and `Credentials`, which
holds only S3 keys today.

**Start from:** the S3 shard and its tests in `storeurl_test.go`.

**Size:** medium. It touches how store credentials are held, so the
maintainers review that part with you.

## Next, once their contract exists

These are wanted, but what they must meet is not written down yet, or what
they plug into is not built. Each moves into the list above when it is.

- **Runtimes for other agent harnesses.** C4 runs a block's agent in Claude
  Code today. Its own lean harness, with a written behaviour contract and a
  parity suite that runs the same blocks under each runtime and compares
  what they do, is designed and not yet built. Once it is, a harness that
  meets the contract can run blocks too.
- **One image for both runtimes.** A containerd snapshotter that fills a
  heavy image's tree layers from C4's storage where it can and from the
  registry where it cannot, so one image serves both kinds of runtime and
  nothing is moved twice. It waits for heavy images whose tree layers C4
  writes.
- **More model hubs.** Models are imported from Hugging Face. Another hub
  needs the source interface that the Hugging Face client will be lifted
  into first.

## Proposing a project

A project with a contract someone else could test is one others can review.
Propose one with the [project form](https://github.com/Avalanche-io/c4-operator/issues/new?template=project.yml):
what it does and must never do, where it plugs in, and how anyone can check
it.
