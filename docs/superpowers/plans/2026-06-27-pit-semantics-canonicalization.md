# PIT Semantics Canonicalization Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make point-in-time ("as at T") reads use one canonical rule — inclusive `<=`, native stored precision, no millisecond round-up — uniformly across the memory, sqlite, and postgres storage engines and every PIT read path.

**Architecture:** Remove the 7 `Truncate(time.Millisecond).Add(time.Millisecond)` round-up call sites and flip sqlite `GetAsAt`/`GetAllAsAt` from strict `<` to inclusive `<=`. Every other PIT path is already raw inclusive `<=` and becomes consistent automatically. Drive each engine fix with per-engine white-box boundary tests (deterministic sub-millisecond timestamps); lock cross-engine convergence with an exact-T inclusivity scenario in the shared parity suite (runs against the commercial backend too); add an isolated sqlite test for async select/re-fetch self-consistency; document the contract.

**Tech Stack:** Go 1.26+, three in-tree plugin modules (`plugins/{memory,sqlite,postgres}`, each its own `go.mod`), `e2e/parity` shared scenario registry (root module), PostgreSQL via testcontainers-go (Docker required), sqlite via `modernc.org/sqlite`.

## Global Constraints

- **Canonical PIT rule:** inclusive `<=`, compared at the engine's native stored precision (memory ns, sqlite/postgres µs, commercial ms), **no rounding**. Cross-engine behavioural fidelity is guaranteed only to millisecond granularity.
- **No new error codes, no SPI change, no schema migration, no env vars, no public Go interface change.** (TestErrCode_Parity, COMPATIBILITY.md, chart, SPI pin all untouched.)
- **Commercial (Cassandra) backend is already canonical** — no change in this work; the new parity scenario verifies convergence on its next dependency bump.
- **Logging/secrets:** none touched; standard `log/slog` only.
- **TDD mandatory:** every change is driven by a failing test first (`.claude/rules/tdd.md`).
- **Plugin submodules need explicit test runs** — `go test ./...` from root skips `plugins/*`; run each plugin's tests in its own module directory.
- **Commit messages** end with the `Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>` trailer. Do not put issue numbers in code/comments/test names; `#349` belongs only in commit messages and the PR body.
- **Spec:** `docs/superpowers/specs/2026-06-27-pit-semantics-canonicalization-design.md`.

---

### Task 1: Memory engine — remove round-up (GetAsAt / GetAllAsAt / Iterate-PIT)

**Files:**
- Modify: `plugins/memory/clock.go` (add `NewTestClockAt` constructor)
- Modify: `plugins/memory/entity_store.go` (remove round-up at `GetAsAt` ~line 324, `GetAllAsAt` ~line 420)
- Modify: `plugins/memory/grouped_stats.go` (remove round-up in `buildSnapshot` ~line 90)
- Test: `plugins/memory/pit_boundary_test.go` (new)

**Interfaces:**
- Consumes: `memory.NewStoreFactory(memory.WithClock(c))`, `memory.Clock`, `store.Save/GetAsAt/GetAllAsAt`, `store.(spi.Iterable).Iterate`, existing test helper `ctxWithTenant(spi.TenantID)` (in `plugins/memory/entity_store_test.go:13`).
- Produces: `memory.NewTestClockAt(t time.Time) *memory.TestClock` — a `TestClock` whose virtual time starts at exactly `t` (used by tests that need a millisecond-aligned base).

- [ ] **Step 1: Add the `NewTestClockAt` constructor**

In `plugins/memory/clock.go`, after `NewTestClock`:

```go
// NewTestClockAt returns a TestClock whose virtual time starts at t.
// Tests that need a millisecond-aligned base (so sub-millisecond Advance
// steps land in a known millisecond) use this instead of NewTestClock.
func NewTestClockAt(t time.Time) *TestClock {
	return &TestClock{now: t}
}
```

- [ ] **Step 2: Write the failing boundary tests**

Create `plugins/memory/pit_boundary_test.go`:

