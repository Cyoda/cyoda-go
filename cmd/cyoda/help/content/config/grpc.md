---
topic: config.grpc
title: "grpc configuration"
stability: stable
see_also:
  - config
  - grpc
  - run
---

# config.grpc

## NAME

config.grpc — gRPC listener settings and compute-node credentials.

## SYNOPSIS

cyoda exposes a gRPC endpoint for compute-node integration. The listener port is configured
via `CYODA_GRPC_PORT`. External compute nodes authenticate with `CYODA_COMPUTE_TOKEN` and
connect to the endpoint specified by `CYODA_COMPUTE_GRPC_ENDPOINT`.

## OPTIONS

### gRPC listener

- `CYODA_GRPC_PORT` — gRPC listen port (default: `9090`)
- `CYODA_KEEPALIVE_INTERVAL` — seconds between server keep-alive pings to each
  compute member; also the transport keepalive idle time (default: `10`)
- `CYODA_KEEPALIVE_TIMEOUT` — seconds of inbound silence or write stall before
  a compute member is evicted; also the transport keepalive ack timeout
  (default: `30`)

### Compute-node callouts

A callout is one processor, criterion or function request sent to a compute
member. These settings apply on a single node and in a cluster alike.

- `CYODA_RETRY_FIXED_NUM_RETRIES` — retries after the first try, for a callout
  whose `retryPolicy` is `FIXED` or unset; `retryPolicy: NONE` always means one
  try. The normal number of tries is this plus one. Must be `>= 0`; startup
  fails otherwise (default: `3`)
- `CYODA_CALLOUT_RESPONSE_TIMEOUT_MS` — how long a node waits for one compute
  member's answer when the callout sets no `responseTimeoutMs` of its own.
  Must be `>= 1` and no larger than `CYODA_CALLOUT_RESPONSE_TIMEOUT_MAX_MS`;
  startup fails otherwise (default: `30000`)
- `CYODA_CALLOUT_RESPONSE_TIMEOUT_MAX_MS` — upper bound on `responseTimeoutMs`
  for processors, criterion functions and scheduled-transition functions.
  Workflow import refuses a larger value with `400 VALIDATION_FAILED`. The
  bound is a server setting, so a workflow exported from one deployment can be
  refused by another with a lower bound. A stored workflow whose value exceeds
  a bound lowered after it was imported is not clamped: its callout fails,
  naming this setting. Must be `>= 1`; startup fails otherwise
  (default: `60000`)

Tries multiply the answer limit. With the PostgreSQL backend a callout holds
its transaction's connection idle while it waits, and
`CYODA_POSTGRES_IDLE_IN_TX_TIMEOUT` (default `5m`) reclaims a connection idle
that long. The relation is documented, not enforced: keep
`(retries + 1) × the upper bound`, plus `CYODA_DISPATCH_WAIT_TIMEOUT` and
`CYODA_CALLOUT_HANDOVER_ALLOWANCE`, under that ceiling — 275 s at the upper
bound with every other default. The transaction token a compute member
receives outlives its try by `CYODA_CALLOUT_PASS_ALLOWANCE`, on a single node
too. These three are described in `config cluster`.

### Compute-node client

These variables are used by compute-node clients that connect to a running cyoda instance.

- `CYODA_COMPUTE_GRPC_ENDPOINT` — gRPC endpoint for the compute node to connect to,
  e.g. `localhost:9090` (required when running as a compute client)
- `CYODA_COMPUTE_TOKEN` — bearer token for compute-node authentication
  (required when running as a compute client)
- `CYODA_COMPUTE_HTTP_BASE` — HTTP base URL of the cyoda instance a compute node
  calls back into (e.g. to join the originating transaction); optional, enables
  callback-capable processors when set

## EXAMPLES

**Server (default port):**

```
CYODA_GRPC_PORT=9090
```

**Compute node client:**

```
CYODA_COMPUTE_GRPC_ENDPOINT=cyoda.internal:9090
CYODA_COMPUTE_TOKEN=my-token
```

## SEE ALSO

- config
- grpc
- run
