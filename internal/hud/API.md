# HUD API, v1

The HUD web app (`internal/hud/web`) reads this API and nothing else. The real
backend (`internal/hud/server`, run by the manager) and the mock
(`cmd/hudmock` in the c4-hud repository) both implement it, from the same Go types
(`hud/api`). The MCP server (`internal/mcpserver`) is a client of it. DESIGN.md section 10 describes the HUD; this file is the wire
contract.

All paths are under `/api/v1`. Responses are JSON unless noted.

## Conventions

- **Times** are RFC 3339 strings in UTC, for example
  `2026-10-23T18:36:24Z`. A farm can span time zones; the system runs on
  UTC and the presentation normalizes. The HUD shows each time in the
  viewer's local time (the browser's zone, or `?tz=` for captures), with
  UTC smaller beside it where a time is the subject (clocks, the time base,
  deadline labels); a site shows its own local time where it is the
  subject. Sentences the server writes (estimate messages, control
  confirmations) give times in the farm's zone with UTC beside them; the
  HUD reads structured time fields, never times inside sentences.
- **Durations** are numbers of seconds.
- **Bytes** are numbers of bytes.
- **Unit sets** are run-length index strings: `"0-99,102,140-141"`. The empty
  string is the empty set.
- **Jobs** are named `namespace/name` wherever one job refers to another
  (`dependsOn`, `fromJob`, `capacity[].jobs`, graph ids, node `running`).
- **C4 IDs** are 90-character strings beginning `c4`.
- **Optional fields** are marked `?` and are omitted when they do not apply.

### Unit indexes

A unit's index is stable for the life of the job. Indexes that appear in no
`unitStates` set do not exist yet. Drivers that know a generator's fan-out
reserve the index range of its children when the job is planned, so index
order is layout order (render: pass-major, frame order; a generator's
children land in their pass's range at the generator's frame). The queue's
miniature job view draws units in index order from `unitStates` alone, so this
keeps it legible.

### Viewer and scope

The real backend identifies the viewer from a bearer token (TokenReview), or
from `X-Forwarded-User` and `X-Forwarded-Groups` set by an SSO proxy in front
of it (`--hud-trust-proxy`). The proxy proves itself on every request with a
shared secret in `X-C4-Proxy-Secret` (`--hud-proxy-secret-file`). A request
that names a viewer in those headers without the secret gets 401. A token
whose audience is `c4.avalanche.io/hud-api` is reviewed at that audience.
Mint one with `kubectl create token NAME --audience c4.avalanche.io/hud-api`,
or project it into a pod. The API server refuses such a token, so a token
sent to the HUD is no use anywhere else. Any other token is reviewed at the
API server's own audience. A viewer sees the projects with a namespace where
it may list WorkJobs (SubjectAccessReview, cached for a minute). The mock, and
the real backend started with `--hud-dev-viewers` (local farms, tests and
demos), take `?as=artist` or `?as=wrangler` on any request instead, including
the event stream and object URLs, but on the real backend only with this
machine's installer token (see "Development viewers and the installer
token" below). An `?as=` that names no configured viewer gets 400
naming the ones there are. A development viewer
has no RBAC behind it, so its role's rights stand in:
- every role but facilities may change jobs and mounts in the projects it
  sees;
- producers, wranglers and farm admins approve bursts and grant
  submissions;
- facilities, wranglers and farm admins disable nodes.

A development viewer's control records say "development viewer". On the
installer's secret (the token or its cookie) they are claimed by the
installer, graded `claimed`, and authenticated as "installer's secret on
this machine (claimed)". With open development viewers they are
`unattributed`.

### Development viewers and the installer token

A local farm's manager runs with `--hud-dev-viewers`, and the farm's own
pods, a unit's among them, reach the HUD through its Service. So a
development viewer is honoured only for a request that carries this
machine's installer secret:

- `X-C4-Installer: <token>`, the token in the manager's
  `--hud-installer-token-file` (hex; a trailing newline is ignored), or
- this farm's installer cookie, which a browser gets for the token through
  a single-use code (below), or
- anything, when the manager runs with `--hud-dev-viewers-open`: anyone who
  reaches the HUD is any viewer (development and tests only; the e2e and
  demo harnesses use it).

The manager refuses to start with `--hud-dev-viewers` and neither
`--hud-installer-token-file` nor `--hud-dev-viewers-open`. A new token
takes a restart.

Without the secret, a request with `?as=` gets `401` "development viewers
need this machine's installer token: run c4 farm open". A request without
`?as=` signs in as on a farm with no development viewers (a bearer token,
a unit's token, the SSO proxy), and is refused if it has none: it is
never the wrangler by default.

| route                          | answer                                                                                                                                                                                                                                                            |
| ------------------------------ | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `POST /installer/code`         | `{ code }`: a random single-use code, good for two minutes, at most a few outstanding. Needs `X-C4-Installer`; the cookie is not enough (`401`)                                                                                                                   |
| `GET /installer?code=C&next=N` | (not under `/api/v1`) a good, unused code sets the cookie and answers `303` to `N`, a path on this HUD (`#/` route kept), or to `/` for anything else (`//host`, `/\host`, a URL); a bad, used or expired code answers `401` and says to run `c4 farm open` again |

The cookie is `c4_installer_<id>`, `<id>` the first 8 hex digits of the
token's SHA-256, so two farms on one machine keep theirs apart; the HUD
reads no other. Its value is HMAC-SHA256 of "c4 installer cookie" keyed
with the token, never the token, so a new token voids it. It is
`HttpOnly`, `SameSite=Strict`, `Path=/`, kept a year.

A write (any method but GET, HEAD and OPTIONS) that the cookie alone
authorizes must also carry `X-C4-HUD: 1` (or `X-C4-Installer`), else
`403` "a write from the browser needs the HUD's own page". Another site's
page cannot send a custom header without a CORS preflight, and the HUD
answers none: it sends no `Access-Control-Allow-*` header anywhere.

On a HUD with an installer token, `--single-person-farm` and a farm
binding's `onePerson` hold; on one without, neither does (with open
development viewers, only the flag, for tests).

hudmock is a separate program and keeps open ?as= viewers for stills and
tutorials.

`viewer.role` sets the mode and level a new viewer starts at: `artist`,
`supervisor`, `producer`, `wrangler`, `farm-admin`, `facilities` or
`pipeline-td`. The real backend reads it from the viewer's groups: a group
the operator's `--hud-role-groups group=role,...` names (the studio's own
group names), else a group named `c4:role:<role>` or `c4-role-<role>`;
a development viewer named after a
role has that role, and `name@role=projects` gives one. Without a role the
HUD guesses: wrangler for a viewer of every project, else artist.