```go
package memory_test

import (
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/plugins/memory"
)

// msBase is a millisecond-aligned instant (nanoseconds == 0) so that a
// sub-millisecond Advance keeps both versions inside the same millisecond.
var msBase = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// twoVersionsSameMillisecond saves v1 at msBase and v2 300µs later (same
// millisecond) and returns the store. The buggy round-up rounds msBase up to
// the next millisecond, so a query "as at msBase" wrongly includes v2.
func twoVersionsSameMillisecond(t *testing.T) (spi.EntityStore, interface{ Now() time.Time }) {
	t.Helper()
	clock := memory.NewTestClockAt(msBase)
	factory := memory.NewStoreFactory(memory.WithClock(clock))
	ctx := ctxWithTenant("tenant-pit")
	store, _ := factory.EntityStore(ctx)
	ref := spi.ModelRef{EntityName: "Order", ModelVersion: "1"}

	e := &spi.Entity{
		Meta: spi.EntityMeta{ID: "e-pit", TenantID: "tenant-pit", ModelRef: ref, State: "NEW"},
		Data: []byte(`{"v":1}`),
	}
	if _, err := store.Save(ctx, e); err != nil {
		t.Fatalf("Save v1: %v", err)
	}
	clock.Advance(300 * time.Microsecond)
	e.Data = []byte(`{"v":2}`)
	if _, err := store.Save(ctx, e); err != nil {
		t.Fatalf("Save v2: %v", err)
	}
	return store, clock
}

func TestMemoryPIT_GetAsAt_InclusiveNoRoundUp(t *testing.T) {
	store, _ := twoVersionsSameMillisecond(t)
	ctx := ctxWithTenant("tenant-pit")

	got, err := store.GetAsAt(ctx, "e-pit", msBase)
	if err != nil {
		t.Fatalf("GetAsAt(msBase): %v", err)
	}
	if string(got.Data) != `{"v":1}` {
		t.Errorf("GetAsAt(msBase) = %s, want {\"v\":1} (round-up over-included the same-ms v2)", got.Data)
	}
}

func TestMemoryPIT_GetAllAsAt_InclusiveNoRoundUp(t *testing.T) {
	store, _ := twoVersionsSameMillisecond(t)
	ctx := ctxWithTenant("tenant-pit")
	ref := spi.ModelRef{EntityName: "Order", ModelVersion: "1"}

	got, err := store.GetAllAsAt(ctx, ref, msBase)
	if err != nil {
		t.Fatalf("GetAllAsAt(msBase): %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("GetAllAsAt(msBase) returned %d entities, want 1", len(got))
	}
	if string(got[0].Data) != `{"v":1}` {
		t.Errorf("GetAllAsAt(msBase) data = %s, want {\"v\":1}", got[0].Data)
	}
}

func TestMemoryPIT_Iterate_InclusiveNoRoundUp(t *testing.T) {
	store, _ := twoVersionsSameMillisecond(t)
	ctx := ctxWithTenant("tenant-pit")
	ref := spi.ModelRef{EntityName: "Order", ModelVersion: "1"}

	it := store.(spi.Iterable)
	pit := msBase
	iter, err := it.Iterate(ctx, ref, spi.Filter{}, spi.IterateOptions{PointInTime: &pit})
	if err != nil {
		t.Fatalf("Iterate(msBase): %v", err)
	}
	defer iter.Close()

	var data string
	var seen int
	for iter.Next() {
		seen++
		data = string(iter.Entity().Data)
	}
	if err := iter.Err(); err != nil {
		t.Fatalf("iter err: %v", err)
	}
	if seen != 1 || data != `{"v":1}` {
		t.Errorf("Iterate(msBase) saw %d entities (data=%s), want 1 with {\"v\":1}", seen, data)
	}
}
```

- [ ] **Step 3: Run the tests and confirm they FAIL**

Run: `cd plugins/memory && go test ./... -run TestMemoryPIT -v`
Expected: all three FAIL — `GetAsAt(msBase) = {"v":2}` etc. (the round-up over-includes the same-millisecond v2).

- [ ] **Step 4: Remove the three round-up call sites**

In `plugins/memory/entity_store.go`, in `GetAsAt`, delete the round-up and its comment:

```go
	// (deleted) Round asAt up to the next millisecond boundary...
	// asAt = asAt.Truncate(time.Millisecond).Add(time.Millisecond)
```

i.e. remove the lines:

```go
	// Round asAt up to the next millisecond boundary. Clients work at
	// millisecond precision but submitTime has microsecond/nanosecond
	// precision. A query for "302ms" should include "302.306ms".
	asAt = asAt.Truncate(time.Millisecond).Add(time.Millisecond)
```

In the same file, in `GetAllAsAt`, remove:

```go
	// Round asAt up to the next millisecond boundary (same as GetAsAt).
	asAt = asAt.Truncate(time.Millisecond).Add(time.Millisecond)
```

In `plugins/memory/grouped_stats.go`, in `buildSnapshot`'s PIT branch, replace:

```go
		// Round up to next millisecond boundary, matching GetAllAsAt.
		asAt := pit.Truncate(time.Millisecond).Add(time.Millisecond)
		var snapshot []*spi.Entity
		func() {
			s.factory.entityMu.RLock()
			defer s.factory.entityMu.RUnlock()
			snapshot = s.getAllSnapshotPointersUnlocked(model, asAt)
		}()
		return snapshot, nil
```

with (bind the raw instant, no rounding):

```go
		var snapshot []*spi.Entity
		func() {
			s.factory.entityMu.RLock()
			defer s.factory.entityMu.RUnlock()
			snapshot = s.getAllSnapshotPointersUnlocked(model, *pit)
		}()
		return snapshot, nil
```

If `time` is now unused in `grouped_stats.go`, remove it from the imports.

- [ ] **Step 5: Run the boundary tests and confirm they PASS**

Run: `cd plugins/memory && go test ./... -run TestMemoryPIT -v`
Expected: all three PASS.

- [ ] **Step 6: Run the full memory suite and confirm no regression**

Run: `cd plugins/memory && go test ./... -v`
Expected: PASS, including the existing `TestGetAllAsAt`, `TestSoftDeleteAsAtBeforeDeletion`, `TestGetAllAsAtWithDelete`, grouped-stats and conformance tests (they use multi-millisecond gaps, so they stay green).

- [ ] **Step 7: Commit**

