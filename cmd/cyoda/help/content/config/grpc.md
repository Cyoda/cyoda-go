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
- `CYODA_CALLOUT_JOINED_RESPONSE_MAX_BYTES` — ceiling on the answer to a
  callback, a request a compute member makes under its transaction token. Such
  an answer is built in memory and sent only once the transaction has been let
  go of, so everything waiting on that transaction — the end of the callout
  included — waits behind those bytes. A larger answer fails the callback with
  `413 JOINED_RESPONSE_TOO_LARGE` and is never sent in part, on the HTTP door
  and on both gRPC doors — the unary one and the server-streaming one, where
  the frames of a chunked collection count together.
  The member's remedy is to page the read; raise this when a deployment's
  members legitimately read more in one callback, at the cost of memory held on
  the owning node. It does not govern ordinary, unjoined requests, nor the
  callback's own request body — that is a fixed 10 MiB, which no setting moves.
  Must be `> 0`; startup fails otherwise (default: `10485760`, 10 MiB)
- `CYODA_CALLOUT_JOINED_MAX_WAITERS` — how many of a compute member's callbacks
  may queue for one transaction behind the one holding it. Callbacks of one
  transaction are served one at a time; past the cap a callback is refused with
  `503 TOO_MANY_JOINED_REQUESTS`, retryable, having touched nothing. The count
  is deliberately conservative: it includes callers that are about to be
  admitted, and the waits that are never refused (the node's own work on the
  transaction, and a callback resuming after a callout of its own) take a place
  in it too. Read the value as a floor on how many callbacks are served, not an
  exact admission count — at a small setting a callback can be refused while the
  transaction is in fact free a moment later. Firing many callbacks at one
  transaction at once buys a member no speed, so the cap enforces documented
  advice rather than introducing a rule. Must be `> 0`; startup fails otherwise
  — there is no "unlimited" value (default: `128`)

  This setting bounds how many callbacks may queue, not how many bytes they hold.
  The cap is read before a callback's body is, so that a refusal costs
  the node no buffer, but the reading reserves nothing: callbacks that arrive
  together can all pass it, and each then buffers its whole request — up to the
  fixed 10 MiB above — before taking its place in the queue. There is no
  setting for the request side, and no byte ceiling on the queue as a whole.
  Sizing a node means multiplying: the cap, times the request bodies a member
  actually sends, times the transactions under callout at once. At the default
  of `128` the arithmetic reaches a gigabyte of request bodies for a single
  transaction if its member sends 10 MiB callbacks in bursts. Lower the cap for
  a member that sends large bodies; raise it for one that legitimately submits
  small ones in bursts.

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
