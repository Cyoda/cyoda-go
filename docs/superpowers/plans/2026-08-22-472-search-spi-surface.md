# Search SPI Surface (Streaming) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Implement the #472 SPI surface: streaming async search (worker pool, cancel, heartbeat/claim takeover groundwork), paged entity reads, and purposed history reads — SPI tag 2 plus the cyoda-go engine/plugin work.

**Architecture:** `spi.Iterable` becomes the single unbounded streaming read (ordered, tx-snapshot overlay); `Searcher.Search` becomes strictly bounded; `AsyncSearchStore` gains a streamed, epoch-fenced result save plus a takeover-shaped claim surface (interim disposition: claim-then-FAIL); `EntityStore` gains `GetPage` (per-engine canonical ID order) and two purposed history reads replacing the deleted `GetVersionHistory`.

**Tech Stack:** Go 1.26, `iter.Seq`, pgx v5, modernc/mattn sqlite, spitest conformance, testcontainers e2e.

**Spec:** `docs/superpowers/specs/2026-08-22-472-search-spi-surface-design.md` (commit `b2b7626`). Research: `docs/superpowers/research/2026-08-22-472-search-spi-surface-research.md`. Read both before starting any task.

## Global Constraints

- **Preconditions:** #474 merged in cyoda-go-spi main AND its cyoda-go pin bump merged (SPI tag 1 scope: `Cancel(ctx, jobID string, finishTime time.Time) error` + spitest FinishTime assertions). Verify before Task S1: `grep "Cancel(ctx context.Context, jobID string, finishTime" ../cyoda-go-spi/search_store.go` must match.
- **Cross-repo workflow:** SPI checkout at `../cyoda-go-spi` (absolute: `/Users/paul/go-projects/cyoda-light/cyoda-go-spi`). Local development uses a `use ../cyoda-go-spi` line in `go.work` — NEVER commit it, NEVER use `replace` in go.mod, NEVER `git add -A` (go.work is tracked). SPI commits push to a feature branch in that repo; the tag is cut at milestone end per its MAINTAINING.md (tag first, then pin bump in ONE cyoda-go commit). Consumer notification (KNOWN_CONSUMERS.md) must be posted BEFORE the SPI breaking PR merges, linked from the PR description.
- **Branch/PR:** all cyoda-go work on `feat/472-search-spi-spec` in this worktree; PR targets `release/v0.8.4`, NOT main.
- **Settled policy (do not re-litigate):** no server time guards; async unbounded in items and time; memory-unbounded materialisation is a defect; multi-node primary; fail closed; pre-1.0 breaking changes without shims; entity-ID ordering is per-engine canonical (NOT cross-engine identical); ordered `Iterate` is required only of backends whose async search the engine executes; terminal job statuses are write-once; every executor-side job write is epoch-fenced.
- **TDD:** every task is red→green; run the failing test before implementing. Use `go test -short ./...` or scoped packages during iteration; full suite + per-plugin + one `make race` only at the end (Task F2).
- **Plugin modules:** `plugins/memory|sqlite|postgres` are separate Go modules — root `go test ./...` does NOT cover them; run `go test ./...` inside each plugin directory.
- **Logging:** `log/slog` only; never log credentials/conditions verbatim at error level without sanitisation (job failure messages use the existing `jobFailureMessage` sanitiser).
- **No issue IDs** in code comments, error strings, log messages, OpenAPI, or help topics (PR bodies/commits/specs only).
- **Status vocabulary:** `"RUNNING"`, `"SUCCESSFUL"`, `"FAILED"`, `"CANCELLED"` — exact strings, as today.

## Stream map (dependencies; parallelise disjoint streams per superpowers:subagent-driven-development)

```
S1→S2→S3→S4→S5→S6        (SPI repo: interfaces + spitest + helpers + docs)
S* ──┬─ M1→M2             (memory plugin)      ─┐
     ├─ Q1→Q2→Q3          (sqlite plugin)       ├─ all three parallel after S6
     ├─ P1→P2→P3          (postgres plugin)    ─┘
     └─ E1→E2→E3→E4→E5→E6→E7  (engine; E1 may start after S1; E2+ need M1 green for unit backends)
docs/finishers: D1 (after E7), F1 (pin/tag checklist), F2 (full verification), F3 (PR)
```

Task numbering: S=SPI repo, M=memory, Q=sqlite, P=postgres, E=engine, D=docs, F=finishers.

---

### Task S1: SPI — async-store surface (fields, signatures, sentinels)

**Files:**
- Modify: `../cyoda-go-spi/search_store.go` (SearchJob struct, AsyncSearchStore interface)
- Modify: `../cyoda-go-spi/errors.go` (two sentinels)
- Modify: `../cyoda-go-spi/spitest/asyncsearch.go` (mechanical signature adaptation only — semantic rewrite is S2)

**Interfaces (Produces — every later task consumes these verbatim):**
```go
// search_store.go
type SearchJob struct {
    // ... existing fields unchanged ...
    HeartbeatTime *time.Time // last liveness stamp from the owning executor; nil = never stamped (baseline CreateTime)
    Epoch         int64      // claim/attempt counter; CreateJob persists 1; ClaimStale increments
}

type AsyncSearchStore interface {
    CreateJob(ctx context.Context, job *SearchJob) error // persists Epoch=1 regardless of job.Epoch
    GetJob(ctx context.Context, jobID string) (*SearchJob, error)
    UpdateJobStatus(ctx context.Context, jobID string, epoch int64, status string,
        resultCount int, errMsg string, finishTime time.Time, calcTimeMs int64) error
    SaveResults(ctx context.Context, jobID string, epoch int64, entityIDs iter.Seq[string]) error
    GetResultIDs(ctx context.Context, jobID string, offset, limit int) (entityIDs []string, total int, err error)
    DeleteJob(ctx context.Context, jobID string) error
    ReapExpired(ctx context.Context, ttl time.Duration) (int, error) // cross-tenant, tenant-less ctx
    Cancel(ctx context.Context, jobID string, finishTime time.Time) error // tag 1; idempotent-nil on terminal
    Heartbeat(ctx context.Context, jobID string, epoch int64) error
    ClaimStale(ctx context.Context, staleAfter time.Duration, limit int) ([]*SearchJob, error) // cross-tenant, tenant-less ctx
    ClearResults(ctx context.Context, jobID string) error // idempotent
}

// errors.go
var ErrAlreadyTerminal = errors.New("job is in a terminal status")
var ErrStaleClaim = errors.New("write fenced: stale claim epoch")
```

**Contract text to write as doc comments (copy the semantics from spec §4.1–§4.4 — these comments ARE the contract):**
- Terminal statuses (SUCCESSFUL/FAILED/CANCELLED) are write-once. `UpdateJobStatus`, `Heartbeat`, `SaveResults` against a terminal job return `ErrAlreadyTerminal` (SaveResults: checked at least at chunk boundaries). `Cancel` is the sole idempotent-nil exception. `ClaimStale` never claims a terminal job.
- Epoch fencing: `UpdateJobStatus`/`SaveResults`/`Heartbeat` MUST refuse a call whose `epoch != job.Epoch` with `ErrStaleClaim`.
- `SaveResults`: one call per claim epoch; yield order = save order = `GetResultIDs` order; store-internal batching; result sequence strictly increasing across chunks; store observes ctx; `nil` return means "everything yielded was durably stored", NOT job success.
- `GetResultIDs`: `offset >= 0 && limit >= 1` required, violation returns an error (never panics); non-terminal reads answer with results saved so far.
- `UpdateJobStatus` on a missing job returns `ErrNotFound`. Zero `finishTime` is stored as absent (NULL/nil).
- `Heartbeat` errors: stale epoch → `ErrStaleClaim`; terminal → `ErrAlreadyTerminal`; missing → `ErrNotFound`.
- `ClaimStale`: atomically claims up to `limit` RUNNING jobs whose heartbeat (baseline `CreateTime` when nil) is older than `staleAfter`; bumps `Epoch`, stamps `HeartbeatTime`; concurrent claimers obtain disjoint jobs. Staleness stamp and comparison use the same clock domain (store-side where one exists). Cross-tenant, like `ReapExpired` (cite the `ScheduledTaskStore.ScanDue` precedent comment style at `persistence.go:19-24`).
- Self-executing stores may reject `SaveResults` and no-op `Heartbeat`/`ClaimStale`/`ClearResults`.

