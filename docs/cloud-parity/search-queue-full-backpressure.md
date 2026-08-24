# Async search submit sheds load with `503 SEARCH_QUEUE_FULL` — Cloud twin-alignment spec

This document is the contract Cyoda Cloud implements to stay aligned with
cyoda-go's async-search submit backpressure. cyoda-go is the authoritative
implementation.

`503 SEARCH_QUEUE_FULL` is the one status the #472 search-SPI-surface
milestone adds to the wire; every other endpoint keeps the status set it
already had.

## Endpoint and response

**`POST /search/async/{entityName}/{modelVersion}`** (gRPC:
`EntitySnapshotSearchRequest`).

| Field | Value |
|---|---|
| Status | `503 Service Unavailable` |
| `properties.errorCode` | `SEARCH_QUEUE_FULL` |
| `properties.retryable` | `true` |
| `detail` | `SEARCH_QUEUE_FULL: async search queue is full — retry later` |

The gRPC door returns the same code in its error envelope (client-error
class), not an unclassified `SERVER_ERROR`. Both transports build the error
from one source — `search.QueueFullError()` — so status/code/message/retryable
cannot drift apart per transport.

The submit is rejected outright: no job row survives, so a client that
retries and succeeds sees exactly one job. A client MUST treat the status as
transient and retry with backoff; it is not a request-validity error and the
same request will succeed once capacity frees.

## Two triggers, one code

`SEARCH_QUEUE_FULL` signals *engine-side admission control refused the
submission*, from either of two guards:

1. **Worker-pool saturation** — every worker in the bounded async-search
   pool is busy and its submit queue is at capacity. Sized by
   `CYODA_SEARCH_ASYNC_WORKERS` and `CYODA_SEARCH_ASYNC_QUEUE`. This is a
   whole-node bound: it protects the node from unbounded concurrent scans.
2. **Per-tenant in-flight cap** — the submitting tenant already holds its
   full share of *this node's* async capacity, sized by
   `CYODA_SEARCH_ASYNC_MAX_PER_TENANT`. This is a fairness bound: it stops
   one tenant consuming the whole pool and starving the others. Scope is
   per node, matching the pool it protects; it is not a cluster-wide quota.

Both are backpressure on the same resource and both are retryable, so they
share the status and the code. Cloud MUST NOT split them into two codes; a
client's correct reaction (back off, retry) is identical.

## Invariants Cloud must mirror

1. Submit **fails fast** — it never blocks waiting for a worker, and never
   queues without bound. Correctness over availability applies in the other
   direction here too: shedding the request is the correct answer, and a
   silently-queued-forever submit is not an acceptable substitute.
2. The rejected submit leaves **no residue**: no job row, no cancel-registry
   entry, no heartbeat ticker. A subsequent status poll for a job the client
   never received an ID for is not a thing that can happen.
3. The status is `503` with `retryable: true` — not `429`, not `500`.
4. A tenant's own saturation must not be reported as a different condition
   from node saturation (see "Two triggers" above).

## Unreachable on a self-executing backend — and that is the parity statement

A backend implementing `spi.SelfExecutingSearchStore` (the commercial,
Cassandra-backed backend) runs async searches through its own
consumer/executor pipeline. `SubmitAsync` short-circuits and returns the job
ID **before** the engine's worker pool is ever consulted, so neither guard
above can fire and `SEARCH_QUEUE_FULL` is unreachable on that backend.

That is a deliberate, documented consequence of owning your own execution
pipeline, not a missing implementation: the engine pool it protects does not
exist there. What Cloud owes is the *equivalent* obligation, not the same
code path —

- Cloud's own async pipeline must bound its admission (per node and per
  tenant) rather than accepting unbounded concurrent scans.
- When it sheds, it must do so with **this same status, code and
  `retryable: true`**, so a client written against cyoda-go handles Cloud
  backpressure unchanged.
- Until Cloud's pipeline bounds admission, the absence of the status is a
  known gap on the Cloud side rather than an accepted divergence.

## Backend support

Engine-executed backends (memory, sqlite, postgres) all raise it from the
shared engine pool — it is engine behaviour, not plugin behaviour, so there
is no per-backend variation to reconcile among them. Coverage is an isolated
single-backend e2e (`TestE2E_AsyncSearch_QueueFull_503`) plus the gRPC
envelope test (`TestEntitySearch_SnapshotSearch_QueueFull_Envelope`);
deliberately not a cross-backend parity scenario, because saturating a
worker pool is a concurrency scenario and those stay out of the shared
parity suite.
