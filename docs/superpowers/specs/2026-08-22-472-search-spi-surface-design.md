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
    OrderBy      []OrderSpec // empty = order unspecified; non-empty = MUST honour (no ambient tx)
    TrackingRead bool        // parity with SearchOptions.TrackingRead
}
```

- **Ordering is honoured, scoped to who needs it.** Empty `OrderBy` means
  **order unspecified** — order-insensitive consumers (grouped stats,
  conditional delete) pay no sorting tax anywhere. When `OrderBy` is
  non-empty: backends whose async search the engine executes (all four
  in-house) MUST yield in the requested order — the async executor is the
  only ordered-Iterate consumer. A backend whose async search is
  self-executing (the commercial backend implements `Iterable` too, and
  cross-shard user-field ordering there would be an O(model) in-process
  sort) MAY reject an ordered call with a plain error; its expected
  conformance posture is a documented Skip of the ordered-yield subtests.
  No refusal sentinel (settled with Paul 2026-08-22) — no engine path can
  reach the rejection. **`OrderBy` with an ambient transaction is
  unsupported** (zero consumers: the async executor runs outside
  transactions; in-tx consumers are order-insensitive) — implementations
  return a plain error; the engine never makes that call. The tie-breaker
  rule applies to requested orders: the final sort key is always entity ID
  in the **engine's canonical ID order** (§5), so equal user-field values
  can never yield nondeterministic order.
- **`OrderSpec{Source: Meta, Path: "id"}` means the engine's canonical ID
  order; `Kind` is ignored for that path.** This must be stated on
  `OrderSpec` itself, because the existing SPI ordering docs promise the
  opposite ("every backend produces identical ordering", "Kind fixes the
  cross-backend comparison") — those sentences, and `SearchOptions`'
  empty-`OrderBy` default-order text, are rewritten to the per-engine
  contract in the same tag (§10). Applies uniformly to `Search`, `Iterate`,
  and the async default (§4.5).
  sqlite/postgres push `ORDER BY` into SQL; memory sorts its snapshot.
  Honesty note on the policy boundary: sqlite is embedded, so its sort runs
  in-process (temp B-tree with disk spill) — the memory rule bounds the
  engine's per-request heap; database sort machinery with disk spill is
  accepted, wherever it executes. Consumers that need the default result
  order request entity-ID ordering explicitly (`OrderSpec{Source: Meta,
  Path: "id"}` expresses it, served in the engine's canonical ID order, §5);
  all backends support at least that.
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
- A new exported SPI helper `MergeOrdered` provides the ordered streaming
  merge of a committed row stream with a sorted buffered overlay. Its
  consumer is `GetPage`'s in-transaction page (§5) — ordered `Iterate` never
  runs in a transaction (§2.1), so backends don't need an ordered overlay
  iterator.

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

A new `Iterable` conformance group (none exists today): requested-order yield
honoured (entity-ID and a user-field order asserted concretely; ordered
in-tx call asserted to error), tie-breaker, residual
filter application inside `Next`, ctx cancellation observed, sticky `Err`,
idempotent `Close`, PIT variant, snapshot-at-open overlay behaviour,
`TrackingRead` gating.

### 2.6 Optionality

`Iterable` stays optional SPI-wide. The engine's streamed async and delete
paths require it and use it whenever the store provides it (all four in-house
backends do); a store providing neither `Searcher` nor `Iterable` runs the
documented in-process fallback path, which #477 makes stream.

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
SaveResults(ctx context.Context, jobID string, epoch int64, entityIDs iter.Seq[string]) error
```

One call per claim epoch — which is one call per job until the re-execution
follow-up lands (a claimant then calls `ClearResults` and streams again at
its higher epoch; the contract text needs no amendment for that). Consumed
lazily. Contract:

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
  visibility is gated on `SUCCESSFUL` as today. Cleanup of partial rows:
  a job that goes terminal keeps its rows until TTL reap / `DeleteJob`
  (invisible either way); a job left RUNNING is claimed via §4.3 and the
  claimant clears them.
- **Self-executing stores** (the commercial backend) keep rejecting the call;
  the engine never invokes it for them.

