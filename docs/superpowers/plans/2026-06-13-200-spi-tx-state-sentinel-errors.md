# SPI Sentinel Errors for Transaction State — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace plain-string transaction-state errors across memory/sqlite/postgres plugins with a hierarchy of SPI-level sentinel errors so callers can use `errors.Is` instead of `strings.Contains`.

**Architecture:** Add seven new exported sentinels to `cyoda-go-spi/errors.go` using an unexported `sentinelErr` type with an `Unwrap()` parent. Two-level hierarchy: `ErrTxRolledBack` and `ErrTxAlreadyCommitted` wrap an `ErrTxTerminated` umbrella; `ErrTxNotFound` and `ErrSavepointNotFound` wrap the existing `ErrNotFound`. Conformance tests added to `spitest/transaction.go` exercise each sentinel against every backend via the existing `Harness` pattern; sentinel-graph properties (hierarchy, distinctness) covered as unit tests in `errors_test.go`. Plugin migrations wrap the sentinels at each error site; postgres' `verifyTenant()` helper is a single wrap-site that covers six lifecycle ops. Memory concurrency tests switch from substring matching to `errors.Is`. Tagged as a fresh SPI minor (e.g. `v0.8.0`); cyoda-go bumps the pin in four `go.mod` files.

**Tech Stack:** Go 1.26, `errors` package (stdlib), `errors.Is` / `Unwrap()`, testify (already in SPI repo).

**Spec:** [`docs/superpowers/specs/2026-06-13-200-spi-tx-state-sentinel-errors-design.md`](../specs/2026-06-13-200-spi-tx-state-sentinel-errors-design.md)

**Worktree:** `/Users/paul/go-projects/cyoda-light/cyoda-go/.claude/worktrees/feat-200-spi-tx-state-sentinels` on branch `feat/200-spi-tx-state-sentinel-errors`.

---

## Repos involved

This plan touches **two repos**. Both are local clones, sibling directories:

- **SPI repo** — `/Users/paul/go-projects/cyoda-light/cyoda-go-spi/` (Phase A — own branch, own PR, own tag)
- **cyoda-go worktree** — `/Users/paul/go-projects/cyoda-light/cyoda-go/.claude/worktrees/feat-200-spi-tx-state-sentinels/` (Phase B — this worktree)

Phase A's PR must be merged and tagged BEFORE Phase B's pin bump compiles. During development, use a local `replace` directive in cyoda-go's go.mod files to point at the local SPI checkout — this lets you iterate both phases in parallel without waiting for tags. Remove the replace before opening Phase B's PR.

**Local replace pattern (use during dev, remove before PR):**

```
// At the bottom of each go.mod (root + plugins/{memory,sqlite,postgres}):
replace github.com/cyoda-platform/cyoda-go-spi => /Users/paul/go-projects/cyoda-light/cyoda-go-spi
```

---

## Phase A: cyoda-go-spi PR

All paths in Phase A are relative to `/Users/paul/go-projects/cyoda-light/cyoda-go-spi/`.

### Task A.0: Set up SPI feature branch

**Files:** None (git only).

- [ ] **Step 1: Verify the SPI repo is clean**

Run:
```bash
cd /Users/paul/go-projects/cyoda-light/cyoda-go-spi
git status
```
Expected: `working tree clean` on `main` (or whichever the default branch is).

- [ ] **Step 2: Create feature branch off the default branch**

```bash
git fetch origin
git checkout -b feat/200-tx-state-sentinels origin/main
```

- [ ] **Step 3: Confirm the SPI module path**

```bash
head -1 go.mod
```
Expected: `module github.com/cyoda-platform/cyoda-go-spi`

---

### Task A.1: Add sentinel-graph tests (RED)

**Files:**
- Modify: `errors_test.go`

This is the RED phase for the sentinel definitions. The tests reference sentinel names that don't exist yet — they must FAIL TO COMPILE before Task A.2 makes them pass.

- [ ] **Step 1: Append the new test cases to `errors_test.go`**

Open `errors_test.go` and append these test functions after the existing `TestErrRetryExhausted_DistinctFromErrConflict`:

```go
// TestTxSentinelHierarchy verifies the parent/child relationships defined
// by the sentinelErr.Unwrap() chain. Every child must match its parent
// via errors.Is.
func TestTxSentinelHierarchy(t *testing.T) {
	positive := []struct {
		name   string
		child  error
		parent error
	}{
		{"ErrTxNotFound→ErrNotFound", ErrTxNotFound, ErrNotFound},
		{"ErrSavepointNotFound→ErrNotFound", ErrSavepointNotFound, ErrNotFound},
		{"ErrTxRolledBack→ErrTxTerminated", ErrTxRolledBack, ErrTxTerminated},
		{"ErrTxAlreadyCommitted→ErrTxTerminated", ErrTxAlreadyCommitted, ErrTxTerminated},
	}
	for _, tc := range positive {
		t.Run(tc.name, func(t *testing.T) {
			if !errors.Is(tc.child, tc.parent) {
				t.Errorf("expected errors.Is(%v, %v) == true", tc.child, tc.parent)
			}
		})
	}
}

// TestTxSentinelsAreDistinct verifies that siblings under a shared parent
// and unrelated sentinels do not match each other via errors.Is. These
// negative pairs are load-bearing for callers that distinguish conditions.
func TestTxSentinelsAreDistinct(t *testing.T) {
	negative := []struct {
		name string
		a    error
		b    error
	}{
		// Siblings under ErrNotFound — tx-not-found vs savepoint-not-found
		// must stay distinguishable.
		{"ErrTxNotFound!~ErrSavepointNotFound", ErrTxNotFound, ErrSavepointNotFound},
		{"ErrSavepointNotFound!~ErrTxNotFound", ErrSavepointNotFound, ErrTxNotFound},

		// Siblings under ErrTxTerminated — rolled-back vs already-committed
		// must stay distinguishable for diagnostic purposes.
		{"ErrTxRolledBack!~ErrTxAlreadyCommitted", ErrTxRolledBack, ErrTxAlreadyCommitted},
		{"ErrTxAlreadyCommitted!~ErrTxRolledBack", ErrTxAlreadyCommitted, ErrTxRolledBack},

		// CommitInProgress is a transient race, NOT a terminal state.
		// It must not match the ErrTxTerminated umbrella.
		{"ErrTxCommitInProgress!~ErrTxTerminated", ErrTxCommitInProgress, ErrTxTerminated},
		{"ErrTxTerminated!~ErrTxCommitInProgress", ErrTxTerminated, ErrTxCommitInProgress},

		// Cross-tree pairs.
		{"ErrTxRolledBack!~ErrNotFound", ErrTxRolledBack, ErrNotFound},
		{"ErrTxNotFound!~ErrTxTerminated", ErrTxNotFound, ErrTxTerminated},
		{"ErrTxTenantMismatch!~ErrTxTerminated", ErrTxTenantMismatch, ErrTxTerminated},
		{"ErrTxTenantMismatch!~ErrNotFound", ErrTxTenantMismatch, ErrNotFound},
		{"ErrTxCommitInProgress!~ErrTxNotFound", ErrTxCommitInProgress, ErrTxNotFound},

		// Existing sentinels stay clean.
		{"ErrConflict!~ErrTxTerminated", ErrConflict, ErrTxTerminated},
		{"ErrConflict!~ErrTxRolledBack", ErrConflict, ErrTxRolledBack},
	}
	for _, tc := range negative {
		t.Run(tc.name, func(t *testing.T) {
			if errors.Is(tc.a, tc.b) {
				t.Errorf("expected errors.Is(%v, %v) == false", tc.a, tc.b)
			}
		})
	}
}

// TestTxSentinelWrapChain verifies that fmt.Errorf("...: %w", sentinel)
// composes correctly with the sentinelErr.Unwrap() chain — errors.Is
// must walk both layers and match against the sentinel AND its parent.
func TestTxSentinelWrapChain(t *testing.T) {
	wrapped := fmt.Errorf("plugin context: %w", ErrTxNotFound)
	if !errors.Is(wrapped, ErrTxNotFound) {
		t.Error("wrapped ErrTxNotFound should match ErrTxNotFound")
	}
	if !errors.Is(wrapped, ErrNotFound) {
		t.Error("wrapped ErrTxNotFound should match ErrNotFound via Unwrap chain")
	}
	if errors.Is(wrapped, ErrTxRolledBack) {
		t.Error("wrapped ErrTxNotFound must not match unrelated ErrTxRolledBack")
	}

	deeplyWrapped := fmt.Errorf("outer: %w", fmt.Errorf("inner: %w", ErrTxRolledBack))
	if !errors.Is(deeplyWrapped, ErrTxRolledBack) {
		t.Error("doubly-wrapped ErrTxRolledBack should match ErrTxRolledBack")
	}
	if !errors.Is(deeplyWrapped, ErrTxTerminated) {
		t.Error("doubly-wrapped ErrTxRolledBack should match ErrTxTerminated via Unwrap chain")
	}
}
```

