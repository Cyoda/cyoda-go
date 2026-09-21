---
topic: config.scheduler
title: "cyoda scheduled-transition scheduler configuration"
stability: stable
see_also:
  - config
  - config.cluster
  - config.grpc
  - run
---

# config.scheduler

## NAME

config.scheduler — scan-loop cadence, coordinator/distribution strategy, and expiry-grace env vars for the scheduled-transition runtime.

## DESCRIPTION

- `CYODA_SCHEDULER_ENABLED` (bool, default: `true`) — kill switch for the coordinator scan loop.
- `CYODA_SCHEDULER_SCAN_INTERVAL` (duration, default: `1s`) — coordinator scan cadence.
- `CYODA_SCHEDULER_BATCH_SIZE` (int, default: `100`) — max due tasks pulled per scan.
- `CYODA_SCHEDULER_DISTRIBUTION` (string, default: `round-robin`) — dispatch-target selection strategy: `round-robin` or `self`. Forced to `self` whenever `CYODA_CLUSTER_ENABLED=false`.
- `CYODA_SCHEDULER_COORDINATOR` (string, default: `lowest-node-id`) — coordinator-election strategy; the member with the lexicographically smallest node ID scans on each tick.
- `CYODA_SCHEDULER_REDISPATCH_BACKOFF` (duration, default: `30s`) — best-effort re-dispatch throttle window applied to a task once it is picked up, so the same due task isn't immediately re-dispatched on the next scan. It is a throttle, not a lease: a scheduled transition still running after this long is started a second time while the first is in progress. Only one of the two can commit, so the data stays correct, but the transition's processors run twice. One try of a callout that gets no answer already takes an answer limit (`CYODA_CALLOUT_RESPONSE_TIMEOUT_MS`, default `30000`), and a callout may take several; size this value above the longest scheduled transition you expect, or make the processors of scheduled transitions safe to repeat.
- `CYODA_SCHEDULER_EXPIRY_GRACE` (duration, default: `100ms`) — grace band above a scheduled transition's `timeoutMs` before it is expired instead of fired late; size to at least the maximum expected inter-node clock skew.

## SEE ALSO

- config
- config.cluster
- config.grpc
- run