sqlite prerequisite: iterators read from a dedicated reader connection (WAL
mode exists for exactly this); the plugin's single-connection model otherwise
deadlocks producer against consumer. Plugin-internal change, no SPI impact.

### 4.2 Terminal statuses are final (write-once)

`SUCCESSFUL`, `FAILED`, and `CANCELLED` are terminal. The store MUST refuse
any write against a terminal job — including a same-status rewrite:
`UpdateJobStatus`, `Heartbeat`, and `SaveResults` all return the new
sentinel `spi.ErrAlreadyTerminal` (distinguishable, so a losing writer can
log; for `SaveResults` the check runs at least at chunk boundaries, so a
deposed executor's stream aborts within one chunk); `ClaimStale` never
claims a terminal job. `Cancel` is the sole idempotent-nil exception, per
its tag-1 contract. This closes the zombie-executor race:
without it, an executor that stalls past the stale bound, gets reaped by
another node's claim-then-fail, and then recovers could overwrite FAILED with
SUCCESSFUL — a job the cluster declared dead serving results as complete.
The engine's poll loop (§4.6) aborts on **any** terminal status, not just
CANCELLED. spitest-asserted per transition.

### 4.3 Orphan handling: heartbeat + claim (takeover-shaped surface)

Crash mid-job currently leaves a RUNNING row and partial results forever (the
reaper skips RUNNING; nothing else cleans up). An async job is **re-executable
by construction** — its full definition (condition, options, pinned
PointInTime) is durably stored before execution — so the right long-term
disposition for an orphaned job is takeover and re-execution, not failure
(the commercial backend already has this property via its own shard
recovery; engine-executed backends must not be contractually weaker).
Settled with Paul 2026-08-22: **the SPI ships the takeover-shaped surface
now; the engine's re-execution orchestration is a tracked follow-up**, and
the interim engine disposition is claim-then-FAIL — observably identical to
plain orphan-failing, but nothing about it is contract, so the upgrade to
re-execution later touches zero SPI.

SPI additions:

```go
Heartbeat(ctx context.Context, jobID string, epoch int64) error
ClaimStale(ctx context.Context, staleAfter time.Duration, limit int) ([]*SearchJob, error)
ClearResults(ctx context.Context, jobID string) error

// changed signature (epoch added; all other params as today):
UpdateJobStatus(ctx context.Context, jobID string, epoch int64, status string,
    resultCount int, errMsg string, finishTime time.Time, calcTimeMs int64) error
```

Epoch initialization: `CreateJob` persists the job at epoch 1 regardless of
the struct's field value (store contract, spitest-asserted); the initial
executor runs at epoch 1. Every executor-side write — including the
panic-recovery FAILED write — passes the epoch the executor was started
with (1, or the claimed epoch after the re-execution follow-up).

- `SearchJob` gains `HeartbeatTime *time.Time` and `Epoch int64`. The epoch
  is the claim/attempt counter: the initial executor runs at epoch 1; every
  successful `ClaimStale` atomically increments it.
- **Fencing (the one piece of real new machinery, and why it must ship
  now):** takeover admits a scenario plain failing never had — the old
  executor may still be alive (GC pause, transient partition) while a new
  one writes results for the same job; unfenced, their rows interleave into
  a corrupt result set. Therefore every executor-side write carries its
  epoch — `Heartbeat`, `SaveResults`, and the engine's terminal
  `UpdateJobStatus` — and the store MUST refuse a write whose epoch is not
  the job's current epoch with the new sentinel `spi.ErrStaleClaim`. The
  deposed executor's next write errors and it aborts; the result set has
  exactly one author. Retrofitting the epoch parameter later would be a
  second breaking change to the same three methods — that is why the
  groundwork ships in tag 2 even though re-execution is deferred.
- `ClaimStale` atomically claims up to `limit` RUNNING jobs whose heartbeat
  (or baseline) is older than `staleAfter`: bumps `Epoch`, stamps
  `HeartbeatTime`, returns the claimed jobs. Conditional writes make
  concurrent claimers safe (a job is claimed by exactly one). CANCELLED or
  otherwise terminal jobs are never claimable.
- `ClearResults` deletes a job's result rows (the new claimant starts from a
  clean slate before re-executing). Idempotent.
