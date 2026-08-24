# Tx-aware search pushdown + `trackingRead` — Cloud twin-alignment spec

This document is the contract Cyoda Cloud implements to stay aligned with
cyoda-go's in-transaction search behaviour. cyoda-go is the authoritative
implementation.

## Behaviour

`spi.Searcher.Search` is tx-aware by contract, not by a separate interface:
when an active transaction is present **and no `pointInTime` is set**, `Search`
honours it — read-your-own-writes correct against the transaction's own
uncommitted writes, producing results identical to a full
committed-scan-plus-in-memory-filter for the same transaction state. There is
no fallback to a full-model scan for an active transaction; the engine only
falls back when the search condition itself cannot be translated to a backend
predicate (unsupported condition type).

### Carve-out: point-in-time reads are committed-only

`SearchOptions.PointInTime != nil` is the one case where an active transaction
is **ignored**. A point-in-time read answers from committed state as of the
requested instant and never sees the transaction's own uncommitted writes; it
also records nothing in the read-set, so `trackingRead` has no effect on it.
The two dimensions do not compose: "as at instant T" and "plus writes that have
no commit time yet" have no consistent joint answer, and a backend that served
both would return rows whose visibility depends on which connection ran the
query rather than on T.

The same carve-out applies to every point-in-time read in the family, not just
`Search`: `GetAsAt`, `GetAllAsAt`, `GetPage(asAt)` and `Iterate(PointInTime)`.
It is wire-visible — the `X-Tx-Token` join middleware wraps the whole API mux,
so a compute-node callback issuing `GET /entity/{id}?pointInTime=…` inside the
transition's transaction gets `404 ENTITY_NOT_FOUND` for an entity that
transaction just created.

A caller that wants its own uncommitted writes back omits `pointInTime`.

Pinned by `spitest`'s point-in-time committed-only family and, over HTTP on
every backend, by the `CallbackTxJoin_PITCommittedOnly` parity scenario
(`e2e/parity/pit_committed_only.go`), which creates an entity inside a joined
transaction and requires a plain read to find it and a `pointInTime` read not
to. Memory and sqlite satisfied this structurally (they buffer in-transaction
writes off the store); PostgreSQL ran the query on the caller's own transaction
connection and did not, which is the divergence the parity scenario now blocks.

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

1. In-transaction `Search` results with no `pointInTime` must be RYW-correct —
   a caller must see its own transaction's not-yet-committed writes reflected
   in search results, with no full-model materialisation as the mechanism.
   With `pointInTime` set, the opposite is required: the ambient transaction is
   ignored and only committed state as of that instant is returned, across
   `Search`, `Iterate`, `GetAsAt`, `GetAllAsAt` and `GetPage(asAt)` alike.
   Conformance for this is pinned by `spitest` — a backend that answers a
   point-in-time read from its transaction buffer now fails the suite.
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
