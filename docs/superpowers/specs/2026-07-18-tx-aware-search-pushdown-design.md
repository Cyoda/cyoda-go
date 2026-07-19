# Tx-aware search pushdown — design

Issue: #420 (`feat(search)`), milestone v0.8.3.

## Problem

`SearchService.Search` (`internal/domain/search/service.go:158-183`) gates plugin
pushdown on `tx == nil`:

```go
tx := spi.GetTransaction(ctx)
if searcher, ok := store.(spi.Searcher); ok && tx == nil {
    ... searcher.Search(...) ...
}
// else: GetAll(ctx, modelRef) + in-memory match over the WHOLE model
```

Inside a transaction every search pulls the **entire model** into Go memory
(`store.GetAll` overlays the tx buffer, then `match.Match` filters row-by-row).
For `sqlite`/`postgres` that ships every entity of the model across the DB
boundary into process memory on every in-tx search — `O(n_entities_in_model)`,
unacceptable before scale. `memory` is in-process so it pays only CPU.

The gate exists for a real reason: a tx's uncommitted writes live in the
in-process `tx.Buffer`/`tx.Deletes` (`plugins/memory/entity_store.go:374`
`GetAll`), invisible to the DB engine; a naive SQL `WHERE` would miss them and
break read-your-own-writes (RYW). `GetAll`+`match` is correct, just wasteful.
The commercial (Cassandra) backend has the same gate; a downstream issue there
is queued behind this one.

## The isolation dimension (the non-obvious part)

cyoda's documented contract (`docs/CONSISTENCY.md` §1) is **Snapshot Isolation
with First-Committer-Wins on entity-level conflicts** — *not* serializability.
§3/§7.3/§9 state explicitly that **predicate-read phantom write-skew is out of
contract**. So making in-tx search miss phantoms is not a regression.

But the **read-set is load-bearing**: an in-tx entity-returning read records the
entities it returns into `tx.ReadSet`, and commit-time FCW aborts if a
concurrent committer's write-set intersects `tx.ReadSet ∪ tx.WriteSet`
(`plugins/memory/txmanager.go:254`, `plugins/postgres/txstate.go:116`
`ValidateReadSet`). This is the mechanism behind the documented **workflow
fence** (CONSISTENCY.md Appendix A, §7.4): reading peer entities materialises
them into the read-set so concurrent mutations abort.

Today an in-tx `Search` runs through the `GetAll` fallback, so it *inherits*
`GetAll`'s read-set behaviour — it records the **whole model**. Pushdown changes
what a search contributes to the read-set. That is not a contract violation
(search phantoms were never promised), but it **must be defined, uniform across
backends, and documented**, or it becomes a silent backend-divergence bug. The
plugin `Searcher`s have no read-set logic today because they are never called
in-tx (the gate blocks it).

## Decision

**Do not add a `TxAwareSearcher` interface.** `spi.Searcher` becomes tx-aware by
contract: `Search` honours an active transaction (RYW), producing results
identical to `GetAll`+`match` for the same tx state. The engine stops
special-casing transactions and always delegates to `Searcher.Search`. Each
backend overlays its own write-set internally (`sqlite`/`postgres`/`memory` from
the in-process `tx.Buffer`/`tx.Deletes`; Cassandra downstream from its WAL). The
overlay uses a **bounded streaming merge** — no full-model materialisation.

**Read-set participation is an explicit, opt-in property of the read**, named
after the SQL `FOR SHARE` / "locking read" concept but honest to cyoda's
optimistic model (returned rows are commit-validated, no lock is held). A new
per-query boolean **`TrackingRead`** (default `false`):

- **`false` (default) — plain snapshot read.** Returns the RYW-correct rows,
  records **nothing** in the read-set. Cheap; phantom-unsafe — exactly what
  §3/§7.3 already say in-tx search is. This is the memory-pressure win with no
  commit-validation blow-up.
- **`true` — tracking read.** Additionally records the **returned** entities
  into `tx.ReadSet`, so FCW validates them at commit. Entity-level, on the rows
  actually returned — still no predicate/phantom locking (a concurrent *insert*
  of a new match is not caught, per contract). This is how a search
  participates in the fence: a search whose predicate returns the *complete*
  peer set, run with `TrackingRead`, gives the same guarantee as `GetAll`.

`TrackingRead` is a no-op when there is no active transaction (no read-set
exists). Read-set membership becomes a declared property of the read rather than
a side effect of which API was called — today `GetAll` is implicitly "always
tracking", `Count` is "never". Extending the same flag to `GetAll`/`Get` (so all
reads declare it) is a natural follow-on, explicitly **out of scope** here; the
search flag is designed to be consistent with that direction.