- **Interim engine disposition** (this milestone): the reaper loop claims
  stale jobs and immediately marks them FAILED via the epoch-fenced
  `UpdateJobStatus`, with a finish time and a safe generic message — the
  lying-RUNNING defect is fixed now. **Follow-up** (tracked issue, §11): the
  claimant instead clears results and re-enqueues the job into its worker
  pool; shutdown stops draining-to-FAILED and simply stops heartbeating
  (peers or the restarted node reclaim); a startup sweep reclaims own jobs;
  an attempt cap (epoch bound) backstops crash-looping jobs with FAILED.
  Node failure then becomes invisible to async clients.
- The owning engine node heartbeats periodically from submission onward —
  **including while the job waits in the worker-pool queue** (the submitter
  is alive and owns the queue entry). The heartbeat runs on a **dedicated
  per-job ticker goroutine, independent of scan progress** — never in-band
  with the row stream, where a long non-yielding stretch (low-selectivity
  residual scan, slow save chunk) would starve it and let `ClaimStale` seize a
  healthy long-running job. `heartbeatInterval ≪ staleAfter` is a stated
  config invariant.
- **Clock domain**: the heartbeat stamp and the staleness comparison MUST use
  the same clock (store-side where one exists — SQL `now()` on postgres);
  `staleAfter` must dominate residual engine↔store skew for the
  `CreateTime` baseline case.
- **Nil heartbeat baseline is `CreateTime`** (fail closed): a job that never
  heartbeated — crash between `CreateJob` and the first stamp — goes stale
  `staleAfter` after creation, not never.
- `Heartbeat` error semantics: stale epoch → `ErrStaleClaim`; terminal job →
  `ErrAlreadyTerminal`; missing job → `ErrNotFound`. The executor treats
  **any** `Heartbeat` error as an abort signal (fail closed).
- **`ClaimStale` and `ReapExpired` are cross-tenant** and are called with a
  tenant-less context (the reaper loop owns no tenant) — stated in the
  interface doc, precedent `ScheduledTaskStore.ScanDue`. A tenant-scoped
  implementation would silently claim nothing and reinstate the
  orphan-forever defect. The claimant's *follow-up* writes
  (`UpdateJobStatus`, `ClearResults`) go through tenant-scoped stores: the
  reaper reconstructs a per-job tenant context from the claimed
  `SearchJob.TenantID` (memory's store errors without one — a tenant-less
  follow-up write would silently fail there).
- Self-executing stores may no-op the claim surface (`Heartbeat`,
  `ClaimStale`, `ClearResults`); resilience is their concern, and nothing
  here constrains their recovery model. The tag-2 consumer notice flags that
  the commercial backend's current recovery is rebalance-driven, not
  liveness-driven — a hung-but-owning node leaves shard rows RUNNING — so
  closing that gap is theirs. spitest covers the claim surface (claim
  atomicity, epoch fencing, clear) for engine-executed stores.

### 4.4 Job-record contract text (previously silent)

- `GetResultIDs`: `offset >= 0` and `limit >= 1` are required; violations
  return an error (today memory panics on negatives — a defect this contract
  makes testable). Reads against a non-terminal job answer with the results
  saved so far; `total` is the current saved count. The engine keeps gating
  result visibility on `SUCCESSFUL`, so this is an SPI-level clarification,
  not an API behaviour change.
- `UpdateJobStatus` on a missing job returns `ErrNotFound` (today memory and
  sqlite return a plain error — `jobLookupErr` cannot key on it).
- A zero `finishTime` is stored as absent (sqlite's NULL behaviour becomes
  the contract; memory currently stamps zero times — fixed).

### 4.5 Async result ordering is contractual end-to-end