```bash
git add plugins/memory/clock.go plugins/memory/entity_store.go plugins/memory/grouped_stats.go plugins/memory/pit_boundary_test.go
git commit -m "fix(memory): inclusive PIT bound, drop millisecond round-up

GetAsAt/GetAllAsAt/Iterate-PIT now compare against the raw requested
instant (inclusive <=) at native precision instead of rounding up to the
next millisecond, which over-included same-millisecond later versions.

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

### Task 2: Sqlite engine — remove round-up AND flip strict `<` to `<=`

**Files:**
- Modify: `plugins/sqlite/clock.go` (add `NewTestClockAt` constructor)
- Modify: `plugins/sqlite/entity_store.go` (`GetAsAt` ~lines 414/434, `GetAllAsAt` ~lines 549/559)
- Test: `plugins/sqlite/pit_boundary_test.go` (new)

**Interfaces:**
- Consumes: `sqlite.NewStoreFactoryForTest(ctx, dbPath, opts...)`, `sqlite.WithClock`, existing helper `testCtx(tenantID string)` (`plugins/sqlite/searcher_test.go:13`).
- Produces: `sqlite.NewTestClockAt(t time.Time) *sqlite.TestClock`.

**Why one test, two coupled changes:** On current sqlite, `as-at msBase` rounds up to `msBase+1ms` then does `submit_time < msBase+1ms`, returning the latest version in that window — the same-ms v2 (over-inclusion). Removing the round-up alone changes the predicate to `submit_time < msBase`, which now *excludes* the v1 written at exactly `msBase` (strict `<`), yielding `ENTITY_NOT_FOUND`. Only removing the round-up **and** flipping `<`→`<=` returns v1. The single assertion "GetAsAt(msBase) == v1" is therefore RED on current code (returns v2) and requires both edits to go green.

- [ ] **Step 1: Add the `NewTestClockAt` constructor**

In `plugins/sqlite/clock.go`, after `NewTestClock`:

```go
// NewTestClockAt returns a TestClock whose virtual time starts at t.
func NewTestClockAt(t time.Time) *TestClock {
	return &TestClock{now: t}
}
```

- [ ] **Step 2: Write the failing boundary tests**

Create `plugins/sqlite/pit_boundary_test.go`:

```go
package sqlite_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/plugins/sqlite"
)

var pitBase = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// pitTwoVersions saves v1 at pitBase and v2 300µs later (same millisecond),
// driven by a deterministic TestClock.
func pitTwoVersions(t *testing.T) (*sqlite.StoreFactory, context.Context) {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "pit.db")
	clock := sqlite.NewTestClockAt(pitBase)
	factory, err := sqlite.NewStoreFactoryForTest(context.Background(), dbPath, sqlite.WithClock(clock))
	if err != nil {
		t.Fatalf("create factory: %v", err)
	}
	t.Cleanup(func() { factory.Close() })

	ctx := testCtx("tenant-1")
	store, _ := factory.EntityStore(ctx)
	ref := spi.ModelRef{EntityName: "person", ModelVersion: "1"}

	e := &spi.Entity{
		Meta: spi.EntityMeta{ID: "e-pit", ModelRef: ref, State: "NEW"},
		Data: []byte(`{"v":1}`),
	}
	if _, err := store.Save(ctx, e); err != nil {
		t.Fatalf("Save v1: %v", err)
	}
	clock.Advance(300 * time.Microsecond)
	e.Data = []byte(`{"v":2}`)
	if _, err := store.Save(ctx, e); err != nil {
		t.Fatalf("Save v2: %v", err)
	}
	return factory, ctx
}

func TestSqlitePIT_GetAsAt_InclusiveExactT(t *testing.T) {
	factory, ctx := pitTwoVersions(t)
	store, _ := factory.EntityStore(ctx)

	got, err := store.GetAsAt(ctx, "e-pit", pitBase)
	if err != nil {
		t.Fatalf("GetAsAt(pitBase): %v", err)
	}
	if string(got.Data) != `{"v":1}` {
		t.Errorf("GetAsAt(pitBase) = %s, want {\"v\":1} (inclusive of exactly T, no round-up)", got.Data)
	}
}

func TestSqlitePIT_GetAllAsAt_InclusiveExactT(t *testing.T) {
	factory, ctx := pitTwoVersions(t)
	store, _ := factory.EntityStore(ctx)
	ref := spi.ModelRef{EntityName: "person", ModelVersion: "1"}

	got, err := store.GetAllAsAt(ctx, ref, pitBase)
	if err != nil {
		t.Fatalf("GetAllAsAt(pitBase): %v", err)
	}
	if len(got) != 1 || string(got[0].Data) != `{"v":1}` {
		t.Errorf("GetAllAsAt(pitBase) = %v entities, want 1 with {\"v\":1}", len(got))
	}
}
```

- [ ] **Step 3: Run the tests and confirm they FAIL**

Run: `cd plugins/sqlite && go test ./... -run TestSqlitePIT -v`
Expected: FAIL — `GetAsAt(pitBase) = {"v":2}` (round-up over-includes the same-ms v2).

- [ ] **Step 4: Remove the round-up and flip the predicate — `GetAsAt`**

In `plugins/sqlite/entity_store.go`, `GetAsAt`: remove

```go
	// Round asAt up to the next millisecond boundary. Clients work at
	// millisecond precision but submitTime has microsecond precision.
	asAt = asAt.Truncate(time.Millisecond).Add(time.Millisecond)
