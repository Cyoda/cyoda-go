# Point-in-time read cost, and dating writes at commit — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make a postgres point-in-time read cost what the model costs instead of what the history costs, and date every write at the instant its transaction commits rather than the instant it began.

**Architecture:** The point-in-time base query becomes a `CROSS JOIN LATERAL` from `entities` into `entity_versions` — one index probe per entity instead of a walk over every revision. Separately, one `clock_timestamp()` read in the commit phase becomes the transaction's instant, written to narrow columns over the transaction's own rows; the JSONB documents stop carrying copies of these values and reads project them from the columns.

**Tech Stack:** Go 1.26, pgx/v5, PostgreSQL 17, golang-migrate, testcontainers-go.

**Spec:** `docs/superpowers/specs/2026-09-16-pit-cost-and-commit-stamping-design.md`

**Issues:** #582 (read cost), #583 (stamping), #498 (durable submit time). Milestone v0.9.0.

## Global Constraints

- **Branch and PR:** work on `worktree-feat-pit-cost-and-commit-stamp`; the PR targets `release/v0.9.0`, not `main`. One PR, staged as reviewable commits.
- **TDD is mandatory.** Every task writes a failing test first and runs it to observe the failure before implementing. No production code without a failing test driving it.
- **Iteration tier:** `make test` (unit + cross-backend parity, ~115s cold). **Never** add `-count=1` — the targets keep Go's cache deliberately.
- **Postgres plugin tests** need `CYODA_TEST_DB_URL`; `make test-full` is the Gate 5 command and covers the plugin submodules.
- **Go conventions:** `log/slog` only, errors wrapped with `fmt.Errorf("...: %w", err)`, `uuid.UUID` not `string` for UUIDs.
- **No issue IDs in shipped artefacts** — no `#NNN` in code comments, errors, logs, responses, OpenAPI or help content. Commit messages and PR bodies only.
- **Migration numbering:** next free number is `000011`. Every `.up.sql` needs its `.down.sql`. The embed is a glob (`migrate.go:24`), so new files need no registration.
- **Migration index guard:** `TestMigrations_IndexesOnExistingTablesAreConcurrent` requires `CREATE INDEX CONCURRENTLY` for an index on a table an earlier migration created. `CONCURRENTLY` deadlocks this project's concurrent multi-node boot, so a plain index needs a new entry in that test's `grandfathered` map with its own justification.
- **Parity registration:** a new parity scenario needs an entry in `e2e/parity/registry.go` **and** a bump to `wantParityScenarioCount` in `e2e/parity/registry_count_test.go` (currently `272`), or the suite passes while running nothing.
- **Error codes:** every new code needs `cmd/cyoda/help/content/errors/<CODE>.md` (enforced by `TestErrCode_Parity`) and a line in `cmd/cyoda/help/content/errors.md`.
- **Test fixtures — the names in this plan's test code are wrong.** Every task below writes `newPluginFixture(t)`, which **does not exist**. The real helpers in `plugins/postgres` are:
  - `setupEntityTest(t) *postgres.StoreFactory` (`entity_store_test.go:31`), or `setupEntityTestWithTM(t) (*postgres.StoreFactory, *postgres.TransactionManager)` when the task needs a transaction manager (`:14`)
  - `ctxWithTenant(tid spi.TenantID) context.Context` (`store_factory_test.go:27`)
  - `postgres.PoolForTest(factory)` — a package-level function taking the factory (`export_test.go:131`), **not** a method `factory.PoolForTest()` as the code below writes it
  - `factory.NewTransactionManagerForTest()` does not exist either; use `setupEntityTestWithTM`, or construct a second manager the way that helper does

  Adapt to the real helpers, keep each task's SQL, assertions and failure messages as written, and record the adaptation in your report. Do not invent a parallel fixture.

---

## File Structure

**Part A — query shape**
- `plugins/postgres/search_base.go` — the point-in-time base query becomes a named constant plus a builder; single change point for `Search`, `Iterate`, grouped statistics and `GetPage(asAt)`.
- `plugins/postgres/entity_store.go` — `GetAsAt` gains the same ordering tiebreak so the family does not fork.
- `plugins/postgres/migrations/000011_entities_model_index_all.up.sql` / `.down.sql` — the model index without its `WHERE NOT deleted` predicate.
- `plugins/postgres/migration_index_guard_test.go` — new grandfather entry.
- `plugins/postgres/pit_plan_test.go` (new) — EXPLAIN assertion over the shared constant.
- `e2e/parity/temporal.go`, `e2e/parity/registry.go`, `e2e/parity/registry_count_test.go` — three scenarios.

**Part B — stamping**
- `plugins/postgres/migrations/000012_commit_instant_columns.up.sql` / `.down.sql` — new columns, backfill, `submit_times` table, `transaction_id` index.
- `plugins/postgres/entity_doc.go` — `_meta` stops carrying temporal values; marshal/unmarshal take them as arguments.
- `plugins/postgres/entity_store.go` — every read projects the date columns; `Save`/`Delete` run in their own transaction when there is none; `CompareAndSave` loses the stamp-source split.
- `plugins/postgres/grouped_stats.go`, `plugins/postgres/searcher.go` — the three `postgresIter` construction sites and the scanners.
- `plugins/postgres/transaction_manager.go` — the commit-phase stamp, the restamp statements, `submit_times`, `GetSubmitTime`'s durable fallback.
- `plugins/postgres/sm_audit_store.go` — event timestamps projected from the column.
- `plugins/memory/entity_store.go`, `plugins/sqlite/entity_store.go` — model-reference immutability.
- `cyoda-go-spi` — `ErrEntityModelMismatch`, godoc for the three undocumented values, conformance case, CHANGELOG.

---

## Task 1: Non-transactional Save becomes atomic

A non-transactional `Save` runs four separate autocommit statements: it reads the clock, upserts the `entities` row with a `'null'::jsonb` placeholder, updates that row with the real document, then inserts the version row. A concurrent reader can observe `doc = 'null'`, and a failure between statements leaves `entities` updated without its version row. This is independent of everything else in the plan and strictly good.

**Files:**
- Modify: `plugins/postgres/entity_store.go` (`save`, around lines 80-210)
- Test: `plugins/postgres/entity_store_nontx_atomic_test.go` (new)

**Interfaces:**
- Consumes: `newAcquireContext(ctx, s.acquireTimeout)` and the `pool.BeginTx` pattern already used by `CompareAndSave` (`entity_store.go:227-241`).
- Produces: no signature change. `save` keeps `func (s *entityStore) save(ctx context.Context, entity *spi.Entity, stampFrom txTimeSource) (int64, error)`.

- [ ] **Step 1: Write the failing test**