Results are saved — and therefore read back — in the requested `OrderBy`;
when the request specifies none, the engine requests entity-ID ordering
explicitly (served in the engine's canonical ID order, §5). On the engine path the
order is inherited from §2.1's ordered iterator. The commercial backend
currently ignores the requested ordering (a live cross-backend divergence);
ordering is achievable there by storing result rows keyed by sort-value byte
encodings so read-back order is native storage order — prior art exists in
the commercial platform's distributed-reporting service (pointer shared with
the backend maintainers directly, not reproduced here). The divergence
becomes a tracked bug on their side; notification rides the tag-2 consumer
notice. A short cloud-parity note records async result ordering as contract.

### 4.6 Cancellation and shutdown (engine-side, enabled by tag 1 + this surface)

- jobID→CancelFunc registry in the engine; `CancelAsync` cancels in-process
  and dispatches store `Cancel(ctx, jobID, finishTime)` (tag 1).
- Cross-node: the executor's heartbeat/poll loop re-checks job status and
  cancels its own context on CANCELLED. Backend ctx-responsiveness is pinned
  per backend by tests.
- Bounded worker pool with queueing. The default size is a documented number
  whose rationale is the postgres connection budget (a streaming job needs
  its scan connection plus, per chunk, a save connection) — the engine cannot
  read plugin config through the SPI, so this is a documented default, not a
  computed one. Env vars follow Gate 4 (help topic, README, `DefaultConfig()`
  together). The queue bound and its saturation behaviour (a full queue at
  `POST /search/async` would be user-facing — new 429/503, touching §8) are
  deliberately left to the plan; the plan must decide them, not inherit
  "unbounded" silently.
- Shutdown drains via the registry: cancel in-flight jobs, mark FAILED (via
  the epoch-fenced write) with a safe message; no stuck-RUNNING jobs. The
  re-execution follow-up (#509) replaces this with release-and-reclaim —
  jobs then survive restarts.

## 5. Paged entity read (D3)

```go
GetPage(ctx context.Context, modelRef ModelRef, limit, offset int, asAt *time.Time) ([]*Entity, error)
```

- **Ordering: the engine's canonical entity-ID order** (settled with Paul
  2026-08-22). Each engine defines ONE total, stable, deterministic ID
  order and uses it consistently everywhere it orders by ID — `GetPage`,
  tie-breaking under user-field `OrderBy`, and explicit entity-ID ordering.
  The order is **engine-specific, not identical across engines**:
  cross-engine identity bought nothing real (no API doc promises a specific
  order; paging across a backend migration is meaningless), and demanding
  it made the commercial backend's natively time-clustered schema
  unserviceable. Each in-house backend documents byte-wise ascending
  (`COLLATE "C"` / `BINARY` / Go `<` — their native behaviour today) in
  `docs/plugins/`; the commercial backend documents its timeuuid (creation
  time) order in its own docs. The public API documents list order as
  stable and deterministic, specific order storage-engine-specific (one
  line in API docs + the cloud-parity note). spitest asserts determinism,
  paging self-consistency (page 0–19 + 20–39 ≡ 0–39), and conformance to
  the engine's declared comparator — `Harness` gains an optional
  ID-comparator hook defaulting to byte-wise. Parity tests assert
  set-equality + per-engine determinism, never cross-engine sequences.
  `limit >= 1`, `offset >= 0`; violations error.
