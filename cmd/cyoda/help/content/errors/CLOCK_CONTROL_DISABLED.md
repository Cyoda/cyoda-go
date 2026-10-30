---
topic: errors.CLOCK_CONTROL_DISABLED
title: "CLOCK_CONTROL_DISABLED — clock control is not enabled on this node"
stability: experimental
see_also:
  - errors
  - errors.CLOCK_BEFORE_RECORDED_TIME
---

# errors.CLOCK_CONTROL_DISABLED

## NAME

CLOCK_CONTROL_DISABLED — a request to change the platform clock reached a node that was not started with clock control enabled.

## SYNOPSIS

HTTP: `403` `Forbidden`. Retryable: `no`.

## DESCRIPTION

`POST /api/platform/clock` changes the clock every tenant of the instance shares. It is a test feature, and a node accepts it only when it was started with `CYODA_CLOCK_CONTROL_ENABLED=true`. Without that setting the node runs on real time and refuses every change with this code, after it has checked that the caller is a platform operator.

`GET /api/platform/clock` is not affected: without clock control it answers real time, a rate of 1 and `controllable: false`.

All nodes of a cluster must use the same setting. Start the instance with `CYODA_CLOCK_CONTROL_ENABLED=true` on every node to change its clock; the request is not worth repeating against a node that refused it.

## SEE ALSO

- errors
- errors.CLOCK_BEFORE_RECORDED_TIME
