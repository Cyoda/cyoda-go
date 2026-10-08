# Consistency Time Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Every point-in-time read is fenced by a store-supplied consistency time, so it never gives an answer that can change later, and an async search with no `pointInTime` includes every confirmed save.

**Architecture:** A new required SPI method `TransactionManager.ConsistencyTime` returns `C` by "reserve, then wait" in each backend (memory: reserve under the existing lock; sqlite: reserve under the commit gate; postgres: a floor sequence, an in-flight advisory marker taken at stamp time, and a SQL function that reserves `C` and waits for the tenant's markers). An engine package `internal/domain/consistency` caches the highest `C` per tenant and fences every read that carries an instant; the async default uses a fresh `C`. A new `GET /entity/consistency-time` (and gRPC request) returns a fresh `C`.

**Tech Stack:** Go 1.26, pgx v5 / PostgreSQL 17, sqlite (modernc), oapi-codegen, go-jsonschema, testcontainers-go.

**Spec:** `docs/superpowers/specs/2026-10-05-649-consistency-time-design.md` (commit `40331953`). Research: `docs/superpowers/research/2026-10-04-649-consistency-time-research.md`. Read both before any task.

## Global Constraints

- TDD is mandatory: every behaviour change starts with a test watched failing for the right reason (`.claude/rules/tdd.md`).
- No test hooks in production code. Waits are proven by tests that hold real locks/gates (spec §11).
- Fail closed: never fall back to `time.Now()` or any other clock when `C` cannot be obtained (`.claude/rules/correctness-over-availability.md`).
- No issue numbers (`#649`, `#581`, …) in code, comments, error messages, help topics or OpenAPI (`TestSource_NoIssueNumbers`).
- `log/slog` only. Wrap errors with context: `fmt.Errorf("failed to X: %w", err)`.
- New error codes: `POINT_IN_TIME_AFTER_CONSISTENCY_TIME` (400, not retryable, `properties.consistencyTime`), `CONSISTENCY_TIME_UNAVAILABLE` (503, retryable). Exact strings.
- New endpoint: `GET /entity/consistency-time`, operationId `getConsistencyTime`, body `{"consistencyTime": <RFC3339Nano>}`. gRPC: `EntityConsistencyTimeGetRequest` → `EntityConsistencyTimeResponse`.
- postgres wait budget: 10 000 ms, or the configured statement timeout when it is above 0 and lower. Store-call deadline in the engine: 11 s.
- `cyoda_stamp` sets `lock_timeout` 2000 ms and `idle_in_transaction_session_timeout` = lower of the operator's setting and 5000 ms (0 = unset).
- Postgres migration number: `000016_consistency_time`.
- Per-task test runs: focused package tests plus `make test`. **Do not run `make test-full`, `make race` or the full `internal/e2e` package in a task**; run new e2e tests by name with `-run`. Full tiers run once, in Task 19.
- Long runs go to a log file, never `| tail`: `make test > /tmp/<task>-make-test.log 2>&1`, then `grep -n -E '^(--- FAIL|FAIL|panic:)|FAIL:' /tmp/<task>-make-test.log`.
- SPI is composed locally through `go.work` (`use /Users/paul/go-projects/cyoda-light/cyoda-go-spi`), an **uncommitted** line, until Task 19 swaps the pins.

## Review Focus

1. **A client echoes `C` back verbatim** (the exact `consistencyTime` string, nanoseconds on memory, microseconds on postgres) → the read must be served, never refused. Pinned in Task 12 (e2e) and Task 3 (memory precision).
2. **`pointInTime` with a non-UTC offset** (`2026-10-05T16:03:07.123456+02:00`) equal to `C` → compared as instants, served. Pinned in Task 7.
3. **An ancient instant** (`0001-01-01T00:00:00Z`, or before any data) on a fresh node whose cached `hi` is zero → passes after one store call, returns the normal empty/404 answer. Pinned in Task 7.
4. **Tenant ids that differ only by case** (`Acme` vs `acme`) → two separate cache entries; one tenant's `C` never answers for the other. Pinned in Task 7.
5. **A burst of concurrent fence misses for one tenant** (100 goroutines) → at most two store calls in flight, every caller answered. Pinned in Task 7.

## Streams and order

| Stream | Tasks | Depends on |
|---|---|---|
| A — SPI (`cyoda-go-spi`) | 1, 2 | — |
| B — backends | 3 (memory), 4 (sqlite), 5 (postgres stamp + `C`), 6 (postgres reads) | 2; 3, 4, 5 are independent of each other; 6 after 5 |
| C — engine | 7, 8, 9, 10, 11, 12 | 7 needs 1; 8 needs 7; 9 independent; 10, 11, 12 need 7, 8, 9 |
| D — tests, docs, delivery | 13, 14, 15, 16, 17, 18, 19 | all of B and C |

Streams B and C can run in parallel worktrees once Task 2 is done.

---

### Task 1: SPI — `ConsistencyTime`, sentinel, `Count`/`CountByState` with `asAt`, doc changes

**Repository:** `/Users/paul/go-projects/cyoda-light/cyoda-go-spi`. Create a worktree/branch `feat-consistency-time` off `origin/main` there.

**Files:**
- Modify: `transaction.go` (interface, after `GetSubmitTime` at `:66-69`)
- Modify: `errors.go` (new sentinel next to `ErrTxNotCommitted`, `:133`)
- Modify: `persistence.go:369-389` (`Count`, `CountByState`), `GetVersionMetadata` doc (`~:445`)
- Modify: `default_save_all_test.go:39-40`, `spitest/transaction.go:580,583` (fakes/callers that must compile)
- Modify: `CHANGELOG.md` (`[Unreleased]` → `### Breaking`)

**Interfaces:**
- Produces:
  - `TransactionManager.ConsistencyTime(ctx context.Context) (time.Time, error)`
  - `var ErrConsistencyTimeUnavailable = errors.New("consistency time unavailable")`
  - `EntityStore.Count(ctx context.Context, modelRef ModelRef, asAt *time.Time) (int64, error)`
  - `EntityStore.CountByState(ctx context.Context, modelRef ModelRef, states []string, asAt *time.Time) (map[string]int64, error)`

- [ ] **Step 1: Add the interface method with its contract**

In `transaction.go`, after `GetSubmitTime`:

```go
	// ConsistencyTime returns the consistency time C for the tenant in ctx:
	// an instant, in the store's own stamp domain, with four properties.
	//
	//   - Complete: every save, of any tenant, whose success was returned on
	//     any node before this call started has a stamp <= C.
	//   - Final: a read for this tenant at T <= C that starts after this call
	//     returned sees every save of this tenant stamped <= T, now and later.
	//     A save not yet stamped when C is returned is stamped > C.
	//   - Monotonic: every C returned, for any tenant on any node, is >= every
	//     C returned before this call started, across restarts too.
	//   - Read resolution: if the store widens an instant to a coarser unit
	//     when it reads (a whole millisecond, say), C closes that whole unit.
	//
	// The mechanism is "reserve, then wait": raise the stamp floor to
	// max(store clock, highest stamp issued), then wait until every save of
	// the tenant already holding a stamp <= C has committed or aborted.
	//
	// It never returns a guessed instant. When the store cannot certify C
	// within its own wait budget it returns an error wrapping
	// ErrConsistencyTimeUnavailable. It never uses, joins or holds the
	// transaction in ctx, so it is safe to call from inside one.
	ConsistencyTime(ctx context.Context) (time.Time, error)
```

- [ ] **Step 2: Add the sentinel**

In `errors.go`, after `ErrTxNotCommitted`:

```go
// ErrConsistencyTimeUnavailable is returned (wrapped) by
// TransactionManager.ConsistencyTime when the store cannot certify a
// consistency time within its wait budget — typically because a save of the
// tenant is held in its commit phase. It is transient: a retry may succeed.
var ErrConsistencyTimeUnavailable = errors.New("consistency time unavailable")
```

- [ ] **Step 3: Change `Count` and `CountByState` and document them**

Replace `persistence.go:369` and the `CountByState` declaration (`:389`):

```go
	// Count returns the number of non-deleted entities of modelRef.
	//
	// asAt == nil: the current state. Inside a transaction the count reflects
	// the transactional view (its own uncommitted writes visible, other
	// in-flight transactions' writes not).
	//
	// asAt != nil: the number of entities whose latest revision at or before
	// asAt is not a deletion — committed data only, ignoring any ambient
	// transaction and recording nothing in its read set, the same
	// point-in-time rule as GetPage(asAt) and IterateOptions.PointInTime.
	//
	// Unknown model: 0 with no error.
	Count(ctx context.Context, modelRef ModelRef, asAt *time.Time) (int64, error)
```

Keep the existing `CountByState` doc and append before the declaration:

```go
	//
	// asAt follows Count: nil is the current (transactional) view; non-nil
	// counts committed revisions as at asAt only, ignoring the ambient
	// transaction and recording nothing in its read set.
	CountByState(ctx context.Context, modelRef ModelRef, states []string, asAt *time.Time) (map[string]int64, error)
```

Append to the `GetVersionMetadata` doc comment:

```go
	// It reads committed versions only, inside a transaction too: a
	// transaction's own uncommitted versions are never listed.
```

- [ ] **Step 4: Make the module compile**

Run: `go build ./... && go vet ./...`
Fix each compile error in `default_save_all_test.go:39-40`, `spitest/transaction.go:580,583` and the `Count`/`CountByState` calls in `spitest/entity.go` by passing `nil` for `asAt` (and adding a `ConsistencyTime` method to any non-embedding fake that returns `time.Time{}, errors.New("not implemented")`).
Expected: build and vet clean.

- [ ] **Step 5: Changelog**

Under `## [Unreleased]` / `### Breaking` in `CHANGELOG.md`:

```markdown
- `TransactionManager.ConsistencyTime(ctx)` is required. It returns the
  consistency time: complete, final, monotonic, at the store's read
  resolution (see its doc). New sentinel `ErrConsistencyTimeUnavailable`.
  Migration: implement reserve-then-wait; see the in-tree plugins of
  cyoda-go for reference implementations.
- `EntityStore.Count` and `CountByState` take `asAt *time.Time`. Pass `nil`
  for today's behaviour; non-nil counts committed revisions as at that
  instant, ignoring the ambient transaction.
- `GetVersionMetadata` is documented as committed-only inside a transaction.
```

- [ ] **Step 6: Commit**

```bash
git add transaction.go errors.go persistence.go default_save_all_test.go spitest CHANGELOG.md
git commit -m "feat(spi)!: TransactionManager.ConsistencyTime; Count/CountByState take asAt"
```

---

### Task 2: spitest — `ConsistencyTime` group and point-in-time count cases

**Repository:** `cyoda-go-spi`, same branch.

**Files:**
- Create: `spitest/consistency.go`
- Modify: `spitest/spitest.go:191-202` (register the group)
- Modify: `spitest/entity.go` (register `Count/AsAt`, `CountByState/AsAt`, `GetVersionMetadata/CommittedOnlyInTx` beside `:35-36`)

**Interfaces:**
- Consumes: Task 1 signatures.
- Produces: subtests `ConsistencyTime/AcknowledgedCommitIncluded`, `…/CrossTenantCommitIncluded`, `…/LaterCommitStampsAbove`, `…/Monotonic`, `…/FinalUnderConcurrentWrites`, `…/NonTransactionalSaveIncluded`, `…/CalledInsideTransaction`; `Entity/Count/AsAt`, `Entity/CountByState/AsAt`, `Entity/GetVersionMetadata/CommittedOnlyInTx`.

- [ ] **Step 1: Write the group**

Create `spitest/consistency.go`:

```go
package spitest

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// runConsistencyTimeSuite covers TransactionManager.ConsistencyTime.
func runConsistencyTimeSuite(t *testing.T, h Harness, tracker *skipTracker) {
	runSubtest(t, h, tracker, "AcknowledgedCommitIncluded", testCTAcknowledgedCommitIncluded)
	runSubtest(t, h, tracker, "CrossTenantCommitIncluded", testCTCrossTenantCommitIncluded)
	runSubtest(t, h, tracker, "LaterCommitStampsAbove", testCTLaterCommitStampsAbove)
	runSubtest(t, h, tracker, "Monotonic", testCTMonotonic)
	runSubtest(t, h, tracker, "FinalUnderConcurrentWrites", testCTFinalUnderConcurrentWrites)
	runSubtest(t, h, tracker, "NonTransactionalSaveIncluded", testCTNonTransactionalSaveIncluded)
	runSubtest(t, h, tracker, "CalledInsideTransaction", testCTCalledInsideTransaction)
}

func commitOne(t *testing.T, h Harness, ctx context.Context, model string) (txID, entityID string) {
	t.Helper()
	tm, err := h.Factory.TransactionManager(ctx)
	require.NoError(t, err)
	txID, txCtx := beginGuarded(t, tm, ctx)
	es, err := h.Factory.EntityStore(txCtx)
	require.NoError(t, err)
	entityID = newID()
	_, err = es.Save(txCtx, newEntity(t, model, entityID, map[string]any{"k": 1}))
	require.NoError(t, err)
	require.NoError(t, tm.Commit(txCtx, txID))
	return txID, entityID
}

func testCTAcknowledgedCommitIncluded(t *testing.T, h Harness) {
	ctx := tenantContext(h.NewTenant())
	tm, err := h.Factory.TransactionManager(ctx)
	require.NoError(t, err)
	txID, _ := commitOne(t, h, ctx, "m-ct-ack")
	c, err := tm.ConsistencyTime(ctx)
	require.NoError(t, err)
	submit, err := tm.GetSubmitTime(ctx, txID)
	require.NoError(t, err)
	require.False(t, submit.After(c), "acknowledged commit %v must be <= C %v", submit, c)
}

func testCTCrossTenantCommitIncluded(t *testing.T, h Harness) {
	ctxA := tenantContext(h.NewTenant())
	ctxB := tenantContext(h.NewTenant())
	tmA, err := h.Factory.TransactionManager(ctxA)
	require.NoError(t, err)
	tmB, err := h.Factory.TransactionManager(ctxB)
	require.NoError(t, err)
	txID, _ := commitOne(t, h, ctxA, "m-ct-xt")
	cB, err := tmB.ConsistencyTime(ctxB)
	require.NoError(t, err)
	submit, err := tmA.GetSubmitTime(ctxA, txID)
	require.NoError(t, err)
	require.False(t, submit.After(cB), "tenant A's commit %v must be <= tenant B's C %v", submit, cB)
}

func testCTLaterCommitStampsAbove(t *testing.T, h Harness) {
	ctx := tenantContext(h.NewTenant())
	tm, err := h.Factory.TransactionManager(ctx)
	require.NoError(t, err)
	c, err := tm.ConsistencyTime(ctx)
	require.NoError(t, err)
	txID, _ := commitOne(t, h, ctx, "m-ct-later")
	submit, err := tm.GetSubmitTime(ctx, txID)
	require.NoError(t, err)
	require.True(t, submit.After(c), "commit after C must stamp > C (submit %v, C %v)", submit, c)
}

func testCTMonotonic(t *testing.T, h Harness) {
	ctxA := tenantContext(h.NewTenant())
	ctxB := tenantContext(h.NewTenant())
	tmA, _ := h.Factory.TransactionManager(ctxA)
	tmB, _ := h.Factory.TransactionManager(ctxB)
	var prev time.Time
	for i := 0; i < 20; i++ {
		ctx, tm := ctxA, tmA
		if i%2 == 1 {
			ctx, tm = ctxB, tmB
		}
		c, err := tm.ConsistencyTime(ctx)
		require.NoError(t, err)
		require.False(t, c.Before(prev), "C went backwards: %v after %v", c, prev)
		prev = c
	}
}

// Writers commit concurrently while a checker takes C, counts at C, waits and
// counts again. Both counts must match and must include every commit
// acknowledged before the request. Deterministic proof of the wait lives in
// each plugin's white-box tests; this case catches an implementation that
// returns the raw clock.
func testCTFinalUnderConcurrentWrites(t *testing.T, h Harness) {
	ctx := tenantContext(h.NewTenant())
	tm, err := h.Factory.TransactionManager(ctx)
	require.NoError(t, err)
	mref := spi.ModelRef{EntityName: "m-ct-final", ModelVersion: "1"}
	var acked atomic.Int64
	stop := make(chan struct{})
	var wg sync.WaitGroup
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				commitOne(t, h, ctx, "m-ct-final")
				acked.Add(1)
			}
		}()
	}
	es, err := h.Factory.EntityStore(ctx)
	require.NoError(t, err)
	for i := 0; i < 20; i++ {
		before := acked.Load()
		c, err := tm.ConsistencyTime(ctx)
		require.NoError(t, err)
		n1, err := es.Count(ctx, mref, &c)
		require.NoError(t, err)
		require.GreaterOrEqual(t, n1, before, "count at C must include every acknowledged commit")
		h.AdvanceClock(5 * time.Millisecond)
		n2, err := es.Count(ctx, mref, &c)
		require.NoError(t, err)
		require.Equal(t, n1, n2, "count at C changed after it was given")
	}
	close(stop)
	wg.Wait()
}

func testCTNonTransactionalSaveIncluded(t *testing.T, h Harness) {
	ctx := tenantContext(h.NewTenant())
	tm, err := h.Factory.TransactionManager(ctx)
	require.NoError(t, err)
	es, err := h.Factory.EntityStore(ctx)
	require.NoError(t, err)
	id := newID()
	_, err = es.Save(ctx, newEntity(t, "m-ct-nontx", id, map[string]any{"k": 1}))
	require.NoError(t, err)
	c, err := tm.ConsistencyTime(ctx)
	require.NoError(t, err)
	got, err := es.GetAsAt(ctx, id, c)
	require.NoError(t, err)
	require.Equal(t, id, got.Meta.ID)
}

func testCTCalledInsideTransaction(t *testing.T, h Harness) {
	ctx := tenantContext(h.NewTenant())
	tm, err := h.Factory.TransactionManager(ctx)
	require.NoError(t, err)
	txID, txCtx := beginGuarded(t, tm, ctx)
	_, err = tm.ConsistencyTime(txCtx)
	require.NoError(t, err)
	// The transaction is still usable and still ours to commit.
	require.NoError(t, tm.Commit(txCtx, txID))
}
```

Note: `Save` without a transaction — if a backend refuses it, check how `spitest/entity.go` saves outside a transaction (`es.Save(ctx, …)` is used by existing non-tx cases) and follow that exactly.

- [ ] **Step 2: Register the group**

In `spitest/spitest.go` `StoreFactoryConformance`, beside the other `t.Run` lines:

```go
	t.Run("ConsistencyTime", func(t *testing.T) { runConsistencyTimeSuite(t, h, tracker) })
```

- [ ] **Step 3: Add the point-in-time count and history cases**

In `spitest/entity.go`, register after `Count`/`CountByState` (`:35-36`):

```go
	runSubtest(t, h, tracker, "Count/AsAt", testEntityCountAsAt)
	runSubtest(t, h, tracker, "CountByState/AsAt", testEntityCountByStateAsAt)
	runSubtest(t, h, tracker, "GetVersionMetadata/CommittedOnlyInTx", testEntityVersionMetadataCommittedOnlyInTx)
```

and add:

```go
func testEntityCountAsAt(t *testing.T, h Harness) {
	ctx := tenantContext(h.NewTenant())
	mref := spi.ModelRef{EntityName: "m-cnt-at", ModelVersion: "1"}
	var ids []string
	withTx(t, h, ctx, func(txCtx context.Context) {
		es, _ := h.Factory.EntityStore(txCtx)
		for i := 0; i < 3; i++ {
			id := newID()
			ids = append(ids, id)
			_, err := es.Save(txCtx, newEntity(t, "m-cnt-at", id, map[string]any{}))
			require.NoError(t, err)
		}
	})
	h.AdvanceClock(10 * time.Millisecond)
	mid := h.Now()
	h.AdvanceClock(10 * time.Millisecond)
	withTx(t, h, ctx, func(txCtx context.Context) {
		es, _ := h.Factory.EntityStore(txCtx)
		require.NoError(t, es.Delete(txCtx, ids[0]))
		_, err := es.Save(txCtx, newEntity(t, "m-cnt-at", newID(), map[string]any{}))
		require.NoError(t, err)
		_, err = es.Save(txCtx, newEntity(t, "m-cnt-at", newID(), map[string]any{}))
		require.NoError(t, err)
	})
	es, _ := h.Factory.EntityStore(ctx)
	n, err := es.Count(ctx, mref, &mid)
	require.NoError(t, err)
	require.Equal(t, int64(3), n, "as at mid: three live entities")
	n, err = es.Count(ctx, mref, nil)
	require.NoError(t, err)
	require.Equal(t, int64(4), n, "now: one deleted, two added")

	// Inside a transaction, asAt is committed-only.
	tm, _ := h.Factory.TransactionManager(ctx)
	txID, txCtx := beginGuarded(t, tm, ctx)
	esTx, _ := h.Factory.EntityStore(txCtx)
	_, err = esTx.Save(txCtx, newEntity(t, "m-cnt-at", newID(), map[string]any{}))
	require.NoError(t, err)
	n, err = esTx.Count(txCtx, mref, &mid)
	require.NoError(t, err)
	require.Equal(t, int64(3), n, "asAt inside a tx ignores the tx's own writes")
	require.NoError(t, tm.Rollback(txCtx, txID))
}

func testEntityCountByStateAsAt(t *testing.T, h Harness) {
	ctx := tenantContext(h.NewTenant())
	mref := spi.ModelRef{EntityName: "m-cbs-at", ModelVersion: "1"}
	id := newID()
	withTx(t, h, ctx, func(txCtx context.Context) {
		es, _ := h.Factory.EntityStore(txCtx)
		e := newEntity(t, "m-cbs-at", id, map[string]any{})
		e.Meta.State = "new"
		_, err := es.Save(txCtx, e)
		require.NoError(t, err)
	})
	h.AdvanceClock(10 * time.Millisecond)
	mid := h.Now()
	h.AdvanceClock(10 * time.Millisecond)
	withTx(t, h, ctx, func(txCtx context.Context) {
		es, _ := h.Factory.EntityStore(txCtx)
		got, err := es.Get(txCtx, id)
		require.NoError(t, err)
		got.Meta.State = "approved"
		_, err = es.Save(txCtx, got)
		require.NoError(t, err)
	})
	es, _ := h.Factory.EntityStore(ctx)
	m, err := es.CountByState(ctx, mref, nil, &mid)
	require.NoError(t, err)
	require.Equal(t, map[string]int64{"new": 1}, m)
	m, err = es.CountByState(ctx, mref, []string{"approved"}, &mid)
	require.NoError(t, err)
	require.Empty(t, m)
	m, err = es.CountByState(ctx, mref, nil, nil)
	require.NoError(t, err)
	require.Equal(t, map[string]int64{"approved": 1}, m)
}

func testEntityVersionMetadataCommittedOnlyInTx(t *testing.T, h Harness) {
	ctx := tenantContext(h.NewTenant())
	id := newID()
	withTx(t, h, ctx, func(txCtx context.Context) {
		es, _ := h.Factory.EntityStore(txCtx)
		_, err := es.Save(txCtx, newEntity(t, "m-vm-tx", id, map[string]any{"v": 1}))
		require.NoError(t, err)
	})
	tm, _ := h.Factory.TransactionManager(ctx)
	txID, txCtx := beginGuarded(t, tm, ctx)
	esTx, _ := h.Factory.EntityStore(txCtx)
	got, err := esTx.Get(txCtx, id)
	require.NoError(t, err)
	_, err = esTx.Save(txCtx, got)
	require.NoError(t, err)
	vs, err := esTx.GetVersionMetadata(txCtx, id, spi.VersionMetadataOptions{})
	require.NoError(t, err)
	require.Len(t, vs, 1, "the transaction's own uncommitted version must not be listed")
	require.NoError(t, tm.Rollback(txCtx, txID))
}
```

Check `GetVersionMetadata`'s exact signature and return type in `persistence.go` (`~:445`) and adapt the `require.Len` target to its result shape.

- [ ] **Step 4: Verify the module and lint**

Run: `go build ./... && go vet ./... && ~/go/bin/golangci-lint run ./...`
Expected: clean. (spitest never runs inside the SPI repo; the cases run in the plugins, Tasks 3-6.)

- [ ] **Step 5: Commit and compose locally**

```bash
git add spitest
git commit -m "test(spitest): ConsistencyTime conformance group; Count/CountByState asAt; committed-only version metadata"
```

In the cyoda-go worktree, add the uncommitted local composition (do not commit it):

```bash
cd /Users/paul/go-projects/cyoda-light/cyoda-go/.claude/worktrees/feat-649-consistency-time
go work use /Users/paul/go-projects/cyoda-light/cyoda-go-spi
git diff --stat go.work   # shows the use line; never stage it
```

---

### Task 3: memory — `ConsistencyTime`, wall-clock floor, counts at an instant

**Files:**
- Modify: `plugins/memory/txmanager.go` (`nextSubmitTime` `:662-671`, `Begin` `:742-753`, comment `:725-726`; new method after `GetSubmitTime` `:1259`)
- Modify: `plugins/memory/entity_store.go` (`Count` `:874`, `CountByState` `:914`)
- Test: `plugins/memory/consistency_time_test.go` (new), existing conformance via `plugins/memory/conformance_test.go`

**Interfaces:**
- Consumes: Task 1 SPI.
- Produces: `(*TransactionManager).ConsistencyTime(ctx) (time.Time, error)`.

- [ ] **Step 1: Write the failing white-box tests**

Create `plugins/memory/consistency_time_test.go` (package `memory_test`, as `txmanager_floor_test.go`):

```go
package memory_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/plugins/memory"
)

func ctTenantCtx(tenant string) context.Context {
	return spi.WithUserContext(context.Background(), &spi.UserContext{
		UserID: "u", Tenant: spi.Tenant{ID: spi.TenantID(tenant), Name: tenant},
	})
}

// C comes from the store's clock, not the process clock.
func TestConsistencyTime_ReadsTheStoreClock(t *testing.T) {
	ahead := time.Now().Add(time.Hour)
	f := memory.NewStoreFactory(memory.WithClock(memory.NewTestClockAt(ahead)))
	ctx := ctTenantCtx("t1")
	tm, err := f.TransactionManager(ctx)
	require.NoError(t, err)
	c, err := tm.ConsistencyTime(ctx)
	require.NoError(t, err)
	require.False(t, c.Before(ahead), "C %v must be at the store clock %v, not the process clock", c, ahead)
}

// A commit after C is stamped strictly after C even under a frozen clock.
func TestConsistencyTime_ReservesTheFloor(t *testing.T) {
	clock := memory.NewTestClockAt(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	f := memory.NewStoreFactory(memory.WithClock(clock))
	ctx := ctTenantCtx("t1")
	tm, _ := f.TransactionManager(ctx)
	c, err := tm.ConsistencyTime(ctx)
	require.NoError(t, err)
	txID, txCtx, err := tm.Begin(ctx)
	require.NoError(t, err)
	es, _ := f.EntityStore(txCtx)
	_, err = es.Save(txCtx, &spi.Entity{Meta: spi.EntityMeta{ID: "e1", ModelRef: spi.ModelRef{EntityName: "m", ModelVersion: "1"}}, Data: []byte(`{}`)})
	require.NoError(t, err)
	require.NoError(t, tm.Commit(txCtx, txID))
	submit, err := tm.GetSubmitTime(ctx, txID)
	require.NoError(t, err)
	require.True(t, submit.After(c))
}

// steppingClock returns wall times that step back while keeping Go's
// monotonic reading moving forward, as time.Now does after an NTP step.
type steppingClock struct{ t time.Time }

func (s *steppingClock) Now() time.Time { return s.t }

func TestConsistencyTime_FloorSurvivesWallClockStepBack(t *testing.T) {
	sc := &steppingClock{t: time.Now()} // carries a monotonic reading
	f := memory.NewStoreFactory(memory.WithClock(sc))
	ctx := ctTenantCtx("t1")
	tm, _ := f.TransactionManager(ctx)
	c1, err := tm.ConsistencyTime(ctx)
	require.NoError(t, err)
	// Step the wall clock back 10 ms but keep a later monotonic reading:
	// time.Now() then subtracting via AddDate keeps the monotonic part.
	sc.t = time.Now().Add(-10 * time.Millisecond)
	c2, err := tm.ConsistencyTime(ctx)
	require.NoError(t, err)
	require.False(t, c2.Round(0).Before(c1.Round(0)), "wall-time C went backwards: %v then %v", c1, c2)
}
```

Add the memory "no wait needed" proof, using `gatedClock` (`txmanager_test.go:823-871`, same package): arm the clock, start a `Commit` in a goroutine (it parks inside `nextSubmitTime` holding `entityMu` and `m.mu`), call `ConsistencyTime` in another goroutine (it blocks on `m.mu`), unblock the clock, then assert `GetAsAt(ctx, id, C)` finds the committed entity — a read at `C` that starts after `C` returned sees the commit.

```go
func TestConsistencyTime_ReadAtCSeesCommitParkedInItsStamp(t *testing.T) {
	gc := newGatedClock(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)) // existing helper; match its constructor
	f := memory.NewStoreFactory(memory.WithClock(gc))
	ctx := ctTenantCtx("t1")
	tm, _ := f.TransactionManager(ctx)
	txID, txCtx, err := tm.Begin(ctx)
	require.NoError(t, err)
	es, _ := f.EntityStore(txCtx)
	_, err = es.Save(txCtx, &spi.Entity{Meta: spi.EntityMeta{ID: "e1", ModelRef: spi.ModelRef{EntityName: "m", ModelVersion: "1"}}, Data: []byte(`{}`)})
	require.NoError(t, err)
	entered, unblock := gc.arm()
	go func() { require.NoError(t, tm.Commit(txCtx, txID)) }()
	<-entered
	cCh := make(chan time.Time, 1)
	go func() { c, err := tm.ConsistencyTime(ctx); require.NoError(t, err); cCh <- c }()
	close(unblock)
	c := <-cCh
	esr, _ := f.EntityStore(ctx)
	got, err := esr.GetAsAt(ctx, "e1", c)
	require.NoError(t, err)
	require.Equal(t, "e1", got.Meta.ID)
}
```

Note for the step-back test: `time.Now().Add(d)` keeps the monotonic reading, which shifts with `d`; build the stepped value so that its monotonic reading is later than `c1`'s while its wall value is earlier (e.g. capture `base := time.Now()`, sleep 2 ms, then `sc.t = time.Now().Add(-10*time.Millisecond)` — wall earlier than `c1` only if 10 ms > elapsed; assert the precondition `sc.t.Round(0).Before(c1.Round(0))` and `sc.t.After(c1)` (monotonic comparison) before calling `ConsistencyTime`). This precondition is what makes the test fail today.

- [ ] **Step 2: Run to verify they fail**

Run: `cd plugins/memory && go test -run 'TestConsistencyTime_' ./...`
Expected: compile error "tm.ConsistencyTime undefined" (Step 3 makes them fail on assertions only for the step-back case).

- [ ] **Step 3: Implement**

In `txmanager.go`, change `nextSubmitTime`:

```go
func (m *TransactionManager) nextSubmitTime() time.Time {
	m.mu.Lock()
	defer m.mu.Unlock()
	// Round(0) drops Go's monotonic reading so the floor compares WALL
	// time: stamps and point-in-time instants are wall times, and a wall
	// clock that steps back must be floored, not followed.
	now := m.factory.clock.Now().Round(0)
	if !now.After(m.lastSubmitTime) {
		now = m.lastSubmitTime.Add(time.Microsecond)
	}
	m.lastSubmitTime = now
	return now
}
```

In `Begin` (`:744-753`) change `now := m.factory.clock.Now()` to `now := m.factory.clock.Now().Round(0)`, and replace the parenthetical at `:725-726` ("and the clock is monotonic non-decreasing — see clock.go: wallClock uses Go's monotonic time.Now(), TestClock's virtual time only ever advances forward") with: "and every stamp and snapshot is floored to lastSubmitTime under mu, so later sections never read an earlier value".

Add after `GetSubmitTime`:

```go
// ConsistencyTime implements spi.TransactionManager. It reserves
// C = max(clock, lastSubmitTime) as the new floor, exactly as Begin does,
// so every later stamp is strictly after C. No wait is needed: every writer
// holds factory.entityMu from stamp to publish and every reader takes
// entityMu.RLock, so a read that starts after C was returned cannot observe
// a stamp <= C that is not yet published. The floor is shared by all tenants,
// which makes C complete across tenants. A restart loses every stamp and job
// with it, so there is no earlier C to stay above.
func (m *TransactionManager) ConsistencyTime(ctx context.Context) (time.Time, error) {
	if uc := spi.GetUserContext(ctx); uc == nil || uc.Tenant.ID == "" {
		return time.Time{}, fmt.Errorf("ConsistencyTime: no tenant in context")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.factory.clock.Now().Round(0)
	if now.Before(m.lastSubmitTime) {
		now = m.lastSubmitTime
	}
	m.lastSubmitTime = now
	return now, nil
}
```

- [ ] **Step 4: Counts at an instant**

In `entity_store.go`, change the signatures to `Count(ctx, modelRef, asAt *time.Time)` and `CountByState(ctx, modelRef, states, asAt *time.Time)`. At the top of each (after the empty-states early return in `CountByState`), add the point-in-time branch, before the transaction branch:

```go
	if asAt != nil {
		s.factory.entityMu.RLock()
		defer s.factory.entityMu.RUnlock()
		ents, err := s.getAllSnapshotPointersUnlocked(ctx, modelRef, *asAt)
		if err != nil {
			return 0, fmt.Errorf("Count: %w", err)
		}
		return int64(len(ents)), nil
	}
```

and for `CountByState`:

```go
	if asAt != nil {
		s.factory.entityMu.RLock()
		defer s.factory.entityMu.RUnlock()
		ents, err := s.getAllSnapshotPointersUnlocked(ctx, modelRef, *asAt)
		if err != nil {
			return nil, fmt.Errorf("CountByState: %w", err)
		}
		result := make(map[string]int64)
		for _, e := range ents {
			if filter != nil {
				if _, ok := filter[e.Meta.State]; !ok {
					continue
				}
			}
			result[e.Meta.State]++
		}
		return result, nil
	}
```

(`filter` is built before the transaction branch; move the point-in-time branch after the `filter` construction.) Fix every in-plugin caller and test of `Count`/`CountByState` by passing `nil`.

- [ ] **Step 5: Run tests**

Run: `cd plugins/memory && go test ./... > /tmp/t3-memory.log 2>&1; grep -n -E '^(--- FAIL|FAIL|panic:)|FAIL:' /tmp/t3-memory.log`
Expected: no hits; the conformance suite now runs the `ConsistencyTime/*`, `Entity/Count/AsAt`, `Entity/CountByState/AsAt` and `Entity/GetVersionMetadata/CommittedOnlyInTx` cases green.

- [ ] **Step 6: Commit**

```bash
git add plugins/memory
git commit -m "feat(memory): ConsistencyTime; wall-clock stamp floor; counts at an instant"
```

---

### Task 4: sqlite — `ConsistencyTime`, durable high-water mark, fail-closed floor, counts and grouped stats at an instant

**Files:**
- Create: `plugins/sqlite/migrations/000011_consistency_floor.up.sql`, `000011_consistency_floor.down.sql`
- Modify: `plugins/sqlite/txmanager.go` (`seedLastSubmitTime` `:595-602`; new `ConsistencyTime`)
- Modify: `plugins/sqlite/store_factory.go:448-455` (`initTransactionManager` returns an error), `plugins/sqlite/plugin.go:43`, `store_factory.go:472`
- Modify: `plugins/sqlite/entity_store.go` (`Count` `:996`, `CountByState` `:1079`)
- Modify: `plugins/sqlite/grouped_stats.go` (`:24-32` comment, `:214-228` decline)
- Test: `plugins/sqlite/consistency_time_internal_test.go` (new, package `sqlite`), `plugins/sqlite/grouped_stats_test.go:583,724` (update)

**Interfaces:**
- Produces: `(*transactionManager).ConsistencyTime(ctx) (time.Time, error)`; `(*StoreFactory).initTransactionManager(uuids) error`.

- [ ] **Step 1: Write the failing white-box tests**

Create `plugins/sqlite/consistency_time_internal_test.go` (package `sqlite`, following `txmanager_begin_gate_internal_test.go`; reuse its factory/tenant helpers — read that file first and use the same constructor and context helper names):

```go
package sqlite

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// ConsistencyTime waits for a commit that holds the gate (stamped, rows not
// yet visible) and returns only after it.
func TestConsistencyTime_WaitsForInFlightCommit(t *testing.T) {
	f, ctx := newGateTestFactory(t) // same helper TestBegin_WaitsForInFlightCommit uses
	m := f.tm
	require.NoError(t, m.acquireCommitGate(context.Background()))
	m.mu.Lock()
	m.lastSubmitTime = time.Now().Add(5 * time.Second).UnixMicro() // the in-flight stamp
	m.mu.Unlock()

	done := make(chan time.Time, 1)
	go func() {
		c, err := m.ConsistencyTime(ctx)
		require.NoError(t, err)
		done <- c
	}()
	select {
	case <-done:
		t.Fatal("ConsistencyTime returned while a commit held the gate")
	case <-time.After(150 * time.Millisecond):
	}
	m.releaseCommitGate()
	c := <-done
	require.GreaterOrEqual(t, c.UnixMicro(), time.Now().Add(5*time.Second).UnixMicro()-1000)
}

func TestConsistencyTime_HonoursCallerContext(t *testing.T) {
	f, ctx := newGateTestFactory(t)
	require.NoError(t, f.tm.acquireCommitGate(context.Background()))
	defer f.tm.releaseCommitGate()
	cctx, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	_, err := f.tm.ConsistencyTime(cctx)
	require.ErrorIs(t, err, context.DeadlineExceeded)
}

// Monotonic across a restart whose wall clock stepped back: a C handed out
// before the restart stays at or below every later C and stamp.
func TestConsistencyTime_MonotonicAcrossRestartWithClockBack(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ct.db")
	future := time.Now().Add(time.Hour)
	f1, err := NewStoreFactoryForTest(context.Background(), path, WithClock(NewTestClockAt(future)))
	require.NoError(t, err)
	ctx := gateTestTenantCtx("t1") // same tenant-context helper as the gate tests
	c1, err := f1.tm.ConsistencyTime(ctx)
	require.NoError(t, err)
	require.NoError(t, f1.Close())

	f2, err := NewStoreFactoryForTest(context.Background(), path, WithClock(NewTestClockAt(time.Now())))
	require.NoError(t, err)
	defer f2.Close()
	c2, err := f2.tm.ConsistencyTime(ctx)
	require.NoError(t, err)
	require.False(t, c2.Before(c1), "C after restart %v below C before %v", c2, c1)
}
```

Also add:

- `TestSeed_FloorsAtJobInstantsAndSubmitTimes`: open, insert a `search_jobs` row with `point_in_time = now+1h` (µs) and a `submit_times` row with `submit_time = now+2h` by raw SQL, close, reopen with a clock at real now: `ConsistencyTime` returns `≥ now+2h`.
- `TestSeed_QueryErrorFailsConstruction`: drop `submit_times` with raw SQL on a closed file; `NewStoreFactoryForTest` returns an error.

- [ ] **Step 2: Run to verify they fail**

Run: `cd plugins/sqlite && go test -run 'TestConsistencyTime_' ./...`
Expected: compile error "f.tm.ConsistencyTime undefined".

- [ ] **Step 3: Migration**

`000011_consistency_floor.up.sql`:

```sql
-- The highest consistency time handed out, kept ahead of use so that a C
-- returned before a restart stays at or below every later C and stamp even
-- if the wall clock stepped back across the restart.
CREATE TABLE consistency_floor (
    id     INTEGER PRIMARY KEY CHECK (id = 1),
    micros INTEGER NOT NULL
);
INSERT INTO consistency_floor (id, micros) VALUES (1, 0);
```

`000011_consistency_floor.down.sql`:

```sql
DROP TABLE consistency_floor;
```

Check the number against `ls plugins/sqlite/migrations` (the last is `000010`).

- [ ] **Step 4: Floor seed fails closed; `initTransactionManager` returns an error**

```go
// seedLastSubmitTime floors lastSubmitTime at the highest instant this
// database has ever stamped or handed out, so stamps and consistency times
// stay monotonic across restarts — including one whose wall clock stepped
// back. Any query error is returned: starting from a zero floor would fail
// open.
func (m *transactionManager) seedLastSubmitTime() error {
	var floor sql.NullInt64
	err := m.factory.db.QueryRow(`SELECT MAX(v) FROM (
		SELECT MAX(submit_time) AS v FROM entity_versions
		UNION ALL SELECT MAX(submit_time) FROM submit_times
		UNION ALL SELECT MAX(point_in_time) FROM search_jobs
		UNION ALL SELECT micros FROM consistency_floor WHERE id = 1)`).Scan(&floor)
	if err != nil {
		return fmt.Errorf("failed to seed the submit-time floor: %w", err)
	}
	if floor.Valid {
		m.lastSubmitTime = floor.Int64
	}
	m.consistencyHigh = m.lastSubmitTime
	return nil
}
```

Add field `consistencyHigh int64 // persisted consistency_floor.micros; guarded by mu` to `transactionManager`. Change `initTransactionManager` to `func (f *StoreFactory) initTransactionManager(uuids spi.UUIDGenerator) error` returning the seed error; in `plugin.go:43` and `store_factory.go:472`, close the factory and return the error (`fmt.Errorf("sqlite: %w", err)`).

- [ ] **Step 5: `ConsistencyTime`**

```go
// consistencyHighStep is how far ahead of the C being handed out the durable
// high-water mark is written: one write per second of use at most.
const consistencyHighStep = time.Second

// ConsistencyTime implements spi.TransactionManager. It takes the commit gate
// (waiting for a commit in flight to make its rows visible), reserves
// C = max(clock, lastSubmitTime) as the new floor exactly as Begin does, and
// keeps the durable high-water mark above C. Every stamping path holds the
// gate from stamp to commit, so a read at T <= C that starts after C was
// returned sees every save stamped <= T. The floor is shared by all tenants.
func (m *transactionManager) ConsistencyTime(ctx context.Context) (time.Time, error) {
	if uc := spi.GetUserContext(ctx); uc == nil || uc.Tenant.ID == "" {
		return time.Time{}, fmt.Errorf("ConsistencyTime: no tenant in context")
	}
	if err := m.acquireCommitGate(ctx); err != nil {
		return time.Time{}, fmt.Errorf("ConsistencyTime: %w", err)
	}
	defer m.releaseCommitGate()
	nowMicro := m.factory.clock.Now().UnixMicro()
	m.mu.Lock()
	if nowMicro < m.lastSubmitTime {
		nowMicro = m.lastSubmitTime
	}
	m.lastSubmitTime = nowMicro
	needHigh := nowMicro > m.consistencyHigh
	m.mu.Unlock()
	if needHigh {
		high := nowMicro + consistencyHighStep.Microseconds()
		if _, err := m.factory.db.ExecContext(ctx,
			`UPDATE consistency_floor SET micros = ? WHERE id = 1 AND micros < ?`, high, high); err != nil {
			return time.Time{}, fmt.Errorf("ConsistencyTime: failed to persist the high-water mark: %w", err)
		}
		m.mu.Lock()
		if high > m.consistencyHigh {
			m.consistencyHigh = high
		}
		if m.lastSubmitTime < nowMicro { // unchanged invariant; floor only rises
			m.lastSubmitTime = nowMicro
		}
		m.mu.Unlock()
	}
	return time.UnixMicro(nowMicro), nil
}
```

The high-water write happens while the gate is held, so no stamp can land between the reservation and the write. On restart, `seedLastSubmitTime` floors at `consistency_floor.micros` (≥ every C handed out).

- [ ] **Step 6: Counts at an instant**

Change the signatures. Add the point-in-time branch at the top of `Count` (before the transaction branch):

```go
	if asAt != nil {
		var count int64
		err := s.readDB.QueryRowContext(ctx, `SELECT COUNT(*)
			FROM entity_versions ev
			INNER JOIN (
				SELECT entity_id, MAX(version) AS max_ver FROM entity_versions
				WHERE tenant_id = ? AND model_name = ? AND model_version = ? AND submit_time <= ?
				GROUP BY entity_id
			) latest ON ev.entity_id = latest.entity_id AND ev.version = latest.max_ver
			WHERE ev.tenant_id = ? AND ev.change_type != 'DELETED'`,
			string(s.tenantID), modelRef.EntityName, modelRef.ModelVersion, timeToMicro(*asAt), string(s.tenantID)).Scan(&count)
		if err != nil {
			return 0, fmt.Errorf("count entities as at: %w", err)
		}
		return count, nil
	}
```

`CountByState` with `asAt`: the same subquery, `SELECT COALESCE(json_extract(json(ev.meta), '$.state'), '') AS state, COUNT(*) … GROUP BY state`, with the `IN (…)` state filter applied to `json_extract(json(ev.meta), '$.state')` when `states != nil`. Use the field that `readDB` has in this file (`s.readDB` — confirm the name used by `getPageAsAt` at `entity_store.go:1252`). Fix every in-plugin caller and test with `nil`.

- [ ] **Step 7: Grouped stats at an instant — push down**

In `grouped_stats.go`, delete the decline at `:225-228` and the "PIT pushdown is out of scope for v1" comment (`:24-32` header bullet and `:214-216`). Build the aggregate over the snapshot base when `opts.PointInTime != nil`:

```go
	from := " FROM entities WHERE tenant_id = ? AND model_name = ? AND model_version = ? AND NOT deleted"
	args := []any{string(s.tenantID), model.EntityName, model.ModelVersion}
	if opts.PointInTime != nil {
		base, baseArgs := s.searchSnapshotBase(spi.SearchOptions{ModelName: model.EntityName, ModelVersion: model.ModelVersion}, timeToMicro(*opts.PointInTime))
		from = " FROM (" + base + ") pit WHERE 1 = 1"
		args = baseArgs
	}
```

The snapshot base projects `json(ev.data)` / `json(ev.meta)` under different column names than `entities` (`data`, `meta`); alias them in a wrapping select (`SELECT data AS data, meta AS meta …`) or adjust `groupExprToSQL`/`aggregateExprToSQL`/the filter plan to the projected names. Read `grouped_stats.go:232-330` and make the column names match before running the tests. The point-in-time query reads on `readDB` (committed-only).

Update the tests that asserted the decline: `plugins/sqlite/grouped_stats_test.go:724` (now expects buckets, not `ErrAggregationNotPushdownable`), `:583` subtest `PointInTimeBeatsMalformedPath` (a malformed path now returns its path error at a point in time too — assert that), and the "limitation" comment at `:308-322` (committed-only at an instant inside a transaction is the contract; reword).

- [ ] **Step 8: Run tests**

Run: `cd plugins/sqlite && go test ./... > /tmp/t4-sqlite.log 2>&1; grep -n -E '^(--- FAIL|FAIL|panic:)|FAIL:' /tmp/t4-sqlite.log`
Expected: no hits (conformance cases from Task 2 included).

- [ ] **Step 9: Commit**

```bash
git add plugins/sqlite
git commit -m "feat(sqlite): ConsistencyTime with a durable high-water mark; fail-closed floor; counts and grouped stats at an instant"
```

---

### Task 5: postgres — stamp floor, in-flight marker, `ConsistencyTime`

**Files:**
- Create: `plugins/postgres/migrations/000016_consistency_time.up.sql`, `.down.sql`
- Modify: `plugins/postgres/transaction_manager.go:363-367` (stamp), new `ConsistencyTime`
- Modify: `plugins/postgres/entity_store.go:395-399` (non-tx stamp)
- Modify: `plugins/postgres/transaction_manager.go:931+` (`classifySQLState`: `55P03` from the stamp)
- Create: `plugins/postgres/consistency_time.go` (the Go side: budget, error mapping, connection handling)
- Test: `plugins/postgres/consistency_time_test.go` (new)

**Interfaces:**
- Consumes: Task 1 SPI.
- Produces: `(*TransactionManager).ConsistencyTime(ctx) (time.Time, error)`; SQL functions `cyoda_stamp(text)`, `cyoda_consistency_time(text, bigint)`; sequence `cyoda_stamp_floor`.

- [ ] **Step 1: Write the failing tests**

Create `plugins/postgres/consistency_time_test.go`. Use the package's existing test-DB helpers (`newTestPool`, `testDBURL`, the factory constructor used by `commit_instant_test.go`; read `commit_instant_test.go:1-120` and `:520-590` for the helpers and the `pg_stat_activity` poll). Tests:

```go
// A transaction that has called cyoda_stamp and not committed makes
// ConsistencyTime wait; COMMIT releases it, and C is >= the held stamp.
func TestConsistencyTime_WaitsForAStampedTransaction(t *testing.T) {
	f, ctx := newCTFactory(t) // factory + tenant context; follow commit_instant_test.go
	pool := PoolForTest(f)
	holder, err := pool.Begin(context.Background())
	require.NoError(t, err)
	defer holder.Rollback(context.Background())
	var held time.Time
	require.NoError(t, holder.QueryRow(context.Background(), `SELECT cyoda_stamp($1)`, ctTenant).Scan(&held))

	tm, _ := f.TransactionManager(ctx)
	got := make(chan time.Time, 1)
	go func() {
		c, err := tm.ConsistencyTime(ctx)
		require.NoError(t, err)
		got <- c
	}()
	select {
	case <-got:
		t.Fatal("ConsistencyTime returned while a stamped transaction was open")
	case <-time.After(300 * time.Millisecond):
	}
	require.NoError(t, holder.Commit(context.Background()))
	c := <-got
	require.False(t, c.Before(held))
}

// holdStamp opens a transaction on pool, stamps it for tenant and leaves it
// open. The returned func commits it.
func holdStamp(t *testing.T, pool *pgxpool.Pool, tenant string) (stamp time.Time, commit func()) {
	t.Helper()
	tx, err := pool.Begin(context.Background())
	require.NoError(t, err)
	require.NoError(t, tx.QueryRow(context.Background(), `SELECT cyoda_stamp($1)`, tenant).Scan(&stamp))
	done := false
	t.Cleanup(func() {
		if !done {
			_ = tx.Rollback(context.Background())
		}
	})
	return stamp, func() { done = true; require.NoError(t, tx.Commit(context.Background())) }
}

// Another tenant's held stamp does not delay this tenant's C.
func TestConsistencyTime_OtherTenantDoesNotDelay(t *testing.T) {
	f, ctx := newCTFactory(t)
	_, _ = holdStamp(t, PoolForTest(f), "some-other-tenant")
	tm, _ := f.TransactionManager(ctx)
	start := time.Now()
	_, err := tm.ConsistencyTime(ctx)
	require.NoError(t, err)
	require.Less(t, time.Since(start), 200*time.Millisecond)
}

// The SQL budget is enforced for the whole call: three held markers, a
// 600 ms budget → 55P03 in about 600 ms, not 3 × 600 ms.
func TestConsistencyTimeSQL_BudgetIsPerCall(t *testing.T) {
	f, _ := newCTFactory(t)
	pool := PoolForTest(f)
	for i := 0; i < 3; i++ {
		holdStamp(t, pool, ctTenant)
	}
	start := time.Now()
	var c time.Time
	err := pool.QueryRow(context.Background(), `SELECT cyoda_consistency_time($1, 600)`, ctTenant).Scan(&c)
	var pgErr *pgconn.PgError
	require.ErrorAs(t, err, &pgErr)
	require.Equal(t, "55P03", pgErr.Code)
	require.Less(t, time.Since(start), 1200*time.Millisecond)
}

// The Go mapping: a budget overrun is ErrConsistencyTimeUnavailable; a caller
// that gives up gets its own context error.
func TestConsistencyTime_ErrorMapping(t *testing.T) {
	f, ctx := newCTFactoryWithStatementTimeout(t, 300*time.Millisecond) // TM built with that timeout
	holdStamp(t, PoolForTest(f), ctTenant)
	tm, _ := f.TransactionManager(ctx)
	_, err := tm.ConsistencyTime(ctx)
	require.ErrorIs(t, err, spi.ErrConsistencyTimeUnavailable)

	cctx, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel()
	_, err = tm.ConsistencyTime(cctx)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.NotErrorIs(t, err, spi.ErrConsistencyTimeUnavailable)
}

// A cancel during the wait leaves no advisory lock behind, and a later stamp
// on any connection proceeds at once.
func TestConsistencyTime_CancelLeavesNoLock(t *testing.T) {
	f, ctx := newCTFactory(t)
	pool := PoolForTest(f)
	_, commit := holdStamp(t, pool, ctTenant)
	tm, _ := f.TransactionManager(ctx)
	cctx, cancel := context.WithTimeout(ctx, 150*time.Millisecond)
	defer cancel()
	_, err := tm.ConsistencyTime(cctx)
	require.Error(t, err)
	commit()
	var n int
	require.NoError(t, pool.QueryRow(context.Background(),
		`SELECT count(*) FROM pg_locks WHERE locktype = 'advisory' AND objsubid = 2 AND granted`).Scan(&n))
	require.Equal(t, 0, n, "no advisory lock may outlive a cancelled call")
	start := time.Now()
	_, commit2 := holdStamp(t, pool, ctTenant)
	commit2()
	require.Less(t, time.Since(start), 100*time.Millisecond)
}

// The floor survives a DB clock behind it (own database): set the floor an
// hour ahead; C and the next commit's stamp are both at or above it.
func TestConsistencyTime_FloorAheadOfClock(t *testing.T) {
	f, ctx := newCTFactoryOwnDB(t) // a fresh database, so moving the floor affects no other test
	pool := PoolForTest(f)
	ahead := time.Now().Add(time.Hour).UnixMicro()
	_, err := pool.Exec(context.Background(), `SELECT setval('cyoda_stamp_floor', $1, true)`, ahead)
	require.NoError(t, err)
	tm, _ := f.TransactionManager(ctx)
	c, err := tm.ConsistencyTime(ctx)
	require.NoError(t, err)
	require.GreaterOrEqual(t, c.UnixMicro(), ahead)
	txID := commitOneEntity(t, f, ctx) // Begin, Save, Commit; follow commit_instant_test.go
	submit, err := tm.GetSubmitTime(ctx, txID)
	require.NoError(t, err)
	require.True(t, submit.After(c))
}

// The commit-phase stamp timing out on the floor mutex is retryable storage
// unavailability, not a 500.
func TestStamp_LockTimeoutIsStorageUnavailable(t *testing.T) {
	f, ctx := newCTFactory(t)
	side, err := PoolForTest(f).Acquire(context.Background())
	require.NoError(t, err)
	_, err = side.Exec(context.Background(), `SELECT pg_advisory_lock(0, 0)`)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = side.Exec(context.Background(), `SELECT pg_advisory_unlock(0, 0)`)
		side.Release()
	})
	err = commitOneEntityErr(t, f, ctx) // returns the Commit error
	var su interface{ StorageUnavailable() bool }
	require.ErrorAs(t, err, &su)
	require.True(t, su.StorageUnavailable())
}

// An acquire timeout while getting C carries the storage-unavailable marker.
func TestConsistencyTime_AcquireTimeoutIsStorageUnavailable(t *testing.T) {
	f, ctx := newCTFactoryTinyPool(t) // pool of 1 connection, short acquire timeout
	held, err := PoolForTest(f).Acquire(context.Background())
	require.NoError(t, err)
	defer held.Release()
	tm, _ := f.TransactionManager(ctx)
	_, err = tm.ConsistencyTime(ctx)
	var su interface{ StorageUnavailable() bool }
	require.ErrorAs(t, err, &su)
}
```

The factory helpers (`newCTFactory`, `newCTFactoryWithStatementTimeout`, `newCTFactoryOwnDB`, `newCTFactoryTinyPool`, `commitOneEntity`, `commitOneEntityErr`, constant `ctTenant`) go in the same test file; build them from the existing test plumbing in `commit_instant_test.go`, `main_test.go` and `conformance_test.go:41-124` (per-test database: `CREATE DATABASE` on the admin pool, `Migrate`, `DropSchemaForTest` on cleanup; plugin config via the same `Config` struct `NewFactory` reads, with `StatementTimeout`, `MaxConns` and the acquire timeout set).

- [ ] **Step 2: Run to verify they fail**

Run: `cd plugins/postgres && go test -run 'TestConsistencyTime|TestStamp_' ./...`
Expected: compile error ("tm.ConsistencyTime undefined") and, once stubbed, `function cyoda_stamp(unknown) does not exist`.

- [ ] **Step 3: Migration**

`000016_consistency_time.up.sql` — exactly the SQL in spec §6.3 (sequence, seed `setval`, `cyoda_stamp`, `cyoda_consistency_time`), prefixed with this comment:

```sql
-- Consistency time. cyoda_stamp_floor holds the highest stamp or consistency
-- time ever issued, in microseconds since the epoch. Every commit calls
-- cyoda_stamp, which takes a transaction-level advisory lock
-- (hashtext(tenant), xact_key) — the in-flight marker, released after the
-- commit's rows are visible — and stamps above the floor. cyoda_consistency_time
-- reserves C above every stamp issued, then waits for the tenant's markers.
-- Advisory key layout (two-int form, objsubid = 2, used by nothing else here):
--   (0, 0)                         the floor mutex, held for microseconds
--   (hashtext(tenant), 1..2^31-1)  in-flight markers, one per committing tx
```

`000016_consistency_time.down.sql`:

```sql
DROP FUNCTION IF EXISTS cyoda_consistency_time(text, bigint);
DROP FUNCTION IF EXISTS cyoda_stamp(text);
DROP SEQUENCE IF EXISTS cyoda_stamp_floor;
```

Check that the migration-index guard test (`migration_index_guard_test.go`) and `migrate_test.go` accept the new files; extend them if they enumerate migrations.

- [ ] **Step 4: Use `cyoda_stamp` at both stamp sites**

`transaction_manager.go:365`:

```go
	// cyoda_stamp takes this transaction's in-flight marker and a stamp above
	// the floor. Design rule: nothing after this statement waits on a lock —
	// the statements below touch only rows this transaction wrote (the
	// sm_audit_events UPDATE matches this transaction's own label), so a
	// consistency-time call waiting on the marker cannot deadlock with it.
	if err := tx.QueryRow(ctx, "SELECT cyoda_stamp($1)", string(tenantID)).Scan(&instant); err != nil {
		return time.Time{}, fmt.Errorf("read commit instant: %w", classifyStampError(err))
	}
```

`entity_store.go:397` (non-transactional path; `tid` is the tenant):

```go
	// See stampCommitInstant for the design rule this statement opens.
	if err := s.q.QueryRow(ctx, `SELECT cyoda_stamp($1)`, tid).Scan(&instant); err != nil {
		return fmt.Errorf("failed to read commit instant: %w", classifyStampError(err))
	}
```

- [ ] **Step 5: Go side of `ConsistencyTime`**

Create `plugins/postgres/consistency_time.go`:

```go
package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgerrcode"
	"github.com/jackc/pgx/v5/pgconn"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// consistencyWaitBudget caps how long one ConsistencyTime call waits for the
// tenant's commits in their commit phase.
const consistencyWaitBudget = 10 * time.Second

// stampLockTimeoutError marks a commit whose stamp could not take the floor
// mutex in time. The transaction rolls back; a retry may succeed.
type stampLockTimeoutError struct{ cause error }

func (e *stampLockTimeoutError) Error() string           { return "commit stamp: lock wait exceeded: " + e.cause.Error() }
func (e *stampLockTimeoutError) Unwrap() error           { return e.cause }
func (e *stampLockTimeoutError) StorageUnavailable() bool { return true }

func classifyStampError(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == pgerrcode.LockNotAvailable {
		return &stampLockTimeoutError{cause: err}
	}
	return err
}

// waitBudgetMillis is 10 s, or the configured statement timeout when that is
// above 0 and lower (0 means no limit).
func (tm *TransactionManager) waitBudgetMillis() int64 {
	b := consistencyWaitBudget
	if st := tm.statementTimeout; st > 0 && st < b {
		b = st
	}
	return b.Milliseconds()
}

// ConsistencyTime implements spi.TransactionManager. It runs
// cyoda_consistency_time on its own pool connection, in autocommit, never on
// the caller's transaction. An error closes the connection instead of
// returning it to the pool, so no session-level lock can outlive it.
func (tm *TransactionManager) ConsistencyTime(ctx context.Context) (time.Time, error) {
	tenantID, err := resolveTenant(ctx)
	if err != nil {
		return time.Time{}, fmt.Errorf("ConsistencyTime: %w", err)
	}
	conn, err := tm.acquire(ctx) // the bounded acquire the TM already uses; see unjoinedQuerier
	if err != nil {
		return time.Time{}, fmt.Errorf("ConsistencyTime: %w", err)
	}
	var c time.Time
	qerr := conn.QueryRow(ctx, `SELECT cyoda_consistency_time($1, $2)`, string(tenantID), tm.waitBudgetMillis()).Scan(&c)
	if qerr != nil {
		_ = conn.Conn().Close(context.Background())
		conn.Release()
		if ctxErr := ctx.Err(); ctxErr != nil {
			return time.Time{}, fmt.Errorf("ConsistencyTime: %w", ctxErr)
		}
		var pgErr *pgconn.PgError
		if errors.As(qerr, &pgErr) && (pgErr.Code == pgerrcode.LockNotAvailable || pgErr.Code == pgerrcode.QueryCanceled) {
			return time.Time{}, fmt.Errorf("ConsistencyTime: %w: %w", spi.ErrConsistencyTimeUnavailable, qerr)
		}
		return time.Time{}, fmt.Errorf("ConsistencyTime: %w", classifyError(qerr))
	}
	conn.Release()
	return c, nil
}
```

`tm.statementTimeout` and `tm.acquire`: add a `statementTimeout time.Duration` field set from the plugin config where the TM is built (`store_factory.go:333`, a `TransactionManagerOption`), and use the existing bounded-acquire helper (read `unjoined_querier.go` and `ceilings.go:100-125` for `acquireTimeoutError`; reuse that helper rather than `pool.Acquire` directly so an acquire timeout carries the storage-unavailable marker). `stampOwnCommitInstant`'s and the TM's stamp errors still pass through `classifyError` afterwards; `stampLockTimeoutError` must survive it (check `classifySQLState` does not re-wrap a non-`PgError` top-level; it unwraps with `errors.As`, so add `case errors.As(err, new(*stampLockTimeoutError))` → return as-is before the switch).

- [ ] **Step 6: Run tests**

Run: `cd plugins/postgres && go test ./... > /tmp/t5-postgres.log 2>&1; grep -n -E '^(--- FAIL|FAIL|panic:)|FAIL:' /tmp/t5-postgres.log`
Expected: no hits; the `ConsistencyTime/*` conformance cases run here too (the `Count/AsAt` cases still fail until Task 6 — if so, list them in the log check and proceed; Task 6 turns them green).

- [ ] **Step 7: Commit**

```bash
git add plugins/postgres
git commit -m "feat(postgres): stamp floor and in-flight marker; ConsistencyTime by reserve-then-wait"
```

---

### Task 6: postgres — committed-only history, guard removal, counts and grouped stats at an instant

**Files:**
- Modify: `plugins/postgres/search_base.go:54-71` (template), comment `:120-147`
- Modify: `plugins/postgres/entity_store.go:604-611` (`GetAsAt`), `:890-949` (`Count`, `CountByState`), `:1136,1161` (`GetVersionMetadata`)
- Modify: `plugins/postgres/grouped_stats.go:31-35,374-388,444`
- Move: `plugins/postgres/migrate.go:365` `dropSchema` → `plugins/postgres/migrate_testhelpers_test.go` (keep `DropSchemaForTest` in `export_test.go` working)
- Modify: `plugins/postgres/pit_committed_only_test.go:12-19`, `plugins/postgres/pit_time_test.go:11-38` (comments), `plugins/postgres/grouped_stats_test.go:552`

- [ ] **Step 1: Write failing tests**

- `TestGetVersionMetadata_CommittedOnlyInTx` (plugin-level, mirrors the spitest case so the failure is local and fast).
- `TestPIT_FloorAheadOfDBClock_RowsVisible`: own database; `setval` the floor 1 h ahead; commit a save; `GetAsAt(ctx, id, C)` with `C` from `ConsistencyTime` finds it (fails today because of `transaction_time <= CURRENT_TIMESTAMP`).
- `TestGroupedAggregate_PointInTimeIsPushedDown`: replaces the assertion at `grouped_stats_test.go:552` — expects buckets equal to the streaming result for the same instant.

- [ ] **Step 2: Run to verify they fail**

Run: `cd plugins/postgres && go test -run 'TestGetVersionMetadata_CommittedOnlyInTx|TestPIT_FloorAheadOfDBClock|TestGroupedAggregate_PointInTimeIsPushedDown' ./...`
Expected: FAIL (two versions listed; not found; `ErrAggregationNotPushdownable`).

- [ ] **Step 3: Implement**

- Delete `AND ev.transaction_time <= CURRENT_TIMESTAMP` from `pitBaseQueryTemplate` and from `GetAsAt`. Rewrite the `committedQuerier` comment's sentence about the guard: the pool pin is what reads committed state; the consistency-time fence (engine) is what makes an instant final.
- `GetVersionMetadata`: replace `s.q` with `s.committedQuerier()` at both statements.
- `Count(ctx, modelRef, asAt)`: when `asAt != nil`,

```go
		var count int64
		base, args := s.searchBaseQuery(modelRef.EntityName, modelRef.ModelVersion, asAt)
		err := s.committedQuerier().QueryRow(ctx, `SELECT count(*) FROM (`+base+`) pit`, args...).Scan(&count)
		if err != nil {
			return 0, fmt.Errorf("failed to count entities as at: %w", err)
		}
		return count, nil
```

  `CountByState` with `asAt`: `SELECT COALESCE(doc -> '_meta' ->> 'state', '') AS state, COUNT(*) FROM (<base>) pit [WHERE doc -> '_meta' ->> 'state' = ANY($5)] GROUP BY state` on `committedQuerier()`.
- `GroupedAggregate`: delete the decline and its comment; when `opts.PointInTime != nil` use `FROM (<pitBase>) pit WHERE TRUE` with the base's four args, shift `plan.where` placeholders by `len(args)`, and run on `committedQuerier()` (the grouped-stats service only pushes down outside a transaction; committed-only is the rule anyway).
- Move `dropSchema` to a `_test.go` file; `DropSchemaForTest` keeps calling it.
- Update the stale comments listed under **Files**.

- [ ] **Step 4: Run tests**

Run: `cd plugins/postgres && go test ./... > /tmp/t6-postgres.log 2>&1; grep -n -E '^(--- FAIL|FAIL|panic:)|FAIL:' /tmp/t6-postgres.log`
Expected: no hits, conformance included.

- [ ] **Step 5: Commit**

```bash
git add plugins/postgres
git commit -m "fix(postgres): committed-only version history; drop the CURRENT_TIMESTAMP guard; counts and grouped stats at an instant"
```

---

### Task 7: engine — `internal/domain/consistency`

**Files:**
- Create: `internal/domain/consistency/consistency.go`
- Create: `internal/domain/consistency/consistency_test.go`
- Modify: `internal/common/error_codes.go` — constants only here if Task 9 has not landed; otherwise consume Task 9's constants (coordinate: Task 9 can be merged first, it is independent).

**Interfaces:**
- Consumes: `spi.TransactionManager.ConsistencyTime`, `spi.ErrConsistencyTimeUnavailable`, `common.ErrCodePointInTimeAfterConsistencyTime`, `common.ErrCodeConsistencyTimeUnavailable` (Task 9).
- Produces:
  - `func New(txMgr spi.TransactionManager) *Service`
  - `func (s *Service) Fresh(ctx context.Context) (time.Time, error)`
  - `func (s *Service) Fence(ctx context.Context, t time.Time) error`
  - errors are `*common.AppError` (refusal 400; unavailable 503) or `common.Internal(...)`; a caller's own context error is returned unwrapped-classified as `ctx.Err()`.

- [ ] **Step 1: Write the failing tests**

`consistency_test.go` with a fake TM (embed `spi.TransactionManager`, override `ConsistencyTime` with a function field and a call counter, and a gate channel to hold calls open):

```go
package consistency

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/common"
)

type fakeTM struct {
	spi.TransactionManager
	calls    atomic.Int64
	inflight atomic.Int64
	maxIn    atomic.Int64
	gate     chan struct{} // nil: return at once
	next     func() (time.Time, error)
}

func (f *fakeTM) ConsistencyTime(ctx context.Context) (time.Time, error) {
	f.calls.Add(1)
	n := f.inflight.Add(1)
	defer f.inflight.Add(-1)
	for {
		m := f.maxIn.Load()
		if n <= m || f.maxIn.CompareAndSwap(m, n) {
			break
		}
	}
	if f.gate != nil {
		select {
		case <-f.gate:
		case <-ctx.Done():
			return time.Time{}, ctx.Err()
		}
	}
	return f.next()
}

func tctx(tenant string) context.Context {
	return spi.WithUserContext(context.Background(), &spi.UserContext{
		UserID: "u", Tenant: spi.Tenant{ID: spi.TenantID(tenant), Name: tenant},
	})
}

func fixed(c time.Time) func() (time.Time, error) { return func() (time.Time, error) { return c, nil } }

func TestFence_PassesBelowCachedHighWithoutAStoreCall(t *testing.T) {
	c := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	tm := &fakeTM{next: fixed(c)}
	s := New(tm)
	_, err := s.Fresh(tctx("a"))
	require.NoError(t, err)
	require.NoError(t, s.Fence(tctx("a"), c.Add(-time.Hour)))
	require.Equal(t, int64(1), tm.calls.Load())
}

func TestFence_RefusesLaterInstantWithC(t *testing.T) {
	c := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	s := New(&fakeTM{next: fixed(c)})
	err := s.Fence(tctx("a"), c.Add(time.Millisecond))
	var appErr *common.AppError
	require.ErrorAs(t, err, &appErr)
	require.Equal(t, http.StatusBadRequest, appErr.Status)
	require.Equal(t, common.ErrCodePointInTimeAfterConsistencyTime, appErr.Code)
	require.Equal(t, c.Format(time.RFC3339Nano), appErr.Props["consistencyTime"])
	require.False(t, appErr.Retryable)
}

// Review Focus 2: an instant with a non-UTC offset equal to C is served.
func TestFence_ComparesInstantsNotStrings(t *testing.T) {
	c := time.Date(2026, 10, 5, 14, 3, 7, 123456000, time.UTC)
	s := New(&fakeTM{next: fixed(c)})
	same := c.In(time.FixedZone("+02", 2*3600))
	require.NoError(t, s.Fence(tctx("a"), same))
}

// Review Focus 3: an ancient instant on a fresh node passes.
func TestFence_AncientInstantPasses(t *testing.T) {
	s := New(&fakeTM{next: fixed(time.Now())})
	require.NoError(t, s.Fence(tctx("a"), time.Time{}.Add(time.Nanosecond)))
	require.NoError(t, s.Fence(tctx("a"), time.Date(1, 1, 1, 0, 0, 0, 0, time.UTC)))
}

// Review Focus 4: tenants are keyed exactly.
func TestFence_TenantKeysAreExact(t *testing.T) {
	c := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	tm := &fakeTM{next: fixed(c)}
	s := New(tm)
	_, err := s.Fresh(tctx("Acme"))
	require.NoError(t, err)
	require.NoError(t, s.Fence(tctx("acme"), c.Add(-time.Hour)))
	require.Equal(t, int64(2), tm.calls.Load(), "acme must not use Acme's cached C")
}

// Review Focus 5: a burst of misses holds at most two calls in flight.
func TestFence_BurstHoldsAtMostTwoCalls(t *testing.T) {
	gate := make(chan struct{})
	c := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	tm := &fakeTM{gate: gate, next: fixed(c)}
	s := New(tm)
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if i%2 == 0 {
				_, _ = s.Fresh(tctx("a"))
			} else {
				_ = s.Fence(tctx("a"), c.Add(-time.Second))
			}
		}(i)
	}
	time.Sleep(100 * time.Millisecond)
	close(gate)
	wg.Wait()
	require.LessOrEqual(t, tm.maxIn.Load(), int64(2))
}

func TestFresh_DoesNotJoinACallStartedBeforeIt(t *testing.T) {
	// Call 1 is held; Fresh starts call 2 and returns call 2's result.
	gate := make(chan struct{})
	var n atomic.Int64
	tm := &fakeTM{gate: gate, next: func() (time.Time, error) {
		return time.Unix(n.Add(1), 0), nil
	}}
	s := New(tm)
	go func() { _, _ = s.Fresh(tctx("a")) }()
	time.Sleep(30 * time.Millisecond) // call 1 in flight
	got := make(chan time.Time, 1)
	go func() { c, _ := s.Fresh(tctx("a")); got <- c }()
	time.Sleep(30 * time.Millisecond)
	close(gate)
	require.Equal(t, int64(2), (<-got).Unix(), "Fresh must use a call that started after it")
}

func TestFresh_CallerCancelDoesNotFailSharers(t *testing.T) {
	gate := make(chan struct{})
	c := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	s := New(&fakeTM{gate: gate, next: fixed(c)})
	cctx, cancel := context.WithCancel(tctx("a"))
	errA := make(chan error, 1)
	go func() { _, err := s.Fresh(cctx); errA <- err }()
	time.Sleep(20 * time.Millisecond)
	resB := make(chan error, 1)
	go func() { resB <- s.Fence(tctx("a"), c.Add(-time.Second)) }()
	time.Sleep(20 * time.Millisecond)
	cancel()
	require.ErrorIs(t, <-errA, context.Canceled)
	close(gate)
	require.NoError(t, <-resB)
}

func TestErrors_UnavailableIsRetryable503(t *testing.T) {
	s := New(&fakeTM{next: func() (time.Time, error) {
		return time.Time{}, fmt.Errorf("x: %w", spi.ErrConsistencyTimeUnavailable)
	}})
	_, err := s.Fresh(tctx("a"))
	var appErr *common.AppError
	require.ErrorAs(t, err, &appErr)
	require.Equal(t, http.StatusServiceUnavailable, appErr.Status)
	require.Equal(t, common.ErrCodeConsistencyTimeUnavailable, appErr.Code)
	require.True(t, appErr.Retryable)
}

type suErr struct{}

func (suErr) Error() string             { return "down" }
func (suErr) StorageUnavailable() bool  { return true }

func TestErrors_StorageUnavailableMarker(t *testing.T) {
	s := New(&fakeTM{next: func() (time.Time, error) { return time.Time{}, suErr{} }})
	_, err := s.Fresh(tctx("a"))
	var appErr *common.AppError
	require.ErrorAs(t, err, &appErr)
	require.Equal(t, common.ErrCodeStorageUnavailable, appErr.Code)
}

func TestErrors_OtherIsInternal(t *testing.T) {
	s := New(&fakeTM{next: func() (time.Time, error) { return time.Time{}, errors.New("boom") }})
	_, err := s.Fresh(tctx("a"))
	var appErr *common.AppError
	require.ErrorAs(t, err, &appErr)
	require.Equal(t, http.StatusInternalServerError, appErr.Status)
}

func TestNew_NilTransactionManagerPanics(t *testing.T) {
	require.Panics(t, func() { New(nil) })
}
```

(Add `"fmt"` to the imports.)

- [ ] **Step 2: Run to verify they fail**

Run: `go test ./internal/domain/consistency/...`
Expected: compile error (package has no `New`).

- [ ] **Step 3: Implement**

`consistency.go`:

```go
// Package consistency fences point-in-time reads with the store's
// consistency time: a read at an instant later than the consistency time is
// refused, and a read whose instant cyoda-go chooses uses a fresh one.
package consistency

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/common"
)

// storeCallDeadline bounds one store call: the store's own wait budget (10 s)
// plus a margin, so a shared call cannot outlive its callers' patience by much.
const storeCallDeadline = 11 * time.Second

// maxInFlight is the number of store calls one tenant may have open at once
// on this node, so a commit held in its commit phase cannot drain the
// connection pool.
const maxInFlight = 2

type call struct {
	seq  uint64
	ctx  context.Context // the starter's context without its cancellation; carries the tenant
	done chan struct{}
	c    time.Time
	err  error
}

type tenantState struct {
	hi       time.Time // highest C seen; a returned C stays final forever
	inflight []*call   // oldest first, at most maxInFlight
}

// Service fences reads with the consistency time. It is safe for concurrent
// use; one instance serves the whole process.
type Service struct {
	txMgr   spi.TransactionManager
	mu      sync.Mutex
	seq     uint64 // last call sequence number handed out
	tenants map[spi.TenantID]*tenantState
}

// New returns a Service over txMgr. A nil txMgr panics: there is no fallback
// clock.
func New(txMgr spi.TransactionManager) *Service {
	if txMgr == nil {
		panic("consistency.New: nil TransactionManager")
	}
	return &Service{txMgr: txMgr, tenants: make(map[spi.TenantID]*tenantState)}
}

func tenantOf(ctx context.Context) (spi.TenantID, error) {
	uc := spi.GetUserContext(ctx)
	if uc == nil || uc.Tenant.ID == "" {
		return "", errors.New("no tenant in context")
	}
	return uc.Tenant.ID, nil
}

// Fresh returns a consistency time from a store call that started after
// Fresh was called (completeness needs that).
func (s *Service) Fresh(ctx context.Context) (time.Time, error) {
	tenant, err := tenantOf(ctx)
	if err != nil {
		return time.Time{}, common.Internal("failed to obtain the consistency time", err)
	}
	s.mu.Lock()
	entrySeq := s.seq
	s.mu.Unlock()
	for {
		cl, wait := s.freshCall(ctx, tenant, entrySeq)
		if wait != nil { // two calls in flight, both older than us
			if err := waitOn(ctx, wait); err != nil {
				return time.Time{}, err
			}
			continue
		}
		if err := waitOn(ctx, cl); err != nil {
			return time.Time{}, err
		}
		if cl.err != nil {
			return time.Time{}, classify(cl.err)
		}
		return cl.c, nil
	}
}

// Fence returns nil when t is at or before a consistency time; otherwise the
// refusal, or the error that prevented getting one.
func (s *Service) Fence(ctx context.Context, t time.Time) error {
	tenant, err := tenantOf(ctx)
	if err != nil {
		return common.Internal("failed to obtain the consistency time", err)
	}
	s.mu.Lock()
	st := s.state(tenant)
	if !t.After(st.hi) {
		s.mu.Unlock()
		return nil
	}
	var joined *call
	if n := len(st.inflight); n > 0 {
		joined = st.inflight[n-1]
	}
	s.mu.Unlock()
	if joined != nil {
		if err := waitOn(ctx, joined); err != nil {
			return err
		}
		if joined.err == nil && !t.After(joined.c) {
			return nil
		}
	}
	c, err := s.Fresh(ctx)
	if err != nil {
		return err
	}
	if t.After(c) {
		return refusal(t, c)
	}
	return nil
}

func (s *Service) state(tenant spi.TenantID) *tenantState {
	st, ok := s.tenants[tenant]
	if !ok {
		st = &tenantState{}
		s.tenants[tenant] = st
	}
	return st
}

// freshCall returns a call that started after entrySeq to join, starting one
// if allowed; or, when two older calls are in flight, the newest to wait for.
func (s *Service) freshCall(ctx context.Context, tenant spi.TenantID, entrySeq uint64) (cl, wait *call) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.state(tenant)
	for i := len(st.inflight) - 1; i >= 0; i-- {
		if st.inflight[i].seq > entrySeq {
			return st.inflight[i], nil
		}
	}
	if len(st.inflight) >= maxInFlight {
		return nil, st.inflight[len(st.inflight)-1]
	}
	s.seq++
	// One caller's cancel must not fail the others who share this call.
	cl = &call{seq: s.seq, ctx: context.WithoutCancel(ctx), done: make(chan struct{})}
	st.inflight = append(st.inflight, cl)
	go s.run(tenant, cl)
	return cl, nil
}

func (s *Service) run(tenant spi.TenantID, cl *call) {
	ctx, cancel := context.WithTimeout(cl.ctx, storeCallDeadline)
	defer cancel()
	cl.c, cl.err = s.txMgr.ConsistencyTime(ctx)
	s.mu.Lock()
	st := s.state(tenant)
	if cl.err == nil && cl.c.After(st.hi) {
		st.hi = cl.c
	}
	for i, x := range st.inflight {
		if x == cl {
			st.inflight = append(st.inflight[:i], st.inflight[i+1:]...)
			break
		}
	}
	s.mu.Unlock()
	close(cl.done)
}
```

```go
func waitOn(ctx context.Context, cl *call) error {
	select {
	case <-cl.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func refusal(t, c time.Time) *common.AppError {
	cs := c.UTC().Format(time.RFC3339Nano)
	e := common.Operational(http.StatusBadRequest, common.ErrCodePointInTimeAfterConsistencyTime,
		fmt.Sprintf("pointInTime %s is later than the consistency time %s; read at or before the consistency time (GET /entity/consistency-time returns the current one)",
			t.UTC().Format(time.RFC3339Nano), cs))
	e.Props = map[string]any{"consistencyTime": cs}
	return e
}

func classify(err error) error {
	if errors.Is(err, spi.ErrConsistencyTimeUnavailable) {
		return common.Operational(http.StatusServiceUnavailable, common.ErrCodeConsistencyTimeUnavailable,
			"the consistency time is not available yet: a save is still committing — retry").AsRetryable().WithCause(err)
	}
	return common.Internal("failed to obtain the consistency time", err)
}
```

Note: `Fresh` and `Fence` return the caller's own `ctx.Err()` unchanged when the caller gives up; the transports already map a cancelled request.

- [ ] **Step 4: Run tests**

Run: `go test -race ./internal/domain/consistency/...`
Expected: PASS (race on this package only; it is concurrency code).

- [ ] **Step 5: Commit**

```bash
git add internal/domain/consistency
git commit -m "feat(consistency): fence point-in-time reads with the store's consistency time"
```

---

### Task 8: engine — wiring (required constructor arguments, tracing wrapper, fakes)

**Files:**
- Modify: `internal/observability/tx_tracing.go` (forward `ConsistencyTime` with a span), `internal/observability/tx_tracing_test.go:14`
- Modify: `internal/domain/workflow/engine_test.go:2261`, `internal/domain/workflow/fire_scheduled_test.go:770` (fakes)
- Modify: `internal/domain/entity/handler.go:86` (`New` gains `cons *consistency.Service`), `internal/domain/entity/grouped_stats_service.go:40`, `grouped_stats_handler.go:61`
- Modify: `internal/domain/search/service.go:301` (`NewSearchService` gains `cons *consistency.Service`)
- Modify: `internal/grpc/server.go:71` (`NewServer` gains `cons *consistency.Service`)
- Modify: `app/app.go:243-250,409,537,644` and the `NewServer` call
- Modify: every test call site of `entity.New` (32), `NewSearchService` (127), `NewGroupedStatsHandler` (13), `NewGroupedStatsService` (32), `NewServer`
- Create: `internal/domain/consistency/testing.go`? **No** — no test helpers in production packages. Each test package gets an unexported helper in a `_test.go` file.

**Interfaces:**
- Consumes: Task 7 `consistency.New`.
- Produces: `entity.New(factory, txMgr, uuids, engine, gate, cons)`, `entity.NewGroupedStatsService(maxBuckets, cons)`, `entity.NewGroupedStatsHandler(resolve, maxBuckets, cons)`, `search.NewSearchService(factory, uuids, searchStore, cons)`, `grpc.NewServer(..., cons)` (last parameter). Each panics on a nil `cons`.

- [ ] **Step 1: Failing test for the tracing wrapper**

In `tx_tracing_test.go`, add `TestTracing_ForwardsConsistencyTime`: the fake TM returns a fixed time; the wrapper returns the same and records a span named `tx.consistency_time` (match the naming used by the wrapper's other methods).

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/observability/...`
Expected: compile error (wrapper lacks `ConsistencyTime`).

- [ ] **Step 3: Implement wrapper and fakes**

Add to `tx_tracing.go`, following the wrapper's `GetSubmitTime` method exactly (span name, attributes, error recording):

```go
func (t *TracingTransactionManager) ConsistencyTime(ctx context.Context) (time.Time, error) {
	ctx, span := t.tracer.Start(ctx, "tx.consistency_time")
	defer span.End()
	c, err := t.inner.ConsistencyTime(ctx)
	if err != nil {
		span.RecordError(err)
	}
	return c, err
}
```

Add `ConsistencyTime` to the three non-embedding fakes (return `time.Now(), nil` is NOT allowed where a test asserts behaviour; for these lifecycle-counting fakes return `time.Time{}, errors.New("not used by this test")`).

- [ ] **Step 4: Required constructor arguments**

Add the parameter as the last argument of each constructor, store it, and `panic` on nil with a message naming the constructor (`"entity.New: nil consistency service"`). In `app/app.go`, after the tracing wrap (`:250`):

```go
	// One consistency service for the process: it caches the highest
	// consistency time per tenant and fences every point-in-time read.
	a.consistency = consistency.New(a.transactionManager)
```

and pass `a.consistency` to `entity.New` (`:537`), `NewSearchService` (`:409`), `NewGroupedStatsHandler` (`:644`) and `NewServer`.

- [ ] **Step 5: Test call sites**

For each test package that calls a changed constructor, add one `_test.go` helper (name it `newTestConsistency`) that builds `consistency.New(tm)` from the store factory the test already has:

```go
func newTestConsistency(t *testing.T, f spi.StoreFactory) *consistency.Service {
	t.Helper()
	tm, err := f.TransactionManager(context.Background())
	if err != nil {
		t.Fatalf("transaction manager: %v", err)
	}
	return consistency.New(tm)
}
```

Where a test already has a `spi.TransactionManager` value, pass `consistency.New(thatTM)`. Then update every call site. Find them with:

```bash
grep -rln --include='*_test.go' -E 'entity\.New\(|NewSearchService\(|NewGroupedStatsHandler\(|NewGroupedStatsService\(|grpc\.NewServer\(|\bNewServer\(' . | grep -v '^./plugins'
```

A test whose factory has no transaction manager (a bare fake factory) must embed a fake TM whose `ConsistencyTime` returns a fixed instant chosen by that test — never `time.Now()` read implicitly.

- [ ] **Step 6: Run tests**

Run: `go build ./... && go vet ./... && make test > /tmp/t8-make-test.log 2>&1; grep -n -E '^(--- FAIL|FAIL|panic:)|FAIL:' /tmp/t8-make-test.log`
Expected: no hits.

- [ ] **Step 7: Commit**

```bash
git add -A internal app
git commit -m "refactor: wire one consistency service into the entity, search, grouped-stats and gRPC services"
```

---

### Task 9: error codes and help topics

**Files:**
- Modify: `internal/common/error_codes.go` (constants after `ErrCodeSearchQueueFull` `:151`; `knownErrorCodes` `:259-357`)
- Create: `cmd/cyoda/help/content/errors/POINT_IN_TIME_AFTER_CONSISTENCY_TIME.md`, `cmd/cyoda/help/content/errors/CONSISTENCY_TIME_UNAVAILABLE.md`
- Modify: `cmd/cyoda/help/content/errors.md` (ERROR CODE INDEX), `cmd/cyoda/help/content/errors/STORAGE_UNAVAILABLE.md:31`

**Interfaces:**
- Produces: `common.ErrCodePointInTimeAfterConsistencyTime = "POINT_IN_TIME_AFTER_CONSISTENCY_TIME"`, `common.ErrCodeConsistencyTimeUnavailable = "CONSISTENCY_TIME_UNAVAILABLE"`.

- [ ] **Step 1: Run the parity tests to see them pass before, then add the constants**

Add:

```go
	// ErrCodePointInTimeAfterConsistencyTime is returned when a read's
	// pointInTime is later than the consistency time: an answer at that
	// instant could still change. 400, not retryable as sent; the
	// problem's properties.consistencyTime carries the current consistency
	// time, at or before which the read is served.
	ErrCodePointInTimeAfterConsistencyTime = "POINT_IN_TIME_AFTER_CONSISTENCY_TIME"
	// ErrCodeConsistencyTimeUnavailable is returned when the store could not
	// certify a consistency time within its wait budget, typically because a
	// save of the tenant is held in its commit phase. 503, retryable.
	ErrCodeConsistencyTimeUnavailable = "CONSISTENCY_TIME_UNAVAILABLE"
```

and both in `knownErrorCodes`.

- [ ] **Step 2: Run to verify the help parity test fails**

Run: `go test ./cmd/cyoda/help/... ./internal/common/...`
Expected: FAIL in `TestErrCode_Parity` / `TestErrorIndex_ListsEveryCode` (no topic, no index line).

- [ ] **Step 3: Write the topics and index lines**

`POINT_IN_TIME_AFTER_CONSISTENCY_TIME.md`:

```markdown
---
topic: errors.POINT_IN_TIME_AFTER_CONSISTENCY_TIME
title: "POINT_IN_TIME_AFTER_CONSISTENCY_TIME — the requested instant is later than the consistency time"
stability: stable
see_also:
  - errors
  - errors.CONSISTENCY_TIME_UNAVAILABLE
  - crud
  - search
---

# errors.POINT_IN_TIME_AFTER_CONSISTENCY_TIME

## NAME

POINT_IN_TIME_AFTER_CONSISTENCY_TIME — a read asked for an instant later than the consistency time.

## SYNOPSIS

HTTP: `400` `Bad Request`. Retryable: `no` (as sent).

## DESCRIPTION

A read with `pointInTime` returns the data as at that instant, and that answer never changes afterwards. That holds only for an instant at or before the **consistency time**: the instant up to which every save is final. A later instant — in the future, or within the few milliseconds a save takes to commit — could still gain saves, so the read is refused instead of answered.

The problem's `properties.consistencyTime` carries the current consistency time. Read at that instant or earlier. To read "as of now" with a stable answer, take the consistency time from `GET /api/entity/consistency-time` and pass it as `pointInTime`; a read at that value is never refused. A client clock that runs ahead of the store's clock is the usual cause of this error.

## SEE ALSO

- errors
- errors.CONSISTENCY_TIME_UNAVAILABLE
- crud
- search
```

`CONSISTENCY_TIME_UNAVAILABLE.md`:

```markdown
---
topic: errors.CONSISTENCY_TIME_UNAVAILABLE
title: "CONSISTENCY_TIME_UNAVAILABLE — the store could not provide a consistency time in time"
stability: stable
see_also:
  - errors
  - errors.POINT_IN_TIME_AFTER_CONSISTENCY_TIME
  - errors.STORAGE_UNAVAILABLE
  - crud
---

# errors.CONSISTENCY_TIME_UNAVAILABLE

## NAME

CONSISTENCY_TIME_UNAVAILABLE — the store could not certify a consistency time within its wait budget.

## SYNOPSIS

HTTP: `503` `Service Unavailable`. Retryable: `yes`.

## DESCRIPTION

To give a consistency time, the store waits for your tenant's saves that are in their commit phase to finish. That normally takes milliseconds. When a save stays in its commit phase longer than the store's wait budget (10 seconds, or `CYODA_POSTGRES_STATEMENT_TIMEOUT` when lower), the request is refused rather than answered with an instant that could still change. Nothing is created.

Raised by `GET /api/entity/consistency-time`, by an async search submitted without `pointInTime`, and by any read with a `pointInTime` that is not already known to be at or before the consistency time. Retry after a short back-off. Repeated occurrences mean a commit is stalling — for example a node that died mid-commit; on postgres such a session ends within seconds.

## SEE ALSO

- errors
- errors.POINT_IN_TIME_AFTER_CONSISTENCY_TIME
- errors.STORAGE_UNAVAILABLE
- crud
```

Index lines in `errors.md`:

```markdown
- `errors.POINT_IN_TIME_AFTER_CONSISTENCY_TIME` — `400` — not retryable — the read's pointInTime is later than the consistency time
- `errors.CONSISTENCY_TIME_UNAVAILABLE` — `503` — retryable — the store could not certify a consistency time within its wait budget
```

`STORAGE_UNAVAILABLE.md:31`: add one sentence: "A statement timeout while the store is producing a consistency time is reported as `CONSISTENCY_TIME_UNAVAILABLE`, not as a `500`."

- [ ] **Step 4: Run tests**

Run: `go test ./cmd/cyoda/help/... ./internal/common/...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/common cmd/cyoda/help/content
git commit -m "feat(errors): POINT_IN_TIME_AFTER_CONSISTENCY_TIME and CONSISTENCY_TIME_UNAVAILABLE"
```

---

### Task 10: engine — fences in the entity service; stats and gRPC get-all honour `pointInTime`

**Files:**
- Modify: `internal/domain/entity/service.go` (`GetEntity` `:408-420`, `ListEntities` `:1888-1923`, `GetChangesMetadata` `:783-795`, `DeleteEntitiesConditional` `:1278-1294`, `GetStatistics*` `:466,520,564,610`)
- Modify: `internal/domain/entity/handler.go:396,421,452,477` (pass `params.PointInTime`)
- Modify: `internal/grpc/search.go:284-287` (get-all), `:441-537` (stats ×2)
- Modify: `internal/domain/model/service.go:377,443` (`Count(..., nil)`)
- Modify: `internal/domain/entity/handler_test.go:1499-1530` (future instant now refused)
- Test: `internal/domain/entity/consistency_fence_test.go` (new)

**Interfaces:**
- Consumes: Task 7 `(*consistency.Service).Fence`; Task 8 handler field `h.cons`.
- Produces: `GetStatistics(ctx, pointInTime *time.Time)`, `GetStatisticsByState(ctx, states *[]string, pointInTime *time.Time)` (keep the existing parameter order, append `pointInTime`), and the ForModel variants likewise.

- [ ] **Step 1: Write the failing unit tests**

`consistency_fence_test.go` builds a memory factory, a fake TM whose `ConsistencyTime` returns a fixed `C`, `consistency.New(fake)`, and `entity.New(..., cons)`. One test per call site, each asserting that `T = C + 1ms` returns the 400 refusal and `T = C` succeeds:

```go
func TestFence_GetEntity(t *testing.T)                 { /* GetEntity with PointInTime C+1ms → appErr.Code == ErrCodePointInTimeAfterConsistencyTime; C → entity */ }
func TestFence_GetEntity_MissingEntityIsRefusedFirst(t *testing.T) { /* unknown id, T = C+1ms → 400, not 404 */ }
func TestFence_ListEntities_PageSizeZero(t *testing.T) { /* pageSize 0, T = C+1ms → 400 */ }
func TestFence_GetChangesMetadata(t *testing.T)        { /* … */ }
func TestFence_Delete_ModelNotFoundBeforeFence(t *testing.T) { /* unknown model, T = C+1ms → 404 MODEL_NOT_FOUND */ }
func TestFence_Delete_BothPaths(t *testing.T)          { /* batchSize 0 and batchSize 10, T = C+1ms → 400; no transaction begun (fake TM Begin counter == 0) */ }
func TestFence_Stats_AllVariants(t *testing.T)         { /* four functions, T = C+1ms → 400 even with no models; T between two commits → counts as at T */ }
func TestFence_PropagatesUnavailable(t *testing.T)     { /* fake returns ErrConsistencyTimeUnavailable → 503 CONSISTENCY_TIME_UNAVAILABLE from each call site */ }
```

Write each body in full (memory factory; save entities through the handler or the store; read `service_list_test.go:57-130` for the existing setup pattern).

- [ ] **Step 2: Run to verify they fail**

Run: `go test -run 'TestFence_' ./internal/domain/entity/...`
Expected: FAIL (reads served at T > C).

- [ ] **Step 3: Implement the call sites**

- `GetEntity`: in `case input.PointInTime != nil:` call `if err := h.cons.Fence(ctx, *input.PointInTime); err != nil { return nil, err }` before `GetAsAt`.
- `ListEntities`: after the model checks and before `if pageSize > 0` (`:1922`): `if pointInTime != nil { if err := h.cons.Fence(ctx, *pointInTime); err != nil { return nil, err } }`.
- `GetChangesMetadata`: before `GetVersionMetadata` when `pointInTime` is non-nil and non-zero.
- `DeleteEntitiesConditional`: after the condition parse (`:1290`), before the `batchSize` branch:

```go
	if pointInTime != nil {
		// The model check and the fence run before any transaction opens, so
		// no pooled connection is held while the fence waits, and an unknown
		// model keeps its 404 ahead of the refusal. Both paths below keep
		// their own in-scope model check.
		modelStore, err := h.factory.ModelStore(ctx)
		if err != nil {
			return nil, common.Internal("failed to access model store", err)
		}
		if _, err := modelStore.Get(ctx, ref); err != nil {
			if errors.Is(err, spi.ErrNotFound) {
				return nil, common.Operational(http.StatusNotFound, common.ErrCodeModelNotFound,
					fmt.Sprintf("cannot find model entityName=%s, version=%s", ref.EntityName, ref.ModelVersion))
			}
			return nil, common.Internal("failed to load model", err)
		}
		if err := h.cons.Fence(ctx, *pointInTime); err != nil {
			return nil, err
		}
	}
```

- `GetStatistics*`: append `pointInTime *time.Time`; after `EnsureModelRegistered` (ForModel variants) / after `modelStore.GetAll` (all-models variants), unconditionally `if pointInTime != nil { if err := h.cons.Fence(ctx, *pointInTime); err != nil { return nil, err } }`; pass `pointInTime` to `Count`/`CountByState`.
- HTTP stats handlers pass `params.PointInTime`; gRPC stats handlers pass `req.PointInTime`; gRPC get-all passes `req.PointInTime` instead of `nil`.
- `model/service.go:377,443`: `Count(ctx, ref, nil)`.
- `handler_test.go:1512`: the future instant now expects the 400 refusal; add a second case at `C` that expects the full history.

- [ ] **Step 4: Run tests**

Run: `go test ./internal/domain/entity/... ./internal/domain/model/... ./internal/grpc/...` then `make test > /tmp/t10-make-test.log 2>&1; grep -n -E '^(--- FAIL|FAIL|panic:)|FAIL:' /tmp/t10-make-test.log`
Expected: no hits.

- [ ] **Step 5: Commit**

```bash
git add internal
git commit -m "feat(entity): fence point-in-time reads; stats and gRPC get-all honour pointInTime"
```

---

### Task 11: engine — fences in search, grouped stats and transitions; async default; dead branch

**Files:**
- Modify: `internal/domain/search/service.go` (`Search` `:662-737`, `SubmitAsync` `:911-1000`, async paging `:1494-1516`)
- Modify: `internal/domain/search/handler.go:346-347` (comment)
- Delete test: `internal/domain/search/job_lookup_outage_test.go:364` (the skip assertion) — replace with a unit test that a `GetAsAt` not-found during paging is an internal error
- Modify: `internal/domain/entity/grouped_stats_service.go:50-300`
- Modify: `internal/domain/entity/transitions_handler.go:83-96`
- Test: `internal/domain/search/consistency_fence_test.go` (new), `internal/domain/entity/grouped_stats_fence_test.go` (new), `internal/domain/entity/transitions_handler_test.go` (extend)

**Interfaces:**
- Consumes: Task 7, Task 8 (`s.cons` on `SearchService`, `svc.cons` on `GroupedStatsService`, `h.cons` on `Handler`).

- [ ] **Step 1: Write the failing tests**

- `TestSubmitAsync_DefaultInstantComesFromTheStore`: fake TM returns `C = now + 1h`; submit with no `pointInTime`; the stored job's `PointInTime` equals `C` exactly (fails today: `time.Now()`).
- `TestSubmitAsync_UnavailableCreatesNoJob`: fake returns `ErrConsistencyTimeUnavailable` → 503 `CONSISTENCY_TIME_UNAVAILABLE`, and the search store holds no job.
- `TestSubmitAsync_CapPreCheckWinsOverFence`: tenant at its cap and `T = C + 1ms` → `SEARCH_QUEUE_FULL`.
- `TestSubmitAsync_FenceRefusesLaterInstant`, `TestSearch_FenceRefusesLaterInstant`.
- `TestAsyncResults_NotFoundIsInternal`: a fake store whose `GetAsAt` returns `spi.ErrNotFound` during paging → internal error (not a short page).
- `TestGroupedStats_FenceRefusesLaterInstant`; `TestGroupedStats_PathErrorBeatsFence` (malformed path with `T = C + 1ms` → path 400).
- `TestTransitions_FenceOnPointInTimeAndTransactionID`.

- [ ] **Step 2: Run to verify they fail**

Run: `go test -run 'TestSubmitAsync_|TestSearch_Fence|TestAsyncResults_NotFound|TestGroupedStats_Fence|TestGroupedStats_PathErrorBeatsFence|TestTransitions_Fence' ./internal/domain/...`
Expected: FAIL.

- [ ] **Step 3: Implement**

- `SubmitAsync`: delete `:980-983`. Between the cap pre-check (`:993-997`) and `jobID :=` (`:999`):

```go
	// The job's instant: the caller's, fenced, or a fresh consistency time.
	// Either way it is final, so every page and any reclaim on another node
	// read the same answer.
	if opts.PointInTime != nil {
		if err := s.cons.Fence(ctx, *opts.PointInTime); err != nil {
			return "", err
		}
	} else {
		c, err := s.cons.Fresh(ctx)
		if err != nil {
			return "", err
		}
		opts.PointInTime = &c
	}
```

- `Search`: after query validation, before `store.Search`: fence when `opts.PointInTime != nil`.
- Async paging: delete the `errors.Is(err, spi.ErrNotFound)` skip branch and its warning; any `GetAsAt` error returns `common.Internal("failed to read an async search result", err)`. Delete the comment at `handler.go:346-347`.
- `QueryGroupedStats`: after request/path validation, before pushdown/`Iterate`: fence when `req.PointInTime != nil`.
- Transitions: after the instant is resolved (both sources), before `GetAsAt`: `if err := h.cons.Fence(ctx, pointInTime); err != nil { common.WriteError(w, r, err); return }`.

- [ ] **Step 4: Run tests**

Run: `go test ./internal/domain/...` then `make test > /tmp/t11-make-test.log 2>&1; grep -n -E '^(--- FAIL|FAIL|panic:)|FAIL:' /tmp/t11-make-test.log`
Expected: no hits.

- [ ] **Step 5: Commit**

```bash
git add internal
git commit -m "feat(search): async default from the store's consistency time; fence direct, async, grouped and transitions reads"
```

---

### Task 12: API — `GET /entity/consistency-time` and the gRPC request

**Files:**
- Modify: `api/openapi.yaml` (new path near `/entity/stats` `:976`, `ConsistencyTimeDto` near `ModelStatsDto` `:11062`)
- Regenerate: `api/generated.go` (`go generate ./api/...`)
- Modify: `internal/api/server.go`, `internal/api/unimplemented.go` (delegation + stub)
- Modify: `internal/domain/entity/handler.go` (method `GetConsistencyTime`)
- Create: `docs/cyoda/schema/search/EntityConsistencyTimeGetRequest.json`, `EntityConsistencyTimeResponse.json`
- Modify: `docs/cyoda/schema/common/CloudEventType.json`, `internal/grpc/cloudevent_types.go:58-78`, `internal/grpc/search.go:29-56` (unary case), `internal/grpc/errors.go` (error builder)
- Regenerate: `api/grpc/events/types.go` (`./scripts/generate-events.sh`)
- Test: `internal/domain/entity/consistency_time_handler_test.go`, `internal/grpc/consistency_time_test.go`, `internal/e2e/consistency_time_test.go`

**Interfaces:**
- Produces: HTTP `GET /entity/consistency-time` → `{"consistencyTime": "…"}`; gRPC `EntityConsistencyTimeGetRequest` → `EntityConsistencyTimeResponse{consistencyTime?}`.

- [ ] **Step 1: OpenAPI**

Add under `paths`:

```yaml
  /entity/consistency-time:
    get:
      tags: [<the tag /entity/stats uses>]
      summary: Get the consistency time
      description: >-
        Returns the consistency time for the caller's tenant: the instant up to
        which every save is final. It includes every save confirmed before this
        call, on any node. A read with pointInTime at or before this value is
        never refused and its answer never changes. See `cyoda help crud`.
      operationId: getConsistencyTime
      responses:
        '200':
          description: The current consistency time.
          content:
            application/json:
              schema:
                $ref: '#/components/schemas/ConsistencyTimeDto'
        '401':
          $ref: '#/components/responses/Unauthorized'
        '403':
          $ref: '#/components/responses/Forbidden'
        '500':
          $ref: '#/components/responses/InternalServerError'
        '503':
          $ref: '#/components/responses/ServiceUnavailable'
```

and under `components/schemas`:

```yaml
    ConsistencyTimeDto:
      type: object
      required: [consistencyTime]
      properties:
        consistencyTime:
          type: string
          format: date-time
          description: >-
            The consistency time, at the store's full precision. Pass it
            unchanged as pointInTime.
```

Add `CONSISTENCY_TIME_UNAVAILABLE` to the `ServiceUnavailable` response's code list (`:12234-12254`).

- [ ] **Step 2: Generate and write the failing handler test**

Run: `go generate ./api/...` — compile fails until `server.go`/`unimplemented.go` gain the method. Write `TestGetConsistencyTime_ReturnsFreshC` (fake TM returns `C` with nanoseconds; the body's `consistencyTime` parses back to exactly `C`) and `TestGetConsistencyTime_Unavailable503`.

- [ ] **Step 3: Implement the handler**

```go
// GetConsistencyTime implements GET /entity/consistency-time.
func (h *Handler) GetConsistencyTime(w http.ResponseWriter, r *http.Request) {
	c, err := h.cons.Fresh(r.Context())
	if err != nil {
		common.WriteError(w, r, err)
		return
	}
	common.WriteJSON(w, http.StatusOK, genapi.ConsistencyTimeDto{ConsistencyTime: c})
}
```

`encoding/json` renders `time.Time` as RFC 3339 with nanoseconds, never rounded — the property the spec requires. Add the delegation in `internal/api/server.go` and the stub in `unimplemented.go` following `:54-60` / `:23-25`.

- [ ] **Step 4: gRPC schemas and dispatch**

`EntityConsistencyTimeGetRequest.json` (copy the structure of `EntityGetRequest.json`: draft 2020-12, `$id https://cyoda.com/cloud/event/search/EntityConsistencyTimeGetRequest.json`, `allOf: [{"$ref": "../common/BaseEvent.json"}]`, no extra properties beyond the base). `EntityConsistencyTimeResponse.json` (copy `EntityResponse.json`'s envelope; add `consistencyTime: {type: string, format: date-time, description: "Set when success is true."}`, not in `required`). Add both names to `CloudEventType.json`. Run `./scripts/generate-events.sh` (needs `go-jsonschema` in `$GOPATH/bin`). Add the constants in `cloudevent_types.go` and a `case` in the unary `EntitySearch` switch calling:

```go
func (s *CloudEventsServiceImpl) handleConsistencyTimeGetRequest(ctx context.Context, ce *cloudevent.Event, req events.EntityConsistencyTimeGetRequestJson) (*cloudevent.Event, error) {
	c, err := s.cons.Fresh(ctx)
	if err != nil {
		return s.consistencyTimeError(ctx, req.ID, err)
	}
	resp := events.EntityConsistencyTimeResponseJson{ID: newEventID(), RequestID: req.ID, Success: true, ConsistencyTime: &c}
	return s.marshalResponse(EntityConsistencyTimeResponse, resp)
}
```

Match the exact helper names in `internal/grpc/search.go` (`handleEntityGetRequest`, `entityResponseError`) — copy their structure; `consistencyTimeError` follows `entityStatsError` (`internal/grpc/errors.go:347-362`).

- [ ] **Step 5: Tests on every door**

- `internal/grpc/consistency_time_test.go`: 200 envelope with `consistencyTime`; a TM wrapper returning `ErrConsistencyTimeUnavailable` → `Success=false`, `CLIENT_ERROR`, message prefix `CONSISTENCY_TIME_UNAVAILABLE`, `Retryable=true` (use the `onCommitTxMgr` pattern, `entity_timeout_test.go:280-298`).
- `internal/e2e/consistency_time_test.go` on the main validated stack: `GET /entity/consistency-time` → 200 and a time; 401 without a token; 403 with a token lacking `ROLE_M2M` (follow `route_guard_test.go`); **Review Focus 1:** create an entity, take `C`, `GET /entity/{id}?pointInTime=<C string verbatim>` → 200 (no re-formatting of the string).
- `cmd/cyoda/help/content/grpc.md`: add both message types to MESSAGE TYPES and to the unary RPC's list; fix `:89` and `:93` (spec §7.3). `TestGRPCEventTypeCatalogueParity` must pass.

- [ ] **Step 6: Run tests**

Run: `make check-codegen && go test ./internal/api/... ./internal/domain/entity/... ./internal/grpc/... ./docs/cyoda/schema/... ./cmd/cyoda/help/... ./app/...` then `go test -timeout 30m -run 'TestConsistencyTimeEndpoint' ./internal/e2e/...`
Expected: PASS (route classification tests included).

- [ ] **Step 7: Commit**

```bash
git add api internal docs/cyoda/schema cmd/cyoda/help scripts
git commit -m "feat(api): GET /entity/consistency-time and EntityConsistencyTimeGetRequest"
```

---

### Task 13: e2e — refusal and 200 on every fenced operation; check order; existing tests

**Files:**
- Create: `internal/e2e/consistency_fence_test.go`
- Modify: `internal/e2e/entity_delete_unconditional_test.go:97-108`, `internal/e2e/grouped_stats_invalid_path_test.go:48-52,111-118,174-182`, `internal/e2e/zzz_errorcode_matrix_test.go` (declarations), `internal/e2e/async_stream_test.go:907`, `internal/e2e/scheduled_run_fencing_test.go:382`
- Create: `internal/e2e/async_default_floor_ahead_test.go`

- [ ] **Step 1: Write the tests**

`consistency_fence_test.go` — a helper `consistencyTime(t) string` calls `GET /entity/consistency-time` with `doAuth` and returns the string; `later(c string) string` adds 1 ms. Then one subtest per fenced HTTP operation, each with two cases — `pointInTime = later(C)` → 400, body `errorCode = POINT_IN_TIME_AFTER_CONSISTENCY_TIME` and `properties.consistencyTime` present; `pointInTime = C` → 2xx:

| Subtest | Request |
|---|---|
| `GetOneEntity` | `GET /entity/{id}?pointInTime=` |
| `GetOneEntity_MissingEntity` | unknown id, later(C) → 400 (check order) |
| `GetOneEntity_InTransaction` | inside a joined transaction (`X-Tx-Token`), later(C) → 400; C → committed revision |
| `GetAllEntities` | `GET /entity/{name}/{version}?pointInTime=` |
| `SearchDirect` | `POST /search/direct/{name}/{version}` body with `pointInTime` (follow `search_test.go` request shapes) |
| `SearchAsyncSubmit` | `POST /search/async/{name}/{version}?pointInTime=` |
| `DeleteEntities` | `DELETE /entity/{name}/{version}?pointInTime=` |
| `Stats`, `StatsForModel`, `StatsByState`, `StatsByStateForModel` | the four GETs with `pointInTime`; also assert counts at an instant between two saves |
| `GroupedStats` | `POST /entity/stats/{name}/{version}/query` body `pointInTime` |
| `ChangesMetadata` | `GET /entity/{id}/changes?pointInTime=` |
| `Transitions_PointInTime`, `Transitions_TransactionID` | `GET /entity/{id}/transitions?pointInTime=` / `?transactionId=` (transaction id case expects 200) |

`async_default_floor_ahead_test.go` — own database via `newSchedDB(t)` and `newStackOn(t, s, nil)`; raise the floor an hour ahead (`SELECT setval('cyoda_stamp_floor', <µs of now+1h>, true)` on `s`'s DB); create two entities; submit an async search with no `pointInTime`; wait `SUCCESSFUL`; expect 2 results (the #649 defect's shape, deterministic).

- [ ] **Step 2: Run to verify the new tests pass and the old ones fail**

Run: `go test -timeout 30m -run 'TestConsistencyFence|TestAsyncDefault_FloorAhead|TestDeleteEntities_Unconditional_PointInTime|TestGroupedStats_' ./internal/e2e/...`
Expected: new tests PASS (Tasks 10-12 landed); `entity_delete_unconditional_test.go:97-108` FAILS (expects 200 for 2099) — fix it next.

- [ ] **Step 3: Update existing tests**

- `entity_delete_unconditional_test.go:97-108`: "future instant selects the current state" becomes "future instant is refused" (400 with the code); keep `:145-150` unchanged (404 before fence).
- `grouped_stats_invalid_path_test.go`: force the streaming path with a joined transaction (`grouped_stats_service.go:124-125`) instead of a point in time; take instants from `consistencyTime(t)`.
- `async_stream_test.go:907`, `scheduled_run_fencing_test.go:382`: seed `point_in_time` at `now - 1 minute` (at or below every consistency time), not `now + 1 minute`.
- `zzz_errorcode_matrix_test.go`: declare `POINT_IN_TIME_AFTER_CONSISTENCY_TIME` on `getOneEntity`, `getAllEntities`, `getEntityStatisticsForModel`, `getEntityStatisticsByStateForModel`, `deleteEntities` (and `CONSISTENCY_TIME_UNAVAILABLE` on the same keys once Task 14 produces it; do both in Task 14 if the matrix fails on an unproduced cell in between). If `getConsistencyTime` becomes a key, declare `FORBIDDEN`.
- Remove the stale skip `e2e/parity/externalapi/entity_delete.go:88` after confirming its reason no longer holds (run the scenario).
- `e2e/parity/externalapi/negative_validation.go:191-196` (`12_07`): its skip reason is wrong (conditional delete with `pointInTime` exists). Ruling (Paul, 2026-10-05): cyoda-go has no match-count limit on conditional delete; Cloud's `entitySearchLimit` is not part of the contract — a client that wants the guard counts at an instant (stats with `pointInTime`) and deletes at the same instant. Rewrite the skip reason to say exactly that ("not applicable: cyoda-go has no entitySearchLimit; count at pointInTime, then delete at the same pointInTime"), and record the same in `e2e/externalapi/dictionary-mapping.md` for `12/neg/07`.

- [ ] **Step 4: Run tests**

Run: `go test -timeout 30m -run 'TestConsistencyFence|TestAsyncDefault_FloorAhead|TestDeleteEntities_|TestGroupedStats_|TestErrCodeMatrix|TestOpenAPIConformance' ./internal/e2e/...` then `make test > /tmp/t13-make-test.log 2>&1; grep -n -E '^(--- FAIL|FAIL|panic:)|FAIL:' /tmp/t13-make-test.log`
Expected: no failures. (The conformance report and matrix tests need the whole package's coverage to be meaningful; they run fully in Task 19.)

- [ ] **Step 5: Commit**

```bash
git add internal/e2e e2e/parity/externalapi
git commit -m "test(e2e): every fenced operation refuses a later instant and serves the consistency time"
```

---

### Task 14: e2e — the 503 stack

**Files:**
- Create: `internal/e2e/consistency_unavailable_test.go`

- [ ] **Step 1: Write the tests**

Stack: `s := newSchedDB(t)`; `t.Setenv("CYODA_POSTGRES_STATEMENT_TIMEOUT", "1s")`; build the stack with `newStackOn(t, s, nil)`, wrapped with `openapivalidator.NewMiddleware` as `entity_delete_nonconvergence_test.go:94` does, so its responses feed the conformance report and the matrix. A holder on `s`'s database for the stack's tenant:

```go
func holdMarker(t *testing.T, dbURL, tenant string) (release func()) {
	t.Helper()
	conn, err := pgx.Connect(context.Background(), dbURL)
	require.NoError(t, err)
	tx, err := conn.Begin(context.Background())
	require.NoError(t, err)
	var s time.Time
	require.NoError(t, tx.QueryRow(context.Background(), `SELECT cyoda_stamp($1)`, tenant).Scan(&s))
	stop := make(chan struct{})
	go func() {
		// Keep a statement running so the 5 s idle limit does not end us.
		_, _ = tx.Exec(context.Background(), `SELECT pg_sleep(30)`)
		<-stop
	}()
	return func() {
		close(stop)
		_ = conn.Close(context.Background()) // ends the sleep and the transaction
	}
}
```

Then, with the marker held, one subtest per fenced operation with `pointInTime = 2099-01-01T00:00:00Z` (a far-future instant cannot pass on the node's cached value) → 503, `errorCode = CONSISTENCY_TIME_UNAVAILABLE`, `retryable = true`; plus `GET /entity/consistency-time` → 503; plus async submit with no `pointInTime` → 503 and no job created (`GET` of the job list / status shows none).

- [ ] **Step 2: Run**

Run: `go test -timeout 30m -run 'TestConsistencyUnavailable' ./internal/e2e/...`
Expected: PASS, each subtest ~1 s.

- [ ] **Step 3: Matrix declarations**

Declare `CONSISTENCY_TIME_UNAVAILABLE` on the keyed operations (`getOneEntity`, `getAllEntities`, `getEntityStatisticsForModel`, `getEntityStatisticsByStateForModel`, `deleteEntities`). Run: `go test -timeout 30m -run 'TestConsistencyUnavailable|TestErrCodeMatrix' ./internal/e2e/...`.

- [ ] **Step 4: Commit**

```bash
git add internal/e2e
git commit -m "test(e2e): CONSISTENCY_TIME_UNAVAILABLE on every fenced operation and the endpoint"
```

---

### Task 15: gRPC tests

**Files:**
- Create: `internal/grpc/consistency_fence_test.go`

- [ ] **Step 1: Write the tests**

Use `newTestEnv(t)` (`rpc_test.go:45-80`) with its memory factory; wrap the factory's TM so `ConsistencyTime` returns a chosen `C` (or an error), following `onCommitTxMgr` (`entity_timeout_test.go:280-298`). One test per fenced gRPC request — `EntityGetRequest`, `EntityGetAllRequest`, `EntitySearchRequest`, `EntitySnapshotSearchRequest`, `EntityDeleteAllRequest`, `EntityStatsGetRequest`, `EntityStatsByStateGetRequest`, `EntityChangesMetadataGetRequest` — each with: `pointInTime = C + 1ms` → `Success=false`, `Error.Code = CLIENT_ERROR`, message prefix `POINT_IN_TIME_AFTER_CONSISTENCY_TIME`, `Retryable=false`, message contains `C`; `pointInTime = C` → success; TM error `ErrConsistencyTimeUnavailable` with a far-future instant → `CONSISTENCY_TIME_UNAVAILABLE`, `Retryable=true`; one representative with a storage-unavailable marker error → `STORAGE_UNAVAILABLE`. Plus `EntityGetAllRequest` and the two stats requests return data as at `T` (not the current state). Plus `EntitySnapshotSearchRequest` without `pointInTime` → job instant equals `C`. Update `entity_deleteall_fields_test.go:214` to a far-future instant so it pins the model-before-fence order.

- [ ] **Step 2: Run**

Run: `go test ./internal/grpc/...`
Expected: PASS.

- [ ] **Step 3: Commit**

```bash
git add internal/grpc
git commit -m "test(grpc): fence envelopes on every gRPC read with an instant"
```

---

### Task 16: parity scenarios and the compute test client

**Files:**
- Create: `e2e/parity/consistency_time.go`
- Modify: `e2e/parity/registry.go` (entries), `e2e/parity/registry_count_test.go` (`wantParityScenarioCount` 230 → 230 + N), `e2e/parity/client/http.go` (client methods)
- Modify: `cmd/compute-test-client/callback.go:650`

- [ ] **Step 1: Client methods**

In `e2e/parity/client/http.go` add `GetConsistencyTime(t) (string, error)` (`GET /entity/consistency-time`) and raw variants that take a `pointInTime` string for the fenced operations not already parameterised (follow `GetEntityStatsRaw` at `:1295`).

- [ ] **Step 2: Scenarios**

`consistency_time.go` with `Run*` functions registered as:

```go
	{"ConsistencyTime_Endpoint", RunConsistencyTimeEndpoint},           // 200, value parses, monotonic over 5 calls
	{"ConsistencyTime_FenceRefusesLater", RunConsistencyTimeFenceRefuses}, // each HTTP fenced operation: later(C) → 400 + properties.consistencyTime
	{"ConsistencyTime_ReadAtCServed", RunConsistencyTimeReadAtC},       // each HTTP fenced operation at C → 2xx; get-by-id echoes C verbatim
	{"ConsistencyTime_AsyncDefaultIncludesConfirmedSaves", RunConsistencyTimeAsyncDefault},
	{"ConsistencyTime_StatsHonourPointInTime", RunConsistencyTimeStatsAsAt}, // counts as at an instant between two saves, all four stats endpoints
```

Instants between two saves come from server stamps (`pit_time.go` helpers), never the process clock. Bump `wantParityScenarioCount` by 5 and the header comment.

- [ ] **Step 3: Compute test client**

`callback.go:650`: replace `time.Now().Add(time.Hour)` with the consistency time fetched from the platform (HTTP `GET /entity/consistency-time` on the client's base URL, or the gRPC request — use whichever channel the callback already uses for its read). The scenario `CallbackTxJoin_PITCommittedOnly` keeps asserting 404 for the uncommitted secondary.

- [ ] **Step 4: Run**

Run: `make test > /tmp/t16-make-test.log 2>&1; grep -n -E '^(--- FAIL|FAIL|panic:)|FAIL:' /tmp/t16-make-test.log` (runs parity on memory, sqlite, postgres uncached).
Expected: no hits.

- [ ] **Step 5: Commit**

```bash
git add e2e/parity cmd/compute-test-client
git commit -m "test(parity): consistency-time scenarios on every backend"
```

---

### Task 17: multi-node and concurrency tests

**Files:**
- Create: `e2e/parity/multinode/consistency_time.go` (registered in `multinode/registry.go:35-45`)
- Create: `internal/e2e/consistency_concurrency_test.go`

- [ ] **Step 1: Multi-node scenario**

With the postgres multi-node fixture (`e2e/parity/postgres/multinode_fixture.go`, 3 nodes): save on node 0 → `GET /entity/consistency-time` on node 1 → `GET /entity/{id}?pointInTime=C` on node 1 finds it; the `C` from node 0 passes the fence on node 2 (never 400); an async search submitted on node 1 without `pointInTime` includes the node-0 save.

- [ ] **Step 2: Concurrency e2e (isolated, own database)**

`TestConsistency_ConcurrentWritersAndReaders`: 8 goroutines create entities for 10 s and record each acknowledged id; 2 goroutines loop: record the acknowledged set, take `C`, list the model at `C` with `pageSize` 1000, sleep 30 ms, list again — the two lists are identical and contain every id acknowledged before `C` was requested; 1 goroutine submits async searches without `pointInTime` and checks each result includes every id acknowledged before the submit. Also `TestConsistency_PagingAtCIsStable`: write while paging at one `C` with `pageSize` 10; the union of pages equals one list at `C`.

- [ ] **Step 3: Run**

Run: `go test -timeout 30m -run 'TestConsistency_' ./internal/e2e/...` and `go test -timeout 30m -run 'TestMultiNode' ./e2e/parity/postgres/...`
Expected: PASS.

- [ ] **Step 4: Commit**

```bash
git add e2e/parity/multinode internal/e2e
git commit -m "test: consistency time across nodes and under concurrent writes"
```

---

### Task 18: documentation and cloud parity

**Files:** (spec §8, §9)
- `api/openapi.yaml`: each fenced operation's `pointInTime` parameter description gains "Must be at or before the consistency time (GET /entity/consistency-time); a later instant is refused with 400 POINT_IN_TIME_AFTER_CONSISTENCY_TIME."; `400` and `503` descriptions gain the codes; async submit `pointInTime`: "Absent: the consistency time at submission."; delete the phantom "point-in-time" sentence at `:7212`. Run `go generate ./api/...` and `make check-codegen`.
- `cmd/cyoda/help/content/crud.md`: rewrite POINT-IN-TIME SEMANTICS (`:579-614`) — the consistency time, the fence, the four properties in plain words, the new endpoint, "reads without pointInTime read the current state", the **list-paging caveat** ("pages of a list without `pointInTime` are read at different moments; for consistent pages take the consistency time once and pass it on every page"), change history and audit trail committed-only in a transaction; fix transitions (`:395`: "When neither is provided, the current state is used.").
- `cmd/cyoda/help/content/search.md:236`: "if not provided, the consistency time at submission is used"; grouped-stats text: no streaming fallback for point-in-time on sqlite/postgres.
- `docs/CONSISTENCY.md` §1a (`:54-97`): replace the stability rule with the consistency time; correct the sqlite statement (readers do not take the gate; the consistency time waits for it); delete "deliberately not scheduled".
- `docs/ARCHITECTURE.md:1475-1479` and DD-11 (`:2470-2476`): the async default is the consistency time; message catalogue (`:1827`) gains the new gRPC pair.
- `docs/plugins/POSTGRES.md`: stamping via `cyoda_stamp`; the floor sequence and both functions; the advisory key layout; the 5 s commit-phase limit; the role grants (`SELECT, UPDATE` on the sequence, `USAGE` on the schema, `EXECUTE`); the "no session-level state" statement (`:458-460`) now names the microsecond floor mutex and why it cannot leak (guarded block, connection closed on error); the asynchronous-replica exposure.
- gRPC schemas: `EntityGetRequest.json:20`, `EntityStatsGetRequest.json:15`, `EntityStatsByStateGetRequest.json:15`, `EntityChangesMetadataGetRequest.json:20` → "If not provided, the current state is read."; `EntitySnapshotSearchRequest.json` → "If not provided, the consistency time at submission." Re-run `./scripts/generate-events.sh`.
- Comments: `e2e/parity/pit_time.go:12-38` (stamps come from `cyoda_stamp` at commit).
- `docs/cloud-parity/consistency-time.md` (new; follow `delete-not-converged.md`'s layout) + a row in `docs/cloud-parity/README.md`: definition, fence + 400, async default, endpoint + gRPC pair, both codes with status/retryable/detail table and gRPC envelope, stats and get-all honouring `pointInTime`, history committed-only in a transaction; Cloud differences to close (C not complete; reads later than C served unfenced; no endpoint). Also state: conditional delete has no match-count limit; Cloud's `entitySearchLimit` on delete-by-condition at a point in time is not part of the contract (the client counts at the instant, then deletes at the same instant).

- [ ] **Step 1: Write the docs** (as listed).
- [ ] **Step 2: Run** `go test ./cmd/cyoda/help/... ./docs/... && make check-codegen`. Expected: PASS.
- [ ] **Step 3: Fresh-agent doc check** — dispatch a fresh agent with only `cyoda help crud`, `cyoda help search` and the new error topics, asking it to describe how an application reads a model consistently across pages and what it does on each new error; fix any gap it trips over.
- [ ] **Step 4: Commit**

```bash
git add api cmd/cyoda/help docs e2e/parity/pit_time.go
git commit -m "docs: consistency time — help topics, OpenAPI, consistency and postgres docs, cloud parity"
```

---

### Task 19: pins, full verification, issues, PR

- [ ] **Step 1: SPI PR**

In `cyoda-go-spi`: push branch `feat-consistency-time`, open the PR into `main` (title `feat(spi)!: TransactionManager.ConsistencyTime; Count/CountByState take asAt`), notify consumers per `KNOWN_CONSUMERS.md`. After it merges (squash), note the merge commit.

- [ ] **Step 2: Pin swap (one commit)**

Remove the local `go.work` use line. In root, `plugins/memory`, `plugins/sqlite`, `plugins/postgres`: `go get github.com/cyoda-platform/cyoda-go-spi@<merge commit>` and `go mod tidy`; `make repin-plugins` after the plugin commits are pushed; update `COMPATIBILITY.md` (pseudo-version line in the v0.9.0 row). Run `./scripts/check-spi-pin-sync.sh` and `GOWORK=off go build ./...`.

```bash
git add go.mod go.sum plugins/*/go.mod plugins/*/go.sum COMPATIBILITY.md
git commit -m "chore(deps): pin cyoda-go-spi main with ConsistencyTime"
```

- [ ] **Step 3: Full verification** (once, logs to files)

```bash
make preflight
make check-gofmt && make check-codegen && make check-spi-pin-sync && go vet ./...
for m in plugins/memory plugins/sqlite plugins/postgres; do (cd $m && go vet ./...); done
make test-full > /tmp/649-test-full.log 2>&1; grep -n -E '^(--- FAIL|FAIL|panic:)|FAIL:' /tmp/649-test-full.log
make race > /tmp/649-race.log 2>&1; grep -n -E 'WARNING: DATA RACE|^(--- FAIL|FAIL|panic:)' /tmp/649-race.log
```

Expected: no hits in either log. A failure is diagnosed from its log, never by re-running.

- [ ] **Step 4: Review gates** — `superpowers:requesting-code-review` (fresh-context reviewer on the whole branch) and `cyoda-go-security-audit`, both on the exact PR head; any fix wave reruns both.

- [ ] **Step 5: Issues**

- cassandra issue (spec §6.4) in `Cyoda/cyoda-go-cassandra`, linking cassandra#110, #97, #64.
- CaaS ticket in Jira project CP (`[CaaS]` prefix, component CaaS) for the cloud-parity doc.

- [ ] **Step 6: PR**

Open the PR into `release/v0.9.0` (check the base), body: summary, `### Breaking` (new required SPI method, `Count`/`CountByState` signatures, refused future instants), test evidence (log paths, counts), links to the SPI PR, cassandra issue, CaaS ticket; "Closes #649, #581" stated in prose (release-branch merges do not auto-close — close both by hand on merge). Update #651 row 2.