- [ ] **Step 2: Run the tests to confirm they fail to compile**

```bash
go test ./...
```
Expected: compile failures — `undefined: ErrTxNotFound`, `undefined: ErrSavepointNotFound`, etc. This is the RED.

---

### Task A.2: Define the sentinels (GREEN)

**Files:**
- Modify: `errors.go`

- [ ] **Step 1: Append the new sentinels and `sentinelErr` type**

Open `errors.go` and append after the existing `ErrRetryExhausted` declaration:

```go
// sentinelErr is the unexported error type used to declare sentinels that
// belong in a hierarchy. The Unwrap method makes errors.Is walk to the
// parent sentinel as well as match the leaf, so callers can match either
// the specific condition or its umbrella.
type sentinelErr struct {
	msg    string
	parent error
}

func (e *sentinelErr) Error() string { return e.msg }
func (e *sentinelErr) Unwrap() error { return e.parent }

// ErrTxNotFound indicates that a transaction handle does not refer to a
// known transaction — either the txID never existed, or its state has
// been fully purged. Wraps ErrNotFound so existing
//
//	errors.Is(err, spi.ErrNotFound)
//
// checks on tx-lifecycle paths continue to match.
var ErrTxNotFound = &sentinelErr{msg: "transaction not found", parent: ErrNotFound}

// ErrSavepointNotFound indicates that a savepoint identifier does not
// refer to a known savepoint on the given transaction. Wraps ErrNotFound.
var ErrSavepointNotFound = &sentinelErr{msg: "savepoint not found", parent: ErrNotFound}

// ErrTxTerminated is the umbrella sentinel for any operation on a
// transaction that has reached a terminal state (committed or rolled
// back). Callers that do not need to distinguish rollback from commit
// can match this directly.
//
// NOTE: Backends that own transaction state in a remote engine
// (postgres) may surface mid-op rollback as ErrConflict (e.g. via
// SQLSTATE 25P02) instead of ErrTxRolledBack, where the engine's
// abort code is already semantically meaningful. The ErrTxTerminated
// sentinel is required only where the plugin owns its own in-process
// tx-state buffer (memory, sqlite, cassandra). Consumers writing
// backend-agnostic code should match both ErrTxTerminated and
// ErrConflict on data-op paths.
var ErrTxTerminated = errors.New("transaction in terminal state")

// ErrTxRolledBack indicates that an in-flight operation observed the
// transaction marked rolled-back (memory/sqlite race) or attempted an op
// on a transaction whose terminal state is Rollback. Wraps ErrTxTerminated.
// See ErrTxTerminated godoc for the postgres-engine caveat.
var ErrTxRolledBack = &sentinelErr{msg: "transaction rolled back", parent: ErrTxTerminated}

// ErrTxAlreadyCommitted indicates an attempt to Join, Commit, or
// otherwise operate on a transaction whose terminal state is Commit.
// Wraps ErrTxTerminated.
var ErrTxAlreadyCommitted = &sentinelErr{msg: "transaction already committed", parent: ErrTxTerminated}

// ErrTxCommitInProgress indicates that Commit was called on a
// transaction another goroutine is already committing. Distinct from
// ErrTxTerminated because the transaction is not yet terminal — the
// loser of the race may still observe the committed result.
var ErrTxCommitInProgress = errors.New("transaction commit in progress")

// ErrTxTenantMismatch indicates a transaction-lifecycle operation
// (Join, Commit, Rollback, Savepoint, etc.) was attempted with a
// UserContext whose tenant does not match the transaction's tenant.
// Tenant-isolation invariant — distinct from data-op tenant checks.
var ErrTxTenantMismatch = errors.New("transaction tenant mismatch")
```

- [ ] **Step 2: Run the tests to confirm they pass**

```bash
go test ./...
```
Expected: all tests pass, including the three new ones (`TestTxSentinelHierarchy`, `TestTxSentinelsAreDistinct`, `TestTxSentinelWrapChain`).

- [ ] **Step 3: Vet**

```bash
go vet ./...
```
Expected: no output.

- [ ] **Step 4: Commit**

```bash
git add errors.go errors_test.go
git commit -m "feat(errors): add transaction-state sentinel hierarchy

Introduce seven sentinel errors for transaction-lifecycle conditions:

- ErrTxNotFound, ErrSavepointNotFound (wrap ErrNotFound)
- ErrTxTerminated umbrella with ErrTxRolledBack, ErrTxAlreadyCommitted
- ErrTxCommitInProgress, ErrTxTenantMismatch

Hierarchy via an unexported sentinelErr type with Unwrap(). Callers can
match the specific cause or the umbrella; existing errors.Is(err,
ErrNotFound) checks on tx-lifecycle paths continue to match through
the new ErrTxNotFound / ErrSavepointNotFound wrap.

ErrTxTerminated godoc documents the postgres-engine caveat: remote-
engine backends may surface mid-op rollback as ErrConflict (SQLSTATE
25P02) instead of ErrTxRolledBack.

Tests cover every transitive parent match (positive), every off-
diagonal sibling/cross-tree pair (negative), and the fmt.Errorf wrap-
chain composition.

Refs Cyoda-platform/cyoda-go#200"
```

---

### Task A.3: Add per-backend conformance subtests

**Files:**
- Modify: `spitest/transaction.go`

The subtests reference the sentinels from Task A.2 and exercise behaviour the spec calls out. In the SPI repo they're library code — there's no backend to run them against here. The RED→GREEN cycle happens in Phase B against memory/sqlite/postgres. In this task we just verify they compile and the suite registration is syntactically right.

- [ ] **Step 1: Add the seven subtests to `runTransactionSuite`**

Open `spitest/transaction.go`. Find the existing `runTransactionSuite` function (currently registers `CommitVisibility`, `RollbackDiscards`, `Join`, `SubmitTime`, `Savepoint/ReleaseMergesWork`, `Savepoint/RollbackToDiscards`, `BeginAfterCommit`). Insert the new `runSubtest` calls AFTER `BeginAfterCommit` and BEFORE the closing brace:

```go
	runSubtest(t, h, tracker, "TxStateErrors/JoinAfterCommit", testTxStateJoinAfterCommit)
	runSubtest(t, h, tracker, "TxStateErrors/CommitAfterCommit", testTxStateCommitAfterCommit)
	runSubtest(t, h, tracker, "TxStateErrors/CommitAfterRollback", testTxStateCommitAfterRollback)
	runSubtest(t, h, tracker, "TxStateErrors/OpAfterRollback", testTxStateOpAfterRollback)
	runSubtest(t, h, tracker, "TxStateErrors/TenantMismatchOnJoin", testTxStateTenantMismatchOnJoin)
	runSubtest(t, h, tracker, "TxStateErrors/TenantMismatchOnCommit", testTxStateTenantMismatchOnCommit)
	runSubtest(t, h, tracker, "TxStateErrors/SavepointNotFound", testTxStateSavepointNotFound)
```

- [ ] **Step 2: Append the subtest implementations to the end of `spitest/transaction.go`**

