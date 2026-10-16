---
name: c4-farm
description: Operate a C4 render, CI and ML farm on Kubernetes through its MCP tools (c4_*). Use when someone asks why a shot, change or training run is late, what is running or waiting, whether a deadline will be met, how to make a job finish sooner, which nodes are failing work, what burst capacity costs, or which files changed between two versions. The farm has no priorities: work is ordered by deadlines and estimates, and you change outcomes by moving a deadline, boosting, holding, requeuing units or approving capacity.
---

# Operating a C4 farm

The `c4-farm` MCP server (`c4-operator mcp`) exposes the farm the HUD shows. Every
tool acts as the user who connected: you see only their projects, and every change
is checked against their Kubernetes permissions and recorded as an Event naming them.

## Read before you act

1. `c4_farm_status` for the whole farm: slots in use, units waiting, failures, burst
   capacity, and the jobs at risk.
2. `c4_list_jobs` with `at_risk_only: true` for trouble, or filtered by `project`,
   `class` (`render`, `ci`, `ml`) or `phase`. Rows come in dispatch order: rank 1
   runs next.
3. `c4_get_job` for one job: its estimate (ETA p50 and p90, slack, required and
   attainable slots) and the feasibility message, which says in production's terms
   what is wrong ("needs 14 slots, can get 8; misses by 25 minutes"). Heavy frame
   ranges explain a render that is slower than its average frame suggests.
4. `c4_get_unit` for a failed or slow unit: attempts, node, log tail, events.

## Changing what happens

There are no priorities. To make a job finish sooner:

- `c4_set_deadline` moves its deadline. This is usually what "raise its priority"
  means; the queue reorders everything else to meet it.
- `c4_boost_job` (1 to 2) asks for more slots than the deadline needs, from the
  job's own project's quota first.
- `c4_hold_job` stops new units of a job that should wait; with `stop_running` it
  also stops the units running now. Release it with `hold: false`.

For failures:

- A unit that failed for a transient reason: `c4_requeue_units` with a unit set
  (`"14"` or `"0-9,14"`). This discards the units' previous results.
- One node failing one job's units: `c4_avoid_node`. Failing everything:
  `c4_disable_node` (running units finish) and tell the person who owns the node.

For capacity: `c4_capacity_and_cost` shows burst requests and what each needs.
`c4_approve_capacity` provisions paid capacity; state its cost and the jobs it is
for, and approve only when the user has asked you to.

## Data questions

Views are C4 IDs of whole trees. `c4_diff_views` lists the files that differ between
two views of a project from directory records alone, without a checkout: use it to
decide which CI targets a change touches, or what a render changed.

## Provenance questions

Every entity that does or decides work signs its own record: the node that ran a
unit, the controller that resolved its inputs, the queue that scheduled it, the HUD
for actions people and agents take (including yours), and the importer or ingest
that brought the content in. `c4_trace_file` returns the task record of one file,
with its declared AI use and its C2PA and OMC definitions. `c4_verify_custody`
walks from that file back to its ingest or import and reports whether every record
verifies, whether any step lacks a record, and who signed each step. Report gaps as
gaps: a step with no record is not a forgery.

## Conventions

- Jobs are `namespace/name`; tools take them as `namespace` and `name`.
- Times are RFC 3339 in the farm's time zone. Durations are seconds.
- Unit sets are run-length strings: `"0-99,102,140-141"`.
- Billing is by cluster-hours, container-hours, GPU-hours or a production window.
  Bytes avoided is a savings figure, never a charge.
