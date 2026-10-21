# Running c4-sync

c4-sync keeps a tracker and the farm in step, so nobody retypes work
between them. A tracker entity (a GitHub issue, a Flow Production Tracking
Task, a Kitsu task, an MLflow run) becomes a C4 job with the tracker's due
date as its deadline. The job's work states go back to the tracker, and the
job's output is published there with its C4 ID. When a person approves or
asks for changes in the tracker, c4-sync relays that decision to the job's
review wait.

c4-sync is **experimental**. Its two resources, `Tracker` and `TrackerLink`
(`c4.avalanche.io/v1alpha1`), are not part of the operator's install and
may change. Its place in the design is DESIGN.md section 15.2.

| tracker                  | `kind`   | work in                                             | review out                                                           | decisions back                                   |
| ------------------------ | -------- | --------------------------------------------------- | -------------------------------------------------------------------- | ------------------------------------------------ |
| GitHub                   | `github` | labeled issues; milestone `due_on`                  | one comment; a check run per commit; commit statuses                 | pull request reviews; check-run buttons          |
| Flow Production Tracking | `flow`   | Tasks by field; `due_date` (a date)                 | Task status and two fields; a Version per output                     | Version status changes and Notes                 |
| Kitsu                    | `kitsu`  | tasks by type, status and shot; `due_date` (a date) | comments carrying the task's status; task data; a preview per output | a person's comment moving the task out of review |
| MLflow                   | `mlflow` | scheduled runs; `c4.deadline` tag                   | run status and `c4.*` tags; a model version per output               | the approve alias; a review tag                  |
| Linear                   | `linear` | labeled issues; `dueDate` (a date)                  | workflow state by type; one status card; an attachment per output    | state changes by people, with their comments     |

## What it writes, and what it never writes

c4-sync writes **work states only**: queued, running, waiting for review,
reworking, needs a person, failed, done. It never writes a judgment. It
never approves, merges, sets a Flow Version's approved status, moves a Kitsu
task to done or out of it, or sets an MLflow alias. Approval stays with the
person who approves, in the tracker. It changes no status when only the ETA
moves, and it writes one cause per failure group, not one per frame.

It changes jobs only through the HUD API, as its own identity
(`X-C4-Agent: c4-sync`), so the HUD authorizes, signs and books each
change like a person's. A decision it relays names the tracker, the
entity that carries it, and the reviewer in its note. The decision's raw
evidence is stored by C4 ID in the project's store.

Each tracker write is made once (`internal/effect`). c4-sync keeps the
write in the link's `status.written` before it sends it, so a crash or a
lost answer leaves it marked `sending`. The next pass then does one of
three things, as the adapter allows for that write:
- sends it again, when sending it twice changes nothing (setting a field,
  editing a comment, updating a check run);
- looks for it first, by the key marker that a new comment or Note
  carries, and sends it only if it is not there;
- holds it as `unconfirmed`, when nothing can tell whether it happened. The
  link then shows `Unconfirmed`. The HUD lists the write on the job, on
  the Tracker and on its owner's own page, and MCP lists it with
  `c4_list_held_writes`. A person looks in the tracker and says whether
  it happened (`c4_confirm_write`, or the HUD). It is never sent twice
  on a guess.

A write the tracker refused (a 4xx) is known not to have happened and is
tried fresh. c4-sync looks for a write only after an interruption, or
once when the link has no record of the field (its status was lost), not
before every write.

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
   ([credential leases](agents/credential-leases.md)); until a lease holds
   it, rotate it yourself.
   - GitHub: a GitHub App installation token, which lasts an hour.
   - Flow: the API script's key. Give the script its own permission group,
     limited to the project.
   - Kitsu: a bot's API token. An admin creates the bot, gives it the
     manager role (Kitsu lets managers set any status and read the event
     log), an expiry date, and a place in the production's team.
     Regenerating the token revokes the old one.
   - MLflow: the password of a basic-auth user with EDIT on the experiment.
     It needs MANAGE only if c4-sync should create model versions under an
     admin-only model.
   - Linear: a personal API key limited to the team, or an OAuth app's
     access token (`actor=app`, so its changes are the app's, not a
     person's).
2. **A Role letting c4-sync read those Secrets by name**, and nothing else.
   See `config/samples/tracker-github/role.yaml`.
3. **The Tracker.** Start from a sample:
   `config/samples/tracker-github`, `tracker-flow`, `tracker-kitsu`,
   `tracker-linear` or `tracker-mlflow`.

Each Tracker says:
- `intake`: which entities become jobs, and from what job spec;
- `deadline`: how a date with no time of day becomes one, for example
  `{dateOnlyAt: "14:00", timeZone: America/Los_Angeles}`, Flow's dailies.
  Without it, a date falls at the project's due time (Project
  `spec.work.dueTime`, 18:00 UTC without one), as the farm reads any date;
