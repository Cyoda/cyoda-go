# Write visibility: a successful write is visible to every subsequent read — Cloud twin-alignment spec

This document is the contract Cyoda Cloud implements to stay aligned with
cyoda-go's write visibility. cyoda-go is the authoritative implementation.

## The contract

**A successful write response means the write is visible to subsequent reads
on every node.** Point reads, listings and searches issued after the
response — on any node — see the write, and so does a point-in-time read at
or after the write's instant. There is no opt-in and no client-side wait.

## `waitForConsistencyAfter` is retired

The boolean query parameter is removed from the seven entity write
operations (`create`, `createCollection`, `updateCollection`,
`updateSingle`, `updateSingleWithLoopback`, `patchSingle`,
`patchSingleWithLoopback`). It asked for a guarantee cyoda-go gives
unconditionally, so it could toggle nothing; keeping it would have been
advertised behaviour that does not exist. A request that still carries the
parameter is refused with `400 BAD_REQUEST: unknown query parameter
"waitForConsistencyAfter"`, as any query parameter an operation does not
declare is (`strict-member-names.md`), and the write is not made. No gRPC
event ever carried an equivalent field.

On Cloud the flag exists because a new transaction reads at the consistency
time captured when it was created, and concurrent transactions can hold that
time below the first transaction's submit time; the flag polls after commit
until the consistency time has passed the write so that a second transaction
can read it. That is a mechanism for satisfying the contract above, and it
belongs inside the storage layer.

## What a backend must do

1. Return from commit only after the write is visible to every read path
   (by id, listing, search, point-in-time) on every node.
2. Expose no post-commit rollout to any reader: a transaction's effects
   become visible atomically, never one row or one index at a time.
3. Give a new transaction a snapshot that is never below a commit already
   acknowledged to any client.

The in-tree backends (memory, sqlite, postgres) meet all three by
construction: commit returns after the write is visible; nothing runs
asynchronously after commit; there is no entity cache in front of the store;
multi-node postgres shares one store and keeps no node-local read state.

## Alignment consequence for Cloud

Cloud's HTTP write path must satisfy the contract without a client opt-in —
its consistency wait becomes unconditional — or the divergence is recorded
on the Cloud side. Cloud's own gRPC writes already wait by default.

## Related delete semantics

`pointInTime` on `DELETE /entity/{entityName}/{modelVersion}` (and on the
gRPC `EntityDeleteAllRequest`) selects the entities that existed at that
instant in committed state — the ambient transaction is ignored, as on every
point-in-time read — and deletes their current rows; an entity selected at
the instant but already gone is reported per id. This now holds on the
empty-body (whole-model) form too, which previously ignored the instant.
A joined request that created entities in the open transaction and then
issues a whole-model delete *with* an instant does not delete those buffered
entities; without an instant it still does.

`verbose` lists every id the delete attempted (the matched set); an id whose
delete failed also appears in `idToError` (HTTP) / `errorsById` (gRPC). The
empty-body form lists them too; it used to return an empty list beside a
non-zero count.
