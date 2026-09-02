# #477 Plan 1 of 3 — Plugin internals (no SPI change) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Remove every whole-model materialisation inside the memory and sqlite plugins, and fix the two in-transaction write defects found beside them, without touching the SPI.

**Architecture:** sqlite gains one in-transaction overlay cursor (`readDB` committed snapshot merged with the transaction buffer through `spi.MergeOrdered`) that `Iterate`, `GetPage`, `Count` and `CountByState` all consume; `Begin` is gated behind in-flight commits so a `readDB` read at `SnapshotTime` is guaranteed to see every commit that claimed an earlier submit time. The memory plugin stops copying entity payloads on search, counts, delete-all and iteration, and records read-sets per yield. Both plugins refuse `CompareAndSave` after a same-transaction delete.

**Tech Stack:** Go 1.26, `github.com/cyoda-platform/cyoda-go-spi` at the current pin (`v0.8.4-0.20260901144642-f6863ae5e2e3`), sqlite via `ncruces/go-sqlite3`, plugin modules `plugins/memory` and `plugins/sqlite` (each its own `go.mod`; `go.work` composes them).

**Spec:** `docs/superpowers/specs/2026-09-01-477-no-search-path-materialises-design.md` — §4.1–§4.6, §10. This plan is PR 1 of §8.

## Global Constraints

- Branch: `fix/477-search-fallback` in the worktree `.claude/worktrees/fix+477-search-fallback`; PR targets `release/v0.8.4`.
- TDD: every task writes its failing test first and runs it red before the implementation.
- Mutex discipline (`.claude/rules/go-mutex-discipline.md`): every `Lock()`/`RLock()` is followed by `defer Unlock()` on the next line; early release uses an IIFE.
- Lock order sqlite: `tx.OpMu` → `commitMu` → `m.mu`; memory: `tx.OpMu` → `factory.entityMu`.
- No test-only hooks, counters or seams in production code (spec §10).
- No issue IDs in code comments, errors or logs.
- `log/slog` only.
- Run one package while iterating: `cd plugins/sqlite && go test ./...` / `cd plugins/memory && go test ./...`. End of plan: `make repin-plugins`, then `make test-full` (Docker required).
- Commit trailer on every commit:
  ```
  Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
  Claude-Session: https://claude.ai/code/session_01C8cfZpHnEMh9rXna94SYGf
  ```

---

## File map

| File | Responsibility after this plan |
|---|---|
| `plugins/sqlite/txmanager.go` | `Begin` acquires `commitMu` around its snapshot floor-and-capture (Task 1) |
| `plugins/sqlite/tx_overlay.go` (new) | `openTxOverlay`: the one in-transaction committed-plus-buffer cursor; `sqliteTxIter` (Tasks 3–5) |
| `plugins/sqlite/entity_store.go` | `unstageDelete`, `CompareAndSave` delete check (Task 2); `getPageTx` over the overlay (Task 4); in-tx `Count`/`CountByState` over the overlay (Task 5); in-tx `DeleteAll` id-only (Task 6); `getAllTx` deleted (Task 3) |
| `plugins/sqlite/grouped_stats.go` | in-tx `Iterate` over the overlay; `sqliteSliceIter` deleted (Task 3) |
| `plugins/memory/entity_store.go` | `unstageDelete`, `CompareAndSave` delete check (Task 7); in-tx `Count`/`CountByState` over pointers (Task 10); in-tx `DeleteAll` id-only (Task 11) |
| `plugins/memory/grouped_stats.go` | `buildSnapshot` records nothing; `memoryIter` records per yield (Task 8); `GroupedAggregate` records nothing (Task 8) |
| `plugins/memory/searcher.go` | pointer snapshot under the lock, survivors copied (Task 9) |
| `cmd/cyoda/help/content/crud.md` | in-tx iterator description (Task 12) |
| `CHANGELOG.md` | Fixed entries (Task 12) |

Test files are named per task. sqlite tests that need package internals (`commitMu`, `tm`) are `package sqlite`; the rest are `package sqlite_test` using the existing helpers `newAttrFactory(t)` (`attribution_test.go:26`, returns `(*sqlite.StoreFactory, spi.TransactionManager)`) and `attrCtx(tenant, user, spi.PrincipalUser)` (`attribution_test.go:16`). Memory tests are `package memory_test` using `newTxManager(t)` (`txmanager_test.go:13`, returns `(*memory.StoreFactory, *memory.TransactionManager)`) and `tenantCtx(spi.TenantID)`; where a test needs `tx` internals it is `package memory`.

---

### Task 1: sqlite `Begin` waits for an in-flight commit's flush