**Behaviour change (documented):** in-tx `Search` no longer records the whole
model by default. A workflow that (contrary to §5 guidance) relied on in-tx
`Search`'s accidental whole-model read-set for a fence must either use `GetAll`
(unchanged) or pass `TrackingRead` with a peer-complete predicate. This is a
conflict-footprint change within the SI+FCW contract, not an isolation-level
change.

### Rejected / considered

- Separate `spi.TxAwareSearcher` interface: legitimises the full-scan fallback;
  the tx already rides in `ctx`. A tx-ignoring `Searcher` would be a bug, so
  tx-awareness belongs on `Searcher`.
- Unconditionally recording matches (no opt-in): forces `O(matches)` read-set +
  commit-validation cost on every in-tx search and removes the caller's ability
  to declare intent.
- Recording *nothing* even with opt-in (treat search like `Count`): removes FCW
  protection on rows the search actually returned; inconsistent with search
  being entity-returning.

## Canonical search semantics move to the SPI

Overlaying a buffered entity requires evaluating the **full** predicate against
it in Go (it is not in the DB), and interleaving buffered rows into the
SQL-ordered committed stream requires a Go comparator matching the SQL
`ORDER BY`. Both already exist in cyoda-go but are unreachable by the plugin
modules and are **already hand-synchronised across three copies**:

- `internal/match.MatchFilter` (complete `spi.Filter` evaluator; its godoc says
  it "mirrors `plugins/sqlite/post_filter.go`" and is pinned by an `e2e/parity`
  smoke test),
- `plugins/sqlite/post_filter.go` `evaluateFilter`/`evaluateLeaf`,
- `plugins/postgres/grouped_stats.go` `evalPostFilter`,

plus the ordering comparator `internal/domain/search/ordersort.go`
(`sortEntities`/`lessByKey`).

"Do it right" collapses the hand-sync. These move to the SPI (the module every
consumer already imports); existing sites delegate:

1. **`spi.MatchFilter(f Filter, data []byte, meta EntityMeta) bool`** — the
   canonical `Filter` evaluator (ported from `internal/match`, the pinned
   reference). `internal/match.MatchFilter`, `sqlite.evaluateFilter`,
   `postgres.evalPostFilter` become thin wrappers.
2. **`spi.LessByOrder(a, b *Entity, specs []OrderSpec) bool`** — the canonical
   `OrderSpec` comparator (ported from `ordersort.go`, Kind-aware: numeric /
   temporal-ms-floor / bool / text-byte-order, NULLS-LAST, `entity_id`
   tiebreaker). `search.sortEntities` and the merge below both use it.
3. **`spi.MergePage(next func() (*Entity, bool, error), adds []*Entity, deleted func(id string) bool, specs []OrderSpec, offset, limit int) ([]*Entity, error)`**
   — a bounded k-way merge of a **sorted** streamed committed source (`next`,
   lazy pull) with a **sorted** in-memory `adds` slice, skipping `deleted` ids,
   ordered by `LessByOrder`, early-stopping once `offset+limit` survivors are
   gathered. Reused by all backends (Cassandra's `adds`/`deleted` come from its
   WAL).

**Dependency consequence:** `spi.MatchFilter` uses `tidwall/gjson`, absent from
the SPI's `go.mod` today (SPI depends only on `google/uuid`). `internal/match`,
`sqlite`, and `postgres` all already depend on gjson, so this **centralises** a
dependency every in-tree consumer already carries. The SPI is nonetheless the
**public** contract module (out-of-tree plugins, compute-node authors inherit
its deps transitively). This is the one notable dependency-surface decision and
is called out for explicit sign-off; the alternative (a dedicated
`cyoda-go-spi/search` sub-package or sibling helper module) keeps the core SPI
thin at the cost of another import path.

These land on `cyoda-go-spi` main and are picked up via a re-pinned
pseudo-version (the v0.8.3 coordinated-release window is open — no per-issue
tag; one SPI tag at milestone end).

## Engine change (`internal/domain/search/service.go`)

Remove `&& tx == nil`. All OSS backends implement `Searcher` (memory becomes one
— below), so the pushdown branch runs whenever the store implements `Searcher`,
tx or not. Thread `opts.TrackingRead` into `spi.SearchOptions`:

```go
if searcher, ok := store.(spi.Searcher); ok {
    filter, translateErr := ConditionToFilter(cond)
    if translateErr == nil {
        return searcher.Search(ctx, filter, spi.SearchOptions{ /* ...,*/ TrackingRead: opts.TrackingRead })
    }
    // translate-failure fallback only (below)
}
```

`GetAll`/`GetAllAsAt` + `match` remains **only** for the condition-translation
failure path. In-tx, that rare path still records the whole-model read-set (via
`GetAll`) regardless of `TrackingRead` — a conservative edge (more aborts, never
unsafe), documented in code. `search.sortEntities` delegates to
`spi.LessByOrder`.