`viewer.solo` is true when the farm is this one person's: a local farm's
installer (the operator's `--single-person-farm`). The HUD then opens on
what needs them (`GET /needs-me`).

Every response is filtered on the server to the viewer's projects:

- jobs, units, logs, previews, objects, graph nodes and capacity requests of
  other projects are not returned;
- a job, unit or object outside the viewer's projects answers `404`, not
  `403`, so its name does not leak;
- farm totals stay farm-wide, and `farm.otherProjectsSlots` is the slots
  other projects use;
- nodes are shared infrastructure: every node is listed with its health, but
  `running` lists only units of the viewer's projects.

## GET /snapshot

The farm at a glance. Also delivered by the event stream.

```
{
  now,
  viewer: { user, projects[], allProjects: bool, role? },
  farm: { slotsUsed, slotsTotal, queuedUnits, unitsPerHour, failuresLastHour,
          burstSlots, burstCostPerHour,
          bytesMaterialized, bytesFetchedFromStore, bytesAvoided, otherProjectsSlots },
  pools: [ { name, attrs: { class, gpuModel?, vram?, capacityType }, slots, used, usedOutside?, costPerSlotHour,
             burst?: { maxSlots, provisioningSeconds, costPerSlotHour, requested, provisioned } } ],
  classes: [ { name, driver, slo: { type, p50Seconds?, p95Seconds?, turnaroundSeconds? }, lens } ],
  projects: [ { name, root, quotas: [ { pool, nominal, borrowingLimit, used } ],
                burstSpendToday, burstBudgetPerDay, bytesAvoided,
                workerHours: [ { pool, class, hours } ], gpuHours } ],
  jobs: [ JobSummary ],
  nodes: [ { name, pool, locality, zone?, region?, state, slots, used, usedOutside?, failuresLastHour, suspect,
             cacheBytes, cacheHitRate, running?: [ { job, unit, slots } ],
             capped?, shapeSlots?, capacityReason? } ],
  sites?: [ { name, label, region, lat, lon, kind, localities[], tz? } ],
  gates?: [ { namespace, name, holder?: { job, unit, name?, since?, expectStart? }, waiting: [ ... ] } ],
  incidents: [ { id, needsYou, title, detail?, count, countOf?, jobs: [ "ns/name" ], since, state,
                 checklist: [ { label, act?, href?, action?, job?, units?, done? } ] } ],
  observed?: [ { session, name?, kind, running, word, needsYou?, cwd?, startedAt?, lastActive?, turnAge?,
                 backgroundOpen, usage?, limit?, transcript?, onFarm, turnsTotal?, turn?, turnLimit?, parent?,
                 parentClaimed?, description?, waitsOn?, agentType?, model?, depth?, workersDone?, account?, weighted?,
                 machine, node, owners[], observedAt, postedAt } ],
  tiers: { tiers: [ { name, hitRate, requestsIn, upstreamRequests, bytesServed, shedRate, inflightFills } ],
           fanIn: { requesters, nodeRequests, peerRequests, regionalRequests, storeRequests },
           publishP50, publishP95, captureP50 },
  capacity: [ { pool, slots, until, reason, jobs[], estCost, state, prewarmViews[] } ],
  usage: { clusterHours,
           containerHours: [ { project, pool, hours } ],
           gpuHours: [ { project, hours } ],
           productionWindows: [ { project, name, from, to, active } ] }
}
```

A node is `capped` when a limit lowered it below its pool's shape: a host
runner's power cap, through the node's `c4.avalanche.io/slot-limit` label.
`slots` is then what it offers now, `shapeSlots` what its pool gives it, and
`capacityReason` says why in plain words ("battery 21% and falling on a
45 W adapter"). The HUD draws the unusable slots as blocked.

| field                     | meaning                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                       |
| ------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `farm.slotsTotal`         | slots on ready nodes, including disabled ones (they still hold running units)                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                 |
| `farm.bytesAvoided`       | `bytesMaterialized - bytesFetchedFromStore`, today (DESIGN 11.1); the savings proof, shown as savings and never billed                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                        |
| `projects[].root`         | where the project's trees mount in its units (Project `spec.root`, `/show` unset): a submission's `tree` and the jobs it follows mount there                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                  |
| `classes[].slo.type`      | `deadline`, `latency` or `throughput`                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                         |
| `classes[].lens`          | the name of the HUD lens module for the class (`render`, `ci`, `ml`; anything else gets the generic lens)                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                     |
| `nodes[].state`           | `idle`, `busy`, `disabled`, `notReady`, `unprovisioned`, `warming`                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                            |
| `nodes[].suspect`         | node faults across two or more jobs in the last hour; test and job failures do not count                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                      |
| `nodes[].zone`            | the node's `topology.kubernetes.io/zone` label (an on-prem farm labels racks or rooms here)                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                   |
| `nodes[].region`          | the node's `topology.kubernetes.io/region` label; it joins the node to a site                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                 |
| `gates`                   | each gate (a one-slot resource a namespace's jobs pass first come, first served, such as a merge gate): its holder and its line in order, each with when it joined and when it is expected to pass                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                            |
| `incidents`               | what needs attention, by root cause: one item per cause with its symptoms folded in (FAA AC 25.1322-1 ¶8d), needs before what the farm handles, most work first. Failures after their retries are grouped by cause across jobs (the message's cause, numbers blurred); a decision or an escalated review is one item per job, `needsYou` only when it is the viewer's or a party's; anything else a job needs a person for is one per job; machines that stopped reporting in the last hour are grouped by rack (`topology.kubernetes.io/rack` or `c4.avalanche.io/rack`, else the name less its number) with the units they interrupted, `needsYou` false. `state` is `open`, or `acknowledged` once every job in it has its failed units taken or assigned. `checklist` is ordered: `href` a HUD route, or `action` a job control on `job` for `units`; `done` from the job's acks since the incident began |
| `sites`                   | where the farm's nodes are, for the map; omitted when none are declared, and the HUD then shows sites and racks only                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                          |
| `sites[].region`          | the region the site's nodes carry in `nodes[].region`                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                         |
| `sites[].kind`            | `onprem` (a studio machine room) or `cloud` (a provider region)                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                               |
| `sites[].localities`      | the `nodes[].locality` values at the site (racks, zones), in display order                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                    |
| `sites[].lat`, `.lon`     | the site's position in degrees, WGS 84                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                        |
| `sites[].tz`              | the site's time zone, an IANA name (`America/New_York`), so its clock shows its local time; absent shows the viewer's                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                         |
| `tiers.tiers[].name`      | `node`, `peer`, `regional`, `store`, in fetch order                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                           |
| `tiers.fanIn`             | requests per minute entering each tier; each tier forwards only its misses                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                    |
| `capacity[].state`        | `PendingApproval`, `Provisioning`, `Provisioned`                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                              |
| `capacity[].prewarmViews` | views the new nodes fetch before units land on them                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                           |
| `usage`                   | metered use today: what billing is based on. Billing is never by bytes                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                        |
| `usage.clusterHours`      | hours the installation has run today (farm-wide, shown to every viewer)                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                       |
| `usage.containerHours`    | pod run time per project and pool, not weighted by slots; only the viewer's projects                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                          |
| `usage.gpuHours`          | GPU-hours per project; only the viewer's projects                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                             |
| `usage.productionWindows` | declared fixed-fee periods (for example "Show A production window, Oct 1 to Dec 20"); `active` if `now` is inside it                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                          |

`jobs` is in dispatch order, and `rank` is the position in that order within
the viewer's scope (1 is next).

### JobSummary

```
{
  namespace, name, project, class, driver, owner?, trackerRef?, trackerURL?, note?, phase, order,
  units: { total, unevaluated, queued, blocked, running, retrying, succeeded, failed, held,
           preflight?, backoff?, egress?, accepted?, waits? },
  unitStates: { unevaluated, queued, blocked, running, retrying, succeeded, failed,
                preflight?, backoff?, egress?, accepted?, waits? },
  objective: { type, deadline?, latencyTargetSeconds?, from?, source?, mode?,
               suggestion?: { deadline, state, policy, quantile, revisions, maxRevisions, nextSession?, atRisk?,
                              history: [ { at, deadline, unitsMeasured, record?, reason? } ] } },
  estimate: { remainingSeconds, p90RemainingSeconds, etaP50, etaP80?, etaP90, etaP95?, noEta?, calibrated,
              requiredSlots, attainableSlots, runningSlots, slackSeconds, feasible,
              message?, burst?: { pool, slots, cost, needsApproval }, risk? },
  rank, pools[],
  inputs: [ { materialization?, view?, fromJob? } ],
  output?: { view, changesets },
  dependsOn[], createdAt,
  nudge: { hold, boost },
  heavyRegions?: [ { from, to, factor, label? } ],
  blacklist?: [ nodeName ],
  shot?,
  work: { doneSlotSeconds, runningSlotSeconds, runningRemainingSlotSeconds,
          remainingSlotSeconds, p90RemainingSlotSeconds, basis,
          doneFraction?, deliveredFraction? },
  needsPerson?: { reason, message, since },
  noNetwork?,
  waits?: [ { unit, name?, waitsFor, ask?, choices?, reply?, block?, escalated?, since?, expectSeconds?, blocking?, needsYou?,
             do?, note?, actor?, at?, record?, view? } ],
  rounds?: [ { unit, name?, round, max, until?, stoppedBy?, generators?, expectMore?, expectSeconds? } ],
  steers?: [ { unit, name?, seq, actor, at, text, record?, delivered?, seen?, ask?, answer? } ],
  needs?: [ { unit, name?, text, why?, at, record?, state, raisedAt, answerBy?, pressingAt?, pressingWhy?, pressingRecord?, about?,
             rechecks?: { done, max, usedUp?, lastAt?, lastWhy?, inFlight }, supplied?, by?, suppliedAt?, suppliedRecord?,
             withdrawn?: { by, why, at, record? } } ],
  asks?: [ { id, from, to, text, at, answer?, cannot?, answered?, wait?, needs? } ],
  branches?: [ { unit, name?, repo, branch, base, head?, commits } ],
  acks?: [ { kind, units, actor, assignee?, note?, at, record? } ],
  observed?, label?, account?, turnsTotal?
}
```

| field                                 | meaning                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                        |
| ------------------------------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `phase`                               | `Queued`, `Running`, `Blocked` (waiting on `dependsOn`), `Held` (by a person, or on an open question once nothing else runs), `Yielded` (a wrapped run that yielded), `Succeeded`, `Failed`; `Forecast` for a forecast job (`spec.forecast`), which never runs and is estimated among the real work by a separate pass                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                         |
| `order`                               | `binary`, `sequential`, `reverse`, `failure-rate` (CI), `gang` (wrapped ML)                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                    |
| `units.held`                          | waiting units of a held job; they are not also counted in `queued`                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                             |
| `units.planned`, `unitStates.planned` | generators not evaluated yet (also `unevaluated`, the older name, with the same value)                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                         |
| `units.locked`, `unitStates.locked`   | units waiting for a unit of the job that holds what they claim ("locked by")                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                   |
| `units.flagged`, `unitStates.flagged` | units a review escalated to a person, waiting on them (`needsPerson.reason` `review`)                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                          |
| `unitStates`                          | has no `held` key: a held job's waiting units are in `queued`, and the client draws them as held when `nudge.hold` is set                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                      |
| `unitStates.running`                  | includes a generator while it is being evaluated                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                               |
| `unitStates` phases                   | each unit is in one set: `planned` (a generator not evaluated yet), `queued` or `blocked` (waiting on its job's dependencies or on units of its own job); `preflight` (its pod prepares: placement, inputs, cache, image, license); `running` (the first attempt works); `retrying` (a later attempt works); `backoff` (an attempt failed, the retry waits for a slot); `egress` (the work is done and its output is being captured and added); `succeeded` (added and recorded); `failed` (failed after its retries: a person must act); `needs` (stopped on an open question, `needs`: not failed, and the job is `Held` once nothing else runs); `flagged` (a review escalated it to a person: requeue, take or accept it); `accepted` (failed, and a person accepted it); `superseded` (a review chose others: done, not accepted, out of the output). `units` counts the same sets                                                                        |
| `estimate.risk`                       | the deadline risk shown: `none`; `atRisk` (the band at the job's quantile, 0.95 for a suggested deadline and 0.9 otherwise, ends after the deadline, still fixable); `willMiss` (late even with every slot it can get); `missed` (the deadline has passed, unfinished). Finished jobs are `none`                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                               |
| `needsPerson`                         | set while the job waits on a person: `reason` is `failed` (failed units nobody has taken), `stuck` (no plan, no pool fits, a dependency failed, its image cannot be pulled), `approval` (burst over the project's budget waits for approval; the message names the slots, the pool, the cost and who approves it), `review` (a review escalated units to a person: they are `flagged`), `decision` (a wait unit waits for a person's decision), `measure` (its units finished short of its required measure), `quota` (the namespace's quota is used up) or `permission` (the API server refuses its units' pods: Pod Security, a webhook, an admission policy; or they wait for a credential lease); `since` is when it began. The job's conditions `Admitted`, `Images` and `Lease` (False while they hold) carry the same cause with what to do, and an agent bundle carries them too, not Ready. `c4 farm ls` shows such a job as `stuck` with its reason, not as ready                                                                                   |
| `noNetwork` | set once a unit of a job that names no tool has run: "this job names no tool, so it has no network: name a tool that declares one" (the job's `Network` condition, reason `NoTool`). `c4 farm ls` adds it to a `held` line only while the job waits on a person for failed units with nothing running, not for every job with no tool |
| `measure`                             | the job's own measure of done, when it declares one (`spec.measure`): `{ name, value, of, done, at?, detail?, required? }`, from the latest reading a unit reported; `required` makes it the job's completion predicate                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                        |
| `trackerRef`                          | the tracker entity the job serves (`<tracker>/<type>/<id>`), when a tracker's sync submitted it                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                |
| `trackerURL`                          | where a person opens that entity, as the tracker links it: http(s), or a path on the HUD's own host (a development farm's stand-in)                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                            |
| `forecast`                            | a forecast job's size (`spec.forecast`): `{ slotHours?, sd?, unitHours?, start?, exists? }`, numbers as decimal strings; absent on a real job                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                  |
| `modelShare`                          | the share of the job's work, slot time over every attempt, that model units did, 0 to 1 ("models did N% of the work"); absent before any unit ran. `spend.modelShare` is their share of the cost                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                               |
| `spend`                               | what the job cost so far, split by what did each unit's work (`reasoning.does`: `procedure` or `model`): `{ modelUnits, proceduralUnits, modelTokens, unpricedTokens?, inputTokens?, outputTokens?, cacheReadTokens?, cacheWriteTokens?, weightedTokens?, unpricedWeightedTokens?, tokenCost, estimatedTokenCost?, modelSlotCost, proceduralSlotCost, modelShare, modelShareBasis? }` in US dollars. The token classes are raw counts; `weightedTokens` weighs them by price into input-equivalents (output 5, cache writes 1.25, cache reads 0.1, or the model's rate: DESIGN 10.6). Tokens reported without a price are in `unpricedTokens` (raw) and `unpricedWeightedTokens`, and are estimated at the model's published rates in `estimatedTokenCost`. `modelShare` is model units' part of the spend, 0 to 1, counting that estimate; `modelShareBasis` is `priced`, `estimated`, or `partial` (some tokens of a model with no known price are left out) |
| `acks`                                | who took (`acknowledge`), was given (`assign`, with `assignee`) or accepted (`accept`) which failed units, and when; a requeue clears a unit's                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                 |
| `waits`                               | what the job waits for ("waits for"): its wait units (`waitsFor` is `person:<id>`, `party:<name>` or `clock:<time>`; `ask`, `choices`; `reply` when the decision must carry content), waits a running unit opened for itself at a safe point (`block`: it resumes once decided) and units a review escalated (`escalated`), open (`since`, `expectSeconds` from the server's measured decisions, `blocking` when unfinished work waits on it, `needsYou` for a person's open wait that holds up work or an escalated unit) or decided (`do`, `note`, `actor`, `at`, `record`, and `view`, the reply). A wait holds no slot; its unit is in `unitStates.waits`                                                                                                                                                                                                                                                                                                  |
| `rounds`                              | the job's loops (DESIGN 10.6, rounds): a generator run as bounded rounds of siblings, round k's units named `r<k>/<name>` and children of `generators[k-1]`. `round` is the round running or last run, `max` the bound, `until` the review whose pass ends the loop; `stoppedBy` (`until`, `generator`, `max` or `failed`) once it ended; while it goes on, `expectMore` rounds expected, the pending one included, taking `expectSeconds` on the critical path. Draw one row per round ("round 3 of at most 20"), finished rounds folded, the current one open, the expected rest as one dashed row ("about 1.6 more rounds")                                                                                                                                                                                                                                                                                                                                 |
| `steers`                              | the latest messages sent to the job's units while they ran (DESIGN 10.6, steering): `seq` numbers a unit's messages, `delivered` is when one reached a pod of the unit, `seen` that the unit read it                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                           |
| `needs`                               | the units that stopped because their brief lacked information only a person could give (an error that can heal, not a question): what each asked for, why and what about. `state` is `raised` (the rest of the job runs on), `pressing` (urgent: `pressingWhy` says why, from the plan or its age), `supplied` (`supplied` is the text, `by` who: a person or `unit:<ns>/<job>/<unit>`, the job's answerer) or `withdrawn` (`withdrawn.by` who, and why: by default the asking unit, on a re-check, or one of the people who may control the job). `answerBy` is the latest an answer can come with the job on time. `rechecks` (model units only) are the block's re-checks of its question, `usedUp` once all have run. An open one is an incident (`needsinformation`, `severity` warning, urgent once pressing) whose steps supply or withdraw it                                                                                                          |
| `branches`                            | the git branches the job's blocks worked on, one per unit, from `status.unitBranches`: a block on a host works in its own worktree of the developer's repository (`repo`, on the machine that ran it), on `branch` from `base` to `head`, and its `commits` are its output. The latest attempt's record is kept, and stays after the unit's pods are gone, so `c4 farm get` prints it. A unit that ran in a container has none yet                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                             |
| `causes`                              | the job's failures and flags grouped by cause, newest last (status.causes): `{ kind, key, title, units, count, sample, first, last, nodes?, did?, to? }`. `kind` is `exit` (a unit's code, reason and first message line), `node` (a node that failed units up to the job's limit, kept off it), `verdict` (a review's reason) or `needs`; `did` is what the farm did (`retried`, `failed`, `blacklisted`, `reworked`, `escalated`, `accepted`). Failed jobs' incidents are grouped by the latest cause whose units failed                                                                                                                                                                                                                                                                                                                                                                                                                                     |
| `asks`                                | the latest questions the job's units asked one another (`c4-unit ask`; `id` is `<unit>#<n>`, or `w<unit>` for a wait unit served by a unit), from and to unit indexes (`to` -1 when none could answer), and the `answer` or why it `cannot` be answered, once `answered`                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                       |
| `objective.deadline`                  | the effective deadline: the job's own, or one propagated from a dependent, or ready time plus the latency target                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                               |
| `objective.from`                      | the dependent job whose deadline binds, when the deadline is inherited                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                         |
| `objective.source`                    | where the deadline comes from: `spec` (a person, a tool or tracking), `milestone` (the earliest Milestone covering the job, named in `objective.milestone`), `suggested` (the farm) or `class` (the class's objective)                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                         |
| `objective.mode`                      | who owns the deadline: `Pinned`, `Suggested` or `Tracking` (DESIGN 10.7)                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                       |
| `objective.suggestion`                | the farm's suggested deadline, kept for reference also once a person pins another                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                              |
| `suggestion.state`                    | `Estimating` (it may still move later, only if at risk) or `Settled`; the farm never moves a suggestion earlier, and a settled one not at all                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                  |
| `suggestion.policy`                   | `achievable`, `sessions` or `turnaround`; `quantile` is the finish percentile it meets, 0.95 by default: suggestions are generous, and tightening one is the person's choice (`deadline-options`)                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                              |
| `suggestion.atRisk`                   | the finish at `quantile` is now after the suggestion (shown in orchid)                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                         |
| `suggestion.history[].reason`         | `first`, or `at-risk` for a later move the farm made because the suggestion was at risk                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                        |
| `suggestion.nextSession`              | for a sessions policy, the following session when the job is not expected to make its own: the one-click alternative                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                           |
| `suggestion.history[].record`         | the C4 ID of the queue's signed record of that value                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                           |
| `estimate.etaP50` to `etaP95`         | the finish band a person is shown: ready by then with probability 50, 80, 90 and 95%                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                           |
| `estimate.noEta`                      | when set, the job has no ETA and this says why in plain words (held, suspended, waiting for capacity, no deadline); `etaP50` and `etaP90` then carry the snapshot's time, so show the reason, not a time                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                       |
| `estimate.calibrated`                 | the band comes from finished jobs' outcomes (live calibration), not the estimator's own spread                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                 |
| `estimate.remainingSeconds`           | wall-clock seconds from `now` to `etaP50`                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                      |
| `estimate.p90RemainingSeconds`        | wall-clock seconds from `now` to `etaP90`                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                      |
| `estimate.requiredSlots`              | `W90 / T` (DESIGN 9.9)                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                         |
| `estimate.slackSeconds`               | the deadline as planned (the job's own, or the earlier one a dependent sets) less the shown `etaP90` (`etaP95` for a suggested deadline); negative when the shown finish is late; absent without a band. `feasible` keeps the all-capacity meaning                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                             |
| `estimate.message`                    | the feasibility warning in production's terms; also set for held and failed jobs                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                               |
| `estimate.burst`                      | the smallest burst that restores feasibility, its estimated cost, and whether a person must approve it                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                         |
| `output.changesets`                   | the number of unit changesets folded into the output view so far                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                               |
| `heavyRegions[].from`, `.to`          | axis values (frame numbers for render), inclusive                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                              |
| `heavyRegions[].factor`               | the region's mean estimated cost over the job median                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                           |
| `blacklist`                           | nodes the job will not use, added automatically after `nodeErrorLimit` failures or by a person                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                 |
| `shot`                                | the job's `c4.avalanche.io/shot` label; the HUD draws a shot's jobs (sim, FX, comp, transcode) as one sheet                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                    |
| `work`                                | progress weighted by work, in slot-seconds (seconds times the slots a unit holds); see below                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                   |

#### Work-weighted progress

Unit counts treat a 40-second frame and a 40-minute hero frame alike.
`work` weighs them:

| field                              | meaning                                                                                                                                           |
| ---------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------- |
| `work.doneSlotSeconds`             | measured run time of the succeeded units, times their slots                                                                                       |
| `work.runningSlotSeconds`          | time the running units have run so far, times their slots                                                                                         |
| `work.runningRemainingSlotSeconds` | the estimator's expected remainder of the running units (past its estimate, a running unit needs a quarter of its elapsed time again)             |
| `work.remainingSlotSeconds`        | `W`: everything still to do, that is waiting and blocked units, the remainder of running units, and the expected fan-out of unexpanded generators |
| `work.p90RemainingSlotSeconds`     | `W90 = W + 1.28 σ` (DESIGN 10.8), the figure the scheduler plans on                                                                               |
| `work.basis`                       | `estimate`, or `units` when the job has no estimate yet                                                                                           |
| `work.doneFraction`                | the share done, counting units whose output is still moving (`egress`): `done / (done + running + remaining)`; 1 for a succeeded job              |
| `work.deliveredFraction`           | the share whose output is added and recorded (`succeeded` only)                                                                                   |

`remainingSlotSeconds` includes `runningRemainingSlotSeconds`, so the job's
expected total is `done + running + remaining`, and a bar fills as four
segments: done, running spent, running remainder, and the rest
(`remaining - runningRemaining`); `p90Remaining - remaining` is the band
past it. Failed units waiting for a person are in none of the fields.
`doneSlotSeconds` includes the units in `egress`.

`doneFraction` and `deliveredFraction` never go down: the server keeps each
job's highest, and lowers them only when finished units are requeued (fewer
units succeeded or moving than before). A remainder estimate that grows
shows in the time left, never as done work lost. The client also keeps its
own high-water mark per job, so an out-of-order snapshot cannot lower them.

With `basis: "units"` the fields carry unit counts as weights: `done` is
the succeeded units, `running` the running units, `runningRemaining` 0,
and `remaining` and `p90Remaining` every other unit except failed ones. The
client draws the same fill and marks it as counted in units.

In the real backend, `done` sums the job's measured unit times from its
status, `running` comes from the unit pods' start times, and `remaining`
and `p90Remaining` are the queue's `W` and `W90` from the job's status. The
snapshot approximates `runningRemaining` from the job's mean measured unit
time (capped at `remaining`), so a snapshot never rebuilds an estimator; the
job detail computes it with the job's estimator.

## GET /jobs/{ns}/{name}

A JobSummary with these fields added:

```
{
  ...JobSummary,
  axis: { kind, start?, step?, labels?, batch? },
  groups: [ { key, label, units, generator?, spans?, parent? } ],
  unitList?: [ { i, label, state, attempts, node?, group, pos, risk?,
                 name?, reasoning?, usage?, reused?, parent?, exists?, blockedOn?, claims?, lockedBy?, review?, safepoint?, stopping?, branch?,
                 work: { spent, expected }, reads?: [ { job, units } ], eta? } ],
  riskUnits,
  progress: { "<i>": { fraction, measured } },
  costByFrame?: [ { frame, seconds?, estimate?, low?, high? } ],
  expansions?: [ { generator, children, at, document, error? } ],
  edges?: [ { from, to } ],
  holds?: [ { holder, name, on, waiting: [ { unit, name, on } ] } ],
  interruptions?: [ { unit, at, reason, node? } ],
  interrupted?,
  fresh?,
  classSummary
}
```

| field                  | meaning                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                         |
| ---------------------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `axis.kind`            | `frame`, `shard`, `rank`, `step`, `segment`, `package`; `start` and `step` are set for frames                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                   |
| `groups`               | rows of the job view: passes (render), stages (CI), ranks (ML); `units` is the group's unit set                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                 |
| `groups[].generator`   | the group holds generators                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                      |
| `groups[].spans`       | for a generator group, the keys of the groups its children land in; an unexpanded generator is drawn across those rows                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                          |
| `unitList`             | one entry per existing unit; omitted above 5,000 units, when the client lays units out from `groups` (the k-th member is at position k)                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                         |
| `unitList[].state`     | a `unitStates` key, or `held`                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                   |
| `unitList[].preflight` | for a unit in `preflight`: `{ step, bytes?, of? }`; `step` is `place` (waiting for a node), `stage` (inputs), `fetch` (cache), `image`, `license` or `start`. The node daemon and class drivers report steps in the pod's `c4.avalanche.io/preflight` annotation; otherwise the step comes from the pod's status                                                                                                                                                                                                                                                                                                                                                |
| `unitList[].egress`    | for a unit in `egress`: `{ bytes?, of?, sealed, folded, verified }`; `sealed` once the output is captured (it has a C4 ID); the bytes come from the pod's `c4.avalanche.io/egress` annotation                                                                                                                                                                                                                                                                                                                                                                                                                                                                   |
| `unitList[].branch`    | the unit's git branch, as in the snapshot's `branches`: `{ repo, branch, base, head, commits }`                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                 |
| `unitList[].ack`       | for a failed unit someone took: `{ kind, actor, assignee?, note?, at, record? }`                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                |
| `unitList[].accepted`  | for an accepted unit: who accepted it and when, the same shape                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                  |
| `unitList[].safepoint` | for a running unit that announces safe points (`c4-unit safepoint`): `{ seq, at, everySeconds?, next, decision? }`: its last one; once two are seen, the interval between them; `next`, what the queue would decide at the next one as things stand (`stop`, `continue` or `undecided`: a stop is asked but no longer held, as after a restart); and `decision`, the last answer, `{ seq, do, why, at }`                                                                                                                                                                                                                                                        |
| `unitList[].stopping`  | for a running unit asked to stop softly: `{ reason, by?, nextSafepoint? }`: why, the deadline after which it is stopped anyway, and when its next safe point is expected                                                                                                                                                                                                                                                                                                                                                                                                                                                                                        |
| `unitList[].attempts`  | the number of attempts so far. A yield and an interruption give their attempt back: a unit that yielded or was stopped by the farm runs its next attempt under the same number, so it gains no retry notch and is never shown in `backoff` or `retrying` for it                                                                                                                                                                                                                                                                                                                                                                                                 |
| `interruptions`        | the latest attempts the farm stopped (WorkJob status): `{ unit, at, reason, node? }`, reason the pod's DisruptionTarget reason or `Deleted`; `interrupted` counts them all. They spent no retry; the HUD draws the unit queued with an "interrupted" notch                                                                                                                                                                                                                                                                                                                                                                                                      |
| `unitList[].pos`       | position on the axis (frame offset, shard number, rank)                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                         |
| `unitList[].risk`      | a waiting unit the scheduler's projection finishes after the deadline                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                           |
| `riskUnits`            | the same as a unit set, available at any size                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                   |
| `progress`             | running units; `measured` is true when the unit reports progress itself, false when the fill is elapsed time over the estimate                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                  |
| `costByFrame`          | one entry per frame of the last pass: `seconds` when measured, else `estimate` with its band `low` to `high`                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                    |
| `expansions`           | generators that have been evaluated: `children` is a unit set, `at` a time, `document` the expansion document's C4 ID, `error?` why the queue refused it (it added nothing); a client that sees a new entry animates the split                                                                                                                                                                                                                                                                                                                                                                                                                                  |
| `edges`                | unit-level dependencies inside the job (beauty after the deep env and volumes layers; integration after the shards); omitted above 5,000 units                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                  |
| `axis.batch`           | frames per display batch: the render class's `chunk` when it is above 1, else the class fps (24, one second of screen time); frame axes only                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                    |
| `groups[].parent`      | the key of the generator group whose expansion creates this group's units (`batch` under `shot`, `usd` under `batch`)                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                           |
| `unitList[].parent`    | the index of the generator unit whose expansion created this unit; omitted for units the job planned                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                            |
| `unitList[].review`    | what a review last decided for the unit, when more than a plain pass: `{ do, why?, note?, to?, by, passes?, with?, score?, scored? }`: `rework` (with the change to its next attempts and how many reworks), `escalate` (to whom; the unit is `flagged`), `accept` (with its note; drawn pale), `supersede` (a review chose others: greyed with a strike), or `pass` after a rework or with a score. `score` is the unit's last score (a decimal), `scored` how many verdicts scored it; draw it on the unit's cell. A review unit carries `select`, its last selection rule ("top 1", "threshold 0.8", "ucb 1.4 top 1"), to name on its cell with the kept set |
| `unitList[].name`      | the unit's name within the job, as its expansion document gives it (`C3.2/phase-1`); edges and inputs use it                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                    |
| `unitList[].blockedOn` | for a `blocked` unit, the first unit of its own job it waits on that has not finished: its name, else its label ("blocked on <x>"); the full set is in `edges`                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                  |
| `unitList[].claims`    | what the unit claims (paths or resources it writes): its holds and claims                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                       |
| `unitList[].reasoning` | what the unit reasons with: `{ does, model?, effort?, tools?, observed? }`. The top level is declared (the expansion document's or plan's `reasoning`, `does` else the class's: `model` or `procedure`); `observed` is what the unit's latest attempt reported (`{ does?, model?, models?, effort?, efforts?, thinking?, tools?, toolNames?, mcpServers? }`), for model units                                                                                                                                                                                                                                                                                   |
| `unitList[].reused`    | a succeeded unit that ran nothing: an earlier run of the same computation (process and input views), exact and verified, gave its output; drawn committed green with the reuse mark. `units.reused` counts them (also in `succeeded`)                                                                                                                                                                                                                                                                                                                                                                                                                           |
| `unitList[].usage`     | a model unit's use of models over its attempts: `{ inputTokens, outputTokens, cacheReadTokens?, unpricedTokens?, cost, slotCost, costSource?, models: [ { model, inputTokens, outputTokens, cacheReadTokens?, unpricedTokens?, cost? } ], usageError? }`; `usageError` says why an attempt's usage document was refused (its tokens are not counted)                                                                                                                                                                                                                                                                                                            |
| `unitList[].lockedBy`  | for a `locked` unit, the unit of its job holding what it claims: `{ unit, name, on }` ("locked by <name>"), `on` the claim it waits for                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                         |
| `holds`                | each unit holding claims that others wait on: `{ holder, name, on, waiting: [ { unit, name, on } ] }`, the waiting line in dispatch order                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                       |
| `inputSubstitutes`     | inputs the job's units read, at a site, as a validated recompute's output: `[ { mount, view, substitute, site, recompute } ]` (`view` as the job names it, `substitute` what was read)                                                                                                                                                                                                                                                                                                                                                                                                                                                                          |
| `procedural`           | model units whose work a procedure might do: run at least three times on the same inputs, they wrote the same bytes every time: `[ { unit, name, runs, why } ]`, a report for a person                                                                                                                                                                                                                                                                                                                                                                                                                                                                          |
| `unitList[].gate`      | the gate a unit of kind gate passes (a one-slot resource shared by the namespace's jobs, first come, first served); while it waits, `lockedBy` is `{ gate, position, holder?, expectStart? }`, `holder` as `job#unit`                                                                                                                                                                                                                                                                                                                                                                                                                                           |
| `unitList[].exists`    | the probability a tentative unit runs, below 1 only; its expected work is weighted by it                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                        |
| `unitList[].work`      | the unit's work in slot-seconds: `spent` so far and `expected` in total (below)                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                 |
| `unitList[].reads`     | units of other jobs this unit reads, when that is not simply the same frame (a temporal comp reads frames n-1 to n+1); `units` is a unit set of that job                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                        |
| `unitList[].eta`       | the scheduler's projected finish of a waiting unit; the finish times of a job's waiting units are its isochrones                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                |
| `fresh`                | the job was made since the HUD last read the farm: it is read from the API server, with its plan and no pods yet, and the next read (within seconds) has the rest. A job just made is never answered 404; the same holds for its units, deadline options, custody, trace and book                                                                                                                                                                                                                                                                                                                                                                               |

A unit's `work`, in slot-seconds:

| unit                   | `spent`                                    | `expected`                                                            |
| ---------------------- | ------------------------------------------ | --------------------------------------------------------------------- |
| succeeded              | its measured run time                      | the same                                                              |
| running                | its elapsed time                           | elapsed plus the estimator's expected remainder                       |
| waiting, blocked, held | 0                                          | the estimator's mean for the unit                                     |
| unevaluated generator  | 0                                          | its own evaluation plus the expected work of the units it will reveal |
| failed                 | its last attempt's run time, when measured | the estimator's mean                                                  |

Above 5,000 units `unitList` is omitted, and clients weigh the job by its
summary's `work` instead of per unit.

Expansion may nest (DESIGN 10.6): a job script generator expands into
batch generators, each of which expands into per-frame generators, which
expand into passes. `groups[].parent` gives the levels, `groups[].spans`
lists every group its descendants land in, and `unitList[].parent` links
each unit to the generator that created it. The HUD groups a frame job's
units into batches of `axis.batch` frames when the job has no batch
generators of its own.

In the real backend, units a generator added carry `parent` and `name`;
`groups` are the `group` names their documents give, each with the
`generator` that made it when there is one. Pipelined
`fromJob` reads (DESIGN 10.5) are designed and not tracked per unit, so
`reads` is omitted. The queue's plan does not export per-unit finish times
yet, so `eta` is omitted.

A wrapped ML run's ranks keep a `progress` entry while it is yielded: the
fraction its latest checkpoint saved.

`classSummary` depends on the driver:

- **render**: `{ passes[], fps }`
- **ci**: the CI class within the viewer's scope (the same for every CI job):
  ```
  { changes: [ { id, title, author, pipelines: [ { job, stage, state, queueWaitSeconds, runSeconds,
                                                   checkoutSeconds, cacheOutcome, timeToGreenSeconds? } ] } ],
    ttg: { p50, p95, targetP50, targetP95, samples[] },
    flaky: [ { target, flips, lastSeen, views[] } ],
    mergeQueue: { depth, p50WaitSeconds, p95WaitSeconds, ejectRate, mergesPerHour,
                  entries: [ { change, position, state, speculativeBase } ] },
    runnerPools: [ { pool, used, slots } ] }
  ```
  `pipelines[].stage` is `build`, `test`, `integration` or `speculative` (the
  merge-queue run). `state` is `waiting`, `blocked`, `running`, `passed` or
  `failed`. `cacheOutcome` is `warm`, `delta` or `cold`. `mergeQueue.entries`
  lists waiting and testing entries with positions from 1, and recently merged
  or ejected entries with position 0.
- **ml**:
  ```
  { wrapped, ranks, trials?, checkpoints: [ { view, step, at, bytesNew, bytesDeduped } ],
    yields: [ { at, reason, resumedAt? } ], usefulGpuHours, lostGpuHours,
    datasetView, codeView, step, totalSteps }
  ```
- **agent**: the bundle that owns the job (an AgentBundle; its members are
  the job's units):
  ```
  { bundle, phase, cap?, running,
    members: [ { name, unit, kind, state, node?, epoch?, checkpoint?: { session, seq, id },
                 runs: [ { from, to? } ] } ],
    clock: { start?, virtualNow?, suspendedSince?, suspendedBy?, driftSeconds },
    suspended: [ { from, to?, by } ] }
  ```
  `kind` is `resident` or `task`. `state` is `running`; `yielding` (asked to
  stop at its next turn boundary); `suspended` (the queue stopped it at a
  turn boundary, and it resumes from `checkpoint`); `held` (a person
  suspended the bundle); `waiting` (not started); `done`; or `failed`.
  `epoch` is the member's lease number, and `checkpoint` is where it
  resumes: its Claude Code session, the checkpoint's sequence number, and
  `id`, the bare C4 ID of the transcript chain's head at that turn. `clock` is
  the bundle's time: it stops while the bundle is suspended.
  `virtualNow` is its value at the last status write, and `driftSeconds`
  is how far it trails the real clock. `suspended` lists the recent
  suspended intervals, oldest first; the last has no `to` while the bundle
  is still suspended. `by` is `queue` (the farm stopped every member at a
  turn boundary, and resumes them itself) or `person` (someone suspended
  the bundle, and a person resumes it). `phase` is `Running`, `Holding` or
  `Held` (by a person), `Suspended` (by the queue), or `Pending`. `runs` are the
  member's recent runs, oldest first, the current one without `to`. The
  real backend reports only the current run, from its unit pod; hudmock
  reports the scenario's history. `cap` is the bundle's limit on members running at
  once.

## GET /jobs/{ns}/{name}/units/{i}

```
{ i, label, state,
  attempts: [ { n, node, start, end?, exitCode?, reason? } ],
  estimateSeconds, durationSeconds?, logTail[], events: [ { type, reason, message, at } ],
  outputChangeset?, expandedInto? }
```

`durationSeconds` is the run time of the last attempt, or the elapsed time of
a running one. `expandedInto` is a unit set. `events[].type` is `Normal` or
`Warning`, as in Kubernetes Events.

## GET /jobs/{ns}/{name}/preview

```
{ fps, frames: [ { frame, url? } ] }
```

One entry per frame of the last pass, in frame order. `url` is set for
rendered frames and points at `/api/v1/objects/{project}/{c4id}`. The player
holds each rendered frame until the next rendered one.

## GET /jobs/{ns}/{name}/deadline-options[?at={time}[&at={time}...]]

What each candidate deadline would take, from what-if runs of the
scheduling policy on a copy of the queue's last plan. Nothing changes and
nothing is spent. `at` is an RFC 3339 time, given up to 8 times.

Suggested deadlines are generous (met 95% of the time by default) and the
farm never moves one earlier: tightening is the person's choice, and this
is where they make it. Without `at`, the queue offers the tighter choices
itself, each with a `label` (times in labels are local, with UTC alongside): the upcoming sessions of the job's project
(from its `sessions` deadline policies, three per calendar) before the
job's deadline, and the earliest time it makes at 80% without making
another job late. Each comes with its odds (`meets`) and what it would
displace.

```
{ job, now, policy, quantile, deadline?, band?: { p50, p80, p90, p95 }, achievable?, floor, floorWhy,
  options: [ { at, label?, band?: { p50, p80, p90, p95 }, meets, onTime, boost?,
               burst?: { slots: { pool: n }, pools[], cost, closes, automatic, why? },
               displaces, displaced[], unnamed, floor?, floorWhy? } ] }
```

| field                 | meaning                                                                                                                                                                                                                                                                                                                                         |
| --------------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `policy`, `quantile`  | the job's deadline policy and the finish percentile its deadlines are read at                                                                                                                                                                                                                                                                   |
| `deadline`, `band`    | the job's effective deadline and finish band as things stand                                                                                                                                                                                                                                                                                    |
| `achievable`          | the earliest deadline, on the policy's times, by which the job is ready at `quantile` without making a job that is on time late                                                                                                                                                                                                                 |
| `floor`, `floorWhy`   | the earliest the job can finish at all: its longest remaining unit, or its remaining work on every slot it can attain, and which binds                                                                                                                                                                                                          |
| `options[].label`     | the choice the queue offered (`dailies session Tue 08:00 PDT (15:00 UTC)`, `earliest at 80% without making another job late`); absent for a time the caller gave                                                                                                                                                                                |
| `options[].band`      | the finish band with the job's deadline at `at`                                                                                                                                                                                                                                                                                                 |
| `options[].meets`     | the highest of 50, 80, 90 and 95 whose finish is at or before `at`; 0 when none                                                                                                                                                                                                                                                                 |
| `options[].onTime`    | the finish at `quantile` is at or before `at`                                                                                                                                                                                                                                                                                                   |
| `options[].boost`     | the smallest boost, 1.1 to 2.0, by which the job keeping its own deadline is ready by `at` without making an on-time job late; omitted when none does or none is needed. It is an estimate: the plan's finish times do not model boost directly, so a boost b is planned as the slots the job would need for a deadline (deadline - now)/b away |
| `options[].burst`     | the burst the policy would buy for the job at `at`: slots per pool, cheapest first, and the estimated cost; `closes` when it restores the deadline, `automatic` when it is within the project's budget and below the lateness it prevents (otherwise `why` says what approval it needs)                                                         |
| `options[].displaces` | jobs on time with this job's deadline far out that `at` would make late                                                                                                                                                                                                                                                                         |
| `options[].displaced` | those of them in the viewer's projects, by name; `unnamed` counts the others, which are never named                                                                                                                                                                                                                                             |
| `options[].floor`     | set when nothing closes `at` (not on time, no boost, no closing burst): the earliest finish possible, with `floorWhy`                                                                                                                                                                                                                           |

`400` for a malformed or ninth `at`; `404` for a job outside the
viewer's projects; `409` when the job is not in the queue's plan (finished,
not schedulable, or newer than the last tick); `501` when the manager runs
without the work queue; `503` when the queue has not planned yet (it runs
on the leader replica, so a HUD on another replica answers this).

## GET /objects/{project}/{c4id}

The object's bytes (preview proxies are PNG or JPEG). Served only to viewers
of the project. Objects are immutable, so responses may be cached forever.

## GET /views/{project}/diff?from={c4id}&to={c4id}[&limit=N]

```
{ from, to, changes: [ { path, op, id?, size, oldId?, oldSize?, target? } ],
  counts: { added, modified, removed }, bytesAddedOrModified, truncated, seconds }
```

The files that differ between two views of a project, read from directory
records only: subtrees with equal IDs are skipped, so the cost follows the
size of the change, not of the tree. `op` is `added`, `modified` or
`removed`. `limit` defaults to 1,000. This is the query a CI system asks to
choose which targets a change affects (DESIGN.md section 8.2), and the MCP
tool `c4_diff_views`.

## GET /graph

```
{ nodes: [ { id, class, driver, phase, etaP90?, deadline?, slackSeconds?, critical, shot? } ],
  edges: [ { from, to, view? } ] }
```

`id` is `namespace/name`. `shot` is the job's shot label, as in JobSummary. `deadline` is the effective (propagated) deadline.
`critical` marks the critical path: from each unfinished sink, the chain of
unfinished inputs that finish last. `view` is the upstream job's output view.
Nodes are in dispatch order.

## GET /book?limit=N&job={ns}/{name}

The farm's book: who did what, to which object, signed by whom.

```
{ entries: [ { seq, at, actor, signer, kind, object: { type, job?, unit?, node?, pool?, view? },
               message, record? } ] }
```

Entries are newest last. `limit` (default 100, at most 1,000) keeps the
newest N. `job` keeps the entries about that job and its units; a job
outside the viewer's projects answers `404`. Entries are filtered to the
viewer's scope like everything else: entries about other projects' jobs,
units, views and capacity requests are not returned. Entries about a node
alone (provisioned, disabled, not ready) are shared infrastructure and are
returned to everyone.

| field         | meaning                                                                                                                                                                                                        |
| ------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `seq`         | orders entries that share a time; increasing along the book                                                                                                                                                    |
| `actor`       | who: `node/gpu-r3-01`, `component/queue`, `component/controller`, a person (`dana`), or an agent and the person it acts for (`agent/wrangler-bot for dana`; the agent names itself in the `X-C4-Agent` header) |
| `signer`      | the entity that signed the entry's record (DESIGN 9.2): `node`, `queue`, `controller`, `hud`, `importer` or `ingest`                                                                                           |
| `kind`        | what happened; see below                                                                                                                                                                                       |
| `object.type` | `job`, `unit` (with `job` and `unit`), `node`, `capacity` (with `pool`, and `job` when it is for one job) or `view` (with `view`, and `job` when a job reads it)                                               |
| `message`     | a sentence in production's terms                                                                                                                                                                               |
| `record`      | the C4 ID of the signed record, when there is one: a task record for a capture, a decision record for a yield, a control record for an action                                                                  |

| kind                                                                                                                                                 | signer             | entry                                                                                                                                                                                                                                                               |
| ---------------------------------------------------------------------------------------------------------------------------------------------------- | ------------------ | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `captured`                                                                                                                                           | node               | a unit's output captured as a changeset and folded into the job's output                                                                                                                                                                                            |
| `failed`                                                                                                                                             | node               | a unit's attempt failed, with the exit and the reason                                                                                                                                                                                                               |
| `retry`                                                                                                                                              | queue              | a failed unit queued again, away from the node it failed on                                                                                                                                                                                                         |
| `blacklist`                                                                                                                                          | queue              | a node added to a job's blacklist after `nodeErrorLimit` failures                                                                                                                                                                                                   |
| `expanded`                                                                                                                                           | queue              | a generator's expansion document became units                                                                                                                                                                                                                       |
| `yield`, `stop`, `yielded`, `resume`, `safepoint-stop`, `safepoint-continue`, `safepoint-move`, `reuse`, `interrupted`                               | queue              | a unit asked to yield at its next safe point, stopped, yielded, or resumed; the queue's decision at a safe point (stop, continue, or move to a cheaper pool); a unit that reused an earlier run's output; an attempt the farm stopped (interrupted: no retry spent) |
| `capacity-requested`                                                                                                                                 | queue              | burst capacity requested for jobs at risk                                                                                                                                                                                                                           |
| `capacity-approved`                                                                                                                                  | hud or queue       | a person approved a request over budget, or the queue approved one within it                                                                                                                                                                                        |
| `provisioned`                                                                                                                                        | controller         | burst nodes ready                                                                                                                                                                                                                                                   |
| `grant`, `revoke`                                                                                                                                    | hud                | a person's grant to a tracker (object type `grant`, `grant: ns/name`), or its revocation                                                                                                                                                                            |
| `hold`, `release`, `boost`, `deadline`, `forecast`, `withdrawn`, `write-confirmed`, `write-denied`, `requeue`, `disable`, `enable`, `avoid`, `allow` | hud                | a person's or agent's control                                                                                                                                                                                                                                       |
| `ingest`, `import`                                                                                                                                   | ingest or importer | a view entered the farm                                                                                                                                                                                                                                             |
| `started`, `finished`                                                                                                                                | queue              | a job began running, and its last unit ended                                                                                                                                                                                                                        |
| `node-not-ready`, `node-ready`, …                                                                                                                    | node               | a node's own Events: the Event's reason in kebab case (`NodeNotReady` is `node-not-ready`)                                                                                                                                                                          |

Kinds are open: a client shows an unknown kind by its `message`.

In the real backend the book is read on demand, never watched, from these
sources:

- Kubernetes Events on WorkJobs and SubmitGrants in the viewer's namespaces, and on
  CapacityRequests and Nodes in the operator's namespace and `default`,
  field-selected by involved object kind (and by name for `job=`). Events
  from the kubelet are signed `node`, from the queue `queue`, from other
  components `controller`; the kind is the Event's reason in kebab case
  (`NodeNotReady` is `node-not-ready`).
- The HUD's own control Events, whose annotations carry the actor
  (`c4.avalanche.io/actor`), the book kind (`c4.avalanche.io/book-kind`)
  and the control record (`c4.avalanche.io/record`), so a control's entry
  names who made it and the record that proves it.
- WorkJob status: each job's start and finish (the finish names the job
  record the queue signed), the queue's decisions with their signed decision
  records, and yields with the view each one saved.
- CapacityRequests themselves, as `capacity-requested` at their creation.

Per-unit captures and failures are in the job's provenance index (each
capture's task record, `GET /trace`) and its status counts rather than in
the real backend's book. `seq` there is the entry's time in microseconds,
made strictly increasing within one answer; it is not a cursor (it shifts
with `limit`, `job` and scope). To resume, use `since` (below).
Events expire after the API server's event TTL (an hour by default); the
signed records they name stay in the project store, and decisions stay in
WorkJob status.

## GET /book?since={seq}&limit=N

The farm's transition log (DESIGN.md 9.2, Storage and use): every change of
a job's status the work queue wrote, numbered farm-wide in the order it
wrote them, kept durably. A client that was away (c4-sync after an outage)
reads what it missed, in order, from its cursor.

```
{ entries: [ { seq, at, project, job, unit?, kind, actor?, do?, record?, message, key, policy? } ], next, more, head }
```

- `kind`: `phase`, `started`, `finished`, `wait-opened` (with an
  escalation, "escalated to …"), `wait-decided`, `verdict`, `ack`,
  `decision`, `yield`, `interrupted`, `needs`, `pressing`, `supplied`,
  `needs-withdrawn`, `cause`.
- `policy`, on a decision a policy value made (a question pressing, a
  re-check refused at its cap, a question refused at `asks.max`): `[ { key,
  value, from: { scope, ref? } } ]`, `scope` farm, project, job or unit.
- Order by `seq`, never by `at`. `at` is when the change happened; `seq` is
  when the queue recorded it.
- `key` names the change itself. One recorded twice (after a crash) has the
  same key.
- Only the viewer's projects' entries are returned. `next` is the cursor to
  send next: it moves past entries the viewer can't see. `more` says a full
  page was cut at `limit`. `head` is the latest entry, where a new client
  starts.
- `410` with `{ error, oldest }` when the log no longer reaches
  `since + 1` for a project. Read the jobs again, then go on from `oldest`.

The queue alone writes it, each tick, before the statuses it describes.
In the operator's namespace, the ConfigMap `c4-work-book` holds only the
next number, and each project's ConfigMap `c4-work-book-<project>` (label
`c4.avalanche.io/work-book`) holds its latest entries and segment index.
So no object grows with the number of projects, and each stays far under
the API server's 1 MiB limit. Every 128 entries (or 256 KiB, whichever
comes first) are sealed as an object in the project's own store, naming
the previous one by C4 ID; the head lists the latest 64 per project. Any
HUD replica serves it.

## GET /needs-me

Only what this viewer may act on now: the one-person view, and MCP's
`c4_needs_me`. Each item is listed only if the controls would take its
actions from this viewer (patch rights on the job, then who may answer
it), so a listed button is never refused.

```
{ now, viewer,
  items: [ { id, kind, job?, project?, unit?, unitName?, title, ask?, why?,
             state, severity, since, answerBy?, pressingWhy?, choices?,
             actions: [ { act, action, path, body? } ], href? } ] }
```

| `kind`     | what waits                                   | who gets it                                                                             | `actions`                                                      |
| ---------- | -------------------------------------------- | --------------------------------------------------------------------------------------- | -------------------------------------------------------------- |
| `question` | a unit stopped for information (`job.needs`) | the job's owner; with no owner, anyone who may change the job                           | `supply` (`/supply`, add `text`), `withdraw-needs` (add `why`) |
| `decision` | a wait for a person, a party or an agent     | whom it names (a member of its party); on a single-person farm, the installer, any role | one `decide` per choice, its `do` in `body`                    |
| `burst`    | capacity waiting for approval                | whoever may approve the request                                                         | `approve` (`/capacity/{pool}/approve`)                         |

- `id` is `question:<ns>/<job>/<unit>`, `decision:<ns>/<job>/<unit>` or
  `burst:<pool>`.
- `title` reads "comp in sm0140 asks you", as the question's incident does.
- `state` is `raised` or `pressing` for a question, `open` for a decision,
  `pending` for a burst; `severity` is `urgent` once pressing, else
  `warning`.
- `answerBy` is the latest an answer keeps the job on time.
- `body` never carries the person's words: the client adds `text` (a
  supply) or `why` (a withdrawal).
- Standing in for someone else is not something that needs you, so it is
  not listed; the job's own controls still take it where allowed.

An observed session whose `word` is `needs you` (see `observed`, below) is
listed too, as `kind: session`, `id: session:<node>/<session>`, its
`needsYou` as `ask`. No farm control answers it, so its `actions` are
empty and `open` says how: `claude --resume <session>`.

Ordered pressing first, then by `answerBy`, then oldest. Settled questions
and decided waits leave the list.

## POST /nodes/{node}/observed

A host runner's report of the Claude Code sessions on its machine,
`c4-agent observe --json` as written, posted about every minute:

```
{ machine, observedAt, owners: [ ... ], sessions: [ { session, name?, kind, running, word, needsYou?, cwd?, startedAt?,
                                     lastActive?, turnAge?, backgroundOpen, usage?, limit?, transcript?, onFarm,
                                     turns?: [ { index, start, end, tokens, weighted, state } ], turnsTotal?, turn?,
                                     parent?, waitsOn?, account?, ... } ] }
```

- Only the node's own runner may post: the service account
  `c4-hosts/<node>`. Any other caller, or another node's runner, gets 403.
- Each report replaces the node's last. Reports are held in memory only,
  never in a resource or the book, and a report not renewed within 5
  minutes is dropped.
- At most 200 sessions; more is 413.
- `owners` are the runner's `--owner` users, whose machine it is.
- The answer is `{ shownTo: [ ... ], why? }`: those of the runner's owners
  who also own a pool the node is in (Pool `spec.owners`), so a runner
  cannot put rows before someone who never took its node. They alone see
  the rows, as the snapshot's `observed`, each with `machine`, `node`,
  `owners`, `observedAt` and `postedAt` added. No one else sees them, a
  farm admin included, nor any count of them. An empty `shownTo` comes
  with `why`: the runner names no owner, or its node is in no pool its
  owners own.
- The rows are served only in a viewer's own answers (the snapshot,
  needs-me, and MCP through them). Nothing that leaves the farm, such as a
  diagnostic bundle, reads them.
- `nodes[].usedOutside` counts a node's reported sessions in use outside
  the queue (`working`, `needs you`, `at its limit`), and
  `pools[].usedOutside` sums its nodes. The HUD draws a busy machine as
  "in use outside the queue", not free. It is a bare count, shown to
  anyone who sees the node, and never slots: the queue does not read it,
  so a person's own sessions never make the farm refuse their queued work.
- A session's `turns` (its latest 200, oldest first, of `turnsTotal`) are
  read on post but not repeated in the snapshot's rows: its lead entry
  carries them.

### Lead entries

Each observed session (any row but a worker, `kind` `subagent` or
`teammate`) is also one entry in the snapshot's `jobs`, drawn as the
queue draws any job: `driver` and `class` `lead`, `observed: true`,
`namespace` `observed:<node>` (a colon no Kubernetes namespace has, so no
farm job shares the key), `name` the session, `label` its name, `account`
its sign-in's opaque label (sessions that share it share one usage
limit), `owner` the viewer, no project. Entries come after the farm's
jobs, with `rank` 0 and no deadline: never ranked, planned or counted in
incidents, and the queue does not read them. They have the observed rows'
visibility exactly: the runner's owners see them, no one else.

- Its turns are its frames. `GET /jobs/observed:<node>/<session>` answers
  the job page's shape: `axis: { kind: turn, start, step: 1 }`, `start`
  the first listed turn's number; groups `{ key: turn, generator: true,
  spans: [work] }`, one unit per turn, and `{ key: work, parent: turn,
  perBlock: true, sparse: true }`, one unit per worker at `pos` = the
  worker's `turn` - `start` (a nested worker in its starter's row; one
  started before the listed turns, or by an unknown call, at the nearest
  listed row). Anyone who does not see the session gets 404.
- States are the queue's words: a done turn `succeeded`, the turn in
  flight `running`, a turn that ended on a call with no result `needs`
  (its person's prompt), `waits` (on its lead) or `failed` (the session
  stopped on an error). A worker: done `succeeded`, failed `failed`,
  working `running`, waiting on its person `needs`, on its lead or idle
  `waits`, stopped `held`. The phase is `Running` while the session is up,
  `Queued` at its usage limit, else `Succeeded` or `Failed`.
- The turns before the listed ones count in `units.total` and
  `units.succeeded`, and `turnsTotal` is the session's turns in all, so a
  session of 289 turns reads 289, not 200.
- Turns to come exist only under a turn limit (`turnLimit` on the row;
  observing reports none yet): then they are `planned`, the first
  `generates` how many, and the ETA is the turns left at the measured time
  per turn (p90 a sixth longer), after the reset when a usage limit holds
  the session. With no limit there is none: `noEta`, and the HUD reads "N
  turns so far". A session at its usage limit says in `estimate.message`
  when the limit resets.

## GET /nodes/{node}/pods

A host runner's own pods: the unit pods bound to its Node, for that node's
runner alone (the service account `c4-hosts/<node>`; anyone else gets
403). A host runner reads no pod from the API server, so its token reads
nothing of anyone else's work.

```
{ node, pods: [ { pod: <the Pod as the API server has it>, job?: { name, uid, owner, grant? } } ] }
```

`job` is the WorkJob that controls the pod, matched by UID (a job made
again under the same name is another): whose work it is, and the grant
it was submitted under, which the runner's records name.

## POST /nodes/{node}/enroll-offer and POST /nodes/{node}/enroll

Enrolling a machine is an offer and an acceptance, as a device sign-in
is, so no one enrolls a machine they do not hold.

- **The offer:** the node's own runner posts `{ holder }` (whose Claude
  Code sign-in it uses) with its own token, and gets `{ code, expires,
  record }`: a code of eight letters (`XXXX-XXXX`, no look-alike letters)
  that stands for 10 minutes, shown at the machine (`c4-agent enroll-offer`). A
  new offer replaces the last. The offer is signed (`control/v1`,
  `enroll-offer`, by the node's account).
- **The acceptance:** the machine's holder (the person whose Claude Code
  sign-in its runner uses), signed in as themselves, posts `{ code }`, or
  `{ code, for }` to add someone else. With the standing code, once, the
  holder, or the person they name, becomes one of the machine's owners:
  the HUD writes the Node's `c4.avalanche.io/node-owners`, a JSON list of
  names (its runner cannot, at its creation or after), and signs the act
  (`control/v1`, `enroll`, by the holder, for the owner added, naming the
  offer's record). Anyone but the holder is refused with how to be added
  (the holder runs `c4 farm enroll <node> --for <them>`), since another
  person's jobs spend the holder's sign-in only with the holder's consent.
  The code is read in any case, with or without its dash or spaces. A wrong
  code, a used one, an expired one, a unit or a service account gets 403,
  and five wrong codes end the offer.
- The queue places a person's host work only on a Node that names them.

## GET /events

Server-sent events. One event type:

```
event: snapshot
data: <the GET /snapshot body>
```

At most one per second, and only when something changed. Comment lines
(`: keepalive`) keep idle connections open. Clients should also fetch
`/snapshot` once on load so the first paint does not wait on the stream; the
HUD does.

## GET /capacity/forecast

What the farm's forecast jobs (`spec.forecast`: work a tracker expects
from a bid or a schedule, not yet submitted; DESIGN.md 10.12) would
need among the real work, as the queue's last forecast pass planned it.
Nothing of it is requested or acted on.

```
{ at?, jobs: [ns/name], bursts: [ { pool, slots, live, costPerSlotHour?, extraCostPerHour? } ],
  worse: [ { job, risk, live } ] }
```

`at` is the last forecast pass, absent when there has been none (no
forecast jobs). `jobs` are the forecast jobs planned and `worse` the real
jobs whose risk the forecast work raises (`risk` with it, `live` as the
farm plans now), both only in projects the viewer sees. `bursts` are
farm-wide, as pools are in the snapshot: the burst slots suggested in a
pool with the forecast work (`slots`) and now (`live`), approved or not,
and what the difference would cost an hour at the pool's burst price.
`503` from a HUD without the work queue.

## Controls

Every control is a POST with a JSON body. In the real backend each one is one
Kubernetes write: the user must be allowed that write (a SubjectAccessReview
for the verb and object), the HUD's service account makes it, and an Event on
the object names the user. Success is `200` with:

```
{ ok: true, message, job? | node? }
```

`message` is a sentence for the HUD to show ("sh040-fx-v003: held"). `job` is
the updated JobSummary; `node` the updated node. Errors are `4xx` with
`{ error }`. Every job control, `/ack` and `/decide` included, answers
`{ ok: true, message, job }`.

| path                                      | body                                                                   | effect                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                              |
| ----------------------------------------- | ---------------------------------------------------------------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `/jobs/{ns}/{name}/nudge`                 | `{ hold?, holdStop?, boost?, deadline?, deadlineMode?, nextSession? }` | `hold`: release no new units (`false` releases); `holdStop: true`: hold and stop running units; `boost` 1 to 2; `deadline` RFC 3339 or a date at the project's due time, which pins it unless sent with `deadlineMode: Tracking` (production tracking's date, a pinned job handed back to tracking in one write); `deadlineMode` `Pinned` (alone, pins the deadline the job has now), `Suggested` (alone) or `Tracking`; `nextSession: true` moves the deadline to `objective.suggestion.nextSession` and pins it (`409` when none is offered)                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                      |
| `/jobs/{ns}/{name}/ack`                   | `{ units, kind?, assignee?, note? }`                                   | a person takes failed units: `kind` is `acknowledge` (default: the viewer has them), `assign` (to `assignee`) or `accept` (the failure stands; they count as done, so nothing waits on them). Only failed units are taken (`409` when none is). Written as a request in the `c4.avalanche.io/acks` annotation that the queue carries out once; it shows in `acks` and the job's book                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                |
| `/jobs/{ns}/{name}/decide`                | `{ unit, do, note?, reply?: { view } \| { text }, via? }`              | a person decides what waits for them: a wait unit (`do` is `approve`, one of its `choices`, or `fail`; a wait with `reply` takes one unless `do` is `fail`, a stored view's C4 ID or a text the HUD stores as a view of one file, `reply.md`, which the units it holds up mount; `400` for a reply the wait does not ask for) or a unit a review escalated (`accept`, `rework` or `fail`). `unit` is a name or an index; `409` when nothing open waits as that unit. Signed first, with its custody (`direct`, or for `via` `relayed` / `relayed-party`); written with its record as a request in the `c4.avalanche.io/decide` annotation that the queue carries out once; it shows in `waits` and the job's book. `via: { tracker, source, by, person, evidence? }` is a decision made in a tracker, relayed by its sync (c4-sync only; see Relayed decisions). `403` unless the decider is whom the wait names (the person, a member of the party, the agent), or stands in where the farm and the project allow it (custody `stand-in`, the record's `for` naming the person); never a unit, nor an agent the wait does not name |
| `/jobs/{ns}/{name}/steer`                 | `{ unit, text }`                                                       | a message to a unit while it runs, without ending it: `unit` is a name or an index, `text` up to 8 KiB. Signed first; written as a request in the `c4.avalanche.io/steer` annotation that the queue carries out once, delivering it to the unit's pod, where the unit reads it at its next safe point (`c4-unit steer`). A unit not running yet gets it when it starts; `409` for a unit that has ended. It shows in the job's `steers` (`delivered`, `seen`) and its book                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                          |
| `/jobs/{ns}/{name}/supply`                | `{ unit, text }`                                                       | gives a unit that stopped for information its brief lacked (`needs`, NeedsPerson `needsinformation`) what it asked for. Signed first; written as a request in the `c4.avalanche.io/supply` annotation that the queue carries out once: the unit runs again, resuming where it stopped, with the text in `$C4_SUPPLIED`. `409` for a unit that is not waiting for information; `403` for a unit (a unit's question is for a person)                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                  |
| `/jobs/{ns}/{name}/needs/{unit}/withdraw` | `{ why }`                                                              | withdraws the question a unit stopped for (`needs`) once it no longer applies, under the who-may rule `withdraw` (a unit never). Signed first; written as a request in the `c4.avalanche.io/withdraw-needs` annotation that the queue carries out once: the unit runs again without an answer, with the why in `$C4_WITHDRAWN`. `400` without `why`, `403` for one who may not, `409` for a unit with no open question                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                              |
| `/jobs/{ns}/{name}/requeue`               | `{ units }`                                                            | queue the units again; `units` is a unit set string or an array of indexes                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                          |
| `/jobs/{ns}/{name}/forecast`              | `{ slotHours?, sd?, unitHours?, start?, exists? }`                     | resize a forecast job (`spec.forecast`), as its tracker's bid changes: a field sent replaces the job's, `""` clears it (the class's plan or the default stands), a field left out is kept; booked as kind `forecast`. The same size again writes and signs nothing (`message` says unchanged); `409` on a real job, `400` for a size that is not a number                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                           |
| `/jobs/{ns}/{name}/withdraw`              | `{ reason? }`                                                          | delete a forecast job whose work is no longer expected (its tracker's entity left every intake rule, as an omitted shot does), signed and booked as kind `withdrawn` with the reason; needs leave to delete the job, and only its owner, the sync for the Tracker it came from, or the sync holding a grant from its owner withdraws it (`403` otherwise). `409` on a real job, `404` once it is gone                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                               |
| `/jobs/{ns}/{name}/blacklist`             | `{ node, remove? }`                                                    | add the node to the job's blacklist, or remove it                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                   |
| `/nodes/{name}/disable`                   | `{ disabled }`                                                         | taint or untaint the node farm-wide; running units finish                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                           |
| `/capacity/{pool}/approve`                | `{}`                                                                   | approve the pending capacity request in the pool                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                    |

## Submitting

What `c4 farm submit` calls: objects, views and jobs. MCP's `c4_submit_job` and `c4_run_<tool>` submit through `POST /submissions` instead. Each write
is checked like a control (a SubjectAccessReview for creating
materializations, or workjobs, in the project's namespace) and made with the
HUD's service account; the bytes go through the manager's own store access,
so no store credential leaves the cluster. `{project}` is a project the
viewer sees; anything else is `404`.

A host runner, signed in as its own service account `c4-hosts/<node>`, may
call the three object and view writes below for a project only while a unit
pod of that project is bound to its Node (`403` otherwise). That is how it
stores its units' control documents and session views; it cannot submit.

### POST /objects/{project}/missing

```
{ ids: [c4id] }  ->  { missing: [c4id] }
```

The IDs the project's store lacks, in the order given, without duplicates.
At most 100,000 per request (a body up to 16 MiB); `413` for more.

### PUT /objects/{project}/{c4id}

The body is the object's bytes. It is held on the manager's disk and
identified first: `400` if its C4 ID is not `{c4id}`, so the store never
holds bytes under the wrong name. `200 { ok, id, stored, bytes? }`;
`stored` is false when the store already had it.

### POST /views/{project}

```
{ files: [ { path, id, size, exec? } ], dirs?: [ path/ ], links?: [ { path, target } ] }  ->  { view, files, bytes }
```

A body up to 512 MiB (`413` past it). Records a tree of stored objects as a view: its directory records go into
the store and `view` is the tree's C4 ID, the same ID `c4-operator tree-id`
gives the directory on disk. Paths are clean, relative and slash-separated.
`dirs` are the tree's empty directories (any other directory is implied by
its paths) and `links` its symbolic links, recorded as given and never
followed by the farm; both are part of the ID.
`409 { missing: [c4id], files, bytes }` when an object is not in the store.

### GET /views/{project}/{c4id}/files[?path=prefix&limit=N]

```
{ view, files: [ { path, id, size, exec? } ], dirs?, links?, truncated? }
```

The files of a view, or of the subtree at `path`, with its empty
directories and links; the bytes of each file come from
`GET /objects/{project}/{c4id}`. This is what `c4 farm get` reads. `get`
makes a link only when its target stays inside the folder it writes to.

### POST /jobs

```
{ project? | namespace?, name?, type?, stage?, shot?, spec, inputs?: [ { view? | fromJob?, mountPath, readOnly? } ],
  aiUsage?, trackerRef?, trackerURL?, follows?, due?, for?, dryRun? }
->
{ ok, dryRun?, namespace, name, project, link, message, materializations?,
  estimate?: { units, work50, work90, eta50?, eta80?, eta90?, eta95?, deadline?, deadlineSource,
               suggestion?: { deadline, state, policy, quantile, nextSession? },
               slackSeconds, feasible, atRisk, calibrated?, message? },
  estimateError? }
```

`spec` is a WorkJob spec (`c4.avalanche.io/v1alpha1`). The request and its spec are read strictly: a field neither has is `400`, naming it. `spec.tool` is `400` too: the farm sets it when it lowers a tool's submission (`POST /submissions`), and the units of a job without it reach no network unless they name a tool (Networks, below). The body is at most 1 MiB. The HUD names the
job `<shot>-<stage>-vNNN` (stage defaults to type) when `name` is empty, one version past the latest
such job in the namespace; makes a Materialization for each input view
(named from the view's ID, so the same view is one Materialization);
adds `fromJob` inputs to `dependsOn`; sets the deadline mode to `Suggested`
when the spec has no deadline; and stamps `c4.avalanche.io/owner` (the
viewer), `c4.avalanche.io/submitted-via` (`hud` or `mcp`), and the labels
`c4.avalanche.io/shot`, `c4.avalanche.io/job-type` and
`c4.avalanche.io/stage` (which, with the shot, key the shot history a job's
estimate starts from). A job a unit submits (with its pod's token and a `c4`
grant) is owned by the owner of the unit's own job, whose runners run it
and who answers for it, and `c4.avalanche.io/submitted-by-unit` names the
unit as a record names it (`<namespace>/<job>@<uid6>/<unit>`, the job's UID
from the unit pod's controller reference); a unit whose job is gone, was
made again under the same name, or has no owner, gets `403`. Outside its own job's namespace the owner must also be allowed to create workjobs there (the API server is asked about the owner by name), else `403` naming the owner; in that namespace the owner already was. Only the farm sets that annotation: the owner webhook
drops anyone else's, and it cannot be added or changed later. `aiUsage` becomes
the job's `c4.avalanche.io/ai-usage` annotation. `trackerRef`
(`<tracker>/<type>/<id>`) is the tracker entity the job is made for: it
becomes the annotation `c4.avalanche.io/tracker-ref` and the label
`c4.avalanche.io/tracker`, and the job's record names it. `trackerURL`,
with it, is where a person opens the entity (annotation
`c4.avalanche.io/tracker-url`): an http(s) URL, or a path on the HUD's own
host; anything else is a `400`. `follows` names
the job in the namespace this one is the next version of (`spec.follows`,
in the record too); it must exist. `due` pins the deadline: an RFC 3339 time,
or a date (`2026-10-23`) at the project's due time (Project
`spec.work.dueTime: { at: "HH:MM", timeZone }`, 18:00 UTC without one).
`for` names the person a tracker's sync (c4-sync) submits the job for,
under the grant that person gave the Tracker (see Grants): the person
owns it, `submitted-via` is `c4-sync`, and `c4.avalanche.io/grant` is the
grant's record. It is refused, never created as the caller's own job:
`403` when the caller is not a tracker's sync, `trackerRef` names no
Tracker in the namespace, the person has no grant to it, the grant was
revoked or has ended, or it does not cover the job's pools or class;
`400` without `trackerRef`; `501` on a farm without grants installed. A
refusal answers `{ error, reason }`, `reason` being `no-grant`,
`grant-lapsed` (revoked or ended), `grant-uncovered` (not the job's pools
or class), `no-tracker`, `not-sync` or `not-installed`, so the sync tells
the person what to do.
`replaces` names a forecast job (`spec.forecast`) in the namespace that
this real job replaces: once the job is made, the forecast is deleted and
booked (kind `replaced`) with a signed control record of kind `replace`.
Only one who could have made the forecast replaces it: its owner, the
sync for the Tracker it came from, or the sync submitting under a grant
from its owner (`403` for anyone else). The job carries `c4.avalanche.io/replaces`; a
delete that fails is tried again, with backoff, until the forecast is
gone (a forecast made after the job is left alone). Only a forecast is replaced, and only by a real
job (`400` otherwise). A forecast is submitted with no container, since
it never runs.

`estimate` is what the queue expects of the job against the farm as it last
planned it. `slackSeconds` is the deadline less `eta90` (`eta95` for a
suggested deadline), as for a running job. With `dryRun`
nothing is created; the answer is `200`, or `503` when the queue has not
planned yet (or runs on another replica) and `422` when the job's class
cannot plan it. Without `dryRun` the answer is `201`, with
`estimateError` when there was no estimate; `409` when the name is taken.
A book entry of kind `submit` names the viewer (or the agent for the
viewer, through MCP).

A job may accept a validated recompute of an input in place of moving it to
another site (experimental, staging). `POST /jobs/{ns}/{name}/accept-validated`
`{ accept }` sets it, as anyone who may patch the job, and answers `{ ok,
message, job, record }`. It is an approval, so it is signed: the request
names its control record, and the queue holds to the newest acceptance it
can verify. A farm with signing off also reads
`c4.avalanche.io/accept-validated: "true"`. A validated recompute's output differs from the
recorded one, so only the consuming job may choose it; exact recomputes and
identical outputs need no consent, and volatile ones are never offered.
At that site the queue points the job at the recompute's output (input
substitution): its unit mounts the output through a Materialization the
job owns, its pod says what it read in place of what
(`c4.avalanche.io/input-substitutes`), the job's derivation record takes
the input it read, and status lists it (`inputSubstitutes`). The job's spec,
its Materializations and other jobs are not changed, and elsewhere it reads
its inputs as named.

A recompute runs as a WorkJob labeled `c4.avalanche.io/recompute: "true"`,
named `rc-` and a hash of the computation and site, in the producing job's
namespace. `c4.avalanche.io/recompute-of` names the job and unit it reruns
(`namespace/name#index`); `c4.avalanche.io/recompute-unit` keeps its plan
to that unit (its definition from the producing job's plan: JSON name,
label, key, position, slots, reasoning, environment and work), and is the
queue's to set; `c4.avalanche.io/recompute-view`
and `-site` name the input it rebuilds and where, and the queue sets
`c4.avalanche.io/recompute-output` to the view its unit wrote.

### POST /submissions

```
{ submission, dryRun?, explain?, key?, plans?: { <path>: <plan as JSON> } }
->
{ ok, dryRun?, errors?: [ { path, message, fix, reason? } ], submission, record?,
  jobs?: [ { key, namespace, name, tool, version, what, estimate?, estimateError?, after?: [name], link } ],
  estimate?: { eta50?, eta90?, deadline?, slackSeconds, feasible, atRisk, jobs },
  warnings?: [string], explain?, message? }
```

`submission` is a submission (`c4: submit/1`, the submission format,
schema/README.md) as JSON, its keys in the order written. The farm checks
it against the project (its tools and their parameters, the jobs it
follows, its dues, trees and `tracker:`), lowers each job to a `POST /jobs`
request, and creates the jobs in the order written: `201`. `project` may
be left out by a viewer with one project. `for:` and the people a
submission names are not checked yet: the HUD keeps no list of the farm's
people, so only a grant's refusal (below) speaks for them. A tool pinned
to a version older than the one in use (`proxies@v4` when v5 is) is
refused: "proxies is v5 now; its form changed: draw it again".

It is all or nothing, whatever the client does. Every job is checked as
`POST /jobs` checks it before the first is made. The jobs are created
held, so none of their units runs (and nobody may change their hold:
`409`; the HUD shows them as being submitted), the submission is signed
for once they all exist, and they are released together: the record goes
on every job while all are held, then the holds come off. If a create, the signature or
the release fails, the jobs made are deleted, each only while it is still
the job made, and booked so; a submission already signed for gets a
withdrawal, signed by the farm after the deletes, naming what was deleted
and what was left; a job that could not be deleted is named in the
refusal. The rollback has a window of its own, so a submission that ran
out of time is still rolled back. All of this goes on if the client hangs up. A HUD that stops
midway leaves its jobs held and marked pending
(`c4.avalanche.io/submission-pending`) until `submission.pendingFor` (10
minutes, a project policy value, at least 3), after which they are deleted
with every job of their attempt, and a signed attempt is withdrawn. A name
taken meanwhile is numbered past and the jobs made again, never reported; a
job's `version` is never numbered again. Every refusal is `{ path,
message, fix }` in the farm's words, at a dotted path (`jobs.comp.after`,
`jobs.movie` for a job the farm would not take): `422` for the file or the
farm's policy, `403` without access, `404` for a project the viewer cannot
see, `503` when the farm cannot make a job just now (too many requests,
a timeout, a server error: "try again in a moment"). Nothing of the
operator's own text reaches the caller, in a refusal or a dry run's
`estimateError`. A folder tree is refused beside the file's other errors. A submission for a person
(`for:`) refused for its grant is refused at `for` with POST /jobs's
`reason` (`no-grant`, `grant-lapsed`, `grant-uncovered`, `no-tracker`,
`not-sync`, `not-installed`) and the status POST /jobs gives, so one
handler serves both; a farm with no trackers is refused at `tracker`, one
with no grants at `for`, once (`501`), as not available on this farm,
never as a thing to retry. A unit's submission refused for who it is (its
own job, that job's owner, what its tool grants) says so in its own words
at `jobs.<key>` (`403`). A create the API turns away for the moment keeps
its class: `503`, try again.

`key` makes a retry safe. A client names the submission it means (any
text) and sends the same key on every retry: the request is answered
once. A retry is given the jobs the first made (`200`, "submitted
already, with this key"), or `409` at `key` while they are still being
made, never a second set. A key names one submission: sent with another
file it is refused at `key` (`409`). Keys are the submitter's own, and
answer for `submission.keyFor` (24 hours); after that the same key makes
a new submission. Without a key the
same file sent twice is two submissions, the second numbered past the
first. MCP's tools send one and give it back (`key`).

With `dryRun` the jobs are planned together, each after the jobs it
follows and competing for the same slots, so the last job's `eta` is when
the chain is ready (`200`). A job's `estimate.reused` is how many of its
units would reuse an earlier run's output rather than run (at most: a
share runs anyway to verify), and its `what` says so (`48 frames in 6
units; 6 of 6 units reused`; `1 frame in 1 unit`, `1 unit over 1001-1048`
for a once job filling its frame parameter, `1 unit`). `estimateError` says why there is none (the
queue has not planned yet).

Each job is named `<name>-<key>-vNNN` (`name` defaults to the shot,
lowercased, with anything outside `[a-z0-9-]` made `-`, repeats collapsed
and the ends trimmed: `sm0220_fx` names `sm0220-fx-light-v001`; a shot with
nothing left is refused at `shot`). A job's `version` (`4` or `v004`, as a
tracker numbers it) names it `<name>-<key>-v004`; one whose name exists
or is below the latest is refused ("sm0150-proxies-v004 exists; leave
version out for the next one, v005"). Otherwise it is one
version past the latest such job, for the whole submission before any is
made, so a job's inputs can name the jobs it follows; it `follows` the
previous version of the same name and key unless the file says otherwise.
A job runs the tool's unit image pinned in its `tool.lock`, with the tool
runner (`c4-unit run`) as the command and the tool's tree read-only at
`/etc/c4/tools`; its parameters are `C4_PARAMS`, its shot `C4_SHOT`, the
submission's `root:` `C4_SOURCE_ROOTS`. Memory and GPUs are limited to
what is requested, CPU only requested; sizes are binary (`64G` is
`64Gi`). A `frames` job without frames takes those of the frames job it
follows, else the shot's cut where the farm knows it, else it is refused;
a `once` job's frame parameter takes the same default. The tree is
mounted writable at the project's root: a C4 ID as its view, `job:<name>`
as the latest such job's output, and for a job with `after` the output of
the job it follows (`dependsOn`). `read` trees mount read-only at
`/c4/read/<name>`. A folder is the front end's to upload first (`POST
/objects`, `POST /views`); a dry run takes a C4 ID the store lacks, a
submit does not. A `due` that names a milestone labels the job
`c4.avalanche.io/milestone: <name>`, which joins the Milestone whatever its
selector; a time (`fri 18:00`, `18:00`, `+3h`, RFC 3339, in the project's
zone) or a date pins the deadline; `suggested` (the default) leaves it to
the farm.

Each job carries two identities: `c4.avalanche.io/submission`, the C4 ID
of the file (the compact JSON sent), and `c4.avalanche.io/submission-record`,
the ID of the signed record of submitting it (`record`; kind `submit`),
so the same file sent twice is two submissions, the next versions of each
job. `note:` is the job's `c4.avalanche.io/note` and a line of its book
entry, with the submission's ID. `c4.avalanche.io/derivation-class: exact`
marks a job whose tool says `reuse: exact`.

**Agent work.** A job that runs an `agent` tool, or a plan (`plan:`
with `each`), is a `graph` job whose unit table is stored with it: the
farm writes the table as an expansion document, stores it in the
project's store before it creates the job, and names it in
`params.table` (a dry run plans it without storing it). A single task is
a table of one unit, named for the job's key; a plan's blocks are the
table, read from the plan file the request carries in `plans` (by the
path as written, as JSON; the farm never reads a plan from the tree, and
a plan the request lacks is refused at `jobs.<key>.plan`: "port.plan.yaml
was not sent with the submission"). The plan file's C4 ID is in the
submission's record. Each unit is a block, a fresh agent session: a
block's `params` named for the tool's parameters are its settings (its
`reasoning` first, then those params, then the job's `with`, then the
tool's defaults), set as the unit's `C4_BLOCK_PROMPT` (`task`),
`C4_BLOCK_TOOLS` (`allow`), `C4_BLOCK_MODEL` and `C4_BLOCK_EFFORT`; other
params stay facts of the plan, and a block may not set `C4_*` itself. The
template, once for the job, is the tool's, with `C4_AGENT_BLOCK=1`,
`C4_BLOCK_PROFILE` (the tool's name) and `C4_BLOCK_REPO`, the
submission's `root`: the repository on the developer's machine, where a
host runner gives each block its own worktree. A tool that runs on hosts
goes to the farm's host pools the submitter may use (pools whose node
selector names `c4.avalanche.io/host`, with no `owners` or the submitter
among them); with none, a tool that also has an image runs as block pods
on the class's pools, and one that runs only on hosts is refused: "no
machine of yours runs agents on this farm yet". A planned block runs
`each.tool`, any other the job's `tool:` (or `each.tool` when it names
none); blocks running two different tools in one job are refused for now.
Its `answers: {by: unit:<name>}` names one of its own blocks (for a single
task, the job's key), since every block reasons.

What the farm cannot run yet is refused in its words, not run otherwise:
plans of frames or once tools, checks, approvals, waits, a lead or a tool
answering questions, generators and their children, units over more than
frames, policy, a job that follows two, git trees, a `preview` other than
`fml` and `none` (this farm orders only those), tools that need licences,
the network beyond their licence servers (an agent's `model-api` is
allowed), sidecars, a Kubernetes overlay, a gate, farm access, or only a
host (except an agent tool). `answers: {by: unit:<name>}` must name a unit of the job that
reasons: a frames or once job's units (`<key>/<n>` from 0) each run a
procedure, so each is refused ("render/3 cannot answer questions: it runs
a procedure"), as is a name that is no unit of the job. `warnings` are what it does differently meanwhile: a job that
follows another frame by frame waits for the whole of it. `estimate` is
the jobs' as one: the latest finish, the earliest deadline, the least
slack. `explain` is what each job lowers to: its WorkJob (project/name),
class and params, inputs, image, command and tool, resources, units and
deadline. `message` names the jobs as project/name.

### GET /projects/{project}

```
-> { name, root, timeZone, dueTime, milestones: [ { name, label?, at } ] }
```

What a submitter writes for: where the project's trees mount (`spec.root`,
`/show` without one), the zone a due without one is read in and the time
of day a date falls at (`spec.work.dueTime`, 18:00 UTC without one), and
the Milestones of the project's namespace, soonest first; `label` is the
tracker entity that owns the date. `404` for a project the viewer cannot
see.

## Tools

A tool is a folder with a `tool.yaml` (`c4: tool/1`, `schema/README.md`):
what one unit runs, its parameters and its test. Publishing stores it as a
`Tool` in the project's namespace, or in the operator's for the whole farm,
and offers it to submissions (`POST /submissions`) and to agents (MCP).
Tools and agent configurations are both `Tool` objects and share one name
space. The schemas are served at `GET /schema/{submit,tool,lock}`.

### POST /tools/{project}

```
{ view: c4id, farm?: bool, dryRun?: bool }
  ->  { ok, errors?: [ { path, message, fix } ], tool, version, view, image, scope, lock,
        release?, test: { job, state: running|ok|failed|untested } }
```

`view` is the folder's tree, already stored through `PUT /objects` and
`POST /views`. Who publishes is the HUD's rule, not RBAC on `Tool`
objects: a person with the `pipeline-td` or `farm-admin` role, or an agent
acting for one (MCP); with `farm`, the `farm-admin` role only. A unit never
publishes, whatever its tool's `needs.farm`. Otherwise `403`. Publishing:

- reads `tool.yaml` from the tree's top and checks it: the schema's errors
  and the farm's (an `emits` naming no tool, a format string naming no
  parameter), each with its dotted path and fix, as `422 { errors }`;
- refuses, at `tool`, what the farm cannot run yet (sidecars, farm access,
  a Kubernetes overlay, licences, and `network: [licences]`: "licence
  servers are not counted yet"), in the words a submission's refusal
  uses, and a tool an agent publishes with no `test`;
- refuses, at `needs.network`, a network the farm does not define. The farm
  admin defines networks in the `c4-networks` ConfigMap in the operator's
  namespace: each key a network's name, each value a YAML list of rules
  (`- {host: api.studio.example, ports: [443]}`, or `cidr`, or `service:
  <namespace>/<name>`). A tool's jobs, its test's included, reach the
  networks it declares and nothing else; one that declares none is
  sealed. A `c4-networks` the farm cannot read as networks is refused
  here too (`422`, "the c4-networks ConfigMap in c4-system is malformed:
  ..."), for a farm admin to fix, not retried;
- refuses a name an agent's configuration holds ("render is an agent
  configuration on this farm; choose another name");
- writes the `Tool` only once nothing is left to refuse, so a refused
  publish leaves none behind;
- pins every image the tool names (`runs.image`, each sidecar's) by digest
  into `tool.lock` (`c4: lock/1`, `GET /schema/lock`), writing the
  published tree (`view` in the answer) with it, so the tree's ID covers
  the exact images. `c4/<name>` is the farm's own image,
  `<registry>/c4-<name>:<tag>` (`--hud-image-registry`, and
  `--hud-image-tag`, by default the operator's own version). A digest
  comes from a node that reports one for the image, else from its
  registry (anonymous bearer tokens, as ghcr.io and Docker Hub hand out);
  an image neither knows is refused at `runs.image`;
- with `dryRun`, answers `200` with the would-be `tool.lock` and `view`,
  and writes nothing;
- takes a tree that is one of the release's own tools (the server knows
  their IDs; the release builds each with its `tool.lock` written) as it
  is, its pins kept, and books it as released (`release`). Its test runs
  on the farm like any other's. No other tree is a release's, and the
  request cannot say otherwise;
- answers `200` with the version a tree already is, testing nothing again,
  unless that version's test failed, when it is tested again as the same
  version;
- otherwise records version `v<N>`, numbered as it is recorded, and runs
  the tool's `test` as a job on the test's folder of the tree, named by the
  tool, its scope and its tree (`notes-test-1a2b3c4d`, under 50
  characters), answering `201` once the job exists (`test.state:
  running`). A person's tool with no test is offered at once, marked
  `untested`;
- offers a tested version only when its test passes: the job succeeded and
  wrote every path the test's `expect` lists. A version whose test fails is
  kept in the tool's history with the reason, and the version before it
  stays in use. Offering sets every field of the `Tool` from the version
  (`spec.view`, egress from `needs.network`, sidecars, `spec.c4` from
  `needs.farm`), clearing what the version does not declare;
- with `farm`, writes the `Tool` in the operator's namespace and, once a
  version is offered, copies its tree into every project's store and grants
  it in projects with `scope: Grants`, projects made later included. A
  version no longer offered and used by no job loses its grants;
- is booked: the publish (`tool-publish`: who, the version, the tree, the
  test job), the test's pass or failure with its reason (`tool-test`), and
  the offer (`tool-offer`).

### GET /tools/{project}

```
{ tools: [ { name, title, summary, kind, version, view, image, scope: project|farm, untested? } ] }
```

The tools the project may use, at the versions in use: its own and the
farm's, a project's tool shadowing a farm tool of the same name. A tool
with no version offered yet is not listed.

### GET /tools/{project}/{tool}[?version=vN|c4id]

```
{ name, title, summary, kind, version, view, image, scope, untested?,
  tool, params, paramSchema, frames?, needs?, test: { job, state, seconds?, reason? },
  versions: [ { version, view, image, by, at, release?, test } ], earlier?: [ { version, view, image, release?, test } ] }
```

A version of a tool (the one in use by default), by name or by tree:
`tool` is its `tool.yaml` as JSON, keys in the order written, for a form
to draw from; `params` as `tool.yaml` writes them; `paramSchema` the JSON
Schema of a submission's `with` (a param is required when it has no
default; `x-show` and `x-from` carry `show` and `from`, and a
`RANGE_EXPR` has its pattern); and its test. `earlier` are the versions
older than the last 32 (up to 1000 more), by tree and how their tests
ended. A submission's `tool@v<N>` or `tool@c4…` resolves only a tree that
was a version of the tool, and only one whose test passed or that was
offered untested: a tree that was never published, or whose test failed,
is refused, however its `tool.yaml` names it. `c4 farm tool publish`
polls this until `test.state` is `ok` or `failed`.

## Networks (the c4-networks ConfigMap)

A tool declares what its units may reach by name (`needs.network:
[studio-api]`); the farm admin says what each name is. The names live in
one ConfigMap, `c4-networks`, in the operator's namespace (`c4-system`):

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: c4-networks
  namespace: c4-system
data:
  model-api: |
    - {host: api.anthropic.com, ports: [443]}
  studio-api: |
    - {host: api.studio.example, ports: [443]}
  pypi: |
    - {host: pypi.org}
    - {host: files.pythonhosted.org}
  licence-servers: |
    - {cidr: 10.20.0.0/24, ports: [27000, 27001]}
  asset-db: |
    - {service: pipeline/asset-db, ports: [5432]}
```

`model-api` is what the `agent` tool and the lead tools declare: the model
provider's API, through the egress proxy (no OAuth host is needed inside
the unit). `c4 farm up` defines it on a local farm; a plain install
defines no network, since letting units reach a model provider is the farm
admin's call. A farm on Bedrock, Vertex or a gateway defines `model-api` as
those hosts, or as a `service`. Units on host runners run outside the
cluster, where no NetworkPolicy applies.

