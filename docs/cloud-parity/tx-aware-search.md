# Tx-aware search pushdown + `trackingRead` — Cloud twin-alignment spec

This document is the contract Cyoda Cloud implements to stay aligned with
cyoda-go's in-transaction search behaviour. cyoda-go is the authoritative
implementation.

## Behaviour

`spi.Searcher.Search` is tx-aware by contract, not by a separate interface:
when an active transaction is present, `Search` honours it — read-your-own-writes
correct against the transaction's own uncommitted writes, producing results
identical to a full committed-scan-plus-in-memory-filter for the same
transaction state. There is no fallback to a full-model scan for an active
transaction; the engine only falls back when the search condition itself
cannot be translated to a backend predicate (unsupported condition type).

A new **optional** boolean request field, **`trackingRead`** (default
`false`), is exposed on the synchronous search surface only (HTTP
`POST /search/direct/{entityName}/{modelVersion}` and the gRPC `Search` RPC —
the two entry points reachable inside a joined transaction). Async search is
detached (background context, no transaction) and does not expose it.

- **`trackingRead=false` (default).** A plain snapshot read: returns the
  RYW-correct rows and records nothing in the transaction's read-set.
- **`trackingRead=true`.** Additionally records the **entities returned**
  into the read-set, so commit-time first-committer-wins validates them
  (entity-level; a concurrent write to one of the returned rows aborts the
  transaction with a conflict at commit).

Neither setting closes the phantom-write-skew gap already documented in
`docs/CONSISTENCY.md` §3/§7.3: a concurrent *insert* of a new entity matching
the predicate is never caught by a search, tracking or not. `trackingRead=true`
protects only the rows a search actually returned.

## Invariant Cloud must mirror

1. In-transaction `Search` results must be RYW-correct — a caller must see
   its own transaction's not-yet-committed writes reflected in search
   results, with no full-model materialisation as the mechanism.
2. `trackingRead` is additive and optional; omitting it must behave exactly
   as `trackingRead=false`.
3. `trackingRead=true` records only the returned entities into the
   transaction's read/conflict set — not a predicate lock, not the whole
   candidate set considered before filtering.
4. In-transaction `Search` requires co-location with the transaction owner
   (the node holding the transaction's buffer/connection state). A search
   routed to a node that has only joined the transaction, without owning its
   buffer, must not silently drop read-your-own-writes correctness.

## Non-goal

Full serializable (phantom-safe) search is out of scope — this feature
changes what a search contributes to the conflict-detection read-set, not
cyoda's documented isolation level (Snapshot Isolation + First-Committer-Wins,
`docs/CONSISTENCY.md` §1).