Append (after the existing subtests, inside the same package):

```go
// testTxStateJoinAfterCommit verifies that joining a transaction whose
// terminal state is Commit produces ErrTxAlreadyCommitted (which also
// matches ErrTxTerminated via Unwrap).
func testTxStateJoinAfterCommit(t *testing.T, h Harness) {
	ctx := tenantContext(h.NewTenant())
	tm, err := h.Factory.TransactionManager(ctx)
	require.NoError(t, err)

	txID, txCtx, err := tm.Begin(ctx)
	require.NoError(t, err)
	require.NoError(t, tm.Commit(txCtx, txID))

	_, err = tm.Join(ctx, txID)
	require.Error(t, err, "Join after Commit must fail")
	require.True(t,
		errors.Is(err, spi.ErrTxAlreadyCommitted) || errors.Is(err, spi.ErrTxNotFound),
		"Join after Commit must wrap ErrTxAlreadyCommitted or ErrTxNotFound (backends that purge committed-tx state collapse these); got: %v", err)
	require.True(t, errors.Is(err, spi.ErrTxTerminated) || errors.Is(err, spi.ErrTxNotFound),
		"Join after Commit must wrap ErrTxTerminated or ErrTxNotFound; got: %v", err)
}

// testTxStateCommitAfterCommit verifies that double-Commit produces
// ErrTxAlreadyCommitted or ErrTxNotFound (backends that purge state
// after the first Commit collapse to NotFound).
func testTxStateCommitAfterCommit(t *testing.T, h Harness) {
	ctx := tenantContext(h.NewTenant())
	tm, err := h.Factory.TransactionManager(ctx)
	require.NoError(t, err)

	txID, txCtx, err := tm.Begin(ctx)
	require.NoError(t, err)
	require.NoError(t, tm.Commit(txCtx, txID))

	err = tm.Commit(txCtx, txID)
	require.Error(t, err, "second Commit must fail")
	require.True(t,
		errors.Is(err, spi.ErrTxAlreadyCommitted) || errors.Is(err, spi.ErrTxNotFound),
		"second Commit must wrap ErrTxAlreadyCommitted or ErrTxNotFound; got: %v", err)
}

// testTxStateCommitAfterRollback verifies that Commit on a rolled-back tx
// produces ErrTxRolledBack or ErrTxNotFound (backends that purge state
// after Rollback collapse to NotFound).
func testTxStateCommitAfterRollback(t *testing.T, h Harness) {
	ctx := tenantContext(h.NewTenant())
	tm, err := h.Factory.TransactionManager(ctx)
	require.NoError(t, err)

	txID, txCtx, err := tm.Begin(ctx)
	require.NoError(t, err)
	require.NoError(t, tm.Rollback(txCtx, txID))

	err = tm.Commit(txCtx, txID)
	require.Error(t, err, "Commit after Rollback must fail")
	require.True(t,
		errors.Is(err, spi.ErrTxRolledBack) || errors.Is(err, spi.ErrTxNotFound),
		"Commit after Rollback must wrap ErrTxRolledBack or ErrTxNotFound; got: %v", err)
}

// testTxStateOpAfterRollback verifies that a data op against a rolled-back
// transaction produces ErrTxTerminated. Backends with remote tx state
// (postgres) may skip this via Harness.Skip — see the ErrTxTerminated
// godoc caveat.
func testTxStateOpAfterRollback(t *testing.T, h Harness) {
	ctx := tenantContext(h.NewTenant())
	tm, err := h.Factory.TransactionManager(ctx)
	require.NoError(t, err)

	txID, txCtx, err := tm.Begin(ctx)
	require.NoError(t, err)

	es, err := h.Factory.EntityStore(txCtx)
	require.NoError(t, err)

	id := newID()
	_, err = es.Save(txCtx, newEntity(t, "m-op-after-rb", id, map[string]any{"k": "v"}))
	require.NoError(t, err)

	require.NoError(t, tm.Rollback(txCtx, txID))

	_, err = es.Get(txCtx, id)
	require.Error(t, err, "Get after Rollback must fail")
	require.True(t, errors.Is(err, spi.ErrTxTerminated),
		"op after Rollback must wrap ErrTxTerminated; got: %v", err)
}

// testTxStateTenantMismatchOnJoin verifies that tenant B cannot Join a
// transaction begun by tenant A; the error wraps ErrTxTenantMismatch.
func testTxStateTenantMismatchOnJoin(t *testing.T, h Harness) {
	ctxA := tenantContext(h.NewTenant())
	ctxB := tenantContext(h.NewTenant())

	tmA, err := h.Factory.TransactionManager(ctxA)
	require.NoError(t, err)
	txID, _, err := tmA.Begin(ctxA)
	require.NoError(t, err)
	t.Cleanup(func() { _ = tmA.Rollback(ctxA, txID) })

	tmB, err := h.Factory.TransactionManager(ctxB)
	require.NoError(t, err)
	_, err = tmB.Join(ctxB, txID)
	require.Error(t, err, "tenant B Join of tenant A tx must fail")
	require.True(t, errors.Is(err, spi.ErrTxTenantMismatch),
		"cross-tenant Join must wrap ErrTxTenantMismatch; got: %v", err)
}

// testTxStateTenantMismatchOnCommit verifies that tenant B cannot Commit a
// transaction begun by tenant A; the error wraps ErrTxTenantMismatch.
func testTxStateTenantMismatchOnCommit(t *testing.T, h Harness) {
	ctxA := tenantContext(h.NewTenant())
	ctxB := tenantContext(h.NewTenant())

	tmA, err := h.Factory.TransactionManager(ctxA)
	require.NoError(t, err)
	txID, txCtxA, err := tmA.Begin(ctxA)
	require.NoError(t, err)
	t.Cleanup(func() { _ = tmA.Rollback(txCtxA, txID) })

	tmB, err := h.Factory.TransactionManager(ctxB)
	require.NoError(t, err)
	err = tmB.Commit(ctxB, txID)
	require.Error(t, err, "tenant B Commit of tenant A tx must fail")
	require.True(t, errors.Is(err, spi.ErrTxTenantMismatch),
		"cross-tenant Commit must wrap ErrTxTenantMismatch; got: %v", err)
}

// testTxStateSavepointNotFound verifies that RollbackToSavepoint with an
// unknown savepoint id produces ErrSavepointNotFound (which also matches
// ErrNotFound via Unwrap).
func testTxStateSavepointNotFound(t *testing.T, h Harness) {
	ctx := tenantContext(h.NewTenant())
	tm, err := h.Factory.TransactionManager(ctx)
	require.NoError(t, err)

	txID, txCtx, err := tm.Begin(ctx)
	require.NoError(t, err)
	t.Cleanup(func() { _ = tm.Rollback(txCtx, txID) })

	err = tm.RollbackToSavepoint(txCtx, txID, "no-such-savepoint")
	require.Error(t, err, "RollbackToSavepoint with unknown id must fail")
	require.True(t, errors.Is(err, spi.ErrSavepointNotFound),
		"unknown savepoint must wrap ErrSavepointNotFound; got: %v", err)
	require.True(t, errors.Is(err, spi.ErrNotFound),
		"ErrSavepointNotFound must also match ErrNotFound via Unwrap; got: %v", err)
}
```

- [ ] **Step 3: Run the SPI tests to confirm everything compiles**

```bash
go test ./...
```
Expected: all tests pass. (The new subtests are not invoked here — no backend — but they must compile cleanly.)

- [ ] **Step 4: Vet**

```bash
go vet ./...
```
Expected: no output.

- [ ] **Step 5: Commit**

```bash
git add spitest/transaction.go
git commit -m "feat(spitest): add transaction-state sentinel conformance subtests

Seven new subtests under runTransactionSuite cover the conditions
defined by the sentinel hierarchy added in the previous commit:

- TxStateErrors/JoinAfterCommit
- TxStateErrors/CommitAfterCommit
- TxStateErrors/CommitAfterRollback
- TxStateErrors/OpAfterRollback (backends with remote tx state may
  skip via Harness.Skip)
- TxStateErrors/TenantMismatchOnJoin
- TxStateErrors/TenantMismatchOnCommit
- TxStateErrors/SavepointNotFound

Assertions tolerate the spec's documented contract slack — backends
that purge tx state after termination may collapse 'AlreadyCommitted'
or 'RolledBack' into NotFound; the assertion accepts either.

Refs Cyoda-platform/cyoda-go#200"
```