- Serves `ListEntities` (HTTP `pageNumber × pageSize` maps directly; offset
  pagination is required by the public API's random page access).
- **In-transaction contract** (`ListEntities` is reachable inside a joined
  transaction via compute-node callbacks): when `asAt` is nil, `GetPage`
  overlays the transaction buffer like `GetAll` does today — the page
  reflects the merged view snapshotted at the call (ordered via the
  `MergeOrdered` rule), deleted entities excluded, and **the returned page
  recorded in the read-set — unconditionally, no `TrackingRead` knob**
  (its one consumer, the in-tx list, is a tracked read by intent). This is
  a deliberate narrowing, not preservation: today's in-tx `GetAll` records
  the whole model into the conflict read-set; `GetPage` records only the
  page, so first-committer-wins validation narrows from model-wide to
  page-wide. Committed-only reads would silently break a processor that
  saves then lists within its transaction. With `asAt` set, the read is
  committed-only — the SPI-wide in-transaction point-in-time rule.
- Per-entity load errors fail the call (fail-fast) — page boundaries must be
  deterministic.
- Index work required for the latency acceptance criterion — which reads
  precisely as: **no O(model) payload materialisation; offset cost is
  O(offset)** (true on every backend; SQL `OFFSET` itself scans O(offset)
  index entries, so "bounded by pageSize" must not be encoded literally in
  a test): postgres extends the model index to include `entity_id`
  with `COLLATE "C"`; sqlite adds a `(tenant_id, model_name, model_version,
  entity_id)` index. **Commercial backend**: under the per-engine ordering
  contract its native timeuuid clustering IS its canonical ID order — a
  k-way merge over its time-sorted shard cursors streams globally ordered
  IDs from today's schema: O(page) memory, O(offset) reads for the offset,
  no schema addition and no O(model) sort. (Their engine-side `ListEntities`
  order visibly changes from today's engine-sorted byte-wise order to their
  documented native order when they adopt `GetPage` — sanctioned by the
  per-engine contract, called out in the consumer notice.)
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
  one transaction). `ErrNotFound` when absent. **DELETED / payload-less
  versions never match** — today's scan skips them, so the transaction that
  deleted an entity answers 404, and a naive SQL pushdown on the
  transaction-ID column would silently change that (sqlite stores the ID on
  tombstone rows). spitest-asserted.
- Empty `txID` never reaches the store: the engine validates it upstream
  (the HTTP parameter is typed as a UUID). Defensively, implementations
  MUST NOT match empty stored transaction IDs (sqlite writes empty IDs for
  non-transactional saves) and return `ErrNotFound` for an empty input.
