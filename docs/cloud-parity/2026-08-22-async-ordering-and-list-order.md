# Async result ordering and list order — Cloud twin-alignment note

This document is the contract Cyoda Cloud implements to stay aligned with
cyoda-go's #472 search SPI-surface milestone. cyoda-go is the authoritative
implementation; the behaviour described here is derived directly from its
design spec (`docs/superpowers/specs/2026-08-22-472-search-spi-surface-design.md`)
and implemented code.

## 1. Async result ordering is contractual, end-to-end

An async search job's results are saved — and therefore read back via
`GET /search/async/{jobId}` — in the request's `OrderBy`. When the request
specifies none, the engine substitutes an explicit entity-ID `OrderBy`, so a
job's results are never in an unspecified order: they are either the
requested order, or the owning engine's canonical entity-ID order (§2).

**Current state:** on cyoda-go, every engine-executed backend (memory,
sqlite, postgres) honours this — the ordered `Iterate` surface streams rows
in `OrderBy` order and `SaveResults` preserves yield order as save order as
`GetResultIDs` page order. Cyoda Cloud's async execution today **ignores**
the requested ordering — a live divergence this note flags as a tracked bug
on the Cloud side, not an accepted difference. Achieving it does not require
a schema addition: storing result rows keyed by sort-value byte encodings
makes read-back order native storage order (prior art exists in Cyoda
Cloud's own distributed-reporting service; details shared with the Cloud
backend maintainers directly). Sequencing note: this overlaps
`spi#31`'s async wire-syntax translation work — implement ordering support
against whatever request representation that work lands, rather than
building a second parser over the raw blob it replaces.

## 2. List order is per-engine canonical, not cross-backend identical

`GetPage` (backing `GET /entity/{entityName}/{modelVersion}`, paged entity
listing) orders by **the engine's own canonical entity-ID order** — one
total, stable, deterministic order, used consistently everywhere that engine
orders by entity ID: `GetPage`, the tie-break under a user-field `OrderBy`,
and an explicit entity-ID `OrderBy`.

This order is **engine-specific, not identical across engines** — that is a
deliberate design decision, not an oversight left unresolved. Cross-engine
identical order buys nothing real: no API contract promises a specific
order, and paging across a backend migration is meaningless regardless.
Demanding identical order across engines would also make a
natively-time-clustered schema unserviceable without an added, unneeded
model-wide sort.

**Per-engine canonical order:**

| Backend | Canonical entity-ID order |
|---|---|
| memory | Byte-wise ascending (Go `<`) |
| sqlite | Byte-wise ascending (SQLite default `BINARY` collation) |
| postgres | Byte-wise ascending (`COLLATE "C"`, pinned explicitly) |
| commercial (Cassandra-backed) | Its own native `timeuuid` (creation-time) clustering order — becomes its documented canonical order when it adopts `GetPage` |

**Public API contract:** list order is stable and deterministic; the
specific order is storage-engine-specific (entity-ID based). This line
appears on the `GET /entity/{entityName}/{modelVersion}` OpenAPI operation
description and the corresponding `cyoda help crud` topic text.

**Impact on the commercial backend:** adopting `GetPage` visibly changes its
engine-side `ListEntities` order — from today's engine-sorted byte-wise
order to its own documented native (timeuuid) order. That change is
sanctioned by the per-engine ordering contract above, not a regression to
reconcile against cyoda-go's in-house backends. A k-way merge over its
time-sorted shard cursors can stream globally ordered IDs from its existing
schema: O(page) memory, O(offset) reads for the offset, no schema addition,
no O(model) sort.

**Other access paths affected by the same milestone**, notified here for
completeness (each has its own SPI contract, referenced rather than
reproduced):

- `GetVersionByTransaction` (earliest version carrying a given transaction
  ID) has no by-transaction access path in the commercial backend's change
  table today — the transaction column sits behind the version in the
  clustering order. Its available options are a scan of the entity's own
  change partition (bounded by that entity's history; must scan past a
  match to prove "earliest" since the clustering is version-descending), or
  a schema addition. Backend maintainers' call.
- `GetVersionMetadata` replaces `GetVersionHistory` (removed, pre-1.0, no
  shim) as the metadata-only history read backing
  `GET /entity/{id}/changes` and the audit-event search's merge. `Deleted`
  is now canonical (derived from change type), replacing the
  backend-divergent "entity is nil on some backends" probe.

## 3. Not in scope here

- The explicit-`OrderBy` comparison classes (`OrderText`/`OrderNumeric`/
  `OrderBool`/`OrderTemporal`) remain identical across all backends by
  contract — see `search-sort.md`. Only the entity-ID canonical order (used
  as the async/list default and as the tiebreaker) is per-engine.
- The commercial backend's recovery model for orphaned async jobs
  (rebalance-driven today vs. liveness-driven via `Heartbeat`/`ClaimStale`
  on cyoda-go) is a separate, already-flagged gap — not repeated here.
