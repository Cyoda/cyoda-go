---
topic: config.scheduler
title: "cyoda scheduled-transition scheduler configuration"
stability: stable
see_also:
  - config
  - config.cluster
  - config.grpc
  - run
  - scheduled-tasks
---

# config.scheduler

## NAME

config.scheduler — how each node claims and runs scheduled transitions: claim cadence, run limits, liveness, retries and shutdown.

## DESCRIPTION

Every node claims due scheduled tasks and runs them itself. A node proves it is alive with a heartbeat; another node takes over its tasks only after `CYODA_SCHEDULER_STALE_AFTER` without one. Startup fails on a value outside its rule.

- `CYODA_SCHEDULER_ENABLED` (bool, default: `true`) — kill switch: a node with `false` claims no scheduled task.
- `CYODA_SCHEDULER_SCAN_INTERVAL` (duration, default: `1s`) — how often the node claims due tasks. A node also claims at once when a run slot frees after a claim took every slot. Must be `> 0`.
- `CYODA_SCHEDULER_MAX_RUNS` (int, default: `8`) — most scheduled runs one node holds at once. Must be `>= 1`.
- `CYODA_SCHEDULER_MAX_RUNS_PER_TENANT` (int, default: `4`) — most runs of one tenant on one node. The limit is per node: in a cluster of N nodes, one tenant can hold up to N times this many runs. Must be between `1` and `CYODA_SCHEDULER_MAX_RUNS`.
- `CYODA_SCHEDULER_HEARTBEAT_INTERVAL` (duration, default: `15s`) — how often a node records that it is alive. Must be `> 0`.
- `CYODA_SCHEDULER_STALE_AFTER` (duration, default: `2m`) — how long a node may go without a heartbeat before another node takes over its runs. Must be at least `50s + 3 × CYODA_SCHEDULER_HEARTBEAT_INTERVAL`, and the same on every node of a cluster. A node whose heartbeats keep failing cancels its own runs before this time passes.
- `CYODA_SCHEDULER_MAX_LOST_OWNERS` (int, default: `3`) — a task whose node is lost this many times ends FAILED `OWNER_LOST_REPEATEDLY`. Must be `>= 1`.
- `CYODA_SCHEDULER_RETRY_DELAY` (duration, default: `30s`) — delay before the first retry of a run that failed without handing unsafe work to a compute node. It doubles on each further failure and never passes the transition's `timeoutMs`. Must be `> 0`.
- `CYODA_SCHEDULER_RETRY_DELAY_MAX` (duration, default: `15m`) — the retry delay never grows past this. Must be `>= CYODA_SCHEDULER_RETRY_DELAY`.
- `CYODA_SCHEDULER_SHUTDOWN_DRAIN` (duration, default: `20s`) — on shutdown, how long the node waits for its runs before it cancels them. A run whose unsafe processor callout is in flight is not cancelled. Must be `>= 0`.

## SEE ALSO

- config
- config.cluster
- config.grpc
- run
- scheduled-tasks