- postgres/sqlite push the match into SQL over the entity's own versions
  (bounded by that entity's history); memory maintains a per-entity
  transaction index. The commercial backend has no by-transaction access
  path in its change table (the transaction column sits behind the version
  in the clustering order): its profile is a scan of the entity's own change
  partition — bounded by that entity's history, and it must scan past a
  match to prove "earliest" since its clustering is version-descending — or
  a schema addition, their call via the consumer notice.

### 6.2 Metadata-only history

```go
GetVersionMetadata(ctx context.Context, entityID string, opts VersionMetadataOptions) ([]EntityVersionMeta, error)

type VersionMetadataOptions struct {
    From, Until *time.Time // inclusive window, nil = unbounded side
    Limit       int        // 0 = all
}
```

`Limit 0 = all` deliberately diverges from `GetPage`/`GetResultIDs` (which
require `limit >= 1`): metadata rows are bounded by one entity's history;
pages over models are not. The rationale is stated in the interface doc so
the divergence reads as intent, not accident.

- `EntityVersionMeta` carries what both consumers need and no payload:
  `Version`, `ChangeType`, `Timestamp`, `User`, `AttributedKind`, `Executor`,
  `TransactionID` (may be empty: non-transactional writes store none), and a
  canonical `Deleted bool` (derived from the change type — replacing the
  backend-divergent "`Entity` is nil on some backends" probe). No `TenantID`:
  stores are tenant-scoped by context, and the audit handler takes the tenant
  from the user context. Implementation note: memory must populate `Version`
  for DELETED versions (today it is left zero when the version carries no
  entity), or the Version-DESC tie-break breaks exactly where a delete shares
  a timestamp.
- Order: newest-first, tie-broken by `Version` descending (fixes the
  currently unstable equal-timestamp sort).
- Wire mappings pinned: the changes endpoint's `HasEntity` becomes
  `!Deleted` (today it probes `Entity != nil`, which is backend-divergent
  for tombstones — canonicalizing makes tombstone rows uniform across
  backends, a parity fix); `entityId` in the audit event comes from the
  request's own entity-ID parameter; `TransactionID` from the DTO.
- `GET /entity/{id}/changes` pushes its cutoff (`Until`) and its 1000 cap
  (`Limit`) down. The audit event search pushes its from/to window down; its
  merge with state-machine events and cursor pagination stay in Go over
  bounded metadata rows — accepted bound, stated here.

### 6.3 `GetVersionHistory` is deleted

No consumer of full-history-with-payloads remains (the three production
callers are covered by §6.1/§6.2; verified — no endpoint returns more than
one historical entity with data). Pre-1.0 collapse, no shim. The spitest
rewrite inventory (exhaustive as of this spec; re-verified at plan time):
the entity-suite `GetVersionHistory/Ordering` subtests, the attribution
round-trip subtest, the transaction-suite committed-outcome checks, and the
helpers doc that describes fixtures in its terms — all rewritten against the
replacement surface in the same tag. Consumer notification is mandatory:
conformance Skip maps hard-fail on unmatched keys, so subtest renames alone
can break the commercial backend's run.

## 7. Engine-side consumer reroutes (spec'd for surface justification)

- **Async executor**: ordered `Iterate` → engine wrapper (cancel poll +
  heartbeat + ctx) → `SaveResults(seq)`. Heap per job O(batch).
- **Conditional delete (atomic mode)**: two-phase inside the transaction —
  drain the iterator collecting **IDs only**, `Close()`, then delete per ID.
  No interleaving with an open iterator (illegal per §2.2). The atomic mode
  is inherently O(matched IDs) (the transaction buffer holds every delete);
  the O(page) mode is the existing batched delete, whose selection phase
  streams per batch with per-ID version guards — **except at a pinned
  point-in-time**, where batched selection is also O(matched IDs): deleting
  a live row cannot change what a historical snapshot matched, so the
  per-cycle re-scan the live-state O(page) mode relies on to shrink the
  match set would never terminate. The batched path therefore branches on
  `pointInTime` (`deleteBatched` in `internal/domain/entity/service.go`):
  nil-PIT re-opens a fresh committed-only iterator each cycle and lets
  already-deleted ids drop out on their own; a pinned PIT instead drains the
  selection in one full pass (`resolveBatchTargetsOnePass`), still via
  `spi.Iterable` and still never materialising full entity rows — only ids
  and version-guard baselines are kept. Conscious non-goal: the ID drain
  (atomic mode) still deserialises full entity rows — an `IterateOptions`
  projection knob is out of scope here.
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
| `POST /search/async/{entityName}/{modelVersion}` | 200 submit; 400 invalid condition; 401; 404 model (the submit endpoint has no limit parameter — the service-level limit>max guard is unreachable over HTTP) |
| `GET /search/async/{jobId}` | 200 page; 400 job not complete / pagination over max; 401; 404 job |
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
| Requested-order Iterate honoured (ordered in-tx call errors; self-executing-async backends may reject) incl. tie-breaker, residual, ctx cancel | spitest | — | implied by spitest on all backends | — |
| Overlay snapshot-at-open; TrackingRead gating | spitest | — | — | — |
| Terminal statuses write-once (`ErrAlreadyTerminal` per transition) | spitest | — | — | — |
| Search rejects Limit ≤ 0 | spitest | ✓ (direct handler already rejects) | — | ✓ existing |
| Streamed SaveResults: order, chunk seq continuity, ctx abort | spitest | — | — | — |
| Async results incremental; heap O(batch) (allocation assert, not timing) | ✓ engine | — | — | — |
| Cancel stops scan mid-flight; cross-node on postgres | ✓ registry | ✓ isolated (not parity) | — | ✓ cancel envelope |
| Heartbeat + ClaimStale orphan handling (nil-heartbeat baseline, queued-job heartbeat, concurrent claimers get disjoint jobs) | spitest + engine unit | ✓ | — | — |
| Epoch fencing: stale-epoch Heartbeat/SaveResults/UpdateJobStatus refused (`ErrStaleClaim`); ClearResults idempotent | spitest | — | — | — |
| Shutdown drain: no RUNNING left, FAILED safe message | ✓ engine | ✓ | — | — |
| Worker pool: ≤ poolSize concurrent, excess queue | ✓ engine | isolated e2e | — | — |
| GetResultIDs degenerate inputs error (no panic) | spitest | — | — | — |
| GetPage ordering/limit/offset; fail-fast | spitest | ✓ list endpoint | ✓ | ✓ ListEntities |
| `?pageSize=1` latency bounded by page, not N | — | ✓ (assert query shape / plan, not wall-clock) | — | — |
| GetVersionByTransaction earliest-wins; empty txID rejected; 404 | spitest | ✓ | ✓ | — (no gRPC surface) |
| GetVersionByTransaction pushdown: long-history latency bounded (query-shape assert) | — | ✓ | — | — |
| GetVersionMetadata window/limit/order; Deleted canonical | spitest | ✓ changes endpoint | ✓ | ✓ changes |
| Conditional delete over large model: O(IDs) atomic / O(page) batched | ✓ engine | ✓ isolated | ✓ behaviour | — |
| Async result ordering respected end-to-end | — | ✓ | ✓ (order asserted per backend) | ✓ |

Concurrency scenarios stay in isolated single-backend e2e, never the shared
parity suite. A missing cell at implementation time blocks merge unless
waived with a one-line reason. The two query-shape cells need a named test
hook (query capture or plan inspection) — the plan defines its mechanism; it
is not wall-clock assertion.

## 10. Deliverables checklist (tag 2 + cyoda-go)

- SPI: `IterateOptions{OrderBy, TrackingRead}`, `MergeOrdered` (GetPage's
  overlay merge), sentinel `ErrAlreadyTerminal`, strict-bounded `Search`,
  `SaveResults(iter.Seq[string], epoch)`, terminal write-once contract,
  epoch fencing + `ErrStaleClaim`, `Heartbeat`, `ClaimStale`, `ClearResults`,
  `SearchJob.HeartbeatTime` + `.Epoch`, job-record contract text (§4.4),
  `GetPage` (incl. in-tx overlay contract), `GetVersionByTransaction`,
  `GetVersionMetadata` + `EntityVersionMeta`, delete `GetVersionHistory`,
  drop `MergeBounded`'s now-dead `limit <= 0` unbounded mode (its doc calls
  the mode load-bearing; after this tag no caller passes it); spitest suites
  for all of the above incl. the `Harness` ID-comparator hook (§5); rewrite
  the existing SPI ordering docs to the per-engine contract (`OrderSpec` —
  `Path:"id"` means canonical ID order, `Kind` ignored; `OrderKind`'s
  "identical ordering" promise; `SearchOptions`' empty-`OrderBy` default
  text); CHANGELOG `### Breaking` with migration notes; consumer
  notification pre-merge.