- `as: milestone` with `covers`: instead of a job, the entity becomes a
  Milestone that covers the jobs whose labels match its fields, whoever
  submits them, for example `covers: {c4.avalanche.io/shot: entity.name}`
  for a Flow Task's shot. Those jobs take its due date unless they have
  their own;
- `for`: whose work the jobs are, `{person: dana}` or `{assignee: true}`
  (the entity's assignee, through `people`). The person owns the job, so
  what is theirs serves it, such as a lease of their own sign-in. They must
  first let the Tracker submit for them, which they do as themselves, not
  in the Tracker: `c4 farm grant <tracker>` (optionally `--pools`,
  `--classes`, `--until 30d`), MCP `c4_grant`, or `POST /api/v1/grants`.
  Until then nothing is submitted and the Tracker shows `NoGrant`; c4-sync
  never submits the job as its own instead. `c4 farm grants --tracker
  <ns>/<tracker>` shows who has granted it; `c4 farm revoke` takes a grant
  back. Without `for`, the jobs are c4-sync's;
- `as: forecast` with `forecast`: an entity not yet ready (a shot not
  turned over) becomes a forecast job, `<job>-forecast`. It never runs, but
  the farm plans for it and shows its risk. Its size is `slotHours`, or
  the entity's own estimate times `perUnit` (Flow's bid:
  `{field: est_in_mins, perUnit: "0.05"}` for 3 slot-minutes per bid
  minute), or, with neither, the class's plan of the job's params. When
  the bid changes, the forecast is resized to follow it (its slot-hours
  and unit hours; anything else set on the forecast stays). When the
  entity matches no rule any more (an omitted shot) or is gone, the
  forecast is withdrawn (deleted, signed and booked as `withdrawn`) and
  its link goes; if it comes back, it is a new forecast. Nothing
  is written to the tracker for it. When a job rule matches the entity
  later, the real job replaces the forecast and the link moves to it;
- `review.wait`: the job's wait that the tracker's decision serves. With
  `job.review` (the job's own final review, `@review`, after all its
  work), leave it out: the tracker serves that one, and the job is done
  only once it is decided there;
- `rework`: `inPlace` reruns units, the CI, agent and ML way;
  `nextVersion` makes v002 follow v001 with the note as an input view,
  the media way;
- `statusOut.states`: your site's own status codes (Flow lets admins rename
  every status). Kitsu needs none: its statuses carry flags, and c4-sync
  moves a task to the one flagged work in progress while the job runs and
  to the one flagged for review when it publishes; name a status only to
  override that, or to give a needed person a status of their own;
- `verdictsIn`: which tracker decisions mean approve, rework or accept.
  For Kitsu, a status flagged done approves and one flagged retake asks for
  rework; name a status (`status:cbb: accept`) to override its flag.
  For Linear, a move to a completed-type state approves whatever the team
  calls it; name any other state (`state:Changes Requested: rework`);
- `people`: tracker users mapped to C4 people: GitHub and Flow logins,
  Kitsu and Linear emails.

**People.** A person's wait (`person:dana`) is served only by the tracker
user `people` maps to `dana`. Anyone else's decision is refused and
recorded on the link. Roles never come from the tracker: a tracker admin
cannot grant farm access. MLflow records no author for an alias or a tag.
Its decisions are attributed to `mlflow-registry`, which you map to a
party (`party:ml-leads`), and a person's wait refuses them.

**What it missed.** c4-sync keeps a cursor into the farm's transition log
(`status.farmCursor`, `GET /book?since`) and reads from it each pass, so a
review decided in C4 itself (in the HUD, by an agent over MCP) reaches the
tracker as one comment, even if c4-sync was down when it happened.
Decisions the tracker relayed are not echoed back. A new Tracker starts at
the log's head.

