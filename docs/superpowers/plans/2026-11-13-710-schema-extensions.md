# Schema Extensions: Never a Conflict, Never a Slowdown — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Every backend commits a schema extension in a short transaction of its own, never fails a write because another write extends the same model, never loses a returned extension, and adds no work to writes whose data fits the schema.

**Architecture:** `ModelStore.ExtendSchema` never joins the caller's transaction. postgres runs each extension in a short `READ COMMITTED` transaction on a dedicated model pool under a per-model `FOR NO KEY UPDATE` lock; memory and sqlite keep their own-transaction paths and gain the same checks. The model cache gets an invalidation generation (fill guard, per-generation single-flight, refresh without evicting) and coalesces identical extensions per node; the negative path cache is deleted.

**Tech Stack:** Go 1.27.4, pgx/v5 + pgxpool, modernc sqlite, `golang.org/x/sync/singleflight`, testcontainers-go, cyoda-go-spi (`spitest`).

**Spec:** `docs/superpowers/specs/2026-11-12-710-schema-extensions-design.md` (approved at c02e8e93). Section numbers below (§N) refer to it. Read it in full before any task.

## Global Constraints

- TDD per `.claude/rules/tdd.md`: each behaviour change starts with a test shown failing for the right reason. A test that cannot go RED on at least one backend is dropped (spec §10).
- No test hooks in production code (memory `feedback_no_test_hooks_in_production`); concurrency is driven by real goroutines/connections, never injected pauses.
- Concurrency tests are isolated single-backend tests, never in the shared parity suite (`.claude/rules/test-coverage.md`).
- `log/slog` only; errors wrapped `fmt.Errorf("failed to X: %w", err)`; mutex discipline per `.claude/rules/go-mutex-discipline.md` (every Lock immediately followed by `defer Unlock`, IIFEs for early release).
- Time reads: any real-time read carries `// realtime: <reason>` (`.claude/rules/time-reads.md`).
- No `#710` or other issue ids in code, comments, help, OpenAPI or errors (memory `feedback_no_issue_ids_in_code`); commits and PR body only.
- Gate 4: a changed env var updates its `cmd/cyoda/help/content/config/*.md` topic, `README.md` (if it lists the area) and `DefaultConfig()`/plugin defaults together.
- New env var: `CYODA_POSTGRES_MODEL_CONNS`, default `8`, minimum `2`, malformed or smaller fails startup. Removed: `CYODA_SCHEMA_EXTEND_MAX_RETRIES` (in-tree; cassandra keeps its own until its issue lands).
- SPI: PR into `cyoda-go-spi` `main`, no tag; cyoda-go pins a pseudo-version of SPI `main` in root + 3 plugin `go.mod`s in ONE commit at the end (`make repin-plugins`, then tidy). During development use a local `go work edit -use <abs>/cyoda-go-spi` line that is NEVER committed: stage files explicitly, never `git add -A` / `git add .` (memory `feedback_spi_release_mechanics`).
- Verification while iterating: package-scoped `go test ./path/...`; `make test` before a stream merges; never `make test-full`/`make race` locally (CI is the full-suite gate). Never edit a worktree while its suite runs; long runs log to a file.
- Help/docs are present tense, no history (memory `feedback_spec_no_history`).

## Review Focus

1. A burst of concurrent PUTs on one model, each adding a different nested field under a parent another write adds at the same moment → every write `2xx`, every field in the exported schema (Task 12 e2e `TestSchemaExtension_ConcurrentNestedUnderNewParent`).
2. A request whose client disconnects while its extension waits for a slot → the other requests sharing that extension still succeed, and nothing stays blocked (Task 8 `TestExtendCoalesce_LeaderCancelled_LiveFollowerRetries`, `TestExtendCoalesce_FollowerLeavesOnOwnContext`).
3. A search on a field the same client added a moment earlier on the same node, while another search on that model is in flight → `200` (Task 9 `TestSearch_OwnNewField_AfterRacingSearch_Found`).
4. Startup with `CYODA_POSTGRES_MODEL_CONNS=1`, `=abc`, unset → the first two fail startup with a message naming the variable; unset gives 8 (Task 5 config tests).
5. A model unlocked while a write's callout is in flight, the write's processor returning a new field → `409 MODEL_NOT_LOCKED`, the model's schema unchanged (Task 12 e2e rows, Task 1 spitest).

---

## Streams and order

- **Stream S (SPI):** Task 1. Everything that names `spi.ErrModelNotLocked` waits for it (via the local `go.work` line).
- **Stream P (plugins), after Task 1:** Tasks 2, 3, 4, 5 — memory, sqlite and postgres are independent of each other (Tasks 4 → 5 in order).
- **Stream C (core):** Task 6 (no SPI dependency), then Tasks 7–10 (7 needs Task 1; 8, 9, 10 are independent of each other).
- **Stream E (through the stack), after P and C merge:** Tasks 11–15.

Run streams P and C in parallel worktrees; cherry-pick onto the feature branch (memory `feedback_parallelise_independent_sdd_streams`).

---

### Task 1: SPI contract, `ErrModelNotLocked`, ExtendSchema conformance suite

