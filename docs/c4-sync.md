# Running c4-sync

c4-sync keeps a tracker and the farm in step, so nobody retypes work
between them. A tracker entity (a GitHub issue, a Flow Production Tracking
Task, an MLflow run) becomes a C4 job with the tracker's due date as its
deadline. The job's work states go back to the tracker, and the job's
output is published there with its C4 ID. When a person approves or asks
for changes in the tracker, c4-sync relays that decision to the job's
review wait.

c4-sync is **experimental**. Its two resources, `Tracker` and `TrackerLink`
(`c4.avalanche.io/v1alpha1`), are not part of the operator's install and
may change. The design is in proposals/integrations (ADAPTER.md, MAPPING.md).

| tracker                  | `kind`   | work in                             | review out                                             | decisions back                          |
| ------------------------ | -------- | ----------------------------------- | ------------------------------------------------------ | --------------------------------------- |
| GitHub                   | `github` | labeled issues; milestone `due_on`  | one comment; a check run per commit; commit statuses   | pull request reviews; check-run buttons |
| Flow Production Tracking | `flow`   | Tasks by field; `due_date` (a date) | Task status and two fields; a Version per output       | Version status changes and Notes        |
| MLflow                   | `mlflow` | scheduled runs; `c4.deadline` tag   | run status and `c4.*` tags; a model version per output | the approve alias; a review tag         |

## What it writes, and what it never writes

c4-sync writes **work states only**: queued, running, waiting for review,
reworking, needs a person, failed, done. It never writes a judgment. It
never approves, merges, sets a Flow Version's approved status, or sets an
MLflow alias. Approval stays with the person who approves, in the tracker.
It changes no status when only the ETA moves, and it writes one cause per
failure group, not one per frame.

It changes jobs only through the HUD API, as its own identity
(`X-C4-Agent: c4-sync`), so the HUD authorizes, signs and books each
change like a person's. A decision it relays names the tracker, the
entity that carries it, and the reviewer in its note. The decision's raw
evidence is stored by C4 ID in the project's store.

## Install

From a release (one manifest with the two CRDs, RBAC, the Deployment and
the webhook Service, in `c4-system`):

```sh
kubectl apply -f https://github.com/Avalanche-io/c4-operator/releases/download/<tag>/c4-sync.yaml
kubectl -n c4-system rollout status deploy/c4-sync
```

or from a checkout:

```sh
kubectl apply -f config/crd/c4.avalanche.io_trackers.yaml -f config/crd/c4.avalanche.io_trackerlinks.yaml
kubectl apply -k config/sync
```

`make image-sync` builds the image for this machine as `c4-sync:dev`.

c4-sync calls the HUD at `--hud` (default in the manifest:
`http://c4-hud.c4-system.svc`) with a projected service-account token that
the kubelet rotates. The HUD must allow its service account
(`system:serviceaccount:c4-system:c4-sync`) to create and patch WorkJobs
in each project namespace it syncs. That is the same SubjectAccessReview
the HUD applies to people.

## Connect a tracker

Each project that syncs with a tracker creates three things in its own
namespace:

1. **The credential, as a Secret.** c4-sync reads it on every use, so a
   rotation takes effect at once. A credential lease keeps it current
   (proposals/integrations GAPS 8); until leases exist, rotate it yourself.
   - GitHub: a GitHub App installation token, which lasts an hour.
   - Flow: the API script's key. Give the script its own permission group,
     limited to the project.
   - MLflow: the password of a basic-auth user with EDIT on the experiment.
     It needs MANAGE only if c4-sync should create model versions under an
     admin-only model.
2. **A Role letting c4-sync read those Secrets by name**, and nothing else.
   See `config/samples/tracker-github/role.yaml`.
3. **The Tracker.** Start from a sample:
   `config/samples/tracker-github`, `tracker-flow` or `tracker-mlflow`.

Each Tracker says:
- `intake`: which entities become jobs, and from what job spec;
- `deadline`: how a date with no time of day becomes one, for example
  14:00 in `America/Los_Angeles`, Flow's dailies;
- `review.wait`: the job's wait that the tracker's decision serves;
- `rework`: `inPlace` reruns units, the CI, agent and ML way;
  `nextVersion` makes v002 follow v001 with the note as an input view,
  the media way;
- `statusOut.states`: your site's own status codes (Flow lets admins rename
  every status);
- `verdictsIn`: which tracker decisions mean approve, rework or accept;
- `people`: tracker users mapped to C4 people.

**People.** A person's wait (`person:dana`) is served only by the tracker
user `people` maps to `dana`. Anyone else's decision is refused and
recorded on the link. Roles never come from the tracker: a tracker admin
cannot grant farm access. MLflow records no author for an alias or a tag.
Its decisions are attributed to `mlflow-registry`, which you map to a
party (`party:ml-leads`), and a person's wait refuses them.

## Webhooks

Deliveries arrive at `POST /webhooks/<namespace>/<tracker>` on the
`c4-sync` Service (port 80). Put it behind your ingress, or a relay for
sites that cannot take inbound traffic. Set `webhookSecret` so each
delivery is verified:
- GitHub: `X-Hub-Signature-256`;
- Flow: `X-SG-SIGNATURE`, `sha1=`;
- MLflow: `X-MLflow-Signature`, `v1,`, with a five-minute timestamp window.

Deliveries are only hints. c4-sync also polls the tracker (`events.pollSeconds`)
and reconciles every link (`events.resyncSeconds`), so a lost delivery
costs time, never correctness. Flow is read from its event log, which it
can replay from any entry.

## Watching it

```sh
kubectl get trackers,trackerlinks -A
kubectl -n <ns> describe tracker <name>
```

A Tracker reports `Ready`, `RateLimited` (with when it resumes) and
`Intake`. Intake as a milestone needs the operator's Milestone, which is
not built yet (`MilestoneNotBuilt`). A TrackerLink's status shows:
- the summary state last written;
- what is under review;
- each relayed or refused decision, with the reason;
- the previous versions;
- `Diverged`, when a person pinned another deadline in the HUD (the person
  wins).

## MLflow projects on the farm

`integrations/mlflow-c4` adds two MLflow plugins:
- `mlflow run --backend c4` stores the project as a view, submits it as a
  C4 job, and tags the run `c4.job`, so c4-sync adopts the run instead of
  submitting it twice;
- a `c4` artifact repository reads `c4:///<view>` sources, so `models:/`
  downloads work.

An MLflow server refuses a `c4://<view>` source whose path is empty, and
accepts `c4:///<view>`. That is why c4-sync publishes the second form.
See integrations/mlflow-c4/README.md.

## Limits

- Experimental API; no Milestone, relayed-decision custody or `follows`
  field in the operator yet (proposals/integrations GAPS 1, 3, 6). Until
  then, a relayed decision's facts are in its note, and a version chain
  is kept in its TrackerLink.
- One c4-sync replica: there is no leader election yet.
- Tracker request limits are those each vendor publishes (GitHub's), or a
  conservative default (`rate`) where none are published (Flow).
