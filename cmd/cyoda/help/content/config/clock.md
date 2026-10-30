---
topic: config.clock
title: "cyoda platform clock configuration"
stability: stable
see_also:
  - config
  - config.auth
  - config.cluster
  - run
---

# config.clock

## NAME

config.clock — turn on the controllable platform clock, a test feature for moving business time.

## DESCRIPTION

The platform clock is the time cyoda records as business time: entity and audit dates, transition times and scheduled due times. By default it is the real clock and cannot be changed.

- `CYODA_CLOCK_CONTROL_ENABLED` (bool, default: `false`) — build a controllable clock, give it to the storage backend and to everything that reads business time, and accept `POST /platform/clock`. With `false`, `POST /platform/clock` answers `403 CLOCK_CONTROL_DISABLED` and `GET /platform/clock` answers real time at rate 1.

This is a test feature, not for production; startup logs a WARN when it is on. The clock is instance-wide: one setting affects every tenant. Changing it needs a platform operator; in mock IAM mode the one mock principal is an admin, and any admin passes as platform operator. Use the same value on every node of a cluster: the nodes share the setting through gossip, a node that joins takes it from its peers, and the nodes need synchronised real clocks. A node that starts with no peer holding a setting starts on real time, and startup fails if the store holds times more than 1 s later than real time.

## SEE ALSO

- config
- config.auth
- config.cluster
- run