```

and in the SQL change `submit_time < ?` to `submit_time <= ?`:

```go
	     WHERE tenant_id = ? AND entity_id = ? AND submit_time <= ?
```

- [ ] **Step 5: Remove the round-up and flip the predicate — `GetAllAsAt`**

In the same file, `GetAllAsAt`: remove

```go
	// Round asAt up to the next millisecond boundary.
	asAt = asAt.Truncate(time.Millisecond).Add(time.Millisecond)
```

and change `submit_time < ?` to `submit_time <= ?`:

```go
	     WHERE tenant_id = ? AND model_name = ? AND model_version = ? AND submit_time <= ?
```

- [ ] **Step 6: Run the boundary tests and confirm they PASS**

Run: `cd plugins/sqlite && go test ./... -run TestSqlitePIT -v`
Expected: PASS. (Intermediate check, optional: with only the round-up removed but the predicate still `<`, these FAIL with `ErrNotFound` — confirming the strict `<` was masked by the round-up. Both edits are required.)

- [ ] **Step 7: Run the full sqlite suite and confirm no regression**

Run: `cd plugins/sqlite && go test ./... -v`
Expected: PASS (searcher PIT, grouped-stats, conformance, crash/stress all green).

- [ ] **Step 8: Commit**

```bash
git add plugins/sqlite/clock.go plugins/sqlite/entity_store.go plugins/sqlite/pit_boundary_test.go
git commit -m "fix(sqlite): inclusive PIT bound for GetAsAt/GetAllAsAt

Drop the millisecond round-up and flip submit_time '<' to '<=' so as-at
reads are inclusive of exactly the requested instant at microsecond
precision. The round-up had masked the strict '<'; both are corrected
together. Adds the first GetAsAt/GetAllAsAt tests for sqlite.

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

### Task 3: Postgres engine — remove round-up (GetAsAt / GetAllAsAt)

**Files:**
- Modify: `plugins/postgres/entity_store.go` (`GetAsAt` ~line 183, `GetAllAsAt` ~line 242)
- Test: `plugins/postgres/pit_boundary_test.go` (new)

**Interfaces:**
- Consumes: existing test helpers `setupEntityTest(t) *postgres.StoreFactory` (`plugins/postgres/entity_store_test.go:31`), `makeEntity(id string) *spi.Entity` (`:42`), `ctxWithTenant(...)`, and `factory.Pool() *pgxpool.Pool` (`plugins/postgres/store_factory.go:86`). Postgres has **no** injectable clock (it writes `valid_time = CURRENT_TIMESTAMP`), so the test hand-places `valid_time` via `pool.Exec(UPDATE ...)` after `Save`.
- Produces: nothing consumed downstream.

**Determinism note:** `Save` writes two real version rows (correct docs + `entities` row); the test then rewrites their `valid_time`/`transaction_time` to two explicit same-millisecond instants. `transaction_time` is set to a past date so the `transaction_time <= CURRENT_TIMESTAMP` filter still passes. Postgres `GetAsAt`/`GetAllAsAt` are already `<=`; the only defect is the round-up.

- [ ] **Step 1: Write the failing boundary tests**

Create `plugins/postgres/pit_boundary_test.go`:

```go
package postgres_test

import (
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

const (
	pitTenant = "entity-tenant"
	pitBaseTS = "2026-01-01 00:00:00+00"        // millisecond-aligned
	pitNextTS = "2026-01-01 00:00:00.000300+00" // +300µs, same millisecond
)

func pitParseBase(t *testing.T) time.Time {
	t.Helper()
	bt, err := time.Parse(time.RFC3339Nano, "2026-01-01T00:00:00Z")
	if err != nil {
		t.Fatalf("parse base: %v", err)
	}
	return bt
}

// pitSetup saves v1 then v2 on one entity, then forces deterministic,
// same-millisecond valid_times (v1 at pitBaseTS, v2 300µs later).
func pitSetup(t *testing.T) (*spi.Entity, spi.EntityStore, func() time.Time) {
	t.Helper()
	factory := setupEntityTest(t)
	ctx := ctxWithTenant(pitTenant)
	store, _ := factory.EntityStore(ctx)
	pool := factory.Pool()

	ent := makeEntity("ent-pit")
	ent.Data = []byte(`{"value":"v1"}`)
	if _, err := store.Save(ctx, ent); err != nil {
		t.Fatalf("Save v1: %v", err)
	}
	ent.Data = []byte(`{"value":"v2"}`)
	if _, err := store.Save(ctx, ent); err != nil {
		t.Fatalf("Save v2: %v", err)
	}

	if _, err := pool.Exec(ctx,
		`UPDATE entity_versions SET valid_time=$1, transaction_time=$1
		 WHERE tenant_id=$2 AND entity_id=$3 AND version=1`,
		pitBaseTS, pitTenant, "ent-pit"); err != nil {
		t.Fatalf("update v1 valid_time: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`UPDATE entity_versions SET valid_time=$1, transaction_time=$1
		 WHERE tenant_id=$2 AND entity_id=$3 AND version=2`,
		pitNextTS, pitTenant, "ent-pit"); err != nil {
		t.Fatalf("update v2 valid_time: %v", err)
	}
	return ent, store, func() time.Time { return pitParseBase(t) }
}

func TestPostgresPIT_GetAsAt_InclusiveNoRoundUp(t *testing.T) {
	_, store, base := pitSetup(t)
	ctx := ctxWithTenant(pitTenant)

	got, err := store.GetAsAt(ctx, "ent-pit", base())
	if err != nil {
		t.Fatalf("GetAsAt(base): %v", err)
	}
	if string(got.Data) != `{"value":"v1"}` {
		t.Errorf("GetAsAt(base) = %s, want v1 (round-up over-included same-ms v2)", got.Data)
	}
}

func TestPostgresPIT_GetAllAsAt_InclusiveNoRoundUp(t *testing.T) {
	_, store, base := pitSetup(t)
	ctx := ctxWithTenant(pitTenant)
	ref := spi.ModelRef{EntityName: "Order", ModelVersion: "1"}

	got, err := store.GetAllAsAt(ctx, ref, base())
	if err != nil {
		t.Fatalf("GetAllAsAt(base): %v", err)
	}
	if len(got) != 1 || string(got[0].Data) != `{"value":"v1"}` {
		t.Errorf("GetAllAsAt(base) = %d entities, want 1 with v1", len(got))
	}
}
```