**Steps:**
- [ ] **S1.1** In `../cyoda-go-spi`: create branch `feat/search-spi-tag2` off main. Add the two sentinels to `errors.go` with one-line doc comments each.
- [ ] **S1.2** Apply the `SearchJob` fields and interface changes above with the full doc-comment contract text. `go build ./...` — expect spitest compile failures.
- [ ] **S1.3** Mechanically adapt `spitest/asyncsearch.go` call sites to the new signatures: `UpdateJobStatus(ctx, id, 1, ...)`, `SaveResults(ctx, id, 1, slices.Values(ids))` (import `slices`), leaving all existing assertions semantically unchanged. `go build ./... && go test ./...` green (spitest is compile-checked here; it executes in plugin repos).
- [ ] **S1.4** Commit: `feat(search)!: epoch-fenced async job surface — Heartbeat, ClaimStale, ClearResults, streamed SaveResults`

### Task S2: SPI — spitest async-store conformance rewrite

**Files:**
- Modify: `../cyoda-go-spi/spitest/asyncsearch.go`
- Modify: `../cyoda-go-spi/spitest/spitest.go` (only if new Skip-key docs needed)

**Interfaces:**
- Consumes: S1 surface. Produces: subtest names that plugin Skip maps may reference — keep EXISTING names stable (`CreateAndGet`, `UpdateStatus/*`, `SaveAndGetResults/Pagination`, `Cancel`, `Cancel/NotFound`, `DeleteJob`, `ReapExpired`, `TenantIsolation`) and ADD: `Epoch/InitialisedToOne`, `Epoch/FencedWrites`, `Terminal/WriteOnce`, `Claim/StaleClaimed`, `Claim/FreshNotClaimed`, `Claim/NilHeartbeatBaseline`, `Claim/ConcurrentDisjoint`, `Claim/TerminalNeverClaimed`, `ClearResults/Idempotent`, `SaveResults/ChunkSeqContinuity`, `GetResultIDs/DegenerateInputs`, `GetResultIDs/NonTerminalPartial`, `UpdateStatus/MissingIsNotFound`, `UpdateStatus/ZeroFinishTimeAbsent`, `Heartbeat/Semantics`.

**Steps:**
- [ ] **S2.1** Write the new subtests. Core assertions (each ~10 lines, follow the existing `testAS*` style; `h.AdvanceClock` drives staleness):
```go
// Epoch/InitialisedToOne: CreateJob with job.Epoch=42 → GetJob().Epoch == 1
// Epoch/FencedWrites: UpdateJobStatus(...epoch 2...) on epoch-1 job → ErrStaleClaim;
//   Heartbeat(ctx,id,2) → ErrStaleClaim; SaveResults(ctx,id,2,seq) → ErrStaleClaim (before or at first chunk)
// Terminal/WriteOnce: UpdateJobStatus(...,1,"SUCCESSFUL",...) then UpdateJobStatus(...,1,"FAILED",...) → ErrAlreadyTerminal;
//   Heartbeat after terminal → ErrAlreadyTerminal; SaveResults after terminal → ErrAlreadyTerminal; Cancel after terminal → nil
// Claim/StaleClaimed: create; AdvanceClock(staleAfter+1); ClaimStale(staleAfter,10) returns the job with Epoch==2 and fresh HeartbeatTime
// Claim/FreshNotClaimed: create; Heartbeat(ctx,id,1); ClaimStale → empty
// Claim/NilHeartbeatBaseline: create (never heartbeat); AdvanceClock(staleAfter+1); claimed (baseline=CreateTime)
// Claim/ConcurrentDisjoint: two sequential ClaimStale calls — second returns empty for already-claimed jobs (epoch bumped = fresh heartbeat)
// Claim/TerminalNeverClaimed: SUCCESSFUL job stale forever → never returned
// ClearResults/Idempotent: save 3 ids at epoch 1 → ClearResults → GetResultIDs total 0 → ClearResults again → nil
// SaveResults/ChunkSeqContinuity: two SaveResults calls at epochs 1 then (after ClaimStale+ClearResults) 2 — second save's ids page back in second-save order (no PK collision, no interleave)
// GetResultIDs/DegenerateInputs: offset -1 → error (not panic); limit 0 → error; limit -1 → error
// GetResultIDs/NonTerminalPartial: RUNNING job with 5 ids saved → GetResultIDs(0,10) returns 5, total 5
// UpdateStatus/MissingIsNotFound: unknown UUID → errors.Is(err, spi.ErrNotFound)
// UpdateStatus/ZeroFinishTimeAbsent: UpdateJobStatus(..., time.Time{}, ...) → GetJob().FinishTime == nil
// Heartbeat/Semantics: missing job → ErrNotFound; wrong epoch → ErrStaleClaim; terminal → ErrAlreadyTerminal
```
- [ ] **S2.2** `go build ./... && go vet ./...` green (execution happens in plugin suites — the red→green cycle for these tests completes in M1/Q1/P1).
- [ ] **S2.3** Commit: `test(spitest): conformance for epoch fencing, terminal write-once, claim surface`

### Task S3: SPI — IterateOptions, ordering docs, strict-bounded Search

**Files:**
- Modify: `../cyoda-go-spi/iterable.go`, `../cyoda-go-spi/searcher.go`
- Modify: `../cyoda-go-spi/spitest/searcher.go` (ZeroLimit/NegativeLimit subtests)
- Create: `../cyoda-go-spi/spitest/iterable.go` (conformance group)
- Modify: `../cyoda-go-spi/spitest/spitest.go` (register the group; Harness hook)

**Interfaces (Produces):**
```go
// iterable.go
type IterateOptions struct {
    PointInTime  *time.Time
    OrderBy      []OrderSpec // empty = order unspecified; non-empty = MUST honour (engine-executed-async backends); no ambient tx
    TrackingRead bool        // read-set recording gate, same rule as SearchOptions.TrackingRead
}
// spitest/spitest.go — Harness gains:
IDOrder func(a, b string) int // nil = byte-wise (strings.Compare); the engine's canonical entity-ID comparator
```
Contract-text edits (doc comments): `Searcher.Search` — `Limit >= 1` REQUIRED; `Limit <= 0` is a contract violation returning an error (delete the "unbounded" paragraphs). `SearchOptions.OrderBy` empty-default text → "the engine's canonical entity-ID order (engine-specific, documented per backend)". `OrderSpec` — for `Source: Meta, Path: "id"` the comparator is the engine's canonical ID order and `Kind` is ignored. `OrderKind` — delete the "every backend produces identical ordering" promise, replace with the per-engine statement. `Iterable` doc: remains optional SPI-wide (the engine's streamed async/delete paths use it when present; stores with neither `Searcher` nor `Iterable` run the documented in-process fallback); ordered calls with an ambient transaction return an error; overlay = snapshot at `Iterate()` time, mutating the tx during iteration is forbidden; self-executing-async backends may reject ordered calls.

**Steps:**
- [ ] **S3.1** Apply `IterateOptions` fields + all doc-comment rewrites above. `go build ./...`.
- [ ] **S3.2** Rewrite `spitest/searcher.go` subtests `ZeroLimitUnbounded`→`ZeroLimitRejected`, `NegativeLimitUnbounded`→`NegativeLimitRejected`: both assert a non-nil error and empty result (note: renames can break consumer Skip maps — record both old names in the CHANGELOG migration notes).
- [ ] **S3.3** Create `spitest/iterable.go` — `runIterableSuite(t, h)` registered from `spitest.go` behind a `store.(spi.Iterable)` type-assertion guard (same pattern as `runSearcherSuite` at `searcher.go:75-77`). Subtests:
```go
// Unordered/YieldsAllMatches: zero-value Filter + empty OrderBy over 7 seeded entities → all 7, any order
// Ordered/EntityID: OrderBy {Meta,"id"} asc → yield order matches sort by h.IDOrder (default strings.Compare)
// Ordered/UserFieldWithTieBreak: OrderBy {Data,"status",OrderText} with duplicate values → within equal keys, h.IDOrder ascending
// Ordered/InTxErrors: inside withTx, OrderBy non-empty → Iterate returns error
// Residual/AppliedInNext: filter with a non-pushable leaf (Ne) → only matches yielded
// Ctx/CancelObserved: cancel ctx mid-iteration → Next()==false and Err() is ctx-derived
// Err/Sticky + Close/Idempotent: after Close, Next()==false; second Close nil
// PIT/SnapshotVariant: PointInTime set → pre-cutoff state yielded
// Overlay/SnapshotAtOpen: in tx: save entity, open unordered iterator, verify buffered entity visible exactly once
// TrackingRead/Gating: in tx with TrackingRead=false → read-set unchanged; =true → yielded ids recorded
//   (read-set observation: commit a conflicting write from a second tx and assert commit outcome, same technique as the transaction suite)
```
Use the searcher suite's seeding helpers (`seedSearcherEntities`, decoy interleaving) — copy the pattern, do not import test internals.
- [ ] **S3.4** `go build ./... && go vet ./...`; commit: `feat(search)!: strict-bounded Search, ordered Iterate contract, Iterable conformance`

