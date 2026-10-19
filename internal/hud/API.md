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
of it (`--hud-trust-proxy`). A viewer sees the projects with a namespace where
it may list WorkJobs (SubjectAccessReview, cached for a minute). The mock, and
the real backend started with `--hud-dev-viewers` (tests and demos only), take
`?as=artist` or `?as=wrangler` on any request instead, including the event
stream and object URLs.

`viewer.role` sets where a new viewer's detail level starts: `artist`,
`supervisor`, `producer`, `wrangler`, `farm-admin`, `facilities` or
`pipeline-td`. The real backend reads it from a group named
`c4:role:<role>` or `c4-role-<role>`; a development viewer named after a
role has that role, and `name@role=projects` gives one. Without a role the
HUD guesses: wrangler for a viewer of every project, else artist.

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
  pools: [ { name, attrs: { class, gpuModel?, vram?, capacityType }, slots, used, costPerSlotHour,
             burst?: { maxSlots, provisioningSeconds, costPerSlotHour, requested, provisioned } } ],
  classes: [ { name, driver, slo: { type, p50Seconds?, p95Seconds?, turnaroundSeconds? }, lens } ],
  projects: [ { name, quotas: [ { pool, nominal, borrowingLimit, used } ],
                burstSpendToday, burstBudgetPerDay, bytesAvoided,
                workerHours: [ { pool, class, hours } ], gpuHours } ],
  jobs: [ JobSummary ],
  nodes: [ { name, pool, locality, zone?, region?, state, slots, used, failuresLastHour, suspect,
             cacheBytes, cacheHitRate, running?: [ { job, unit, slots } ],
             capped?, shapeSlots?, capacityReason? } ],
  sites?: [ { name, label, region, lat, lon, kind, localities[], tz? } ],
  gates?: [ { namespace, name, holder?: { job, unit, name?, since?, expectStart? }, waiting: [ ... ] } ],
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