(If `makeEntity` uses a model name other than `Order`/version `1`, set `ref` to match it — check `plugins/postgres/entity_store_test.go:42`.)

- [ ] **Step 2: Run the tests and confirm they FAIL**

Run: `cd plugins/postgres && go test ./... -run TestPostgresPIT -v`
Expected: FAIL — `GetAsAt(base) = {"value":"v2"}` (round-up over-includes the same-ms v2). (Docker must be running for the testcontainer.)

- [ ] **Step 3: Remove the two round-up call sites**

In `plugins/postgres/entity_store.go`, `GetAsAt`, remove:

```go
	// Round up to the next millisecond boundary (matching memory implementation).
	asAt = asAt.Truncate(time.Millisecond).Add(time.Millisecond)
```

In `GetAllAsAt`, remove:

```go
	asAt = asAt.Truncate(time.Millisecond).Add(time.Millisecond)
```

If `time` becomes unused in the file, remove it from imports (it is still used elsewhere — verify with the compiler).

- [ ] **Step 4: Run the boundary tests and confirm they PASS**

Run: `cd plugins/postgres && go test ./... -run TestPostgresPIT -v`
Expected: PASS.

- [ ] **Step 5: Run the full postgres suite and confirm no regression**

Run: `cd plugins/postgres && go test ./... -v`
Expected: PASS (existing `TestEntityStore_GetAsAt`, `TestEntityStore_GetAllAsAt`, grouped-stats, conformance all green).

- [ ] **Step 6: Commit**

```bash
git add plugins/postgres/entity_store.go plugins/postgres/pit_boundary_test.go
git commit -m "fix(postgres): inclusive PIT bound, drop millisecond round-up

GetAsAt/GetAllAsAt bind the raw requested instant (already <=) instead of
rounding up to the next millisecond, which over-included same-millisecond
later versions.

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

### Task 4: Cross-backend parity — exact-T inclusivity scenario

**Files:**
- Create: `e2e/parity/pit_boundary.go`
- Modify: `e2e/parity/registry.go` (register the new scenario)
- Test: runs via existing per-backend wrappers (`e2e/parity/{memory,sqlite,postgres}` + commercial on its next dep bump)

**Interfaces:**
- Consumes: `parity.BackendFixture`, the in-process driver `driver.NewInProcess(t, fixture)` with `CreateModelFromSample`, `LockModel`, `CreateEntity`, `UpdateEntityData`, `GetEntityChanges(id) ([]parityclient.EntityChangeMeta, error)`, `GetEntityAt(id, t) (parityclient.EntityResult, error)`. `EntityChangeMeta.TimeOfChange time.Time`; `EntityResult.Data map[string]any`. (See `e2e/parity/externalapi/point_in_time.go` for the established pattern.)
- Produces: `RunPITBoundaryExactT(t *testing.T, fixture parity.BackendFixture)` registered as scenario `"PITBoundaryExactT"`.

**What it guards:** For each version written at its own reported `TimeOfChange` Tᵢ, `GetEntityAt(Tᵢ)` returns that version's data — inclusive of exactly T, at millisecond granularity (the floor every backend, including the commercial ms-precision one, can honour). This is RED on pre-fix sqlite (strict `<` returns the previous version) and green on every backend after Tasks 1–3.

- [ ] **Step 1: Write the scenario**

Create `e2e/parity/pit_boundary.go`:

```go
package parity

import (
	"testing"
	"time"

	"github.com/cyoda-platform/cyoda-go/e2e/externalapi/driver"
)

