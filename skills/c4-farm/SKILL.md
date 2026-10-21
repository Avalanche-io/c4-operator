---
name: c4-farm
description: Operate a C4 render, CI and ML farm on Kubernetes through its MCP tools (c4_*). Use when someone wants to submit a render, test or training job, or asks why a shot, change or training run is late, what is running or waiting, whether a deadline will be met, how to make a job finish sooner, which nodes are failing work, what burst capacity costs, or which files changed between two versions. The farm has no priorities: work is ordered by deadlines and estimates, and you change outcomes by moving a deadline, boosting, holding, requeuing units or approving capacity.
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
5. `c4_read_book` for what happened and who did it: captures and failures by
   node, retries, yields and resumes, capacity requests and approvals, and every
   hold, boost and deadline change, each with its signer and signed record. Give
   `namespace` and `name` for one job's history.

`c4_get_job` also reports `work`: progress weighted by slot-seconds, which is
the honest measure when frames differ in cost (a hero FX range at 3.4 times the
median can be half a shot's work in a fifth of its frames). `c4_list_jobs` filters by `shot` to
show a shot's whole chain (sim, FX, comp, transcode).

## Submitting work

Work goes in through a published tool. `c4_list_tools` lists the tools in the
person's projects, and `c4_describe_tool` gives one's parameters, frames and needs.
`c4_run_<tool>` submits one job of a tool: the tool's parameters, each an argument
of its own (one the tool does not take is refused, with the name it may have
meant), `frames` for a frames tool, the `shot`, when it is `due` (a milestone, a
time, a date or `suggested`, never a priority) and the `tree` it works on (a
view's C4 ID, or `job:<name>` for an earlier job's output). A tool published
while you work appears among your tools; if one changed since you read it, the
farm refuses the old form and the tool is drawn again: read it and call it again.
`c4_submit_job` takes a whole submission (`c4: submit/1`, as an object or as
YAML) for several jobs that follow each other, or one job in the raw form (a
`class` and an `image`) for work with no tool. A refusal is an answer with
`errors`, each at its path with its fix; send the answer's `key` again when you
retry after an error or a timeout, and nothing is made twice. Always call either with `dry_run: true` first and tell the person what
the farm expects (units, ready p50 and p90, slack, on track or not), or what it
refused and how to fix it; submit only when they agree. The jobs are owned by the
person, and the book records that you submitted them for them. `c4_publish_tool`
publishes a tool folder already in the project's store; the farm tests it, and
offers the version only once the test passes.

## Changing what happens

There are no priorities. To make a job finish sooner:

- `c4_deadline_options` first, with the times the user has in mind: for each, how
  likely the job is to be ready by then, the boost or burst (and its cost) that
  would make it, how many other jobs it would make late (named only in the user's
  own projects; say "would make 2 jobs late", never guess the others), and, when
  nothing makes it, the earliest the job can finish. It changes nothing.
- `c4_set_deadline` moves its deadline and pins it. This is usually what "raise its
  priority" means; the queue reorders everything else to meet it. With `mode`
  (`pin`, `suggested`, `tracking`) it changes who owns the deadline instead.
- `c4_boost_job` (1 to 2) asks for more slots than the deadline needs, from the
  job's own project's quota first.
- `c4_hold_job` stops new units of a job that should wait; with `stop_running` it
  also stops the units running now. Release it with `hold: false`.

A job submitted without a deadline gets one from the farm: the earliest it can
make without making another job late, or the next review session.
`c4_suggest_deadline` shows it, where the deadline comes from, the finish band
("ready by 14:20, 80%") and whether the suggestion has settled. A job that will
miss its session is offered the next one: `c4_move_to_next_session`.

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