```go
// plugins/postgres/entity_store_nontx_atomic_test.go
package postgres_test

// TestNonTxSave_IsAtomic proves a non-transactional Save leaves no partial
// state behind when one of its statements fails. A version row planted at
// version 1 makes the save's own INSERT into entity_versions violate the
// primary key (tenant_id, entity_id, version); the entities row written
// earlier in the same save must not survive that failure.
func TestNonTxSave_IsAtomic(t *testing.T) {
	ctx, factory, pool, tenant := newPluginFixture(t)
	store, err := factory.EntityStore(ctx)
	if err != nil {
		t.Fatalf("EntityStore: %v", err)
	}

	id := uuid.NewString()
	mref := spi.ModelRef{EntityName: "atomic-probe", ModelVersion: "1"}

	// Plant the collision: version 1 already exists for this id.
	if _, err := pool.Exec(ctx,
		`INSERT INTO entity_versions
		   (tenant_id, entity_id, model_name, model_version, version, valid_time, doc)
		 VALUES ($1, $2, $3, $4, 1, CURRENT_TIMESTAMP, '{"_meta":{}}'::jsonb)`,
		string(tenant), id, mref.EntityName, mref.ModelVersion); err != nil {
		t.Fatalf("plant version row: %v", err)
	}

	_, saveErr := store.Save(ctx, &spi.Entity{
		Meta: spi.EntityMeta{ID: id, ModelRef: mref},
		Data: []byte(`{"n":1}`),
	})
	if saveErr == nil {
		t.Fatal("Save must fail: version 1 already exists for this entity")
	}

	var entitiesRows int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM entities WHERE tenant_id = $1 AND entity_id = $2`,
		string(tenant), id).Scan(&entitiesRows); err != nil {
		t.Fatalf("count entities: %v", err)
	}
	if entitiesRows != 0 {
		t.Errorf("entities row survived a failed non-tx Save: got %d rows, want 0 — "+
			"the save's statements are not atomic", entitiesRows)
	}
}
```

- [ ] **Step 2: Run it to verify it fails**

Run: `go test ./plugins/postgres/ -run TestNonTxSave_IsAtomic`
Expected: FAIL — `entities row survived a failed non-tx Save: got 1 rows, want 0`.

- [ ] **Step 3: Wrap the non-transactional path in its own transaction**

In `save`, before the timestamp read, branch when there is no ambient transaction. Mirror `CompareAndSave`'s existing sequence exactly — acquire-only deadline, `ReadCommitted`, the tenant GUC for RLS, rollback on a `WithoutCancel` context:

```go
// A non-transactional save issues four statements. Run them in one
// transaction of its own so a failure cannot leave the entities row
// updated without its version row, and so no reader observes the
// placeholder document the upsert writes before the real one lands.
// CompareAndSave has done this since it gained its row lock; this brings
// the plain path level with it.
if spi.GetTransaction(ctx) == nil && s.pool != nil {
	acquireCtx, cancelAcquire := newAcquireContext(ctx, s.acquireTimeout)
	tx, err := s.pool.BeginTx(acquireCtx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	cancelAcquire()
	if err != nil {
		return 0, classifyAcquireErr(ctx, acquireCtx, "begin non-tx save", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	if _, err := tx.Exec(ctx, "SELECT set_config('app.current_tenant', $1, true)", string(s.tenantID)); err != nil {
		return 0, fmt.Errorf("set tenant for non-tx save: %w", classifyError(err))
	}

	version, err := s.saveOn(ctx, tx, entity, stampFrom)
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("commit non-tx save: %w", classifyError(err))
	}
	return version, nil
}
return s.saveOn(ctx, s.q, entity, stampFrom)
```

Extract the existing body of `save` into `saveOn(ctx context.Context, q Querier, entity *spi.Entity, stampFrom txTimeSource) (int64, error)`, replacing every `s.q` inside it with the passed `q`. Do not change any statement's SQL in this task.

- [ ] **Step 4: Run the test to verify it passes**

Run: `go test ./plugins/postgres/ -run TestNonTxSave_IsAtomic`
Expected: PASS.

- [ ] **Step 5: Run the plugin suite for regressions**

Run: `go test ./plugins/postgres/`
Expected: PASS.

Two hazards found while executing this task, recorded so the next reader does not repeat them:

- **`CompareAndSave` must call `saveOn`, not `save`.** It already opens its own `pgx.Tx`, and `spi.GetTransaction(ctx)` does not see that raw transaction, so calling `save` re-enters the branch above and opens a second transaction on the same pool while the first holds the row lock — a self-deadlock that surfaces only as a ten-minute test timeout. Pass the querier down instead, and pin it with a regression test that runs the non-transactional `CompareAndSave` under a short context deadline.
- **Passing `q` into `saveOn` is not sufficient on its own.** Every write inside the body must run on that querier, `replaceClaims` included; a copy of the store with its querier repointed (the pattern `CompareAndSave` already uses) is what makes that hold.

- [ ] **Step 5a: Wrap `Delete` too**

`Delete` has the identical four-statement shape and the same window. Wrap it in the same transaction, driven by its own failing test.

Note on testing it: planting a colliding `entity_versions` row — the obvious mirror of the `Save` test above — **does not work here and passes against the unfixed code**. `Delete` writes its version row *first* and updates `entities` second, the opposite order to `Save`, so the collision aborts its first write and leaves nothing partial behind. Make the *second* write fail instead: add a temporary `CHECK (NOT deleted)` constraint on `entities` for the duration of the test, so the delete's `UPDATE entities SET deleted = true` is what fails, then assert the version row it wrote first did not survive.

- [ ] **Step 6: Commit**

```bash
git add plugins/postgres/entity_store.go plugins/postgres/entity_store_nontx_atomic_test.go
git commit -m "fix(postgres): a non-transactional save is one transaction, not four statements"
```

---

## Task 2: The point-in-time base query follows entities

**Files:**
- Modify: `plugins/postgres/search_base.go` (`searchBaseQuery`)
- Test: `plugins/postgres/pit_lateral_test.go` (new)

**Interfaces:**
- Consumes: nothing new.
- Produces: `searchBaseQuery(entityName, modelVersion string, pit *time.Time) (string, []any)` — unchanged signature, unchanged positional args (`$1` tenant, `$2` entity name, `$3` model version, `$4` instant). The outer projection stays `SELECT doc` in this task; Task 6 widens it. The outer query must continue to expose `doc` and `entity_id` unqualified, because `fieldExpr` (`query_planner.go:446`) and `orderByFieldExpr` (`searcher.go:353`) emit bare `doc`, bare `entity_id` and bare column names.

- [ ] **Step 1: Write the failing test**

```go
// plugins/postgres/pit_lateral_test.go
package postgres_test

// TestPIT_DeletedSinceInstant_StillReturned pins the case the lateral form
// must not lose: an entity deleted AFTER the instant is still part of the
// snapshot at that instant.
func TestPIT_DeletedSinceInstant_StillReturned(t *testing.T) {
	ctx, factory, _, _ := newPluginFixture(t)
	store, err := factory.EntityStore(ctx)
	if err != nil {
		t.Fatalf("EntityStore: %v", err)
	}
	mref := spi.ModelRef{EntityName: "pit-lateral", ModelVersion: "1"}

	id := uuid.NewString()
	if _, err := store.Save(ctx, &spi.Entity{
		Meta: spi.EntityMeta{ID: id, ModelRef: mref, State: "LIVE"},
		Data: []byte(`{"category":"physics"}`),
	}); err != nil {
		t.Fatalf("Save: %v", err)
	}

	instant := readbackInstant(t, ctx, store, id) // the backend-stamped time of that version

	if err := store.Delete(ctx, id); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	got, err := store.Search(ctx, spi.Filter{}, spi.SearchOptions{
		ModelName: mref.EntityName, ModelVersion: mref.ModelVersion,
		Limit: 10, PointInTime: &instant,
	})
	if err != nil {
		t.Fatalf("Search at instant: %v", err)
	}
	if len(got) != 1 || got[0].Meta.ID != id {
		t.Fatalf("entity deleted after the instant must still appear in the snapshot at it: got %d results", len(got))
	}

	now := time.Now()
	after, err := store.Search(ctx, spi.Filter{}, spi.SearchOptions{
		ModelName: mref.EntityName, ModelVersion: mref.ModelVersion,
		Limit: 10, PointInTime: &now,
	})
	if err != nil {
		t.Fatalf("Search now: %v", err)
	}
	if len(after) != 0 {
		t.Fatalf("deleted entity must be absent at now: got %d results", len(after))
	}
}

// TestPIT_CreatedAfterInstant_Absent pins the other direction.
func TestPIT_CreatedAfterInstant_Absent(t *testing.T) {
	ctx, factory, _, _ := newPluginFixture(t)
	store, err := factory.EntityStore(ctx)
	if err != nil {
		t.Fatalf("EntityStore: %v", err)
	}
	mref := spi.ModelRef{EntityName: "pit-lateral-after", ModelVersion: "1"}

	seed := uuid.NewString()
	if _, err := store.Save(ctx, &spi.Entity{
		Meta: spi.EntityMeta{ID: seed, ModelRef: mref}, Data: []byte(`{"n":0}`),
	}); err != nil {
		t.Fatalf("seed Save: %v", err)
	}
	instant := readbackInstant(t, ctx, store, seed)

	later := uuid.NewString()
	if _, err := store.Save(ctx, &spi.Entity{
		Meta: spi.EntityMeta{ID: later, ModelRef: mref}, Data: []byte(`{"n":1}`),
	}); err != nil {
		t.Fatalf("later Save: %v", err)
	}

	got, err := store.Search(ctx, spi.Filter{}, spi.SearchOptions{
		ModelName: mref.EntityName, ModelVersion: mref.ModelVersion,
		Limit: 10, PointInTime: &instant,
	})
	if err != nil {
		t.Fatalf("Search at instant: %v", err)
	}
	for _, e := range got {
		if e.Meta.ID == later {
			t.Fatal("an entity created after the instant must not appear in the snapshot at it")
		}
	}
}
```

`readbackInstant` reads the entity's own backend-stamped time rather than a client clock — the point-in-time semantics spec requires black-box exact-T tests to query at the backend-reported timestamp:

```go
func readbackInstant(t *testing.T, ctx context.Context, store spi.EntityStore, id string) time.Time {
	t.Helper()
	metas, err := store.GetVersionMetadata(ctx, id, spi.VersionMetadataOptions{Limit: 1})
	if err != nil {
		t.Fatalf("GetVersionMetadata: %v", err)
	}
	if len(metas) == 0 {
		t.Fatalf("no version metadata for %s", id)
	}
	return metas[0].Timestamp
}
```

- [ ] **Step 2: Run to verify the tests pass against today's query**

Run: `go test ./plugins/postgres/ -run 'TestPIT_(DeletedSince|CreatedAfter)'`
Expected: PASS. These pin behaviour that must **survive** the rewrite; they are the safety net, not the driver. Observe them green now so a regression in Step 4 is unambiguous.

- [ ] **Step 3: Replace the point-in-time branch**

In `search_base.go`, replace the `pit != nil` branch. Note the three load-bearing details: the inner subquery projects what today's `latest` derived table projected, the model predicate is repeated inside the lateral, and `version DESC` is the final tiebreak.

```go
// pitBaseQueryTemplate is the point-in-time base SELECT. It is a named
// constant for the same reason getPageCurrentQuery is: pit_plan_test.go's
// EXPLAIN assertion must plan the query that ACTUALLY runs, not a copy that
// drifts from it.
//
// Shape: one index probe per entity via idx_ev_bitemporal, instead of
// DISTINCT ON walking every revision of every entity up to the instant.
//
// The inner subquery projects exactly what the former `latest` derived table
// projected. The caller's pushdown condition and ORDER BY are generated with
// BARE column names (query_planner.go fieldExpr, searcher.go
// orderByFieldExpr) — `doc`, `entity_id`, `version`, `deleted`. Exposing both
// `entities` and the lateral to those expressions would make `doc` ambiguous
// and would silently resolve `version` against the entity's CURRENT row
// rather than its row at the instant.
//
// The model predicate is repeated inside the lateral: model membership is a
// property of the version row, and filtering only the entities row would move
// that axis onto the entity's current model.
//
// version DESC is the final tiebreak. Every row a transaction writes shares
// one valid_time and one transaction_time, so a delete-then-recreate in one
// transaction ties on both keys; without the tiebreak the winner is arbitrary
// and a plan change can flip it.
//
// $1 tenant, $2 entity name, $3 model version, $4 instant.
const pitBaseQueryTemplate = `SELECT doc FROM (
                SELECT v.doc, v.entity_id, v.version, v.model_name, v.model_version
                FROM entities e
                CROSS JOIN LATERAL (
                  SELECT ev.doc, ev.entity_id, ev.version, ev.model_name, ev.model_version
                  FROM entity_versions ev
                  WHERE ev.tenant_id = e.tenant_id AND ev.entity_id = e.entity_id
                    AND ev.model_name = $2 AND ev.model_version = $3
                    AND ev.valid_time <= $4
                    AND ev.transaction_time <= CURRENT_TIMESTAMP
                  ORDER BY ev.valid_time DESC, ev.transaction_time DESC, ev.version DESC
                  LIMIT 1
                ) v
                WHERE e.tenant_id = $1 AND e.model_name = $2 AND e.model_version = $3
             ) latest
             WHERE (doc->'_meta'->>'deleted')::boolean IS NOT TRUE`
```

and in `searchBaseQuery`:

```go
	if pit != nil {
		return pitBaseQueryTemplate, []any{tid, entityName, modelVersion, *pit}
	}
```

Update the function's doc comment: the outer projection is still `SELECT doc` (the S-1 invariant), and the equivalence rests on `entities` holding a row for every entity that ever existed, an entity's model reference being immutable, and a tombstone being filtered by the existing deleted check.

- [ ] **Step 4: Run the point-in-time tests**

Run: `go test ./plugins/postgres/ -run 'TestPIT|TestSearch|TestIterate|TestGetPage|TestGroupedStats'`
Expected: PASS, including the two new tests.

- [ ] **Step 5: Give `GetAsAt` the same tiebreak**

`GetAsAt` has its own hand-written query (`entity_store.go:379-386`). Add `, version DESC` to its `ORDER BY` so the family cannot fork:

```go
		 ORDER BY valid_time DESC, transaction_time DESC, version DESC
```

- [ ] **Step 6: Run the full plugin suite**

Run: `go test ./plugins/postgres/`
Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add plugins/postgres/search_base.go plugins/postgres/entity_store.go plugins/postgres/pit_lateral_test.go
git commit -m "perf(postgres): a point-in-time read costs entities, not revisions"
```

---

## Task 3: The entities index must cover deleted rows

**Files:**
- Create: `plugins/postgres/migrations/000011_entities_model_index_all.up.sql`
- Create: `plugins/postgres/migrations/000011_entities_model_index_all.down.sql`
- Modify: `plugins/postgres/migration_index_guard_test.go` (grandfather entry)
- Test: `plugins/postgres/pit_plan_test.go` (new)

**Interfaces:**
- Consumes: `pitBaseQueryTemplate` from Task 2, `explainPlan` from `entity_page_plan_test.go:17`.
- Produces: index `idx_entities_model_entity_id` covering deleted rows.

- [ ] **Step 1: Write the failing plan test**

```go
// plugins/postgres/pit_plan_test.go
package postgres_test

// TestPITBaseQuery_ProbesPerEntity asserts the property that bounds the cost:
// the point-in-time read reaches entity_versions through idx_ev_bitemporal,
// one probe per entity, rather than scanning every revision.
//
// It plans pitBaseQueryTemplate itself — the constant the production path
// runs — so a query edit moves the assertion with it.
func TestPITBaseQuery_ProbesPerEntity(t *testing.T) {
	ctx, factory, pool, tenant := newPluginFixture(t)
	mref := spi.ModelRef{EntityName: "pit-plan", ModelVersion: "1"}
	seedPagePlanEntities(t, factory, tenant, mref, 200)

	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer conn.Release()

	plan := explainPlan(t, ctx, conn, postgres.PITBaseQueryForTest(),
		string(tenant), mref.EntityName, mref.ModelVersion, time.Now())

	if !strings.Contains(plan, "idx_ev_bitemporal") {
		t.Errorf("point-in-time read must probe idx_ev_bitemporal per entity; plan was:\n%s", plan)
	}
	if strings.Contains(plan, "Unique") {
		t.Errorf("plan still deduplicates revisions (DISTINCT ON shape); plan was:\n%s", plan)
	}
}
```

Export the constant for the test via a small test hook file, following the plugin's existing export-for-test convention:

```go
// plugins/postgres/export_test_queries.go
package postgres

// PITBaseQueryForTest exposes the point-in-time base SQL so the plan test
// plans the query that actually runs.
func PITBaseQueryForTest() string { return pitBaseQueryTemplate }
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./plugins/postgres/ -run TestPITBaseQuery_ProbesPerEntity`
Expected: FAIL to compile — `undefined: postgres.PITBaseQueryForTest` — then, once the hook exists, PASS or FAIL on plan content. If it passes immediately, keep it: it is a regression guard for Task 2's shape.

- [ ] **Step 3: Write the migration**

```sql
-- plugins/postgres/migrations/000011_entities_model_index_all.up.sql
-- A point-in-time read enumerates entities from `entities` and probes each
-- one's revision at the instant. It must see entities deleted SINCE that
-- instant, whose current row carries deleted = true, so the model index can
-- no longer be partial.
--
-- Replacing the partial index rather than adding a second one: two
-- near-identical indexes on the hot write table cost every insert twice for
-- no gain. Current-state reads keep their `AND NOT deleted` predicate in the
-- query and simply filter after the index lookup.
--
-- Plain CREATE INDEX, not CONCURRENTLY: see the grandfathered entry in
-- migration_index_guard_test.go — CONCURRENTLY deterministically deadlocks
-- this project's concurrent multi-node boot.
DROP INDEX IF EXISTS idx_entities_model_entity_id;

CREATE INDEX IF NOT EXISTS idx_entities_model_entity_id
    ON entities (tenant_id, model_name, model_version, entity_id COLLATE "C");
```

```sql
-- plugins/postgres/migrations/000011_entities_model_index_all.down.sql
DROP INDEX IF EXISTS idx_entities_model_entity_id;

CREATE INDEX IF NOT EXISTS idx_entities_model_entity_id
    ON entities (tenant_id, model_name, model_version, entity_id COLLATE "C")
    WHERE NOT deleted;
```

- [ ] **Step 4: Add the grandfather entry**

In `migration_index_guard_test.go`, add to the `grandfathered` map:

```go
		// idx_entities_model_entity_id, rebuilt without its partial
		// predicate so point-in-time reads can see entities deleted since
		// the instant. Same reasoning as 000008's entry above: CREATE INDEX
		// CONCURRENTLY deadlocks the concurrent multi-node boot path
		// (golang-migrate holds a session advisory lock for the whole Up()
		// run; CONCURRENTLY then waits on every other backend, including a
		// second node's migrator blocked on that very lock). A plain build
		// briefly locks writers out, which is acceptable pre-1.0.
		"000011_entities_model_index_all.up.sql": true,
```

- [ ] **Step 5: Run the guard and plan tests**

Run: `go test ./plugins/postgres/ -run 'TestMigrations|TestPITBaseQuery'`
Expected: PASS.

- [ ] **Step 6: Run the migration tests against a database**

Run: `go test ./plugins/postgres/ -run 'TestRunMigrate'`
Expected: PASS, including the concurrent-boot test.

- [ ] **Step 7: Commit**

```bash
git add plugins/postgres/migrations/000011_* plugins/postgres/migration_index_guard_test.go plugins/postgres/pit_plan_test.go plugins/postgres/export_test_queries.go
git commit -m "perf(postgres): index every entity of a model, not only the live ones"
```

---

## Task 4: Cross-backend parity for the instant

**Files:**
- Modify: `e2e/parity/temporal.go` (three new `Run*` functions)
- Modify: `e2e/parity/registry.go` (three entries)
- Modify: `e2e/parity/registry_count_test.go` (`272` → `275`)

**Interfaces:**
- Consumes: `BackendFixture` and the existing helpers in `e2e/parity/temporal.go`.
- Produces: `RunPITDeletedSinceInstant`, `RunPITCreatedAfterInstant`, `RunPITDeleteRecreateSameTx`, each `func(t *testing.T, fixture BackendFixture)`.

- [ ] **Step 1: Write the three scenarios**

```go
// e2e/parity/temporal.go

// RunPITDeletedSinceInstant asserts an entity deleted after an instant is
// still part of the snapshot at that instant, and absent from the current one.
func RunPITDeletedSinceInstant(t *testing.T, fixture BackendFixture) {
	c := fixture.Client(t)
	model := uniqueModel(t, c, "pit-deleted-since")

	id := c.CreateEntity(t, model, map[string]any{"category": "physics"})
	instant := c.EntityLastUpdateTime(t, id)
	c.DeleteEntity(t, id)

	atInstant := c.ListEntities(t, model, WithPointInTime(instant))
	if len(atInstant) != 1 {
		t.Fatalf("at the instant: got %d entities, want 1 (the entity was deleted later)", len(atInstant))
	}
	now := c.ListEntities(t, model)
	if len(now) != 0 {
		t.Fatalf("now: got %d entities, want 0", len(now))
	}
}

// RunPITCreatedAfterInstant asserts an entity created after an instant is
// absent from the snapshot at that instant.
func RunPITCreatedAfterInstant(t *testing.T, fixture BackendFixture) {
	c := fixture.Client(t)
	model := uniqueModel(t, c, "pit-created-after")

	seed := c.CreateEntity(t, model, map[string]any{"n": 0})
	instant := c.EntityLastUpdateTime(t, seed)
	later := c.CreateEntity(t, model, map[string]any{"n": 1})

	atInstant := c.ListEntities(t, model, WithPointInTime(instant))
	for _, e := range atInstant {
		if e.Meta.ID == later {
			t.Fatal("an entity created after the instant appeared in the snapshot at it")
		}
	}
}

// RunPITDeleteRecreateSameTx asserts that an entity deleted and recreated
// within one transaction resolves deterministically at an instant after that
// transaction: the recreate wins, on every backend. Both rows share the
// transaction's instant, so only a version tiebreak can order them.
func RunPITDeleteRecreateSameTx(t *testing.T, fixture BackendFixture) {
	c := fixture.Client(t)
	model := uniqueModel(t, c, "pit-delete-recreate")

	id := c.CreateEntity(t, model, map[string]any{"gen": 1})

	tx := c.BeginTransaction(t)
	c.DeleteEntityInTx(t, tx, id)
	c.CreateEntityWithIDInTx(t, tx, model, id, map[string]any{"gen": 2})
	c.CommitTransaction(t, tx)

	after := c.EntityLastUpdateTime(t, id)
	got := c.ListEntities(t, model, WithPointInTime(after))
	if len(got) != 1 {
		t.Fatalf("after the delete+recreate transaction: got %d entities, want 1", len(got))
	}
	if gen := got[0].Data["gen"]; gen != float64(2) {
		t.Errorf("the recreate must win at an instant after the transaction: got gen=%v, want 2", gen)
	}
}
```

If a helper named above does not exist on the parity client, add it next to its closest sibling in `e2e/parity/client/` rather than inlining raw HTTP in the scenario.

- [ ] **Step 2: Register them**

In `e2e/parity/registry.go`, add to `allTests`:

```go
	{Name: "PIT/DeletedSinceInstant", Fn: RunPITDeletedSinceInstant},
	{Name: "PIT/CreatedAfterInstant", Fn: RunPITCreatedAfterInstant},
	{Name: "PIT/DeleteRecreateSameTx", Fn: RunPITDeleteRecreateSameTx},
```

and update the header comment's total from 272 to 275.

- [ ] **Step 3: Bump the count guard**

In `e2e/parity/registry_count_test.go`: `const wantParityScenarioCount = 275`.

- [ ] **Step 4: Run the parity suite**

Run: `make test`
Expected: PASS, with the three new scenarios running on memory, sqlite and postgres. A failure on memory or sqlite here is a real divergence — report it rather than special-casing the scenario.

- [ ] **Step 5: Commit**

```bash
git add e2e/parity/
git commit -m "test(parity): pin the instant's edges — deleted since, created after, delete+recreate"
```

---

## Task 5: Schema for the commit instant

**Files:**
- Create: `plugins/postgres/migrations/000012_commit_instant.up.sql` / `.down.sql`

**Interfaces:**
- Produces: `entity_versions.transaction_id`, `entity_versions.creation_date`, `entities.creation_date`, `entities.last_modified`, table `submit_times`, index `idx_ev_transaction`.

- [ ] **Step 1: Write the migration**

```sql
-- plugins/postgres/migrations/000012_commit_instant.up.sql
-- Temporal values move out of the JSONB document and into columns, which the
-- original storage design already names as the source of truth. The commit
-- phase then stamps narrow columns instead of rewriting whole documents.

ALTER TABLE entity_versions ADD COLUMN IF NOT EXISTS transaction_id TEXT NOT NULL DEFAULT '';
ALTER TABLE entity_versions ADD COLUMN IF NOT EXISTS creation_date TIMESTAMPTZ;
ALTER TABLE entities ADD COLUMN IF NOT EXISTS creation_date TIMESTAMPTZ;
ALTER TABLE entities ADD COLUMN IF NOT EXISTS last_modified TIMESTAMPTZ;

-- Backfill from the documents before writes stop populating them.
UPDATE entity_versions
   SET transaction_id = COALESCE(doc->'_meta'->>'transaction_id', ''),
       creation_date  = NULLIF(doc->'_meta'->>'creation_date', '')::timestamptz
 WHERE creation_date IS NULL;

UPDATE entities
   SET creation_date = NULLIF(doc->'_meta'->>'creation_date', '')::timestamptz,
       last_modified = NULLIF(doc->'_meta'->>'last_modified_date', '')::timestamptz
 WHERE creation_date IS NULL;

ALTER TABLE entity_versions ALTER COLUMN creation_date SET NOT NULL;
ALTER TABLE entities ALTER COLUMN creation_date SET NOT NULL;
ALTER TABLE entities ALTER COLUMN last_modified SET NOT NULL;

-- The commit phase finds a transaction's own rows by this column, and
-- GetVersionByTransaction moves onto it from its unindexed JSON probe.
CREATE INDEX IF NOT EXISTS idx_ev_transaction
    ON entity_versions (tenant_id, transaction_id, entity_id, version);

-- Durable submit times: an in-process map answers only on the node that
-- committed, and only until a restart.
CREATE TABLE IF NOT EXISTS submit_times (
    tenant_id   TEXT        NOT NULL,
    tx_id       TEXT        NOT NULL,
    submit_time TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (tenant_id, tx_id)
);

CREATE INDEX IF NOT EXISTS idx_submit_times_pruning ON submit_times (submit_time);

ALTER TABLE submit_times ENABLE ROW LEVEL SECURITY;
CREATE POLICY submit_times_tenant_isolation ON submit_times
    USING (tenant_id = current_setting('app.current_tenant', true));
```

```sql
-- plugins/postgres/migrations/000012_commit_instant.down.sql
DROP TABLE IF EXISTS submit_times;
DROP INDEX IF EXISTS idx_ev_transaction;
ALTER TABLE entities DROP COLUMN IF EXISTS last_modified;
ALTER TABLE entities DROP COLUMN IF EXISTS creation_date;
ALTER TABLE entity_versions DROP COLUMN IF EXISTS creation_date;
ALTER TABLE entity_versions DROP COLUMN IF EXISTS transaction_id;
```

- [ ] **Step 2: Add the grandfather entry for `idx_ev_transaction`**

`entity_versions` is created in 000001, so the guard demands `CONCURRENTLY`. Add to `grandfathered` in `migration_index_guard_test.go`:

```go
		// idx_ev_transaction and the submit_times indexes: same
		// concurrent-boot deadlock as 000008 and 000011, and this file must
		// stay single-transaction anyway because it backfills columns before
		// setting them NOT NULL — CONCURRENTLY cannot run inside that.
		"000012_commit_instant.up.sql": true,
```

- [ ] **Step 3: Run the migration and guard tests**

Run: `go test ./plugins/postgres/ -run 'TestMigrations|TestRunMigrate'`
Expected: PASS.

- [ ] **Step 4: Commit**

```bash
git add plugins/postgres/migrations/000012_* plugins/postgres/migration_index_guard_test.go
git commit -m "feat(postgres): columns for the commit instant, and durable submit times"
```

---

## Task 6: Reads project the dates from columns

**Files:**
- Modify: `plugins/postgres/entity_doc.go` (`entityMeta`, `marshalEntityDoc`, `unmarshalEntityDoc`, `unmarshalEntityVersion`)
- Modify: `plugins/postgres/entity_store.go` (every read query and `scanEntities`, `scanEntitiesFilterDeleted`)
- Modify: `plugins/postgres/search_base.go` (both branches project the dates)
- Modify: `plugins/postgres/grouped_stats.go`, `plugins/postgres/searcher.go` (the three `postgresIter` sites)
- Test: `plugins/postgres/entity_doc_test.go`

**Interfaces:**
- Consumes: the columns from Task 5.
- Produces:
  - `unmarshalEntityDoc(raw []byte, creationDate, lastModified time.Time) (*spi.Entity, error)`
  - `unmarshalEntityVersion(raw []byte, version int64, validTime, creationDate time.Time) (*spi.EntityVersion, error)`
  - `marshalEntityDoc(entity *spi.Entity, deleted bool) ([]byte, error)` — the three time arguments go away.
  - `scanEntities(rows pgx.Rows) ([]*spi.Entity, error)` — unchanged signature; now scans three columns.
  - Base query projection, in this fixed order everywhere: `doc, creation_date, last_modified`.

- [ ] **Step 1: Write the failing test**

```go
// plugins/postgres/entity_doc_test.go

// TestEntityDoc_TemporalValuesAreNotInTheDocument asserts the document stops
// carrying temporal values: the columns are the source of truth, and a
// commit-phase stamp must not have to rewrite JSONB to keep them honest.
func TestEntityDoc_TemporalValuesAreNotInTheDocument(t *testing.T) {
	ctx, factory, pool, tenant := newPluginFixture(t)
	store, err := factory.EntityStore(ctx)
	if err != nil {
		t.Fatalf("EntityStore: %v", err)
	}
	id := uuid.NewString()
	if _, err := store.Save(ctx, &spi.Entity{
		Meta: spi.EntityMeta{ID: id, ModelRef: spi.ModelRef{EntityName: "doc-shape", ModelVersion: "1"}},
		Data: []byte(`{"n":1}`),
	}); err != nil {
		t.Fatalf("Save: %v", err)
	}

	var meta map[string]any
	if err := pool.QueryRow(ctx,
		`SELECT doc->'_meta' FROM entities WHERE tenant_id = $1 AND entity_id = $2`,
		string(tenant), id).Scan(&meta); err != nil {
		t.Fatalf("read _meta: %v", err)
	}
	for _, k := range []string{"valid_time", "transaction_time", "wall_clock_time", "creation_date", "last_modified_date"} {
		if _, present := meta[k]; present {
			t.Errorf("_meta still carries %q; temporal values belong in columns", k)
		}
	}

	got, err := store.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Meta.CreationDate.IsZero() || got.Meta.LastModifiedDate.IsZero() {
		t.Error("reported dates must be projected from the columns, not dropped")
	}
}
```

- [ ] **Step 2: Run it to verify it fails**

Run: `go test ./plugins/postgres/ -run TestEntityDoc_TemporalValuesAreNotInTheDocument`
Expected: FAIL — `_meta still carries "valid_time"` and four more.

- [ ] **Step 3: Drop the temporal fields from the document**

In `entity_doc.go`, remove `ValidTime`, `TransactionTime`, `WallClockTime`, `CreationDate` and `LastModifiedDate` from `entityMeta`, and drop the corresponding assignments from `marshalEntityDoc`, whose signature becomes:

```go
func marshalEntityDoc(entity *spi.Entity, deleted bool) ([]byte, error)
```

In `unmarshalEntityDoc`, delete the two `time.Parse` blocks and take the values as arguments:

```go
// unmarshalEntityDoc rebuilds an Entity from its stored document plus the
// temporal columns. The dates are NOT in the document: the columns are the
// source of truth, so a commit-phase stamp updates two narrow columns rather
// than rewriting every document it wrote.
func unmarshalEntityDoc(raw []byte, creationDate, lastModified time.Time) (*spi.Entity, error) {
	...
			CreationDate:            creationDate,
			LastModifiedDate:        lastModified,
	...
}
```

`unmarshalEntityVersion` gains `creationDate` and passes it through:

```go
func unmarshalEntityVersion(raw []byte, version int64, validTime, creationDate time.Time) (*spi.EntityVersion, error) {
	entity, err := unmarshalEntityDoc(raw, creationDate, validTime)
	...
}
```

- [ ] **Step 4: Project the columns in every read**

Each of these queries gains `, creation_date, last_modified` (entities) or `, creation_date` (entity_versions), and its scan site gains the matching variables:

| Site | Query |
|---|---|
| `entity_store.go:353` | `SELECT doc, creation_date, last_modified FROM entities ...` (`Get`) |
| `entity_store.go:380` | `SELECT doc, creation_date, valid_time FROM entity_versions ...` (`GetAsAt`) |
| `entity_store.go:419` | `SELECT doc, version, creation_date, last_modified FROM entities ...` (`Delete`) |
| `getPageCurrentQuery` | `SELECT doc, creation_date, last_modified FROM entities ...` |
| `getVersionByTransactionQuery` | `SELECT doc, version, valid_time, creation_date FROM entity_versions ...` |
| `GetVersionMetadata` | `SELECT version, valid_time, creation_date, doc->'_meta' ...` |
| `pitBaseQueryTemplate` | inner projects `ev.creation_date`, `ev.transaction_time AS last_modified`; outer `SELECT doc, creation_date, last_modified` |
| non-PIT base query | `SELECT doc, creation_date, last_modified FROM entities ...` |

`scanEntities` and `scanEntitiesFilterDeleted` scan three columns:

```go
		var doc []byte
		var creationDate, lastModified time.Time
		if err := rows.Scan(&doc, &creationDate, &lastModified); err != nil {
			return nil, fmt.Errorf("failed to scan entity row: %w", err)
		}
		ent, err := unmarshalEntityDoc(doc, creationDate, lastModified)
```

`postgresIter.Next` (`grouped_stats.go:295`) does the same. Update `search_base.go`'s doc comment: the S-1 invariant becomes "the outer projection is exactly `doc, creation_date, last_modified`, in that order — the column list the row scanners depend on".

- [ ] **Step 5: Run the plugin suite**

Run: `go test ./plugins/postgres/`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add plugins/postgres/
git commit -m "refactor(postgres): temporal values live in columns; reads project them"
```

---

## Task 7: The commit phase stamps the transaction's instant

**Files:**
- Modify: `plugins/postgres/transaction_manager.go` (`Commit`)
- Modify: `plugins/postgres/entity_store.go` (`save`/`saveOn`, `Delete`, `CompareAndSave`; delete the `txTimeSource` split)
- Test: `plugins/postgres/commit_instant_test.go` (new)

**Interfaces:**
- Consumes: the columns and `idx_ev_transaction` from Task 5.
- Produces: `func (tm *TransactionManager) stampCommitInstant(ctx context.Context, tx pgx.Tx, tenantID spi.TenantID, txID string) (time.Time, error)` — reads `clock_timestamp()`, restamps the transaction's rows, returns the instant.

- [ ] **Step 1: Write the failing test**

```go
// plugins/postgres/commit_instant_test.go

// TestCommit_DatesWritesAtCommitNotAtStart proves a read at an instant does
// not change after it has been served. A transaction opens, writes, and only
// then commits; an instant captured between the write and the commit must not
// admit the write afterwards.
func TestCommit_DatesWritesAtCommitNotAtStart(t *testing.T) {
	ctx, factory, _, _ := newPluginFixture(t)
	tm, err := factory.TransactionManager(ctx)
	if err != nil {
		t.Fatalf("TransactionManager: %v", err)
	}
	store, err := factory.EntityStore(ctx)
	if err != nil {
		t.Fatalf("EntityStore: %v", err)
	}
	mref := spi.ModelRef{EntityName: "commit-instant", ModelVersion: "1"}

	txID, txCtx, err := tm.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	id := uuid.NewString()
	if _, err := store.Save(txCtx, &spi.Entity{
		Meta: spi.EntityMeta{ID: id, ModelRef: mref}, Data: []byte(`{"n":1}`),
	}); err != nil {
		t.Fatalf("Save in tx: %v", err)
	}

	// An instant taken while the transaction is still open, from the database
	// clock (never a process clock — the two are different clocks).
	instant := dbNow(t, ctx, factory)

	if err := tm.Commit(txCtx, txID); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	got, err := store.Search(ctx, spi.Filter{}, spi.SearchOptions{
		ModelName: mref.EntityName, ModelVersion: mref.ModelVersion,
		Limit: 10, PointInTime: &instant,
	})
	if err != nil {
		t.Fatalf("Search at the instant: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("a write committed AFTER the instant must not appear in the snapshot at it: got %d", len(got))
	}
}

// TestCommit_OneInstantForEveryEntity asserts a transaction's entities share
// one instant, so a point-in-time cut cannot tear it.
func TestCommit_OneInstantForEveryEntity(t *testing.T) {
	ctx, factory, pool, tenant := newPluginFixture(t)
	tm, _ := factory.TransactionManager(ctx)
	store, _ := factory.EntityStore(ctx)
	mref := spi.ModelRef{EntityName: "commit-shared", ModelVersion: "1"}

	txID, txCtx, err := tm.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	ids := []string{uuid.NewString(), uuid.NewString(), uuid.NewString()}
	for _, id := range ids {
		if _, err := store.Save(txCtx, &spi.Entity{
			Meta: spi.EntityMeta{ID: id, ModelRef: mref}, Data: []byte(`{"n":1}`),
		}); err != nil {
			t.Fatalf("Save %s: %v", id, err)
		}
	}
	if err := tm.Commit(txCtx, txID); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	var distinct int
	if err := pool.QueryRow(ctx,
		`SELECT count(DISTINCT valid_time) FROM entity_versions
		  WHERE tenant_id = $1 AND entity_id = ANY($2)`,
		string(tenant), ids).Scan(&distinct); err != nil {
		t.Fatalf("count distinct: %v", err)
	}
	if distinct != 1 {
		t.Errorf("a transaction's writes must share one instant: got %d distinct valid_time values", distinct)
	}

	submit, err := tm.GetSubmitTime(ctx, txID)
	if err != nil {
		t.Fatalf("GetSubmitTime: %v", err)
	}
	var stamped time.Time
	if err := pool.QueryRow(ctx,
		`SELECT valid_time FROM entity_versions WHERE tenant_id = $1 AND entity_id = $2`,
		string(tenant), ids[0]).Scan(&stamped); err != nil {
		t.Fatalf("read valid_time: %v", err)
	}
	if !submit.Equal(stamped) {
		t.Errorf("the recorded submit time must be the instant stamped on the rows: submit=%s stamped=%s", submit, stamped)
	}
}
```

`dbNow` reads the database clock through the pool, never `time.Now()`:

```go
func dbNow(t *testing.T, ctx context.Context, factory *postgres.StoreFactory) time.Time {
	t.Helper()
	var now time.Time
	if err := factory.PoolForTest().QueryRow(ctx, "SELECT clock_timestamp()").Scan(&now); err != nil {
		t.Fatalf("db clock: %v", err)
	}
	return now
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./plugins/postgres/ -run TestCommit_`
Expected: FAIL — `a write committed AFTER the instant must not appear in the snapshot at it: got 1`, because the rows carry the transaction's start time.

- [ ] **Step 3: Stamp in the commit phase**

In `transaction_manager.go`, add the stamping helper and call it in `Commit` **after** read-set validation and **before** `pgxTx.Commit`, replacing the `SELECT CURRENT_TIMESTAMP` submit-time capture:

```go
// stampCommitInstant fixes the transaction's instant and applies it to every
// row the transaction wrote, immediately before COMMIT.
//
// CURRENT_TIMESTAMP is fixed at transaction START, so it dates a write when
// the transaction opened rather than when it became visible. clock_timestamp()
// read here is the closest a transaction can get to its own commit instant.
//
// The rows are found by transaction_id rather than from the in-memory write
// set, which is not authoritative: after a savepoint rollback the write set
// and the table disagree, and the table is right.
//
// Lock note: validateReadSet's FOR SHARE covers the READ set; these updates
// touch rows this transaction already holds exclusively, so no lock upgrade
// occurs and this cannot deadlock against the validation that precedes it.
// That reasoning depends on the WHERE clauses staying scoped to this
// transaction's own rows.
func (tm *TransactionManager) stampCommitInstant(ctx context.Context, tx pgx.Tx, tenantID spi.TenantID, txID string) (time.Time, error) {
	var instant time.Time
	if err := tx.QueryRow(ctx, "SELECT clock_timestamp()").Scan(&instant); err != nil {
		return time.Time{}, fmt.Errorf("read commit instant: %w", err)
	}
	tid := string(tenantID)

	// creation_date is stamped on EVERY row of an entity whose first version
	// belongs to this transaction — not only on version 1. A transaction that
	// creates an entity and then updates it carries the creation date forward
	// by reading inside the transaction, so the later version holds the
	// provisional value; stamping only version 1 would leave them disagreeing.
	if _, err := tx.Exec(ctx,
		`UPDATE entity_versions SET valid_time = $1, transaction_time = $1,
		        creation_date = CASE WHEN entity_id IN (
		            SELECT entity_id FROM entity_versions
		             WHERE tenant_id = $2 AND transaction_id = $3 AND version = 1
		        ) THEN $1 ELSE creation_date END
		  WHERE tenant_id = $2 AND transaction_id = $3`,
		instant, tid, txID); err != nil {
		return time.Time{}, fmt.Errorf("stamp entity versions: %w", err)
	}

	if _, err := tx.Exec(ctx,
		`UPDATE entities e SET last_modified = $1,
		        creation_date = CASE WHEN EXISTS (
		            SELECT 1 FROM entity_versions v
		             WHERE v.tenant_id = e.tenant_id AND v.entity_id = e.entity_id
		               AND v.transaction_id = $3 AND v.version = 1
		        ) THEN $1 ELSE e.creation_date END
		  WHERE e.tenant_id = $2 AND e.entity_id IN (
		            SELECT entity_id FROM entity_versions
		             WHERE tenant_id = $2 AND transaction_id = $3)`,
		instant, tid, txID); err != nil {
		return time.Time{}, fmt.Errorf("stamp entities: %w", err)
	}

	// Audit events share their transaction's instant, so the audit trail and
	// the version history cannot drift apart or invert.
	if _, err := tx.Exec(ctx,
		`UPDATE sm_audit_events SET timestamp = $1
		  WHERE tenant_id = $2 AND transaction_id = $3`,
		instant, tid, txID); err != nil {
		return time.Time{}, fmt.Errorf("stamp audit events: %w", err)
	}

	if _, err := tx.Exec(ctx,
		`INSERT INTO submit_times (tenant_id, tx_id, submit_time) VALUES ($1, $2, $3)
		 ON CONFLICT (tenant_id, tx_id) DO UPDATE SET submit_time = EXCLUDED.submit_time`,
		tid, txID, instant); err != nil {
		return time.Time{}, fmt.Errorf("record submit time: %w", err)
	}

	return instant, nil
}
```

Keep the existing `25P02` classification around the call: if the transaction is already aborted, the first statement fails and must still map to `spi.ErrConflict` exactly as the old timestamp probe did.

- [ ] **Step 4: Write the transaction id, and delete the stamp-source split**

In `entity_store.go`, the version INSERT gains the column:

```go
		`INSERT INTO entity_versions (tenant_id, entity_id, model_name, model_version, version,
		                              valid_time, wall_clock_time, transaction_id, creation_date, doc)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
```

with `transaction_id` taken from `spi.GetTransaction(ctx)` (empty string when there is none — a non-transactional write keeps the empty id `CompareAndSave` depends on) and `creation_date` the provisional value.

Delete `txTimeSource`, `stampAtTxStart` and `stampAtStatement` entirely; `save` and `CompareAndSave` both stamp provisionally and are corrected at their commit. `wall_clock_time` keeps `clock_timestamp()` at the inserting statement — it is the physical insertion moment by design.

- [ ] **Step 5: Pin what the history window now means**

`GetVersionMetadata`'s `From`/`Until` bound on `valid_time`, which after this task is the commit instant rather than the transaction's start. That is the intended meaning, so pin it:

```go
// TestGetVersionMetadata_WindowBoundsOnCommitInstant asserts the history
// window filters on the instant a revision COMMITTED. A revision written by a
// transaction that started before the window and committed inside it must be
// included; the transaction-start instant would exclude it.
func TestGetVersionMetadata_WindowBoundsOnCommitInstant(t *testing.T) {
	ctx, factory, _, _ := newPluginFixture(t)
	tm, _ := factory.TransactionManager(ctx)
	store, _ := factory.EntityStore(ctx)
	id := uuid.NewString()
	mref := spi.ModelRef{EntityName: "history-window", ModelVersion: "1"}

	txID, txCtx, err := tm.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if _, err := store.Save(txCtx, &spi.Entity{
		Meta: spi.EntityMeta{ID: id, ModelRef: mref}, Data: []byte(`{"n":1}`),
	}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	// The window opens AFTER the transaction started but BEFORE it committed.
	from := dbNow(t, ctx, factory)
	if err := tm.Commit(txCtx, txID); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	metas, err := store.GetVersionMetadata(ctx, id, spi.VersionMetadataOptions{From: &from})
	if err != nil {
		t.Fatalf("GetVersionMetadata: %v", err)
	}
	if len(metas) != 1 {
		t.Fatalf("a revision committed inside the window must be in it: got %d versions", len(metas))
	}
}
```

Run: `go test ./plugins/postgres/ -run TestGetVersionMetadata_WindowBoundsOnCommitInstant`
Expected: FAIL before Step 3's change is in place, PASS after.

- [ ] **Step 6: Run the tests**

Run: `go test ./plugins/postgres/ -run 'TestCommit_|TestNonTxCompareAndSave|TestGetVersionMetadata'`
Expected: PASS. `TestNonTxCompareAndSave_StampsAfterTheLockWait` must still pass — commit stamping satisfies its intent — and its comment gets a line saying why it survives the removal of the mechanism it was written for.

- [ ] **Step 7: Run the full plugin suite**

Run: `go test ./plugins/postgres/`
Expected: PASS.

- [ ] **Step 8: Commit**

```bash
git add plugins/postgres/
git commit -m "fix(postgres): date a write at its commit, not at its transaction's start"
```

---

## Task 8: Submit times survive a restart and answer on any node

**Files:**
- Modify: `plugins/postgres/transaction_manager.go` (`GetSubmitTime`, pruning)
- Test: `plugins/postgres/submit_time_durable_test.go` (new)

**Interfaces:**
- Consumes: `submit_times` (Task 5), `stampCommitInstant` (Task 7).
- Produces: `GetSubmitTime` unchanged in signature; the in-process map stays the fast path, the table the fallback.

- [ ] **Step 1: Write the failing test**

```go
// plugins/postgres/submit_time_durable_test.go

// TestGetSubmitTime_AnswersFromAnotherManager proves the submit time is not
// process-local: a second TransactionManager over the same database — which
// is what another cluster node is — resolves a transaction it never committed.
func TestGetSubmitTime_AnswersFromAnotherManager(t *testing.T) {
	ctx, factory, _, _ := newPluginFixture(t)
	tmA, err := factory.TransactionManager(ctx)
	if err != nil {
		t.Fatalf("TransactionManager A: %v", err)
	}
	store, _ := factory.EntityStore(ctx)

	txID, txCtx, err := tmA.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if _, err := store.Save(txCtx, &spi.Entity{
		Meta: spi.EntityMeta{ID: uuid.NewString(), ModelRef: spi.ModelRef{EntityName: "submit-durable", ModelVersion: "1"}},
		Data: []byte(`{"n":1}`),
	}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if err := tmA.Commit(txCtx, txID); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	want, err := tmA.GetSubmitTime(ctx, txID)
	if err != nil {
		t.Fatalf("GetSubmitTime on the committing manager: %v", err)
	}

	// A second manager stands in for another node: same database, empty map.
	tmB := factory.NewTransactionManagerForTest()
	got, err := tmB.GetSubmitTime(ctx, txID)
	if err != nil {
		t.Fatalf("a node that did not commit the transaction must still resolve it: %v", err)
	}
	if !got.Equal(want) {
		t.Errorf("submit time differs across nodes: got %s, want %s", got, want)
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./plugins/postgres/ -run TestGetSubmitTime_AnswersFromAnotherManager`
Expected: FAIL — the second manager reports the transaction as not found.

- [ ] **Step 3: Fall back to the table**

In `GetSubmitTime`, after the in-memory lookup misses, query the table under the tenant gate:

```go
	// The map is node-local and dies with the process. The table is the
	// authority: a lookup routed to any other node, or arriving after a
	// restart, resolves from it. Reporting node-local ignorance as
	// "transaction not found" would be a wrong definitive answer.
	var submit time.Time
	err := tm.pool.QueryRow(ctx,
		`SELECT submit_time FROM submit_times WHERE tenant_id = $1 AND tx_id = $2`,
		string(tenantID), txID).Scan(&submit)
	if errors.Is(err, pgx.ErrNoRows) {
		return time.Time{}, fmt.Errorf("GetSubmitTime %s: %w", txID, spi.ErrTxNotFound)
	}
	if err != nil {
		return time.Time{}, fmt.Errorf("GetSubmitTime: %w", classifyError(err))
	}
	return submit, nil
```

Prune the table on the same 1-hour TTL the map uses, in the same place the map is pruned, with a bounded `DELETE ... WHERE submit_time < $1`.

- [ ] **Step 4: Run the tests**

Run: `go test ./plugins/postgres/ -run 'TestGetSubmitTime|TestTxState'`
Expected: PASS, including the SPI conformance cases for tenant mismatch and not-found, which must keep their sentinel errors.

- [ ] **Step 5: Commit**

```bash
git add plugins/postgres/
git commit -m "fix(postgres): submit times are durable and answer on every node"
```

---

## Task 9: An entity's model reference is immutable

**Files:**
- Modify: `cyoda-go-spi/errors.go` (new sentinel), `cyoda-go-spi/persistence.go` (godoc), `cyoda-go-spi/spitest/entity.go` (conformance case), `cyoda-go-spi/CHANGELOG.md`
- Modify: `plugins/postgres/entity_store.go`, `plugins/memory/entity_store.go`, `plugins/sqlite/entity_store.go`
- Modify: `internal/common/error_codes.go`, the error mapping in `internal/domain/entity/service.go`
- Create: `cmd/cyoda/help/content/errors/ENTITY_MODEL_MISMATCH.md`
- Modify: `cmd/cyoda/help/content/errors.md`

**Interfaces:**
- Produces: `spi.ErrEntityModelMismatch`, error code `ENTITY_MODEL_MISMATCH` (HTTP 400, not retryable).

- [ ] **Step 1: Add the SPI sentinel and document the instants**

```go
// cyoda-go-spi/errors.go

// ErrEntityModelMismatch is returned by Save when the entity's model
// reference differs from the stored entity's. An entity's model is fixed at
// creation: its model name and version never change. A write that would
// change them is rejected rather than silently rewriting which model the
// entity's history belongs to.
var ErrEntityModelMismatch = errors.New("entity model mismatch")
```

And the godoc the SPI has never carried, on `EntityMeta`:

```go
	// CreationDate is the instant the transaction that created this entity
	// committed. LastModifiedDate is the instant the transaction that wrote
	// this revision committed. Both are assigned by the store, never by the
	// caller; a value supplied on Save is ignored.
	CreationDate     time.Time
	LastModifiedDate time.Time
```

and on `GetSubmitTime`:

```go
	// GetSubmitTime returns the instant the transaction committed. It is the
	// same instant stamped on every row that transaction wrote, and it must
	// be answerable by any node, not only the one that committed.
```

- [ ] **Step 2: Write the failing conformance case**

```go
// cyoda-go-spi/spitest/entity.go — register next to the other Save cases
	runSubtest(t, h, tracker, "Save/ModelReferenceIsImmutable", testEntityModelImmutable)

// testEntityModelImmutable asserts an entity's model reference cannot change.
// Backends that overwrite it silently rewrite which model the entity's
// history belongs to, and a point-in-time read of the former model then
// loses the entity entirely.
func testEntityModelImmutable(t *testing.T, h Harness) {
	ctx := tenantContext(h.NewTenant())
	store, err := h.Factory.EntityStore(ctx)
	require.NoError(t, err)

	id := newID()
	first := spi.ModelRef{EntityName: "immutable-model", ModelVersion: "1"}
	_, err = store.Save(ctx, &spi.Entity{Meta: spi.EntityMeta{ID: id, ModelRef: first}, Data: []byte(`{"n":1}`)})
	require.NoError(t, err)

	second := spi.ModelRef{EntityName: "immutable-model", ModelVersion: "2"}
	_, err = store.Save(ctx, &spi.Entity{Meta: spi.EntityMeta{ID: id, ModelRef: second}, Data: []byte(`{"n":2}`)})
	require.Error(t, err, "saving an existing entity under a different model must be rejected")
	require.True(t, errors.Is(err, spi.ErrEntityModelMismatch),
		"must wrap ErrEntityModelMismatch; got: %v", err)
}
```

- [ ] **Step 3: Run it against all three backends to verify it fails**

Run: `go test ./plugins/memory/ ./plugins/sqlite/ ./plugins/postgres/ -run Conformance/Entity/Save/ModelReferenceIsImmutable`
Expected: FAIL on all three — none rejects today.

- [ ] **Step 4: Enforce it in postgres**

The upsert stops overwriting the model columns and reports the mismatch:

```go
		`INSERT INTO entities (tenant_id, entity_id, model_name, model_version, version, deleted, doc,
		                       creation_date, last_modified)
		 VALUES ($1, $2, $3, $4, 1, false, 'null'::jsonb, $5, $5)
		 ON CONFLICT (tenant_id, entity_id) DO UPDATE SET
		   version = entities.version + 1,
		   deleted = false,
		   doc = entities.doc,
		   last_modified = EXCLUDED.last_modified
		 WHERE entities.model_name = EXCLUDED.model_name
		   AND entities.model_version = EXCLUDED.model_version
		 RETURNING version, (xmax = 0)`,
```

A conflicting row whose model differs now matches no `WHERE`, so the statement returns no rows. Translate that into the sentinel:

```go
	if errors.Is(err, pgx.ErrNoRows) {
		// The upsert's WHERE guard refused: the stored entity belongs to a
		// different model. An entity's model is fixed at creation.
		return 0, fmt.Errorf("entity %s: %w", eid, spi.ErrEntityModelMismatch)
	}
```

- [ ] **Step 5: Enforce it in memory and sqlite**

Both buffer in-transaction writes, so the check runs at `Save` against committed state (and against the transaction's own earlier buffered save), not at flush:

- memory (`entity_store.go`, in both the buffered branch and `saveUnlocked`): compare `entity.Meta.ModelRef` with `versions[len(versions)-1].entity.Meta.ModelRef` when the entity exists, and with `tx.Buffer[id].Meta.ModelRef` when buffered; return `fmt.Errorf("entity %s: %w", eid, spi.ErrEntityModelMismatch)` on a difference.
- sqlite (`entity_store.go`): the same, reading `SELECT model_name, model_version FROM entities WHERE tenant_id = ? AND entity_id = ?` before the upsert, and comparing the buffered copy in the transactional branch.

- [ ] **Step 6: Run the conformance case**

Run: `go test ./plugins/memory/ ./plugins/sqlite/ ./plugins/postgres/ -run Conformance/Entity/Save/ModelReferenceIsImmutable`
Expected: PASS on all three.

- [ ] **Step 7: Map it to an HTTP status**

In `internal/common/error_codes.go`, next to the other entity codes:

```go
	ErrCodeEntityModelMismatch = "ENTITY_MODEL_MISMATCH"
```

and in the entity service's error classification, map `spi.ErrEntityModelMismatch` to `common.Operational(http.StatusBadRequest, common.ErrCodeEntityModelMismatch, ...)`.

- [ ] **Step 8: Write the help topic and index line**

Create `cmd/cyoda/help/content/errors/ENTITY_MODEL_MISMATCH.md` following `ENTITY_MODIFIED.md`'s structure exactly — frontmatter (`topic`, `title`, `stability: stable`, `see_also`), then `# errors.ENTITY_MODEL_MISMATCH`, `## NAME`, `## SYNOPSIS` (`HTTP: 400 Bad Request. Retryable: no.`), `## DESCRIPTION`, `## RECOVERY`, `## SEE ALSO`. The description states that an entity's model is fixed at creation, that the stored model wins, and that the recovery is to write to the entity's own model or create a new entity under the intended one.

Add to `cmd/cyoda/help/content/errors.md`, in code order:

```markdown
- `errors.ENTITY_MODEL_MISMATCH` — `400` — not retryable — a save targeted an existing entity under a different model; an entity's model is fixed at creation
```

- [ ] **Step 9: Run the error-code parity test and an e2e**

Run: `go test ./cmd/... -run TestErrCode_Parity` and `go test ./internal/e2e/ -run ModelMismatch`
Expected: PASS. Add the e2e if none exists: a create under model v1 followed by an update declaring v2, asserting `400` and the code in the body.

- [ ] **Step 10: Cover the same behaviour over gRPC**

HTTP and gRPC are separate entry points and the project's coverage rule requires both. Two assertions in `internal/grpc`:

```go
// TestGRPC_ModelMismatch_ReportsErrorCode asserts the gRPC envelope carries
// the same rejection the HTTP surface returns.
func TestGRPC_ModelMismatch_ReportsErrorCode(t *testing.T) {
	// ... create an entity under model version "1", then submit a save
	// declaring model version "2" over gRPC.
	if resp.Success {
		t.Fatal("a save under a different model must not succeed")
	}
	if resp.Error.Code != "ENTITY_MODEL_MISMATCH" {
		t.Errorf("gRPC error code = %q, want ENTITY_MODEL_MISMATCH", resp.Error.Code)
	}
}

// TestGRPC_ReportedDatesAreTheCommitInstant asserts the meta dates the gRPC
// surface reports match the ones the HTTP surface reports for the same
// entity — one instant, one answer, whichever door the caller used.
func TestGRPC_ReportedDatesAreTheCommitInstant(t *testing.T) {
	// ... create over HTTP, read over gRPC, compare meta.creationDate and
	// meta.lastUpdateTime against the HTTP envelope's values.
}
```

Run: `go test ./internal/grpc/ -run 'TestGRPC_ModelMismatch|TestGRPC_ReportedDates'`
Expected: PASS.

- [ ] **Step 11: Commit**

```bash
git add cyoda-go-spi/ plugins/ internal/ cmd/cyoda/help/content/
git commit -m "fix(storage): an entity's model reference is immutable on every backend"
```

---

## Task 10: Documentation

**Files:**
- Modify: `docs/plugins/POSTGRES.md`, `docs/PRD.md`, `docs/CONSISTENCY.md`, `cmd/cyoda/help/content/crud.md`, `CHANGELOG.md`, `docs/superpowers/specs/2026-06-27-pit-semantics-canonicalization-design.md`, `cyoda-go-spi/CHANGELOG.md`

- [ ] **Step 1: Correct `POSTGRES.md`**

Replace the false line `valid_time — application-supplied timestamp (entity's logical time)` and its neighbours with what the code does after this change: `valid_time` and `transaction_time` are the transaction's commit instant, equal for a normal change and reserved for divergence when a backdated write lands; `wall_clock_time` is the physical insertion moment; the reported dates and the submit time come from the same commit instant. Document the new columns, `submit_times`, and the rebuilt index.

- [ ] **Step 2: Reconcile `PRD.md`**

The document defines `valid_time` twice, incompatibly — "when the fact was true in the domain" (§Immutable bi-temporal history) and "when the entity version became the 'current' truth" (§Temporal Integrity). Keep the first, delete the second's row, and state that for a normal change `valid_time` equals `transaction_time`, with backdating the case that separates them.

- [ ] **Step 3: Add the stamping rule to `CONSISTENCY.md`**

It documents isolation but says nothing about when a write is dated. Add a short section: every backend dates a transaction's writes at the instant it commits, one instant shared by all of them; a point-in-time read at an instant is stable once every transaction that had started before it has finished; a read taken while a transaction commits can still change, and closing that is tracked separately.

- [ ] **Step 4: Update `crud.md`**

Its per-backend point-in-time filter table names postgres's predicate; update it, and state the instant the reported `creationDate`/`lastUpdateTime` carry.

- [ ] **Step 5: CHANGELOG entries**

`CHANGELOG.md` under `[Unreleased]`: the cost fix, the stamping fix with its behaviour change to reported timestamps, durable submit times, model immutability with its new error code, and the non-transactional save atomicity fix. `cyoda-go-spi/CHANGELOG.md`: the new sentinel and the documented instants.

- [ ] **Step 6: Correct the stale spec table**

`2026-06-27-pit-semantics-canonicalization-design.md` tabulates the exact predicate each read path uses. Add a dated note that the postgres rows now read through the lateral form and that the stamp moved to commit; do not rewrite its history.

- [ ] **Step 7: Commit**

```bash
git add docs/ cmd/cyoda/help/content/ CHANGELOG.md ../cyoda-go-spi/CHANGELOG.md
git commit -m "docs: the commit instant, the lateral point-in-time read, and the model invariant"
```

---

## Task 11: Verification

- [ ] **Step 1: Full suite**

Run: `make test-full`
Expected: green, root plus all three plugin submodules including `internal/e2e`. Do not claim completion on a narrower run.

- [ ] **Step 2: Static analysis**

Run: `go vet ./...` and, inside each `plugins/*`, `go vet ./...`
Expected: clean.

- [ ] **Step 3: Race detector, once**

Run: `make race`
Expected: green. This is the CI-parity scope, run once before the PR.

- [ ] **Step 4: Confirm the conformance harness clock floor**

The harness reads `Now` from the database clock with a 5 ms `AdvanceClock` floor sized for clock resolution. With the stamp taken in the commit phase it must also absorb commit latency. Run the conformance suite repeatedly and widen the floor if it flakes:

Run: `go test ./plugins/postgres/ -run Conformance -count=5`
Expected: green five times. (`-count` is legitimate here: this step is specifically hunting a flake.)

- [ ] **Step 5: Push and open the PR**

```bash
git push -u origin worktree-feat-pit-cost-and-commit-stamp
gh pr create --base release/v0.9.0 --title "postgres: point-in-time reads cost entities, and writes are dated at commit" --body-file <path>
```

The PR body closes #582, #583 and #498, and states plainly what is *not* fixed: a read taken while another transaction commits can still change, tracked as the consistency horizon.

---

## Self-Review

**Spec coverage.** Part A → Tasks 2, 3, 4. Part B's stamping → Task 7; columns → Task 5; projection → Task 6; audit events → Task 7 (the third `UPDATE`); durable submit time → Tasks 5 and 8; model immutability → Task 9; non-transactional atomicity → Task 1; documentation → Task 10; the coverage matrix → Tasks 2, 4, 7, 8, 9 plus Task 11.

Two gaps found in review and closed inline: gRPC coverage for the rejection and the reported dates is now Task 9 Step 10, and the history window's meaning is now Task 7 Step 5.

**The one assumption I am recording rather than hiding:** the plan leans on fixture helpers (`newPluginFixture`, `PoolForTest`, `NewTransactionManagerForTest`, the parity client's point-in-time helpers) existing or being trivial to add. Where one does not exist, add it in the task that needs it rather than inventing a parallel fixture.

**Type consistency.** `unmarshalEntityDoc(raw, creationDate, lastModified)`, `unmarshalEntityVersion(raw, version, validTime, creationDate)` and `marshalEntityDoc(entity, deleted)` are used with those exact signatures in Tasks 6, 7 and 9. The projection order `doc, creation_date, last_modified` is fixed in Task 6 and relied on by all three `postgresIter` sites. `stampCommitInstant` is defined in Task 7 and used nowhere else.