// RunPITBoundaryExactT asserts exact-T inclusivity: querying as-at a version's
// own reported timestamp returns that version, uniformly across backends. It is
// the cross-engine guard for the canonical inclusive `<=` PIT rule. Sub-ms
// over-inclusion is covered by per-engine white-box tests, not here, because
// the commercial backend stores at millisecond precision.
func RunPITBoundaryExactT(t *testing.T, fixture BackendFixture) {
	t.Helper()
	d := driver.NewInProcess(t, fixture)

	if err := d.CreateModelFromSample("pitb", 1, `{"k":1}`); err != nil {
		t.Fatalf("create model: %v", err)
	}
	if err := d.LockModel("pitb", 1); err != nil {
		t.Fatalf("lock model: %v", err)
	}

	id, err := d.CreateEntity("pitb", 1, `{"k":1}`)
	if err != nil {
		t.Fatalf("create entity: %v", err)
	}
	t1 := latestChangeTime(t, d, fmtID(id))

	if err := d.UpdateEntityData(id, `{"k":2}`); err != nil {
		t.Fatalf("update k=2: %v", err)
	}
	t2 := latestChangeTime(t, d, fmtID(id))

	if err := d.UpdateEntityData(id, `{"k":3}`); err != nil {
		t.Fatalf("update k=3: %v", err)
	}
	t3 := latestChangeTime(t, d, fmtID(id))

	// As-at exactly each version's write time returns that version.
	assertKAt(t, d, id, t1, 1)
	assertKAt(t, d, id, t2, 2)
	assertKAt(t, d, id, t3, 3)
}
```

Add the helpers in the same file:

```go
// latestChangeTime returns the most recent TimeOfChange for the entity — the
// timestamp of the version just written.
func latestChangeTime(t *testing.T, d *driver.Driver, id uuidLike) time.Time {
	t.Helper()
	changes, err := d.GetEntityChanges(id.UUID())
	if err != nil {
		t.Fatalf("GetEntityChanges: %v", err)
	}
	if len(changes) == 0 {
		t.Fatal("GetEntityChanges returned no entries")
	}
	latest := changes[0].TimeOfChange
	for _, c := range changes[1:] {
		if c.TimeOfChange.After(latest) {
			latest = c.TimeOfChange
		}
	}
	return latest
}

func assertKAt(t *testing.T, d *driver.Driver, id uuidLike, at time.Time, wantK float64) {
	t.Helper()
	got, err := d.GetEntityAt(id.UUID(), at)
	if err != nil {
		t.Fatalf("GetEntityAt(%s): %v", at.Format(time.RFC3339Nano), err)
	}
	if got.Data["k"] != wantK {
		t.Errorf("GetEntityAt(%s) k=%v, want %v (exact-T inclusivity violated)",
			at.Format(time.RFC3339Nano), got.Data["k"], wantK)
	}
}
```

**Note for the implementer:** the `id` returned by `CreateEntity` is a `uuid.UUID`. The pseudo-types `uuidLike`/`fmtID`/`uuid.UUID()` above are placeholders for "pass the `uuid.UUID` straight through" — write the real signatures using `github.com/google/uuid`: `latestChangeTime(t, d, id uuid.UUID) time.Time` and `assertKAt(t, d, id uuid.UUID, ...)`, calling `d.GetEntityChanges(id)` / `d.GetEntityAt(id, at)` directly. Drop the `fmtID`/`uuidLike` indirection — it only exists here to flag the type. Confirm against `driver.go:108,308,334`.

- [ ] **Step 2: Register the scenario**

In `e2e/parity/registry.go`, add to the `allTests` slice, next to the existing bi-temporal entries (`TemporalPointInTimeRetrieval`):

```go
	{"PITBoundaryExactT", RunPITBoundaryExactT},
```

Update the running "Total parity scenarios" comment count at the top of the file (+1).

- [ ] **Step 3: Run the scenario on memory and sqlite (fast, no Docker)**

Run: `go test ./e2e/parity/memory/... -run 'PITBoundaryExactT' -v`
Run: `go test ./e2e/parity/sqlite/... -run 'PITBoundaryExactT' -v`
Expected: PASS. (To prove the guard bites, temporarily `git stash` Task 2's sqlite fix and re-run the sqlite parity — it FAILs with `k=1` at t2/t3 — then `git stash pop`.)

- [ ] **Step 4: Run the scenario on postgres (Docker)**

Run: `go test ./e2e/parity/postgres/... -run 'PITBoundaryExactT' -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add e2e/parity/pit_boundary.go e2e/parity/registry.go
git commit -m "test(parity): exact-T PIT inclusivity across backends

New cross-backend scenario asserting as-at a version's own reported
timestamp returns that version (inclusive <=, millisecond floor). Runs
against memory/sqlite/postgres and the commercial backend; would fail on
the pre-fix sqlite strict-< path.

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

### Task 5: Isolated sqlite test — async select / re-fetch self-consistency

**Files:**
- Test: `plugins/sqlite/pit_consistency_test.go` (new)

**Interfaces:**
- Consumes: `sqlite.NewStoreFactoryForTest` + `WithClock`, `sqlite.NewTestClockAt` (Task 2), `testCtx`, `store.(spi.Searcher).Search`, `store.GetAsAt`.
- Produces: nothing.

**What it reproduces:** Async search selects IDs via `Search` (raw `<=` PIT) and re-fetches via `GetAsAt` (pre-fix: rounded + strict `<`). At a same-millisecond boundary the select side matched one version while the re-fetch resolved a different one. After Tasks 1–2 both bind raw `<=`, so they agree. This is an isolated single-backend consistency test (not a parity scenario, per `.claude/rules/test-coverage.md`), and sqlite is the only backend where it is RED pre-fix (postgres has no sync `Searcher` until later predicate-pushdown work).

