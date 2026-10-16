# HUD API, v1

The HUD web app (`internal/hud/web`) reads this API and nothing else. The real
backend (the HUD server in the manager) and the mock (`hack/hudmock`) both
implement it. DESIGN.md section 10 describes the HUD; this file is the wire
contract.

All paths are under `/api/v1`. Responses are JSON unless noted.

## Conventions

- **Times** are RFC 3339 strings in the farm's own offset, for example
  `2026-10-23T11:36:24-07:00`. The HUD shows every time in that offset (taken
  from `snapshot.now`), whatever the browser's time zone, so a wall display and
  a laptop agree.
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

The real backend authenticates with a bearer token (DESIGN 10.7). The mock
takes `?as=artist` or `?as=wrangler` on any request instead, including the
event stream and object URLs.

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
  viewer: { user, projects[], allProjects: bool },
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
  nodes: [ { name, pool, locality, state, slots, used, failuresLastHour, suspect,
             cacheBytes, cacheHitRate, running?: [ { job, unit, slots } ] } ],
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

| field                         | meaning                                                                                                   |
| ----------------------------- | --------------------------------------------------------------------------------------------------------- |
| `farm.slotsTotal`             | slots on ready nodes, including disabled ones (they still hold running units)                             |
| `farm.bytesAvoided`           | `bytesMaterialized - bytesFetchedFromStore`, today (DESIGN 11.1); the savings proof, shown as savings and never billed |
| `classes[].slo.type`          | `deadline`, `latency` or `throughput`                                                                     |
| `classes[].lens`              | the name of the HUD lens module for the class (`render`, `ci`, `ml`; anything else gets the generic lens) |
| `nodes[].state`               | `idle`, `busy`, `disabled`, `notReady`, `unprovisioned`, `warming`                                        |
| `nodes[].suspect`             | node faults across two or more jobs in the last hour; test and job failures do not count                  |
| `tiers.tiers[].name`          | `node`, `peer`, `regional`, `store`, in fetch order                                                       |
| `tiers.fanIn`                 | requests per minute entering each tier; each tier forwards only its misses                               |
| `capacity[].state`            | `PendingApproval`, `Provisioning`, `Provisioned`                                                          |
| `capacity[].prewarmViews`     | views the new nodes fetch before units land on them                                                      |
| `usage`                       | metered use today: what billing is based on. Billing is never by bytes                                    |
| `usage.clusterHours`          | hours the installation has run today (farm-wide, shown to every viewer)                                   |
| `usage.containerHours`        | pod run time per project and pool, not weighted by slots; only the viewer's projects                     |
| `usage.gpuHours`              | GPU-hours per project; only the viewer's projects                                                         |
| `usage.productionWindows`     | declared fixed-fee periods (for example "Show A production window, Oct 1 to Dec 20"); `active` if `now` is inside it |

`jobs` is in dispatch order, and `rank` is the position in that order within
the viewer's scope (1 is next).

### JobSummary

```
{
  namespace, name, project, class, driver, owner?, phase, order,
  units: { total, unevaluated, queued, blocked, running, retrying, succeeded, failed, held },
  unitStates: { unevaluated, queued, blocked, running, retrying, succeeded, failed },
  objective: { type, deadline?, latencyTargetSeconds?, from? },
  estimate: { remainingSeconds, p90RemainingSeconds, etaP50, etaP90,
              requiredSlots, attainableSlots, runningSlots, slackSeconds, feasible,
              message?, burst?: { pool, slots, cost, needsApproval } },
  rank, pools[],
  inputs: [ { materialization?, view?, fromJob? } ],
  output?: { view, changesets },
  dependsOn[], createdAt,
  nudge: { hold, boost },
  heavyRegions?: [ { from, to, factor, label? } ],
  blacklist?: [ nodeName ]
}
```

| field                            | meaning                                                                                                                 |
| -------------------------------- | ----------------------------------------------------------------------------------------------------------------------- |
| `phase`                          | `Queued`, `Running`, `Blocked` (waiting on `dependsOn`), `Held`, `Yielded` (a wrapped run that yielded), `Succeeded`, `Failed` |
| `order`                          | `binary`, `sequential`, `reverse`, `failure-rate` (CI), `gang` (wrapped ML)                                             |
| `units.held`                     | waiting units of a held job; they are not also counted in `queued`                                                      |
| `unitStates`                     | has no `held` key: a held job's waiting units are in `queued`, and the client draws them as held when `nudge.hold` is set |
| `unitStates.running`             | includes a generator while it is being evaluated                                                                         |
| `objective.deadline`             | the effective deadline: the job's own, or one propagated from a dependent, or ready time plus the latency target         |
| `objective.from`                 | the dependent job whose deadline binds, when the deadline is inherited                                                  |
| `estimate.remainingSeconds`      | wall-clock seconds from `now` to `etaP50`                                                                               |
| `estimate.p90RemainingSeconds`   | wall-clock seconds from `now` to `etaP90`                                                                               |
| `estimate.requiredSlots`         | `W90 / T` (DESIGN 9.9)                                                                                                  |
| `estimate.slackSeconds`          | `T - max(W90 / A, t_path)`; negative when the job will miss                                                             |
| `estimate.message`               | the feasibility warning in production's terms; also set for held and failed jobs                                       |
| `estimate.burst`                 | the smallest burst that restores feasibility, its estimated cost, and whether a person must approve it                  |
| `output.changesets`              | the number of unit changesets folded into the output view so far                                                        |
| `heavyRegions[].from`, `.to`     | axis values (frame numbers for render), inclusive                                                                       |
| `heavyRegions[].factor`          | the region's mean estimated cost over the job median                                                                    |
| `blacklist`                      | nodes the job will not use, added automatically after `nodeErrorLimit` failures or by a person                          |