**Causes.** For each cause a person must act on (units that failed
after their retries, a review's escalation, information a brief lacked),
c4-sync keeps one comment on the issue or Task: what it is ("exit 137
OOMKilled"), how many attempts, which units and nodes, and what the farm did.
It edits that comment in place as the cause grows, so the tracker carries one
item per cause, not one per failure. Causes the farm handled on its own (an
attempt retried) are not posted.

**Assignments.** When someone assigns failed units to a person in C4
(`c4 farm`'s acknowledge with an assignee, the HUD, MCP
`c4_acknowledge_units`), c4-sync tells that person where they work: an
@-mention on the GitHub issue, a Note on the Flow Task addressed to them, or
a comment on the Kitsu task that mentions them, so the tracker notifies
them. It does this once per assignment, for a person `people` maps to a
tracker user. Anyone else stays assigned in C4 only, and
the link's `Assigned` condition says so.

## Webhooks

Deliveries arrive at `POST /webhooks/<namespace>/<tracker>` on the
`c4-sync` Service (port 80). Put it behind your ingress, or a relay for
sites that cannot take inbound traffic. Set `webhookSecret` so each
delivery is verified:
- GitHub: `X-Hub-Signature-256`. A check-run button (approve, rework)
  carries its decision in the delivery alone, so without a secret the
  buttons decide nothing: anyone who could reach the endpoint could
  otherwise press them as a reviewer;
- Flow: `X-SG-SIGNATURE`, `sha1=`;
- MLflow: `X-MLflow-Signature`, `v1,`, with a five-minute timestamp window;
- Linear: `Linear-Signature`, hex HMAC-SHA256, with a one-minute timestamp
  window (`webhookTimestamp`), so a captured delivery cannot be replayed.

Deliveries are only hints. c4-sync also polls the tracker (`events.pollSeconds`)
and reconciles every link (`events.resyncSeconds`), so a lost delivery
costs time, never correctness. Flow is read from its event log, which it
can replay from any entry. Kitsu pushes nothing c4-sync can verify or
replay (its live events are Socket.IO), so c4-sync reads its event log.
Linear keeps no event log, so c4-sync polls the team's issues by
`updatedAt` under the webhooks.

## Watching it

```sh
kubectl get trackers,trackerlinks -A
kubectl -n <ns> describe tracker <name>
```

A Tracker reports `Ready`, `RateLimited` (with when it resumes) and
`Intake` (`MilestoneCoversNothing` when a milestone rule names no
`covers`, or an entity lacks a field it names; `NameTaken` when a job
someone else made already has the name an entity's job would take, which
c4-sync never adopts). `kubectl get milestones`
shows each Milestone's due date, how many jobs it covers and their worst
risk. A TrackerLink's status shows:
- the summary state last written;
- what is under review;
- each relayed or refused decision, with the reason;
- the previous versions;
- `Diverged`, when a person pinned another deadline in the HUD (the person
  wins);
- `Unconfirmed`, when a write was interrupted and nothing can tell whether
  it happened (above).

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

## Kitsu

Kitsu changes a task's status only by a comment that carries it, so every
status c4-sync writes is a short comment ("C4: running, ETA Thu 14:20"
and the job's link), posted only when the state changes and never when
the task already has that status. A **done** task is the supervisor's
approval: c4-sync never moves a task out of a done status, whatever the
job does afterwards.

- **Matching.** Intake rules match tasks in Kitsu's own words:
  `task_type` (a task type's name), `task_status` (a status's short name),
  `entity.type` (`Shot`, `Sequence`, or an asset type such as `Prop`),
  `entity.name`, `sequence` and `episode`. Any other key is a task column,
  compared as Kitsu compares it. Milestones can cover `entity.name` (the
  shot) and `task_type`.
- **Publishing.** Each output is a comment that moves the task to review,
  carrying the output view's C4 ID, the job and its provenance, with a
  still of the output (the middle frame's preview proxy) as the comment's
  preview: a new revision, which Kitsu plays in review and playlists. A
  publish interrupted between the comment and the upload is finished on
  the next pass, so no revision is left processing. When the HUD cannot
  give the still (its leader is away), the revision waits for it up to
  five minutes, then goes up without one.
- **Decisions.** A person's comment that moves the task out of review is
  the decision. Its text, with the notes and replies written on the task
  since the review opened, is the decision's note, and with
  `rework: nextVersion` the next version's input.
- **The ETA and link** go to the task's data under `scope.eta_field` and
  `scope.link_field`. Kitsu shows them where a production manager adds
  Task metadata descriptors with those names.
- **Bids.** A task's `estimation` is in minutes; a forecast rule sizes by
  it (`forecast: {field: estimation, perUnit: "0.05"}`).
- **Text.** Kitsu posts a comment again on a shot's other tasks when it
  says `#all` or `#<task type>`; c4-sync's comments never do (a number
  sign before a letter is written full width). A mention is
  `@First Last` followed by a space, which c4-sync writes from the email
  in `people`.
- **Checking a server.** `KITSU_LIVE=<url> KITSU_LIVE_ADMIN=<email>:<password>
  go test -run TestLive ./internal/tracker/kitsu/` runs the adapter against
  a throwaway Kitsu (CGWire's `cgwire/cgwire` image runs one locally). It
  makes a production of its own there; never point it at a studio's.

## Limits

- Experimental API. The jobs c4-sync submits carry their tracker
  entity (`c4.avalanche.io/tracker-ref`) and the version they follow
  (`spec.follows`), both in their records. A decision relayed from a
  tracker is signed as `relayed` (or `relayed-party`, by a party
  such as a model registry), naming the tracker, the entity, the tracker
  user and the stored evidence; the HUD checks it against the Tracker's
  `people`.
- One c4-sync replica: there is no leader election yet.
- Tracker request limits are those each vendor publishes (GitHub's,
  Linear's 2,500 requests an hour per API key), or a conservative default
  (`rate`) where none are published (Flow, Kitsu).
- Linear: work comes in by label and state. Delegating an issue to an app
  user (Linear's agent sessions) and @-mentions in comments are not yet
  used, and project milestones do not yet become Milestones.