- [ ] **Step 1: Write the failing test**

Create `plugins/sqlite/pit_consistency_test.go`:

```go
package sqlite_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/plugins/sqlite"
)

// At a same-millisecond boundary, the version selected by Search (raw <=)
// must be the same version GetAsAt re-fetches. Pre-fix, Search matched v1
// (city=Berlin) at pitBase while rounded GetAsAt resolved v2 (city=Munich).
func TestSqlitePIT_SearchSelectMatchesGetAsAtRefetch(t *testing.T) {
	dir := t.TempDir()
	clock := sqlite.NewTestClockAt(pitBase) // pitBase from pit_boundary_test.go
	factory, err := sqlite.NewStoreFactoryForTest(
		context.Background(), filepath.Join(dir, "c.db"), sqlite.WithClock(clock))
	if err != nil {
		t.Fatalf("factory: %v", err)
	}
	t.Cleanup(func() { factory.Close() })

	ctx := testCtx("tenant-1")
	store, _ := factory.EntityStore(ctx)
	ref := spi.ModelRef{EntityName: "person", ModelVersion: "1"}

	e := &spi.Entity{
		Meta: spi.EntityMeta{ID: "e-c", ModelRef: ref, State: "NEW"},
		Data: []byte(`{"city":"Berlin"}`),
	}
	if _, err := store.Save(ctx, e); err != nil { // v1 @ pitBase
		t.Fatalf("save v1: %v", err)
	}
	clock.Advance(300 * time.Microsecond)
	e.Data = []byte(`{"city":"Munich"}`)
	if _, err := store.Save(ctx, e); err != nil { // v2 @ pitBase+300µs
		t.Fatalf("save v2: %v", err)
	}

	pit := pitBase
	searcher := store.(spi.Searcher)
	results, err := searcher.Search(ctx,
		spi.Filter{Op: spi.FilterEq, Path: "city", Source: spi.SourceData, Value: "Berlin"},
		spi.SearchOptions{ModelName: "person", ModelVersion: "1", PointInTime: &pit})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("Search(city=Berlin, as-at pitBase) returned %d, want 1", len(results))
	}

	// Re-fetch the selected entity at the same instant: must agree (Berlin).
	got, err := store.GetAsAt(ctx, "e-c", pit)
	if err != nil {
		t.Fatalf("GetAsAt re-fetch: %v", err)
	}
	if string(got.Data) != `{"city":"Berlin"}` {
		t.Errorf("re-fetch = %s, want {\"city\":\"Berlin\"} (select/re-fetch disagree at boundary)", got.Data)
	}
}
```