---

### Task A.4: Update SPI CHANGELOG and prepare for tagging

**Files:**
- Modify: `CHANGELOG.md`

- [ ] **Step 1: Inspect the CHANGELOG shape**

```bash
head -40 CHANGELOG.md
```

Note the format the SPI repo uses (Keep-a-Changelog style or otherwise).

- [ ] **Step 2: Add a release entry**

At the top of the version list (under any "Unreleased" section if present), add:

```markdown
## [v0.8.0] - 2026-06-13

### Added

- Transaction-state sentinel hierarchy (`ErrTxNotFound`, `ErrSavepointNotFound`, `ErrTxTerminated`, `ErrTxRolledBack`, `ErrTxAlreadyCommitted`, `ErrTxCommitInProgress`, `ErrTxTenantMismatch`). Backwards-compatible: `ErrTxNotFound` and `ErrSavepointNotFound` wrap `ErrNotFound`; existing `errors.Is(err, ErrNotFound)` callers continue to match.
- Seven new `spitest/transaction.go` subtests asserting backend conformance to the sentinel contract.

### Notes for consumers

- Plugins should wrap the sentinels at every tx-state error site. The memory + sqlite + postgres plugins in `cyoda-go` are migrated as part of the corresponding `cyoda-go v0.8.0` release.
- The `OpAfterRollback` subtest may be skipped on backends that own tx state in a remote engine (e.g. postgres' pgx.Tx surfaces mid-op rollback as `ErrConflict` via SQLSTATE 25P02). See `ErrTxTerminated` godoc for details.
```

(Adjust the version number if `v0.8.0` is already taken; pick the next free minor — never force-move.)

- [ ] **Step 3: Commit**

```bash
git add CHANGELOG.md
git commit -m "docs(changelog): record v0.8.0 transaction-state sentinels"
```

- [ ] **Step 4: Push and open the SPI PR**

```bash
git push -u origin feat/200-tx-state-sentinels
gh pr create --title "feat: transaction-state sentinel hierarchy (#200)" \
             --body "$(cat <<'EOF'
Adds the SPI surface for cyoda-go issue #200: seven new sentinel errors covering transaction-lifecycle conditions, plus per-backend conformance subtests under `spitest/transaction.go`.

Design spec lives in cyoda-go: `docs/superpowers/specs/2026-06-13-200-spi-tx-state-sentinel-errors-design.md`.

Backwards-compatible: `ErrTxNotFound` and `ErrSavepointNotFound` wrap the existing `ErrNotFound`, so callers using `errors.Is(err, ErrNotFound)` on tx-lifecycle paths continue to match.

The cyoda-go side (memory/sqlite/postgres plugin migrations, concurrency-test cleanup, `Harness.Skip` wiring) lands in a follow-up PR after this is tagged.

🤖 Generated with [Claude Code](https://claude.com/claude-code)
EOF
)"
```

- [ ] **Step 5: Wait for review and merge**

This is a manual handoff. Once the PR is merged to `main`, tag it:

```bash
git checkout main
git pull
git tag v0.8.0
git push origin v0.8.0
```

(Tag name should match the CHANGELOG entry from Step 2.)

---

## Phase B: cyoda-go PR

All paths in Phase B are relative to the worktree: `/Users/paul/go-projects/cyoda-light/cyoda-go/.claude/worktrees/feat-200-spi-tx-state-sentinels/`.

### Task B.1: Bump SPI pin in all four go.mods

**Files:**
- Modify: `go.mod`
- Modify: `plugins/memory/go.mod`
- Modify: `plugins/sqlite/go.mod`
- Modify: `plugins/postgres/go.mod`

Phase A must be tagged before this task. Until the tag exists, use the temporary `replace` directive described at the top of this plan.

- [ ] **Step 1: Confirm the new SPI version exists**

```bash
go list -m -versions github.com/cyoda-platform/cyoda-go-spi | tr ' ' '\n' | tail -5
```
Expected: includes `v0.8.0` (or whatever fresh tag Phase A produced).

- [ ] **Step 2: Update root `go.mod`**

In `go.mod`, change the line:
```
github.com/cyoda-platform/cyoda-go-spi v0.7.1
```
to:
```
github.com/cyoda-platform/cyoda-go-spi v0.8.0
```

- [ ] **Step 3: Repeat for each plugin go.mod**

```bash
# plugins/memory/go.mod, plugins/sqlite/go.mod, plugins/postgres/go.mod
# Replace the v0.7.1 line with v0.8.0 in each.
```

- [ ] **Step 4: Tidy each module**

```bash
go mod tidy
(cd plugins/memory && go mod tidy)
(cd plugins/sqlite && go mod tidy)
(cd plugins/postgres && go mod tidy)
```
Expected: `go.sum` files updated; no errors.

- [ ] **Step 5: Confirm the RED — new subtests fail against unmigrated plugins**

```bash
go test ./plugins/memory/... -run TestConformance -v 2>&1 | grep -A1 'TxStateErrors'
```
Expected: failures on subtests like `TxStateErrors/JoinAfterCommit`, `TxStateErrors/OpAfterRollback`, `TxStateErrors/TenantMismatchOnJoin`, etc. — the assertions cannot find the sentinels in the error chain because the plugin doesn't wrap them yet.

This is the RED for Phase B. Do not commit yet — the subsequent tasks turn this GREEN per-plugin.

---

### Task B.2: Memory plugin — migrate `txmanager.go`

**Files:**
- Modify: `plugins/memory/txmanager.go`

- [ ] **Step 1: Verify imports**

Open `plugins/memory/txmanager.go` and confirm `spi` is imported as:
```go
spi "github.com/cyoda-platform/cyoda-go-spi"
```
(It already is — verify only.)

- [ ] **Step 2: Migrate the lookup / closed / tenant / commit-race sites**

Apply these substitutions (line numbers reflect the current file; the exact text may have drifted slightly — match on the format string content):

| Line | Replace | With |
|---|---|---|
| 122 | `fmt.Errorf("transaction not found: %s", txID)` | `fmt.Errorf("Join: %w (txID=%s)", spi.ErrTxNotFound, txID)` |
| 131 | `fmt.Errorf("transaction already closed: %s", txID)` | `fmt.Errorf("Join: %w (txID=%s)", spi.ErrTxAlreadyCommitted, txID)` |
| 140 | `fmt.Errorf("tenant mismatch on transaction join")` | `fmt.Errorf("Join: %w", spi.ErrTxTenantMismatch)` |
| 156 | `fmt.Errorf("transaction not found or already completed: %w", spi.ErrNotFound)` | `fmt.Errorf("Commit: %w (txID=%s)", spi.ErrTxNotFound, txID)` |
| 160 | `fmt.Errorf("transaction tenant mismatch")` | `fmt.Errorf("Commit: %w", spi.ErrTxTenantMismatch)` |
| 164 | `fmt.Errorf("transaction already being committed")` | `fmt.Errorf("Commit: %w (txID=%s)", spi.ErrTxCommitInProgress, txID)` |
| 308 | `fmt.Errorf("transaction not found or already completed: %w", spi.ErrNotFound)` | `fmt.Errorf("Rollback: %w (txID=%s)", spi.ErrTxNotFound, txID)` |
| 312 | `fmt.Errorf("transaction tenant mismatch")` | `fmt.Errorf("Rollback: %w", spi.ErrTxTenantMismatch)` |
| 346 | `fmt.Errorf("transaction not found: %s", txID)` (in `GetSubmitTime`) | `fmt.Errorf("GetSubmitTime: %w (txID=%s)", spi.ErrTxNotFound, txID)` |