### Task S4: SPI — MergeBounded unbounded-mode drop + MergeOrdered helper

**Files:**
- Modify: `../cyoda-go-spi/merge_bounded.go` + its test file
- Create: `../cyoda-go-spi/merge_ordered.go`, `../cyoda-go-spi/merge_ordered_test.go`

**Interfaces (Produces):**
```go
// MergeOrdered merges an already-ordered committed pull-stream with a sorted
// buffered overlay, excluding deleted ids, yielding the merged order.
// cmp is the total order both inputs are sorted by (final key: canonical ID).
func MergeOrdered(
    next func() (*Entity, bool, error), // ordered committed stream
    adds []*Entity,                     // sorted by cmp
    isDeleted func(entityID string) bool,
    cmp func(a, b *Entity) int,
) func() (*Entity, bool, error)
```

**Steps:**
- [ ] **S4.1** Write `merge_ordered_test.go` first: table-driven — interleaved merge order preserved; add replaces committed with same ID (overlay wins, single yield); deleted committed id skipped; committed stream error propagates; empty adds; empty committed. Run: FAIL (undefined).
- [ ] **S4.2** Implement `MergeOrdered` (two-pointer merge; on equal ID, overlay entity wins and committed is consumed). Tests green.
- [ ] **S4.3** `MergeBounded`: change the `limit <= 0` branch to return an error (`fmt.Errorf("MergeBounded: limit must be >= 1")`), rewrite its doc comment (delete "load-bearing unbounded mode"), update its tests (unbounded cases become error assertions). Green.
- [ ] **S4.4** Commit: `feat(search)!: MergeOrdered overlay merge; MergeBounded requires limit >= 1`

### Task S5: SPI — EntityStore read surface (GetPage, history reads, delete GetVersionHistory)

**Files:**
- Modify: `../cyoda-go-spi/persistence.go` (interface), `../cyoda-go-spi/types.go` (DTOs)
- Modify: `../cyoda-go-spi/spitest/entity.go`, `../cyoda-go-spi/spitest/transaction.go`, `../cyoda-go-spi/spitest/helpers.go`
- Modify: `../cyoda-go-spi/txcontext.go` (stale GetVersionHistory doc reference at :117)

**Interfaces (Produces):**
```go
// persistence.go — EntityStore gains, GetVersionHistory is DELETED:
GetPage(ctx context.Context, modelRef ModelRef, limit, offset int, asAt *time.Time) ([]*Entity, error)
GetVersionByTransaction(ctx context.Context, entityID, txID string) (*EntityVersion, error)
GetVersionMetadata(ctx context.Context, entityID string, opts VersionMetadataOptions) ([]EntityVersionMeta, error)

// types.go
type VersionMetadataOptions struct {
    From, Until *time.Time // inclusive window; nil side = unbounded
    Limit       int        // 0 = all (bounded by one entity's history — deliberate divergence from GetPage, stated in doc)
}
type EntityVersionMeta struct {
    Version       int64
    ChangeType    string
    Timestamp     time.Time
    User          string
    AttributedKind PrincipalKind
    Executor      Principal
    TransactionID string // may be empty (non-transactional writes)
    Deleted       bool   // canonical: derived from ChangeType == "DELETED"
}
```
Doc-comment contracts: `GetPage` — canonical per-engine ID order, `limit >= 1 && offset >= 0` else error, fail-fast on row errors, in-tx overlay (snapshot at call, page recorded in read-set unconditionally) only when `asAt == nil`, `asAt` reads committed-only. `GetVersionByTransaction` — earliest matching version; DELETED/payload-less versions never match; empty txID never matches stored-empty ids and returns `ErrNotFound`. `GetVersionMetadata` — newest-first, tie-break Version DESC.

**Steps:**
- [ ] **S5.1** Apply interface + DTO changes with contracts; delete `GetVersionHistory` from the interface. `go build ./...` — spitest breaks; that's next.
- [ ] **S5.2** Rewrite spitest: replace `GetVersionHistory/Ordering` with `GetVersionMetadata/Ordering` (save 3 versions + delete → 4 metas, newest first, `Deleted` true only on the tombstone, Version populated on ALL rows including the tombstone); replace the attribution round-trip's history read with `GetVersionMetadata` (asserts `User`, `AttributedKind`, `Executor` round-trip); rewrite the transaction-suite committed-outcome checks (`transaction.go:565-598`) against `GetVersionByTransaction` and/or `GetVersionMetadata`; add `GetVersionByTransaction/EarliestWins` (same tx saves twice → version 1 returned), `GetVersionByTransaction/DeletedNeverMatches` (deleting tx → ErrNotFound), `GetVersionByTransaction/EmptyTxID` (→ ErrNotFound), `GetPage/OrderAndBounds` (page 0-2 + 2-2 ≡ 0-4 under h.IDOrder; limit 0 → error; offset -1 → error), `GetPage/AsAtSnapshot`. Update `helpers.go:85` doc text.
- [ ] **S5.3** `go build ./... && go vet ./... && go test ./...` (SPI-repo unit tests only). Fix `txcontext.go:117` stale reference.
- [ ] **S5.4** Commit: `feat(entity)!: GetPage + purposed history reads replace GetVersionHistory`

### Task S6: SPI — CHANGELOG and doc sweep

**Files:**
- Modify: `../cyoda-go-spi/CHANGELOG.md`

**Steps:**
- [ ] **S6.1** Under `## [Unreleased]` `### Breaking`, add one bullet per surface change (S1–S5) with a migration note each (old→new signature, one line). Include: spitest subtest renames (`ZeroLimitUnbounded`→`ZeroLimitRejected`, `NegativeLimitUnbounded`→`NegativeLimitRejected`, `GetVersionHistory/Ordering`→`GetVersionMetadata/Ordering`) — Skip maps hard-fail on unmatched keys, consumers must update theirs.
- [ ] **S6.2** `grep -rn "GetVersionHistory\|MatchFilter" ../cyoda-go-spi --include="*.go" | grep -v _test` — zero hits outside historical CHANGELOG text.
- [ ] **S6.3** Commit: `docs(changelog): tag-2 breaking surface with migration notes`. Push branch `feat/search-spi-tag2`.

---

### Task M1: memory plugin — async store (fencing, claim, streamed save)

