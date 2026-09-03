# Direct search is bounded-or-fail — Cloud twin-alignment spec

This document is the contract Cyoda Cloud implements to stay aligned with
cyoda-go's direct (synchronous) search result-bounding behaviour. cyoda-go is
the authoritative implementation.

## Behaviour

Direct search is bounded-or-fail on every backend: `limit` caps the matched
result set rather than paging it. A backend that matches more entities than
`limit` allows must reject the request — never return a truncated prefix,
because a partial result is indistinguishable from a complete one. An exact
match at `limit` succeeds.

- **`limit` is required and positive on the wire** (the server resolves the
  direct-search default of 1000 when the client omits it, and rejects
  anything above the maximum of 10000). Exceeding a positive `limit` fails
  with `400 SEARCH_RESULT_LIMIT`.
- **A non-positive `limit` never reaches a backend.** The search service
  rejects one as a caller error before any store access; a storage plugin
  must reject one too rather than re-defaulting it.

Direct search does not paginate: there is no `offset`, on any transport, at
any layer. A caller needing more than `limit` allows — including an ordered
top-N over a large model (`sort` plus a small `limit`) — must use async
search, which snapshots the full matched set and pages over it.

## SPI-level contract

- `EntityStore.Search`'s `SearchOptions.Limit` carries the same
  bounded-or-fail meaning, and `Limit >= 1` is REQUIRED: the implementation
  MUST return `ErrSearchResultLimitExceeded` (not a truncated result) when the
  matched set exceeds it, and MUST reject `Limit <= 0` as a contract
  violation rather than reading it as unbounded. Streaming every match of a
  predicate is `Iterate`'s job, not `Search`'s.
- `MergePage` is renamed `MergeBounded`: the same k-way merge kernel, but it
  raises `ErrSearchResultLimitExceeded` instead of truncating, and its
  `offset` parameter is gone.
- `SearchOptions.Offset` is removed outright — no replacement field exists.

## Invariant Cloud must mirror

1. A matched set larger than a positive `limit` is a hard failure, not a
   truncation, on every code path (transaction-bound and point-in-time alike).
2. A non-positive `limit` never reaches a backend; a backend MUST reject one
   rather than re-defaulting it.
3. No direct-search code path exposes or honours an offset/page-skip
   parameter.

## Commercial-backend obligation

The commercial backend re-defaulted a non-positive `limit` to its own
built-in default. Under the SPI rule above it must reject one instead —
pinned by `spitest`. This is tracked in the commercial backend's own issue
tracker, not here.

## Backend support

Bounded-or-fail direct search is supported identically by memory, sqlite,
and postgres. The cross-backend parity suite validates the fail-not-truncate
behaviour at and above the limit on every in-tree backend.