**Files:**
- Modify: `plugins/sqlite/txmanager.go:229-246` (`Begin`'s floor-and-capture block)
- Test: `plugins/sqlite/txmanager_begin_gate_internal_test.go` (new, `package sqlite`)

**Interfaces:**
- Consumes: `transactionManager.commitMu sync.Mutex`, `.mu sync.Mutex`, `.lastSubmitTime int64` (`txmanager.go:65-76`); `NewStoreFactoryForTest(ctx, dbPath)`; `attrInternalCtx(tenant, user, kind)` (`attribution_internal_test.go`); `StoreFactory.tm *transactionManager` (`store_factory.go:66`).
- Produces: the guarantee later tasks rely on — a transaction whose `SnapshotTime >= submitTime(X)` can read commit X's rows on `readDB`.

- [ ] **Step 1: Write the failing test**

```go
package sqlite

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// Commit bumps lastSubmitTime (step 4) before its sqlTx commits, holding
// commitMu across the whole flush. Begin floors SnapshotTime to
// lastSubmitTime. If Begin does not wait for commitMu, a transaction begun
// mid-flush has SnapshotTime >= that commit's submit time while the rows
// are not yet visible on readDB — and the conflict check at Commit then
// treats that commit as "before my snapshot" and skips it: a lost update.
//
// The test stands in for a mid-flush commit by holding commitMu and
// bumping lastSubmitTime, exactly the state Commit is in between step 4
// and sqlTx.Commit.
func TestBegin_WaitsForInFlightCommit(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "begin-gate.db")
	f, err := NewStoreFactoryForTest(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("NewStoreFactoryForTest: %v", err)
	}
	defer f.Close()
	m := f.tm
	ctx := attrInternalCtx("tenant-A", "alice", spi.PrincipalUser)

	// Stand in for a commit between step 4 and sqlTx.Commit.
	m.commitMu.Lock()
	func() {
		m.mu.Lock()
		defer m.mu.Unlock()
		m.lastSubmitTime = m.factory.clock.Now().UnixMicro() + 5_000_000 // 5s ahead
	}()
	bumped := func() int64 {
		m.mu.Lock()
		defer m.mu.Unlock()
		return m.lastSubmitTime
	}()

	type beginResult struct {
		txID string
		ctx  context.Context
		err  error
	}
	done := make(chan beginResult, 1)
	go func() {
		id, txCtx, err := m.Begin(ctx)
		done <- beginResult{id, txCtx, err}
	}()

	select {
	case r := <-done:
		m.commitMu.Unlock()
		t.Fatalf("Begin returned while a commit was in flight (txID=%q err=%v); it must wait for commitMu", r.txID, r.err)
	case <-time.After(150 * time.Millisecond):
		// Blocked, as required.
	}

	m.commitMu.Unlock()
	var r beginResult
	select {
	case r = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Begin did not return after commitMu was released")
	}
	if r.err != nil {
		t.Fatalf("Begin: %v", r.err)
	}
	defer func() { _ = m.Rollback(r.ctx, r.txID) }()

	tx := spi.GetTransaction(r.ctx)
	if got := tx.SnapshotTime.UnixMicro(); got < bumped {
		t.Fatalf("SnapshotTime %d is below the flushed commit's submit time %d", got, bumped)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `cd plugins/sqlite && go test -run TestBegin_WaitsForInFlightCommit ./...`
Expected: FAIL with "Begin returned while a commit was in flight".

- [ ] **Step 3: Gate the floor-and-capture behind `commitMu`**

In `plugins/sqlite/txmanager.go`, replace the block at `:229-246`:

```go
	// Snapshot time must be at least lastSubmitTime so that the transaction
	// sees all previously committed data. Without this floor, a monotonic
	// submit-time bump could push a commit past the next Begin's raw clock
	// value, making committed entities invisible to new transactions.
	//
	// The floor is captured under commitMu, which Commit holds from its
	// conflict check through sqlTx.Commit: Commit bumps lastSubmitTime
	// (step 4) before its rows are visible, so a Begin that read the bumped
	// value without waiting would carry a SnapshotTime at or after a commit
	// whose rows it cannot yet see on readDB — and Commit's conflict check
	// would then treat that commit as preceding the snapshot. Waiting here
	// makes "SnapshotTime >= submitTime" imply "rows visible" on every
	// connection. Lock order commitMu → mu, the order Commit uses.
	func() {
		m.commitMu.Lock()
		defer m.commitMu.Unlock()
		m.mu.Lock()
		defer m.mu.Unlock()
		if nowMicro < m.lastSubmitTime {
			nowMicro = m.lastSubmitTime
		}
		tx.SnapshotTime = time.UnixMicro(nowMicro)
		m.active[txID] = tx
	}()
```

Then confirm no path takes `m.mu` before `commitMu` (a lock-order inversion would deadlock):

Run: `grep -n "commitMu.Lock\|mu.Lock()" plugins/sqlite/txmanager.go`
Expected: every `commitMu.Lock` precedes any `m.mu.Lock` in the same function; `Rollback` and `Join` take only `m.mu`.

- [ ] **Step 4: Run the test to verify it passes**

Run: `cd plugins/sqlite && go test -run TestBegin_WaitsForInFlightCommit ./...`
Expected: PASS

- [ ] **Step 5: Run the sqlite package and the race detector on it**

Run: `cd plugins/sqlite && go test ./... && go test -race -run 'TestBegin|Commit|Concurrency' ./...`
Expected: PASS

- [ ] **Step 6: Commit**

```bash
git add plugins/sqlite/txmanager.go plugins/sqlite/txmanager_begin_gate_internal_test.go
git commit -m "fix(sqlite): Begin waits for an in-flight commit's flush before flooring its snapshot"
```

---

### Task 2: sqlite — `CompareAndSave` after a same-transaction delete conflicts; `Save` unstages the delete fully

**Files:**
- Modify: `plugins/sqlite/entity_store.go:238-262` (`Save` tx branch), `:368-393` (`CompareAndSave` tx branch)
- Test: `plugins/sqlite/tx_delete_then_write_test.go` (new, `package sqlite_test`), `plugins/sqlite/tx_delete_then_write_internal_test.go` (new, `package sqlite`)

**Interfaces:**
- Consumes: `newAttrFactory(t)`, `attrCtx(...)`, `spi.ErrConflict`, `spi.ErrNotFound`, `spi.TransactionState.Deletes / DeleteAttribution`.
- Produces: `func unstageDelete(tx *spi.TransactionState, id string)` (package-private, used by `Save`; Task 6's `DeleteAll` does not need it).

- [ ] **Step 1: Write the failing tests**

`plugins/sqlite/tx_delete_then_write_test.go`:

```go
package sqlite_test

import (
	"errors"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

func seedOne(t *testing.T, store spi.EntityStore, ctx contextT, id string, ref spi.ModelRef) *spi.Entity {
	t.Helper()
	e := &spi.Entity{
		Meta: spi.EntityMeta{ID: id, TenantID: "tenant-dtw", ModelRef: ref, State: "open"},
		Data: []byte(`{"n":1}`),
	}
	if _, err := store.Save(ctx, e); err != nil {
		t.Fatalf("seed Save: %v", err)
	}
	got, err := store.Get(ctx, id)
	if err != nil {
		t.Fatalf("seed Get: %v", err)
	}
	return got
}

// A write compares against the transaction's own view: the same-tx delete
// is the current latest, so CompareAndSave must conflict — on every backend.
// Postgres already does; sqlite looked past the buffered delete at the
// committed row and let the save through, resurrecting the entity at commit.
func TestTx_DeleteThenCompareAndSave_Conflicts(t *testing.T) {
	f, tm := newAttrFactory(t)
	ctx := attrCtx("tenant-dtw", "u1", spi.PrincipalUser)
	ref := spi.ModelRef{EntityName: "m-dtw", ModelVersion: "1"}
	store, err := f.EntityStore(ctx)
	if err != nil {
		t.Fatalf("EntityStore: %v", err)
	}
	committed := seedOne(t, store, ctx, "e-cas", ref)

	txID, txCtx, err := tm.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if err := store.Delete(txCtx, "e-cas"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	update := &spi.Entity{Meta: committed.Meta, Data: []byte(`{"n":2}`)}
	_, err = store.CompareAndSave(txCtx, update, committed.Meta.TransactionID)
	if !errors.Is(err, spi.ErrConflict) {
		t.Fatalf("CompareAndSave after same-tx Delete: err = %v, want ErrConflict", err)
	}
	if err := tm.Commit(txCtx, txID); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if _, err := store.Get(ctx, "e-cas"); !errors.Is(err, spi.ErrNotFound) {
		t.Fatalf("after commit Get: err = %v, want ErrNotFound (the delete must stand)", err)
	}
}

// Save after a same-tx Delete is last-write-wins: the entity is present after
// commit and carries the new payload — and no tombstone is written for it.
func TestTx_DeleteThenSave_CommitsPresent(t *testing.T) {
	f, tm := newAttrFactory(t)
	ctx := attrCtx("tenant-dtw", "u1", spi.PrincipalUser)
	ref := spi.ModelRef{EntityName: "m-dtw", ModelVersion: "1"}
	store, err := f.EntityStore(ctx)
	if err != nil {
		t.Fatalf("EntityStore: %v", err)
	}
	committed := seedOne(t, store, ctx, "e-save", ref)

	txID, txCtx, err := tm.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if err := store.Delete(txCtx, "e-save"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := store.Save(txCtx, &spi.Entity{Meta: committed.Meta, Data: []byte(`{"n":2}`)}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if err := tm.Commit(txCtx, txID); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	got, err := store.Get(ctx, "e-save")
	if err != nil {
		t.Fatalf("after commit Get: %v", err)
	}
	if string(got.Data) != `{"n":2}` {
		t.Fatalf("after commit Data = %s, want {\"n\":2}", got.Data)
	}
	versions, err := store.GetVersionMetadata(ctx, "e-save")
	if err != nil {
		t.Fatalf("GetVersionMetadata: %v", err)
	}
	for _, v := range versions {
		if v.ChangeType == spi.ChangeTypeDeleted {
			t.Fatalf("a DELETED version was written for an entity whose delete was unstaged: %+v", versions)
		}
	}
}
```

Add at the top of the file: `type contextT = context.Context` with `"context"` imported (keeps `seedOne`'s signature readable).

`plugins/sqlite/tx_delete_then_write_internal_test.go`:

```go
package sqlite

import (
	"context"
	"path/filepath"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// Deletes and DeleteAttribution always cover the same key set (SPI
// TransactionState doc). Save after Delete must clear both, not just Deletes.
func TestSave_AfterDelete_UnstagesBothMaps(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "unstage.db")
	f, err := NewStoreFactoryForTest(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("NewStoreFactoryForTest: %v", err)
	}
	defer f.Close()
	ctx := attrInternalCtx("tenant-A", "alice", spi.PrincipalUser)
	store, err := f.EntityStore(ctx)
	if err != nil {
		t.Fatalf("EntityStore: %v", err)
	}
	ref := spi.ModelRef{EntityName: "m-unstage", ModelVersion: "1"}
	e := &spi.Entity{Meta: spi.EntityMeta{ID: "e1", TenantID: "tenant-A", ModelRef: ref}, Data: []byte(`{}`)}
	if _, err := store.Save(ctx, e); err != nil {
		t.Fatalf("seed Save: %v", err)
	}
	txID, txCtx, err := f.tm.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer func() { _ = f.tm.Rollback(txCtx, txID) }()
	if err := store.Delete(txCtx, "e1"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	tx := spi.GetTransaction(txCtx)
	if !tx.Deletes["e1"] || len(tx.DeleteAttribution) != 1 {
		t.Fatalf("precondition: delete not staged in both maps: Deletes=%v Attribution=%v", tx.Deletes, tx.DeleteAttribution)
	}
	if _, err := store.Save(txCtx, e); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if tx.Deletes["e1"] {
		t.Errorf("tx.Deletes still holds e1 after Save")
	}
	if _, ok := tx.DeleteAttribution["e1"]; ok {
		t.Errorf("tx.DeleteAttribution still holds e1 after Save")
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `cd plugins/sqlite && go test -run 'TestTx_DeleteThen|TestSave_AfterDelete' ./...`
Expected: `TestTx_DeleteThenCompareAndSave_Conflicts` FAILS ("want ErrConflict"); `TestSave_AfterDelete_UnstagesBothMaps` FAILS ("DeleteAttribution still holds e1"); `TestTx_DeleteThenSave_CommitsPresent` passes or fails on the tombstone assertion — either way it is committed with the fix.

- [ ] **Step 3: Implement `unstageDelete` and the CAS check**

In `plugins/sqlite/entity_store.go`, add near `copyEntity`:

```go
// unstageDelete removes a staged delete for id from BOTH maps the delete
// occupies. Deletes and DeleteAttribution always cover the same key set
// (spi.TransactionState); clearing only one leaves a phantom attribution
// entry a savepoint restore would carry back in.
func unstageDelete(tx *spi.TransactionState, id string) {
	delete(tx.Deletes, id)
	delete(tx.DeleteAttribution, id)
}
```

In `Save`'s tx branch replace

```go
		// If the entity was previously marked for deletion in this tx, unmark it.
		delete(tx.Deletes, entity.Meta.ID)
```

with

```go
		// If the entity was previously marked for deletion in this tx, unmark
		// it (last-write-wins: Save after Delete → present).
		unstageDelete(tx, entity.Meta.ID)
```

In `CompareAndSave`'s tx branch, before the "Check CAS against committed store" query, insert:

```go
		// A write compares against the transaction's own view. A same-tx
		// delete is the current latest state of this entity, so a
		// compare-and-save against it cannot succeed: the caller's expected
		// transaction ID names a version this transaction has already
		// superseded. Same answer postgres gives, where the delete is
		// applied at once.
		if tx.Deletes[entity.Meta.ID] {
			return 0, spi.ErrConflict
		}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `cd plugins/sqlite && go test -run 'TestTx_DeleteThen|TestSave_AfterDelete' ./...`
Expected: PASS (all three)

- [ ] **Step 5: Run the sqlite package**

Run: `cd plugins/sqlite && go test ./...`
Expected: PASS

- [ ] **Step 6: Commit**

```bash
git add plugins/sqlite/entity_store.go plugins/sqlite/tx_delete_then_write_test.go plugins/sqlite/tx_delete_then_write_internal_test.go
git commit -m "fix(sqlite): a compare-and-save after a same-transaction delete conflicts; Save unstages the delete fully"
```

---

### Task 3: sqlite in-transaction `Iterate` streams through one overlay cursor

**Files:**
- Create: `plugins/sqlite/tx_overlay.go`
- Modify: `plugins/sqlite/grouped_stats.go:88-111` (in-tx branch), delete `sqliteSliceIter` (`:164-210`); `plugins/sqlite/entity_store.go` delete `getAllTx` (`:573-616`) once Task 5 lands — in this task `getAllTx` keeps its two remaining callers (`GetAll`, `Count`/`CountByState`).
- Test: `plugins/sqlite/tx_overlay_test.go` (new, `package sqlite_test`)

**Interfaces:**
- Consumes: `planFor(filter)` (`query_planner.go`; returns a plan with `.where string`, `.args []any`, `.preparedPostFilter *spi.PreparedFilter`), `evaluateFilter(pf, e)`, `s.searchSnapshotBase(opts, snapshotMicro)` (`searcher.go:185`), `scanVersionEntity(rows)`, `sortEntitiesByOrder(ctx, rows, nil)`, `spi.MergeOrdered`, `spi.ErrTxAlreadyCommitted`, `tx.Closed`.
- Produces (used by Tasks 4 and 5):

```go
// txOverlayProjection selects which columns the committed cursor reads.
type txOverlayProjection int

const (
	projectFull    txOverlayProjection = iota // entity_id, model, version, data, meta, submit_time
	projectIDState                            // entity_id and meta state only — no payload bytes
)

// txOverlay is the merged (committed ∪ buffer − deletes) pull-stream for one
// model inside one transaction. The caller must hold tx.OpMu.RLock while
// calling openTxOverlay; the returned stream may be pulled without the lock.
type txOverlay struct {
	pull func() (*spi.Entity, bool, error)
	rows *sql.Rows
}

func (s *entityStore) openTxOverlay(ctx context.Context, tx *spi.TransactionState, modelRef spi.ModelRef, filter spi.Filter, proj txOverlayProjection) (*txOverlay, error)
func (o *txOverlay) Close() error

// sqliteTxIter adapts a txOverlay to spi.Iterator, recording yields into the
// read-set when trackingRead is set.
type sqliteTxIter struct { /* fields below */ }
```

- [ ] **Step 1: Write the failing tests**

`plugins/sqlite/tx_overlay_test.go`:

```go
package sqlite_test

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

func seedN(t *testing.T, store spi.EntityStore, ctx context.Context, ref spi.ModelRef, n int) []string {
	t.Helper()
	ids := make([]string, n)
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("e%02d", i)
		ids[i] = id
		if _, err := store.Save(ctx, &spi.Entity{
			Meta: spi.EntityMeta{ID: id, TenantID: "tenant-ovl", ModelRef: ref, State: []string{"open", "closed"}[i%2]},
			Data: []byte(fmt.Sprintf(`{"i":%d}`, i)),
		}); err != nil {
			t.Fatalf("seed Save(%s): %v", id, err)
		}
	}
	return ids
}

func drainIDs(t *testing.T, it spi.Iterator) []string {
	t.Helper()
	var ids []string
	for it.Next() {
		ids = append(ids, it.Entity().Meta.ID)
	}
	if err := it.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := it.Err(); err != nil {
		t.Fatalf("Err: %v", err)
	}
	return ids
}

func iterable(t *testing.T, store spi.EntityStore) spi.Iterable {
	t.Helper()
	it, ok := store.(spi.Iterable)
	if !ok {
		t.Fatal("store is not spi.Iterable")
	}
	return it
}

// The in-tx iterator yields the committed snapshot merged with the buffer,
// minus staged deletes, in entity-ID order — the same set getAllTx produced,
// now as one cursor.
func TestTxIterate_MergedViewInIDOrder(t *testing.T) {
	f, tm := newAttrFactory(t)
	ctx := attrCtx("tenant-ovl", "u1", spi.PrincipalUser)
	ref := spi.ModelRef{EntityName: "m-ovl", ModelVersion: "1"}
	store, _ := f.EntityStore(ctx)
	seedN(t, store, ctx, ref, 6) // e00..e05

	txID, txCtx, err := tm.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer func() { _ = tm.Rollback(txCtx, txID) }()
	if err := store.Delete(txCtx, "e01"); err != nil { // staged delete
		t.Fatalf("Delete: %v", err)
	}
	if _, err := store.Save(txCtx, &spi.Entity{ // buffered add, sorts first
		Meta: spi.EntityMeta{ID: "a00", TenantID: "tenant-ovl", ModelRef: ref, State: "open"},
		Data: []byte(`{"i":100}`),
	}); err != nil {
		t.Fatalf("Save add: %v", err)
	}
	if _, err := store.Save(txCtx, &spi.Entity{ // buffered update shadows e03
		Meta: spi.EntityMeta{ID: "e03", TenantID: "tenant-ovl", ModelRef: ref, State: "open"},
		Data: []byte(`{"i":303}`),
	}); err != nil {
		t.Fatalf("Save update: %v", err)
	}

	it, err := iterable(t, store).Iterate(txCtx, ref, spi.Filter{}, spi.IterateOptions{})
	if err != nil {
		t.Fatalf("Iterate: %v", err)
	}
	var got []string
	var e03Data string
	for it.Next() {
		e := it.Entity()
		got = append(got, e.Meta.ID)
		if e.Meta.ID == "e03" {
			e03Data = string(e.Data)
		}
	}
	_ = it.Close()
	if err := it.Err(); err != nil {
		t.Fatalf("Err: %v", err)
	}
	want := []string{"a00", "e00", "e02", "e03", "e04", "e05"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("ids = %v, want %v", got, want)
	}
	if e03Data != `{"i":303}` {
		t.Fatalf("e03 came from the committed row (%s); the buffered version must win", e03Data)
	}
}

// The filter is pushed into the committed query and applied to the buffer:
// a buffered entity that does not match must not be yielded, and a committed
// row shadowed by a non-matching buffered write must not be yielded either.
func TestTxIterate_FilterAppliesToBothStreams(t *testing.T) {
	f, tm := newAttrFactory(t)
	ctx := attrCtx("tenant-ovl", "u1", spi.PrincipalUser)
	ref := spi.ModelRef{EntityName: "m-ovl-f", ModelVersion: "1"}
	store, _ := f.EntityStore(ctx)
	seedN(t, store, ctx, ref, 4) // e00 open, e01 closed, e02 open, e03 closed

	txID, txCtx, _ := tm.Begin(ctx)
	defer func() { _ = tm.Rollback(txCtx, txID) }()
	// e02 was open; the buffered write closes it → must drop out.
	_, _ = store.Save(txCtx, &spi.Entity{Meta: spi.EntityMeta{ID: "e02", TenantID: "tenant-ovl", ModelRef: ref, State: "closed"}, Data: []byte(`{}`)})
	// new open entity in the buffer → must appear.
	_, _ = store.Save(txCtx, &spi.Entity{Meta: spi.EntityMeta{ID: "z00", TenantID: "tenant-ovl", ModelRef: ref, State: "open"}, Data: []byte(`{}`)})

	filter := spi.Filter{Op: spi.FilterEquals, Path: "state", Source: spi.SourceMeta, Value: "open"}
	it, err := iterable(t, store).Iterate(txCtx, ref, filter, spi.IterateOptions{})
	if err != nil {
		t.Fatalf("Iterate: %v", err)
	}
	got := drainIDs(t, it)
	want := []string{"e00", "z00"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("ids = %v, want %v", got, want)
	}
}

// TrackingRead records only yielded (post-residual) committed ids — never the
// rows the filter excluded, and never buffered own-writes.
func TestTxIterate_TrackingReadRecordsYieldsOnly(t *testing.T) {
	f, tm := newAttrFactory(t)
	ctx := attrCtx("tenant-ovl", "u1", spi.PrincipalUser)
	ref := spi.ModelRef{EntityName: "m-ovl-tr", ModelVersion: "1"}
	store, _ := f.EntityStore(ctx)
	seedN(t, store, ctx, ref, 4)

	txID, txCtx, _ := tm.Begin(ctx)
	defer func() { _ = tm.Rollback(txCtx, txID) }()
	_, _ = store.Save(txCtx, &spi.Entity{Meta: spi.EntityMeta{ID: "z00", TenantID: "tenant-ovl", ModelRef: ref, State: "open"}, Data: []byte(`{}`)})

	filter := spi.Filter{Op: spi.FilterEquals, Path: "state", Source: spi.SourceMeta, Value: "open"}
	it, err := iterable(t, store).Iterate(txCtx, ref, filter, spi.IterateOptions{TrackingRead: true})
	if err != nil {
		t.Fatalf("Iterate: %v", err)
	}
	_ = drainIDs(t, it)

	tx := spi.GetTransaction(txCtx)
	var recorded []string
	for id := range tx.ReadSet {
		recorded = append(recorded, id)
	}
	sort.Strings(recorded)
	want := []string{"e00", "e02"}
	if fmt.Sprint(recorded) != fmt.Sprint(want) {
		t.Fatalf("ReadSet = %v, want %v (yielded committed ids only)", recorded, want)
	}
}

// A same-tx Delete while the iterator is open must not deadlock (the cursor
// reads from readDB, never the single writer connection).
func TestTxIterate_SameTxDeleteWhileOpen_NoDeadlock(t *testing.T) {
	f, tm := newAttrFactory(t)
	ctx := attrCtx("tenant-ovl", "u1", spi.PrincipalUser)
	ref := spi.ModelRef{EntityName: "m-ovl-dl", ModelVersion: "1"}
	store, _ := f.EntityStore(ctx)
	seedN(t, store, ctx, ref, 5)

	txID, txCtx, _ := tm.Begin(ctx)
	defer func() { _ = tm.Rollback(txCtx, txID) }()
	it, err := iterable(t, store).Iterate(txCtx, ref, spi.Filter{}, spi.IterateOptions{})
	if err != nil {
		t.Fatalf("Iterate: %v", err)
	}
	if !it.Next() {
		t.Fatalf("first Next: false, err=%v", it.Err())
	}
	done := make(chan error, 1)
	go func() { done <- store.Delete(txCtx, "e04") }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Delete while iterator open: %v", err)
		}
	case <-timeoutCh(t):
		t.Fatal("Delete deadlocked behind the open iterator")
	}
	_ = drainIDs(t, it)
}

// Commit while an iterator is open ends the iteration with
// ErrTxAlreadyCommitted rather than recording into a closed transaction.
func TestTxIterate_CommitWhileOpen_EndsWithAlreadyCommitted(t *testing.T) {
	f, tm := newAttrFactory(t)
	ctx := attrCtx("tenant-ovl", "u1", spi.PrincipalUser)
	ref := spi.ModelRef{EntityName: "m-ovl-cm", ModelVersion: "1"}
	store, _ := f.EntityStore(ctx)
	seedN(t, store, ctx, ref, 3)

	txID, txCtx, _ := tm.Begin(ctx)
	it, err := iterable(t, store).Iterate(txCtx, ref, spi.Filter{}, spi.IterateOptions{TrackingRead: true})
	if err != nil {
		t.Fatalf("Iterate: %v", err)
	}
	if !it.Next() {
		t.Fatalf("first Next: false, err=%v", it.Err())
	}
	if err := tm.Commit(txCtx, txID); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	for it.Next() {
	}
	_ = it.Close()
	if err := it.Err(); !errors.Is(err, spi.ErrTxAlreadyCommitted) {
		t.Fatalf("Err after commit = %v, want ErrTxAlreadyCommitted", err)
	}
}
```

Add a small helper at the bottom of the file:

```go
func timeoutCh(t *testing.T) <-chan time.Time {
	t.Helper()
	return time.After(5 * time.Second)
}
```

with `"time"` imported. Check the exact filter field names against the SPI (`filter.go`: `Filter{Op, Path, Source, Value}` and the `FilterEquals` constant name) with `grep -n "FilterEquals\|^type Filter struct" -A12 /Users/paul/go-projects/cyoda-light/cyoda-go-spi/filter.go` before running; adjust the literal if a name differs.

- [ ] **Step 2: Run the tests to verify they fail**

Run: `cd plugins/sqlite && go test -run 'TestTxIterate_' ./...`
Expected: `MergedViewInIDOrder` FAILS on order (map order today) or passes by luck — run with `-count=3` to see it flap; `TrackingReadRecordsYieldsOnly` FAILS (pre-residual recording records e01/e03 too); `SameTxDeleteWhileOpen` passes today (the slice iterator holds no connection) and must keep passing; `CommitWhileOpen` FAILS (no `ErrTxAlreadyCommitted`).

- [ ] **Step 3: Create `tx_overlay.go`**

```go
package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// txOverlayProjection selects which columns the committed cursor reads.
type txOverlayProjection int

const (
	// projectFull reads the whole row: id, model, version, data, meta, submit_time.
	projectFull txOverlayProjection = iota
	// projectIDState reads entity_id and the meta state only — no payload
	// bytes cross the driver. Used by the in-tx counts.
	projectIDState
)

// txOverlay is the merged (committed snapshot ∪ transaction buffer − staged
// deletes) pull-stream for one model inside one transaction. It is the ONE
// in-transaction read path: Iterate, GetPage, Count and CountByState all
// consume it, so they cannot disagree on what the transaction sees.
//
// The committed cursor runs on readDB, never on the single writer connection:
// a second statement on the writer while a cursor is open would deadlock,
// and the SPI forbids holding a write-blocking lock for an iterator's
// lifetime. Reading committed rows at tx.SnapshotTime on readDB is correct
// because Begin waits for any in-flight commit's flush before flooring the
// snapshot (txmanager.go, Begin) — so every commit with submit_time <=
// SnapshotTime is visible on every connection.
//
// The buffer and the delete set are copied into locals at open, under the
// tx.OpMu.RLock the caller holds: the overlay is a snapshot at call time
// (spi.Iterable contract), so later mutation of the transaction does not
// change what an already-open stream yields.
type txOverlay struct {
	pull func() (*spi.Entity, bool, error)
	rows *sql.Rows
}

// openTxOverlay opens the stream. Caller holds tx.OpMu.RLock and has checked
// tx.RolledBack. Entities are yielded in byte-wise entity-ID order.
func (s *entityStore) openTxOverlay(ctx context.Context, tx *spi.TransactionState, modelRef spi.ModelRef, filter spi.Filter, proj txOverlayProjection) (*txOverlay, error) {
	plan, err := planFor(filter)
	if err != nil {
		return nil, err
	}
	// planFor's success already ran spi.Prepare on this filter; this call
	// only obtains the prepared value for the buffer side.
	pf, _ := spi.Prepare(filter)

	opts := spi.SearchOptions{ModelName: modelRef.EntityName, ModelVersion: modelRef.ModelVersion}
	var query string
	var args []any
	switch proj {
	case projectIDState:
		query, args = s.snapshotIDStateBase(opts, timeToMicro(tx.SnapshotTime))
	default:
		query, args = s.searchSnapshotBase(opts, timeToMicro(tx.SnapshotTime))
	}
	if plan.where != "" {
		query += " AND (" + plan.where + ")"
		args = append(args, plan.args...)
	}
	query += " ORDER BY ev.entity_id"

	// Snapshot the transaction's view: buffered adds for this model that
	// match, and the set of ids whose committed row must be suppressed —
	// staged deletes plus every buffered id (the buffered version, if it
	// matches, arrives through adds; if it does not match, the committed
	// row must not stand in for it).
	adds := make([]*spi.Entity, 0, len(tx.Buffer))
	suppressed := make(map[string]struct{}, len(tx.Buffer)+len(tx.Deletes))
	for id := range tx.Deletes {
		suppressed[id] = struct{}{}
	}
	for id, e := range tx.Buffer {
		if e.Meta.ModelRef != modelRef {
			continue
		}
		suppressed[id] = struct{}{}
		if _, del := tx.Deletes[id]; del {
			continue
		}
		if pf.Match(e.Data, e.Meta) {
			if proj == projectIDState {
				adds = append(adds, &spi.Entity{Meta: spi.EntityMeta{ID: e.Meta.ID, State: e.Meta.State, ModelRef: e.Meta.ModelRef}})
			} else {
				adds = append(adds, copyEntity(e))
			}
		}
	}
	if err := sortEntitiesByOrder(ctx, adds, nil); err != nil {
		return nil, err
	}

	rows, err := s.readDB.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("tx overlay query: %w", err)
	}

	scanned := 0
	next := func() (*spi.Entity, bool, error) {
		for rows.Next() {
			if scanned&1023 == 0 {
				if err := ctx.Err(); err != nil {
					return nil, false, err
				}
			}
			scanned++
			var e *spi.Entity
			var scanErr error
			if proj == projectIDState {
				e, scanErr = scanIDState(rows, modelRef)
			} else {
				e, scanErr = scanVersionEntity(rows)
			}
			if scanErr != nil {
				return nil, false, scanErr
			}
			if proj == projectFull && plan.preparedPostFilter != nil && !evaluateFilter(*plan.preparedPostFilter, e) {
				continue
			}
			return e, true, nil
		}
		if err := rows.Err(); err != nil {
			if cErr := ctx.Err(); cErr != nil {
				return nil, false, cErr
			}
			return nil, false, fmt.Errorf("row iteration: %w", err)
		}
		return nil, false, nil
	}
	isSuppressed := func(id string) bool { _, ok := suppressed[id]; return ok }
	cmp := func(a, b *spi.Entity) int { return strings.Compare(a.Meta.ID, b.Meta.ID) }

	return &txOverlay{
		pull: spi.MergeOrdered(next, adds, isSuppressed, cmp),
		rows: rows,
	}, nil
}

// Close releases the committed cursor. Idempotent.
func (o *txOverlay) Close() error {
	if o.rows == nil {
		return nil
	}
	rows := o.rows
	o.rows = nil
	return rows.Close()
}

// snapshotIDStateBase is searchSnapshotBase's projection twin: the same
// latest-version-at-snapshot join, selecting only the entity id and the
// meta state. The alias "ev" is kept so plan.where fragments apply unchanged.
func (s *entityStore) snapshotIDStateBase(opts spi.SearchOptions, snapshotMicro int64) (string, []any) {
	query := `SELECT ev.entity_id, json_extract(ev.meta, '$.state')
	          FROM entity_versions ev
	          INNER JOIN (
	              SELECT entity_id, MAX(version) AS max_ver
	              FROM entity_versions
	              WHERE tenant_id = ? AND model_name = ? AND model_version = ? AND submit_time <= ?
	              GROUP BY entity_id
	          ) latest ON ev.entity_id = latest.entity_id AND ev.version = latest.max_ver
	          WHERE ev.tenant_id = ? AND ev.change_type != 'DELETED'`
	args := []any{string(s.tenantID), opts.ModelName, opts.ModelVersion, snapshotMicro, string(s.tenantID)}
	return query, args
}

func scanIDState(row interface{ Scan(...any) error }, modelRef spi.ModelRef) (*spi.Entity, error) {
	var id string
	var state sql.NullString
	if err := row.Scan(&id, &state); err != nil {
		return nil, fmt.Errorf("scan id/state: %w", err)
	}
	return &spi.Entity{Meta: spi.EntityMeta{ID: id, State: state.String, ModelRef: modelRef}}, nil
}

// sqliteTxIter adapts a txOverlay to spi.Iterator. With trackingRead, each
// yielded COMMITTED entity is recorded into the read-set under a short
// tx.OpMu.RLock that also checks the transaction is still open: Commit takes
// OpMu.Lock between two yields, and an iterator must not record into a
// transaction that has since closed.
type sqliteTxIter struct {
	ctx          context.Context
	tx           *spi.TransactionState
	overlay      *txOverlay
	trackingRead bool
	bufferedIDs  map[string]struct{} // own-writes are never read-set entries
	cur          *spi.Entity
	err          error
	closed       bool
}

func (it *sqliteTxIter) Next() bool {
	if it.err != nil || it.closed {
		return false
	}
	e, ok, err := it.overlay.pull()
	if err != nil {
		it.err = err
		return false
	}
	if !ok {
		return false
	}
	if it.trackingRead {
		if _, buffered := it.bufferedIDs[e.Meta.ID]; !buffered {
			if err := it.record(e.Meta.ID); err != nil {
				it.err = err
				return false
			}
		}
	}
	it.cur = e
	return true
}

func (it *sqliteTxIter) record(id string) error {
	it.tx.OpMu.RLock()
	defer it.tx.OpMu.RUnlock()
	if it.tx.RolledBack {
		return fmt.Errorf("Iterate: %w (txID=%s)", spi.ErrTxRolledBack, it.tx.ID)
	}
	if it.tx.Closed {
		return fmt.Errorf("Iterate: %w (txID=%s)", spi.ErrTxAlreadyCommitted, it.tx.ID)
	}
	it.tx.ReadSet[id] = true
	return nil
}

func (it *sqliteTxIter) Entity() *spi.Entity { return it.cur }
func (it *sqliteTxIter) Err() error          { return it.err }

func (it *sqliteTxIter) Close() error {
	if it.closed {
		return nil
	}
	it.closed = true
	it.cur = nil
	return it.overlay.Close()
}
```

Check `tx.Closed`'s exact field name in the SPI (`grep -n "Closed" /Users/paul/go-projects/cyoda-light/cyoda-go-spi/txcontext.go`) and `sortEntitiesByOrder`'s signature (`searcher.go:~208`) before compiling.

- [ ] **Step 4: Route the in-tx `Iterate` branch through the overlay**

In `plugins/sqlite/grouped_stats.go`, replace the in-tx branch (`:93-111`):

```go
	// In-tx, non-PIT: one overlay cursor (committed snapshot on readDB merged
	// with the buffer, deletes suppressed), residual applied inside the
	// stream — never a materialised merged view. See tx_overlay.go.
	if tx != nil && opts.PointInTime == nil {
		return s.iterateTx(ctx, tx, model, filter, opts.TrackingRead)
	}
```

and add to `tx_overlay.go`:

```go
// iterateTx opens the overlay under tx.OpMu.RLock (held only for the open —
// the stream is pulled lock-free) and returns the iterator over it.
func (s *entityStore) iterateTx(ctx context.Context, tx *spi.TransactionState, model spi.ModelRef, filter spi.Filter, trackingRead bool) (spi.Iterator, error) {
	var overlay *txOverlay
	var bufferedIDs map[string]struct{}
	err := func() error {
		tx.OpMu.RLock()
		defer tx.OpMu.RUnlock()
		if tx.RolledBack {
			return fmt.Errorf("Iterate: %w (txID=%s)", spi.ErrTxRolledBack, tx.ID)
		}
		if tx.Closed {
			return fmt.Errorf("Iterate: %w (txID=%s)", spi.ErrTxAlreadyCommitted, tx.ID)
		}
		bufferedIDs = make(map[string]struct{}, len(tx.Buffer))
		for id := range tx.Buffer {
			bufferedIDs[id] = struct{}{}
		}
		var oErr error
		overlay, oErr = s.openTxOverlay(ctx, tx, model, filter, projectFull)
		if oErr != nil {
			return fmt.Errorf("Iterate: %w", oErr)
		}
		return nil
	}()
	if err != nil {
		return nil, err
	}
	return &sqliteTxIter{ctx: ctx, tx: tx, overlay: overlay, trackingRead: trackingRead, bufferedIDs: bufferedIDs}, nil
}
```

Delete `sqliteSliceIter` and its methods (`grouped_stats.go:164-210`). Update the `Iterate` doc comment (`:57-61`): replace "the iterator materializes via getAllTx — the same overlay … used by GetAll's in-tx branch" with "the iterator streams the same overlay through one cursor (tx_overlay.go)".

- [ ] **Step 5: Run the tests to verify they pass**

Run: `cd plugins/sqlite && go test -run 'TestTxIterate_' -count=3 ./...`
Expected: PASS

- [ ] **Step 6: Run the sqlite package and the SPI conformance suite it embeds**

Run: `cd plugins/sqlite && go test ./...`
Expected: PASS (`conformance_test.go` runs spitest, including `Iterable/*`).

- [ ] **Step 7: Commit**

```bash
git add plugins/sqlite/tx_overlay.go plugins/sqlite/grouped_stats.go plugins/sqlite/tx_overlay_test.go
git commit -m "fix(sqlite): in-transaction Iterate streams one overlay cursor instead of materialising the merged view"
```

---

### Task 4: sqlite in-transaction `GetPage` pages the same overlay

**Files:**
- Modify: `plugins/sqlite/entity_store.go:1159-1262` (`getPageTx` and its doc comment)
- Test: `plugins/sqlite/get_page_tx_overlay_test.go` (new, `package sqlite_test`)

**Interfaces:**
- Consumes: `openTxOverlay(ctx, tx, modelRef, spi.Filter{}, projectFull)` (Task 3), `pageSlice` is no longer needed for the tx path.
- Produces: unchanged `GetPage` contract (page in canonical ID order, every returned entity recorded unconditionally).

- [ ] **Step 1: Write the failing test**

```go
package sqlite_test

import (
	"fmt"
	"sort"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// Every in-tx page equals the corresponding slice of the full in-tx Iterate
// sequence, and only the returned page is recorded into the read-set.
func TestGetPageTx_PagesEqualIterateSlices_RecordsPageOnly(t *testing.T) {
	f, tm := newAttrFactory(t)
	ctx := attrCtx("tenant-ovl", "u1", spi.PrincipalUser)
	ref := spi.ModelRef{EntityName: "m-pg-ovl", ModelVersion: "1"}
	store, _ := f.EntityStore(ctx)
	seedN(t, store, ctx, ref, 12) // e00..e11

	txID, txCtx, _ := tm.Begin(ctx)
	defer func() { _ = tm.Rollback(txCtx, txID) }()
	for _, id := range []string{"e00", "e05", "e06"} {
		if err := store.Delete(txCtx, id); err != nil {
			t.Fatalf("Delete %s: %v", id, err)
		}
	}
	_, _ = store.Save(txCtx, &spi.Entity{Meta: spi.EntityMeta{ID: "b00", TenantID: "tenant-ovl", ModelRef: ref}, Data: []byte(`{}`)})
	_, _ = store.Save(txCtx, &spi.Entity{Meta: spi.EntityMeta{ID: "e07", TenantID: "tenant-ovl", ModelRef: ref}, Data: []byte(`{"u":1}`)})

	it, err := iterable(t, store).Iterate(txCtx, ref, spi.Filter{}, spi.IterateOptions{})
	if err != nil {
		t.Fatalf("Iterate: %v", err)
	}
	full := drainIDs(t, it) // b00 e01 e02 e03 e04 e07 e08 e09 e10 e11

	tx := spi.GetTransaction(txCtx)
	for k := range tx.ReadSet {
		delete(tx.ReadSet, k)
	}
	const limit = 4
	for offset := 0; offset < len(full)+limit; offset += limit {
		page, err := store.GetPage(txCtx, ref, limit, offset, nil)
		if err != nil {
			t.Fatalf("GetPage(offset=%d): %v", offset, err)
		}
		hi := offset + limit
		if hi > len(full) {
			hi = len(full)
		}
		var want []string
		if offset < len(full) {
			want = full[offset:hi]
		}
		if got := pageIDs(t, page); fmt.Sprint(got) != fmt.Sprint(want) {
			t.Fatalf("page(offset=%d) = %v, want %v", offset, got, want)
		}
	}
	var recorded []string
	for id := range tx.ReadSet {
		recorded = append(recorded, id)
	}
	sort.Strings(recorded)
	// Every page was read, so every non-buffered id was recorded once; buffered
	// own-writes are not read-set entries.
	var wantRecorded []string
	for _, id := range full {
		if id != "b00" && id != "e07" {
			wantRecorded = append(wantRecorded, id)
		}
	}
	if fmt.Sprint(recorded) != fmt.Sprint(wantRecorded) {
		t.Fatalf("ReadSet = %v, want %v", recorded, wantRecorded)
	}
}
```

`pageIDs` exists in `get_page_tx_deletes_test.go`; `seedN`, `iterable`, `drainIDs` from Task 3's test file (same package).

Note the read-set expectation: today `getPageTx` records buffered own-writes too (`:1257-1260` records every entity on the page). The SPI `GetPage` doc says "every entity on the returned page is recorded"; own-writes are already in the write-set and `Search`/`Iterate` exclude them. Keep the current unconditional behaviour to avoid a contract change in this PR: **record every entity on the page, buffered included** — change `wantRecorded` to `full` in the test above. (This sentence is the decision; the test asserts `full`.)

- [ ] **Step 2: Run the test to verify it fails**

Run: `cd plugins/sqlite && go test -run TestGetPageTx_PagesEqualIterateSlices ./...`
Expected: PASS or FAIL — today's `getPageTx` produces the same pages by a different route. If it passes, keep the test: it pins the equivalence the rewrite must preserve. Proceed.

- [ ] **Step 3: Rewrite `getPageTx` over the overlay**

Replace `getPageTx` (`entity_store.go:1159-1262`, doc comment included) with:

```go
// getPageTx is the in-tx, asAt==nil path: the same overlay cursor Iterate
// uses (tx_overlay.go), pulled offset+limit times. The first offset merged
// rows are discarded as they are pulled, so peak memory is one page plus the
// buffered adds — never the committed prefix. Every entity on the returned
// page is recorded into the read-set unconditionally (GetPage's SPI
// contract; it has no TrackingRead knob).
func (s *entityStore) getPageTx(ctx context.Context, tx *spi.TransactionState, modelRef spi.ModelRef, limit, offset int) ([]*spi.Entity, error) {
	tx.OpMu.RLock()
	defer tx.OpMu.RUnlock()
	if tx.RolledBack {
		return nil, fmt.Errorf("GetPage: %w (txID=%s)", spi.ErrTxRolledBack, tx.ID)
	}
	if tx.Closed {
		return nil, fmt.Errorf("GetPage: %w (txID=%s)", spi.ErrTxAlreadyCommitted, tx.ID)
	}
	overlay, err := s.openTxOverlay(ctx, tx, modelRef, spi.Filter{}, projectFull)
	if err != nil {
		return nil, fmt.Errorf("GetPage: %w", err)
	}
	defer overlay.Close()

	for skipped := 0; skipped < offset; skipped++ {
		_, ok, err := overlay.pull()
		if err != nil {
			return nil, fmt.Errorf("GetPage: %w", err)
		}
		if !ok {
			return []*spi.Entity{}, nil
		}
	}
	page := make([]*spi.Entity, 0, limit)
	for len(page) < limit {
		e, ok, err := overlay.pull()
		if err != nil {
			return nil, fmt.Errorf("GetPage: %w", err)
		}
		if !ok {
			break
		}
		page = append(page, e)
	}
	for _, e := range page {
		tx.ReadSet[e.Meta.ID] = true
	}
	return page, nil
}
```

The whole call runs under `tx.OpMu.RLock` (a page is bounded work; the lock is not held across caller iteration).

- [ ] **Step 4: Run the tests to verify they pass**

Run: `cd plugins/sqlite && go test -run 'TestGetPageTx' ./...`
Expected: PASS (including the existing `get_page_tx_deletes_test.go`).

- [ ] **Step 5: Run the sqlite package**

Run: `cd plugins/sqlite && go test ./...`
Expected: PASS

- [ ] **Step 6: Commit**

```bash
git add plugins/sqlite/entity_store.go plugins/sqlite/get_page_tx_overlay_test.go
git commit -m "fix(sqlite): in-transaction GetPage pages the overlay cursor instead of draining offset+limit rows"
```

---

### Task 5: sqlite in-transaction `Count` / `CountByState` tally the overlay; `getAllTx` deleted

**Files:**
- Modify: `plugins/sqlite/entity_store.go:880-889` (`Count` tx branch), `:963-983` (`CountByState` tx branch), delete `getAllTx` (`:573-616`) and route `GetAll`'s tx branch (`:533-542`) through the overlay for the remainder of this PR (the method itself is removed in Plan 2/3).
- Test: `plugins/sqlite/tx_count_overlay_test.go` (new, `package sqlite_test`)

**Interfaces:**
- Consumes: `openTxOverlay(..., projectIDState)` (Task 3).
- Produces: nothing new.

- [ ] **Step 1: Write the failing test**

```go
package sqlite_test

import (
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// In-tx counts reflect the transaction's own view for every buffer shape:
// a create, an update, a delete of a committed entity, a create then delete,
// a delete then Save, a state change, and after DeleteAll.
func TestTxCount_EveryBufferShape(t *testing.T) {
	f, tm := newAttrFactory(t)
	ctx := attrCtx("tenant-ovl", "u1", spi.PrincipalUser)
	ref := spi.ModelRef{EntityName: "m-cnt", ModelVersion: "1"}
	store, _ := f.EntityStore(ctx)
	seedN(t, store, ctx, ref, 4) // e00 open, e01 closed, e02 open, e03 closed

	txID, txCtx, _ := tm.Begin(ctx)
	defer func() { _ = tm.Rollback(txCtx, txID) }()
	save := func(id, state string) {
		t.Helper()
		if _, err := store.Save(txCtx, &spi.Entity{Meta: spi.EntityMeta{ID: id, TenantID: "tenant-ovl", ModelRef: ref, State: state}, Data: []byte(`{}`)}); err != nil {
			t.Fatalf("Save %s: %v", id, err)
		}
	}
	del := func(id string) {
		t.Helper()
		if err := store.Delete(txCtx, id); err != nil {
			t.Fatalf("Delete %s: %v", id, err)
		}
	}
	check := func(step string, wantTotal int64, wantByState map[string]int64) {
		t.Helper()
		got, err := store.Count(txCtx, ref)
		if err != nil {
			t.Fatalf("%s: Count: %v", step, err)
		}
		if got != wantTotal {
			t.Fatalf("%s: Count = %d, want %d", step, got, wantTotal)
		}
		by, err := store.CountByState(txCtx, ref, nil)
		if err != nil {
			t.Fatalf("%s: CountByState: %v", step, err)
		}
		if len(by) != len(wantByState) {
			t.Fatalf("%s: CountByState = %v, want %v", step, by, wantByState)
		}
		for st, n := range wantByState {
			if by[st] != n {
				t.Fatalf("%s: CountByState[%s] = %d, want %d (%v)", step, st, by[st], n, by)
			}
		}
	}

	check("baseline", 4, map[string]int64{"open": 2, "closed": 2})
	save("n00", "open")          // create
	check("create", 5, map[string]int64{"open": 3, "closed": 2})
	save("e01", "open")          // update + state change closed→open
	check("update", 5, map[string]int64{"open": 4, "closed": 1})
	del("e02")                   // delete committed
	check("delete committed", 4, map[string]int64{"open": 3, "closed": 1})
	save("n01", "closed")
	del("n01")                   // create then delete
	check("create then delete", 4, map[string]int64{"open": 3, "closed": 1})
	del("e03")
	save("e03", "open")          // delete then Save → present, open
	check("delete then save", 4, map[string]int64{"open": 4})
	if err := store.DeleteAll(txCtx, ref); err != nil {
		t.Fatalf("DeleteAll: %v", err)
	}
	check("after DeleteAll", 0, map[string]int64{})
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `cd plugins/sqlite && go test -run TestTxCount_EveryBufferShape ./...`
Expected: PASS today (the materialising path is correct, only expensive). Keep it: it pins the answers the rewrite must preserve.

- [ ] **Step 3: Rewrite the two tx branches and delete `getAllTx`**

Add to `tx_overlay.go`:

```go
// countTx tallies the overlay with the id/state projection: no payload
// bytes are read, and the answer is by construction the view Iterate and
// GetPage return. Caller holds tx.OpMu.RLock.
func (s *entityStore) countTx(ctx context.Context, tx *spi.TransactionState, modelRef spi.ModelRef, tally func(state string)) error {
	overlay, err := s.openTxOverlay(ctx, tx, modelRef, spi.Filter{}, projectIDState)
	if err != nil {
		return err
	}
	defer overlay.Close()
	for {
		e, ok, err := overlay.pull()
		if err != nil {
			return err
		}
		if !ok {
			return nil
		}
		tally(e.Meta.State)
	}
}
```

`Count`'s tx branch becomes:

```go
	if tx != nil {
		tx.OpMu.RLock()
		defer tx.OpMu.RUnlock()
		if tx.RolledBack {
			return 0, fmt.Errorf("Count: %w (txID=%s)", spi.ErrTxRolledBack, tx.ID)
		}
		var n int64
		if err := s.countTx(ctx, tx, modelRef, func(string) { n++ }); err != nil {
			return 0, fmt.Errorf("Count: %w", err)
		}
		return n, nil
	}
```

`CountByState`'s tx branch becomes:

```go
	if tx != nil {
		tx.OpMu.RLock()
		defer tx.OpMu.RUnlock()
		if tx.RolledBack {
			return nil, fmt.Errorf("CountByState: %w (txID=%s)", spi.ErrTxRolledBack, tx.ID)
		}
		var filter map[string]struct{}
		if states != nil {
			filter = make(map[string]struct{}, len(states))
			for _, st := range states {
				filter[st] = struct{}{}
			}
		}
		result := make(map[string]int64)
		err := s.countTx(ctx, tx, modelRef, func(st string) {
			if filter != nil {
				if _, ok := filter[st]; !ok {
					return
				}
			}
			result[st]++
		})
		if err != nil {
			return nil, fmt.Errorf("CountByState: %w", err)
		}
		return result, nil
	}
```

`GetAll`'s tx branch (`:533-542`) — the method survives until Plan 3 — drains the overlay:

```go
	if tx != nil {
		tx.OpMu.RLock()
		defer tx.OpMu.RUnlock()
		if tx.RolledBack {
			return nil, fmt.Errorf("GetAll: %w (txID=%s)", spi.ErrTxRolledBack, tx.ID)
		}
		overlay, err := s.openTxOverlay(ctx, tx, modelRef, spi.Filter{}, projectFull)
		if err != nil {
			return nil, fmt.Errorf("GetAll: %w", err)
		}
		defer overlay.Close()
		var all []*spi.Entity
		for {
			e, ok, err := overlay.pull()
			if err != nil {
				return nil, fmt.Errorf("GetAll: %w", err)
			}
			if !ok {
				break
			}
			// GetAll records unconditionally (no TrackingRead knob).
			tx.ReadSet[e.Meta.ID] = true
			all = append(all, e)
		}
		return all, nil
	}
```

Delete `getAllTx` (`:573-616`) and fix the `trackRead` doc comment above it. Update the `CountByState` doc comment (`:952-955`) — "In-tx callers fall back to GetAll-then-count-in-Go" → "In-tx callers tally the overlay cursor with an id/state projection (tx_overlay.go)".

- [ ] **Step 4: Run the tests to verify they pass**

Run: `cd plugins/sqlite && go test -run 'TestTxCount|TestTxIterate|TestGetPageTx' ./... && grep -n "getAllTx" plugins/sqlite/*.go`
Expected: PASS; the grep finds only comments if any (fix them) — no code references.

- [ ] **Step 5: Run the sqlite package**

Run: `cd plugins/sqlite && go test ./...`
Expected: PASS

- [ ] **Step 6: Commit**

```bash
git add plugins/sqlite/entity_store.go plugins/sqlite/tx_overlay.go plugins/sqlite/tx_count_overlay_test.go
git commit -m "fix(sqlite): in-transaction counts tally the overlay cursor with an id/state projection; getAllTx removed"
```

---

### Task 6: sqlite in-transaction `DeleteAll` stages ids without reading payloads

**Files:**
- Modify: `plugins/sqlite/entity_store.go:753-800` (`DeleteAll` tx branch)
- Test: `plugins/sqlite/tx_delete_all_test.go` (new, `package sqlite_test`)

- [ ] **Step 1: Write the failing test**

```go
package sqlite_test

import (
	"errors"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// In-tx DeleteAll stages every committed id of the model and every buffered
// id, and after commit none of them exist. (Behavioural pin for the
// id-only rewrite; the rewrite itself is reviewed, not measured.)
func TestTxDeleteAll_StagesCommittedAndBuffered(t *testing.T) {
	f, tm := newAttrFactory(t)
	ctx := attrCtx("tenant-ovl", "u1", spi.PrincipalUser)
	ref := spi.ModelRef{EntityName: "m-dall", ModelVersion: "1"}
	other := spi.ModelRef{EntityName: "m-keep", ModelVersion: "1"}
	store, _ := f.EntityStore(ctx)
	seedN(t, store, ctx, ref, 3)
	if _, err := store.Save(ctx, &spi.Entity{Meta: spi.EntityMeta{ID: "k00", TenantID: "tenant-ovl", ModelRef: other}, Data: []byte(`{}`)}); err != nil {
		t.Fatalf("seed other: %v", err)
	}

	txID, txCtx, _ := tm.Begin(ctx)
	_, _ = store.Save(txCtx, &spi.Entity{Meta: spi.EntityMeta{ID: "b00", TenantID: "tenant-ovl", ModelRef: ref}, Data: []byte(`{}`)})
	if err := store.DeleteAll(txCtx, ref); err != nil {
		t.Fatalf("DeleteAll: %v", err)
	}
	if n, _ := store.Count(txCtx, ref); n != 0 {
		t.Fatalf("in-tx Count after DeleteAll = %d, want 0", n)
	}
	if err := tm.Commit(txCtx, txID); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	for _, id := range []string{"e00", "e01", "e02", "b00"} {
		if _, err := store.Get(ctx, id); !errors.Is(err, spi.ErrNotFound) {
			t.Fatalf("Get(%s) after commit: err = %v, want ErrNotFound", id, err)
		}
	}
	if _, err := store.Get(ctx, "k00"); err != nil {
		t.Fatalf("other model's entity must survive: %v", err)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `cd plugins/sqlite && go test -run TestTxDeleteAll_StagesCommittedAndBuffered ./...`
Expected: PASS today. Keep it as the behavioural pin.

- [ ] **Step 3: Rewrite the tx branch to select ids only**

Replace the query and loop in the tx branch (`:761-798`) with:

```go
		// Stage every committed id of the model visible at the snapshot —
		// ids only, no payload bytes. Reads on readDB (see tx_overlay.go
		// for why an in-tx snapshot read on readDB is correct).
		snapshotMicro := timeToMicro(tx.SnapshotTime)
		rows, err := s.readDB.QueryContext(ctx,
			`SELECT ev.entity_id
			 FROM entity_versions ev
			 INNER JOIN (
			     SELECT entity_id, MAX(version) AS max_ver
			     FROM entity_versions
			     WHERE tenant_id = ? AND model_name = ? AND model_version = ? AND submit_time <= ?
			     GROUP BY entity_id
			 ) latest ON ev.entity_id = latest.entity_id AND ev.version = latest.max_ver
			 WHERE ev.tenant_id = ? AND ev.change_type != 'DELETED'`,
			string(s.tenantID), modelRef.EntityName, modelRef.ModelVersion, snapshotMicro,
			string(s.tenantID))
		if err != nil {
			return fmt.Errorf("query snapshot ids for deleteAll: %w", err)
		}
		defer rows.Close()

		a, e := spi.AttributionFor(ctx)
		attribution := spi.WriteAttribution{Attributed: a, Executor: e}

		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				return fmt.Errorf("scan id for deleteAll: %w", err)
			}
			tx.Deletes[id] = true
			delete(tx.Buffer, id)
			tx.WriteSet[id] = true
			tx.DeleteAttribution[id] = attribution
		}
		if err := rows.Err(); err != nil {
			return fmt.Errorf("row iteration: %w", err)
		}
```

Keep the "Also delete any buffered entities for this model" block that follows unchanged.

- [ ] **Step 4: Run the tests to verify they pass**

Run: `cd plugins/sqlite && go test -run 'TestTxDeleteAll|TestTxCount' ./...`
Expected: PASS

- [ ] **Step 5: Run the sqlite package**

Run: `cd plugins/sqlite && go test ./...`
Expected: PASS

- [ ] **Step 6: Commit**

```bash
git add plugins/sqlite/entity_store.go plugins/sqlite/tx_delete_all_test.go
git commit -m "fix(sqlite): in-transaction DeleteAll stages ids without reading payloads"
```

---

### Task 7: memory — `CompareAndSave` after a same-transaction delete conflicts; `Save` unstages fully

**Files:**
- Modify: `plugins/memory/entity_store.go:172-200` (`Save` tx branch), `:200-245` (`CompareAndSave` tx branch)
- Test: `plugins/memory/tx_delete_then_write_test.go` (new, `package memory_test`), `plugins/memory/tx_delete_then_write_internal_test.go` (new, `package memory`)

**Interfaces:**
- Consumes: `newTxManager(t)` (`txmanager_test.go:13`), `tenantCtx(spi.TenantID)`; in-package: `NewStoreFactory()`, `StoreFactory.txManager`.
- Produces: `func unstageDelete(tx *spi.TransactionState, id string)` in `plugins/memory`.

- [ ] **Step 1: Write the failing tests**

`plugins/memory/tx_delete_then_write_test.go` — same two scenarios as Task 2, memory fixture:

```go
package memory_test

import (
	"errors"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

func TestTx_DeleteThenCompareAndSave_Conflicts(t *testing.T) {
	f, tm := newTxManager(t)
	ctx := tenantCtx("tenant-dtw")
	ref := spi.ModelRef{EntityName: "m-dtw", ModelVersion: "1"}
	store, err := f.EntityStore(ctx)
	if err != nil {
		t.Fatalf("EntityStore: %v", err)
	}
	if _, err := store.Save(ctx, &spi.Entity{Meta: spi.EntityMeta{ID: "e-cas", TenantID: "tenant-dtw", ModelRef: ref}, Data: []byte(`{"n":1}`)}); err != nil {
		t.Fatalf("seed Save: %v", err)
	}
	committed, err := store.Get(ctx, "e-cas")
	if err != nil {
		t.Fatalf("seed Get: %v", err)
	}

	txID, txCtx, err := tm.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if err := store.Delete(txCtx, "e-cas"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	_, err = store.CompareAndSave(txCtx, &spi.Entity{Meta: committed.Meta, Data: []byte(`{"n":2}`)}, committed.Meta.TransactionID)
	if !errors.Is(err, spi.ErrConflict) {
		t.Fatalf("CompareAndSave after same-tx Delete: err = %v, want ErrConflict", err)
	}
	if err := tm.Commit(txCtx, txID); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if _, err := store.Get(ctx, "e-cas"); !errors.Is(err, spi.ErrNotFound) {
		t.Fatalf("after commit Get: err = %v, want ErrNotFound", err)
	}
}

func TestTx_DeleteThenSave_CommitsPresent(t *testing.T) {
	f, tm := newTxManager(t)
	ctx := tenantCtx("tenant-dtw")
	ref := spi.ModelRef{EntityName: "m-dtw", ModelVersion: "1"}
	store, _ := f.EntityStore(ctx)
	if _, err := store.Save(ctx, &spi.Entity{Meta: spi.EntityMeta{ID: "e-save", TenantID: "tenant-dtw", ModelRef: ref}, Data: []byte(`{"n":1}`)}); err != nil {
		t.Fatalf("seed Save: %v", err)
	}
	committed, _ := store.Get(ctx, "e-save")

	txID, txCtx, _ := tm.Begin(ctx)
	if err := store.Delete(txCtx, "e-save"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := store.Save(txCtx, &spi.Entity{Meta: committed.Meta, Data: []byte(`{"n":2}`)}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if err := tm.Commit(txCtx, txID); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	got, err := store.Get(ctx, "e-save")
	if err != nil {
		t.Fatalf("after commit Get: %v", err)
	}
	if string(got.Data) != `{"n":2}` {
		t.Fatalf("Data = %s, want {\"n\":2}", got.Data)
	}
	versions, err := store.GetVersionMetadata(ctx, "e-save")
	if err != nil {
		t.Fatalf("GetVersionMetadata: %v", err)
	}
	for _, v := range versions {
		if v.ChangeType == spi.ChangeTypeDeleted {
			t.Fatalf("a DELETED version was written for an unstaged delete: %+v", versions)
		}
	}
}
```

`plugins/memory/tx_delete_then_write_internal_test.go`:

```go
package memory

import (
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

func TestSave_AfterDelete_UnstagesBothMaps(t *testing.T) {
	f := NewStoreFactory()
	defer f.Close()
	ctx := tenantCtxInternal("tenant-A")
	store, err := f.EntityStore(ctx)
	if err != nil {
		t.Fatalf("EntityStore: %v", err)
	}
	ref := spi.ModelRef{EntityName: "m-unstage", ModelVersion: "1"}
	e := &spi.Entity{Meta: spi.EntityMeta{ID: "e1", TenantID: "tenant-A", ModelRef: ref}, Data: []byte(`{}`)}
	if _, err := store.Save(ctx, e); err != nil {
		t.Fatalf("seed Save: %v", err)
	}
	tm, _ := f.TransactionManager(ctx)
	txID, txCtx, err := tm.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer func() { _ = tm.Rollback(txCtx, txID) }()
	if err := store.Delete(txCtx, "e1"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	tx := spi.GetTransaction(txCtx)
	if !tx.Deletes["e1"] || len(tx.DeleteAttribution) != 1 {
		t.Fatalf("precondition: Deletes=%v Attribution=%v", tx.Deletes, tx.DeleteAttribution)
	}
	if _, err := store.Save(txCtx, e); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if tx.Deletes["e1"] {
		t.Errorf("tx.Deletes still holds e1 after Save")
	}
	if _, ok := tx.DeleteAttribution["e1"]; ok {
		t.Errorf("tx.DeleteAttribution still holds e1 after Save")
	}
}
```

Check for an existing in-package tenant-context helper: `grep -n "func tenantCtx\|func .*Ctx(" plugins/memory/*_internal_test*.go plugins/memory/path_validation_internal_test.go`. If none, add `tenantCtxInternal` to the new file:

```go
func tenantCtxInternal(tenant string) context.Context {
	return spi.WithUserContext(context.Background(), &spi.UserContext{
		UserID: "u1", Kind: spi.PrincipalUser,
		Tenant: spi.Tenant{ID: spi.TenantID(tenant), Name: tenant},
	})
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `cd plugins/memory && go test -run 'TestTx_DeleteThen|TestSave_AfterDelete' ./...`
Expected: `Conflicts` FAILS ("want ErrConflict"); `UnstagesBothMaps` FAILS ("DeleteAttribution still holds e1").

- [ ] **Step 3: Implement**

Add near `copyEntity` in `plugins/memory/entity_store.go`:

```go
// unstageDelete removes a staged delete for id from BOTH maps the delete
// occupies (Deletes and DeleteAttribution always cover the same key set).
func unstageDelete(tx *spi.TransactionState, id string) {
	delete(tx.Deletes, id)
	delete(tx.DeleteAttribution, id)
}
```

In `Save`'s tx branch replace `delete(tx.Deletes, entity.Meta.ID)` with `unstageDelete(tx, entity.Meta.ID)` (keep the surrounding comment, replacing "Keeps tx.Buffer and tx.Deletes mutually exclusive" with "Keeps tx.Buffer and tx.Deletes/DeleteAttribution mutually exclusive").

In `CompareAndSave`'s tx branch, after the `RolledBack` check and before the IIFE, insert:

```go
		// A write compares against the transaction's own view: a same-tx
		// delete is the current latest state, so a compare-and-save against
		// it conflicts. Same answer postgres gives.
		if tx.Deletes[entity.Meta.ID] {
			return 0, spi.ErrConflict
		}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `cd plugins/memory && go test -run 'TestTx_DeleteThen|TestSave_AfterDelete' ./...`
Expected: PASS

- [ ] **Step 5: Run the memory package**

Run: `cd plugins/memory && go test ./...`
Expected: PASS

- [ ] **Step 6: Commit**

```bash
git add plugins/memory/entity_store.go plugins/memory/tx_delete_then_write_test.go plugins/memory/tx_delete_then_write_internal_test.go
git commit -m "fix(memory): a compare-and-save after a same-transaction delete conflicts; Save unstages the delete fully"
```

---

### Task 8: memory `Iterate` records per yield; `GroupedAggregate` records nothing

**Files:**
- Modify: `plugins/memory/grouped_stats.go:81`, `:117-183` (`buildSnapshot` signature and tx branch), `:236-290` (`memoryIter`), `:355-357` (`GroupedAggregate` snapshot call)
- Test: `plugins/memory/iterate_tracking_test.go` (new, `package memory_test`)

**Interfaces:**
- Consumes: `spi.GroupedAggregator` interface (`GroupedAggregate(ctx, model, filter, opts)`), `spi.GroupedAggregateOptions` (check the exact type name with `grep -n "GroupedAggregate(" /Users/paul/go-projects/cyoda-light/cyoda-go-spi/grouped_aggregator.go`).
- Produces: `buildSnapshot(ctx, model, pit)` (no `trackingRead` parameter); `memoryIter{tx *spi.TransactionState, trackingRead bool, bufferedIDs map[string]struct{}}`.

- [ ] **Step 1: Write the failing tests**

```go
package memory_test

import (
	"fmt"
	"sort"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

func seedStates(t *testing.T, store spi.EntityStore, ctx contextT, ref spi.ModelRef, states ...string) {
	t.Helper()
	for i, st := range states {
		if _, err := store.Save(ctx, &spi.Entity{
			Meta: spi.EntityMeta{ID: fmt.Sprintf("e%02d", i), TenantID: "tenant-it", ModelRef: ref, State: st},
			Data: []byte(`{}`),
		}); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
}

func readSetIDs(tx *spi.TransactionState) []string {
	var ids []string
	for id := range tx.ReadSet {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// In-tx Iterate with TrackingRead records only the committed ids it yields —
// not the whole merged model at open.
func TestIterate_InTx_TrackingReadRecordsYieldsOnly(t *testing.T) {
	f, tm := newTxManager(t)
	ctx := tenantCtx("tenant-it")
	ref := spi.ModelRef{EntityName: "m-it", ModelVersion: "1"}
	store, _ := f.EntityStore(ctx)
	seedStates(t, store, ctx, ref, "open", "closed", "open", "closed")

	txID, txCtx, _ := tm.Begin(ctx)
	defer func() { _ = tm.Rollback(txCtx, txID) }()
	_, _ = store.Save(txCtx, &spi.Entity{Meta: spi.EntityMeta{ID: "z00", TenantID: "tenant-it", ModelRef: ref, State: "open"}, Data: []byte(`{}`)})

	it, err := store.(spi.Iterable).Iterate(txCtx, ref, spi.Filter{Op: spi.FilterEquals, Path: "state", Source: spi.SourceMeta, Value: "open"}, spi.IterateOptions{TrackingRead: true})
	if err != nil {
		t.Fatalf("Iterate: %v", err)
	}
	tx := spi.GetTransaction(txCtx)
	if got := readSetIDs(tx); len(got) != 0 {
		t.Fatalf("ReadSet populated at open: %v; must record per yield", got)
	}
	var yielded []string
	for it.Next() {
		yielded = append(yielded, it.Entity().Meta.ID)
	}
	_ = it.Close()
	if err := it.Err(); err != nil {
		t.Fatalf("Err: %v", err)
	}
	sort.Strings(yielded)
	if fmt.Sprint(yielded) != fmt.Sprint([]string{"e00", "e02", "z00"}) {
		t.Fatalf("yielded = %v", yielded)
	}
	if got := readSetIDs(tx); fmt.Sprint(got) != fmt.Sprint([]string{"e00", "e02"}) {
		t.Fatalf("ReadSet = %v, want [e00 e02] (yielded committed ids only)", got)
	}
}

// In-tx GroupedAggregate records nothing — the rule every backend shares.
func TestGroupedAggregate_InTx_RecordsNothing(t *testing.T) {
	f, tm := newTxManager(t)
	ctx := tenantCtx("tenant-it")
	ref := spi.ModelRef{EntityName: "m-ga", ModelVersion: "1"}
	store, _ := f.EntityStore(ctx)
	seedStates(t, store, ctx, ref, "open", "closed")

	txID, txCtx, _ := tm.Begin(ctx)
	defer func() { _ = tm.Rollback(txCtx, txID) }()
	ga := store.(spi.GroupedAggregator)
	_, err := ga.GroupedAggregate(txCtx, ref, spi.Filter{}, spi.GroupedAggregateOptions{
		GroupBy:    []spi.GroupExpr{{Source: spi.SourceMeta, Path: "state"}},
		MaxBuckets: 10,
	})
	if err != nil {
		t.Fatalf("GroupedAggregate: %v", err)
	}
	if got := readSetIDs(spi.GetTransaction(txCtx)); len(got) != 0 {
		t.Fatalf("GroupedAggregate recorded %v into the read-set; must record nothing", got)
	}
}
```

Add `type contextT = context.Context` with the import. Check `GroupExpr`/`GroupedAggregateOptions` field names in the SPI before running and adjust the literal.

- [ ] **Step 2: Run the tests to verify they fail**

Run: `cd plugins/memory && go test -run 'TestIterate_InTx_TrackingRead|TestGroupedAggregate_InTx' ./...`
Expected: both FAIL ("ReadSet populated at open" / "recorded … into the read-set").

- [ ] **Step 3: Implement**

`buildSnapshot`: drop the `trackingRead` parameter and the two `tx.ReadSet[...] = true` lines in the tx branch; update its doc comment (remove the "trackingRead gates…" paragraph; add "Recording is the iterator's job, per yield — see memoryIter.record"). Update the `Iterate` call to `s.buildSnapshot(ctx, model, opts.PointInTime)` and the `GroupedAggregate` call to `s.buildSnapshot(ctx, model, opts.PointInTime)`; replace the comment above the latter with:

```go
	// In-transaction grouped stats records nothing into the read-set — the
	// rule every backend shares (sqlite and postgres record nothing; the
	// engine passes no TrackingRead for stats either).
```

`memoryIter` gains fields and per-yield recording:

```go
type memoryIter struct {
	snapshot     []*spi.Entity
	prepared     spi.PreparedFilter
	ctx          context.Context
	tx           *spi.TransactionState // nil outside a transaction
	trackingRead bool
	bufferedIDs  map[string]struct{}
	idx          int
	cur          *spi.Entity
	err          error
	closed       bool
}

func (it *memoryIter) Next() bool {
	if it.err != nil || it.closed {
		return false
	}
	if err := it.ctx.Err(); err != nil {
		it.err = err
		return false
	}
	for it.idx < len(it.snapshot) {
		e := it.snapshot[it.idx]
		it.idx++
		if !it.prepared.Match(e.Data, e.Meta) {
			continue
		}
		if it.tx != nil && it.trackingRead {
			if _, buffered := it.bufferedIDs[e.Meta.ID]; !buffered {
				if err := it.record(e.Meta.ID); err != nil {
					it.err = err
					return false
				}
			}
		}
		it.cur = e
		return true
	}
	return false
}

// record enters a yielded committed id into the read-set under a short
// tx.OpMu.RLock, refusing once the transaction has closed: Commit takes
// OpMu.Lock between two yields, and an open iterator must not record into a
// committed transaction.
func (it *memoryIter) record(id string) error {
	it.tx.OpMu.RLock()
	defer it.tx.OpMu.RUnlock()
	if it.tx.RolledBack {
		return fmt.Errorf("Iterate: %w (txID=%s)", spi.ErrTxRolledBack, it.tx.ID)
	}
	if it.tx.Closed {
		return fmt.Errorf("Iterate: %w (txID=%s)", spi.ErrTxAlreadyCommitted, it.tx.ID)
	}
	it.tx.ReadSet[id] = true
	return nil
}
```

In `Iterate`, build the iterator with the transaction and the buffered-id set captured at open (under the same `tx.OpMu.RLock` `buildSnapshot` holds — simplest: capture `bufferedIDs` inside `buildSnapshot`'s tx branch and return it as a second value; signature `buildSnapshot(ctx, model, pit) ([]*spi.Entity, map[string]struct{}, error)`, `GroupedAggregate` ignores the second value):

```go
	snapshot, bufferedIDs, err := s.buildSnapshot(ctx, model, opts.PointInTime)
	if err != nil {
		return nil, err
	}
	...
	return &memoryIter{
		snapshot:     snapshot,
		prepared:     prepared,
		ctx:          ctx,
		tx:           spi.GetTransaction(ctx),
		trackingRead: opts.TrackingRead,
		bufferedIDs:  bufferedIDs,
	}, nil
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `cd plugins/memory && go test -run 'TestIterate_InTx_TrackingRead|TestGroupedAggregate_InTx' ./...`
Expected: PASS

- [ ] **Step 5: Run the memory package (spitest `Iterable/TrackingRead*` included)**

Run: `cd plugins/memory && go test ./...`
Expected: PASS

- [ ] **Step 6: Commit**

```bash
git add plugins/memory/grouped_stats.go plugins/memory/iterate_tracking_test.go
git commit -m "fix(memory): Iterate records the read-set per yield; grouped stats records nothing in a transaction"
```

---

### Task 9: memory `Search` filters over pointers and copies only survivors

**Files:**
- Modify: `plugins/memory/searcher.go:58-100` (non-tx and in-tx PIT branches), `:100-208` (RYW branch), `:216-236` (`currentStateMatchesUnlocked`), `:245-270` (`matchSortBounded`)
- Test: `plugins/memory/searcher_survivors_test.go` (new, `package memory_test`)

**Interfaces:**
- Consumes: `getAllSnapshotPointersUnlocked(modelRef, snapshotTime)` (`grouped_stats.go:209`).
- Produces: `currentStatePointersUnlocked(ctx, modelRef)` replacing `currentStateMatchesUnlocked`; `matchSortBounded` copies on append.

- [ ] **Step 1: Write the failing test**

The property (no payload copied for non-survivors) is not observable; the behavioural pin is that results stay correct and that returned entities do not alias store state:

```go
package memory_test

import (
	"fmt"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

func searcher(t *testing.T, store spi.EntityStore) spi.Searcher {
	t.Helper()
	s, ok := store.(spi.Searcher)
	if !ok {
		t.Fatal("store is not spi.Searcher")
	}
	return s
}

// Returned entities are copies: mutating a result must not change what the
// store returns next. Covers all three branches (non-tx, in-tx PIT, in-tx RYW).
func TestSearch_ResultsDoNotAliasStore(t *testing.T) {
	f, tm := newTxManager(t)
	ctx := tenantCtx("tenant-srch")
	ref := spi.ModelRef{EntityName: "m-alias", ModelVersion: "1"}
	store, _ := f.EntityStore(ctx)
	for i := 0; i < 3; i++ {
		if _, err := store.Save(ctx, &spi.Entity{Meta: spi.EntityMeta{ID: fmt.Sprintf("e%d", i), TenantID: "tenant-srch", ModelRef: ref, State: "open"}, Data: []byte(`{"v":1}`)}); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	opts := spi.SearchOptions{ModelName: ref.EntityName, ModelVersion: ref.ModelVersion, Limit: 10}
	filter := spi.Filter{Op: spi.FilterEquals, Path: "state", Source: spi.SourceMeta, Value: "open"}

	mutateAndRecheck := func(name string, c contextT, o spi.SearchOptions) {
		t.Helper()
		first, err := searcher(t, store).Search(c, filter, o)
		if err != nil || len(first) != 3 {
			t.Fatalf("%s: Search = %d entities, err=%v", name, len(first), err)
		}
		first[0].Data[5] = '9' // {"v":9}
		first[0].Meta.State = "mutated"
		again, err := searcher(t, store).Search(c, filter, o)
		if err != nil || len(again) != 3 {
			t.Fatalf("%s: second Search = %d entities, err=%v (a mutated result leaked into the store)", name, len(again), err)
		}
		for _, e := range again {
			if string(e.Data) != `{"v":1}` || e.Meta.State != "open" {
				t.Fatalf("%s: store state changed through a returned entity: %s %s", name, e.Meta.ID, e.Data)
			}
		}
	}

	mutateAndRecheck("non-tx", ctx, opts)

	txID, txCtx, _ := tm.Begin(ctx)
	defer func() { _ = tm.Rollback(txCtx, txID) }()
	_, _ = store.Save(txCtx, &spi.Entity{Meta: spi.EntityMeta{ID: "e1", TenantID: "tenant-srch", ModelRef: ref, State: "open"}, Data: []byte(`{"v":1}`)}) // buffered update
	mutateAndRecheck("in-tx RYW", txCtx, opts)

	pit := spi.GetTransaction(txCtx).SnapshotTime
	pitOpts := opts
	pitOpts.PointInTime = &pit
	mutateAndRecheck("in-tx PIT", txCtx, pitOpts)
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `cd plugins/memory && go test -run TestSearch_ResultsDoNotAliasStore ./...`
Expected: PASS today (everything is copied). Keep it: it is the pin the pointer rewrite must not break.

- [ ] **Step 3: Rewrite the three branches**

Replace `currentStateMatchesUnlocked` with a pointer variant:

```go
// currentStatePointersUnlocked returns the latest non-deleted versions
// matching modelRef as store pointers — no copy. Stored *spi.Entity values
// are immutable after publish (saveUnlocked and the commit flush build a
// fresh entity through copyEntity), so filtering over them lock-free is
// safe; only survivors are copied, by matchSortBounded. Caller must hold at
// least s.factory.entityMu.RLock().
func (s *EntityStore) currentStatePointersUnlocked(ctx context.Context, modelRef spi.ModelRef) ([]*spi.Entity, error) {
	result := make([]*spi.Entity, 0)
	i := 0
	for _, versions := range s.factory.entityData[s.tenant] {
		if i&1023 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		i++
		if len(versions) == 0 {
			continue
		}
		latest := versions[len(versions)-1]
		if latest.deleted {
			continue
		}
		if latest.entity.Meta.ModelRef == modelRef {
			result = append(result, latest.entity)
		}
	}
	return result, nil
}
```

In `Search`: the non-tx branch uses `s.getAllSnapshotPointersUnlocked(modelRef, *opts.PointInTime)` (wrap: it returns no error) or `s.currentStatePointersUnlocked(ctx, modelRef)`; the in-tx PIT branch uses `getAllSnapshotPointersUnlocked`; the RYW branch's `committed` comes from `getAllSnapshotPointersUnlocked(modelRef, tx.SnapshotTime)`. `getAllSnapshotUnlocked` (the copying variant, `entity_store.go:125`) keeps its other callers until Tasks 10–11 remove them.

`matchSortBounded` copies on append:

```go
		if pf.Match(e.Data, e.Meta) {
			filtered = append(filtered, copyEntity(e))
			if len(filtered) > limit {
				return nil, fmt.Errorf("search: more than %d matches: %w", limit, spi.ErrSearchResultLimitExceeded)
			}
		}
```

In the RYW branch, `filteredCommitted` holds pointers; after `spi.MergeBounded` returns `page`, copy the committed survivors (buffered adds were already copied):

```go
	for i, e := range page {
		if _, buffered := tx.Buffer[e.Meta.ID]; !buffered {
			page[i] = copyEntity(e)
		}
	}
```

Place this before the read-set recording loop. Update the `Search` doc comment's "copyEntity happens inside getAllSnapshotUnlocked, so no raw store pointer escapes the lock" to "the snapshot is pointers; survivors are copied before they are returned, so no raw store pointer escapes".

- [ ] **Step 4: Run the tests to verify they pass**

Run: `cd plugins/memory && go test -run 'TestSearch' -race ./...`
Expected: PASS (race-clean: pointers are read after the lock is released, which the immutability invariant permits).

- [ ] **Step 5: Run the memory package**

Run: `cd plugins/memory && go test ./...`
Expected: PASS (spitest `Searcher/*` included).

- [ ] **Step 6: Commit**

```bash
git add plugins/memory/searcher.go plugins/memory/searcher_survivors_test.go
git commit -m "fix(memory): Search filters over store pointers and copies only the survivors"
```

---

### Task 10: memory in-transaction `Count` / `CountByState` tally pointers

**Files:**
- Modify: `plugins/memory/entity_store.go:782-791` (`Count` tx branch), `:829-846` (`CountByState` tx branch)
- Test: `plugins/memory/tx_count_test.go` (new, `package memory_test`)

- [ ] **Step 1: Write the failing test**

Same scenario as Task 5's `TestTxCount_EveryBufferShape`, on the memory fixture (`newTxManager`, `tenantCtx("tenant-cnt")`, seed four entities with states open/closed/open/closed using `seedStates` from Task 8's file). Copy the body verbatim, replacing the fixture lines.

- [ ] **Step 2: Run the test to verify it fails**

Run: `cd plugins/memory && go test -run TestTxCount_EveryBufferShape ./...`
Expected: PASS today. Keep it as the pin.

- [ ] **Step 3: Rewrite the two tx branches**

Add a shared helper:

```go
// countTx tallies the transaction's view of modelRef without copying: the
// committed pointer snapshot at tx.SnapshotTime minus staged deletes, plus
// buffered own-writes of the model. Caller holds tx.OpMu.RLock.
func (s *EntityStore) countTx(ctx context.Context, tx *spi.TransactionState, modelRef spi.ModelRef, tally func(state string)) error {
	var committed []*spi.Entity
	func() {
		s.factory.entityMu.RLock()
		defer s.factory.entityMu.RUnlock()
		committed = s.getAllSnapshotPointersUnlocked(modelRef, tx.SnapshotTime)
	}()
	for i, e := range committed {
		if i&1023 == 0 {
			if err := ctx.Err(); err != nil {
				return err
			}
		}
		if tx.Deletes[e.Meta.ID] {
			continue
		}
		if _, buffered := tx.Buffer[e.Meta.ID]; buffered {
			continue // the buffered version is tallied below
		}
		tally(e.Meta.State)
	}
	for id, e := range tx.Buffer {
		if e.Meta.ModelRef != modelRef || tx.Deletes[id] {
			continue
		}
		tally(e.Meta.State)
	}
	return nil
}
```

`Count`'s tx branch:

```go
	if tx != nil {
		tx.OpMu.RLock()
		defer tx.OpMu.RUnlock()
		if tx.RolledBack {
			return 0, fmt.Errorf("Count: %w (txID=%s)", spi.ErrTxRolledBack, tx.ID)
		}
		var n int64
		if err := s.countTx(ctx, tx, modelRef, func(string) { n++ }); err != nil {
			return 0, fmt.Errorf("Count: %w", err)
		}
		return n, nil
	}
```

`CountByState`'s tx branch (the `filter` map is already built above it):

```go
	if tx != nil {
		tx.OpMu.RLock()
		defer tx.OpMu.RUnlock()
		if tx.RolledBack {
			return nil, fmt.Errorf("CountByState: %w (txID=%s)", spi.ErrTxRolledBack, tx.ID)
		}
		result := make(map[string]int64)
		err := s.countTx(ctx, tx, modelRef, func(st string) {
			if filter != nil {
				if _, ok := filter[st]; !ok {
					return
				}
			}
			result[st]++
		})
		if err != nil {
			return nil, fmt.Errorf("CountByState: %w", err)
		}
		return result, nil
	}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `cd plugins/memory && go test -run 'TestTxCount' ./...`
Expected: PASS

- [ ] **Step 5: Run the memory package**

Run: `cd plugins/memory && go test ./...`
Expected: PASS

- [ ] **Step 6: Commit**

```bash
git add plugins/memory/entity_store.go plugins/memory/tx_count_test.go
git commit -m "fix(memory): in-transaction counts tally the pointer snapshot instead of copying the merged view"
```

---

### Task 11: memory in-transaction `DeleteAll` stages ids from the pointer snapshot

**Files:**
- Modify: `plugins/memory/entity_store.go:671-680` (`DeleteAll` tx branch snapshot)
- Test: `plugins/memory/tx_delete_all_test.go` (new, `package memory_test`) — Task 6's `TestTxDeleteAll_StagesCommittedAndBuffered` on the memory fixture.

- [ ] **Step 1: Write the test** (copy Task 6's test body onto the memory fixture; `seedStates` from Task 8 for seeding, or a three-line loop).

- [ ] **Step 2: Run it** — Expected: PASS today; keep as the pin.

- [ ] **Step 3: Replace the copying snapshot**

```go
		var mainEntities []*spi.Entity
		func() {
			s.factory.entityMu.RLock()
			defer s.factory.entityMu.RUnlock()
			mainEntities = s.getAllSnapshotPointersUnlocked(modelRef, tx.SnapshotTime)
		}()
```

(drop `snapErr`; the pointer variant returns no error). The loop below reads only `ent.Meta.ID`.

Then check whether `getAllSnapshotUnlocked` still has callers:

Run: `grep -n "getAllSnapshotUnlocked" plugins/memory/*.go`
Expected: only `GetAll` (`entity_store.go:468`) and its own definition. `GetAll` is removed in Plan 3; leave it.

- [ ] **Step 4: Run the memory package**

Run: `cd plugins/memory && go test ./...`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add plugins/memory/entity_store.go plugins/memory/tx_delete_all_test.go
git commit -m "fix(memory): in-transaction DeleteAll stages ids from the pointer snapshot"
```

---

### Task 12: Docs, plugin re-pin, full verification

**Files:**
- Modify: `cmd/cyoda/help/content/crud.md:533-536`, `CHANGELOG.md` (Unreleased → Fixed), root `go.mod`/`go.sum` via `make repin-plugins`.

- [ ] **Step 1: Update the help topic**

In `cmd/cyoda/help/content/crud.md:533-536`, replace the sentence describing the sqlite in-transaction iterator as "materializes via getAllTx" and the memory one as `buildSnapshot` with:

> Inside a transaction, sqlite streams one merged cursor (committed snapshot on the reader connection plus the transaction's own buffered writes, staged deletes suppressed); memory walks a pointer snapshot of the merged view. Neither copies entity payloads beyond the rows it yields, and `trackingRead` records only yielded rows.

Run the help content linter: `go test ./cmd/cyoda/help/... -run TestContent`
Expected: PASS

- [ ] **Step 2: CHANGELOG**

Under `## [Unreleased]` → `### Fixed`, add:

```
- sqlite: `Begin` now waits for an in-flight commit's flush before flooring its
  snapshot time, so a transaction begun mid-commit cannot miss rows a commit
  it is ordered after has already claimed.
- sqlite and memory: `CompareAndSave` after a same-transaction `Delete` returns
  a conflict on every backend (memory and sqlite previously resurrected the
  entity at commit); `Save` after `Delete` clears the delete's attribution too.
- sqlite: in-transaction `Iterate`, `GetPage`, `Count`, `CountByState` and
  `DeleteAll` no longer materialise the model's merged view — one overlay
  cursor serves them all, and counts read no payload bytes.
- memory: `Search` no longer copies every entity's payload before filtering;
  in-transaction `Iterate` records the read-set per yield instead of the whole
  model at open; grouped stats records nothing in a transaction, matching
  sqlite and postgres.
```

- [ ] **Step 3: Re-pin the plugins and run the full suite**

Run: `make repin-plugins && make test-full`
Expected: repin self-verifies; `make test-full` green (root + all three plugins + e2e). Fix anything red before proceeding; a failure here is not "pre-existing" until reproduced at the merge-base.

- [ ] **Step 4: Race detector, once**

Run: `make race`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add cmd/cyoda/help/content/crud.md CHANGELOG.md go.mod go.sum
git commit -m "docs: in-transaction reads stream; plugin re-pin"
```

- [ ] **Step 6: Review gates, then PR**

Dispatch the fresh-context code review (`superpowers:requesting-code-review`) and the security review (`antigravity-bundle-security-developer:cc-skill-security-review`) on the branch diff against `release/v0.8.4`. Open the PR with `gh pr create --base release/v0.8.4`, milestone `v0.8.4`, body referencing #477 and the spec; add the #516 row-13 progress note on the issue in the same session.

---

## Self-review

- **Spec coverage:** §4.1 → Task 1; §4.2 → Tasks 2, 7; §4.3 → Tasks 5, 10; §4.4 → Tasks 3, 4; §4.5 → Tasks 8, 9; §4.6 → Tasks 6, 11; §8 PR-1 docs → Task 12. §4.7 and the spitest cases belong to Plans 2–3.
- **Placeholders:** none; every code step carries the code. Two "check the exact SPI name" instructions are verification steps, not gaps.
- **Type consistency:** `openTxOverlay(ctx, tx, modelRef, filter, proj)` and `txOverlay.pull/Close` are used identically in Tasks 3, 4, 5; `countTx` has the same shape in both plugins; `unstageDelete(tx, id)` in both; `buildSnapshot(ctx, model, pit)` returns `(snapshot, bufferedIDs, err)` in Task 8 and its two callers are updated there.