**Repo:** `../cyoda-go-spi` (worktree off `main`, branch `feat/extend-schema-own-tx`).

**Files:**
- Modify: `persistence.go` (`ExtendSchema` doc ~:558-579, `Get` doc), `errors.go` (new `ErrModelNotLocked`; `ErrRetryExhausted` doc ~:24-35)
- Create: `spitest/model_extend.go`; Modify: `spitest/spitest.go` (register suite), `spitest/model.go` if a shared model fixture helper belongs there
- Modify: `CHANGELOG.md`

**Interfaces:**
- Produces: `var ErrModelNotLocked = errors.New("model is not locked")` in package `spi`.
- Produces (spitest): `const toySchema...` style helpers private to spitest; the suite type-asserts `h.Factory.(interface{ SetApplyFunc(func(base []byte, delta spi.SchemaDelta) ([]byte, error)) })`. If the factory has no such setter the suite records a skip via the tracker. The toy apply function: schema bytes are `{"fields":["a","b"]}` (sorted, deduplicated), a delta is a JSON array of field names, apply = sorted union, re-marshalled with `encoding/json` (so `apply(s, nil)` re-encodes identically).

- [ ] **Step 1: Write the conformance cases (they are the failing tests; they run in the plugins, Tasks 2–5)**

In `spitest/model_extend.go`, `runModelExtendSuite(t, h, tracker)` with subtests (each on a fresh tenant, model saved with `{"fields":["a"]}` and LOCKED, toy apply wired):
- `ExtendSchema/VisibleOnReturn`: `ExtendSchema(ctx, ref, ["b"])` → nil; `Get(ctx, ref).Schema` == `{"fields":["a","b"]}`.
- `ExtendSchema/IgnoresCallerTransaction`: Begin a transaction, `ExtendSchema(txCtx, ref, ["b"])`, Rollback; `Get(context without tx)` contains `"b"`.
- `ExtendSchema/GetIgnoresCallerTransaction`: Begin T; `ExtendSchema` on a ctx WITHOUT T adds `"c"`; `Get(txCtx)` (T still open) contains `"c"`.
- `ExtendSchema/NoOpWritesNothing`: extend `["b"]` twice; then extend `["c"]`; schema == `{"fields":["a","b","c"]}` (behavioural; row counts are checked per plugin).
- `ExtendSchema/ConcurrentExtendersAllSucceed`: 16 goroutines each `ExtendSchema(["f<i>"])`; all return nil (never `errors.Is(err, spi.ErrConflict)` or `spi.ErrRetryExhausted`); final schema holds all 16.
- `ExtendSchema/NotLockedRefused`: model saved UNLOCKED; `ExtendSchema` returns an error with `errors.Is(err, spi.ErrModelNotLocked)`; schema unchanged.
- `ExtendSchema/MissingModel`: unknown ref → `errors.Is(err, spi.ErrNotFound)`.
- `Lock/NotFound` (extend the existing model suite): `Lock`, `Unlock`, `SetChangeLevel`, `IsLocked` on a missing ref → `errors.Is(err, spi.ErrNotFound)`.

Register `runModelExtendSuite` in `StoreFactoryConformance` next to the model suite.

- [ ] **Step 2: Add `ErrModelNotLocked`; rewrite the `ExtendSchema` and `Get` docs to spec §3 (six numbered properties verbatim in substance); `ErrRetryExhausted` doc stops naming schema extension.**

- [ ] **Step 3: `go build ./... && go vet ./...` in the SPI (spitest never executes in the SPI repo).** Expected: clean.

- [ ] **Step 4: CHANGELOG entry (Breaking: ExtendSchema contract; Added: ErrModelNotLocked, ExtendSchema conformance suite). Commit; push; open the SPI PR into `main` (title only per sibling-repo convention not required here — SPI is ours; body: the contract change).**

- [ ] **Step 5: In the cyoda-go feature worktree: `go work edit -use /Users/paul/go-projects/cyoda-light/cyoda-go-spi` (on the SPI branch checkout). Do not commit `go.work`.** Expected: `go build ./...` resolves `spi.ErrModelNotLocked`.

---

### Task 2: memory plugin

**Files:**
- Modify: `plugins/memory/model_store.go` (`ExtendSchema` ~:112-133; `Lock`/`Unlock`/`SetChangeLevel`/`IsLocked` ~:66-106)
- Modify: `plugins/memory/conformance_test.go` (wire nothing new: memory exposes `SetApplyFunc`? — if it uses only `WithApplyFunc`, add `func (f *StoreFactory) SetApplyFunc(fn ApplyFunc)` so the spitest suite runs)
- Modify: `plugins/memory/model_extensions_test.go` (fixtures lock the model)

**Interfaces:**
- Consumes: `spi.ErrModelNotLocked` (Task 1).
- Produces: `(*memory.StoreFactory).SetApplyFunc(fn ApplyFunc)` if not present.