(Confirm `spi.Searcher.Search` returns a slice whose `len` is the match count, matching `searcher_test.go`'s usage; if it returns `[]spi.Entity`/IDs, adjust the `len(results)` check accordingly — the assertion that matters is the `GetAsAt` re-fetch returning Berlin.)

- [ ] **Step 2: Run and confirm it FAILS on pre-fix code**

To see it RED, you must run it against the unfixed `GetAsAt`. If Task 2 is already merged, this test passes immediately — note that in the commit message and verify it would fail by temporarily reverting the Task 2 predicate. If running before Task 2, expected: FAIL — re-fetch returns `{"city":"Munich"}`.

Run: `cd plugins/sqlite && go test ./... -run TestSqlitePIT_SearchSelect -v`

- [ ] **Step 3: Confirm it PASSES with the Task 2 fix in place**

Run: `cd plugins/sqlite && go test ./... -run TestSqlitePIT_SearchSelect -v`
Expected: PASS.

- [ ] **Step 4: Commit**

```bash
git add plugins/sqlite/pit_consistency_test.go
git commit -m "test(sqlite): async select/re-fetch agree at PIT boundary

Isolated single-backend consistency test: Search (raw <=) and GetAsAt
re-fetch resolve the same version at a same-millisecond boundary. RED on
the pre-fix rounded GetAsAt; green under the canonical inclusive bound.

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

### Task 6: Document the PIT contract + changelog

**Files:**
- Modify: `cmd/cyoda/help/content/crud.md` (add a "Point-in-time semantics" block)
- Modify: `cmd/cyoda/help/content/search.md` (cross-reference)
- Modify: `cmd/cyoda/help/content/analytics.md` (cross-reference)
- Modify: `CHANGELOG.md`

**Interfaces:** none (documentation only). No `errors/*.md` changes.

- [ ] **Step 1: Add the canonical contract block to `crud.md`**

In `cmd/cyoda/help/content/crud.md`, after the paragraph describing `pointInTime`/`transactionId` mutual exclusivity (around line 321), insert:

```markdown
## POINT-IN-TIME SEMANTICS

A `pointInTime` read returns entity state **as at exactly that instant,
inclusive**: a version whose write timestamp equals `pointInTime` is included
(`<=`), and no rounding is applied to the requested time. The bound is compared
against stored version timestamps at the storage engine's native precision.

Behaviour is identical across every read path — single-entity read, list,
search, grouped statistics, change history, and available transitions — and
across storage backends. Because backends store timestamps at different
precisions (down to milliseconds on some deployments), cross-backend results are
guaranteed to agree at **millisecond granularity**; finer-grained ordering
within a single millisecond is backend-defined. Timestamps are accepted and
emitted as RFC 3339 with full fractional precision.
```

- [ ] **Step 2: Cross-reference from `search.md` and `analytics.md`**

In `cmd/cyoda/help/content/search.md`, in the section describing `pointInTime` for search, add:

```markdown
Point-in-time search uses the canonical inclusive (`<=`, no rounding) bound —
see `cyoda help crud` ("Point-in-time semantics").
```

In `cmd/cyoda/help/content/analytics.md`, near the grouped-stats `pointInTime` description, add the same one-line cross-reference.

- [ ] **Step 3: Add a CHANGELOG entry**

In `CHANGELOG.md`, under the unreleased / v0.8.2 "Fixed" section (create the section if absent, matching the file's existing Keep-a-Changelog style):

```markdown
### Fixed
- Point-in-time ("as at T") reads now apply one canonical rule across all
  storage engines and read paths: inclusive of the requested instant (`<=`),
  compared at native precision, with no millisecond round-up. Previously the
  memory engine and the SQL `GetAsAt`/`GetAllAsAt` paths rounded the requested
  time up to the next millisecond (over-including later same-millisecond
  versions), and sqlite used a strict `<` bound — so different backends, and
  different read paths within one backend, could disagree at sub-millisecond
  boundaries.
```

- [ ] **Step 4: Verify help content renders**

Run: `go run ./cmd/cyoda help crud | sed -n '/POINT-IN-TIME/,/RFC 3339/p'`
Expected: the new section prints without template errors.

Run: `go test ./cmd/cyoda/... -run 'Help' -v`
Expected: PASS (help-topic/error-code parity tests stay green — no new error codes).

- [ ] **Step 5: Commit**

```bash
git add cmd/cyoda/help/content/crud.md cmd/cyoda/help/content/search.md cmd/cyoda/help/content/analytics.md CHANGELOG.md
git commit -m "docs: document canonical point-in-time read semantics

Inclusive-of-T, no rounding, native precision, millisecond cross-backend
fidelity floor. Documented in the crud help topic with cross-references
from search and analytics; changelog entry under v0.8.2.

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

### Task 7: Full verification before PR

**Files:** none (verification only).

- [ ] **Step 1: Run every module's tests**

Run: `make test-all`
Expected: root + `plugins/memory|sqlite|postgres` all green (Docker running for postgres + e2e testcontainers). This includes the new parity scenario across all in-tree backends.

- [ ] **Step 2: Vet all modules**

Run: `go vet ./...` (root), then `cd plugins/memory && go vet ./...`, `cd plugins/sqlite && go vet ./...`, `cd plugins/postgres && go vet ./...`
Expected: clean.

- [ ] **Step 3: Race sanity check (once, before PR)**

Run: `make race`
Expected: PASS. (Per `.claude/rules/race-testing.md` — single end-of-deliverable run, not per-step.)

- [ ] **Step 4: Confirm no stray issue IDs or round-up sites remain**

Run: `grep -rn "Truncate(time.Millisecond).Add(time.Millisecond)" plugins/ internal/ --include=*.go`
Expected: no matches (all 7 production sites removed; the grep should return nothing in non-test code — any remaining hit is a missed site).

Run: `grep -rn "349" plugins/ internal/ cmd/ e2e/ --include=*.go --include=*.md | grep -v docs/superpowers`
Expected: no issue-ID leakage into shipped code/content.

---

## Self-Review

**Spec coverage:**
- Canonical rule (inclusive `<=`, no rounding, native precision) → Tasks 1–3 (per engine).
- Remove 7 round-up sites → Task 1 (3 memory), Task 2 (2 sqlite), Task 3 (2 postgres).
- sqlite `<`→`<=` flip → Task 2.
- Already-correct paths unchanged (searcher/Iterate raw `<=`) → exercised by Task 4 parity + Task 5 consistency, not modified.
- Per-engine white-box over-inclusion / strictness tests → Tasks 1, 2, 3.
- Cross-backend exact-T inclusivity parity (incl. commercial) → Task 4.
- Async self-consistency, isolated single-backend (sqlite) → Task 5.
- Documentation (crud canonical block + search/analytics xref) + changelog → Task 6.
- Verification (test-all, vet, race, no-residue greps) → Task 7.
- Out of scope (storage-precision normalization, tx-snapshot semantics, commercial-repo change, #37 pushdown) → not present in any task. ✓

**Placeholder scan:** The only intentional placeholder is the `uuidLike`/`fmtID` indirection in Task 4 Step 1, explicitly flagged with instructions to replace it with `uuid.UUID` straight-through calls and the exact driver line refs to confirm against. All other steps contain complete code and exact commands.

**Type consistency:** `NewTestClockAt` added in memory (Task 1) and sqlite (Task 2) before use; `pitBase` defined in Task 2's `pit_boundary_test.go` and reused in Task 5 (same package `sqlite_test`); `RunPITBoundaryExactT`/`"PITBoundaryExactT"` consistent between Task 4 Steps 1 and 2; `EntityChangeMeta.TimeOfChange` and `EntityResult.Data` match `e2e/parity/client/types.go`.
