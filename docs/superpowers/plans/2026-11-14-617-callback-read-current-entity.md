# #617 Callback Read Returns the Current Entity — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A callback under the transaction token reads the anchor as the workflow currently has it. Storage records one version per entity per transaction, enforces unique keys at commit from its own stored model, and never lets an entity write and a change of its model both succeed.

**Architecture:** The work goes bottom-up, in four phases. Phase B changes storage to one version per entity per transaction (SPI conformance first, then memory, sqlite and postgres). Phase C makes the model-store checks atomic (§4.5; postgres gets a `model_lifecycle` table). Phase D moves the unique-key claims and the model-state check to commit, reading keys from the stored model, and deletes `spi.WithUniqueKeys`. Phase E adds the engine's save before each callout that carries the token (§2), the payload `transactionId` rule, and the reordered end of the scheduled fire. Phases A (fixture) and F (docs, SPI PR, pins) bracket them.

**Tech Stack:** Go 1.27, PostgreSQL (pgx v5, REPEATABLE READ entity transactions), SQLite, in-memory plugin, `cyoda-go-spi` + `spitest` conformance, testcontainers e2e, `cmd/compute-test-client` for callbacks.

**Spec:** `docs/superpowers/specs/2026-11-06-617-callback-read-current-entity-design.md` (cited below as "spec §N"). Read it before any task. It is the authority; this plan only orders the work and fixes names.

## Global Constraints

- TDD per `.claude/rules/tdd.md`: one failing test first, watched failing for the right reason. Each spitest case is shown RED in every in-tree plugin before that plugin's fix (spec §3.4).
- Fail closed (`.claude/rules/correctness-over-availability.md`): no fallback that commits a wrong value.
- A backend that diverges from the others on the same contract is a bug (CLAUDE.md "External Storage Plugins").
- Business time comes from `spi.Clock`. A real-time read carries `// realtime: <reason>` (`.claude/rules/time-reads.md`); `internal/platformclock/time_reads_test.go` enforces it on Go code and on postgres SQL from migration 000017 on.
- `log/slog` only. No `#NNN` issue ids in code, comments, errors, logs, OpenAPI or help text.
- Every `Lock()`/`RLock()` is followed on the next line by `defer Unlock()`/`defer RUnlock()`; use an IIFE for early release (`.claude/rules/go-mutex-discipline.md`).
- No test hooks in production code. A non-functional property is reviewed, not unit-tested through an injected counter.
- No new error codes and no new HTTP status codes (spec §5, last line). New SPI sentinels: `spi.ErrModelLocked`, `spi.ErrModelHasEntities`, `*spi.ModelHasEntitiesError`. `spi.ErrModelNotLocked` already exists and is reused.
- SPI work is on a branch of `/Users/paul/go-projects/cyoda-light/cyoda-go-spi` and goes in as a PR into SPI `main`. cyoda-go uses it through a local `go.work` `use` line that is **never committed**: stage files explicitly and never run `git add -A` or `git add .`. The SPI is not tagged; Task 26 pseudo-pins `main`.
- Verification: `make test` (needs Docker; `make preflight` must pass), the touched packages' tests, and `go vet ./...` in the root and in each touched plugin. Never `-count=1` (except when hunting a flake), never a hand-rolled `go test ./...` as a verdict, never `make test-full` or `make race` locally (CI runs them). Use gofmt from `$(go env GOROOT)/bin/gofmt`.
- Line numbers in this plan and the spec are approximate (`~:N`). Find the code by its name.

## Review Focus

1. **A transaction that writes entities of several models, one of them not `LOCKED` at commit.** The whole commit is refused, nothing of any model is committed, and the backends agree. Test: Task 12 `Unique/CommitRefusesIfAnyModelNotLocked`.
2. **A unique-key value that JSONB re-renders, such as the number `1e100`, written inside a postgres transaction.** It commits, as it does on memory and sqlite. Test: Task 15 `TestCommitClaims_UsesWrittenBytes`.
3. **A savepoint rollback that undoes a re-key.** Save A with key `x`, savepoint, save A with `y`, roll back to the savepoint, commit: A holds `x`, and `y` is free. Test: spitest `Unique/SavepointRollbackRestoresClaim` (Task 12).
4. **A callback that deletes the anchor, followed by a later callout in the same cascade.** The engine must not save the anchor back. Test: Task 19 `TestPreCalloutSave_AnchorDeletedByCallback_NotResurrected`.
5. **A model with no unique keys.** A commit still checks that the model is `LOCKED`, and it writes no claims. Test: spitest `Unique/KeylessModelCommitsNoClaims` (Task 12).

---

## Execution streams

- **A → B → C → D → E → F**, in that order. B and C are independent of each other and may run in parallel after A. D needs both. E needs D, because the pre-callout saves rely on claims being computed at commit on postgres (spec §4 intro).
- Inside each of B, C and D, the memory, sqlite and postgres tasks touch disjoint files. Run them in parallel worktrees once that phase's spitest task has landed.

---

## Phase A — Setup and the shared fixture

### Task 1: Branches, local SPI composition and the shared locked-model fixture

**Files:**
- SPI: create branch `feat/617-one-version-model-checks` in `/Users/paul/go-projects/cyoda-light/cyoda-go-spi` off `origin/main`.
- cyoda-go worktree: `go.work` gets an uncommitted `use /Users/paul/go-projects/cyoda-light/cyoda-go-spi` line.
- Create: `cyoda-go-spi/spitest/fixture_model.go`
- Modify: `cyoda-go-spi/spitest/helpers.go` and every spitest suite that saves entities (entity, transaction, search, searcher, audit, async search, iterable, scheduled tasks, consistency, workflow — whichever saves entities).

**Interfaces:**
- Produces: `func EnsureLockedModel(t testing.TB, f spi.StoreFactory, ctx context.Context, ref spi.ModelRef, keys ...spi.UniqueKey)`. It is exported, so plugins and root-module tests use it. If the model is absent, it `Save`s it `UNLOCKED` with `keys` and a minimal schema (as `newModelDescriptor` in `spitest/model.go` builds), then calls `Lock`. If the model is already present and `LOCKED`, it returns, and the test fails if `keys` differ from the stored ones. It is idempotent per ref, and every failure is `require`d.

- [ ] **Step 1:** Create the SPI branch and add the `go.work` `use` line. Then run `go build ./... && (cd plugins/memory && go build ./...)` and expect it to compile.
- [ ] **Step 2:** Write `EnsureLockedModel` and call it in every spitest test that saves an entity, once per model the test uses, before the first save. `newEntity` keeps its signature.
- [ ] **Step 3:** Run `cd plugins/memory && go test ./... -run TestConformance`, then the same in `plugins/sqlite` and `plugins/postgres`. Expect all PASS: there is no behaviour change yet, so the fixture is proven harmless.
- [ ] **Step 4:** Commit the SPI change: `test(spitest): EnsureLockedModel fixture; suites create the models they write`.

---

## Phase B — One version per entity per transaction (spec §3)

### Task 2: spitest one-version cases and SPI `EntityStore` doc