Leave line 339 (`"transaction not yet committed"` in `GetSubmitTime`) **untouched** — per spec, this stays plain (one-site operational signal).

- [ ] **Step 3: Migrate the savepoint sites**

| Line | Replace | With |
|---|---|---|
| 379 | `fmt.Errorf("Savepoint: transaction %s not found", txID)` | `fmt.Errorf("Savepoint: %w (txID=%s)", spi.ErrTxNotFound, txID)` |
| 382 | `fmt.Errorf("Savepoint: tenant mismatch on transaction %s", txID)` | `fmt.Errorf("Savepoint: %w (txID=%s)", spi.ErrTxTenantMismatch, txID)` |
| 392 | `fmt.Errorf("Savepoint: transaction %s already closed", txID)` | `fmt.Errorf("Savepoint: %w (txID=%s)", spi.ErrTxAlreadyCommitted, txID)` |
| 449 | `fmt.Errorf("RollbackToSavepoint: transaction %s not found", txID)` | `fmt.Errorf("RollbackToSavepoint: %w (txID=%s)", spi.ErrTxNotFound, txID)` |
| 452 | `fmt.Errorf("RollbackToSavepoint: tenant mismatch on transaction %s", txID)` | `fmt.Errorf("RollbackToSavepoint: %w (txID=%s)", spi.ErrTxTenantMismatch, txID)` |
| 459 | `fmt.Errorf("RollbackToSavepoint: transaction %s already closed", txID)` | `fmt.Errorf("RollbackToSavepoint: %w (txID=%s)", spi.ErrTxAlreadyCommitted, txID)` |
| 467 | `fmt.Errorf("RollbackToSavepoint: savepoint %s not found", savepointID)` | `fmt.Errorf("RollbackToSavepoint: %w (savepointID=%s)", spi.ErrSavepointNotFound, savepointID)` |
| 471 | `fmt.Errorf("RollbackToSavepoint: savepoint %s not found", savepointID)` | `fmt.Errorf("RollbackToSavepoint: %w (savepointID=%s)", spi.ErrSavepointNotFound, savepointID)` |
| 500 | `fmt.Errorf("ReleaseSavepoint: transaction %s not found", txID)` | `fmt.Errorf("ReleaseSavepoint: %w (txID=%s)", spi.ErrTxNotFound, txID)` |
| 503 | `fmt.Errorf("ReleaseSavepoint: tenant mismatch on transaction %s", txID)` | `fmt.Errorf("ReleaseSavepoint: %w (txID=%s)", spi.ErrTxTenantMismatch, txID)` |
| 508 | `fmt.Errorf("ReleaseSavepoint: savepoint %s not found", savepointID)` | `fmt.Errorf("ReleaseSavepoint: %w (savepointID=%s)", spi.ErrSavepointNotFound, savepointID)` |
| 511 | `fmt.Errorf("ReleaseSavepoint: savepoint %s not found", savepointID)` | `fmt.Errorf("ReleaseSavepoint: %w (savepointID=%s)", spi.ErrSavepointNotFound, savepointID)` |

(If the file also has a "tx already closed" check in `ReleaseSavepoint` analogous to lines 392/459, wrap with `spi.ErrTxAlreadyCommitted` the same way.)

Leave lines 79, 82 (no user context / no tenant) and the `store_factory.go:159` "TransactionManager not initialized" **untouched** — per spec, SDK-misuse errors stay plain.

- [ ] **Step 4: Build**

```bash
(cd plugins/memory && go build ./...)
```
Expected: clean build. If there's a compilation error, the most likely cause is a variable name in the wrap that doesn't match the function's local variable (e.g. `txID` vs `id`) — adjust to match.

---

### Task B.3: Memory plugin — migrate `entity_store.go`

**Files:**
- Modify: `plugins/memory/entity_store.go`

All eight sites have the same shape: `fmt.Errorf("transaction has been rolled back")` with the tx-state check `if txState.RolledBack { ... }`. The migration adds the sentinel and (where the txID is in scope) the tx ID for debuggability.

- [ ] **Step 1: Migrate each "transaction has been rolled back" site**

For each of the 8 sites (currently at lines 106, 131, 253, 296, 348, 450, 513, 588), apply the same substitution:

Old:
```go
return fmt.Errorf("transaction has been rolled back")
```
or
```go
return ..., fmt.Errorf("transaction has been rolled back")
```

New (preserve the return shape — adjust the function-name prefix to match the enclosing method, e.g. `Save:`, `Get:`, `Delete:`, etc.):
```go
return fmt.Errorf("<MethodName>: %w", spi.ErrTxRolledBack)
```
or
```go
return ..., fmt.Errorf("<MethodName>: %w", spi.ErrTxRolledBack)
```

If `txID` is in scope at the site (check the enclosing function — it usually is via `txState.TxID` or a parameter), include it:
```go
return fmt.Errorf("<MethodName>: %w (txID=%s)", spi.ErrTxRolledBack, txID)
```

Concrete per-site method names (cross-check against the function the line lives in):

| Line | Enclosing method | Prefix to use |
|---|---|---|
| 106 | `Save` | `"Save: %w (txID=%s)"` |
| 131 | `CompareAndSave` | `"CompareAndSave: %w (txID=%s)"` |
| 253 | `Get` | `"Get: %w (txID=%s)"` |
| 296 | `GetAll` | `"GetAll: %w (txID=%s)"` |
| 348 | `Delete` | `"Delete: %w (txID=%s)"` |
| 450 | `Exists` | `"Exists: %w (txID=%s)"` |
| 513 | `Count` | `"Count: %w (txID=%s)"` |
| 588 | `DeleteAll` | `"DeleteAll: %w (txID=%s)"` |

- [ ] **Step 2: Run the conformance suite — observe GREEN**

```bash
(cd plugins/memory && go test ./... -run TestConformance -v 2>&1 | grep -E 'PASS|FAIL.*TxStateErrors|^---')
```
Expected: every `TxStateErrors/*` subtest reports PASS.

If a subtest still FAILs, the most likely cause is that a wrap was missed (the test follows a different code path) or a variable name mismatch. Use `go test -run 'TestConformance/Transaction/TxStateErrors/JoinAfterCommit' -v` to focus and inspect.

- [ ] **Step 3: Run the full memory plugin test suite — ensure no regression**

```bash
(cd plugins/memory && go test ./...)
```
Expected: ALL tests pass, including the existing concurrency tests (which still use `strings.Contains` — they'll be cleaned up in the next task but the substring matches still match the new wrapped messages because we preserved the prefix words like "rolled back", "not found", etc. in the sentinel's `.Error()` strings).

- [ ] **Step 4: Commit**

```bash
git add plugins/memory/txmanager.go plugins/memory/entity_store.go go.mod go.sum plugins/memory/go.mod plugins/memory/go.sum plugins/sqlite/go.mod plugins/sqlite/go.sum plugins/postgres/go.mod plugins/postgres/go.sum
git commit -m "feat(memory): wrap tx-state sentinel errors

Migrates plugins/memory error sites to wrap the new SPI sentinels:

- ErrTxNotFound, ErrSavepointNotFound (wrap ErrNotFound)
- ErrTxTerminated, ErrTxRolledBack, ErrTxAlreadyCommitted
- ErrTxCommitInProgress, ErrTxTenantMismatch

txmanager.go covers lookup/closed/tenant/commit-race/savepoint paths.
entity_store.go covers the 8 'transaction has been rolled back' sites
across Save, CompareAndSave, Get, GetAll, Delete, Exists, Count,
DeleteAll, adding the txID to the wrapped message for debuggability.

GetSubmitTime's 'not yet committed' path (txmanager.go:339) stays plain
per spec (one-site operational signal).

SDK-misuse errors (no user context, no tenant, TransactionManager not
initialized) stay plain per spec.

Also bumps cyoda-go-spi pin to v0.8.0 in root go.mod and all three
plugin go.mods.

Refs #200"
```

---

### Task B.4: Memory plugin — concurrency test cleanup