## Plugin change (`sqlite`, `postgres`, `memory`)

`memory` gains a `Searcher` implementation (iterate model snapshot + overlay
buffer via the same helpers) so the flag and the tx-aware path are uniform and
the engine's `GetAll` fallback is translate-failure-only. `GetAll` itself is
unchanged (still always-tracking — the fence relies on it).

All three `Search` implementations read `tx := spi.GetTransaction(ctx)` and
branch:

- **`tx == nil`** (unchanged for sqlite/postgres): committed pushdown; SQL
  `LIMIT`/`OFFSET` when no residual, else Go paging.
- **`tx != nil && PIT != nil`**: committed-**only** pushdown at the PIT (no
  overlay). In-tx PIT stays committed-only (matches `GetAllAsAt`, which "always
  reads committed data"), and replaces the old in-tx `GetAllAsAt` full scan with
  an index pushdown — identical results, far less memory. No read-set (mirrors
  today's in-tx PIT fallback, which records none).
- **`tx != nil && PIT == nil`** — the RYW overlay:
  1. Stream committed candidates via the existing SQL pushdown in `ORDER BY`
     order, **without** SQL `LIMIT`/`OFFSET` (overlay shifts positions);
     residual post-filter still applies per row.
  2. Under `tx.OpMu.RLock` (lock order `tx.OpMu` → store mutex; fail fast on
     `tx.RolledBack`; IIFE-scoped per `go-mutex-discipline.md`), read
     `tx.Buffer`/`tx.Deletes`. Build the sorted `adds`:
     `{ copyEntity(e) | id,e ∈ tx.Buffer, e.Meta.ModelRef == modelRef,
       !tx.Deletes[id], spi.MatchFilter(filter, e.Data, e.Meta) }`.
  3. `deleted(id) = tx.Deletes[id] || (_,ok:=tx.Buffer[id]; ok)` — a committed
     row that is deleted or superseded by a buffered version is skipped (its
     buffered version, if it matches, re-enters via `adds` at its current sort
     position — version supersession).
  4. `spi.MergePage(committedNext, adds, deleted, specs, offset, limit)`.
  5. **If `TrackingRead`**: record each **returned** entity into `tx.ReadSet`
     (each backend's `recordReadIfInTx` equivalent), under the same OpMu.RLock
     discipline as `GetAll`. Buffered adds are the tx's own writes (already in
     `WriteSet`) — record committed returned rows' versions.

`sqlite.evaluateFilter` and `postgres.evalPostFilter` re-point at
`spi.MatchFilter`. `sqlite`'s `SearchScanLimit` budget applies to the committed
stream (a broad in-tx filter that scans past budget still errors
`ErrScanBudgetExhausted`). Memory bound of the overlay is
`~offset+limit+len(adds)` via lazy `next()`; a large `tx.Deletes` set can deepen
the committed scan (bounded by tx size) — noted in code.

### RYW reference semantics (acceptance bar)

Identical to `GetAll`+`match` for the same tx state: created/updated-in-T match
present (buffered version); deleted-in-T absent; committed entity whose buffered
version no longer matches absent; committed match untouched present; ordering /
pagination identical.

## API / transport surface

New **optional** request field `trackingRead` (bool, default `false`) on the
**synchronous** search path only — HTTP `POST /api/v1/search/...` and the gRPC
`Search` RPC (the in-tx-reachable path via the #402 tx-token join,
`callback_txjoin_grpc_search_test.go`). Async search is detached (background ctx,
no tx) so the flag is not exposed there. Threading:
HTTP/gRPC request → domain `SearchOptions.TrackingRead` → `spi.SearchOptions`.

No new status or error codes. The existing per-endpoint codes are unchanged;
restated for the checklist:

| Endpoint | Code | Trigger |
|---|---|---|
| `POST /api/v1/search/{model}` (sync) | 200 | results (incl. in-tx RYW); `trackingRead` additive optional |
| | 400 `INVALID_FIELD_PATH` | unknown condition/sort path |
| | 400 `BAD_REQUEST` | limit > MaxPageSize |
| | 404 `MODEL_NOT_FOUND` | unregistered model |
| gRPC `Search` | envelope `Success`/`Error.Code` (unchanged) | — |

Gate-4 doc surface touched by the new field: `api/openapi.yaml` (embedded via
`//go:embed`), the gRPC search request proto (regen + `cyoda help grpc`
artefacts), and any search help topic.

## Coverage matrix (scenario × layer)

| Scenario | Unit | Running-backend e2e | Cross-backend parity | gRPC |
|---|---|---|---|---|
| In-tx RYW: created-in-T present | sqlite, postgres, memory | e2e (HTTP, tx-token) | parity (mem/sqlite/pg) | grpc e2e |
| In-tx RYW: deleted-in-T absent | sqlite, postgres, memory | e2e | parity | — |
| In-tx RYW: buffered version no-longer-matches → absent | sqlite, postgres, memory | — | parity | — |
| In-tx RYW: committed match untouched present | sqlite, postgres, memory | e2e | parity | — |
| In-tx overlay pagination — incl. boundary interleave (add tying a committed row on all non-id keys; add adjacent to NULL/empty-string committed value; temporal-floor tie; desc + NULLS-LAST) | sqlite, postgres, memory | — | parity | — |
| In-tx PIT committed-only | sqlite, postgres, memory | e2e | parity | — |
| In-tx search does **not** full-scan (regression) | sqlite (scan-budget respected w/ narrow filter), postgres (bounded/GetAll-not-called spy) | — | — | — |
| `TrackingRead=true`: concurrent write to a **returned** entity → 409 conflict | — | isolated single-backend e2e (per backend; not parity) | — | grpc e2e |
| `TrackingRead=true`: concurrent write to a **non-returned** entity → no conflict | — | isolated single-backend e2e | — | — |
| `TrackingRead=false`: concurrent write to a returned entity → **no** conflict (snapshot read) | — | isolated single-backend e2e | — | — |
| `trackingRead` param plumbed HTTP→domain→spi→plugin | domain | e2e | — | grpc e2e |
| Engine de-guard routing (Searcher called in-tx; translate-failure → GetAll) | domain (`service_test`) | — | — | — |
| `memory` as Searcher: non-tx search results unchanged | memory unit | — | parity | — |
| Multi-node: owner-forwarded in-tx search returns RYW via overlay | — | cluster e2e | — | — |
| `spi.MatchFilter` canonical semantics | SPI unit (port existing cases) | — | — | — |
| `spi.LessByOrder` canonical ordering | SPI unit | — | — | — |
| `spi.MergePage` bounded merge/page (interleave, deletes, early-stop bound) | SPI unit | — | — | — |

Concurrency scenarios are isolated single-backend e2e asserting consistency
(one coherent result), **never** the shared parity suite
(`concurrency-tests-not-in-parity.md`).

## Multi-node

The overlay is only correct because in-tx `Search` executes on the tx-owner node
(where `tx.Buffer` lives). The gRPC tx-route interceptor forwards
`EntitySearch`/`EntitySearchCollection` to the owner, and HTTP `TxJoin` joins
locally or fails closed (`ErrTxNotFound` → 404), so the premise holds today. The
spec states the invariant explicitly: **in-tx `Searcher.Search` requires
co-location with the tx owner**; a `Searcher` run against a joined-but-bufferless
tx would silently drop RYW. A cluster e2e asserts owner-forwarded in-tx search
returns RYW via the overlay (matrix row above). Aligns with
`.claude/rules/multi-node-primary.md`.

## Docs / gates

- **`docs/CONSISTENCY.md`** (canonical isolation reference, Gate 4): document
  `TrackingRead`, the default-snapshot-read behaviour of in-tx search, and the
  reworded fence clause: *in-tx search is a predicate read; it records only the
  entities it returns (and only under `TrackingRead`), and does not prevent
  phantoms; the fence requires reading **every** entity the invariant covers —
  a predicate returning a subset breaks it. Use `GetAll`, or a peer-complete
  `TrackingRead` search.*
- **`docs/cloud-parity/`** entry: the tx-aware `Searcher` contract + the
  `trackingRead` field are behaviour/API Cloud mirrors (Gate 7).
- `spi.Searcher` godoc tightened to the tx-aware + `TrackingRead` contract.
- `api/openapi.yaml`, gRPC proto/`cyoda help` artefacts for the new field.
- `COMPATIBILITY.md` on the eventual v0.8.3 SPI tag (milestone end).

## Non-goals / follow-up

- **Cassandra tx-aware `Search`** — commercial repo (queued downstream issue);
  same `spi.MergePage`, WAL-sourced `adds`/`deleted`.
- Extending `TrackingRead` (explicit read-set participation) to `GetAll`/`Get` —
  a natural unification, deliberately out of scope; the search flag is designed
  consistent with it.

## Rollout ordering (correctness)

Engine de-guard and plugin tx-awareness land together — de-guarding before the
plugins overlay would route in-tx searches to a committed-only `Search` and
silently break RYW. TDD order: SPI primitives (+tests) → plugin tx-aware
`Search` incl. `memory` Searcher (RED parity/regression → GREEN) → `TrackingRead`
plumbing + read-set recording (RED conflict-footprint tests → GREEN) → engine
de-guard (RED routing → GREEN) → e2e/parity/cluster → docs.