**Files:**
- Create: `cyoda-go-spi/spitest/entity_one_version.go` (registered from `runEntitySuite`)
- Modify: `cyoda-go-spi/spitest/entity.go`: delete `testEntityGetVersionByTransactionEarliestWins` (~:1648) and its registration
- Modify: `cyoda-go-spi/persistence.go`: the `EntityStore` doc for `Save`/`CompareAndSave`/`Delete`/`SaveAll`/`DeleteAll`, the §3.1 rule, and `GetVersionByTransaction` §3.2
- Modify: `cyoda-go-spi/CHANGELOG.md`

**Interfaces:**
- Produces: subtests under `TestConformance/Entity/OneVersionPerTx/…` (names below), used as RED/GREEN gates by Tasks 3–5.

- [ ] **Step 1: Write the cases.** Each runs in one transaction and commits, then checks `GetVersionMetadata` (count and change type) and `Get` (content). Use the §3.1 table as the checklist:
  - `SaveSave` → 1 version, `CREATED`, content of the last save.
  - `SaveCompareAndSave` (CAS names the transaction's own id) → 1 version, the CAS's content.
  - `DeleteThenSave_Existed` → 1 version this transaction, `UPDATED`.
  - `SaveThenDelete_Existed` → 1 version this transaction, `DELETED`.
  - `CreateThenDelete` → 1 version, `DELETED` (tombstone).
  - `CreateThenSave` → 1 version, `CREATED`.
  - `SaveSaveDeleteSave` → 1 version, `UPDATED` when the entity existed before, with the last save's content.
  - `VersionNumberIsPreviousPlusOne`: entity at version n, then a transaction that saves twice → version n+1.
  - `AttributionIsLastWrite`: two saves under different `tenantContextAs` users → the version carries the second user.
  - `SavepointRestoresReplacedPending`: save v1, savepoint, save v2, roll back to the savepoint, commit → 1 version with v1.
  - `SavepointRestoresNoPending`: savepoint, save, roll back to the savepoint, commit → no version from this transaction.
  - `GetVersionByTransaction/LastSaveWins` (replaces EarliestWins): two saves → returns the second payload.
  - `GetVersionByTransaction/InsideWritingTxIsNotFound`: inside txID after a save, `GetVersionByTransaction(txCtx, id, txID)` → `spi.ErrNotFound`.
  - `SaveAllDeleteAll`: `SaveAll` of [a, b], then `SaveAll` of [a'] → a has 1 version (a'), b has 1. Then `DeleteAll` in a fresh transaction after `SaveAll` in it → 1 `DELETED` version per entity.
- [ ] **Step 2:** Run `go test ./... -run 'TestConformance/Entity/OneVersionPerTx'` in each of the three plugins. Expect FAIL in each, at least on: `SaveSave` (memory and sqlite give 2 versions, from `supersededSaves`), `GetVersionByTransaction/InsideWritingTxIsNotFound` (postgres returns the uncommitted save), and `DeleteThenSave` / `SaveThenDelete` on postgres. Record which cases fail on which backend in the commit message: that is the RED proof the spec requires.
- [ ] **Step 3:** Update the SPI doc comments and the SPI CHANGELOG (`### Breaking`: one version per entity per transaction; `GetVersionByTransaction` reads committed versions only).
- [ ] **Step 4:** Commit to the SPI branch: `feat(spitest)!: one version per entity per transaction`.

### Task 3: memory — one version per transaction

**Files:**
- Modify: `plugins/memory/txmanager.go` (`supersededSaves`, `supersededLens`, the savepoint length record, cleanup ~:88, ~:144-174, ~:239, ~:297-308, and the commit flush)
- Modify: `plugins/memory/store_factory.go` (`txIndex` ~:89-107), `plugins/memory/entity_store.go` (`GetVersionByTransaction` ~:1168), `plugins/memory/sm_audit_store.go` ~:269 (comment)
- Test: `plugins/memory/*_test.go` that pin multi-version-per-transaction behaviour

- [ ] **Step 1:** Run `cd plugins/memory && go test ./... -run 'TestConformance/Entity/OneVersionPerTx'` and confirm the RED from Task 2.
- [ ] **Step 2:** Delete `supersededSaves` with its length record and cleanup. Commit writes one version per buffered entity (and one tombstone per `tx.Deletes` entry, as today). `txIndex` records that single version per (entity, tx).
- [ ] **Step 3:** Run the same command and expect PASS. Then run `go test ./...` in `plugins/memory` and fix every test that asserted the old version counts, citing spec §8 in the commit body.
- [ ] **Step 4:** Commit: `fix(memory)!: one version per entity per transaction`.

### Task 4: sqlite — one version per transaction

**Files:**
- Modify: `plugins/sqlite/txmanager.go` (`supersededSaves` ~:170-184, ~:235, `stageSuperseded` ~:495-527, `supersededFor` ~:528, the flush loop ~:1037-1060)
- Modify: `plugins/sqlite/entity_store.go` (`GetVersionByTransaction` ~:1404 doc, ~:312 comment)
- Test: `plugins/sqlite/tx_flush_changetype_test.go` `TestFlushToSQLite_ChangeTypeSameTxDoubleSave` (expects 2 → 1)

- [ ] **Step 1:** Confirm RED with `cd plugins/sqlite && go test ./... -run 'TestConformance/Entity/OneVersionPerTx'`.
- [ ] **Step 2:** Change `TestFlushToSQLite_ChangeTypeSameTxDoubleSave` to expect 1 version (`CREATED`, last payload), and run it to watch it fail.
- [ ] **Step 3:** Delete `supersededSaves`, `stageSuperseded` and `supersededFor`. The flush writes one row per buffered entity and keeps the `deletedBufferedEntities` tombstone path for create-then-delete. This also removes the defect where `save, save, delete, save` flushed the stale first save.
- [ ] **Step 4:** Run the conformance filter and `go test ./...` in `plugins/sqlite`, and expect PASS.
- [ ] **Step 5:** Commit: `fix(sqlite)!: one version per entity per transaction`.

### Task 5: postgres — pending version replaced in place; `GetVersionByTransaction` off the transaction

**Files:**
- Modify: `plugins/postgres/entity_store.go` (`saveOn` ~:175, the `KNOWN ASYMMETRY` comment ~:194-218, `deleteOn` ~:739, `GetVersionByTransaction` ~:1192-1210)
- Test: `plugins/postgres/entity_store_one_version_test.go` (new) for SQL-specific cases

**Interfaces:**
- Produces: unchanged signatures. The behaviour is spec §3.3.

- [ ] **Step 1:** Confirm RED with `cd plugins/postgres && go test ./... -run 'TestConformance/Entity/OneVersionPerTx'`.
- [ ] **Step 2: Write `TestSaveOn_NonTransactionalAddsVersionEachTime`.** Two `Save` calls with no transaction → 2 versions. This guards the `transaction_id = ''` path, which must keep adding versions.
- [ ] **Step 3:** In `saveOn`/`deleteOn`, only when there is an ambient transaction, detect in SQL whether `entity_versions` at `entities.version` has `transaction_id` = this transaction. If so, `UPDATE` that row in place (doc, model columns, change type, deleted flag, timestamp) and `UPDATE entities` without incrementing `version`. Derive the change type from whether a version before this transaction exists, never from the upsert's `xmax`. The timestamp is the same business-clock value the insert uses today (`spi.Clock`, not `now()`). Correct the `KNOWN ASYMMETRY` comment (`saveOn` copies the entity first).
- [ ] **Step 4:** `GetVersionByTransaction` reads through the committed querier, as `GetVersionMetadata` does, never `s.q`.
- [ ] **Step 5:** Run the conformance filter, the new test, and `go test ./...` in `plugins/postgres`. Expect PASS, including the existing commit-stamp and creation-date tests (the version number is kept, so `stampCommitInstant` and the `version = 1` creation-date stamping still work).
- [ ] **Step 6:** Commit: `fix(postgres)!: one version per entity per transaction; by-transaction read is committed-only`.

### Task 6: root module — one version per transaction

**Files:**
- Modify: `internal/domain/entity/service.go` comment on `getEntityByTransactionID` (~:363-376, "earliest matching version")
- Test: `internal/domain/workflow/fire_scheduled_test.go` ~:1095-1179 (expects 4 versions → 3)
- Test: `internal/e2e` and `e2e/parity`: find every count of `/changes` entries after a joined-callback write or a `COMMIT_BEFORE_DISPATCH` run (`grep -rn "changes" internal/e2e e2e/parity | grep -i "len\|count"`)
- Create: parity scenario `OneVersionPerTransaction` in `e2e/parity/history_reads.go`, registered in `registry.go`
- Create: e2e `internal/e2e/by_transaction_read_test.go`

- [ ] **Step 1: Write the parity scenario.** A workflow whose `SYNC` processor (`cb-write-self` from the compute test client) writes the anchor through the callback → `/entity/{id}/changes` shows 1 entry for that transaction. Then `GET /entity/{id}?transactionId=X` returns the final entity.
- [ ] **Step 2: Write the e2e tests.**
  - `TestByTransaction_FinalEntityAfterCommit`
  - `TestByTransaction_InsideWritingTx_404` (a callback under X's token asks for `?transactionId=X` → `404 ENTITY_NOT_FOUND`)
  - `TestByTransaction_TxDeletedEntity_404`
- [ ] **Step 3:** Run the new parity scenario and e2e tests, and the scheduled-fire test, against the Task 3–5 plugins. Expect PASS. Before that, confirm the parity scenario and the inside-tx e2e test FAIL on the pre-Task-3 commit (check out that commit in a scratch worktree). A test that cannot go RED is dropped.
- [ ] **Step 4:** Commit: `test(e2e,parity): one version per transaction; by-transaction read is committed-only`.

---

## Phase C — A model cannot change while entities are being written to it (spec §4.5)

### Task 7: SPI sentinels and spitest model-refusal cases

**Files:**
- Modify: `cyoda-go-spi/errors.go` (new sentinels; widen `ErrModelNotLocked`'s doc to cover an entity commit and a non-transactional save)
- Modify: `cyoda-go-spi/persistence.go` (`ModelStore` doc: `Unlock` refuses live entities atomically; `Delete` and `Save` refuse a `LOCKED` model; `Delete` counts no entities, because an `UNLOCKED` model has none — spec §4.5)
- Modify: `cyoda-go-spi/spitest/model.go`; `cyoda-go-spi/errors_test.go`

**Interfaces:**
- Produces:
  ```go
  var ErrModelLocked = errors.New("model is locked")
  var ErrModelHasEntities = errors.New("model has entities")
  type ModelHasEntitiesError struct { Ref ModelRef; Count int64 }
  func (e *ModelHasEntitiesError) Error() string
  func (e *ModelHasEntitiesError) Is(target error) bool // true for ErrModelHasEntities
  ```

- [ ] **Step 1:** Write `TestModelHasEntitiesError_IsSentinel` in `errors_test.go`: `errors.Is(&ModelHasEntitiesError{Count: 2}, ErrModelHasEntities)` is true, and `errors.As` recovers `Count == 2`. Run `go test ./ -run TestModelHasEntitiesError` in the SPI and expect a build failure, then add the types and expect PASS.
- [ ] **Step 2: Write the spitest cases** under `TestConformance/Model/…`:
  - `Unlock/RefusesLiveEntities`: a locked model with 2 committed entities → `errors.As` `*ModelHasEntitiesError` with `Count == 2`; the model is still `LOCKED`.
  - `Delete/RefusesLocked` → `ErrModelLocked`; the model still exists.
  - `Save/RefusesOverLocked`: `Save` of a descriptor over a stored `LOCKED` model → `ErrModelLocked`; stored keys and state unchanged.
  - `Interleave/UnlockVsUncommittedWrite`: tx1 saves an entity of M (uncommitted); `Unlock(M)` succeeds (count 0); tx1 commit → `ErrModelNotLocked`. Then the reverse order: tx1 commits first → `Unlock` → `ModelHasEntitiesError`. Never both succeed. Its commit half goes GREEN only in Phase D, so each plugin lists `Model/Interleave/UnlockVsUncommittedWrite` in its conformance `Skip` map with reason `"commit-time model check: Phase D"`, and that plugin's Task 13/14/15 removes the entry.
- [ ] **Step 3:** Run `go test ./... -run 'TestConformance/Model/(Unlock|Delete|Save)/'` in each plugin. Expect FAIL (no plugin checks today). Record the result.
- [ ] **Step 4:** Commit to the SPI branch: `feat!: ModelStore refuses changes under live entities and over a locked model`.

### Task 8: memory model store — atomic checks

**Files:**
- Modify: `plugins/memory/model_store.go` (`Save` ~:26, `Delete` ~:56, `Unlock` ~:77)
- Modify: `plugins/memory/entity_store.go` (an internal lock-free count helper beside `Count` ~:912)

**Interfaces:**
- Produces: `func (f *StoreFactory) countLiveUnlocked(tenant spi.TenantID, ref spi.ModelRef) int64`, called with `entityMu` held.

- [ ] **Step 1:** Confirm RED with the Task 7 filter.
- [ ] **Step 2:** `Unlock`, `Delete` and `Save` take `entityMu` (write), then `modelMu`, check, and change. `modelMu` is never taken around `entityMu`. Use an IIFE if a section must release early.
- [ ] **Step 3: Write `TestModelStore_SaveAfterLockRefused`** (per-plugin interleave row of spec §7): read the descriptor while `UNLOCKED`, `Lock`, then `Save` the read descriptor → `ErrModelLocked`, with keys unchanged.
- [ ] **Step 4:** Run the filter and `go test ./...` in `plugins/memory`, and expect PASS. Fix every test that re-saves over a `LOCKED` model (spec §8): it saves `UNLOCKED` then locks, or saves once.
- [ ] **Step 5:** Commit: `fix(memory)!: model unlock, delete and save check entities and state atomically`.

### Task 9: sqlite model store — atomic checks

**Files:**
- Modify: `plugins/sqlite/model_store.go` (`Save` ~:39, `Delete` ~:164, `Unlock` ~:256). Each runs its check and change in its one write transaction on `s.db`.

- [ ] **Step 1:** Confirm RED with the Task 7 filter.
- [ ] **Step 2:** Implement: count live entities with the same `sqlTx`; `Save` uses `ON CONFLICT … DO UPDATE … WHERE json_extract(doc,'$.state') <> 'LOCKED'`, or a check in the same transaction.
- [ ] **Step 3:** Write the same `TestModelStore_SaveAfterLockRefused`. Run the filter and `go test ./...` in `plugins/sqlite`, and expect PASS. Fix the tests that re-save over a locked model.
- [ ] **Step 4:** Commit: `fix(sqlite)!: model unlock, delete and save check entities and state atomically`.

### Task 10: postgres — `model_lifecycle` table and atomic model operations

**Files:**
- Create: `plugins/postgres/migrations/000019_model_lifecycle.up.sql` / `.down.sql`
- Modify: `plugins/postgres/model_store.go` (`Save` ~:53-95, `Delete` ~:135-161, `Lock` ~:166, `Unlock` ~:179-217, `updateStateField` ~:254)
- Memory and sqlite (Tasks 8, 9): `Delete` checks the state only, like postgres.
- Create: `plugins/postgres/model_lifecycle_test.go`

**Interfaces:**
- Produces: table `model_lifecycle (tenant_id, model_name, model_version, generation BIGINT NOT NULL DEFAULT 0)`, primary key `(tenant_id, model_name, model_version)`, no foreign key, RLS enabled with the same tenant policy as `models` (`000001_initial_schema.up.sql` ~:145-146), filled from `models` by the up migration. Also `func bumpLifecycle(ctx context.Context, q Querier, tenantID spi.TenantID, ref spi.ModelRef) error` (insert-or-increment) and `func deleteLifecycle(...) error`. Task 15 consumes the table.

- [ ] **Step 1:** Confirm RED with the Task 7 filter.
- [ ] **Step 2: Write the migration.** Write `TestMigration_ModelLifecycleFilledFromModels` (an existing model has a lifecycle row after migrating) and expect FAIL, then add the migration and expect PASS. SQL time functions are not used.
- [ ] **Step 3:** Implement spec §4.5's postgres list. All writes go in `inModelTx`: `models` first, lifecycle second.
  - `Lock`: conditional `UPDATE … WHERE doc->>'state' <> 'LOCKED'`. On zero rows, an existence check tells "already locked" (no bump) from `ErrNotFound`. Bump when the state changed.
  - `Unlock`: as today, then bump, then count live entities; refuse `*ModelHasEntitiesError` (rolls back).
  - `Delete`: `DELETE … RETURNING doc`; `LOCKED` → `ErrModelLocked` (roll back); delete the lifecycle row. No entity count.
  - `Save`: `INSERT … ON CONFLICT … DO UPDATE SET doc = EXCLUDED.doc WHERE models.doc->>'state' <> 'LOCKED' RETURNING 1`; no row on conflict → `ErrModelLocked`; then bump or insert the lifecycle row.
  - `SetChangeLevel` and `ExtendSchema` do not touch `model_lifecycle`.
- [ ] **Step 4: Write the postgres interleave tests** in `model_lifecycle_test.go`, two connections, deterministic order:
  - `TestSetUniqueKeysSaveAfterLockCommits_Refused`
  - `TestLockIdempotent_NoBump` (generation unchanged)
  - `TestUnlockBumpsGeneration`
  - `TestDeleteRemovesLifecycleRow`
  - `TestSetChangeLevelAndExtend_DoNotBump`
- [ ] **Step 5:** Run the filter and `go test ./...` in `plugins/postgres`, and expect PASS. Fix the tests that re-save over a locked model.
- [ ] **Step 6:** Commit: `feat(postgres)!: model_lifecycle row; model operations check entities and state atomically`.

### Task 11: model service — use the store's answers; error mapping; e2e/parity/gRPC

**Files:**
- Modify: `internal/domain/model/service.go` (`ImportModel` ~:136-229, `UnlockModel` ~:346-400, `DeleteModel` ~:412-466, `SetUniqueKeys` ~:608-674): delete the two `entityStore.Count` calls (~:384, ~:450) and `DeleteModel`'s `MODEL_HAS_ENTITIES` branch; map the store's errors with `errors.As`/`errors.Is` before the generic `common.Internal` wrap
- Test: `internal/domain/model/service_test.go` (or the existing handler test file)
- Test: parity `e2e/parity/model.go` scenario `UnlockWithEntities`; gRPC `internal/grpc/` unlock envelope test; e2e unlock is covered by `internal/e2e/model_errors_test.go` ~:45

**Interfaces:**
- Consumes: Task 7 sentinels.
- Produces: answers with unchanged codes and props. `MODEL_HAS_ENTITIES` carries `entityName`, `entityVersion`, `entityCount` (from `ModelHasEntitiesError.Count`). `MODEL_ALREADY_LOCKED` from the store carries `entityName`, `entityVersion`, `expectedState`, `actualState` for delete and `SetUniqueKeys`, and `entityName`, `entityVersion` for import (spec §4.5).

- [ ] **Step 1:** Write unit tests with a store double that answers `&spi.ModelHasEntitiesError{Count: 3}` on `Unlock` and `spi.ErrModelLocked` on `Save`/`Delete`. Expect `409 MODEL_HAS_ENTITIES` with `entityCount == 3` and `409 MODEL_ALREADY_LOCKED` with the props above. Run them and expect FAIL: today they answer `500`.
- [ ] **Step 2:** Implement the mapping and delete the `Count` calls. `common.Internal` gets no new branch.
- [ ] **Step 3:** Add the parity scenario (unlock with entities → `409 MODEL_HAS_ENTITIES`, `entityCount`) and the gRPC test (unlock with entities → envelope `Success == false`, `Error.Code == "MODEL_HAS_ENTITIES"`). Run them and expect PASS. They pass today at the service level; their value is regression cover for the moved check, so no RED is needed. Say so in the commit body. Delete of a model with entities answers `MODEL_ALREADY_LOCKED` (existing tests).
- [ ] **Step 4:** Commit: `fix(model)!: unlock, delete, import and key changes use the store's atomic answers`.

---

## Phase D — Unique keys and the model check at commit (spec §4.1–§4.4)

### Task 12: spitest — commit-time claims from the stored model

**Files:**
- Create: `cyoda-go-spi/spitest/unique.go` (`runUniqueSuite`, registered in `spitest.go` as `t.Run("Unique", …)`)
- Modify: `cyoda-go-spi/unique.go` doc (the §4.1 rule; keys read by the store from its own model)

- [ ] **Step 1: Write the cases.** Keys are declared only on the model, through `EnsureLockedModel(…, keys...)`. **No `spi.WithUniqueKeys` anywhere.**
  - `KeysFromStoredModel/Tx` and `KeysFromStoredModel/NonTx`: a duplicate → `ErrUniqueViolation`.
  - `SwapInTx`, `ChainInTx` and `FreeAfterClaimInTx` → commit.
  - `SameValueTwiceInTx` → `ErrUniqueViolation` at commit.
  - `PartialKeyAtCommit` → `ErrPartialUniqueKey`.
  - `SavepointRollbackRestoresClaim` (Review Focus 3).
  - `KeylessModelCommitsNoClaims` (Review Focus 5).
  - `CommitRefusesModelNotLocked`: the tx saves an entity of M; M is unlocked through the store while the tx is open → commit `ErrModelNotLocked`, nothing committed.
  - `CommitRefusesIfAnyModelNotLocked` (Review Focus 1): the tx writes M1 and M2, and M2 is unlocked → commit refused, neither entity stored.
  - `CommitRefusesMissingModel`: the tx saves an entity of a model that was never created → `ErrModelNotLocked`.
  - `CreateThenDeleteCommitsOnUnlockedModel`: the tx creates e in M and deletes it; M is unlocked while the tx is open (count 0); commit succeeds with only a tombstone, with no model read.
  - `NonTxSaveRefusesModelNotLocked`.
  - Lift Task 7's skipped `Interleave` case from the plugins' `Skip` maps when each plugin's Task 13–15 lands.
- [ ] **Step 2:** Run `go test ./... -run 'TestConformance/Unique'` in each plugin and expect FAIL (the keys come from context today, so no enforcement). Record the result.
- [ ] **Step 3:** Commit to the SPI branch: `feat(spitest)!: unique keys at commit from the stored model; commit refuses a model that is not LOCKED`.

### Task 13: memory — commit-time claims and model check

**Files:**
- Modify: `plugins/memory/txmanager.go` (commit ~:908-1117: model read, then release all, then insert all; delete `recordUniqueKeys`, `txUniqueKeys` and their cleanup ~:104, ~:111-116, ~:237, ~:282-292, ~:594), `plugins/memory/entity_store.go` (~:281, ~:349, ~:376; `saveUnlocked` ~:420 reads the model), `plugins/memory/store_factory.go` (`releaseClaims` ~:311-317 stays the per-entity primitive)
- Test: `plugins/memory/unique_claims_test.go` (`SameTxReclaim*` → both orders commit), plus the tests listed in spec §4.4 that use `spi.WithUniqueKeys`

- [ ] **Step 1:** Confirm RED with `go test ./... -run 'TestConformance/Unique'`.
- [ ] **Step 2:** Implement spec §4.1 and §4.2 "memory". Under `entityMu`: read every saved entity's model in (name, version) order through the model map (`modelMu` inside `entityMu`); refuse a missing or non-`LOCKED` model with `ErrModelNotLocked`; release the claims of every entity in `tx.Buffer` ∪ `tx.Deletes`; compute from the buffered bytes; insert all claims; check `pending` for duplicates inside the batch.
- [ ] **Step 3:** Rewrite `SameTxReclaim*` and migrate the listed tests to declare keys on the model.
- [ ] **Step 4:** Run `go test ./...` in `plugins/memory` and expect PASS. Then run `make test` in the root: root unit tests that use the memory factory and save entities of a model they never created and locked now fail. Migrate each to `spitest.EnsureLockedModel` (spec §8), or to the package's existing import-and-lock helper where the test goes through the service. The root still compiles because the SPI keeps `WithUniqueKeys` until Task 16.
- [ ] **Step 5:** Commit: `fix(memory)!: unique keys and model state checked at commit from the stored model`.

### Task 14: sqlite — commit-time claims and model check

**Files:**
- Modify: `plugins/sqlite/txmanager.go` (`recordUniqueKeys`, `uniqueKeysFor`, `txUniqueKeys`, the savepoint note: ~:128, ~:147-152, ~:231, ~:241-258, ~:686, ~:1140, ~:1448-1455; the flush ~:1037), `plugins/sqlite/entity_store.go` (~:325, ~:469, ~:537, ~:560), `plugins/sqlite/unique_claims.go`
- Test: `plugins/sqlite/unique_claims_test.go`, `sm_audit_tx_join_test.go`

- [ ] **Step 1:** Confirm RED with `go test ./... -run 'TestConformance/Unique'`.
- [ ] **Step 2:** In the flush, read the `models` rows with the flush's own `sqlTx`, never through the model store (one writer connection, so a call into it would wait for itself). Then release all claims and insert all. `saveDirectlyLocked` does the same in its own transaction.
- [ ] **Step 3:** Rewrite `SameTxReclaim*` and migrate the listed tests. Run `go test ./...` in `plugins/sqlite` and expect PASS.
- [ ] **Step 4:** Commit: `fix(sqlite)!: unique keys and model state checked at commit from the stored model`.

### Task 15: postgres — commit-time claims, lifecycle check, bytes record

**Files:**
- Modify: `plugins/postgres/txstate.go` (a `writeBytes map[string][]byte` beside `writeSet`, snapshotted in `savepointEntry`; nil = deleted), `plugins/postgres/entity_store.go` (`saveOn`/`deleteOn` record bytes and stop calling `replaceClaims` inside a transaction; the claim release in `deleteOn` ~:884 and the model-wide claim `DELETE` in `DeleteAll` ~:935 are removed inside a transaction; the ~:402 comment), `plugins/postgres/unique_claims.go` (`replaceClaims` reads the keys from the model, for the non-transactional path), `plugins/postgres/transaction_manager.go` (`Commit`: steps 1-4 after read-set validation, before `stampCommitInstant` ~:417-420; errors through `abortedCommitError` ~:283-288 and `classifyTxError`)
- Create: `plugins/postgres/commit_claims.go`, `plugins/postgres/commit_claims_test.go`

**Interfaces:**
- Consumes: the `model_lifecycle` table (Task 10).
- Produces: `func (s *txState) RecordWriteBytes(id string, data []byte)`, `func (s *txState) WrittenEntities() map[string][]byte`, and `func commitClaims(ctx context.Context, q Querier, tenantID spi.TenantID, written map[string][]byte, refs map[string]spi.ModelRef) error`.

- [ ] **Step 1:** Confirm RED with `go test ./... -run 'TestConformance/Unique'`.
- [ ] **Step 2:** Write the postgres-only tests in `commit_claims_test.go`:
  - `TestCommitClaims_UsesWrittenBytes`: key value `1e100` inside a transaction → commits (Review Focus 2).
  - `TestCommitClaims_ModelCreatedAfterSnapshot_Conflict`: a model created and locked after the tx's snapshot → commit `spi.ErrConflict`; the retry commits.
  - `TestCommitClaims_ModelNeverExisted_NotLocked` → `ErrModelNotLocked`; no lifecycle row is left behind.
  - `TestCommitClaims_LifecycleBumpedAfterSnapshot_Conflict` (40001 → `ErrConflict`).
  - `TestCommitClaims_ExtendAndChangeLevel_DoNotBlock`: an extension or a change-level change of M, committed during the tx or holding the `models` row at its commit, neither fails nor delays the commit. Bound the wait with a short context so that a block fails the test.
  - `TestNonTxSave_WaitsOnUncommittedUnlock_Refused`.
  - `TestInterleave_LockUncommittedThenUnlockThenCommit` (never an entity in an unlocked model).
  - `TestUnlock_WaitsOnCommitHoldingLifecycleRow`: the unlock waits, then answers `ModelHasEntitiesError` when the writer committed, or proceeds when it rolled back.

  Run them and expect FAIL. If PostgreSQL does not raise `40001` on the `INSERT … ON CONFLICT DO NOTHING` probe for a row invisible to the snapshot, **stop and report**: spec §4.2 depends on it.
- [ ] **Step 3:** Implement spec §4.2 "postgres":
  - Step 1 of the commit is `SELECT … FROM model_lifecycle … FOR SHARE` in (name, version) order. For a ref with no row, run `INSERT … ON CONFLICT DO NOTHING RETURNING 1`: inserted → `ErrModelNotLocked`; `40001` → `ErrConflict`. Then read `models` state and keys in the snapshot.
  - Steps 2-4: `DELETE` the claims of every written id, then `INSERT` the new claims in (model, key id, signature) order.
  - The non-transactional path runs the `FOR SHARE` as its own statement and the `models` read as a later statement, inside `replaceClaims`, before `stampOwnCommitInstant`.
- [ ] **Step 4:** Rewrite `SameTxReclaim*` and migrate `unique_claims_test.go` and `entity_store_nontx_atomic_test.go` to keys on the model. Run `go test ./...` in `plugins/postgres` and expect PASS.
- [ ] **Step 5:** Commit: `fix(postgres)!: unique keys and model state checked at commit from the stored model`.

### Task 16: delete `spi.WithUniqueKeys` and the root module's key plumbing

**Files:**
- Delete: `cyoda-go-spi/unique_context.go`, `cyoda-go-spi/unique_context_test.go`
- Modify: `internal/domain/entity/service.go` (the four `spi.WithUniqueKeys` sites ~:272, ~:2075-2077, ~:2263, ~:2533-2534; `parsedItem.uniqueKeys` ~:1943, ~:1993; comments about keys riding the context). `checkUniqueKeys` (~:187) stays.
- Modify: `internal/domain/entity/service_unique_keys_test.go`, `internal/e2e/unique_keys_processor_test.go` header (~:8-12)
- Modify: `plugins/memory` `sm_audit_txindex_prune_test.go` and `tx_abort_test.go`, plus any remaining plugin test that uses the context helpers

- [ ] **Step 1:** Run the exit check and expect hits:
  `grep -rn --include='*.go' "UniqueKeysFromContext\|WithUniqueKeys\|recordUniqueKeys\|txUniqueKeys\|uniqueKeysFor" . /Users/paul/go-projects/cyoda-light/cyoda-go-spi`
- [ ] **Step 2:** Delete the helpers and every use. Rewrite `service_unique_keys_test.go` to state keys on the model.
- [ ] **Step 3:** Run the exit check and expect empty output. Run `go build ./... && go vet ./...` in the root and in each plugin, then `go test ./internal/domain/entity/...`, and expect PASS.
- [ ] **Step 4:** Run the existing `unique_keys_*` e2e and parity tests and `make test`, and expect PASS.
- [ ] **Step 5:** Commit the SPI branch (`feat!: remove WithUniqueKeys/UniqueKeysFromContext`) and cyoda-go (`refactor(entity)!: storage reads unique keys from its own model`).

### Task 17: e2e, parity and gRPC for commit-time keys and the commit-time model check

Written after Task 16. Each test is proven RED on the Phase C head (the commit before Task 13), checked out in a scratch worktree; a test that cannot go RED there is dropped.

**Files:**
- Create: `e2e/parity/unique_keys_commit_time.go` (registered in `registry.go`)
- Create: `internal/e2e/unique_keys_commit_time_test.go`

- [ ] **Step 1: Write the parity scenarios** (all backends):
  - `UniqueSwapInOneTx`: two entities swap key values through a joined callback in one transaction → commits.
  - `UniqueChainOfMoves`: A takes B's value, B takes C's → commits.
  - `UniqueFreeAfterClaim`: claim first, free second → commits.
  - `UniqueSameValueTwiceInTx` → `409 UNIQUE_VIOLATION`.
  - `UniqueIntermediateDuplicateFixed`: a callback write duplicates, and a later processor fixes it → commits.
  - `ModelNotLockedAtCommit`: a create whose `SYNC` processor (`cb-unlock-then-tag`, or a new catalog entry `cb-unlock-model` if that one does not fit) unlocks the model without the token during the callout. Every backend answers `409`. On postgres, a retry until the answer is not `CONFLICT` gives `MODEL_NOT_LOCKED`. No entity is stored. The model is `UNLOCKED` with 0 entities.
- [ ] **Step 2: Write the e2e tests** (postgres; spec §7 rows):
  - `TestUniqueKeys_JoinedCallbackDuplicate_OwnerCommit409` (callback 2xx, owner `409`), plus the gRPC `tx-token` variant
  - `TestUniqueKeys_CallbackProcessorPartialKey_OwnerCommit422`
  - `TestModelNotLockedAtCommit_Create` and `_CollectionCreate`: first answer `409 CONFLICT`, retry `409 MODEL_NOT_LOCKED`
  - `TestModelNotLockedAtCommit_UpdateViaCallbackCreate`: an update whose `SYNC` callback creates an entity of model M2, which the processor then unlocks → `409`; nothing committed; M2 `UNLOCKED` with 0 entities
  - gRPC: single create and collection create → envelope `Error.Code`
- [ ] **Step 3:** Run them in the scratch worktree on the Phase C head and expect FAIL: the swaps are refused, postgres refuses intermediate states, and the model-not-locked cases commit. Record the outcome per case.
- [ ] **Step 4:** Run them on the branch head (`make test` for parity; `go test -timeout 30m ./internal/e2e/ -run 'UniqueKeys|ModelNotLockedAtCommit'`; the gRPC package) and expect PASS. Commit: `test: commit-time unique keys and model check (e2e, parity, gRPC)`.

### Task 18: scheduled fire — claims follow the final entity

**Files:**
- Test: `internal/domain/workflow/fire_scheduled_test.go`; `internal/e2e/scheduled_fire_unique_keys_test.go` (new)

- [ ] **Step 1:** Write the engine/plugin-level test `TestFireScheduled_KeyChangeMovesClaim` (the fire changes a key field → the old value is free and the new one is held) and `TestFireScheduled_BreaksKey_FailedRun` (a fire whose final entity duplicates a key → the run fails and `decideBookkeeping` records the attempt). Add the e2e equivalent: the task ends `FAILED` once `timeoutMs` passes, with the error text on the task. Confirm the first FAILS on the pre-Task-13 base (memory dropped the claims) and PASSES now.
- [ ] **Step 2:** Commit: `test(scheduler): a scheduled fire's claims follow its final entity`.

---

## Phase E — The engine saves the anchor before each callout that carries the token (spec §2)

### Task 19: pre-callout save helper; SYNC / ASYNC_SAME_TX processors

**Files:**
- Modify: `internal/domain/workflow/anchor.go` (new helper), `internal/domain/workflow/engine_processors.go` (`executeSyncProcessor` ~:227-283)
- Modify: `internal/domain/workflow/processor_output.go` or a new `errors.go` (new sentinel)
- Test: `internal/domain/workflow/anchor_presave_test.go` (new); `engine_cbd_callback_test.go` (`TestProcessorCallback_FirstWriteOfCommittedBytes_IsAdopted` relies on the stale read; rewrite it to the rule of spec §2.4)

**Interfaces:**
- Produces:
  ```go
  // ErrAnchorSaveInfra marks a failure of the save the engine makes before a
  // callout that carries the transaction token.
  var ErrAnchorSaveInfra = errors.New("saving the entity before a callout failed")

  // saveAnchorBeforeCallout makes the transaction hold the in-memory anchor
  // (spec §2.2) and sets entity.Meta.TransactionID to the stored anchor's id
  // (spec §2.5). No-op unless txID != "" and a transaction is in ctx.
  func (e *Engine) saveAnchorBeforeCallout(ctx context.Context, entity *spi.Entity, txID string, creating bool) error
  ```
  Rules: read with `readAnchor`. If found and `Data` bytes and `Meta.State` are equal → no save; set `Meta.TransactionID` to the read id. If they differ → `EntityStore.Save` of the in-memory anchor; set `Meta.TransactionID = txID`. If not found: save only when `creating` and the anchor has not been stored yet (track this per cascade run); otherwise no save, and `Meta.TransactionID` is unchanged. Every error is `errors.Join(ErrAnchorSaveInfra, err)`. It never acquires the `txcoord` gate and runs before `beforeDispatch` and before `txcoord.Pause`.
- Consumes: `creating` is true only on the `Execute` (create) path; thread it through the run's state, not a context value.

- [ ] **Step 1: Write the engine unit tests** (memory factory, `EnsureLockedModel`, fake external processor that reads the anchor under the tx ctx during dispatch):
  - `TestPreCalloutSave_Update_CallbackSeesRequestData`
  - `TestPreCalloutSave_Create_CallbackFindsAnchor`
  - `TestPreCalloutSave_SecondProcessorSeesFirstProcessorsData`
  - `TestPreCalloutSave_NoChange_NoSave` (no new write in the tx: `readAnchor` still shows the committed tx id)
  - `TestPreCalloutSave_NoTxID_NoSave` and `TestPreCalloutSave_NoTxInCtx_NoSave`
  - `TestPreCalloutSave_AnchorDeletedByCallback_NotResurrected` (Review Focus 4)
  - `TestPreCalloutSave_FailureIsInfra_NotMarkedUnsafe` (a failing store double → `errors.Is(err, ErrAnchorSaveInfra)` and the run guard shows no unsafe work)

  Run `go test ./internal/domain/workflow/ -run 'PreCalloutSave'` and expect FAIL.
- [ ] **Step 2:** Implement the helper and call it at the top of `executeSyncProcessor` when `tx != nil`, before the existing "before" `readAnchor`, so the "before" read comes after the save (spec §2.4).
- [ ] **Step 3:** Rewrite `TestProcessorCallback_FirstWriteOfCommittedBytes_IsAdopted` to the post-save rule (the "before" view is the saved anchor). Run `go test ./internal/domain/workflow/...` and expect PASS.
- [ ] **Step 4:** Commit: `feat(workflow)!: save the anchor before a processor callout that carries the token`.

### Task 20: the other callouts — ASYNC_NEW_TX, function criteria, schedule functions

**Files:**
- Modify: `engine_processors.go` (ASYNC_NEW_TX: the save goes **before** `e.coord.OpenChild` ~:337, in the chain's scope), `criterion_eval.go` (`dispatchCriterionFunction` ~:166, before `Pause` ~:173), `arm.go` (`armViaFunction` ~:264, before `Pause` ~:269)
- Test: `anchor_presave_test.go`

- [ ] **Step 1: Write the tests:**
  - `TestPreCalloutSave_AsyncNewTx_CallbackSeesAnchor_FailureKeepsSave` (a processor failure discards the child scope but not the save)
  - `TestPreCalloutSave_CriterionCascade_SeesCurrentStateAndData`
  - `TestPreCalloutSave_WorkflowSelectionOnCreate_EmptyState`
  - `TestPreCalloutSave_ScheduleFunction_ClientDoor`
  - `TestAvailableTransitionsRead_RunsCriterion_NoWrite` (`resolveWorkflowForQuery` passes an empty txID)

  Expect FAIL.
- [ ] **Step 2:** Implement: one call per site. The `criterionContext` (`cc.txID`, `cc.ctx`) carries the dispatch's ids. `COMMIT_BEFORE_DISPATCH` gets no call (spec §2.1).
- [ ] **Step 3:** Run `go test ./internal/domain/workflow/...` and expect PASS. Commit: `feat(workflow)!: save the anchor before criterion, async-new-tx and schedule-function callouts`.

### Task 21: payload `transactionId` and the scheduled fire's precondition

**Files:**
- Modify: `engine_processors.go` (`flushAndCommitSegment` ~:576-594 and the post-dispatch apply `CompareAndSave` ~:533 set `entity.Meta.TransactionID` to the stored id), `fire_scheduled.go` (move `expectedTxID` ~:271-273 and the attribution stamp ~:267-269 before `resolveWorkflow` ~:239; the `expectedTxID == ""` refusal stays where it is and tests the captured value)
- Test: `anchor_presave_test.go`, `fire_scheduled_test.go`

- [ ] **Step 1:** Audit every reader of `entity.Meta.TransactionID` in `internal/domain/workflow` and `internal/domain/entity` (`grep -rn "Meta.TransactionID" internal/domain`). List in the commit body each reader that depends on the old value, and how it is handled.
- [ ] **Step 2: Write the tests:**
  - `TestPayloadTxID_MatchesCallbackRead_UpdatePath`
  - `TestPayloadTxID_MatchesCallbackRead_CommitBeforeDispatch`
  - `TestFireScheduled_DeclineAfterCriterion_NoVersion_IfMatchStillValid`
  - `TestFireScheduled_PreconditionTakenBeforeCallouts`

  Expect FAIL on the first two and the last.
- [ ] **Step 3:** Implement, run `go test ./internal/domain/...`, and expect PASS. Commit: `fix(workflow)!: the payload's transactionId is the stored anchor's`.

### Task 22: scheduled fire seals after its schedule functions (spec §2.6)

**Files:**
- Modify: `internal/domain/workflow/fire_scheduled.go` (the end of `fireScheduled`, ~:316-375; the `writtenHere` comment)
- Test: `fire_scheduled_test.go`, `fire_callback_test.go`

- [ ] **Step 1: Write the tests:**
  - `TestFireScheduled_ScheduleFunctionCallbackAdmitted` (today `410`)
  - `TestFireScheduled_ScheduleFunctionCallbackDeletesAnchor_RunFails` (nothing committed)
  - `TestFireScheduled_ArmsFunctionSchedule_FinalCASNamesThisTx`

  Expect FAIL.
- [ ] **Step 2:** Reorder to spec §2.6 steps 1-6: cascade; anchor read and "deleted in this transaction" outcome; `removeOwnLife` then `reconcileScheduledTasks`; `Seal`; a second anchor read deciding `writtenHere` (not found → fail the run); final `CompareAndSave` and commit.
- [ ] **Step 3:** Run `go test ./internal/domain/workflow/... ./internal/scheduler/...` and expect PASS. Commit: `fix(workflow): a scheduled fire seals after its schedule functions`.

### Task 23: error classification of the pre-callout save; HTTP, gRPC, e2e, parity

**Files:**
- Modify: `internal/domain/entity/service.go` (`engineConflictIsTransactionConflict` ~:2777 and `classifyWorkflowError`'s infrastructure list ~:2861 both include `wfengine.ErrAnchorSaveInfra`)
- Test: `internal/domain/entity/service_presave_errors_test.go` (new); `internal/grpc/` envelope tests; `internal/e2e/callback_read_current_test.go` (new); parity `e2e/parity/callback_read_current.go` (new, registered)

- [ ] **Step 1: Write the service unit tests** with a failing store double:
  - a conflict from the save → `409 CONFLICT`, retryable; in a collection update it is not isolated as `ENTITY_MODIFIED`, and the loop stops
  - a storage outage → `503`
  - any other failure → `500` with a ticket, never `400 WORKFLOW_FAILED`

  Expect FAIL, then implement and expect PASS.
- [ ] **Step 2: Write the e2e tests** (callbacks via `cmd/compute-test-client`; reuse `cb-read-your-writes`, `cb-criterion-reads`, `cb-grpc-search-read-your-writes`, `cb-ifmatch-update`, and add a catalog entry only where none fits):
  - update / create / function criterion / workflow selection / `ASYNC_NEW_TX` / schedule function (client door and scheduled fire) → callback `GET` under the token returns the current anchor
  - callback search, count and page find the anchor by its current data (HTTP and gRPC)
  - a callback `If-Match` with the payload id matches (update path, scheduled fire, `COMMIT_BEFORE_DISPATCH` with `startNewTxOnDispatch: true`)
  - a scheduled fire that declines after a function criterion → no new version
  - available transitions under a token → no write
  - gRPC (in `internal/grpc`, with the service over a store double that fails the pre-callout save): a conflict → envelope `Success == false`, `Error.Code == "CONFLICT"`; a storage outage → the `503` code; any other failure → the `500` code. The e2e storage-failure cell is waived in the spec (no outage without a test hook).

  Confirm each FAILS on the pre-Task-19 base, and drop any that cannot.
- [ ] **Step 3: Write the parity scenarios:** update `SYNC` read-back sees the request data; create `SYNC` read-back finds the anchor; function criterion sees current data; callback search, count and page under the token.
- [ ] **Step 4: Write the isolated postgres e2e tests** (not parity; assert consistency, not an interleave):
  - `TestAnchorRowLock_OtherClientWaits_OneWinner`
  - `TestAnchorRowLock_TokenlessSelfWrite_CalloutTimesOut_Rollback` (use a short dispatch timeout)
  - `TestAnchorRowLock_ReaderCommitWaitsThenValidates`
  - `TestPreCalloutSave_Conflict_Retryable409`

  Each must show the old outcome on the pre-Task-19 base, or it is dropped.
- [ ] **Step 5:** Run `make test` and `go test -timeout 30m ./internal/e2e/ -run 'CallbackReadCurrent|AnchorRowLock|PreCalloutSave'`, and expect PASS. Commit: `test: callback reads return the current anchor (e2e, parity, gRPC)`.

---

## Phase F — Documentation, contract records, SPI PR and pins

### Task 24: documentation (spec §9)

**Files:** every file listed in spec §9:
- `cmd/cyoda/help/content/workflows.md`: "What a callback read of the entity returns"; a processor writes its own anchor only with the token; the postgres row-lock wait
- `models.md`: "Transactional ordering"; ~:157 non-atomic unlock; ~:346 import-overlaps-lock sentence
- `errors/UNIQUE_VIOLATION.md`, `errors/MODEL_NOT_LOCKED.md`, `errors/MODEL_HAS_ENTITIES.md` (~:30)
- `crud.md` and `cluster.md`, if they disagree
- `api/openapi.yaml`: `TxToken` parameter; `transactionId` parameter; 409 descriptions ~:3069, ~:3292, ~:3479, ~:3821, ~:4216; overviews ~:64, ~:309; unlock ~:5632-5635; free-before-claim ~:5770. Then `go generate ./api`.
- `docs/CONSISTENCY.md`: §6; ~:280-294; rule 9 ~:676-680
- `docs/PROCESSOR_EXECUTION_MODES.md`, `docs/ARCHITECTURE.md` ~:1860
- the composite-unique-keys staleness residual, wherever it is described (grep `models.md`, `UNIQUE_VIOLATION.md`, `docs/`)

- [ ] **Step 1:** For each phrase that becomes false, grep every phrasing across `cmd/cyoda/help`, `docs/` (except `docs/superpowers/plans`), `api/openapi.yaml` and `README.md`: "earliest", "free before", "free-before-claim", "not atomic", "Stop writes", "saved only when", "UniqueKeysFromContext", "write-time backends", "only when the model is unlocked". Record the empty post-edit greps in the commit body.
- [ ] **Step 2:** Edit, present tense, no history narrative and no issue ids. Run `go generate ./api`, then `go test ./cmd/cyoda/help/... ./api/... ./internal/oasdiffcheck/...`, and expect PASS.
- [ ] **Step 3:** Commit: `docs: callback reads, one version per transaction, commit-time keys, atomic model changes`.

### Task 25: contract records

**Files:** create `docs/cloud-parity/617-one-version-per-transaction.md` (spec §3, §4 and §5 in a few lines, the format of the folder's README); `CHANGELOG.md` `### Breaking`; `COMPATIBILITY.md` (the SPI pseudo-pin row).

- [ ] **Step 1:** Write them and commit: `docs: cloud-parity record and changelog`.
- [ ] **Step 2:** File the issue in `cyoda-go-cassandra` (private; see the sibling-repo conventions): the contract change, the cases its code does not meet (save-then-delete, delete-then-save, savepoints), commit-time unique keys from the stored model, the §4.5 model-change rule, and the removal of `WithUniqueKeys`/`UniqueKeysFromContext` that breaks its build. Do not include internal issue numbers in its title.

### Task 26: SPI PR, pseudo-pin, final verification

- [ ] **Step 1:** Push the SPI branch and open the PR into SPI `main`. Wait for its CI to be green and for it to merge.
- [ ] **Step 2:** In cyoda-go, in **one commit**: pseudo-pin the SPI merge commit in the root and the three plugin `go.mod`s, run `make repin-plugins`, then `go mod tidy` per module, and `make check-spi-pin-sync`. Drop the local `go.work` `use` line only from the working tree, never in a commit.
- [ ] **Step 3:** Run `make preflight && make test`, `go vet ./...` in the root and each plugin, and `GOWORK=off go build ./...` in the root and each plugin. Expect everything green.
- [ ] **Step 4:** Run the review gates on the exact head: `superpowers:requesting-code-review` (a fresh reviewer over the whole branch), then `cyoda-go-security-audit` (a new auditor over the whole PR range). Re-run both after any fix wave. Push, open the PR into `release/v0.9.0`, and finish only when CI is green on the exact PR head.