**Files:**
- Modify: `plugins/memory/concurrency_inreadops_test.go`
- Modify: `plugins/memory/concurrency_cas_test.go`
- Modify: `plugins/memory/concurrency_savepoint_test.go`

The three test files currently use `strings.Contains(msg, ...)` to tolerate race-induced tx-state errors. Replace with `errors.Is`.

- [ ] **Step 1: Verify imports**

In each of the three files, ensure these imports are present:
```go
import (
	"errors"
	...
	spi "github.com/cyoda-platform/cyoda-go-spi"
	...
)
```
If `errors` or `spi` is missing in a file you're editing, add it. The `strings` import becomes unused after this task — Go's compiler will refuse the build if it's left dangling, so the cleanup is forced.

- [ ] **Step 2: Replace the substring blocks in `concurrency_inreadops_test.go`**

Find each block of the shape (current lines around 87-90, 280-283):

```go
msg := err.Error()
if strings.Contains(msg, "rolled back") ||
    strings.Contains(msg, "already completed") ||
    strings.Contains(msg, "not found") ||
    strings.Contains(msg, "already being committed") {
    // tolerated tx-state race
    return
}
```

Replace with:

```go
if errors.Is(err, spi.ErrTxTerminated) ||
    errors.Is(err, spi.ErrTxNotFound) ||
    errors.Is(err, spi.ErrTxCommitInProgress) {
    // tolerated tx-state race
    return
}
```

(The three-way disjunction is required — `ErrTxCommitInProgress` is intentionally NOT under the `ErrTxTerminated` umbrella because the tx is not yet terminal. Spec section "Concurrency Test Update" line 257.)

- [ ] **Step 3: Replace the substring blocks in `concurrency_cas_test.go`**

Same pattern at current lines ~97-100, ~173-175. Apply the same `errors.Is`-based replacement. Note this file tolerates "rolled back" / "already completed" / "not found" but does NOT currently match "already being committed" — for those sites, the two-way disjunction is sufficient:

```go
if errors.Is(err, spi.ErrTxTerminated) ||
    errors.Is(err, spi.ErrTxNotFound) {
    return
}
```

Confirm by grepping the file for `"already being committed"` before deciding two-way vs three-way per site:

```bash
grep -n "already being committed" plugins/memory/concurrency_cas_test.go
```

- [ ] **Step 4: Replace the substring blocks in `concurrency_savepoint_test.go`**

Includes the TODO marker at lines 49-52. Remove the TODO comment block entirely:

```go
// TODO(#200): replace substring matches on tolerated errors ("not found",
// "rolled back", "already closed", "already completed", "already being
// committed") with errors.Is against sentinel error types once they land.
// Until then a closed-mid-op tx is a legitimate outcome, not a defect.
```

Delete those four lines.

Then migrate the substring blocks at lines 123-127, 143-144, 295-296 (and any others — verify with `grep -n "strings.Contains" plugins/memory/concurrency_savepoint_test.go`). The sites that currently match "already being committed" use the three-way disjunction; sites that don't use the two-way.

- [ ] **Step 5: Confirm `strings` import is no longer needed**

```bash
grep -n '"strings"' plugins/memory/concurrency_*.go
```
If `strings` is unused in any of the three files, remove the import line.

- [ ] **Step 6: Run the memory plugin tests**

```bash
(cd plugins/memory && go test ./... -v 2>&1 | tail -30)
```
Expected: all tests pass.

