# Design: search SPI surface for streaming — async lifecycle, paged reads, history reads

Date: 2026-08-22. Phase 0a of the v0.8.4 search remediation (#472; policy #475).
Facts underlying every decision here: `docs/superpowers/research/2026-08-22-472-search-spi-surface-research.md`.
Reviewed by a fresh-context design review; all findings resolved or absorbed below.

## 1. Scope and sequencing

This spec settles the **SPI surface** #472 needs. Engine-side machinery (worker
pool, cancel registry, shutdown drain) is specified only to the depth that
justifies the surface; its implementation detail belongs to the #472 plan.

Sequencing (unchanged from the milestone spine): #474 ships first as SPI tag 1
(`Cancel(ctx, jobID, finishTime)` + spitest FinishTime assertions). Everything
below ships as **SPI tag 2**, one coordinated release with `### Breaking`
changelog entries and consumer notification before merge per the SPI repo's
MAINTAINING.md. The scan-budget sentinel removal remains its own later tag
(phase 3). Structural dependency: mid-stream-cancelled jobs become reapable
only because tag 1 makes `Cancel` stamp a finish time.

Settled constraints this spec implements, not revisits: no server-imposed time
guards; async searches unbounded in items and time; memory-unbounded
materialisation is a defect — whole-model paths must stream; multi-node is
primary; correctness over availability (fail closed); pre-1.0 SPI breaking
changes without shims.

## 2. `Iterate` is the streaming surface (D1)

`spi.Iterable`/`spi.Iterator` becomes the single unbounded streaming read.
`Searcher.Search` remains the direct-search surface only (§3).

### 2.1 `IterateOptions` gains two fields

```go
type IterateOptions struct {
    PointInTime  *time.Time
    OrderBy      []OrderSpec // empty = entity ID ascending, byte-wise
    TrackingRead bool        // parity with SearchOptions.TrackingRead
}
```

- **Ordering is contractual.** The iterator MUST yield in the requested order;
  empty `OrderBy` means entity ID ascending, byte-wise (the existing default
  order contract of `Searcher`). The tie-breaker rule applies: the final sort
  key is always entity ID, so equal user-field values can never yield
  nondeterministic order. sqlite/postgres push `ORDER BY` into SQL (streaming
  server-side sorts); memory sorts its snapshot; how a backend achieves the
  order is its concern, the yielded order is the contract.
- **Read-set semantics**: entities enter the transaction read-set only when
  `TrackingRead` is set — same rule as `Search`. This corrects sqlite's in-tx
  `Iterate`, which today records reads unconditionally.

### 2.2 Transaction / RYW overlay semantics

- The merged view (committed ∪ transaction buffer) is **snapshotted at
  `Iterate()` time**. Mutating the transaction while one of its iterators is
  open is a caller error with unspecified visibility; the contract forbids it.
  Engine callers comply by drain-then-act (§7).
- On postgres, an in-transaction iterator runs on the transaction's single
  connection: while it is open, no other statement may run on that
  transaction. Combined with the snapshot rule this makes "open iterator,
  interleave writes" illegal everywhere rather than backend-dependent.
- sqlite's in-tx branch may keep materialising the merged snapshot initially
  (current behaviour) but the target shape is the lazy committed stream +
  buffered-adds merge that `searchTxOverlay` already demonstrates.
- A new exported SPI helper `MergeOrdered` (iterator-shaped sibling of
  `MergeBounded`) provides the ordered streaming merge of a committed row
  stream with a sorted buffered overlay, so backends don't each reinvent it.

### 2.3 Postgres async ceiling ports to Iterate