## GET /jobs/{ns}/{name}

A JobSummary with these fields added:

```
{
  ...JobSummary,
  axis: { kind, start?, step?, labels? },
  groups: [ { key, label, units, generator?, spans? } ],
  unitList?: [ { i, label, state, attempts, node?, group, pos, risk? } ],
  riskUnits,
  progress: { "<i>": { fraction, measured } },
  costByFrame?: [ { frame, seconds?, estimate?, low?, high? } ],
  expansions?: [ { generator, children, at } ],
  edges?: [ { from, to } ],
  classSummary
}
```

| field                 | meaning                                                                                                                                  |
| --------------------- | ---------------------------------------------------------------------------------------------------------------------------------------- |
| `axis.kind`           | `frame`, `shard`, `rank`, `step`, `segment`, `package`; `start` and `step` are set for frames                                            |
| `groups`              | rows of the job view: passes (render), stages (CI), ranks (ML); `units` is the group's unit set                                         |
| `groups[].generator`  | the group holds generators                                                                                                               |
| `groups[].spans`      | for a generator group, the keys of the groups its children land in; an unexpanded generator is drawn across those rows                  |
| `unitList`            | one entry per existing unit; omitted above 5,000 units, when the client lays units out from `groups` (the k-th member is at position k) |
| `unitList[].state`    | a `unitStates` key, or `held`                                                                                                            |
| `unitList[].attempts` | the number of attempts so far                                                                                                            |
| `unitList[].pos`      | position on the axis (frame offset, shard number, rank)                                                                                  |
| `unitList[].risk`     | a waiting unit the scheduler's projection finishes after the deadline                                                                     |
| `riskUnits`           | the same as a unit set, available at any size                                                                                            |
| `progress`            | running units; `measured` is true when the unit reports progress itself, false when the fill is elapsed time over the estimate         |
| `costByFrame`         | one entry per frame of the last pass: `seconds` when measured, else `estimate` with its band `low` to `high`                             |
| `expansions`          | generators that have been evaluated: `children` is a unit set, `at` a time; a client that sees a new entry animates the split             |
| `edges`               | unit-level dependencies inside the job (beauty after shadow and reflection; integration after the shards); omitted above 5,000 units   |

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

## GET /objects/{project}/{c4id}

The object's bytes (preview proxies are PNG or JPEG). Served only to viewers
of the project. Objects are immutable, so responses may be cached forever.

## GET /graph

```
{ nodes: [ { id, class, driver, phase, etaP90?, deadline?, slackSeconds?, critical } ],
  edges: [ { from, to, view? } ] }
```

`id` is `namespace/name`. `deadline` is the effective (propagated) deadline.
`critical` marks the critical path: from each unfinished sink, the chain of
unfinished inputs that finish last. `view` is the upstream job's output view.
Nodes are in dispatch order.

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

Every control is a POST with a JSON body. In the real backend each one is a
Kubernetes write made as the user (DESIGN 10.5). Success is `200` with:

```
{ ok: true, message, job? | node? }
```

`message` is a sentence for the HUD to show ("sh040-fx-v003: held"). `job` is
the updated JobSummary; `node` the updated node. Errors are `4xx` with
`{ error }`.

| path                                  | body                                         | effect                                                                                                  |
| ------------------------------------- | -------------------------------------------- | ------------------------------------------------------------------------------------------------------- |
| `/jobs/{ns}/{name}/nudge`             | `{ hold?, holdStop?, boost?, deadline? }`    | `hold`: release no new units (`false` releases); `holdStop: true`: hold and stop running units; `boost` 1 to 2; `deadline` RFC 3339 |
| `/jobs/{ns}/{name}/requeue`           | `{ units }`                                  | queue the units again; `units` is a unit set string or an array of indexes                              |
| `/jobs/{ns}/{name}/blacklist`         | `{ node, remove? }`                          | add the node to the job's blacklist, or remove it                                                       |
| `/nodes/{name}/disable`               | `{ disabled }`                               | taint or untaint the node farm-wide; running units finish                                               |
| `/capacity/{pool}/approve`            | `{}`                                         | approve the pending capacity request in the pool                                                       |

## Changes from the first draft of the contract

| change                                   | why                                                                                                       |
| ---------------------------------------- | --------------------------------------------------------------------------------------------------------- |
| job detail `units?` list is `unitList`   | `units` is already JobSummary's counts; a detail is a JobSummary plus fields, so the names collided       |
| `unitList[].pos` added                   | children of a generator are not in their group's position order, so the client needs each unit's position |
| `groups[].generator`, `groups[].spans`   | the client draws an unexpanded generator across the rows its children will occupy, and splits it in place |
| `riskUnits` added                        | yellow risk marks for jobs above 5,000 units, where `unitList` is omitted                                 |
| `JobSummary.blacklist` added             | blacklists are shown in the queue, the job view and on node cards                                         |
| `objective.from` added                   | an inherited deadline names the job it came from                                                          |
| `nodes[].running[].slots` added          | units can take more than one slot; the node card splits slots between the viewer's and other projects    |
| ML `classSummary.step`, `totalSteps`     | the ML lens shows progress in steps                                                                       |
| `usage` added to the snapshot           | usage is shown and billed as cluster-hours, container-hours, GPU-hours or a production window; bytes avoided is savings only |
| control responses `{ ok, message, ... }` | the HUD shows the farm's own sentence after each control                                                  |
| `requeue` accepts an index array         | convenience; the unit set string is still the canonical form                                             |
| `blacklist` accepts `remove`             | the job view offers "Allow node again"                                                                    |