**Files:**
- Modify: `plugins/memory/search_store.go`
- Test: spitest conformance (already wired via the plugin's conformance test) + `plugins/memory/search_store_test.go` for memory-only details

**Interfaces:**
- Consumes: S1/S2 surface verbatim. Produces: a fully conformant `spi.AsyncSearchStore`.

**Steps:**
- [ ] **M1.1** Add `use ../cyoda-go-spi` to the repo-root `go.work` (UNCOMMITTED — verify `git status` never shows go.work staged; stage files explicitly in every commit of this plan, never `git add -A`).
- [ ] **M1.2** Run the plugin conformance test: `cd plugins/memory && go test -run 'Conformance' ./... 2>&1 | head -40` — RED (compile errors + missing methods).
- [ ] **M1.3** Implement on `searchJobEntry`/store:
```go
// entry gains: heartbeat *time.Time; epoch int64 (CreateJob sets epoch=1, ignores input)
// guard helper used by UpdateJobStatus/Heartbeat/SaveResults:
//   missing → spi.ErrNotFound (wrap)  ← replaces today's plain error
//   terminalStatuses[entry.job.Status] → spi.ErrAlreadyTerminal
//   epoch != entry.epoch → spi.ErrStaleClaim
// UpdateJobStatus: zero finishTime → entry.job.FinishTime = nil (replaces the always-stamp)
// SaveResults(ctx, id, epoch, seq): drain seq appending to entry.entityIDs; every 1024 yields re-run the
//   guard under the lock (chunk boundary) and check ctx.Err()
// Heartbeat: guard, then stamp entry.heartbeat via the factory clock (same clock ReapExpired compares with)
// ClaimStale: iterate ALL tenants (tenant-less ctx — do NOT resolve a tenant); for each RUNNING entry with
//   coalesce(heartbeat, job.CreateTime).Before(now.Add(-staleAfter)): epoch++, heartbeat=now, append job copy; stop at limit
// ClearResults: entry.entityIDs = nil; unknown job → nil (idempotent)
// GetResultIDs: offset < 0 || limit < 1 → fmt.Errorf(...) (kills the slice-arithmetic panic)
```
Mutex discipline: every critical section `Lock(); defer Unlock()` — IIFE-wrap the chunk-boundary re-guard inside SaveResults' drain loop.
- [ ] **M1.4** Conformance `AsyncSearch/*` green, incl. all S2 subtests. `go test ./...` in the plugin green.
- [ ] **M1.5** Commit: `feat(memory): epoch-fenced async job store with claim surface`

### Task M2: memory plugin — reads (strict Search, ordered Iterate, GetPage, history)

**Files:**
- Modify: `plugins/memory/searcher.go`, `plugins/memory/grouped_stats.go`, `plugins/memory/entity_store.go`

**Steps:**
- [ ] **M2.1** RED: `Conformance/Searcher` + `Conformance/Iterable` + `Conformance/Entity` — failures on the new contracts.
- [ ] **M2.2** `Search`: `opts.Limit <= 0` → `fmt.Errorf("search: limit must be >= 1")` at the top; the bounded-or-fail path becomes the only path (delete the unbounded gates in `matchSortBounded` and the tx-overlay call).
- [ ] **M2.3** `Iterate`: when `opts.OrderBy` non-empty — if `spi.GetTransaction(ctx) != nil` return `fmt.Errorf("iterate: ordered iteration inside a transaction is unsupported")`; else `sort.SliceStable` the snapshot with `spi.LessByOrder` (byte-wise ID tie-break is memory's documented canonical order). Gate read-set recording on `opts.TrackingRead`.
- [ ] **M2.4** `GetPage`: validate `limit >= 1 && offset >= 0`; merged view (committed map; in-tx + `asAt == nil` overlays the tx buffer exactly as `GetAll`'s tx branch; `asAt != nil` → PIT snapshot, committed-only); collect IDs, sort byte-wise, clamp-slice `[offset:offset+limit]`, `copyEntity` only the page; in-tx: record the returned page in the read-set (unconditional — no knob, per spec §5).
- [ ] **M2.5** History: maintain per-tenant `txIndex map[entityID]map[txID]int64` (earliest non-DELETED version), updated on every save; `GetVersionByTransaction`: empty txID → `spi.ErrNotFound`; index hit → copy of that version; miss → `spi.ErrNotFound`. `GetVersionMetadata`: walk the entity's version slice newest-first, filter From/Until, stop at Limit; **populate `Version` on DELETED rows** (fixes the zero-Version tombstone); `Deleted = changeType == "DELETED"`. Delete `GetVersionHistory` and its in-plugin callers.
- [ ] **M2.6** Conformance + plugin tests green. Commit: `feat(memory): strict search, ordered iterate, GetPage, purposed history reads`

### Task Q1: sqlite plugin — async store + schema migration

**Files:**
- Create: `plugins/sqlite/migrations/000002_search_epoch.up.sql` (+ `.down.sql`)
- Modify: `plugins/sqlite/search_store.go`

**Steps:**
- [ ] **Q1.1** Migration:
```sql
ALTER TABLE search_jobs ADD COLUMN heartbeat_time INTEGER;          -- UnixMicro; NULL = never stamped
ALTER TABLE search_jobs ADD COLUMN epoch INTEGER NOT NULL DEFAULT 1;
```
- [ ] **Q1.2** RED: `cd plugins/sqlite && go test -run 'Conformance/AsyncSearch' ./...`.
- [ ] **Q1.3** Implement. Fenced-write shape — single conditional statement, zero-rows classified by probe:
```sql
UPDATE search_jobs SET ... WHERE tenant_id=? AND id=? AND epoch=?
  AND status NOT IN ('SUCCESSFUL','FAILED','CANCELLED');
-- probe on RowsAffected==0: SELECT status, epoch FROM search_jobs WHERE tenant_id=? AND id=?
--   no row → spi.ErrNotFound; terminal status → spi.ErrAlreadyTerminal; else → spi.ErrStaleClaim
```
`SaveResults(ctx, id, epoch, seq)`: 500-id chunks, one short write-tx each; at each chunk start run the fenced probe + `ctx.Err()`; `seq` continues from a running in-call counter (rows are empty post-`ClearResults`, so 0-based never collides — proven by the `ChunkSeqContinuity` conformance case). `Heartbeat`: fenced `UPDATE ... SET heartbeat_time=?` using the store's injected clock (the same clock `ReapExpired` compares with — the clock-domain rule). `ClaimStale` (tenant-less): `SELECT tenant_id, id, epoch FROM search_jobs WHERE status='RUNNING' AND COALESCE(heartbeat_time, create_time) < ? LIMIT ?`, then per candidate a CAS `UPDATE ... SET epoch=epoch+1, heartbeat_time=? WHERE tenant_id=? AND id=? AND epoch=? AND status='RUNNING'`; only CAS winners are re-read (`GetJob`-shape scan) and returned. `ClearResults`: `DELETE FROM search_job_results WHERE tenant_id=? AND job_id=?`, idempotent. `GetResultIDs`: reject `offset < 0 || limit < 1` before SQL. `UpdateJobStatus`: missing → `spi.ErrNotFound`; zero finishTime stays NULL (already correct — keep).
- [ ] **Q1.4** Conformance green; plugin green. Commit: `feat(sqlite): epoch-fenced async job store, claim surface, chunked streamed save`

### Task Q2: sqlite plugin — reader connection, ordered Iterate, strict Search

**Files:**
- Modify: `plugins/sqlite/store_factory.go`, `plugins/sqlite/grouped_stats.go`, `plugins/sqlite/searcher.go`

**Steps:**
- [ ] **Q2.1** RED unit test in the plugin: start a non-tx `Iterate` iterator holding rows open, then call `SaveResults` from another goroutine with a 5s deadline — times out today (single connection). Also RED: `Conformance/Iterable`, `Searcher/ZeroLimitRejected`.
- [ ] **Q2.2** `store_factory.go`: open a second `*sql.DB` (`readDB`) on the same DSN, `SetMaxOpenConns(1)`, same PRAGMAs, no migration run; non-tx iterator construction (`sqliteIter` sites in `grouped_stats.go`) queries `readDB`; in-tx iteration stays on the tx connection. Comment: WAL is the reader-concurrency prerequisite for streamed saves. Close `readDB` in the factory's Close. Deadlock test green.
- [ ] **Q2.3** `Iterate`: `OrderBy` non-empty + ambient tx → error; else append `orderByClause(opts.OrderBy)` (reuse the searcher's builder — it already emits the `entity_id` tie-break; BINARY collation is sqlite's documented canonical order). Gate the tx branch's read-set recording on `opts.TrackingRead`.
- [ ] **Q2.4** `Search`: `Limit <= 0` → error at the top; unbounded gates deleted (`LIMIT n+1` pushdown condition loses its `Limit > 0` clause; overflow check unconditional; `searchTxOverlay` always passes a positive limit to `MergeBounded`).
- [ ] **Q2.5** Conformance + plugin green (scan-budget tests untouched — budget removal is a later phase). Commit: `feat(sqlite): dedicated reader connection, ordered iterate, strict bounded search`

### Task Q3: sqlite plugin — GetPage, index, history reads

**Files:**
- Modify: `plugins/sqlite/entity_store.go`, `plugins/sqlite/migrations/000002_search_epoch.up.sql` (append)

**Steps:**
- [ ] **Q3.1** Append to 000002: `CREATE INDEX idx_entities_model_id ON entities (tenant_id, model_name, model_version, entity_id);`
- [ ] **Q3.2** RED: `Conformance/Entity` new subtests.
- [ ] **Q3.3** `GetPage`: bounds check; non-tx `asAt==nil`: `SELECT ... FROM entities WHERE tenant_id=? AND model_name=? AND model_version=? AND NOT deleted ORDER BY entity_id LIMIT ? OFFSET ?`. `asAt != nil`: snapshot join (reuse the `searchSnapshotBase` family) + `ORDER BY entity_id LIMIT/OFFSET`, committed-only. In-tx `asAt==nil`: committed ordered scan on the tx connection with `LIMIT ?` = `offset+limit` (drain fully, close rows — no interleaving), merge with byte-wise-sorted tx-buffer adds via `spi.MergeOrdered`, skip `tx.Deletes`, slice the page, record the page in the read-set.
- [ ] **Q3.4** `GetVersionByTransaction`: empty txID arg → `spi.ErrNotFound` pre-query; else `SELECT <full version row> FROM entity_versions WHERE tenant_id=? AND entity_id=? AND transaction_id=? AND change_type != 'DELETED' ORDER BY version ASC LIMIT 1`; no row → `spi.ErrNotFound`.
- [ ] **Q3.5** `GetVersionMetadata`: metadata-only projection — `SELECT version, change_type, submit_time, <user/attribution cols>, transaction_id FROM entity_versions WHERE tenant_id=? AND entity_id=? [AND submit_time >= ?] [AND submit_time <= ?] ORDER BY version DESC [LIMIT ?]` — **never select `data`**; `Deleted = change_type == 'DELETED'`. Delete `GetVersionHistory` and its SQL. (Column names: read the 000001 migration and `scanSearchJob`-era scan helpers for the authoritative spelling before writing the query.)
- [ ] **Q3.6** Conformance + plugin green. Commit: `feat(sqlite): GetPage with model+id index, metadata-only history reads`

### Task P1: postgres plugin — async store + schema migration

**Files:**
- Create: `plugins/postgres/migrations/000003_search_epoch.up.sql` (+ `.down.sql`)
- Modify: `plugins/postgres/search_store.go`

**Steps:**
- [ ] **P1.1** Migration (verify the actual created-time column name in `000001_initial_schema.up.sql:93-108` before writing — the plan text uses `created_at`):
```sql
ALTER TABLE search_jobs ADD COLUMN heartbeat_time timestamptz;
ALTER TABLE search_jobs ADD COLUMN epoch bigint NOT NULL DEFAULT 1;
```
- [ ] **P1.2** RED: `cd plugins/postgres && go test -run 'Conformance/AsyncSearch' ./...` (Docker required).
- [ ] **P1.3** Implement. Fenced writes as in Q1 (conditional UPDATE + classify-by-probe). `Heartbeat` stamps `now()` server-side (clock-domain rule: `ClaimStale` also compares with `now()`). `ClaimStale` (tenant-less, single atomic statement):
```sql
UPDATE search_jobs j SET epoch = j.epoch + 1, heartbeat_time = now()
WHERE (j.tenant_id, j.id) IN (
  SELECT tenant_id, id FROM search_jobs
  WHERE status = 'RUNNING' AND COALESCE(heartbeat_time, created_at) < now() - $1::interval
  ORDER BY created_at LIMIT $2
  FOR UPDATE SKIP LOCKED)
RETURNING <all job columns>;
```
`SaveResults(ctx, id, epoch, seq)`: 1000-id batches; per batch a fenced probe + `ctx.Err()`, then `pool.CopyFrom` with `seq` from a running counter — the CopyFrom connection is held per batch, never per job. `ClearResults`: `DELETE FROM search_job_results WHERE job_id=$1 AND tenant_id=$2`, idempotent. `GetResultIDs` bounds check. `UpdateJobStatus` missing → `spi.ErrNotFound`; zero finishTime → NULL.
- [ ] **P1.4** Conformance green. Commit: `feat(postgres): epoch-fenced async store, SKIP LOCKED claim, chunked CopyFrom save`

### Task P2: postgres plugin — ceiling port, ordered Iterate, strict Search

**Files:**
- Modify: `plugins/postgres/grouped_stats.go`, `plugins/postgres/searcher.go`

**Steps:**
- [ ] **P2.1** RED: `Conformance/Iterable`, `Searcher/ZeroLimitRejected`, plus a plugin unit test for the ceiling error path on Iterate (template: the existing `searchUnderOwnCeiling` classification tests — 57014 → `searchCeilingError`).
- [ ] **P2.2** `Iterate` ceiling: under the same three-way gate as `searchCommitted` (`ceiling marker present && s.pool != nil && spi.GetTransaction(ctx) == nil`): acquire with `newAcquireContext`, `pool.Begin`, `set_config('app.current_tenant', ...)`, `SET LOCAL statement_timeout`, scan on the tx; `postgresIter.Close()` gains the rollback with `context.WithoutCancel` (mirror `searcher.go:140`); 57014 classified via `classifyScanError`. Ordered: ambient-tx + `OrderBy` → error; else append `orderByClause(opts.OrderBy)`.
- [ ] **P2.3** `Search`: `Limit <= 0` → error; unbounded gates deleted (LIMIT pushdown and overflow-raise conditions become unconditional).
- [ ] **P2.4** Conformance + plugin green. Commit: `feat(postgres): ceiling-scoped ordered iterate, strict bounded search`

### Task P3: postgres plugin — GetPage, COLLATE index, history reads

**Files:**
- Modify: `plugins/postgres/entity_store.go`, `plugins/postgres/migrations/000003_search_epoch.up.sql` (append)

**Steps:**
- [ ] **P3.1** Append: `CREATE INDEX idx_entities_model_entity_id ON entities (tenant_id, model_name, model_version, entity_id COLLATE "C") WHERE NOT deleted;`
- [ ] **P3.2** RED: `Conformance/Entity` new subtests.
- [ ] **P3.3** `GetPage`: bounds check; non-tx current-state: `SELECT doc FROM entities WHERE tenant_id=$1 AND model_name=$2 AND model_version=$3 AND NOT deleted ORDER BY entity_id COLLATE "C" LIMIT $4 OFFSET $5`. `asAt`: wrap the PIT base (`search_base.go`) with outer `ORDER BY entity_id COLLATE "C" LIMIT/OFFSET`, committed-only. In-tx `asAt==nil`: committed ordered scan ON THE TX CONNECTION with `LIMIT offset+limit`, drain fully and close rows BEFORE any other statement, merge with sorted buffered adds via `spi.MergeOrdered`, skip tx deletes, slice the page, record it in the read-set.
- [ ] **P3.4** `GetVersionByTransaction`: empty txID → `spi.ErrNotFound` pre-query; else `SELECT doc, version, valid_time FROM entity_versions WHERE tenant_id=$1 AND entity_id=$2 AND doc->'_meta'->>'transactionId' = $3 AND <not-deleted predicate> ORDER BY version ASC LIMIT 1`. Before writing the deleted predicate, read `scanEntitiesFilterDeleted` (`entity_store.go:541-575`) and reuse its exact `_meta` deletion probe as SQL.
- [ ] **P3.5** `GetVersionMetadata`: `SELECT version, valid_time, doc->'_meta' FROM entity_versions WHERE tenant_id=$1 AND entity_id=$2 [valid_time window] ORDER BY version DESC [LIMIT $n]` — decode ONLY the `_meta` JSON object per row (change type, user, attribution, transaction id); never unmarshal the full doc. Delete `GetVersionHistory`.
- [ ] **P3.6** Conformance + plugin green. Commit: `feat(postgres): GetPage with COLLATE "C" index, metadata-only history reads`

---

### Task E1: engine — bounded worker pool + queue + config + error code

**Files:**
- Create: `internal/domain/search/pool.go`, `internal/domain/search/pool_test.go`
- Modify: `app/config.go` (+ `app/config_test.go`), `cmd/cyoda/help/config_registry.go`
- Create: `cmd/cyoda/help/content/errors/SEARCH_QUEUE_FULL.md`
- Modify: `internal/common/error_codes.go`, `api/openapi.yaml` (503 response on the async submit op)

**Interfaces (Produces):**
```go
// pool.go
type jobFunc func(ctx context.Context)
// NewWorkerPool starts n workers draining a queue of cap qlen.
// Submit returns ErrQueueFull when the queue is full — never blocks.
// Drain(ctx) stops intake, cancels the pool ctx, waits for workers.
func NewWorkerPool(workers, queueLen int) *WorkerPool
func (p *WorkerPool) Submit(job jobFunc) error
func (p *WorkerPool) Drain(ctx context.Context)
var ErrQueueFull = errors.New("async search queue is full")
// error_codes.go: ErrCodeSearchQueueFull = "SEARCH_QUEUE_FULL"
```
Config (Gate-4 trio completed in D1): `CYODA_SEARCH_ASYNC_WORKERS` default **8**, `CYODA_SEARCH_ASYNC_QUEUE` default **256**. Rationale comment (documented default, not computed): postgres budget — a streaming job holds its scan connection plus, per chunk, a save connection; 8 workers ≤ (default 25 max conns − reserve)/2.

**Steps:**
- [ ] **E1.1** RED `pool_test.go`: N=2 workers, queue 1 — three instant Submits succeed (2 running + 1 queued), fourth returns `ErrQueueFull`; jobs run at most 2 concurrently (atomic high-water assert); `Drain` waits for in-flight jobs and cancels their ctx.
- [ ] **E1.2** Implement (channel of jobFunc, `workers` goroutines, `sync.WaitGroup`, pool-lifetime `context.WithCancel`). Green.
- [ ] **E1.3** Config fields + `DefaultConfig()` + registry binding + `config_test.go` cases (defaults + env override + rejection of `workers < 1` / `queue < 0` at startup — config is a QA'd artefact, hard error not clamp).
- [ ] **E1.4** Error mapping: `SubmitAsync`'s handler maps `ErrQueueFull` → `common.Operational(http.StatusServiceUnavailable, common.ErrCodeSearchQueueFull, "async search queue is full — retry later")` with `retryable: true`; gRPC snapshot-search handler maps it to its error envelope. Write `errors/SEARCH_QUEUE_FULL.md` (compact: what it means, that it is retryable, the two env vars that size the pool). Add the 503 response to the async submit operation in `api/openapi.yaml`. RED→GREEN: `TestErrCode_Parity` (run `go test ./cmd/cyoda/... -run ErrCode`), handler unit test for the mapping.
- [ ] **E1.5** Commit: `feat(search): bounded async worker pool with queue-full backpressure`

### Task E2: engine — streaming async executor (Iterate → SaveResults, heartbeat, cancel registry)

**Files:**
- Modify: `internal/domain/search/service.go` (SubmitAsync + new executor internals)
- Create: `internal/domain/search/executor_test.go`
- Modify: `app/app.go` (wire pool + registry + config into `NewSearchService` builders)

**Interfaces (Produces, consumed by E3/E7):**
```go
// service builders (chain like the existing With*):
func (s *SearchService) WithAsyncPool(p *WorkerPool) *SearchService
func (s *SearchService) WithHeartbeat(interval time.Duration) *SearchService
// registry (internal): jobID → context.CancelFunc; register on start, deregister on exit.
func (s *SearchService) CancelRunning(jobID string) bool // used by CancelAsync for in-process cancel
```
Executor algorithm (replaces the goroutine body at `service.go:569-650`; keep the panic recovery + health latch + sanitised messages):
1. `SubmitAsync`: after `CreateJob` (+ self-executing short-circuit, unchanged): build `jobCtx, cancel := context.WithCancel(spi.WithUserContext(pool ctx, uc))` (+ `AsyncScanContext` marker as today), register in the cancel registry, **start the heartbeat ticker goroutine immediately** (the spec requires heartbeating while queued — the submitter owns the queue entry), THEN `pool.Submit(job)`; on `ErrQueueFull`: stop the ticker, deregister, fenced `UpdateJobStatus(..., 1, "FAILED", ...)` is NOT written — the job row is deleted via `DeleteJob` (a job that never entered the queue must not linger RUNNING), and the error propagates to the transports (E1.4). The closure carries `jobCtx`, modelRef, cond, opts, jobID, epoch=1.
2. Executor (runs when a worker picks the job up): defer deregister + ticker stop.
3. The **heartbeat ticker goroutine** (dedicated, independent of scan progress, running since submit): every `interval`: `Heartbeat(jobCtx, jobID, 1)` — any error → `slog.Warn` + `cancel()`; every tick also `GetJob` and `cancel()` on ANY terminal status (cross-node cancel + terminal abort in one poll). Stops via jobCtx.
4. Translate cond → `spi.Filter`. Untranslatable → the existing `GetAll`-fallback path UNCHANGED (interim), but its result IDs are fed through the same `SaveResults` streaming call via `slices.Values(ids)`.
5. Translatable: `orderBy := resolved sort keys; if empty → the entity-ID OrderSpec` (`Path: "id"` with the meta Source — verify the exact `Source` constant name in the SPI's `searcher.go` before writing; the engine requests the canonical default explicitly). `it, err := iterable.Iterate(jobCtx, ref, filter, spi.IterateOptions{PointInTime: opts.PointInTime, OrderBy: orderBy})` — store must implement `spi.Iterable` here; if it implements `Searcher` but not `Iterable`, fail the job with the sanitised internal message (fail closed; all in-house stores implement it).
6. `seq := func(yield func(string) bool) { for it.Next() { if !yield(it.Entity().Meta.ID) { return } } }`; `saveErr := store.SaveResults(jobCtx, jobID, 1, seq)`; then `defer it.Close()` ordering: Close before terminal write; `prodErr := it.Err()`.
7. Terminal write (all epoch-fenced, epoch 1): producer/ctx/save error precedence — jobCtx cancelled → re-`GetJob`: if CANCELLED leave it (store cancel already stamped terminal); else FAILED(sanitised). prodErr or saveErr → FAILED(sanitised). Else `UpdateJobStatus(jobCtx, jobID, 1, "SUCCESSFUL", count, "", now, calcMs)` where `count` is the engine's yield counter. `ErrAlreadyTerminal`/`ErrStaleClaim` from the terminal write → `slog.Warn` (lost the race; correct state already written), no health latch.
8. Panic recovery: as today, but the FAILED write passes epoch 1 and tolerates `ErrAlreadyTerminal`/`ErrStaleClaim`.

**Steps:**
- [ ] **E2.1** RED `executor_test.go` (memory store factory + tiny pool). Tests: (a) results order = requested order incl. default entity-ID order — 50 seeded, verify `GetResultIDs` sequence; (b) **incremental streaming**: fake `AsyncSearchStore` wrapper whose `SaveResults` records the maximum number of ids yielded between two consecutive store-side "consumed" observations — feed 10_000 matches, assert the engine never handed a materialised slice (wrapper sees interleaved production, high-water ≪ total; concrete assert: the wrapper consumes with a 1ms delay every 1000 ids and asserts the producer goroutine is blocked in yield, not done); (c) cancel mid-flight: seed slow store (yield-delay), `CancelRunning(jobID)` + store `Cancel` → job ends CANCELLED, iterator closed, no SUCCESSFUL overwrite (terminal write-once observed); (d) heartbeat recorded while queued and while scanning (fake clock / short interval — assert `GetJob().HeartbeatTime` advances); (e) heartbeat error aborts: fence the job (bump epoch directly in the store) → executor aborts, no result corruption; (f) untranslatable condition still completes via fallback with results saved.
- [ ] **E2.2** Implement per the algorithm. Green.
- [ ] **E2.3** `CancelAsync`: keep the tag-1 store `Cancel(ctx, jobID, finishTime)` dispatch AND call `CancelRunning(jobID)` (in-process immediate abort). Unit test: cancel of a job running in-process aborts without waiting for the poll tick.
- [ ] **E2.4** Wire in `app/app.go`: construct pool from config, `WithAsyncPool(...).WithHeartbeat(cfg.SearchJobHeartbeatInterval)`; `App.Shutdown` calls `pool.Drain(ctx)` then marks each still-registered job FAILED via the fenced write with the safe message (interim disposition; note pointing to the re-execution follow-up).
- [ ] **E2.5** Root `go test ./internal/domain/search/... -v` green. Commit: `feat(search): streaming async executor with heartbeat, cancel registry, bounded pool`

### Task E3: engine — reaper claim-then-FAIL + stale config

**Files:**
- Modify: `app/app.go` (reaper loop), `app/config.go`
- Create: `internal/domain/search/reaper.go`, `internal/domain/search/reaper_test.go`

**Interfaces (Produces):**
```go
// reaper.go — called from the app reaper ticker alongside ReapExpired:
// FailStaleJobs claims up to batch stale RUNNING jobs and marks each FAILED
// (epoch-fenced, per-job tenant ctx reconstructed from SearchJob.TenantID).
func FailStaleJobs(ctx context.Context, store spi.AsyncSearchStore, staleAfter time.Duration, batch int) (int, error)
```
Config: `CYODA_SEARCH_JOB_HEARTBEAT_INTERVAL` default **15s**; `CYODA_SEARCH_JOB_STALE_AFTER` default **5m**; startup validation: `staleAfter >= 4×interval` (the spec's `interval ≪ staleAfter` invariant made checkable).

**Steps:**
- [ ] **E3.1** RED `reaper_test.go` (memory store): create RUNNING job with old CreateTime (no heartbeat) → `FailStaleJobs` → job FAILED with finish time + generic message (assert message is the sanitised constant, not internals); fresh-heartbeat job untouched; SUCCESSFUL job untouched; claimed-but-fence-lost path: bump epoch between claim and write → `ErrStaleClaim` tolerated, counted as skipped.
- [ ] **E3.2** Implement: `ClaimStale(bgCtx, staleAfter, batch)`; per claimed job build `spi.WithUserContext(ctx, systemUserContextFor(job.TenantID))` (reuse the pattern the engine uses for background tenant work — find the existing system-principal constructor before writing one), then `UpdateJobStatus(tenantCtx, job.ID, job.Epoch, "FAILED", 0, jobFailureFallback, now, 0)`.
- [ ] **E3.3** Wire into the existing reaper ticker in `app/app.go` (same loop that calls `ReapExpired`), config validation in `app/config.go` + tests. Green.
- [ ] **E3.4** Commit: `feat(search): stale-job claim-then-fail reaper (takeover groundwork disposition)`

### Task E4: engine — conditional delete two-phase + sibling reroutes

**Files:**
- Modify: `internal/domain/entity/service.go` (`DeleteEntitiesConditional` single-tx path `:1030-1075`, `deleteBatched` `:1150-1199`, `DeleteAllEntities` `:841`)
- Test: `internal/domain/entity/delete_stream_test.go` (new)

**Steps:**
- [ ] **E4.1** RED: unit tests on the memory backend: (a) single-tx conditional delete of 5k matched / 5k decoys — correct MatchedCount, decoys survive, and a fake/spy proves the selection never called `Search` (assert via a Searcher-spy factory) and never materialised entities (spy Iterable counts `Entity()` calls == matches, no slice retention possible to assert directly — assert the code path via the spy interfaces); (b) `deleteBatched` with batchSize 100 over 1k matches: ≤ (matches/batch)+1 iterator opens, per-batch commit visible; (c) `DeleteAllEntities` returns ids/count without `GetAll` (spy asserts `Iterate` used); (d) untranslatable condition on the single-tx path still deletes via the fallback (interim).
- [ ] **E4.2** Single-tx path: replace the `searchSvc.Search(txCtx, ..., Limit:-1)` call with: translate cond→filter (fallback on error, unchanged); `it, _ := iterable.Iterate(txCtx, ref, filter, spi.IterateOptions{PointInTime: pit /* nil OrderBy, no TrackingRead */})`; drain into `ids []string` ONLY; `it.Close()` **before** the first delete (the no-interleave rule); then the existing per-id delete loop.
- [ ] **E4.3** `deleteBatched`: per cycle open a committed-only iterator, pull up to batchSize ids, Close, resolve+delete with the existing per-id version guards, commit, repeat until an empty pull; drop the whole-`matched` slice and the O(matches) `targets` buffer. `cond == nil` uses a zero-value `spi.Filter` through the same iterator (replaces `GetAll`).
- [ ] **E4.4** `DeleteAllEntities`: same zero-filter iterator drain for ids/count (payloads never materialised).
- [ ] **E4.5** Green; scoped e2e: `go test ./internal/e2e/... -run 'TestDelete' -v`. Commit: `feat(entity): streamed selection for conditional/batched/all deletes`

### Task E5: engine — ListEntities via GetPage

**Files:**
- Modify: `internal/domain/entity/service.go:1339-1424`
- Modify: `e2e/parity/registry.go` + new parity scenario file `e2e/parity/list_paging.go`

**Steps:**
- [ ] **E5.1** RED: service unit test (memory): page 1 size 3 over 10 entities == ids 3..5 in byte-wise order; page past end → empty; parity scenario (registered): pages are deterministic (same call twice → same ids), self-consistent (page0+page1 == page-size-2×0..1 union), and set-equal to the full model — NO cross-engine sequence assertion.
- [ ] **E5.2** Replace `GetAll`/`GetAllAsAt` + sort + slice with `entityStore.GetPage(ctx, ref, int(page.PageSize), int(page.PageNumber)*int(page.PageSize), pointInTime)`. Envelope construction unchanged.
- [ ] **E5.3** Green: unit + `go test ./internal/e2e/... -run 'List' -v` + parity on memory/sqlite/postgres. Commit: `feat(entity): ListEntities pages at the store via GetPage`

### Task E6: engine — history read rewires

**Files:**
- Modify: `internal/domain/entity/service.go` (`getEntityByTransactionID :398-412`, `GetChangesMetadata :736-792`), `internal/domain/audit/handler.go:55-101`
- Modify: `e2e/parity/registry.go` + `e2e/parity/history_reads.go` (new)

**Steps:**
- [ ] **E6.1** RED: unit tests (memory): (a) `?transactionId=` returns the earliest matching version's entity; deleting-tx txID → 404 (`ErrNotFound` mapping unchanged); (b) changes metadata: `HasEntity == !Deleted` (tombstone row present with `HasEntity=false` uniformly), newest-first, equal-timestamp rows ordered by Version DESC, 1000-cap pushed down (spy asserts `opts.Limit == 1000`, `opts.Until == cutoff`); (c) audit search: window pushed down (spy asserts From/To), `entityId` sourced from the request parameter, `actor.legalId` from the caller's tenant (user context), merge with SM events + cursor pagination unchanged.
- [ ] **E6.2** Implement: `getEntityByTransactionID` → `store.GetVersionByTransaction(ctx, entityID, txID)` (return `v.Entity`); `GetChangesMetadata` → `GetVersionMetadata(ctx, id, spi.VersionMetadataOptions{Until: cutoff, Limit: 1000})`, envelope maps `HasEntity: !m.Deleted`, `TransactionID: m.TransactionID`; audit handler → `GetVersionMetadata(ctx, id, spi.VersionMetadataOptions{From: params.FromUtcTime, Until: params.ToUtcTime})`, event map fields from the meta DTO + request param + ctx tenant. `grep -rn "GetVersionHistory" internal/ app/` → zero hits.
- [ ] **E6.3** Parity scenario: seed 3 saves + 1 delete; assert changes-metadata shape identical across backends (incl. the tombstone row) and by-transaction earliest-wins; register in `registry.go`.
- [ ] **E6.4** Green: unit + `go test ./internal/e2e/... -run 'Changes|Audit|Transaction' -v` + parity. Commit: `feat(entity): history endpoints use purposed metadata/transaction reads`

### Task E7: engine — matrix reconciliation + isolated concurrency e2e

**Files:**
- Create: `internal/e2e/async_stream_test.go`, `internal/e2e/async_cancel_multinode_test.go` (isolated; NOT parity)
- Modify: `e2e/parity/registry.go` (+ `e2e/parity/async_ordering.go`)
- Modify: `internal/grpc/search_test.go` (envelope assertions for queue-full + cancel)

**Steps:**
- [ ] **E7.1** Isolated e2e (single-backend postgres, real HTTP): (a) submit async over a seeded model; poll status to SUCCESSFUL; assert `GET /search/async/{id}` pages ordered by the requested sort with entity-ID tie-break, `totalPages` arithmetic intact; (b) cancel mid-flight: seed a large model, submit, cancel immediately via `PUT .../cancel`, assert terminal CANCELLED and results endpoint answers 400 job-not-complete — never SUCCESSFUL afterwards (poll a further 2s); (c) **cross-node**: two `SearchService` instances over ONE postgres store factory (the TestMultiNode two-app pattern) — submit on A, cancel via B's service (store cancel only, no in-process registry hit), assert A's executor aborts via its status poll within 2× heartbeat interval; (d) queue-full: app configured `WORKERS=1, QUEUE=1` + a store whose Iterate blocks — third submit → HTTP 503 `SEARCH_QUEUE_FULL`; (e) shutdown drain: start a slow job, `App.Shutdown`, assert job FAILED with the safe message, not RUNNING.
- [ ] **E7.2** Parity `async_ordering.go`: submit with an explicit user-field sort on each backend; assert per-backend deterministic order respecting the sort keys (tie-break per that engine's documented ID order — use set+pairwise-key assertions, no cross-engine sequence compare).
- [ ] **E7.3** gRPC: envelope tests — snapshot submit under full queue → error envelope with `SEARCH_QUEUE_FULL`; snapshot cancel → `Success`; ListEntities + changes-metadata gRPC paths still green (`go test ./internal/grpc/... -v`).
- [ ] **E7.4** Matrix audit: open spec §9; for every row write the test name(s) into a checklist comment block at the top of `internal/e2e/async_stream_test.go`; two sanctioned waivers to record there verbatim: (1) "query-shape asserts implemented as plugin-level EXPLAIN tests (Q3/P3), not HTTP e2e — layer shift, same guarantee"; add those EXPLAIN tests now if Q3/P3 did not: sqlite `EXPLAIN QUERY PLAN` asserts `idx_entities_model_id` usage for GetPage and the by-transaction query; postgres `EXPLAIN` asserts the partial COLLATE index for GetPage; (2) "O(batch) heap asserted structurally via the E2.1 interleave fake, not via allocation counters". Any other empty cell: add the test or a one-line waiver — a silently missing cell blocks merge.
- [ ] **E7.5** Green: `go test ./internal/e2e/... -v` (Docker). Commit: `test(search): matrix-complete e2e — ordering, cancel cross-node, queue-full, drain`

---

### Task D1: documentation (Gate 4 + spec §10 doc deliverables)

**Files:**
- Modify: `cmd/cyoda/help/content/config/*.md` (the topic that documents `CYODA_SEARCH_*` today — locate via `grep -rl CYODA_SEARCH cmd/cyoda/help/content/`), `README.md` (env table), `docs/ARCHITECTURE.md` (async search + list/history sections; audit the touched sections for stale claims per the reference-not-history rule)
- Modify: `docs/plugins/SQLITE.md`; create `docs/plugins/MEMORY.md`, `docs/plugins/POSTGRES.md` if absent (minimal: canonical entity-ID order section each)
- Create: `docs/cloud-parity/2026-08-22-async-ordering-and-list-order.md`
- Modify: `CHANGELOG.md`, `api/openapi.yaml` (list-endpoint description line)

**Steps:**
- [ ] **D1.1** Env vars (4 new: `CYODA_SEARCH_ASYNC_WORKERS`, `CYODA_SEARCH_ASYNC_QUEUE`, `CYODA_SEARCH_JOB_HEARTBEAT_INTERVAL`, `CYODA_SEARCH_JOB_STALE_AFTER`): help topic + README + `DefaultConfig()` comments — all three in one commit (Gate 4). Include the `staleAfter >= 4×interval` validation rule and the pool-sizing rationale one-liner.
- [ ] **D1.2** Canonical-order docs: each plugin doc states its entity-ID order (memory/sqlite/postgres: byte-wise ascending) and that list order is engine-specific by contract. `api/openapi.yaml` list endpoint description gains: "Order is stable and deterministic; the specific order is storage-engine-specific (entity-ID based)." Same line in the relevant help topic.
- [ ] **D1.3** Cloud-parity note: async result ordering is contractual (requested order, engine-default entity-ID order); list order is per-engine canonical; commercial backend impact summarised (their ordering divergence + GetPage/by-transaction access paths are notified via the SPI consumer notice).
- [ ] **D1.4** `CHANGELOG.md` under Unreleased: Added (worker pool env vars, queue-full 503, streamed async results, paged list reads, purposed history reads), Changed (async results now saved incrementally; conditional delete streams selection), Fixed (orphaned RUNNING jobs now failed via claim; unstable equal-timestamp changes ordering; tombstone `hasEntity` uniform across backends). No `### Breaking` for the binary (wire surface unchanged; SPI breaking notes live in the SPI CHANGELOG).
- [ ] **D1.5** ARCHITECTURE.md: rewrite the async-search execution + list/history read paragraphs to the new design (present tense, no history); delete any sentence the change falsifies.
- [ ] **D1.6** Commit: `docs: search streaming env vars, per-engine order contract, cloud-parity note`

### Task F1: SPI merge + pin bump (ONE commit)

**Steps:**
- [ ] **F1.1** Post the consumer notification: an issue/comment in the commercial backend repo summarising every `### Breaking` entry from S6 (signature table old→new), the spitest subtest renames + new groups (their Skip-map update), the per-engine ordering contract (their timeuuid order becomes their documented canonical order; the visible list-order change when they adopt `GetPage`), the ordered-async requirement (sort-key-encoded result storage guidance; sequencing overlap with the async wire-syntax translation issue), the by-transaction access-path choice, and the rebalance-vs-liveness recovery note. Link the notification from the SPI PR description.
- [ ] **F1.2** Open the SPI PR (`feat/search-spi-tag2` → main), merge after review. NO tag yet — the milestone tag is cut when all v0.8.4 SPI changes are in (per MAINTAINING.md); cyoda-go rides the pseudo-version meanwhile.
- [ ] **F1.3** In cyoda-go, ONE commit: bump `go.mod` (root + all three plugin go.mods) to the new SPI pseudo-version (`go get github.com/cyoda-platform/cyoda-go-spi@<merged-sha>` in each module), `make repin-plugins` for the plugin pseudo-pin window, `go mod tidy` everywhere. REMOVE the `use ../cyoda-go-spi` line from go.work. Verify consumer resolution: `GOWORK=off go build ./...` at root AND in each plugin.
- [ ] **F1.4** Commit: `chore(spi): pin search tag-2 surface` (COMPATIBILITY.md is updated at the real tag/release, not the pseudo-pin — note this in the commit body).

### Task F2: full verification (end-of-deliverable)

**Steps:**
- [ ] **F2.1** `go vet ./...` (root) + `go vet ./...` in each plugin.
- [ ] **F2.2** Root full suite: `go test ./... -v` (includes `internal/e2e`; Docker running; local postgres image per the known-good local images note).
- [ ] **F2.3** Per-plugin: `cd plugins/memory && go test ./...`; same for sqlite, postgres.
- [ ] **F2.4** One `make race` (CI-parity scope). Fix anything red before proceeding — no PR with failing tests.
- [ ] **F2.5** `make todos` — no new TODOs introduced by this work.

### Task F3: review gates + PR

**Steps:**
- [ ] **F3.1** Dispatch the fresh-context code review per `superpowers:requesting-code-review` (standing request — subagent, full diff of this branch vs `release/v0.8.4`). Address findings per `superpowers:receiving-code-review`.
- [ ] **F3.2** Run the security review skill (`antigravity-bundle-security-developer:cc-skill-security-review`) over the diff — attention points: tenant isolation of the cross-tenant `ClaimStale`/`ReapExpired` (results/jobs never leak across tenants in claims), no condition/query text in error responses or logs above DEBUG, queue-full response carries no internals.
- [ ] **F3.3** `gh pr create` targeting `release/v0.8.4`, milestone `v0.8.4`, body: summary, spec/plan/research links, coverage-matrix statement, the two waivers from E7.4, closes-refs for the issues this delivers against (per the release-branch issue-closure convention — closing happens manually at merge). PR body ends with the standard generated-with footer.

---

## Coverage-matrix carry-forward (spec §9 → tasks; gap-free per `.claude/rules/test-coverage.md`)

| Spec §9 row | Task(s) |
|---|---|
| Ordered Iterate honoured / in-tx errors / tie-break / residual / ctx | S3 (spitest) → M2/Q2/P2 |
| Overlay snapshot-at-open; TrackingRead gating | S3 → M2/Q2/P2 |
| Terminal write-once | S2 → M1/Q1/P1 |
| Search rejects Limit ≤ 0 | S3 (spitest) + E7.3 (gRPC existing) + HTTP existing |
| Streamed SaveResults order/chunk-seq/ctx | S2 → M1/Q1/P1 |
| Async incremental, O(batch) | E2.1 (structural fake; waiver recorded E7.4) |
| Cancel mid-flight + cross-node | E2.1(c), E7.1(b)(c) |
| Heartbeat + ClaimStale (baseline, queued, concurrent) | S2 → plugins; E2.1(d), E3.1 |
| Epoch fencing + ClearResults | S2 → M1/Q1/P1; E2.1(e) |
| Shutdown drain | E2.4, E7.1(e) |
| Worker pool bounds / queue-full | E1.1, E7.1(d), E7.3 |
| GetResultIDs degenerate inputs | S2 → plugins |
| GetPage order/limit/offset/fail-fast + parity + gRPC | S5 → M2/Q3/P3; E5; E7.3 |
| pageSize latency (query shape) | Q3/P3 EXPLAIN tests via E7.4 (waiver: layer shift) |
| GetVersionByTransaction earliest/empty/404 (+ no gRPC surface) | S5 → plugins; E6 |
| By-transaction query shape | E7.4 EXPLAIN tests |
| GetVersionMetadata window/limit/order/Deleted | S5 → plugins; E6 |
| Conditional delete O(IDs)/O(page) | E4.1, E7.4 note |
| Async ordering end-to-end | E7.1(a), E7.2 (parity), E7.3 |

## Execution notes

- Streams M, Q, P are disjoint after S6 — run as parallel subagent streams (separate worktrees not needed; the plugin directories are disjoint modules — but if parallel agents edit shared files (`go.work`), serialise those steps).
- E1 can start immediately after S1 (its only SPI dependency is compile-level).
- The engine cannot compile against the new SPI until S1 lands and the `go.work` use-line exists — all E tasks depend on that.