- cyoda-go: plugin implementations (incl. sqlite reader connection, postgres
  ceiling port + chunked CopyFrom, index migrations), engine reroutes (§7),
  worker pool + registry + drain, Gate-4 doc trio for new env vars,
  canonical-ID-order sections in `docs/plugins/*.md` + the API-doc line that
  list order is stable but storage-engine-specific, cloud-parity note
  covering async result ordering AND the engine-specific list order,
  CHANGELOG.
- Commercial backend (their repo, via notification): signature updates;
  ordered async result storage (sort-key-encoded rows) as the fix for the
  now-tracked ordering divergence — flagging the sequencing overlap with the
  async wire-syntax translation work (spi#31) so ordering support doesn't
  build a second parser over the raw blob that work replaces; the
  per-engine ordering contract for `GetPage` (§5 — their native timeuuid
  order becomes their documented canonical order; the visible list-order
  change when they adopt it); the
  transaction-lookup access-path choice (§6.1); the rebalance-vs-liveness
  recovery gap (§4.3); conformance Skip-map updates.

## 11. Explicitly out of scope

- Removing the untranslatable-condition fallback (#477, preconditioned on the
  quantifier work; reminder bullet added there).
- Re-executing claimed orphan jobs (#509) — the SPI groundwork (claim
  surface + epoch fencing) is complete in tag 2 precisely so that follow-up
  needs no further SPI change; the interim disposition is claim-then-FAIL
  (§4.3).
- Scan-budget machinery removal (phase 3, own SPI tag).
- Write-path version-count growth (#167).
- Async job wire-syntax translation (spi#31).
