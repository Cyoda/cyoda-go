---
topic: errors.CLOCK_NOT_CONFIRMED
title: "CLOCK_NOT_CONFIRMED — a node did not confirm a platform clock change"
stability: experimental
see_also:
  - errors
  - clock
  - config.cluster
---

# errors.CLOCK_NOT_CONFIRMED

## NAME

CLOCK_NOT_CONFIRMED — a node of the cluster did not confirm a platform clock change, or one of the steps of a move.

## SYNOPSIS

HTTP: `503` `Service Unavailable`. Retryable: `no`.

## DESCRIPTION

`POST /api/platform/clock` pushes every new clock setting to all other nodes of the cluster and answers only when each of them has echoed exactly that setting back. A node is reported when, within `CYODA_PROXY_TIMEOUT`:

- it did not answer;
- it refused the setting, because clock control is off there or the setting failed its validation;
- it answered a different setting than the one sent;
- it has no usable address.

The detail names the nodes that did not confirm. Nothing is rolled back: the nodes that confirmed keep the setting, so the cluster may hold different clocks. Start over by restarting the cluster with a fresh store.

The clock is a test feature for a static cluster that is idle, with one change at a time. A later change that answers 200 puts every node on one setting again, but it does not undo what ran while the nodes held different clocks.

## SEE ALSO

- errors
- clock
- config.cluster
