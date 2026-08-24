# Research: current state relevant to the #472 SPI surface (Phase 0a)

Date: 2026-08-22. Factual current-state record — no design decisions here.
Sources: fresh-context exploration of this repo (worktree of `release/v0.8.4`
@ `90861eb`), the SPI checkout (`../cyoda-go-spi`, main @ `f746c12`), and the
commercial Cassandra backend checkout (`../cyoda-go-cassandra`).

Scope: the three design calls #472 Phase 0a must settle —
(a) incremental result-append shape on `AsyncSearchStore`,
(b) paginated entity-read variant,
(c) streaming search variant vs paging the bounded interface for the
`Limit <= 0` residual consumers.

## 1. SPI surface today (cyoda-go-spi main @ f746c12, 5 commits past v0.8.3)

`AsyncSearchStore` (`search_store.go:42-54`):

- `CreateJob`, `GetJob`, `DeleteJob`, `ReapExpired(ttl)`.
- `UpdateJobStatus(ctx, jobID, status, resultCount, errMsg, finishTime, calcTimeMs)`.
- `SaveResults(ctx, jobID, entityIDs []string)` — whole-slice, single-shot.
  No incremental/append variant exists.
- `GetResultIDs(ctx, jobID, offset, limit) (ids, total, err)` — the only
  offset/limit pagination in the SPI; pages the *persisted result-ID list*.
- `Cancel(ctx, jobID)` — doc'd as pure status transition; silent on
  FinishTime (that silence is #474's subject; #474 is OPEN, no SPI PR yet,
  planned surface `Cancel(ctx, jobID, finishTime)`).
- `SelfExecutingSearchStore` (`search_store.go:36-39`) = `AsyncSearchStore` +
  marker `SelfExecuting()`; the engine skips its own executor goroutine for
  such stores. No OSS backend implements it; Cassandra does.

`Searcher` (`searcher.go:33-35`): single method
`Search(ctx, filter, opts) ([]*Entity, error)`. `Limit > 0` is bounded-or-fail
(`ErrSearchResultLimitExceeded`, never a truncated prefix); `Limit <= 0`
unbounded, backend MUST NOT substitute a default. `SearchOptions` carries
`Limit`, `OrderBy []OrderSpec`, `PointInTime`, `TrackingRead`; struct doc:
"There is no Offset: direct search does not paginate (async search does, over
its persisted result-ID list)."

`EntityStore` reads (`persistence.go:67-101`): `Get`, `GetAsAt`, `GetAll`,
`GetAllAsAt`, `Exists`, `Count`, `CountByState`, `GetVersionHistory`. Every
list-shaped read returns a whole slice — no limit, offset, cursor, or
ordering parameter anywhere. `CountByState` is the only method with a
pushdown MUST.

`Iterable`/`Iterator` (`iterable.go:31-51`): `Iterate(ctx, model, filter,
opts) (Iterator, error)`; `Iterator` = `Next/Entity/Err/Close`. Contract:
pushdown of pushable filter parts, residual applied inside `Next()`, MUST
observe ctx cancellation, sticky `Err()`, idempotent `Close()`, no global
write-blocking lock. `IterateOptions` has only `PointInTime`, doc'd as
"reserved as the extension point for future iteration knobs". **No Limit, no
OrderBy, and no spitest conformance suite** (Searcher and AsyncSearch are the
only search-adjacent spitest groups).

spitest facts that bind changes:

- `StoreFactoryConformance` fails the run on any unmatched `Skip` key, so
  renaming/adding subtests can turn a backend's Skip map red.
- `AsyncSearch/SaveAndGetResults/Pagination` asserts order preservation:
  `GetResultIDs` pages must come back in save order.