- **Keys** are network names, as tools write them in `needs.network`:
  lower-case letters, digits and `-`, starting with a letter. `licences`
  is not a key: it names the licence servers of the pools a job's
  licences come from, and goes with them. Licences are not counted yet,
  so a tool declaring `network: [licences]` is refused at publish.
- **Each value** is a YAML list of rules. Each rule names exactly one of:
  - `host`: a host name, or `*.domain` for the names under it (not the
    domain itself: write both to reach both). Hosts are reached only
    through the farm's egress proxy, which checks each request's host and
    port against the rule, resolves the name itself and dials the address
    it checked. A name that resolves only to loopback, private, link-local
    (the cloud metadata service among them), carrier-grade NAT, the IPv6
    forms that carry an IPv4 address (6to4, Teredo, NAT64 well-known and
    local-use, IPv4-compatible) or other reserved addresses, or to the
    cluster's own addresses as the proxy reads them (each node's pod
    blocks and addresses, and the ServiceCIDR blocks where the cluster
    has those objects), is refused (403), unless one of the unit's `cidr`
    rules covers the address on the request's port. A cluster whose
    service block is not published as a ServiceCIDR and lies outside the
    private ranges is not known to the proxy. A name that does not
    resolve answers 502 ("could not resolve"), not a refusal;
  - `cidr`: an address block, reached directly, not through the proxy, so
    the proxy's log has no entry for it;
  - `service`: a cluster service as `<namespace>/<name>`, reached directly
    (its pods).

  `ports` limits a rule to those TCP ports (1 to 65535); with none, any
  port (a host rule's proxy requests are then allowed on any port). The
  syntax is checked when the ConfigMap is read: a host is a DNS name (or
  an address), a wildcard is `*.` before a domain with a dot in it, a
  `cidr` an address block, a `service` `<namespace>/<name>`.
- **What a job gets.** A tool job's units (`spec.tool`) reach the union of
  the rules of the networks its tool declares, and nothing else. Their
  NetworkPolicy (`c4-job-tool-<job>`) allows each `cidr` and `service`
  rule, and the egress proxy when any rule is a `host`. It allows DNS, and
  only to the cluster's DNS servers (`k8s-app: kube-dns` in kube-system),
  when a unit names something itself: a `cidr` or `service` rule. A unit
  that reaches only hosts gets the proxy's cluster IP in `HTTPS_PROXY`
  and no DNS. With no network declared the policy allows nothing at all,
  not even DNS: the units are sealed, and their records say so. The
  proxy reads the networks from the job (the one the unit's pod names as
  its controller, by UID) and this ConfigMap, so an admin's change
  applies to the next request a unit makes (after the proxy's short
  cache). `spec.tool` is the farm's: the HUD sets it when it lowers a
  tool's submission or test, refuses it in a `POST /jobs` body (`400`),
  and a job's `spec.tool` cannot be added, changed or removed after it
  is created.
- **What every other unit gets: nothing.** Every namespace with units
  has a NetworkPolicy, `c4-units-deny-egress`, written by the queue
  before it releases a unit there and put back if it is removed or
  changed:

  ```yaml
  spec:
    podSelector:
      matchExpressions:
      - {key: c4.avalanche.io/job, operator: Exists}
      - {key: c4.avalanche.io/unit, operator: Exists}
    policyTypes: [Egress]
  ```

  It selects every unit pod and allows no egress. NetworkPolicies add up,
  so a unit reaches what its tool job's policy or its tool's policy
  (`c4-tools-<tool>`, for an agent's `reasoning.tools`) allows, and
  nothing else. A job with no `spec.tool` and no unit naming a tool (a
  plain `POST /jobs` submission, MCP's raw image form, a tracker's job)
  has no network at all: no DNS, no HUD, no model provider. A tool job's
  units stay sealed while the job is deleted, after its own policy is
  gone. Inside a tool job a unit cannot name another tool in
  `reasoning.tools`: it holds, and the job's `Tools` condition says why.
  Once a unit of a job with no tool has run, the job says so in one
  sentence, in its `Network` condition and the summary's `noNetwork`:
  "this job names no tool, so it has no network: name a tool that
  declares one".
- **What a tool job may claim.** Before the queue releases a tool job's
  unit, it checks `spec.tool.network` against what the tool's version
  declares: `needs.network` in the `tool.yaml` of the tree the Tool
  records for that version (`spec.tool.view`, which the HUD's lowering
  sets), not the job's own copy. A job that claims a network the version
  does not declare, names no version, or names a tree that is no version
  of the tool is held, with the `Tools` condition saying why ("this job
  claims network pypi that tool fetch version v2 does not declare"). So a
  Tracker's template or a WorkJob written directly may name a tool, but
  never reach more than the tool declares.
- **A name the farm does not define** is refused when a tool is published
  (`needs.network`: "fetch declares network studio-db, which this farm
  does not define: a farm admin adds it to the c4-networks ConfigMap in
  c4-system (internal/hud/API.md, Networks)"), and refused again when a
  job naming it is submitted (`POST /submissions`, a tool's test, a dry run
  included: 422 with the same words), so a job is not made only to wait.
  A job made before the network was removed holds its units with that
  reason in its `Tools` condition.
- **A unit's job is no wider than the unit.** A job a unit submits, in any
  namespace, reaches at most the networks its own job's tool declares; a
  wider one is refused (403: "show-a/agent reaches only model-api, and
  this job would reach huggingface; submit it as a person, or as a unit
  whose tool declares it"). A unit whose job has no network submits only
  sealed jobs. Pools are not bounded this way: they follow the owner's
  rights and the pools' owners.
- **A refused request** gets the proxy's 403 naming the line to add: the
  narrowest network that allows the host (a rule naming the host before a
  wildcard, ports before any port, then the network with fewer rules:
  "add needs: {network: [pypi]} to fetch's tool.yaml and publish it
  again"), or, when no network allows it, that a farm admin must define
  one first.
- A malformed value (not a list, a rule naming none or two of host,
  cidr and service, or one whose syntax is wrong) makes every lookup fail
  with the network's name, so a unit holds rather than runs with less or
  more than its tool says, and a publish that declares a network is
  refused (`422`) until a farm admin fixes it.

## Relayed decisions

A person who decides in a tracker (approves a Version in Flow, reviews a
pull request) never signed in to the HUD. The tracker's sync relays the
decision with `via`: the Tracker (in the job's namespace), the entity that
carries it (`source`), the tracker user (`by`), whom the Tracker's
`people` map makes them (`person`), and the C4 ID of the stored tracker
event (`evidence`). The HUD takes it only from the sync, only for a job of
that Tracker (label `c4.avalanche.io/tracker`), and only when `people`
maps `by` to `person` and the wait is for that person (a `party:` wait
takes any user the map names); otherwise `403`, `400` for a malformed
`via`. The control record's `actor` is the person as the tracker named
them (`dana via tracker github (@dana)`), `authenticated` says who relayed
it, `relay` is `via`, and `custody` is `relayed`, or
`relayed-party` when `people` maps the user to a party rather than
a person (a model registry, which names no person). A decision made in the
HUD is `direct`.

## Grants

A person lets a Tracker submit work for them (docs/c4-sync.md, `for`), so
jobs it makes from their issues or tasks are theirs, and what is theirs
(a lease of their own sign-in) serves them. A grant is always the
signed-in person's own, signed as a control record of kind `grant`; the
HUD is the only writer of one. It ends (`until`); the person, or a
project admin (one allowed to delete `submitgrants` there), revokes it,
signed as kind `revoke`, and the grant is kept, marked. Nobody grants for
someone else, and a tracker's sync cannot grant itself.

| route                        | body or query                                                   | answer                                                                                                                                |
| ---------------------------- | --------------------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------- |
| `POST /grants`               | `{ project? \| namespace?, tracker, pools?, classes?, until? }` | `{ ok, message, grant, record }`; `until` is RFC 3339 or a span (`30d`, `72h`), default 30 days, at most 90; granting again renews it |
| `GET /grants`                | `?tracker={ns}/{name}`, `?person=`                              | `{ grants }`: by default the viewer's own; with `tracker`, who has granted it; only in projects the viewer sees                       |
| `DELETE /grants/{ns}/{name}` |                                                                 | `{ ok, message, grant, record }`; `409` when revoked already; `403` for anyone but the person or a project admin                      |
| `GET /trackers`              |                                                                 | `{ trackers }`: the Trackers in projects the viewer sees, to choose one to grant                                                      |

A grant is `{ namespace, name, project, person, tracker, pools?, classes?,
until, record, revoked?: { by, at, record }, active, why? }`; `why` says
why it does not serve now. The work queue checks it again each time it
releases a unit that mounts the person's credential lease: a unit running
when a grant is revoked or ends finishes its attempt, and the next is not
released.

A tracker is `{ namespace, name, project, kind, site, syncs?, ready, why?,
granted, mine? }`, in words a person reads: `site` is what it connects to
(`github.com/petrelfinch/pipeline`, `Flow Production Tracking,
Saltmarsh`), from its kind, endpoint and scope; `syncs` says what each
intake rule turns into jobs (`issues labeled render`, `tasks assigned to
a person, where step.Step.short_name is LGT`), with the tracker's own
field names; `ready` is c4-sync's Ready condition, false with `why` when
it failed, has not run yet, or has not resynced in three resync periods;
`granted` counts the people whose grants to it serve now; `mine` is the
viewer's own grant to it, active or not. On a farm without Trackers
installed the list is empty.

## The farm binding

The farm binding names whom the farm's records accrue to. `c4 farm up
--owner` names a local farm's one person; the farm binding, a signed
record, follows when `up` sends it (it does not yet). The installer binds
the farm once, through this route, and the HUD binds every deployment of
the farm to that owner. The binding is a
`sign` grant over every record kind (task, resolution, decision, job,
control, import and ingest). It is signed as a control record of kind
`bind`, and kept as the cluster-scoped `DeploymentBinding` named `farm`.

Only the HUD writes a binding (the `c4-deployment-bindings` policy), and a
binding never changes. A new owner is an owner change: a new binding that
follows this one, which a farm can't make yet.

| route                | body                                 | answer                                                                                                                                                          |
| -------------------- | ------------------------------------ | --------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `POST /farm/binding` | `{ owner, to, runFor?, onePerson? }` | `{ ok, message, binding, record }`; binding again the same way answers `existing: true` and changes nothing; `409` for any other owner, `runFor` or `onePerson` |
| `GET /farm/binding`  |                                      | `{ ok, binding, record }`; `404` while the farm has no owner bound                                                                                              |

`to` is `<farm>/*`, every deployment of the farm (`laptop-1/*`). The
first binding names the farm. `runFor`, on a managed farm, is the party
the owner runs it for: the provider owns the farm, and the record says
who it is run for. `onePerson` says the farm is the owner's alone (see
below); only a local farm's bind request says it, and it never goes with
`runFor` (`400`).

Who may bind:
- A person signed in who may create `deploymentbindings`.
- A development viewer with the wrangler or farm-admin role, on the
  installer's secret (token or cookie). That binding is graded `claimed`.

Refused (`403`): a unit, an agent (an MCP client naming one, or a tracker's
sync), and a development viewer without the installer's secret (open
development viewers).

A binding is `{ owner, to, runFor?, onePerson?, onePersonInEffect?, onePersonNote?, for, start, by, record, follows? }`.
`record` is the signed bind record's C4 ID. The DeploymentBinding also
carries the record itself (`spec.signed`), so a farm with no project
store yet still has the proof. Every project's store keeps a copy.

Being bound to someone is not using the farm alone. Only a binding the
installer marks `onePerson` makes its owner the farm's one person, who
holds every role, as `--single-person-farm` names someone on a farm with
no binding, and only on a HUD guarded by an installer token (not one with
open development viewers). There, `onePerson` is accepted at bind and
honoured at use; elsewhere a bind with it is refused, `403` "onePerson:
this HUD has no installer token, so the farm is not one person's". For a
`onePerson` binding the view adds `onePersonInEffect` and
`onePersonNote`: "while this HUD is reached only with this machine's
installer token", or "not in effect: this HUD has no installer token". That person may decide a wait addressed to a role, signed as
standing in for that role (`for: role:wrangler`). A wait for another
person stays that person's, and standing in for them is the farm's policy
(`decide.standIn`), as on any farm. The `onePerson` flag is part of the
signed bind record's `change`, and like the owner it never changes in
place. A shared farm bound to lee, without `onePerson`, has no such
person: lee keeps lee's own roles and waits. A managed farm never has one.

## Held tracker writes

c4-sync makes each write to a tracker once (docs/c4-sync.md). A
write that was interrupted, where nothing the tracker offers can tell
whether it happened, is **held**. It is neither sent again nor dropped
until a person looks in the tracker and says which. That person is the
job's owner, or anyone allowed to change the job. Nobody edits a
TrackerLink by hand.

| route                            | body or query                                         | answer                                                                                                                                                                                                                                                                                                                                                                                   |
| -------------------------------- | ----------------------------------------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `GET /held`                      | `?job={ns}/{name}`, `?tracker={ns}/{name}`, `?mine=1` | `{ held }`, oldest first, in projects the viewer sees                                                                                                                                                                                                                                                                                                                                    |
| `POST /held/{ns}/{link}/{field}` | `{ happened, note? }`                                 | `{ ok, message, record }`: `happened: true` and c4-sync takes it as written; `false` and it writes it again on its next pass. Signed as control record `confirm-write` or `deny-write`, booked on the job as kind `write-confirmed` or `write-denied`. `400` without `happened`, `403` for anyone but the job's owner or one allowed to change the job, `409` when nothing is held there |

A held write is `{ namespace, project, link, field, tracker, kind,
entity: { type, id, url? }, job?, owner?, state?, since, says, yours }`.
`says` puts it in words ("the GitHub comment on Issue 7, saying
running"), `entity.url` is where to look, and `yours` is true when the
job is the viewer's: the HUD lists those on the viewer's own page.

## People

A Tracker's `spec.people` offers each tracker login as a C4 person (or
`party:<name>`). That is the project's word, and alone it would let
whoever edits the Tracker make any login decide as anyone. So a mapping
counts once the person it names confirms it, as themselves, in the HUD:
the HUD signs the offer and the confirmation as one control record (kind
`people-confirm`) and writes a `TrackerPerson`, whose only writer it is
(the `c4-tracker-people` policy). For `party:<name>`, a member of the
party confirms. Never a tracker's sync, a unit or an agent: an agent may
tell the person what is offered, and the person confirms it.

| route                        | body or query                                | answer                                                                                                                                                                                                                                                                                        |
| ---------------------------- | -------------------------------------------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `GET /people`                | `?tracker={ns}/{name}`, `?mine=1`, `?all=1`  | `{ entries }`: every offer in projects the viewer sees, `offered` or `confirmed` (`all` adds `revoked`)                                                                                                                                                                                       |
| `POST /people`               | `{ project? \| namespace?, tracker, login }` | `{ ok, message, entry, record }`: the viewer confirms the login the Tracker offers as them. Confirming another person for a login revokes the first. `403` for anyone else, a sync, a unit or an agent (`X-C4-Client: mcp`, `X-C4-Agent`); `404` when the Tracker offers no one for the login |
| `DELETE /people/{ns}/{name}` |                                              | `{ ok, message, entry, record }`: the person, a party member or a project admin takes it back, signed as `people-revoke`; the entry is kept, marked. `409` when revoked already                                                                                                               |

An entry is `{ namespace, project, tracker, login, person, state, name?,
confirmed?: { by, at, record }, revoked?, yours, lets }`; `lets` says what
confirming lets the login do. A relayed decision's control record carries
its `chain` (chain/1): `by` the tracker user, `for` the mapped person, and
`under` the confirmation it rests on (`{ record, kind: people-map }`),
`accountable`. A decision by a login nobody has confirmed still goes
through, with `accountable: false` and a `gap` saying who has not
confirmed it.

## Changes from the first draft of the contract

| change                                    | why                                                                                                                                               |
| ----------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------- |
| job detail `units?` list is `unitList`    | `units` is already JobSummary's counts; a detail is a JobSummary plus fields, so the names collided                                               |
| `unitList[].pos` added                    | children of a generator are not in their group's position order, so the client needs each unit's position                                         |
| `groups[].generator`, `groups[].spans`    | the client draws an unexpanded generator across the rows its children will occupy, and splits it in place                                         |
| `riskUnits` added                         | yellow risk marks for jobs above 5,000 units, where `unitList` is omitted                                                                         |
| `JobSummary.blacklist` added              | blacklists are shown in the queue, the job view and on node cards                                                                                 |
| `objective.from` added                    | an inherited deadline names the job it came from                                                                                                  |
| `nodes[].running[].slots` added           | units can take more than one slot; the node card splits slots between the viewer's and other projects                                             |
| ML `classSummary.step`, `totalSteps`      | the ML lens shows progress in steps                                                                                                               |
| `usage` added to the snapshot             | usage is shown and billed as cluster-hours, container-hours, GPU-hours or a production window; bytes avoided is savings only                      |
| control responses `{ ok, message, ... }`  | the HUD shows the farm's own sentence after each control                                                                                          |
| `requeue` accepts an index array          | convenience; the unit set string is still the canonical form                                                                                      |
| `blacklist` accepts `remove`              | the job view offers "Allow node again"                                                                                                            |
| `JobSummary.work` added                   | progress by unit count misleads when frames differ tenfold in cost; the fill is drawn by work, from the same `W` and `W90` the scheduler plans on |
| `unitList[].work` added                   | each box can show how much of the job it is, and a running box fills against its own expected total                                               |
| `JobSummary.shot`, graph `shot` added     | a shot's chain of jobs (sim, FX, comp, transcode) is shown as one sheet                                                                           |
| `unitList[].parent`, `groups[].parent`    | expansion nests (job script, batches, frames, passes); the client draws each level and links children to their generator                          |
| `axis.batch` added                        | the HUD groups frames into batches the same way whether or not the job has batch generators                                                       |
| `unitList[].reads` added                  | a unit that reads several upstream frames (a temporal comp) draws its reads across rows, not only the same frame                                  |
| `GET /book` added                         | the farm's history with who signed each line, so a wrangler can read why a unit retried or a run yielded without kubectl                          |
| `nodes[].zone`, `.region`, `sites` added  | nodes are drawn at their sites on a map: the studio machine room and the cloud regions burst capacity comes from                                  |
| `unitList[].eta` added                    | the projected finish of each waiting unit, drawn as isochrones across the job view                                                                |
| `estimate.etaP80`, `etaP95`, `calibrated` | the whole finish band a person is shown, and whether it is calibrated from finished jobs, so the HUD can say "ready by ~14:20 (80%)"              |
| `objective.source`, `mode`, `suggestion`  | the deadline's source is always visible (you, suggested, tracking), with the farm's suggestion and its signed revisions                           |
| `GET /jobs/{ns}/{name}/deadline-options`  | the feasibility readout as a person picks a time: likelihood, boost, burst cost, displacement (named only in the viewer's projects), floor        |
| nudge `deadlineMode`, `nextSession`       | accept the suggestion, hand the deadline to tracking, or move a job that will miss its session to the next one in one click                       |