- [ ] **Step 1: Run the conformance suite against memory: `cd plugins/memory && go test -run TestConformance/.*ExtendSchema -count=1 ./...`.** Expected: FAIL on `NotLockedRefused` (memory extends an UNLOCKED model) and `Lock/NotFound` (bare "not found" errors, ~:71, :82, :93, :103).
- [ ] **Step 2: `ExtendSchema` refuses a model whose `State != spi.ModelLocked` with `fmt.Errorf("model %s is not locked: %w", ref, spi.ErrModelNotLocked)`, under `modelMu`, before applying. `Lock`, `Unlock`, `SetChangeLevel`, `IsLocked` wrap `spi.ErrNotFound`.** No no-op check (memory keeps no log, spec §4.3).
- [ ] **Step 3: Fix existing memory tests that extend an UNLOCKED model: lock it first.** Run `go test ./...` in `plugins/memory`. Expected: PASS.
- [ ] **Step 4: Commit** (`plugins/memory/...` files only).

---

### Task 3: sqlite plugin

**Files:**
- Modify: `plugins/sqlite/model_store.go` (`ExtendSchema` ~:444-571, `Get` ~:99-121, `Lock` ~:200-296), `plugins/sqlite/model_extensions.go` (~:23-127), `plugins/sqlite/config.go` (~:21, :33), `plugins/sqlite/plugin.go` (~:23-24), `plugins/sqlite/store_factory.go` (~:479-486 test factory)
- Create: `plugins/sqlite/migrations/000014_drop_schema_ext_tx_id.up.sql` / `.down.sql`
- Test: `plugins/sqlite/model_extensions_internal_test.go`, `plugins/sqlite/model_extension_lifecycle_test.go`, `plugins/sqlite/conformance_test.go`

**Interfaces:**
- Consumes: `spi.ErrModelNotLocked`.
- Produces: none for other tasks.

- [ ] **Step 1: Write failing plugin tests**
  - `TestSQLite_ExtendSchema_NoOpWritesNoRow`: extend a delta twice → `SELECT COUNT(*) FROM model_schema_extensions WHERE kind='delta'` for the ref == 1.
  - `TestSQLite_ExtendSchema_BusyRetriedUntilContextEnds`: hold an external IMMEDIATE lock on the db file for 3× the old budget's total backoff, release it → `ExtendSchema` returns nil (today: `spi.ErrRetryExhausted`); with a ctx that expires while the lock is held → returns `ctx.Err()`-wrapped error, never `ErrRetryExhausted`.
  - `TestSQLite_Lock_WritesNoExtensionRow`: Save (UNLOCKED) + Lock → zero rows in `model_schema_extensions` for the ref.
  - `TestSQLite_Get_OneReadTransaction_NoTornReadWithUnlock`: N goroutines `Get` while M cycles of extend/Unlock/Lock run; every `Get` result equals either the pre- or post-Unlock fold (never base doc with the log already cleared).
  - Run: `cd plugins/sqlite && go test -run 'TestSQLite_(ExtendSchema_NoOp|ExtendSchema_Busy|Lock_WritesNo|Get_OneRead)' ./...` → FAIL. Then the spitest `ExtendSchema/NotLockedRefused` → FAIL.