Confirm specifically that the concurrency tests still pass (they're race-conditional so multiple runs may be wise):

```bash
(cd plugins/memory && go test ./... -run 'Concurrency' -count=5)
```
Expected: 5 successful runs.

- [ ] **Step 7: Vet**

```bash
(cd plugins/memory && go vet ./...)
```
Expected: no output.

- [ ] **Step 8: Commit**

```bash
git add plugins/memory/concurrency_inreadops_test.go plugins/memory/concurrency_cas_test.go plugins/memory/concurrency_savepoint_test.go
git commit -m "test(memory): replace strings.Contains with errors.Is on tx-state errors

The three concurrency_*_test.go helpers (runOpVsRollback, runOpVsCommit)
that tolerate tx-state-race errors now use errors.Is against the SPI
sentinels added in cyoda-go-spi v0.8.0 instead of substring matching.

Race-loser tolerance now matches via:
  errors.Is(err, spi.ErrTxTerminated) ||  // rolled-back or already-committed
  errors.Is(err, spi.ErrTxNotFound) ||    // tx purged
  errors.Is(err, spi.ErrTxCommitInProgress)  // mid-Commit race (not terminal)

The third disjunct is required only at sites that currently tolerate
'already being committed' — concurrency_inreadops_test.go:283,
concurrency_savepoint_test.go:143,295.

Drops the TODO(#200) marker block at concurrency_savepoint_test.go:49-52.

Refs #200"
```

---

### Task B.5: SQLite plugin — migrate `txmanager.go` and `entity_store.go`

**Files:**
- Modify: `plugins/sqlite/txmanager.go`
- Modify: `plugins/sqlite/entity_store.go`

SQLite mirrors memory almost line-for-line. The same substitutions apply.

- [ ] **Step 1: Apply the txmanager substitutions**

For sqlite `plugins/sqlite/txmanager.go`, the lines are slightly offset from memory but the error strings are identical. Apply the same table from Task B.2 Steps 2 and 3, matching on the error message text rather than line numbers (the editor's find-and-replace by string is fastest). Sites to migrate (use `grep -n "fmt.Errorf" plugins/sqlite/txmanager.go` to enumerate):

- `"transaction not found: %s"` → `ErrTxNotFound`
- `"transaction already closed: %s"` → `ErrTxAlreadyCommitted`
- `"tenant mismatch on transaction join"` → `ErrTxTenantMismatch`
- `"transaction not found or already completed: %w"` → `ErrTxNotFound`
- `"transaction tenant mismatch"` → `ErrTxTenantMismatch`
- `"transaction already being committed"` → `ErrTxCommitInProgress`
- `"GetSubmitTime: transaction not found: %s"` (around L474) → `ErrTxNotFound`
- All `"Savepoint: ..."`, `"RollbackToSavepoint: ..."`, `"ReleaseSavepoint: ..."` not-found / tenant / already-closed / savepoint-not-found sites → corresponding sentinels per the memory mapping.

Use the same wrap pattern (`fmt.Errorf("MethodName: %w (txID=%s)", spi.ErrXxx, txID)`).

Leave the "no user context" / "no tenant" sites plain.

- [ ] **Step 2: Apply the entity_store substitutions**

For sqlite `plugins/sqlite/entity_store.go`, the eight "transaction has been rolled back" sites mirror memory. Use the same per-method-name table from Task B.3 Step 1.

- [ ] **Step 3: Run sqlite tests — observe GREEN**

```bash
(cd plugins/sqlite && go test ./... -v 2>&1 | grep -E 'PASS|FAIL.*TxStateErrors|^---')
```
Expected: every `TxStateErrors/*` subtest reports PASS.

- [ ] **Step 4: Run sqlite full suite**

```bash
(cd plugins/sqlite && go test ./...)
```
Expected: all tests pass.

- [ ] **Step 5: Vet**

```bash
(cd plugins/sqlite && go vet ./...)
```
Expected: no output.

- [ ] **Step 6: Commit**

```bash
git add plugins/sqlite/txmanager.go plugins/sqlite/entity_store.go
git commit -m "feat(sqlite): wrap tx-state sentinel errors

Mirrors plugins/memory migration: every tx-lifecycle and entity-store
'rolled back' error site now wraps the corresponding SPI sentinel.

Conformance subtests TxStateErrors/* under spitest/transaction.go now
pass on the sqlite backend.

Refs #200"
```

---

### Task B.6: Postgres plugin — migrate `transaction_manager.go`

**Files:**
- Modify: `plugins/postgres/transaction_manager.go`

Postgres has subset coverage: no in-process `RolledBack` state, no commit-in-progress, but full savepoint paths and a `verifyTenant()` helper that all six lifecycle ops route through.

- [ ] **Step 1: Migrate the lookup not-found sites**

| Line | Replace | With |
|---|---|---|
| 118 | `fmt.Errorf("Commit: transaction %s not found", txID)` | `fmt.Errorf("Commit: %w (txID=%s)", spi.ErrTxNotFound, txID)` |
| 122 | `fmt.Errorf("Commit: tx state for %s not found", txID)` | `fmt.Errorf("Commit: %w (txID=%s)", spi.ErrTxNotFound, txID)` |
| 215 | `fmt.Errorf("Rollback: transaction %s not found", txID)` | `fmt.Errorf("Rollback: %w (txID=%s)", spi.ErrTxNotFound, txID)` |
| 220 | `fmt.Errorf("Rollback: transaction %s not found", txID)` | `fmt.Errorf("Rollback: %w (txID=%s)", spi.ErrTxNotFound, txID)` |
| 244 | `fmt.Errorf("Join: transaction %s not found", txID)` | `fmt.Errorf("Join: %w (txID=%s)", spi.ErrTxNotFound, txID)` |
| 249 | `fmt.Errorf("Join: transaction %s not found", txID)` | `fmt.Errorf("Join: %w (txID=%s)", spi.ErrTxNotFound, txID)` |
| 318 | `fmt.Errorf("Savepoint: transaction %s not found", txID)` | `fmt.Errorf("Savepoint: %w (txID=%s)", spi.ErrTxNotFound, txID)` |
| 322 | `fmt.Errorf("Savepoint: tx state for %s not found", txID)` | `fmt.Errorf("Savepoint: %w (txID=%s)", spi.ErrTxNotFound, txID)` |
| 344 | `fmt.Errorf("RollbackToSavepoint: transaction %s not found", txID)` | `fmt.Errorf("RollbackToSavepoint: %w (txID=%s)", spi.ErrTxNotFound, txID)` |
| 348 | `fmt.Errorf("RollbackToSavepoint: tx state for %s not found", txID)` | `fmt.Errorf("RollbackToSavepoint: %w (txID=%s)", spi.ErrTxNotFound, txID)` |
| 370 | `fmt.Errorf("ReleaseSavepoint: transaction %s not found", txID)` | `fmt.Errorf("ReleaseSavepoint: %w (txID=%s)", spi.ErrTxNotFound, txID)` |
| 374 | `fmt.Errorf("ReleaseSavepoint: tx state for %s not found", txID)` | `fmt.Errorf("ReleaseSavepoint: %w (txID=%s)", spi.ErrTxNotFound, txID)` |

- [ ] **Step 2: Migrate the `verifyTenant()` helper (single wrap covers six call sites)**

At line 414:

Old:
```go
return fmt.Errorf("%s: tenant mismatch on transaction %s", op, txID)
```

New:
```go
return fmt.Errorf("%s: %w (txID=%s)", op, spi.ErrTxTenantMismatch, txID)
```

This is the entire postgres tenant-mismatch surface — all of Commit/Rollback/Join/Savepoint/RollbackToSavepoint/ReleaseSavepoint route through here.

- [ ] **Step 3: Leave L270 (`GetSubmitTime`) plain**

Per spec, postgres `GetSubmitTime` at L270 deliberately conflates "not yet committed" with "unknown txID" — stays plain on data-structure grounds. Do not modify.

---

### Task B.7: Postgres plugin — migrate `txstate.go`

**Files:**
- Modify: `plugins/postgres/txstate.go`

- [ ] **Step 1: Migrate the savepoint-not-found sites**

| Line | Replace | With |
|---|---|---|
| 166 | `fmt.Errorf("unknown savepoint %q", id)` | `fmt.Errorf("%w (savepointID=%q)", spi.ErrSavepointNotFound, id)` |
| 223 | `fmt.Errorf("unknown savepoint %q", id)` | `fmt.Errorf("%w (savepointID=%q)", spi.ErrSavepointNotFound, id)` |

- [ ] **Step 2: Add the `spi` import if not present**

If `txstate.go` doesn't already import `spi`, add:
```go
spi "github.com/cyoda-platform/cyoda-go-spi"
```

- [ ] **Step 3: Build**

```bash
(cd plugins/postgres && go build ./...)
```
Expected: clean build.

---

### Task B.8: Postgres plugin — wire `Harness.Skip` and run conformance

**Files:**
- Modify: `plugins/postgres/conformance_test.go`

- [ ] **Step 1: Locate the harness construction**

Open `plugins/postgres/conformance_test.go` and find the `spitest.StoreFactoryConformance(t, spitest.Harness{...})` call (around line 153 per earlier survey).

- [ ] **Step 2: Add the `Skip` map**

Inside the `spitest.Harness{}` literal, add:

```go
Skip: map[string]string{
    "Transaction/TxStateErrors/OpAfterRollback": "postgres: pgx.Tx aborts surface as ErrConflict via SQLSTATE 25P02, not as ErrTxRolledBack",
},
```

(If `Skip` is already present in the literal, add the new entry to the existing map rather than declaring a second one.)

- [ ] **Step 3: Run the postgres conformance suite (requires Docker)**

```bash
(cd plugins/postgres && go test ./... -run TestConformance -v 2>&1 | grep -E 'PASS|FAIL.*TxStateErrors|SKIP.*TxStateErrors|^---')
```
Expected: every `TxStateErrors/*` subtest reports PASS, except `TxStateErrors/OpAfterRollback` which reports SKIP with the documented reason.

If `TxStateErrors/SavepointNotFound` FAILs, postgres' savepoint paths are not wrapping `ErrSavepointNotFound` — re-check Task B.7. Do NOT add it to the Skip map; the spec calls for it to pass on postgres.

- [ ] **Step 4: Run the full postgres test suite**

```bash
(cd plugins/postgres && go test ./...)
```
Expected: all tests pass.

- [ ] **Step 5: Vet**

```bash
(cd plugins/postgres && go vet ./...)
```
Expected: no output.

- [ ] **Step 6: Commit**

```bash
git add plugins/postgres/transaction_manager.go plugins/postgres/txstate.go plugins/postgres/conformance_test.go
git commit -m "feat(postgres): wrap tx-state sentinel errors

Migrates plugins/postgres to wrap the new SPI sentinels:

- ErrTxNotFound — every 'transaction/tx state not found' site across
  Commit, Rollback, Join, and the three savepoint methods (12 sites
  in transaction_manager.go).
- ErrTxTenantMismatch — wrapped once in verifyTenant() at L414; this
  single site covers all six lifecycle ops that call it (Commit,
  Rollback, Join, Savepoint, RollbackToSavepoint, ReleaseSavepoint).
- ErrSavepointNotFound — both 'unknown savepoint' sites in txstate.go.

GetSubmitTime at L270 stays plain per spec — the message deliberately
conflates 'not yet committed' with 'unknown txID' because the
submitTimes map cannot distinguish them without extra bookkeeping.

conformance_test.go declares a single Harness.Skip entry for
TxStateErrors/OpAfterRollback because pgx.Tx surfaces mid-op
rollback as ErrConflict (SQLSTATE 25P02), not as ErrTxRolledBack —
documented behaviour per the ErrTxTerminated SPI godoc.

Refs #200"
```

---

### Task B.9: Update `COMPATIBILITY.md`

**Files:**
- Modify: `COMPATIBILITY.md`

- [ ] **Step 1: Locate the matrix**

Open `COMPATIBILITY.md` and find the `## Compatibility matrix — cyoda-go × cyoda-go-spi` section. The matrix table has columns `cyoda-go` / Root pin / Plugin pin / SPI surface added.

- [ ] **Step 2: Add the v0.8.0 row at the top of the matrix**

Insert above the existing `v0.7.1 (planned)` row (or above whatever is the top row):

```markdown
| **`v0.8.0`** _(planned)_ | `cyoda-go-spi v0.8.0` | `cyoda-go-spi v0.8.0` | Tx-state sentinel hierarchy: `ErrTxNotFound`, `ErrSavepointNotFound`, `ErrTxTerminated`, `ErrTxRolledBack`, `ErrTxAlreadyCommitted`, `ErrTxCommitInProgress`, `ErrTxTenantMismatch` |
```

If `v0.7.1` is no longer marked `_(planned)_` (it has been released), drop the `_(planned)_` qualifier from that row.

- [ ] **Step 3: Check the "Plugin tag history" section**

The plugin tag history table at the bottom needs no change for this PR — the in-tree plugin module tags are bumped as part of the v0.8.0 release coordination by the maintainer, not by this PR.

- [ ] **Step 4: Commit**

```bash
git add COMPATIBILITY.md
git commit -m "docs(compatibility): record v0.8.0 SPI surface (#200 sentinels)

Adds the cyoda-go v0.8.0 row to the cyoda-go × cyoda-go-spi
compatibility matrix, listing the seven new transaction-state
sentinels added in cyoda-go-spi v0.8.0.

Refs #200"
```

---

### Task B.10: Full verification

**Files:** None (verification only).

- [ ] **Step 1: Run `make test-all`**

```bash
make test-all
```
Expected: all green (root + memory + sqlite + postgres). Postgres requires Docker; if it's not running, start it first.

- [ ] **Step 2: Run each plugin's full suite explicitly**

```bash
(cd plugins/memory && go test ./...)
(cd plugins/sqlite && go test ./...)
(cd plugins/postgres && go test ./...)
```
Expected: each module green. (`make test-all` should have covered this, but the explicit run confirms.)

- [ ] **Step 3: Vet each module**

```bash
go vet ./...
(cd plugins/memory && go vet ./...)
(cd plugins/sqlite && go vet ./...)
(cd plugins/postgres && go vet ./...)
```
Expected: no output anywhere.

- [ ] **Step 4: One-shot race detector**

```bash
go test -race ./...
(cd plugins/memory && go test -race ./...)
(cd plugins/sqlite && go test -race ./...)
(cd plugins/postgres && go test -race ./...)
```
Expected: all green; no race reports. Per project convention this is a one-shot sanity check before PR, not a per-step gate.

- [ ] **Step 5: Confirm no orphaned `strings.Contains` on tx-state errors**

```bash
grep -rn 'strings.Contains.*rolled back\|strings.Contains.*already completed\|strings.Contains.*already being committed\|strings.Contains.*tenant mismatch' --include='*.go' .
```
Expected: no matches. (If matches remain in `plugins/sqlite/` or other locations, they were missed in this migration — go back and fix.)

- [ ] **Step 6: Confirm the TODO marker is gone**

```bash
grep -rn 'TODO(#200)' --include='*.go' .
```
Expected: no matches.

- [ ] **Step 7: Confirm no local `replace` directives remain**

```bash
grep -n 'replace.*cyoda-go-spi' go.mod plugins/*/go.mod
```
Expected: no matches. (If any remain from the dev-time iteration described at the top of this plan, remove them and re-run `go mod tidy`.)

---

### Task B.11: Open the PR

- [ ] **Step 1: Push the branch**

```bash
git push -u origin feat/200-spi-tx-state-sentinel-errors
```

- [ ] **Step 2: Open the PR against `release/v0.8.0`**

```bash
gh pr create --base release/v0.8.0 \
             --title "feat(spi): tx-state sentinel errors — closes #200" \
             --body "$(cat <<'EOF'
Replaces plain-string transaction-state errors across the memory, sqlite, and postgres storage plugins with the SPI sentinel hierarchy added in `cyoda-go-spi v0.8.0`.

Spec: [`docs/superpowers/specs/2026-06-13-200-spi-tx-state-sentinel-errors-design.md`](../blob/feat/200-spi-tx-state-sentinel-errors/docs/superpowers/specs/2026-06-13-200-spi-tx-state-sentinel-errors-design.md)

Plan: [`docs/superpowers/plans/2026-06-13-200-spi-tx-state-sentinel-errors.md`](../blob/feat/200-spi-tx-state-sentinel-errors/docs/superpowers/plans/2026-06-13-200-spi-tx-state-sentinel-errors.md)

**What changes**

- Root `go.mod` + all three plugin `go.mod`s bump `cyoda-go-spi` pin from `v0.7.1` to `v0.8.0`.
- `plugins/memory/{txmanager.go,entity_store.go}` — every tx-state error site now wraps the relevant SPI sentinel; the eight "transaction has been rolled back" sites in `entity_store.go` gain a `txID=` debuggability field.
- `plugins/memory/concurrency_{inreadops,cas,savepoint}_test.go` — replaces `strings.Contains` tolerance blocks with `errors.Is`. Three-way disjunction (`ErrTxTerminated || ErrTxNotFound || ErrTxCommitInProgress`) at the three sites that tolerate "already being committed"; two-way elsewhere. Drops the `TODO(#200)` marker.
- `plugins/sqlite/{txmanager.go,entity_store.go}` — mirrors memory.
- `plugins/postgres/{transaction_manager.go,txstate.go,conformance_test.go}` — wraps the lookup/tenant/savepoint sites; `verifyTenant()` at L414 is a single wrap that carries the entire postgres tenant-mismatch contract. `OpAfterRollback` conformance subtest is skipped on postgres with a documented reason (pgx.Tx surfaces mid-op rollback as `ErrConflict` via SQLSTATE 25P02).
- `COMPATIBILITY.md` matrix gains the v0.8.0 row listing the seven new sentinels.

**Not in this PR (deliberately)**

- HTTP/gRPC status mapping based on the new sentinels — separate follow-up per `#200` body.
- Cassandra plugin migration — separate repo, picks up on its next SPI bump.
- SDK-misuse error promotion (`no user context`, `no tenant`, `TransactionManager not initialized`) — these stay plain per spec.

Closes #200

🤖 Generated with [Claude Code](https://claude.com/claude-code)
EOF
)" \
             --milestone v0.8.0
```

(Verify the milestone name matches the repo's actual milestone. Per project convention, milestone-targeted PRs are mandatory on `release/vX.Y.Z` branches.)

- [ ] **Step 3: Hand off for review**

The PR is now ready for code-review and security-review per the project workflow. Per `CLAUDE.md`:
- `code-review` skill review
- `antigravity-bundle-security-developer:cc-skill-security-review` security audit
- Address feedback via `superpowers:receiving-code-review`
- Merge when both reviewers approve

---

## Self-review

After writing this plan, checking it against the spec:

- ✅ Sentinel set (7 sentinels with hierarchy) — Task A.2 defines all seven exactly.
- ✅ Backward compat for `ErrNotFound` wrap — `sentinelErr.Unwrap()` chain in Task A.2; verified by `TestTxSentinelHierarchy` and `TestTxSentinelWrapChain` in Task A.1.
- ✅ Postgres godoc caveat for `ErrTxTerminated` — included in Task A.2 godoc.
- ✅ Pure sentinel-graph tests in `errors_test.go` — Task A.1.
- ✅ Per-backend conformance in `spitest/transaction.go` — Task A.3.
- ✅ Memory plugin migration — Tasks B.2, B.3 (txmanager + entity_store).
- ✅ Memory concurrency-test cleanup — Task B.4 (three-way for "already being committed" sites, two-way elsewhere; TODO marker removed).
- ✅ SQLite migration — Task B.5.
- ✅ Postgres migration — Tasks B.6, B.7, B.8 including verifyTenant single-wrap, txstate.go, and Harness.Skip.
- ✅ COMPATIBILITY.md update — Task B.9.
- ✅ Verification (per-plugin tests, vet, race) — Task B.10.
- ✅ PR open targeting `release/v0.8.0` with milestone — Task B.11.
- ✅ Out-of-scope items (HTTP mapping, Cassandra, SDK-misuse promotion) explicitly excluded — referenced in B.11 PR body.

No placeholders. Every step has concrete code or commands. Types and method names are consistent between Task A.1 (tests) and Task A.2 (definitions); between Task A.3 (subtests) and Tasks B.2–B.8 (migrations). The plan is decomposable into bite-sized red/green slices; each commit is self-contained and reviewable.