The `AsyncScanContext` ceiling currently lives only in the Search path. When
the ceiling marker is present, `pool` is available, and no ambient transaction
exists, postgres `Iterate` opens its own transaction, applies
`SET LOCAL statement_timeout`, and rolls the transaction back in `Close()`.
Callers MUST `Close()` (already contract); the engine executor always defers
it. Ceiling expiry surfaces as the existing `searchCeilingError` mapped to the
enriched job-failure message (#475 owns the wording).

### 2.4 Scan budget note

sqlite's `Iterate` does not enforce the scan budget; rerouting delete/async
onto Iterate therefore removes budget enforcement from those paths ahead of
the budget's own removal (phase 3). This is deliberate and directionally
aligned with the settled policy; the CHANGELOG relaxation note covers it.

### 2.5 spitest

A new `Iterable` conformance group (none exists today): ordered yield incl.
tie-breaker, residual filter application inside `Next`, ctx cancellation
observed, sticky `Err`, idempotent `Close`, PIT variant, snapshot-at-open
overlay behaviour, `TrackingRead` gating.

## 3. `Searcher.Search` becomes strictly bounded (reviewer finding, agreed)

After the reroute, `Search` has no unbounded caller: direct search over HTTP
defaults the limit and gRPC rejects non-positive limits. The unbounded mode
(`Limit <= 0`) is retired: **`Limit >= 1` is required; `Limit <= 0` is a
contract-violation error**, not "unbounded". Every backend deletes its
unbounded accumulation branch; spitest's `ZeroLimitUnbounded` /
`NegativeLimitUnbounded` subtests become error assertions. This removes the
defect class instead of parking it: no future internal caller can silently
re-open an O(matches) materialising path through `Search`.

## 4. Async job lifecycle surface (D2)

### 4.1 Streamed result save

```go
SaveResults(ctx context.Context, jobID string, entityIDs iter.Seq[string]) error
```

One call per job, consumed lazily. Contract:

- **Order**: yield order is save order is `GetResultIDs` page order.
- **Batching is store-internal.** sqlite chunks into multiple short write
  transactions (its single-writer lock must never be held for the job's
  lifetime); postgres uses chunked `CopyFrom` (the second connection is held
  per chunk, not per job); memory appends. Result-row sequence numbers are
  strictly increasing across store-internal chunks (fixes sqlite's
  insert-only `seq` restart collision).
- **ctx**: the store observes cancellation while consuming.
- **Errors**: a `nil` return means "everything yielded was durably stored" —
  it does not mean the job succeeded. The engine consults the producer's
  error state (iterator `Err()`, ctx) after `SaveResults` returns and writes
  the terminal status accordingly. Mid-stream failure may leave partial rows;
  cleanup is §4.2's job, and visibility is gated on `SUCCESSFUL` as today.
- **Self-executing stores** (the commercial backend) keep rejecting the call;
  the engine never invokes it for them.

sqlite prerequisite: iterators read from a dedicated reader connection (WAL
mode exists for exactly this); the plugin's single-connection model otherwise
deadlocks producer against consumer. Plugin-internal change, no SPI impact.

### 4.2 Orphan detection: heartbeat + stale-job reap

Crash mid-job currently leaves a RUNNING row and partial results forever (the
reaper skips RUNNING; nothing else cleans up). Two SPI additions:

```go
Heartbeat(ctx context.Context, jobID string) error          // stamps now on the job
FailStale(ctx context.Context, staleAfter time.Duration) (int, error)
```

- `SearchJob` gains `HeartbeatTime *time.Time`.
- The executing engine heartbeats periodically (interval = engine config; the
  same loop that polls job status for cross-node cancel).
- `FailStale` (called by the existing reaper loop on every node) marks
  RUNNING jobs whose heartbeat is older than `staleAfter` as FAILED with a
  finish time and a safe generic message; the TTL reaper then removes them.
  Cluster-safe because the heartbeat lives in the store.
- Self-executing stores may no-op both (the commercial backend has its own
  shard recovery). spitest covers both for engine-executed stores.

### 4.3 `GetResultIDs` contract text (previously silent)

- `offset >= 0` and `limit >= 1` are required; violations return an error
  (today memory panics on negatives — a defect this contract makes testable).
- Reads against a non-terminal job answer with the results saved so far;
  `total` is the current saved count. The engine keeps gating result
  visibility on `SUCCESSFUL`, so this is an SPI-level clarification, not an
  API behaviour change.

### 4.4 Async result ordering is contractual end-to-end

Results are saved — and therefore read back — in the requested `OrderBy`
(default: entity ID ascending). On the engine path this is inherited from
§2.1's ordered iterator. The commercial backend currently ignores the
requested ordering (a live cross-backend divergence); ordering is achievable
there by storing result rows keyed by sort-value byte encodings so read-back
order is native storage order — the commercial platform already ships this
pattern for distributed reporting. The divergence becomes a tracked bug on
their side; notification rides the tag-2 consumer notice. A short
cloud-parity note records async result ordering as contract.

### 4.5 Cancellation and shutdown (engine-side, enabled by tag 1 + this surface)

- jobID→CancelFunc registry in the engine; `CancelAsync` cancels in-process
  and dispatches store `Cancel(ctx, jobID, finishTime)` (tag 1).
- Cross-node: the executor's heartbeat/poll loop re-checks job status and
  cancels its own context on CANCELLED. Backend ctx-responsiveness is pinned
  per backend by tests.
- Bounded worker pool with queueing; default derived from the postgres
  connection budget (a streaming job needs its scan connection plus,
  per chunk, a save connection). Env vars follow Gate 4 (help topic, README,
  `DefaultConfig()` together).
- Shutdown drains via the registry: cancel in-flight jobs, mark FAILED with a
  safe message; no stuck-RUNNING jobs.

## 5. Paged entity read (D3)

```go
GetPage(ctx context.Context, modelRef ModelRef, limit, offset int, asAt *time.Time) ([]*Entity, error)
```

- Ordering: entity ID ascending, byte-wise (`COLLATE "C"` semantics),
  contractual and spitest-asserted. `limit >= 1`, `offset >= 0`; violations
  error.
- Serves `ListEntities` (HTTP `pageNumber × pageSize` maps directly; offset
  pagination is required by the public API's random page access).
- Per-entity load errors fail the call (fail-fast) — page boundaries must be
  deterministic.
- Index work required for the latency acceptance criterion ("bounded by
  pageSize, not N"): postgres extends the model index to include `entity_id`
  with `COLLATE "C"`; sqlite adds a `(tenant_id, model_name, model_version,
  entity_id)` index. The commercial backend has no server-side offset; its
  compliant profile materialises IDs (never entities) to serve the offset —
  documented, and no worse than its status quo.
- `GetAll`/`GetAllAsAt` remain in the SPI (the engine's untranslatable-
  condition fallback consumes them until #477 closes it); `ListEntities`
  simply stops using them.

## 6. History reads (D4)

### 6.1 Transaction lookup

```go
GetVersionByTransaction(ctx context.Context, entityID, txID string) (*EntityVersion, error)
```

- Returns the **earliest** version carrying the transaction ID (preserves
  current linear-scan behaviour; an entity can be saved more than once in
  one transaction). `ErrNotFound` when absent. Empty `txID` is rejected
  (sqlite stores empty transaction IDs for non-tx writes; an empty input
  must not match them).
- postgres/sqlite push the match into SQL over the entity's own versions;
  memory maintains a per-entity transaction index; the commercial backend
  serves it from its transaction-keyed clustering.

### 6.2 Metadata-only history

```go
GetVersionMetadata(ctx context.Context, entityID string, opts VersionMetadataOptions) ([]EntityVersionMeta, error)

type VersionMetadataOptions struct {
    From, Until *time.Time // inclusive window, nil = unbounded side
    Limit       int        // 0 = all
}
```

- `EntityVersionMeta` carries what both consumers need and no payload:
  `Version`, `ChangeType`, `Timestamp`, `User`, `AttributedKind`, `Executor`,
  `TransactionID`, `TenantID`, and a canonical `Deleted bool` (derived from
  the change type — replacing the backend-divergent "`Entity` is nil on some
  backends" probe).
- Order: newest-first, tie-broken by `Version` descending (fixes the
  currently unstable equal-timestamp sort).
- `GET /entity/{id}/changes` pushes its cutoff (`Until`) and its 1000 cap
  (`Limit`) down. The audit event search pushes its from/to window down; its
  merge with state-machine events and cursor pagination stay in Go over
  bounded metadata rows — accepted bound, stated here.

### 6.3 `GetVersionHistory` is deleted

No consumer of full-history-with-payloads remains (the three production
callers are covered by §6.1/§6.2; verified — no endpoint returns more than
one historical entity with data). Pre-1.0 collapse, no shim. The spitest
subtests that lean on it (entity suite ordering, transaction-suite
committed-outcome checks) are rewritten against the replacement surface in
the same tag. Consumer notification is mandatory: conformance Skip maps
hard-fail on unmatched keys, so subtest renames alone can break the
commercial backend's run.

## 7. Engine-side consumer reroutes (spec'd for surface justification)

- **Async executor**: ordered `Iterate` → engine wrapper (cancel poll +
  heartbeat + ctx) → `SaveResults(seq)`. Heap per job O(batch).
- **Conditional delete (atomic mode)**: two-phase inside the transaction —
  drain the iterator collecting **IDs only**, `Close()`, then delete per ID.
  No interleaving with an open iterator (illegal per §2.2). The atomic mode
  is inherently O(matched IDs) (the transaction buffer holds every delete);
  the O(page) mode is the existing batched delete, whose selection phase
  streams per batch with per-ID version guards.
- **Sibling defects rerouted in the same change** (fix-siblings policy):
  `DeleteAllEntities`'s whole-model `GetAll` (needs IDs/count only) and
  `deleteBatched`'s `cond == nil` `GetAll` + O(matches) target slice.
- **List/changes/audit** move to §5/§6 reads.
- **Untranslatable conditions** (array-wildcards until the quantifier lands;
  path-allowlist rejects until fixed): the whole-model fallback is **kept**
  for the unbounded consumers in the interim — removing it is #477's scope,
  and #477's acceptance now names these two consumers explicitly so the
  removal cannot be forgotten. Function conditions are already rejected 400.

## 8. Error/status-code table (wire contract is unchanged — verification table)

No endpoint gains or loses a status code in this change; the table pins what
must still hold on a running backend after the reroute.

| Endpoint | Codes to re-verify |
|---|---|
| `POST /search/async/{entityName}/{modelVersion}` | 200 submit; 400 invalid condition / limit > max; 401; 404 model |
| `GET /search/async/{jobId}` | 200 page; 400 job not complete; 401; 404 job |
| `GET /search/async/{jobId}/status` | 200; 401; 404 |
| `PUT /search/async/{jobId}/cancel` | 200 cancelled=true (RUNNING) / false (terminal); 401; 404 — **now actually stops the scan** |
| `POST /search/direct/{entityName}/{modelVersion}` | 200 NDJSON; 400 incl. limit ≤ 0 rejected; 404; 408 timeoutMillis; `SEARCH_RESULT_LIMIT` over-limit error (status per existing contract) |
| `DELETE /entity/{entityName}/{modelVersion}` (conditional) | 200 report; 400; 401; 404 |
| `GET /entity/{entityName}/{modelVersion}` (list) | 200; 400 pagination; 401; 404 |
| `GET /entity/{entityId}?transactionId=` | 200; 400 mutual-exclusion; 404 no matching version |
| `GET /entity/{entityId}/changes` | 200; 400; 404 |
| `GET /audit/entity/{entityId}` search | 200; 400; 404 |
| gRPC snapshot search / status / cancel / get | existing envelope codes per class |

## 9. Coverage matrix

| Scenario | unit | e2e (postgres) | parity | gRPC |
|---|---|---|---|---|
| Ordered Iterate incl. tie-breaker, residual, ctx cancel | spitest | — | implied by spitest on all backends | — |
| Overlay snapshot-at-open; TrackingRead gating | spitest | — | — | — |
| Search rejects Limit ≤ 0 | spitest | ✓ (direct handler already rejects) | — | ✓ existing |
| Streamed SaveResults: order, chunk seq continuity, ctx abort | spitest | — | — | — |
| Async results incremental; heap O(batch) (allocation assert, not timing) | ✓ engine | — | — | — |
| Cancel stops scan mid-flight; cross-node on postgres | ✓ registry | ✓ isolated (not parity) | — | ✓ cancel envelope |
| Heartbeat + FailStale orphan reap | spitest + engine unit | ✓ | — | — |
| Shutdown drain: no RUNNING left, FAILED safe message | ✓ engine | ✓ | — | — |
| Worker pool: ≤ poolSize concurrent, excess queue | ✓ engine | isolated e2e | — | — |
| GetResultIDs degenerate inputs error (no panic) | spitest | — | — | — |
| GetPage ordering/limit/offset; fail-fast | spitest | ✓ list endpoint | ✓ | ✓ ListEntities |
| `?pageSize=1` latency bounded by page, not N | — | ✓ (assert query shape / plan, not wall-clock) | — | — |
| GetVersionByTransaction earliest-wins; empty txID rejected; 404 | spitest | ✓ | ✓ | ✓ |
| GetVersionMetadata window/limit/order; Deleted canonical | spitest | ✓ changes endpoint | ✓ | ✓ changes |
| Conditional delete over large model: O(IDs) atomic / O(page) batched | ✓ engine | ✓ isolated | ✓ behaviour | — |
| Async result ordering respected end-to-end | — | ✓ | ✓ (order asserted per backend) | ✓ |

Concurrency scenarios stay in isolated single-backend e2e, never the shared
parity suite. A missing cell at implementation time blocks merge unless
waived with a one-line reason.

## 10. Deliverables checklist (tag 2 + cyoda-go)

- SPI: `IterateOptions{OrderBy, TrackingRead}`, `MergeOrdered`, strict-bounded
  `Search`, `SaveResults(iter.Seq[string])`, `Heartbeat`, `FailStale`,
  `SearchJob.HeartbeatTime`, `GetResultIDs` contract text, `GetPage`,
  `GetVersionByTransaction`, `GetVersionMetadata` + `EntityVersionMeta`,
  delete `GetVersionHistory`; spitest suites for all of the above; CHANGELOG
  `### Breaking` with migration notes; consumer notification pre-merge.
- cyoda-go: plugin implementations (incl. sqlite reader connection, postgres
  ceiling port + chunked CopyFrom, index migrations), engine reroutes (§7),
  worker pool + registry + drain, Gate-4 doc trio for new env vars,
  cloud-parity note on async result ordering, CHANGELOG.
- Commercial backend (their repo, via notification): signature updates;
  ordered async result storage (sort-key-encoded rows) as the fix for the
  now-tracked ordering divergence; conformance Skip-map updates.

## 11. Explicitly out of scope

- Removing the untranslatable-condition fallback (#477, preconditioned on the
  quantifier work; reminder bullet added there).
- Scan-budget machinery removal (phase 3, own SPI tag).
- Write-path version-count growth (#167).
- Async job wire-syntax translation (spi#31).