- [ ] **Step 2: Implement**
  - `ExtendSchema`: in its own write transaction (unchanged), check the model exists (`spi.ErrNotFound`) and is `LOCKED` (`spi.ErrModelNotLocked`); fold; `applyFunc(current, delta)` vs `applyFunc(current, nil)` byte-equal → commit nothing, return nil; otherwise insert the caller's delta (no `tx_id`), savepoint when this model's deltas since its last savepoint reach the interval (unchanged rule). Retry `SQLITE_BUSY` (classified `spi.ErrConflict`) with the existing backoff until `ctx` ends; delete the budget (`SchemaExtendMaxRetries`, its config field, env read, `plugin.go` declaration, test-factory default).
  - `Get`: one **deferred** read transaction (`BEGIN DEFERRED` on the read handle, or the `readDB` handle; never `BeginTx` on the `_txlock=immediate` writer, `store_factory.go` ~:104) for the doc read and the fold.
  - `Lock`: delete the savepoint write (~:244-290); Lock is the state update only.
  - Migration 000014 drops `tx_id` from `model_schema_extensions` (sqlite: table rebuild per the migrations' existing pattern, see 000012 for the column-drop precedent). Remove every `tx_id` write.
- [ ] **Step 3: Delete `TransparentRetry_*`, `RetryExhaustion_*`, `SaveOnLock` sqlite tests; lock the model in every remaining test that extends an UNLOCKED model.**
- [ ] **Step 4: `cd plugins/sqlite && go test ./...`** → PASS (including `TestConformance`).
- [ ] **Step 5: Commit.**

---

### Task 4: postgres model pool and config

**Files:**
- Modify: `plugins/postgres/config.go` (new `ModelConns int32`, `envModelConns`, default const, `defaultStoreConfig`), `plugins/postgres/store_factory.go` (create and close the model pool), `plugins/postgres/plugin.go` (variable list ~:26-30), `plugins/postgres/doc.go`
- Create: `plugins/postgres/model_pool.go`
- Test: `plugins/postgres/config_test.go`, `plugins/postgres/model_pool_test.go`

**Interfaces:**
- Produces:
  - `const defaultModelConns int32 = 8`
  - `func envModelConns(getenv func(string) string) (int32, error)` — unset → 8; parse error or `< 2` → error naming `CYODA_POSTGRES_MODEL_CONNS`.
  - `func modelPoolConfig(base *pgxpool.Config, conns int32) *pgxpool.Config` — copy of base, `MaxConns = conns`, `MinConns = 0`, `RuntimeParams["lock_timeout"] = "0"`, base `statement_timeout` / idle-in-transaction kept.
  - `type modelPool struct { pool *pgxpool.Pool; extSlots chan struct{} }` with `func (m *modelPool) acquireExtension(ctx context.Context) (release func(), err error)` — takes a slot (blocks until `ctx` ends; slots = `max(1, conns/2)`), then acquires a connection bounded by the existing acquire-timeout helper (the same one `inWriteTx` uses, `model_store.go` ~:424-443); returns a release that frees both.
  - `StoreFactory` gains `models *modelPool`; `Close` closes it.

- [ ] **Step 1: Write failing tests**
  - `TestEnvModelConns`: table `{"": 8, "2": 2, "16": 16, "1": err, "0": err, "abc": err}`; error text contains `CYODA_POSTGRES_MODEL_CONNS`.
  - `TestModelPoolConfig_LockTimeoutZero_StatementTimeoutKept`.
  - `TestModelPool_ExtensionSlots_LeaveConnectionsForReads` (testcontainers): conns=4 → 2 slots; hold 2 extension slots (each with a connection held), a third `acquireExtension` blocks; a plain `pool.Acquire` for a read still succeeds; cancelling the third's ctx returns `ctx.Err()`.
  - Run → FAIL (undefined).
- [ ] **Step 2: Implement per the Interfaces block.**
- [ ] **Step 3: Docs for the variable:** `cmd/cyoda/help/content/config/database.md` (new bullet beside `CYODA_POSTGRES_SCHEDULER_CONNS`: what it serves — model reads, extensions, model administration; extensions use at most half; default 8, at least 2), `plugins/postgres/plugin.go` list, `plugins/postgres/doc.go`. (The broader doc audit is Task 15.)
- [ ] **Step 4: `cd plugins/postgres && go test -run 'TestEnvModelConns|TestModelPool' ./...`** → PASS.
- [ ] **Step 5: Commit.**

---

### Task 5: postgres ExtendSchema, Get, Lock, fold, migration

**Files:**
- Modify: `plugins/postgres/model_store.go` (`ExtendSchema`/`extendSchemaBody` ~:400-568, `Get` ~:90-112, `Lock` ~:185-232, `Unlock`/`Save`/`Delete`/`SetChangeLevel` to the model pool, `inWriteTx` ~:419-467), `plugins/postgres/model_extensions.go` (fold ~:47-100, `lastSavepointSeq` ~:17-32)
- Create: `plugins/postgres/migrations/000018_drop_schema_ext_tx_id.up.sql` / `.down.sql`
- Test: `plugins/postgres/model_extensions_internal_test.go`, `model_extensions_concurrency_internal_test.go`, `model_extensions_test.go`, `model_extension_lifecycle_test.go`, `classified_querier_internal_test.go` (~:45)

**Interfaces:**
- Consumes: Task 4 `modelPool.acquireExtension`, `StoreFactory.models`; `spi.ErrModelNotLocked`.
- Produces: none for other tasks.

- [ ] **Step 1: Write failing tests**
  - `TestExtendSchema_InEntityTx_NeverConflicts`: two REPEATABLE READ entity transactions on the main pool each call `ExtendSchema(txCtx, ref, d_i)` concurrently while both stay open; both return nil; both deltas in `Get` (today one gets `ErrConflict`).
  - `TestExtendSchema_SurvivesCallerRollback`: replaces `model_extensions_test.go` ~:298-339 (which asserts the opposite).
  - `TestExtendSchema_NoOpWritesNoRow`: same delta twice → one delta row.
  - `TestExtendSchema_NilApplyFuncRefused`: factory without applyFunc → error (today tolerated, ~:516-521).
  - `TestExtendSchema_NotLocked_ErrModelNotLocked`.
  - `TestExtendSchema_SavepointUnderLock_KeepsEveryLowerSeqDelta`: interval 2, 24 concurrent extenders (distinct fields); every field in the final fold (replaces the overlapping-RR `ErrConflict` test ~:34).
  - `TestExtendSchema_IntervalCountsModelDeltas`: interleave 10 extensions of model B between extensions of model A (interval 4); model A gets a savepoint after its own 4th delta, not earlier.
  - `TestLock_WritesNoExtensionRow` (replaces `TestExtendSchema_SaveOnLock` ~:430).
  - `TestGet_OneStatement_NoTornReadWithUnlock`: as the sqlite twin.
  - `TestGet_IgnoresCallerTransaction`: REPEATABLE READ tx T reads the model; another ctx extends; `Get(txCtx)` sees the extension.
  - `TestExtendSchema_NestedUnderParentAddedAfterSnapshot`: tx T begins; another extension adds object `a{x}`; T extends `add_property "a" y` via `ExtendSchema(txCtx)` → nil, fold has `a.x` and `a.y`.
  - Run: `cd plugins/postgres && go test -run '<names>' ./...` → FAIL for the stated reasons.
- [ ] **Step 2: Implement**
  - `ExtendSchema(ctx, ref, delta)`: `release, err := s.models.acquireExtension(ctx)`; on the acquired connection: `BEGIN` (READ COMMITTED), `set_config('app.current_tenant', …)`, `SELECT doc->>'state' FROM models WHERE … FOR NO KEY UPDATE` (no row → `spi.ErrNotFound`; state ≠ LOCKED → `spi.ErrModelNotLocked`), then as separate statements: the one-statement fold (below), `applyFunc(current, delta)` vs `applyFunc(current, nil)` → equal: `COMMIT`, return nil; else `INSERT … kind='delta'` (no `tx_id`), savepoint when `COUNT(*) of this model's delta rows with seq > its latest savepoint seq` ≥ interval, `COMMIT`. `applyFunc == nil` → error before any statement. Never uses the ambient transaction.
  - Delete `extendSchemaBody`'s `UPDATE models SET doc = doc` claim and its comment, and the ambient-transaction branch of `inWriteTx` for extension.
  - Fold: one SQL statement returning the base `doc` schema, the latest savepoint (seq, payload) and the later deltas in `seq` order (e.g. a CTE with `UNION ALL` and a kind discriminator); `Get` uses it on the model pool, ignoring any transaction on `ctx`.
  - `Lock`: the state update only, on the model pool (delete the fold and savepoint write ~:194-231). `Unlock`, `Save`, `Delete`, `SetChangeLevel`, `IsLocked`, `GetAll`: on the model pool, each its own transaction or single statement.
  - Migration 000018: `ALTER TABLE model_schema_extensions DROP COLUMN tx_id;` (down re-adds `TEXT NOT NULL DEFAULT ''`).
  - `classified_querier_internal_test.go` ~:45: use another sample statement; fix its `ExtendSchema` doc comment.
- [ ] **Step 3: Lock the model in every existing postgres test that extends an UNLOCKED model (fixture ~:234 saves `spi.ModelUnlocked`); delete the tests listed in spec §9 that pin the old behaviour.**
- [ ] **Step 4: `cd plugins/postgres && go test ./... > /tmp/<scratch>/pg-plugin.log 2>&1`** → PASS, including `TestConformance` (spitest `ExtendSchema/*`).
- [ ] **Step 5: Commit.**

---

### Task 6: `schema.Diff` emits ops in a fixed order

**Files:**
- Modify: `internal/domain/model/schema/diff.go` (`diffObject` ~:141)
- Test: `internal/domain/model/schema/diff_test.go`

**Interfaces:**
- Produces: `schema.Diff(old, new)` bytes are a function of `(old, new)` only.

- [ ] **Step 1: Write the failing test** `TestDiff_SameFieldsSameBytes`: old = `{a}`; new = `{a, b, c, d, e}` built twice by `Extend` from two documents whose keys are inserted in different orders; `Diff` bytes equal; repeat 50 times (map order varies) → today FAIL.
- [ ] **Step 2: `diffObject` iterates child names sorted (`slices.Sorted(maps.Keys(...))`).**
- [ ] **Step 3: `go test ./internal/domain/model/schema/...`** → PASS (property suites unchanged).
- [ ] **Step 4: Commit.**

---

### Task 7: `common.Internal` maps `ErrModelNotLocked`

**Files:**
- Modify: `internal/common/errors.go` (`Internal` ~:266-300)
- Test: `internal/common/errors_test.go`

**Interfaces:**
- Consumes: `spi.ErrModelNotLocked`.
- Produces: `common.Internal(msg, err)` with `errors.Is(err, spi.ErrModelNotLocked)` → `Operational(409, ErrCodeModelNotLocked, …)`, checked after `StorageUnavailable`/`EpochMismatch` and before the generic case; detail kept, not answered.

- [ ] **Step 1: Failing test** `TestInternal_ModelNotLocked_Is409`: `common.Internal("x", fmt.Errorf("%w: %w", ingest.ErrInternalSchema, spi.ErrModelNotLocked))` → Status 409, Code `MODEL_NOT_LOCKED`, not retryable.
- [ ] **Step 2: Implement.**
- [ ] **Step 3: `go test ./internal/common/...`** → PASS. Commit.

---

### Task 8: model cache — generation, fill guard, flights, extension coalescing

**Files:**
- Modify: `internal/cluster/modelcache/cache.go`, `internal/cluster/modelcache/factory.go`
- Delete: `internal/cluster/modelcache/local_subscriber_test.go`; trim tests that drive `CachingModelStore`'s store methods
- Test: `internal/cluster/modelcache/generation_test.go`, `internal/cluster/modelcache/extend_coalesce_test.go`

**Interfaces:**
- Produces (package `modelcache`):
  - `CachingModelStore` holds per-key `generation uint64` that survives `evict`; `func (c *CachingModelStore) bump(key cacheKey) uint64` (local invalidation and `handleInvalidation` both bump); `func (c *CachingModelStore) storeIfGeneration(key cacheKey, gen uint64, desc *spi.ModelDescriptor) bool`.
  - Miss flights keyed `flightKey(key, gen)`; a caller waits with `DoChan` + `select` on its own ctx; if the flight's result is an error and the leader's ctx is done while the caller's is live, the caller reads again (helper `func (c *CachingModelStore) readThrough(ctx, key, inner spi.ModelStore) (*spi.ModelDescriptor, error)`).
  - `requestScopedStore.RefreshAndGet`: reads storage through the same `(key, gen)` flight, skipping the cached entry; stores under the fill guard; no evict, no bump.
  - `requestScopedStore.ExtendSchema(ctx, ref, delta)`: coalesced in a separate single-flight group keyed `(tenant, ref, string(delta))`; inside the shared call, after `inner.ExtendSchema` returns (any outcome): `evict` + `bump` + broadcast `model.invalidate`. Callers use `DoChan` + own-ctx `select`; retry rule as for reads (leader ctx done, own live → new call).
  - Deleted: `CachingModelStore`'s `Get`/`RefreshAndGet`/`SchemaNode`/`ExtendSchema`/pass-throughs, its `inner` field, `New`'s `inner` parameter (new signature `New(broadcaster spi.ClusterBroadcaster, clk Clock, lease time.Duration) *CachingModelStore`), `var _ spi.ModelStore = (*CachingModelStore)(nil)`, `SubscribeLocal`/`notifyLocalSubscribers`/`localSubs`, `CachingStoreFactory.SubscribeLocal`.

- [ ] **Step 1: Write failing tests** (fake inner store with controllable latency and a counter; fake broadcaster recording payloads):
  - `TestFillGuard_DropsFillThatStraddlesInvalidation`: a miss read blocks; an invalidation (local bump) happens; the read returns → not stored (next `Get` reads inner again). Same with a broadcast invalidation, and after `evict`.
  - `TestMissFlight_PerGeneration`: reader A in flight at gen g; bump; reader B starts → B does not join A (inner called twice).
  - `TestRefresh_DoesNotEvict`: cached entry present; `RefreshAndGet` → inner read once, entry still served to a concurrent `Get` without a second inner read; generation unchanged.
  - `TestExtend_EveryOutcomeEvictsBumpsBroadcasts`: inner returns nil / error / (no-op nil) → each time: entry evicted, generation +1, one broadcast.
  - `TestExtendCoalesce_IdenticalDeltasOneCall`: 20 concurrent identical `ExtendSchema` → inner called once.
  - `TestExtendCoalesce_FollowerLeavesOnOwnContext`: leader's inner call blocks; follower's ctx cancelled → follower returns its own `ctx.Err()` promptly; leader unaffected.
  - `TestExtendCoalesce_LeaderCancelled_LiveFollowerRetries`: leader ctx cancelled mid-call, inner returns a wrapped driver error; follower (live ctx) → starts a new call; inner called twice; follower gets nil.
  - Run `go test ./internal/cluster/modelcache/...` → FAIL.
- [ ] **Step 2: Implement per Interfaces. The broadcast/evict/bump runs inside the shared call's function body (not in callers).**
- [ ] **Step 3: Update `app/app.go` (`NewCachingStoreFactory` call unchanged in shape; `New` call sites) and every test helper using `modelcache.New(inner, …)`.**
- [ ] **Step 4: `go test ./internal/cluster/modelcache/... ./app/...`** → PASS. Commit.

---

### Task 9: search — delete the negative path cache, one refresh helper

**Files:**
- Delete: `internal/domain/search/path_validation_cache.go` and its tests, `search/export_test.go` exports of it
- Modify: `internal/domain/search/service.go` (~:339-342 `pathCache`, ~:497-504 `WithPathValidationCache`, ~:1883-1993 `validateConditionPaths`, ~:2020-2080, ~:2097-2180 sort-key path), `internal/domain/search/path_validate.go` (`ValidateKnownPaths`, `refreshFieldsMap` ~:234-266), `app/app.go` (~:452-453), `internal/common/model_registration.go` (~:16-52), `internal/domain/model/service.go` (`getModelFresh` ~:115-131), `internal/domain/entity/handler.go` (delete `ValidateWithRefresh` ~:162-202 and its tests)
- Test: `internal/domain/search/path_refresh_test.go`

**Interfaces:**
- Produces: `func refreshOnUnknownPaths(ctx context.Context, store spi.ModelStore, ref spi.ModelRef, paths []string, check func(fields FieldsMap) []string) (FieldsMap, error)` in `internal/domain/search` — one refresh (via `RefreshAndGet` when the store has it) when `check` reports unknown paths; returns the fields map to use; all three search sites and `refreshFieldsMap` callers use it. `getModelFresh` stays as the model service's thin `RefreshAndGet` call (no behaviour change) with its comment rewritten.

- [ ] **Step 1: Failing test** `TestSearch_OwnNewField_AfterRacingSearch_Found`: model cache over a fake store; search S1 on path `p` starts its refresh and blocks after reading storage (field absent); the extension adds `p` and invalidates; S1 completes (refused); search S2 on `p` → found (today: the negative cache answers `400`).
- [ ] **Step 2: Delete the negative cache and its wiring; consolidate the refresh into the helper; delete `EnsureModelRegistered`'s refresh (keep its plain `Get` + not-found answer) and `ValidateWithRefresh`; rewrite the two comments (spec §12).**
- [ ] **Step 3: `go test ./internal/domain/search/... ./internal/common/... ./internal/domain/model/... ./internal/domain/entity/... ./app/...`** → PASS. Commit.

---

### Task 10: ingest and processor output

**Files:**
- Modify: `internal/domain/model/ingest/validate.go` (`ValidateOrExtend` ~:77-165, comment ~:161-162), `internal/domain/workflow/processor_output.go` (~:58-119), `internal/domain/entity/service.go` (callers ~:257, :1971, :2238, :2534 adopt the new return), `internal/domain/entity/handler.go` (`validateOrExtend` comment)
- Delete: `internal/domain/entity/handler_extend_conflict_test.go`
- Test: `internal/domain/workflow/processor_output_test.go`, `internal/domain/model/ingest/validate_test.go`

**Interfaces:**
- Produces: `func ValidateOrExtend(ctx context.Context, modelStore spi.ModelStore, desc *spi.ModelDescriptor, parsedData any) (*spi.ModelDescriptor, error)` — returns `desc` unchanged on a nil delta, or a copy whose `Schema` is the extended schema after a successful `ExtendSchema`.

- [ ] **Step 1: Failing tests**
  - `TestValidateOrExtend_ReturnsExtendedDescriptor`.
  - `TestProcessorOutput_SecondProcessorSameField_NoSecondExtend`: two processors in one pipeline return the same new field → `ExtendSchema` called once (counting store double).
  - `TestProcessorOutput_StoreUnavailable_Is503`: `ExtendSchema` returns a storage-unavailable error → the engine error classifies to `503` (today `500`; cause dropped by `%s` at ~:70).
  - `TestProcessorOutput_ModelNotLocked_Is409`.
  - `TestEngine_CriterionOnFieldAddedBySameRequest` (`internal/domain/workflow`): over the real model cache and an in-memory store, a processor returns a new field `p`; the next automated transition's criterion on `p` evaluates against the extended schema and matches (spec §10 "engine unit").
- [ ] **Step 2: Implement; the `ErrProcessorOutputInfra` branches (~:60, ~:70, ~:112) use `%w`; the `ErrProcessorOutputRejected` branch (~:72) keeps `%s`; the memo adopts the returned descriptor; rewrite the memo comment (~:86-91) and the `validate.go` comment.**
- [ ] **Step 3: `go test ./internal/domain/...`** → PASS. Commit.

---

### Task 11: merge streams, `make test`

- [ ] **Step 1: Cherry-pick streams P and C onto the feature branch in task order; resolve.**
- [ ] **Step 2: `make preflight && make test > <scratch>/make-test.log 2>&1`** → PASS (report the testreport summary line).
- [ ] **Step 3: `go vet ./...`** → clean. Commit any merge fixes.

---

### Task 12: e2e and gRPC

**Files:**
- Modify: `internal/e2e/model_schema_extensions_test.go` (~:24-82), `internal/e2e/workflow_proc_output_test.go` (~:135)
- Create: `internal/e2e/schema_extension_doors_test.go`, `internal/e2e/schema_extension_not_locked_test.go`
- Reuse: `internal/e2e/callback_harness_test.go` (programmable compute node)
- gRPC: `internal/grpc/` test file beside the existing entity-door tests

**Interfaces:** consumes everything above.

- [ ] **Step 1: Write the tests (run each on the unfixed base first where the spec asks RED; record which went RED in the commit message):**
  - `TestSchemaExtension_ConcurrentEveryDoor_No409`: extend the existing creates-only test to PUT, collection update, collection create and processor output (manual transition whose processor returns a new field); each door N=8 concurrent writes adding distinct fields → all `2xx`, every field in the export. RED today on PUT/collection update/processor output.
  - `TestSchemaExtension_ConcurrentNestedUnderNewParent` (Review Focus 1).
  - `TestWorkflowProcOutput_ExtensionKeptWhenTransitionFails` (rewrite of ~:135: the field is in the export after the failed transition).
  - `TestSchemaExtension_CriterionOnFieldAddedInSameRequest`: a create whose processor adds `p`; an automated transition's criterion on `p` matches → entity ends in the target state.
  - `TestSchemaExtension_NotLocked_*` (Review Focus 5) via the callback harness: the compute node's processor makes an **unjoined** HTTP `PUT /model/{name}/{version}/unlock` during the callout, then (a) returns data with a new field (create door), (b) issues a joined PUT on the owner's uncommitted entity with a new field, (c) a joined collection update, (d) on a manual transition → each answers `409 MODEL_NOT_LOCKED`; the export shows the model unchanged.
  - `TestSearch_OwnNewField_SameNode_After2xx`: PUT adding `p` returns `2xx`; search on `p` → `200`.
  - gRPC: `TestGRPC_ConcurrentExtension_SuccessEnvelope` (concurrent entity updates over gRPC each adding a field → `Success=true`), `TestGRPC_ModelNotLocked_Envelope` (setup as (a) → `Error.Code == "MODEL_NOT_LOCKED"`).
- [ ] **Step 2: `go test -timeout 30m -run 'TestSchemaExtension|TestWorkflowProcOutput_Extension|TestSearch_OwnNewField' ./internal/e2e/... > <scratch>/e2e.log 2>&1`** and the gRPC package tests → PASS.
- [ ] **Step 3: Commit.**

---

### Task 13: parity

**Files:**
- Create: `e2e/parity/schema_extension_contract.go`; Modify: `e2e/parity/registry.go` (register; remove `schema_save_on_lock` ~:293)
- Delete: `e2e/parity/schema_save_on_lock.go`
- Modify: `e2e/parity/postgres/multinode_test.go` (or a new file beside it)

- [ ] **Step 1: Scenarios (all backends):** `SchemaExtension_KeptAfterFailedWrite` (a create whose workflow fails after a processor added `p` → export has `p`); `SchemaExtension_CriterionSameRequest`; `SchemaExtension_NotLocked` (callback compute node as Task 12 (a) → `409 MODEL_NOT_LOCKED`; the parity fixture must wire the compute client); multi-node (postgres harness): PUT on node A adds `p`; poll export on node B until `p` appears within 5s (broadcast) → pass.
- [ ] **Step 2: `make test` (parity runs uncached in its own invocation)** → PASS. Commit.

---

### Task 14: throughput benchmark

**Files:**
- Create: `internal/e2e/schema_extension_bench_test.go` (build tag or `-run ^$ -bench` usage; postgres via the e2e TestMain)

- [ ] **Step 1: `BenchmarkConcurrentPUT_OneModel/{fits,distinctNewField,sameNewField}`** — 32 goroutines, each iteration a PUT on its own entity of one model: (a) data fits the schema, (b) each adds a distinct new field, (c) all add the same new field per round.
- [ ] **Step 2: Run on the merge-base (cherry-pick only the benchmark file onto a scratch worktree at `release/v0.9.0`) and on the branch, `-benchtime=10s -count=5`, `benchstat` the two; save both outputs in scratch.** Expected: (a) not slower (benchstat shows no significant regression); (b), (c) faster and without errors (the base answers `409` in (b)/(c); record the error rate).
- [ ] **Step 3: Commit the benchmark; the PR body reports the numbers.**

---

### Task 15: documentation audit, changelog, compatibility, cross-repo

**Files:** every site in spec §12 (read each document in full), plus `CHANGELOG.md`, `COMPATIBILITY.md`, `docs/cloud-parity/710-schema-extensions.md` (create), `api/openapi.yaml` then `go generate ./api`.

- [ ] **Step 1: Run the spec §12 grep; for each hit, read the whole document and make it state §3's contract, §5's side effects and §4's mechanics at its level (present tense). Include every site the spec names (models.md incl. the ERRORS list gaining `MODEL_NOT_LOCKED` and the `changeLevel` operation text; config/schema.md; config/database.md; config.md ~:88; workflows.md ~:149, ~:174; cluster.md; quickstart.md; errors/*.md listed; CONSISTENCY.md §3a; ARCHITECTURE.md sites; PROCESSOR_EXECUTION_MODES.md; CONCURRENCY.md; FEATURES/PRD/plugins docs; cloud-parity docs; audits catalog; code comments listed).**
- [ ] **Step 2: `MODEL_NOT_LOCKED.md` gains the extension case; `TestErrCode_Parity` stays green (no new error code is added).**
- [ ] **Step 3: `CHANGELOG.md` `### Breaking` (ExtendSchema contract: own transaction, survives a failed write; `CYODA_SCHEMA_EXTEND_MAX_RETRIES` removed; negative path cache removed) and `### Added` (`CYODA_POSTGRES_MODEL_CONNS`); `COMPATIBILITY.md` SPI pin row; `docs/cloud-parity/710-schema-extensions.md` (contract + side effects, a few lines).**
- [ ] **Step 4: Exit checks:** spec §9 grep → empty; spec §12 grep re-run → no sentence contradicting §3/§5 (list each site and the change in the PR body); `go test ./cmd/cyoda/...` (help tests) → PASS.
- [ ] **Step 5: Pin commit:** after the SPI PR merges into SPI `main`, `make repin-plugins` (root + 3 plugins to the SPI `main` pseudo-version), tidy, `make check-spi-pin-sync`; drop the local `go.work` line from the working tree diff (never committed). One commit.
- [ ] **Step 6: File the `cyoda-go-cassandra` issue (title + body per spec §11: contract, retry until ctx ends and drop its `CYODA_SCHEMA_EXTEND_MAX_RETRIES` budget at `internal/config/config.go` ~:242, no-op check, `ErrModelNotLocked`, `Get` ignores the transaction, the spitest suite).**
- [ ] **Step 7: Commit.**

---

## Finish (outside the task list)

Whole-branch final review (fresh reviewer), then `cyoda-go-security-audit` on the exact PR head, PR into `release/v0.9.0`, CI green on the PR head (memory `feedback_security_review_gate`, `feedback_release_branch_workflow`).
