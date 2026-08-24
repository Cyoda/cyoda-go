# Batched delete fails closed with `409 DELETE_NOT_CONVERGED` — Cloud twin-alignment spec

This document is the contract Cyoda Cloud implements to stay aligned with
cyoda-go's batched conditional delete. cyoda-go is the authoritative
implementation.

`409 DELETE_NOT_CONVERGED` is a new wire code. It is the only status the
delete endpoint gains; every other status it answers is unchanged.

## Endpoint and response

**`DELETE /entity/{entityName}/{modelVersion}`** with `transactionSize` set
and no `pointInTime` (gRPC: `EntityDeleteAllRequest` carrying
`transactionSize`).

| Field | Value |
|---|---|
| Status | `409 Conflict` |
| `properties.errorCode` | `DELETE_NOT_CONVERGED` |
| `properties.retryable` | `true` |
| `detail` | `DELETE_NOT_CONVERGED: delete did not converge after N batches: entities matching the condition are being created as fast as they are removed; stop the concurrent writers, narrow the condition, or retry` |

The gRPC door returns the same domain code in its error envelope
(`Error.Code: CLIENT_ERROR`, message prefixed `DELETE_NOT_CONVERGED:`,
`Retryable: true`) — not an unclassified `SERVER_ERROR`. Both doors build the
error from one place, the delete's cycle guard, so status, code, message and
retryable disposition cannot drift apart per transport.

## What triggers it

The batched delete has no stable cursor across the deletes it is interleaved
with, so it re-selects the matching entities before every batch and terminates
when a selection pass yields nothing new. When entities matching the condition
are created at least as fast as they are removed, that pass never comes up
empty. Left unbounded such a request never returns: it keeps deleting and
re-selecting, growing its counts without limit, with nothing but a
client-supplied deadline to stop it.

The loop is therefore capped at a fixed number of selection cycles, sized to be
unreachable by any converging delete (a ten-million-row wipe at the smallest
sane batch size stays orders of magnitude below it). Exhausting the cap fails
the request.

Two shapes of the same endpoint cannot raise it, because they resolve their
target set exactly once:

- `transactionSize` absent — a single transaction over one resolved set.
- `pointInTime` set — a historical snapshot; deleting live rows cannot change
  what it matched, so a re-scan would never converge and is not used.

## Invariants Cloud must mirror

1. **The request fails; it does not return partial counts as if complete.**
   Batches committed before the cap stay committed — fail-closed governs the
   RESPONSE, not work already durable — but a response describing only the
   part of the delete that fitted inside the cap would be the substituted
   answer `.claude/rules/correctness-over-availability.md` forbids.
2. **The status is `409` with `retryable: true`** — not `500`, not `503`, and
   not the generic `CONFLICT`. `CONFLICT` is entity-level optimistic
   concurrency (one entity's version guard lost its race); an operator's
   remedy there is to re-read that entity and replay the write, which does
   nothing for this condition. The two must stay distinguishable on the wire.
3. **The detail names the cause and the remedies** — stop the concurrent
   writers, narrow the condition, or retry. It carries no internal state
   beyond the batch count.
4. **A converging delete is never tripped by the cap.** The bound exists only
   to terminate a delete that cannot finish; sizing it low enough to be
   reachable by a large legitimate wipe would be a defect.

## Backend support

Engine behaviour, not plugin behaviour: the guard lives in the delete's
streamed selection loop above the storage SPI, so memory, sqlite, postgres and
the commercial backend all raise it identically. There is no per-backend
variation to reconcile.

Coverage is a domain-level unit test driving the genuine insert-storm shape,
plus door-level tests that lower the cycle bound to reach the path
deterministically: `TestDeleteEntities_Batched_NonConvergence_409` (HTTP,
Postgres) and `TestRPC_EntityDeleteAll_NonConvergence_Envelope` (gRPC).
Deliberately not a cross-backend parity scenario — a storm is a concurrency
shape, and those stay out of the shared parity suite.
