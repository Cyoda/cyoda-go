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

- **`limit > 0`** caps the matched set. Exceeding it fails with
  `400 SEARCH_RESULT_LIMIT`.
- **`limit <= 0`** means unbounded. A storage plugin must not substitute a
  default of its own — the calling engine, not the plugin, resolves the
  direct-search default (1000) and maximum (10000) before invoking the
  plugin.

Direct search does not paginate: there is no `offset`, on any transport, at
any layer. A caller needing more than `limit` allows — including an ordered
top-N over a large model (`sort` plus a small `limit`) — must use async
search, which snapshots the full matched set and pages over it.

## SPI-level contract

- `Searcher.Search`'s `SearchOptions.Limit` carries the same bounded-or-fail
  meaning: `Limit > 0` caps the matched set and the implementation MUST
  return `ErrSearchResultLimitExceeded` (not a truncated result) when
  exceeded; `Limit <= 0` MUST be treated as unbounded.
- `MergePage` is renamed `MergeBounded`: the same k-way merge kernel, but it
  raises `ErrSearchResultLimitExceeded` instead of truncating, and its
  `offset` parameter is gone.
- `SearchOptions.Offset` is removed outright — no replacement field exists.

## Invariant Cloud must mirror

1. A matched set larger than a positive `limit` is a hard failure, not a
   truncation, on every code path (transaction-bound, point-in-time, and
   fallback in-memory filtering alike).
2. `limit <= 0` is a deliberate request for the complete matched set; the
   backend must not re-default it to its own cap.
3. No direct-search code path exposes or honours an offset/page-skip
   parameter.

## Known commercial-backend gap

The commercial backend already implements bounded-or-fail search, but its
current implementation re-defaults a non-positive `limit` to its own
built-in default instead of treating it as unbounded — a divergence from
rule 2 above. This is tracked in the commercial backend's own issue tracker,
not here.

## Backend support

Bounded-or-fail direct search is supported identically by memory, sqlite,
and postgres. The cross-backend parity suite validates the fail-not-truncate
behaviour at and above the limit on every in-tree backend.