| field                     | meaning                                                                                                                                                                                            |
| ------------------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `farm.slotsTotal`         | slots on ready nodes, including disabled ones (they still hold running units)                                                                                                                      |
| `farm.bytesAvoided`       | `bytesMaterialized - bytesFetchedFromStore`, today (DESIGN 11.1); the savings proof, shown as savings and never billed                                                                             |
| `classes[].slo.type`      | `deadline`, `latency` or `throughput`                                                                                                                                                              |
| `classes[].lens`          | the name of the HUD lens module for the class (`render`, `ci`, `ml`; anything else gets the generic lens)                                                                                          |
| `nodes[].state`           | `idle`, `busy`, `disabled`, `notReady`, `unprovisioned`, `warming`                                                                                                                                 |
| `nodes[].suspect`         | node faults across two or more jobs in the last hour; test and job failures do not count                                                                                                           |
| `nodes[].zone`            | the node's `topology.kubernetes.io/zone` label (an on-prem farm labels racks or rooms here)                                                                                                        |
| `nodes[].region`          | the node's `topology.kubernetes.io/region` label; it joins the node to a site                                                                                                                      |
| `gates`                   | each gate (a one-slot resource a namespace's jobs pass first come, first served, such as a merge gate): its holder and its line in order, each with when it joined and when it is expected to pass |
| `sites`                   | where the farm's nodes are, for the map; omitted when none are declared, and the HUD then shows sites and racks only                                                                               |
| `sites[].region`          | the region the site's nodes carry in `nodes[].region`                                                                                                                                              |
| `sites[].kind`            | `onprem` (a studio machine room) or `cloud` (a provider region)                                                                                                                                    |
| `sites[].localities`      | the `nodes[].locality` values at the site (racks, zones), in display order                                                                                                                         |
| `sites[].lat`, `.lon`     | the site's position in degrees, WGS 84                                                                                                                                                             |
| `sites[].tz`              | the site's time zone, an IANA name (`America/New_York`), so its clock shows its local time; absent shows the viewer's                                                                              |
| `tiers.tiers[].name`      | `node`, `peer`, `regional`, `store`, in fetch order                                                                                                                                                |
| `tiers.fanIn`             | requests per minute entering each tier; each tier forwards only its misses                                                                                                                         |
| `capacity[].state`        | `PendingApproval`, `Provisioning`, `Provisioned`                                                                                                                                                   |
| `capacity[].prewarmViews` | views the new nodes fetch before units land on them                                                                                                                                                |
| `usage`                   | metered use today: what billing is based on. Billing is never by bytes                                                                                                                             |
| `usage.clusterHours`      | hours the installation has run today (farm-wide, shown to every viewer)                                                                                                                            |
| `usage.containerHours`    | pod run time per project and pool, not weighted by slots; only the viewer's projects                                                                                                               |
| `usage.gpuHours`          | GPU-hours per project; only the viewer's projects                                                                                                                                                  |
| `usage.productionWindows` | declared fixed-fee periods (for example "Show A production window, Oct 1 to Dec 20"); `active` if `now` is inside it                                                                               |

`jobs` is in dispatch order, and `rank` is the position in that order within
the viewer's scope (1 is next).

### JobSummary

```
{
  namespace, name, project, class, driver, owner?, phase, order,
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
  waits?: [ { unit, name?, waitsFor, ask?, choices?, escalated?, since?, expectSeconds?, blocking?, needsYou?,
             do?, note?, actor?, at?, record? } ],
  acks?: [ { kind, units, actor, assignee?, note?, at, record? } ]
}
```

| field                                 | meaning                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                          |
| ------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| `phase`                               | `Queued`, `Running`, `Blocked` (waiting on `dependsOn`), `Held`, `Yielded` (a wrapped run that yielded), `Succeeded`, `Failed`                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                   |
| `order`                               | `binary`, `sequential`, `reverse`, `failure-rate` (CI), `gang` (wrapped ML)                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                      |
| `units.held`                          | waiting units of a held job; they are not also counted in `queued`                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                               |
| `units.planned`, `unitStates.planned` | generators not evaluated yet (also `unevaluated`, the older name, with the same value)                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                           |
| `units.locked`, `unitStates.locked`   | units waiting for a unit of the job that holds what they claim ("locked by")                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                     |
| `units.flagged`, `unitStates.flagged` | units a review escalated to a person, waiting on them (`needsPerson.reason` `review`)                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                            |
| `unitStates`                          | has no `held` key: a held job's waiting units are in `queued`, and the client draws them as held when `nudge.hold` is set                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                        |
| `unitStates.running`                  | includes a generator while it is being evaluated                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                 |
| `unitStates` phases                   | each unit is in one set: `planned` (a generator not evaluated yet), `queued` or `blocked` (waiting on its job's dependencies or on units of its own job); `preflight` (its pod prepares: placement, inputs, cache, image, license); `running` (the first attempt works); `retrying` (a later attempt works); `backoff` (an attempt failed, the retry waits for a slot); `egress` (the work is done and its output is being captured and added); `succeeded` (added and recorded); `failed` (failed after its retries: a person must act); `flagged` (a review escalated it to a person: requeue, take or accept it); `accepted` (failed, and a person accepted it). `units` counts the same sets |
| `estimate.risk`                       | the deadline risk shown: `none`; `atRisk` (the band at the job's quantile, 0.95 for a suggested deadline and 0.9 otherwise, ends after the deadline, still fixable); `willMiss` (late even with every slot it can get); `missed` (the deadline has passed, unfinished). Finished jobs are `none`                                                                                                                                                                                                                                                                                                                                                                                                 |
| `needsPerson`                         | set while the job waits on a person: `reason` is `failed` (failed units nobody has taken), `stuck` (no plan, no pool fits, a dependency failed), `approval` (burst waits for approval), `review` (a review escalated units to a person: they are `flagged`), `decision` (a wait unit waits for a person's decision), `measure` (its units finished short of its required measure), `quota` or `permission`; `since` is when it began                                                                                                                                                                                                                                                             |
| `measure`                             | the job's own measure of done, when it declares one (`spec.measure`): `{ name, value, of, done, at?, detail?, required? }`, from the latest reading a unit reported; `required` makes it the job's completion predicate                                                                                                                                                                                                                                                                                                                                                                                                                                                                          |
| `spend`                               | what the job cost so far, split by unit kind (`procedural` or `model`): `{ modelUnits, proceduralUnits, modelTokens, unpricedTokens?, tokenCost, modelSlotCost, proceduralSlotCost, modelShare }` in US dollars; `modelShare` is model units' part of the spend, 0 to 1; tokens reported without a price are counted in `unpricedTokens` and not in `tokenCost`                                                                                                                                                                                                                                                                                                                                  |
| `acks`                                | who took (`acknowledge`), was given (`assign`, with `assignee`) or accepted (`accept`) which failed units, and when; a requeue clears a unit's                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                   |
| `waits`                               | what the job waits for ("waits for"): its wait units (`waitsFor` is `person:<id>`, `party:<name>` or `clock:<time>`; `ask`, `choices`) and units a review escalated (`escalated`), open (`since`, `expectSeconds` from the server's measured decisions, `blocking` when unfinished work waits on it, `needsYou` for a person's open wait that holds up work or an escalated unit) or decided (`do`, `note`, `actor`, `at`, `record`). A wait holds no slot; its unit is in `unitStates.waits`                                                                                                                                                                                                    |
| `objective.deadline`                  | the effective deadline: the job's own, or one propagated from a dependent, or ready time plus the latency target                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                 |
| `objective.from`                      | the dependent job whose deadline binds, when the deadline is inherited                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                           |
| `objective.source`                    | where the deadline comes from: `spec` (a person, a tool or tracking), `suggested` (the farm) or `class` (the class's objective)                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                  |
| `objective.mode`                      | who owns the deadline: `Pinned`, `Suggested` or `Tracking` (DESIGN 10.7)                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                         |
| `objective.suggestion`                | the farm's suggested deadline, kept for reference also once a person pins another                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                |
| `suggestion.state`                    | `Estimating` (it may still move later, only if at risk) or `Settled`; the farm never moves a suggestion earlier, and a settled one not at all                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                    |
| `suggestion.policy`                   | `achievable`, `sessions` or `turnaround`; `quantile` is the finish percentile it meets, 0.95 by default: suggestions are generous, and tightening one is the person's choice (`deadline-options`)                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                |
| `suggestion.atRisk`                   | the finish at `quantile` is now after the suggestion (shown in orchid)                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                           |
| `suggestion.history[].reason`         | `first`, or `at-risk` for a later move the farm made because the suggestion was at risk                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                          |
| `suggestion.nextSession`              | for a sessions policy, the following session when the job is not expected to make its own: the one-click alternative                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                             |
| `suggestion.history[].record`         | the C4 ID of the queue's signed record of that value                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                             |
| `estimate.etaP50` to `etaP95`         | the finish band a person is shown: ready by then with probability 50, 80, 90 and 95%                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                             |
| `estimate.noEta`                      | when set, the job has no ETA and this says why in plain words (held, suspended, waiting for capacity, no deadline); `etaP50` and `etaP90` then carry the snapshot's time, so show the reason, not a time                                                                                                                                                                                                                                                                                                                                                                                                                                                                                         |
| `estimate.calibrated`                 | the band comes from finished jobs' outcomes (live calibration), not the estimator's own spread                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                   |
| `estimate.remainingSeconds`           | wall-clock seconds from `now` to `etaP50`                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                        |
| `estimate.p90RemainingSeconds`        | wall-clock seconds from `now` to `etaP90`                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                        |
| `estimate.requiredSlots`              | `W90 / T` (DESIGN 9.9)                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                           |
| `estimate.slackSeconds`               | the deadline as planned (the job's own, or the earlier one a dependent sets) less the shown `etaP90` (`etaP95` for a suggested deadline); negative when the shown finish is late; absent without a band. `feasible` keeps the all-capacity meaning                                                                                                                                                                                                                                                                                                                                                                                                                                               |
| `estimate.message`                    | the feasibility warning in production's terms; also set for held and failed jobs                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                 |
| `estimate.burst`                      | the smallest burst that restores feasibility, its estimated cost, and whether a person must approve it                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                           |
| `output.changesets`                   | the number of unit changesets folded into the output view so far                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                 |
| `heavyRegions[].from`, `.to`          | axis values (frame numbers for render), inclusive                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                |
| `heavyRegions[].factor`               | the region's mean estimated cost over the job median                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                             |
| `blacklist`                           | nodes the job will not use, added automatically after `nodeErrorLimit` failures or by a person                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                   |
| `shot`                                | the job's `c4.avalanche.io/shot` label; the HUD draws a shot's jobs (sim, FX, comp, transcode) as one sheet                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                      |
| `work`                                | progress weighted by work, in slot-seconds (seconds times the slots a unit holds); see below                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                     |

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
                 name?, kind?, usage?, reused?, parent?, exists?, blockedOn?, claims?, lockedBy?, review?, safepoint?, stopping?,
                 work: { spent, expected }, reads?: [ { job, units } ], eta? } ],
  riskUnits,
  progress: { "<i>": { fraction, measured } },
  costByFrame?: [ { frame, seconds?, estimate?, low?, high? } ],
  expansions?: [ { generator, children, at, document, error? } ],
  edges?: [ { from, to } ],
  holds?: [ { holder, name, on, waiting: [ { unit, name, on } ] } ],
  interruptions?: [ { unit, at, reason, node? } ],
  interrupted?,
  classSummary
}
```

| field                  | meaning                                                                                                                                                                                                                                                                                                                                                                                                  |
| ---------------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `axis.kind`            | `frame`, `shard`, `rank`, `step`, `segment`, `package`; `start` and `step` are set for frames                                                                                                                                                                                                                                                                                                            |
| `groups`               | rows of the job view: passes (render), stages (CI), ranks (ML); `units` is the group's unit set                                                                                                                                                                                                                                                                                                          |
| `groups[].generator`   | the group holds generators                                                                                                                                                                                                                                                                                                                                                                               |
| `groups[].spans`       | for a generator group, the keys of the groups its children land in; an unexpanded generator is drawn across those rows                                                                                                                                                                                                                                                                                   |
| `unitList`             | one entry per existing unit; omitted above 5,000 units, when the client lays units out from `groups` (the k-th member is at position k)                                                                                                                                                                                                                                                                  |
| `unitList[].state`     | a `unitStates` key, or `held`                                                                                                                                                                                                                                                                                                                                                                            |
| `unitList[].preflight` | for a unit in `preflight`: `{ step, bytes?, of? }`; `step` is `place` (waiting for a node), `stage` (inputs), `fetch` (cache), `image`, `license` or `start`. The node daemon and class drivers report steps in the pod's `c4.avalanche.io/preflight` annotation; otherwise the step comes from the pod's status                                                                                         |
| `unitList[].egress`    | for a unit in `egress`: `{ bytes?, of?, sealed, folded, verified }`; `sealed` once the output is captured (it has a C4 ID); the bytes come from the pod's `c4.avalanche.io/egress` annotation                                                                                                                                                                                                            |
| `unitList[].ack`       | for a failed unit someone took: `{ kind, actor, assignee?, note?, at, record? }`                                                                                                                                                                                                                                                                                                                         |
| `unitList[].accepted`  | for an accepted unit: who accepted it and when, the same shape                                                                                                                                                                                                                                                                                                                                           |
| `unitList[].safepoint` | for a running unit that announces safe points (`c4-unit safepoint`): `{ seq, at, everySeconds?, next, decision? }`: its last one; once two are seen, the interval between them; `next`, what the queue would decide at the next one as things stand (`stop`, `continue` or `undecided`: a stop is asked but no longer held, as after a restart); and `decision`, the last answer, `{ seq, do, why, at }` |
| `unitList[].stopping`  | for a running unit asked to stop softly: `{ reason, by?, nextSafepoint? }`: why, the deadline after which it is stopped anyway, and when its next safe point is expected                                                                                                                                                                                                                                 |
| `unitList[].attempts`  | the number of attempts so far                                                                                                                                                                                                                                                                                                                                                                            |
| `interruptions`        | the latest attempts the farm stopped (WorkJob status): `{ unit, at, reason, node? }`, reason the pod's DisruptionTarget reason or `Deleted`; `interrupted` counts them all. They spent no retry; the HUD draws the unit queued with an "interrupted" notch                                                                                                                                               |
| `unitList[].pos`       | position on the axis (frame offset, shard number, rank)                                                                                                                                                                                                                                                                                                                                                  |
| `unitList[].risk`      | a waiting unit the scheduler's projection finishes after the deadline                                                                                                                                                                                                                                                                                                                                    |
| `riskUnits`            | the same as a unit set, available at any size                                                                                                                                                                                                                                                                                                                                                            |
| `progress`             | running units; `measured` is true when the unit reports progress itself, false when the fill is elapsed time over the estimate                                                                                                                                                                                                                                                                           |
| `costByFrame`          | one entry per frame of the last pass: `seconds` when measured, else `estimate` with its band `low` to `high`                                                                                                                                                                                                                                                                                             |
| `expansions`           | generators that have been evaluated: `children` is a unit set, `at` a time, `document` the expansion document's C4 ID, `error?` why the queue refused it (it added nothing); a client that sees a new entry animates the split                                                                                                                                                                           |
| `edges`                | unit-level dependencies inside the job (beauty after the deep env and volumes layers; integration after the shards); omitted above 5,000 units                                                                                                                                                                                                                                                           |
| `axis.batch`           | frames per display batch: the render class's `chunk` when it is above 1, else the class fps (24, one second of screen time); frame axes only                                                                                                                                                                                                                                                             |
| `groups[].parent`      | the key of the generator group whose expansion creates this group's units (`batch` under `shot`, `usd` under `batch`)                                                                                                                                                                                                                                                                                    |
| `unitList[].parent`    | the index of the generator unit whose expansion created this unit; omitted for units the job planned                                                                                                                                                                                                                                                                                                     |
| `unitList[].review`    | what a review last decided for the unit, when more than a pass: `{ do, why?, note?, to?, by, passes?, with? }`: `rework` (with the change to its next attempts and how many reworks), `escalate` (to whom; the unit is `flagged`), `accept` (with its note; drawn pale), or `pass` after a rework                                                                                                        |
| `unitList[].name`      | the unit's name within the job, as its expansion document gives it (`C3.2/phase-1`); edges and inputs use it                                                                                                                                                                                                                                                                                             |
| `unitList[].blockedOn` | for a `blocked` unit, the first unit of its own job it waits on that has not finished: its name, else its label ("blocked on <x>"); the full set is in `edges`                                                                                                                                                                                                                                           |
| `unitList[].claims`    | what the unit claims (paths or resources it writes): its holds and claims                                                                                                                                                                                                                                                                                                                                |
| `unitList[].kind`      | `procedural` or `model`                                                                                                                                                                                                                                                                                                                                                                                  |
| `unitList[].reused`    | a succeeded unit that ran nothing: an earlier run of the same computation (process and input views), exact and verified, gave its output; drawn committed green with the reuse mark. `units.reused` counts them (also in `succeeded`)                                                                                                                                                                    |
| `unitList[].usage`     | a model unit's use of models over its attempts: `{ inputTokens, outputTokens, cacheReadTokens?, unpricedTokens?, cost, slotCost, costSource?, models: [ { model, inputTokens, outputTokens, cacheReadTokens?, unpricedTokens?, cost? } ] }`                                                                                                                                                              |
| `unitList[].lockedBy`  | for a `locked` unit, the unit of its job holding what it claims: `{ unit, name, on }` ("locked by <name>"), `on` the claim it waits for                                                                                                                                                                                                                                                                  |
| `holds`                | each unit holding claims that others wait on: `{ holder, name, on, waiting: [ { unit, name, on } ] }`, the waiting line in dispatch order                                                                                                                                                                                                                                                                |
| `unitList[].gate`      | the gate a unit of kind gate passes (a one-slot resource shared by the namespace's jobs, first come, first served); while it waits, `lockedBy` is `{ gate, position, holder?, expectStart? }`, `holder` as `job#unit`                                                                                                                                                                                    |
| `unitList[].exists`    | the probability a tentative unit runs, below 1 only; its expected work is weighted by it                                                                                                                                                                                                                                                                                                                 |
| `unitList[].work`      | the unit's work in slot-seconds: `spent` so far and `expected` in total (below)                                                                                                                                                                                                                                                                                                                          |
| `unitList[].reads`     | units of other jobs this unit reads, when that is not simply the same frame (a temporal comp reads frames n-1 to n+1); `units` is a unit set of that job                                                                                                                                                                                                                                                 |
| `unitList[].eta`       | the scheduler's projected finish of a waiting unit; the finish times of a job's waiting units are its isochrones                                                                                                                                                                                                                                                                                         |

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

| kind                                                                                     | signer             | entry                                                                                                    |
| ---------------------------------------------------------------------------------------- | ------------------ | -------------------------------------------------------------------------------------------------------- |
| `captured`                                                                               | node               | a unit's output captured as a changeset and folded into the job's output                                 |
| `failed`                                                                                 | node               | a unit's attempt failed, with the exit and the reason                                                    |
| `retry`                                                                                  | queue              | a failed unit queued again, away from the node it failed on                                              |
| `blacklist`                                                                              | queue              | a node added to a job's blacklist after `nodeErrorLimit` failures                                        |
| `expanded`                                                                               | queue              | a generator's expansion document became units                                                            |
| `yield`, `stop`, `yielded`, `resume`                                                     | queue              | a unit asked to yield at its next checkpoint (or stopped) to make room, the yield itself, and the resume |
| `capacity-requested`                                                                     | queue              | burst capacity requested for jobs at risk                                                                |
| `capacity-approved`                                                                      | hud or queue       | a person approved a request over budget, or the queue approved one within it                             |
| `provisioned`                                                                            | controller         | burst nodes ready                                                                                        |
| `hold`, `release`, `boost`, `deadline`, `requeue`, `disable`, `enable`, `avoid`, `allow` | hud                | a person's or agent's control                                                                            |
| `ingest`, `import`                                                                       | ingest or importer | a view entered the farm                                                                                  |
| `started`, `finished`                                                                    | queue              | a job began running, and its last unit ended                                                             |

Kinds are open: a client shows an unknown kind by its `message`.

In the real backend the book is read on demand, never watched, from these
sources:

- Kubernetes Events on WorkJobs in the viewer's namespaces, and on
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
made strictly increasing, so it is stable from one request to the next.
Events expire after the API server's event TTL (an hour by default); the
signed records they name stay in the project store, and decisions stay in
WorkJob status.

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
`{ error }`.

| path                          | body                                                                   | effect                                                                                                                                                                                                                                                                                                                                                                                                              |
| ----------------------------- | ---------------------------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `/jobs/{ns}/{name}/nudge`     | `{ hold?, holdStop?, boost?, deadline?, deadlineMode?, nextSession? }` | `hold`: release no new units (`false` releases); `holdStop: true`: hold and stop running units; `boost` 1 to 2; `deadline` RFC 3339, which pins it; `deadlineMode` `Pinned` (alone, pins the deadline the job has now), `Suggested` or `Tracking`; `nextSession: true` moves the deadline to `objective.suggestion.nextSession` and pins it (`409` when none is offered)                                            |
| `/jobs/{ns}/{name}/ack`       | `{ units, kind?, assignee?, note? }`                                   | a person takes failed units: `kind` is `acknowledge` (default: the viewer has them), `assign` (to `assignee`) or `accept` (the failure stands; they count as done, so nothing waits on them). Only failed units are taken (`409` when none is). Written as a request in the `c4.avalanche.io/acks` annotation that the queue carries out once; it shows in `acks` and the job's book                                |
| `/jobs/{ns}/{name}/decide`    | `{ unit, do, note? }`                                                  | a person decides what waits for them: a wait unit (`do` is `approve`, one of its `choices`, or `fail`) or a unit a review escalated (`accept`, `rework` or `fail`). `unit` is a name or an index; `409` when nothing open waits as that unit. Signed first; written with its record as a request in the `c4.avalanche.io/decide` annotation that the queue carries out once; it shows in `waits` and the job's book |
| `/jobs/{ns}/{name}/requeue`   | `{ units }`                                                            | queue the units again; `units` is a unit set string or an array of indexes                                                                                                                                                                                                                                                                                                                                          |
| `/jobs/{ns}/{name}/blacklist` | `{ node, remove? }`                                                    | add the node to the job's blacklist, or remove it                                                                                                                                                                                                                                                                                                                                                                   |
| `/nodes/{name}/disable`       | `{ disabled }`                                                         | taint or untaint the node farm-wide; running units finish                                                                                                                                                                                                                                                                                                                                                           |
| `/capacity/{pool}/approve`    | `{}`                                                                   | approve the pending capacity request in the pool                                                                                                                                                                                                                                                                                                                                                                    |

## Submitting

What `c4 farm submit` calls, and MCP's `c4_submit_job` wraps. Each write
is checked like a control (a SubjectAccessReview for creating
materializations, or workjobs, in the project's namespace) and made with the
HUD's service account; the bytes go through the manager's own store access,
so no store credential leaves the cluster. `{project}` is a project the
viewer sees; anything else is `404`.

### POST /objects/{project}/missing

```
{ ids: [c4id] }  ->  { missing: [c4id] }
```

The IDs the project's store lacks, in the order given, without duplicates.
At most 100,000 per request.

### PUT /objects/{project}/{c4id}

The body is the object's bytes. It is held on the manager's disk and
identified first: `400` if its C4 ID is not `{c4id}`, so the store never
holds bytes under the wrong name. `200 { ok, id, stored, bytes? }`;
`stored` is false when the store already had it.

### POST /views/{project}

```
{ files: [ { path, id, size, exec? } ] }  ->  { view, files, bytes }
```

Records a tree of stored objects as a view: its directory records go into
the store and `view` is the tree's C4 ID, the same ID `c4-operator tree-id`
gives the directory on disk. Paths are clean, relative and slash-separated.
`409 { missing: [c4id], files, bytes }` when an object is not in the store.

### GET /views/{project}/{c4id}/files[?path=prefix&limit=N]

```
{ view, files: [ { path, id, size, exec? } ], truncated? }
```

The files of a view, or of the subtree at `path`; the bytes of each come
from `GET /objects/{project}/{c4id}`. This is what `c4 farm get` reads.

### POST /jobs

```
{ project? | namespace?, name?, type?, stage?, shot?, spec, inputs?: [ { view? | fromJob?, mountPath, readOnly? } ],
  aiUsage?, dryRun? }
->
{ ok, dryRun?, namespace, name, project, link, message, materializations?,
  estimate?: { units, work50, work90, eta50?, eta80?, eta90?, eta95?, deadline?, deadlineSource,
               suggestion?: { deadline, state, policy, quantile, nextSession? },
               slackSeconds, feasible, atRisk, calibrated?, message? },
  estimateError? }
```

`spec` is a WorkJob spec (`c4.avalanche.io/v1alpha1`). The HUD names the
job `<shot>-<stage>-vNNN` (stage defaults to type) when `name` is empty, one version past the latest
such job in the namespace; makes a Materialization for each input view
(named from the view's ID, so the same view is one Materialization);
adds `fromJob` inputs to `dependsOn`; sets the deadline mode to `Suggested`
when the spec has no deadline; and stamps `c4.avalanche.io/owner` (the
viewer), `c4.avalanche.io/submitted-via` (`hud` or `mcp`), and the labels
`c4.avalanche.io/shot` and `c4.avalanche.io/job-type`. `aiUsage` becomes
the job's `c4.avalanche.io/ai-usage` annotation.

`estimate` is what the queue expects of the job against the farm as it last
planned it. `slackSeconds` is the deadline less `eta90` (`eta95` for a
suggested deadline), as for a running job. With `dryRun`
nothing is created; the answer is `200`, or `503` when the queue has not
planned yet (or runs on another replica) and `422` when the job's class
cannot plan it. Without `dryRun` the answer is `201`, with
`estimateError` when there was no estimate; `409` when the name is taken.
A book entry of kind `submit` names the viewer (or the agent for the
viewer, through MCP).

A job may carry `c4.avalanche.io/accept-validated: "true"` (experimental,
staging): it accepts a validated recompute of an input in place of moving
it to another site. A validated recompute's output differs from the
recorded one, so only the consuming job may choose it; exact recomputes and
identical outputs need no consent, and volatile ones are never offered.

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