- `AsyncSearch/Cancel` asserts nothing about FinishTime (the #474 gap);
  `ReapExpired` deliberately reaches terminal state via `UpdateJobStatus`,
  not `Cancel`.
- Searcher suite covers bounded-or-fail incl. in-tx RYOW overlay; no
  `OrderBy`/`PointInTime`/`TrackingRead` coverage.

Release state: latest tag v0.8.3; `[Unreleased]` already has a `### Breaking`
section (spi#33 prepare/execute split). cyoda-go pins pseudo-version
`v0.8.4-0.20260811205327-f746c122a064` — the mid-milestone state
MAINTAINING.md prescribes; tag cut only when all v0.8.4 SPI changes are in.

## 2. Engine async lifecycle (`internal/domain/search/service.go`)

`SubmitAsync` (`service.go:452-653`): validates, defaults `PointInTime` to
now, `CreateJob` with status RUNNING (request ctx), then — unless the store
is `SelfExecutingSearchStore` — spawns one goroutine per submission:

- `bgCtx := spi.WithUserContext(context.Background(), uc)`, optionally
  wrapped by the unexported `asyncScanScoper` interface (`service.go:80-82`)
  which only postgres implements (`AsyncScanContext` → statement-timeout
  ceiling marker). **No cancellation, no deadline, no shutdown tie-in.**
- `results, err := s.Search(bgCtx, ...)` — whole result set materialised as
  `[]*spi.Entity`; then a second full-size `ids []string` is built
  (`:624-627`) — both slices live simultaneously — then one
  `SaveResults(bgCtx, jobID, ids)` (`:629`).
- Cancel checks are post-hoc only: `GetJob` after the search returns
  (`:606-613`) and again after `SaveResults` (`:637-645`).
- Panic recovery exists (recover + health-flag latch + FAILED write,
  `:580-598`). If the post-search `GetJob` errors, the job is left RUNNING
  until TTL reap (`:606-609`).
- No jobID→CancelFunc registry. No worker pool: one goroutine per
  submission. Nothing awaits these at shutdown — `App.Shutdown`
  (`app/app.go:912-925`) stops the reaper and scheduler only; an in-flight
  job is abandoned mid-scan and may write to a closing store.

`CancelAsync` (`service.go:748-764`): `GetJob`; if RUNNING, writes CANCELLED
via `UpdateJobStatus`. **Never calls `AsyncSearchStore.Cancel`** (the #474
defect) and cannot reach the running goroutine.

Result read-back `GetAsyncResults` (`service.go:679-738`): job must be
SUCCESSFUL; `GetResultIDs(ctx, jobID, offset, limit)` (limit<=0 → 1000);
then **one `GetAsAt(id, job.PointInTime)` per id** (N+1 per page); genuine
`ErrNotFound` skipped with a Warn, any other error fails the page.

Status vocabulary written by the engine: RUNNING, SUCCESSFUL, FAILED,
CANCELLED (`"COMPLETED"` explicitly not used, `service.go:131`).

HTTP surface: `POST /search/async/{entityName}/{modelVersion}`,
`GET /search/async/{jobId}`, `PUT /search/async/{jobId}/cancel`,
`GET /search/async/{jobId}/status` (`api/generated.go:8565-8568`). gRPC:
snapshot search/status/cancel/get-streaming (`internal/grpc/search.go:130,
183, 218, 649`). Direct search streams `application/x-ndjson`
(`internal/domain/search/handler.go:189-201`).

## 3. `Limit <= 0` callers

Service maps `Limit < 0` to 0 before the store call (`service.go:304-307`).

- **Conditional delete**: `internal/domain/entity/service.go:1033` and
  `:1157` (batched) — `Limit: -1`, comment says scoped delete must never be
  capped. The matched slice then drives `MatchedCount` and a per-id delete
  loop; `cond == nil` on the batched path uses `GetAll` instead.
- **Async submit**: HTTP (`search/handler.go:226-228`) and gRPC
  (`grpc/search.go:149-151`) both build `SearchOptions` with Limit unset
  (0). The bounded-or-fail guard never fires; the engine's in-memory
  fallback path accumulates every match (`service.go:415-423`) after a
  whole-model `GetAll`/`GetAllAsAt` (`:352-360`).

## 4. Part-3 read paths (list / history)

- `ListEntities` (`internal/domain/entity/service.go:1339-1424`):
  `GetAll`/`GetAllAsAt` whole model → `sort.Slice` by `Meta.ID` → slice
  `[start:end]`. Paging is entirely in-memory in the domain.
- `?transactionId=` (`service.go:398-412`): `GetVersionHistory` (full
  history, every version carrying its full `*Entity` payload) → linear scan
  for the matching TransactionID.
- `?pointInTime=` changes metadata (`service.go:736-792`): full
  `GetVersionHistory` → truncate to cutoff → sort desc → cap to 1000
  **after** full materialisation.
- Pagination caps: `MaxPageSize = 10000`, `ValidateOffset`
  (`internal/domain/pagination/pagination.go:22-55`).

## 5. Per-backend facts

### memory

- Search fully materialises in every branch (committed snapshot copy →
  filter into second slice; RYW overlay pre-materialises both sides before
  `spi.MergeBounded`). Amortised `ctx.Err()` checks (`i&1023==0`).
- `SaveResults` = whole-slice **replace** (`search_store.go:120-141`).
- `GetResultIDs` = slice arithmetic; **negative offset/limit panics**
  (sqlite passes the same values to SQL instead).
- `GetAll`/`GetAllAsAt` iterate Go maps — **no ordering guarantee at all**.
- `memoryIter` (`grouped_stats.go:200-240`) implements `spi.Iterator` but
  over a pre-built snapshot slice (memory can't stream from disk; it does
  avoid copying by relying on version immutability).
- No scan-budget machinery in this plugin.

### sqlite

- `searchCommitted` genuinely streams `sql.Rows` but accumulates matches;
  SQL `LIMIT n+1` pushed only when no residual AND `Limit > 0`. The tx
  overlay path has the repo's only lazy committed source: a `next` closure
  over `rows.Next()` fed to `spi.MergeBounded` (`searcher.go:281-313`).
- `SaveResults` = one tx, one INSERT per id, `seq = i` restarting at 0 —
  **insert-only; a second call for the same job collides on the
  `(tenant_id, job_id, seq)` PK**.
- `GetResultIDs` = real SQL `ORDER BY seq LIMIT ? OFFSET ?`.
- `GetAll`/`GetAllAsAt`: no ORDER BY; the in-tx variant flattens a Go map
  (order random even though SQL could have ordered). `GetVersionHistory` is
  the only entity read with an explicit ORDER BY (`version ASC`).
- `sqliteIter` (`grouped_stats.go:190-252`) is a true `sql.Rows`-backed
  `spi.Iterator` reusing the same base-query builders and planner as Search.
- Scan-budget removal inventory (Phase 3): `config.go:19,31`,
  `plugin.go:22`, `store_factory.go:207,312,325-331`, enforcement
  `searcher.go:123-125,291-293`, plus listed tests, engine consumers
  (`internal/domain/search/service.go:321`,
  `grouped_stats_service.go:81`), docs/help/CHANGELOG sites. The sentinel
  itself lives in the SPI (`errors.go:113`).

### postgres

- The O(n) point: residual loop `searcher.go:241-246` — with `Limit <= 0`
  the overflow guard never fires and `results` grows to the full matched
  set. Rows themselves are streamed one at a time by pgx (`postgresIter`);
  the accumulation is the only whole-set buffer. Doc comment at
  `searcher.go:31-35` states exactly this profile.
- `postgresIter` (`grouped_stats.go:135-196`) implements `spi.Iterator`
  over live `pgx.Rows`, residual inside `Next()`, per-row ctx checks;
  `Iterate` shares `searchBaseQuery` + planner with Search but applies no
  ORDER BY and no LIMIT.
- `SaveResults`: builds full `[][]any` then one `CopyFrom`; `seq` = slice
  index. `GetResultIDs`: `ORDER BY seq OFFSET/LIMIT`, served by PK
  `(job_id, seq)`.
- Async ceiling: `AsyncScanContext` marker → `searchUnderOwnCeiling`
  (`searcher.go:117-161`), own tx + `SET LOCAL statement_timeout`; 57014 →
  `searchCeilingError` (not StorageUnavailable, message names the env var).
  Ceiling branch requires: marker present, pool non-nil, **no ambient tx**.
- A long scan pins one pooled connection for its duration;
  `CYODA_POSTGRES_MAX_CONNS` default 25, acquire timeout 10s
  (Go-side). Exhaustion → retryable 503. Three documented contenders for
  connections: TransactionManager.Begin, ExtendSchema, the async scan.
- Ordering assets: default search order is `entity_id COLLATE "C"` with
  NULLS LAST + entity_id tie-breaker (`searcher.go:282-303`); `entities` PK
  `(tenant_id, entity_id)`; `idx_entities_model` does NOT include
  `entity_id`; no GIN. Cancellation is observed cooperatively per-row
  (ctx.Err in `Next`), not by server-side query cancel; context errors are
  deliberately excluded from torn-connection classification.

### Cassandra (commercial, `../cyoda-go-cassandra`, pins SPI v0.8.3)

- `SelfExecutingSearchStore`: the engine goroutine never runs.
  **`SaveResults` is a hard error by design**; `UpdateJobStatus` and
  `ReapExpired` are no-ops (status is aggregated from per-shard rows; TTL is
  native). Conformance Skip entries cover these.
- Async executor materialises the full per-shard candidate set
  (`plan.Execute → []gocql.UUID`), then writes once via `WritePages`
  (fixed 1000-id pages) + one `WriteTerminal`. The `iter.Seq2` streaming
  machinery exists but is used only by the **direct** executor. The async
  plan walk has **zero ctx.Err() checks** — cancellation lands only at the
  next Cassandra round-trip.
- Cancellation: Redpanda JobCancelled event → in-process registry keyed by
  **jobID only** — a node running several shards of one job overwrites
  earlier cancel funcs; one shard's deferred unregister deletes the entry
  (latent defect). `Store.Cancel` batch-writes CANCELLED to all shard rows,
  idempotent.
- `GetResultIDs`: offset emulated by reading pages and discarding until the
  offset is consumed (O(offset) reads); `total` = sum over COMPLETED
  shards. **`limit <= 0` unguarded: negative panics on `make`, zero returns
  one id.** Async results carry NO ordering: `SearchOpts` is stored but
  never parsed by the async executor; emitted order is plan-walk order,
  shard-concatenated — a live cross-backend divergence from the OSS
  engine's sorted async results.
- Ordered entity reads: no CQL pushdown available for user-field ordering
  (index tables cluster by entity_id within period buckets; range reads
  concatenate buckets). Native order exists only per shard partition on
  `entity_id` (timeuuid, 32 shard partitions). An ordered contract implies
  in-process work across shards; `gocql` page state / PageSize are entirely
  unused today. `entityIter` (`spi.Iterable`) is the one cursor-shaped read
  (one live gocql.Iter per shard, advanced shard by shard).
- `GetAll` does one `entity_by_model` drain plus **two reads per id**;
  `GetVersionHistory` reads all `entity_changes` rows and replays field
  deltas (documented O(n) in total field changes).

## 6. Constraints the design must respect (derived facts)

1. **Incremental result writes can only be an engine-side contract.** The
   only self-executing backend (Cassandra) hard-errors on `SaveResults` and
   the engine already skips its executor for such stores. Cassandra's own
   incremental story (`WritePages` per shard) is invisible to the SPI.
2. **Result ordering is already contract** via spitest
   (`SaveAndGetResults/Pagination`: save order == page order, `seq`
   columns). Any append protocol must define seq continuation across calls
   — sqlite's PK collision on repeated `SaveResults` is the concrete
   hazard.
3. **`ResultCount` and SUCCESSFUL are written once, at the end**
   (`UpdateJobStatus`); status vocabulary is fixed strings in three
   plugins + engine (no shared enum).
4. **`Iterable` is implemented by all four backends** with shared
   planner/base-query reuse on sqlite/postgres, and `IterateOptions` is the
   SPI's declared extension point. It lacks Limit, OrderBy, spitest
   conformance, and (on Cassandra) any cross-shard ordering.
5. **Cross-node cancel** must be poll-based on OSS backends (registry is
   process-local; postgres cancel may land on a different node); backend
   ctx-responsiveness exists on all four but is amortised (1023-row stride
   on memory/sqlite engine loops, round-trip-granularity on Cassandra).
6. **Connection budget couples the worker pool to postgres**: each async
   scan holds one pooled connection (default 25 max) for up to the 30m
   ceiling; the ceiling branch is skipped when an ambient tx exists.
7. **`GetResultIDs` semantics differ per backend on degenerate inputs**
   (memory panics on negatives; Cassandra panics on negative limit, returns
   one id on zero; sqlite/postgres pass through to SQL). The engine
   currently shields stores (offset validated, limit defaulted to 1000) —
   but the SPI contract is silent.
8. **`asyncScanScoper` is engine-internal, not SPI** — postgres opts in via
   interface assertion; any redesign of the async execution path must keep
   that ceiling wiring working.
9. **HTTP/gRPC async-results paging is offset-based** (`pageNumber ×
   pageSize`, `MaxPageSize 10000`) and costs N+1 `GetAsAt` per page in the
   engine regardless of store efficiency.
10. **SPI tag discipline**: #474's `Cancel(ctx, jobID, finishTime)` is SPI
    tag 1 (with spitest FinishTime + anchor export); result
    streaming + paginated read are SPI tag 2; scan-budget sentinel removal
    is its own later tag so earlier pin bumps don't drag it in. Breaking
    changes are fine pre-1.0 with `### Breaking` + consumer notification
    before merge (MAINTAINING.md).

## 7. Latent defects observed during research (candidates to fold into scope)

- Engine: post-search `GetJob` error leaves a job RUNNING forever
  (`service.go:606-609`) — only the TTL reaper cleans it up, and reap
  requires a FinishTime that was never written.
- memory `GetResultIDs`: panic on negative offset/limit
  (`search_store.go:145-175`).
- memory `UpdateJobStatus`: always sets FinishTime even when zero
  (diverges from sqlite's NULL); missing job → plain error, not
  `ErrNotFound` (same in sqlite's `UpdateJobStatus`).
- sqlite `SaveResults`: insert-only, repeat call collides on PK.
- Cassandra (their repo, courtesy-note candidates, strictly out of scope
  here per courtesy-PR policy): jobID-keyed cancel registry collision;
  `GetResultIDs` negative/zero limit; async results ignore requested
  ordering (cross-backend divergence with the OSS engine's sorted async
  results).
- Scheduler dispatch documents the same unbounded-goroutine gap
  (`internal/scheduler/service.go:193-198`) and cross-references
  `SubmitAsync` — the worker-pool design has a second in-repo consumer
  shape to stay consistent with.

## 8. Existing in-repo precedents for the worker pool

No bounded worker pool exists anywhere. Closest shapes: OIDC warmup
fixed-worker channel pool (`internal/auth/oidc/registry.go:752-771`),
singleflight (`oidc/singleflight.go`), per-key gate registry
(`internal/txgate/txgate.go`), reaper/ticker loops with stop channels
(`app/app.go:447-465`, scheduler). `golang.org/x/sync` is already a
dependency but `semaphore.Weighted` is unused. `errgroup` appears only for
server lifecycle.
