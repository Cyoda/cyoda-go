# Transaction Lifecycle Safety Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make every transaction roll back and return its connection on every exit path — including a panic — under a DB-side ceiling the application cannot dodge, and delete the dead lifecycle manager that pretended to do this.

**Architecture:** Four pieces. (1) A `txScope` value type in `internal/domain/entity` replaces 40 hand-written `rollbackOwned` calls with one deferred `Release`, plus a panic-safe guard inside the workflow engine over the segments the engine itself opens. (2) PostgreSQL `statement_timeout` / `idle_in_transaction_session_timeout` GUCs on the app pool and a Go-side acquire deadline, with the resulting server aborts classified into real status codes. (3) A single shared startup sequence that runs migrations before the schema-compat check so a booting node stops false-alarming on a peer's in-flight migration. (4) Removal of `internal/cluster/lifecycle` and every doc claim that describes it as working.

**Tech Stack:** Go 1.26+, pgx/v5 + pgxpool, golang-migrate/v4, grpc-go, `log/slog`, testcontainers-go (E2E Postgres).

## Global Constraints

Values are settled (spec §Status). Do not renegotiate them; do not build compatibility shims.

- Ceiling defaults: `CYODA_POSTGRES_STATEMENT_TIMEOUT` **5m**, `CYODA_POSTGRES_IDLE_IN_TX_TIMEOUT` **5m**, `CYODA_POSTGRES_ACQUIRE_TIMEOUT` **10s**, `CYODA_POSTGRES_MIGRATE_LOCK_TIMEOUT` **5m**, `CYODA_POSTGRES_SEARCH_STATEMENT_TIMEOUT` **30m**. `0` disables any of the four PostgreSQL settings.
- Backward compatibility is **not** a constraint. The user base is approximately zero. Pick the correct default and record it under `### Breaking` in `CHANGELOG.md`. Build no upgrade machinery for instances below schema version 2.
- `internal/cluster/lifecycle` and `CYODA_TX_TTL` / `CYODA_TX_REAP_INTERVAL` / `CYODA_TX_OUTCOME_TTL` are **deleted**, not wired.
- Rollback context is always `context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)`. `WithoutCancel` is mandatory: `verifyTenant` reads the `UserContext` off it on all three in-tree plugins.
- No issue numbers (`#471`, `#466`, …) in any shipped artefact — not in error messages, log output, response bodies, code comments, OpenAPI descriptions, or help topics. Issue IDs belong in commit messages and the PR body only.
- `log/slog` only. Wrap errors with `fmt.Errorf("...: %w", err)`. 4xx carries domain detail; 5xx is a generic message plus a ticket UUID.
- `plugins/postgres` is its own Go module. `go test ./...` from the repo root does **not** reach it. Run plugin tests explicitly: `cd plugins/postgres && go test ./...`.
- TDD is mandatory (`.claude/rules/tdd.md`). Every task below starts with a failing test. No `-race` during iteration — one `make race` before the PR.
- Concurrency and fault-injection tests are isolated single-backend E2E, never the shared parity suite. This change adds **no** parity cells: `BackendFixture` is API-only and cannot inject an in-process panic or observe transaction state (spec §6).

## Out of Scope

Verified as needing no change, or deliberately deferred. Do not widen into these; if one turns out to be load-bearing, surface it rather than absorbing it.

- **`internal/domain/model` and the message paths.** Neither opens a transaction of its own — `beginOrJoin` is the entity service's alone — so they do not share the shape `txScope` fixes and need no conversion.
- **The scheduled-fire path is not a leak path.** It enters via `FireScheduledTransition`, which already has the deferred guard (`fire_scheduled.go:112-135`) — a `committed` flag, a deferred rollback, and `curCtx`/`curTxID` advanced on segmentation. That is the discipline being extracted, not a site to convert.
- **Capping `responseTimeoutMs` against the idle ceiling.** It is per-processor workflow config and currently uncapped, so a workflow may configure a callout longer than 5m and have its transaction aborted. Reconciled here by classifying SQLSTATE `25P03` into a clear, actionable error and documenting the relationship, not by adding cross-layer validation between workflow import and storage-plugin config.
- **Request-level context deadlines.** Non-transactional reads still queue unboundedly on acquire — every statement through `store_factory.go:114`'s pool fallback and the thirteen direct pool calls in `search_store.go`. Task 10's "every connection is released within `max(statement_timeout, idle_in_tx_timeout)`" claim is about connection *release*, not about waiting.
- **Context-aware acquisition for `txgate.Registry.Acquire` and `tx.OpMu`.** Both would turn the rollback's bound from "terminates because the holder terminates" into a hard deadline, and both are changes to the concurrency model of core plus two plugins rather than to transaction lifecycle.
- **`fetchEntityTransitions`'s missing 503**, which its documented alias `getEntityTransitions` declares (`api/openapi.yaml:1584`, `transitions_handler.go:117`). Pre-existing drift on the same surface, unrelated to this change's mechanism; recorded so it is not mistaken for something introduced here.

## File Structure

**New files**

| File | Responsibility |
|---|---|
| `internal/common/rollback.go` | `RollbackContext` — the one definition of the bounded, cancellation-free rollback context, shared by `internal/domain/entity` and `internal/domain/workflow`. |
| `internal/domain/entity/txscope.go` | `txScope` + `beginScope`. Owns transaction lifecycle for every entity write flow. |
| `internal/domain/entity/txscope_test.go` | Unit coverage for `Release` / `Advance` / `Commit` semantics against a stub `TransactionManager`. |
| `internal/grpc/recovery.go` | Unary + stream panic-recovery interceptors. |
| `internal/grpc/recovery_test.go` | Panic-through-a-handler coverage. |
| `plugins/postgres/ceilings.go` | GUC duration rendering, ceiling parsing, `RuntimeParams` precedence, `acquireTimeoutError`. |
| `plugins/postgres/ceilings_test.go` | Rendering, parse rejection, precedence (unit, no server). |
| `cmd/cyoda/help/content/errors/STORAGE_UNAVAILABLE.md` | Error-code help topic (`TestErrCode_Parity` is a strict bijection). |
| `internal/e2e/tx_lifecycle_e2e_test.go` | Panic/rollback/pool-baseline E2E on a dedicated tiny-pool harness. |
| `internal/e2e/storage_ceilings_e2e_test.go` | 503 / 500-with-ticket ceiling E2E, HTTP and gRPC. |
| `plugins/postgres/migrate_concurrency_test.go` | Concurrent-boot interleave, dirty-flag, lock-timeout tests (live server). |
| `plugins/postgres/migration_index_guard_test.go` | Static rule over `migrations/*.up.sql`. |

**Modified files** (principal responsibility change only)

| File | Change |
|---|---|
| `internal/domain/entity/handler.go` | `rollbackOwned` deleted; `classifyBeginErr` / `storageUnavailable` added. |
| `internal/domain/entity/service.go` | Eight flows converted to `txScope`; 40 `rollbackOwned` calls and 3 dead nil-guards deleted; collection isolation gated on `segmented`. |
| `internal/domain/workflow/engine.go` | Panic-safe segment guard on `Execute` / `ManualTransition` / `Loopback`; criterion `txID` corrected to the current segment. |
| `internal/domain/workflow/engine_processors.go` | `rollbackOpenSegmentOnFailure` and four plain rollbacks removed; `executeCommitBeforeDispatch` gets its own guard. |
| `app/app.go` | Handler-wide `Recovery`; lifecycle manager, reaper goroutine and `TxLifecycle()` removed. |
| `plugins/postgres/config.go` | Ceiling config fields + parsing; `newPool` applies `RuntimeParams`. |
| `plugins/postgres/transaction_manager.go` | Acquire-scoped context on `Begin`; `25P03` / `57014` classification + `cleanupTx`. |
| `plugins/postgres/migrate.go` | `openDB` takes migration settings; `ensureSchema` shared sequence; `ErrDirty` translation. |
| `docs/ARCHITECTURE.md` | Full audit (§4.4) — its own commit. |

---

## Task 1: Engine-side segment guard

The handler cannot cover the engine's segments: `Execute`, `ManualTransition` and `Loopback` return a **nil** `EngineResult` on every error path, so there is nothing for the caller to advance from. A criterion callout failing mid-cascade — an ordinary occurrence — therefore leaks TX_post today, with no panic involved.

**Files:**
- Create: `internal/common/rollback.go`
- Create: `internal/common/rollback_test.go`
- Modify: `internal/domain/workflow/engine.go` (`Execute` ~`:238-308`, `ManualTransition` ~`:329-385`, `Loopback` ~`:404-472`)
- Modify: `internal/domain/workflow/engine_processors.go` (delete `rollbackOpenSegmentOnFailure` `:145-159`; delete its two call sites `:79`, `:138`; delete plain rollbacks `:292`, `:341`, `:349`, `:353`; add guard to `executeCommitBeforeDispatch` `:251`)
- Test: `internal/domain/workflow/engine_segment_guard_test.go` (new)

**Interfaces:**
- Consumes: nothing from earlier tasks.
- Produces:
  - `func common.RollbackContext(ctx context.Context) (context.Context, context.CancelFunc)` — Task 3 uses this.
  - `func (e *Engine) rollbackSegment(ctx context.Context, openTxID, entryTxID string)` — unexported; no other task calls it.
  - Behavioural guarantee Task 4 depends on: after any error return from `Execute` / `ManualTransition` / `Loopback`, no engine-opened segment is left open.

- [ ] **Step 1: Write the failing test for `common.RollbackContext`**

Create `internal/common/rollback_test.go`:

```go
package common

import (
	"context"
	"testing"
	"time"
)

type ucKey struct{}

func TestRollbackContext_DropsCancellationKeepsValues(t *testing.T) {
	parent, cancel := context.WithCancel(context.WithValue(context.Background(), ucKey{}, "tenant-a"))
	cancel() // the request is already dead — the rollback must still run

	rbCtx, rbCancel := RollbackContext(parent)
	defer rbCancel()

	if err := rbCtx.Err(); err != nil {
		t.Fatalf("rollback context inherited cancellation: %v", err)
	}
	if got := rbCtx.Value(ucKey{}); got != "tenant-a" {
		t.Fatalf("rollback context lost parent values: got %v", got)
	}
	dl, ok := rbCtx.Deadline()
	if !ok {
		t.Fatal("rollback context has no deadline; a wedged Rollback would block the unwinding goroutine forever")
	}
	if d := time.Until(dl); d <= 0 || d > 5*time.Second {
		t.Fatalf("deadline %v is not the documented 5s bound", d)
	}
}
```

- [ ] **Step 2: Run it and confirm it fails**

Run: `go test ./internal/common/ -run TestRollbackContext -v`
Expected: FAIL — `undefined: RollbackContext`.

- [ ] **Step 3: Implement `common.RollbackContext`**

Create `internal/common/rollback.go`:

```go
package common

import (
	"context"
	"time"
)

// rollbackBudget bounds the Rollback call itself. It does NOT bound the wait to
// reach it: a rollback first acquires the per-tx gate, and txgate.Registry.Acquire
// is a plain sync.Mutex with no context (memory and sqlite additionally take
// tx.OpMu inside Rollback). That wait terminates because the gate is never held
// across a dispatch and every operation it can be waiting on is itself bounded —
// by a callout's response timeout, or by the PostgreSQL statement ceiling. Making
// it a hard bound means giving both mutexes context-aware variants, which is a
// change to the concurrency model of core plus two plugins.
const rollbackBudget = 5 * time.Second

// RollbackContext derives the context a rollback must run on: the caller's
// values without the caller's cancellation, under a bounded deadline.
//
// WithoutCancel is load-bearing, not defensive. Every in-tree plugin's Rollback
// calls verifyTenant, which reads the UserContext off this context; a rollback on
// context.Background() is rejected with ErrTxTenantMismatch and the transaction
// leaks anyway. Dropping cancellation is what lets a timed-out or client-aborted
// request still return its connection.
func RollbackContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), rollbackBudget)
}
```

- [ ] **Step 4: Confirm it passes**

Run: `go test ./internal/common/ -run TestRollbackContext -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/common/rollback.go internal/common/rollback_test.go
git commit -m "feat(tx): add the bounded, cancellation-free rollback context"
```

- [ ] **Step 6: Write the failing engine-guard tests**

These are the spec's coverage rows 4a (non-panic error after segmentation), 4b (every `executeCommitBeforeDispatch` failure path), 4 (panic after segmentation) and 8c (memory and sqlite, not only postgres).

Create `internal/domain/workflow/engine_segment_guard_test.go`. Follow the existing construction helpers in `engine_ifmatch_test.go` for building an `Engine` over a memory factory; the assertions below are what is new.

```go
package workflow

import (
	"context"
	"errors"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// countingTxMgr wraps a real TransactionManager and records which txIDs were
// rolled back, so a test can assert the engine released the segment it opened
// without reaching into plugin internals.
type countingTxMgr struct {
	spi.TransactionManager
	rolledBack []string
}

func (c *countingTxMgr) Rollback(ctx context.Context, txID string) error {
	c.rolledBack = append(c.rolledBack, txID)
	return c.TransactionManager.Rollback(ctx, txID)
}

func (c *countingTxMgr) sawRollbackOf(txID string) bool {
	for _, id := range c.rolledBack {
		if id == txID {
			return true
		}
	}
	return false
}

// TestEngine_CriterionFailureAfterSegment_RollsBackOpenSegment is coverage row 4a.
// A FUNCTION criterion that fails mid-cascade is an ordinary occurrence — a
// compute node being down is enough — and it returns from cascadeAutomated,
// outside executeProcessors, after currentTxID has advanced past a
// COMMIT_BEFORE_DISPATCH segment.
func TestEngine_CriterionFailureAfterSegment_RollsBackOpenSegment(t *testing.T) {
	for _, backend := range []string{"memory", "sqlite"} { // coverage row 8c
		t.Run(backend, func(t *testing.T) {
			h := newSegmentGuardHarness(t, backend)

			// Workflow: state A --[CBD processor]--> B --[FUNCTION criterion]--> C.
			// The CBD processor commits TX_pre and opens TX_post; the criterion
			// on the next automated transition then fails.
			h.registerCBDProcessor("segmenter")
			h.registerFailingCriterion("gatekeeper", errors.New("compute node unavailable"))

			entryTxID, entryCtx := h.begin(t)
			_, err := h.engine.Execute(entryCtx, h.entity, "")
			if err == nil {
				t.Fatal("expected the criterion failure to surface")
			}

			openTxID := h.lastSegmentTxID(t) // TX_post, recorded by the CBD stub
			if openTxID == entryTxID {
				t.Fatal("test did not segment; it proves nothing")
			}
			if !h.txMgr.sawRollbackOf(openTxID) {
				t.Fatalf("engine leaked segment %s after a criterion failure", openTxID)
			}
			if h.txMgr.sawRollbackOf(entryTxID) {
				t.Fatalf("engine rolled back the caller's entry transaction %s; that is the caller's to own", entryTxID)
			}
		})
	}
}

// TestEngine_PanicAfterSegment_RollsBackOpenSegment is coverage row 4. The guard
// must not swallow the panic — the door's recovery middleware owns that decision.
func TestEngine_PanicAfterSegment_RollsBackOpenSegment(t *testing.T) {
	h := newSegmentGuardHarness(t, "memory")
	h.registerCBDProcessor("segmenter")
	h.registerPanickingCriterion("boom")

	entryTxID, entryCtx := h.begin(t)

	func() {
		defer func() {
			if rec := recover(); rec == nil {
				t.Error("guard swallowed the panic; recovery is the door's job, not the engine's")
			}
		}()
		_, _ = h.engine.Execute(entryCtx, h.entity, "")
	}()

	openTxID := h.lastSegmentTxID(t)
	if !h.txMgr.sawRollbackOf(openTxID) {
		t.Fatalf("engine leaked segment %s through a panic", openTxID)
	}
	if h.txMgr.sawRollbackOf(entryTxID) {
		t.Fatalf("engine rolled back the caller's entry transaction %s", entryTxID)
	}
}

// TestExecuteCommitBeforeDispatch_EveryFailurePathRollsBack is coverage row 4b.
// Every failure return in executeCommitBeforeDispatch is `return nil, "", err`,
// which is exactly why the guard must read dedicated locals rather than the
// named returns.
func TestExecuteCommitBeforeDispatch_EveryFailurePathRollsBack(t *testing.T) {
	cases := []struct {
		name string
		fail func(*segmentGuardHarness)
	}{
		{"dispatch error, startNewTxOnDispatch=true", (*segmentGuardHarness).failDispatchNewTx},
		{"apply processor data", (*segmentGuardHarness).failApplyProcessorData},
		{"entity store for CAS", (*segmentGuardHarness).failEntityStoreLookup},
		{"CompareAndSave conflict", (*segmentGuardHarness).failCompareAndSave},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newSegmentGuardHarness(t, "memory")
			h.registerCBDProcessor("segmenter")
			tc.fail(h)

			entryTxID, entryCtx := h.begin(t)
			if _, err := h.engine.Execute(entryCtx, h.entity, ""); err == nil {
				t.Fatal("expected a failure")
			}
			openTxID := h.lastSegmentTxID(t)
			if openTxID != "" && !h.txMgr.sawRollbackOf(openTxID) {
				t.Fatalf("TX_post %s leaked on the %q path", openTxID, tc.name)
			}
			_ = entryTxID
		})
	}
}
```

Write `newSegmentGuardHarness` in the same file. It builds the factory for the named backend, a `countingTxMgr` wrapping the plugin's manager, an `Engine` with a stub `extProc` whose `DispatchProcessor` / `DispatchCriteria` behaviour each test configures, a one-entity model, and a workflow whose first automated transition carries a `COMMIT_BEFORE_DISPATCH` processor. `lastSegmentTxID` returns the txID the stub observed on the post-segment dispatch. Model it on the harness already in `engine_ifmatch_test.go`, which builds the same CBD shape.

- [ ] **Step 7: Run them and confirm they fail**

Run: `go test ./internal/domain/workflow/ -run 'TestEngine_CriterionFailureAfterSegment|TestEngine_PanicAfterSegment|TestExecuteCommitBeforeDispatch_EveryFailurePath' -v`
Expected: FAIL — the criterion and panic cases report a leaked segment; the CBD sub-cases for `apply processor data` / `entity store for CAS` / `CompareAndSave conflict` may pass already (those three have plain rollbacks today) while the panic and criterion cases fail. Record which failed; the guard must keep the passing ones passing.

- [ ] **Step 8: Add the guard helper and the three entry-point guards**

In `internal/domain/workflow/engine.go`, add:

```go
// rollbackSegment releases a transaction segment the engine opened and never
// handed back to its caller. It is a no-op for the caller's own entry
// transaction — that one is the caller's to commit or roll back.
//
// Nil-safe on txMgr: segmentation implies a transaction manager, so this cannot
// be nil in production, but the engine is constructed without one in unit tests.
func (e *Engine) rollbackSegment(ctx context.Context, openTxID, entryTxID string) {
	if e.txMgr == nil || openTxID == "" || openTxID == entryTxID {
		return
	}
	rbCtx, cancel := common.RollbackContext(ctx)
	defer cancel()
	if err := e.txMgr.Rollback(rbCtx, openTxID); err != nil && !errors.Is(err, spi.ErrTxNotFound) {
		slog.Warn("failed to roll back engine-opened segment",
			"pkg", "workflow", "txID", openTxID, "err", err)
	}
}
```

In `Execute`, immediately after `txID := e.resolveAuditTxID(entity)` (`:251`):

```go
	// The engine owns every segment it opens after entryTxID until it hands one
	// back on the success return. openCtx/openTxID are dedicated locals, NOT the
	// named returns: every failure path in executeCommitBeforeDispatch is
	// `return nil, "", err`, so a guard reading a named newTxID return would see
	// "" on exactly the paths that need it and skip the rollback.
	//
	// entryTxID is resolveAuditTxID's value, which equals the handler's
	// transaction because every handler stamps Meta.TransactionID = txID before
	// calling in. flushAndCommitSegment's Commit(ctx, txID) already relies on the
	// same invariant.
	entryTxID := txID
	openCtx, openTxID := ctx, txID
	handedOff := false
	defer func() {
		if !handedOff {
			e.rollbackSegment(openCtx, openTxID, entryTxID)
		}
	}()
```

Advance `openCtx/openTxID` at each point the engine segments — immediately after the `attemptTransition` and `cascadeAutomated` calls, **before** their `if err != nil` checks, mirroring `fire_scheduled.go:405-412`:

```go
	if transitionName != "" {
		nCtx, nTxID, err := e.attemptTransition(currentCtx, entity, selectedWF, transitionName, auditStore, currentTxID)
		currentCtx = nCtx
		currentTxID = nTxID
		openCtx, openTxID = currentCtx, currentTxID
		if err != nil {
			return nil, err
		}
	}

	nCtx, nTxID, err := e.cascadeAutomated(currentCtx, entity, selectedWF, auditStore, currentTxID)
	currentCtx = nCtx
	currentTxID = nTxID
	openCtx, openTxID = currentCtx, currentTxID
	if err != nil {
		return nil, err
	}
```

Set `handedOff = true` on the single success path, immediately before building the `EngineResult`:

```go
	handedOff = true
	return &EngineResult{
```

Apply the identical treatment to `ManualTransition` (guard after `:343`; advance after `attemptTransition` `:357` and `cascadeAutomated` `:362`; `handedOff = true` before `:376`) and to `Loopback` (guard after `:417`; advance after `cascadeAutomated` `:449`; `handedOff = true` before both the `STATE_NOT_IN_WORKFLOW` early return at `:437` and the final return at `:463`).

`openTxID != entryTxID` is a sound test because every early return in `attemptTransition` (`:628`, `:642`, `:648`, `:655`) and `fireTransition` (`:685`, `:702`, `:704`) returns the *input* ctx/txID. That is safe only because nothing can have segmented at those points — processors run after criteria. Record that next to the guard:

```go
// Invariant: attemptTransition and fireTransition return their INPUT ctx/txID on
// every early exit, so openTxID != entryTxID cannot be true unless a processor
// actually segmented. Processors run after criteria; reordering them would break
// this guard silently.
```

- [ ] **Step 9: Add the guard to `executeCommitBeforeDispatch`**

It opens TX_post at `:333` and `:409` and can panic before returning it to `executeProcessors`, so it needs its own. In `internal/domain/workflow/engine_processors.go`, immediately after `tPre := txID` (`:252`):

```go
	// This function opens TX_post and can panic before handing it to
	// executeProcessors. Its own guard covers that window; the entry-point guard
	// covers everything after the hand-off.
	segCtx, segTxID := ctx, txID
	segHandedOff := false
	defer func() {
		if !segHandedOff {
			e.rollbackSegment(segCtx, segTxID, txID)
		}
	}()
```

Advance `segCtx/segTxID` immediately after each `Begin` — after `commitAndBeginNextSegment` returns (`:273`) and after the `=false` branch's `e.txMgr.Begin` (`:333`) — before the error checks:

```go
		newTxID, newCtx, err = e.commitAndBeginNextSegment(ctx, entity, txID, expectedFirstFlushTxID, ifMatchConsumed)
		segCtx, segTxID = newCtx, newTxID
		if err != nil {
			...
			return nil, "", err
		}
```

Note `commitAndBeginNextSegment` returns `("", nil, err)` on failure, so `segTxID` becomes `""` and `rollbackSegment` no-ops — correct, since no segment was opened. Set `segHandedOff = true` immediately before the success return at `:357`.

- [ ] **Step 10: Delete `rollbackOpenSegmentOnFailure` and the four plain rollbacks**

The guards subsume all of it. In `engine_processors.go`:
- Delete the function (`:145-159`) and both call sites (`:79`, `:138`).
- Delete `_ = e.txMgr.Rollback(newCtx, newTxID)` at `:292`, `:341`, `:349`, `:353`.
- Rewrite the `executeProcessors` doc comment's final paragraph (`:38-42`), which describes the deleted behaviour, to point at the entry-point guard instead.
- Rewrite the claim at `:145-148` — "The caller-side handler tracks the original entryTxID and will roll that back via its own deferred-rollback path" — as a statement of what now exists, and move it next to the entry-point guard in `engine.go`.

Net: fewer moving parts than before, and the comment becomes true for the first time.

- [ ] **Step 11: Run the new tests plus the whole workflow package**

Run: `go test ./internal/domain/workflow/ -v`
Expected: PASS, including the tests that passed in Step 7.

- [ ] **Step 12: Run the entity and E2E suites for regressions**

Run: `go test ./internal/domain/entity/... && go test ./internal/e2e/... -run 'CommitBeforeDispatch|CBD|Segment'`
Expected: PASS.

- [ ] **Step 13: Commit**

```bash
git add internal/domain/workflow/
git commit -m "fix(workflow): roll back engine-opened segments on every exit path

Execute, ManualTransition and Loopback return a nil EngineResult on every
error, so the caller has nothing to advance from and TX_post is dropped. A
FUNCTION-criterion callout failing mid-cascade reaches this without any panic.
One panic-safe guard per entry point, over dedicated locals rather than the
named returns, subsumes rollbackOpenSegmentOnFailure and four plain rollbacks."
```

---

## Task 2: Criterion dispatch carries the current segment's txID

`engine.go:772` builds the `criterionContext` with `ctx: currentCtx` but `txID: txID` — the cascade-*entry* txID. After a COMMIT_BEFORE_DISPATCH segment that names a **committed** transaction, and it is what `DispatchCriteria` hands the compute node as its join token, so a criterion callback attempting to join gets `ErrTxNotFound`. Same transaction-identity-versus-segment-identity confusion as Task 1, same file.

**Files:**
- Modify: `internal/domain/workflow/engine.go:770-773`
- Test: `internal/domain/workflow/engine_segment_guard_test.go` (extend)
- Test: `internal/e2e/callback_join_e2e_test.go` (extend — locate the existing joined-callback test and add the post-segment case beside it)

**Interfaces:**
- Consumes: `newSegmentGuardHarness` from Task 1.
- Produces: no new symbols. Behaviour: a criterion evaluated after a CBD segment receives the open segment's txID.

- [ ] **Step 1: Write the failing test**

Add to `internal/domain/workflow/engine_segment_guard_test.go` (coverage row 4c):

```go
// TestEngine_CriterionAfterSegment_CarriesCurrentSegmentTxID: the txID handed to
// a FUNCTION criterion is the compute node's join token. After a CBD segment the
// cascade-entry txID names a COMMITTED transaction, so a callback joining on it
// gets ErrTxNotFound.
func TestEngine_CriterionAfterSegment_CarriesCurrentSegmentTxID(t *testing.T) {
	h := newSegmentGuardHarness(t, "memory")
	h.registerCBDProcessor("segmenter")

	var criterionTxID string
	h.registerCriterion("gatekeeper", func(ctx context.Context, txID string) (bool, string, error) {
		criterionTxID = txID
		return true, "", nil
	})

	entryTxID, entryCtx := h.begin(t)
	if _, err := h.engine.Execute(entryCtx, h.entity, ""); err != nil {
		t.Fatalf("execute: %v", err)
	}

	segmentTxID := h.lastSegmentTxID(t)
	if segmentTxID == entryTxID {
		t.Fatal("test did not segment; it proves nothing")
	}
	if criterionTxID == entryTxID {
		t.Fatalf("criterion was handed the committed entry txID %s as its join token", entryTxID)
	}
	if criterionTxID != segmentTxID {
		t.Fatalf("criterion join token = %s, want the open segment %s", criterionTxID, segmentTxID)
	}
}
```

Extend the harness with `registerCriterion(name string, fn func(ctx context.Context, txID string) (bool, string, error))`, recording the `txID` argument `DispatchCriteria` receives.

- [ ] **Step 2: Run it and confirm it fails**

Run: `go test ./internal/domain/workflow/ -run TestEngine_CriterionAfterSegment -v`
Expected: FAIL — `criterion was handed the committed entry txID ... as its join token`.

- [ ] **Step 3: Fix the criterion context**

`internal/domain/workflow/engine.go:770-773`:

```go
			if len(tr.Criterion) > 0 && string(tr.Criterion) != "null" {
				matched, reason, err := e.evaluateCriterion(tr.Criterion, entity, &criterionContext{
					// currentTxID, not txID: after a COMMIT_BEFORE_DISPATCH segment the
					// cascade-entry txID names a committed transaction, and this value is
					// the compute node's join token — a callback joining on it would get
					// ErrTxNotFound. Audit correlation keeps using the entry txID; that is
					// a separate concern from transaction identity.
					ctx: currentCtx, txID: currentTxID, workflowName: wf.Name, transitionName: tr.Name, target: "TRANSITION",
				})
```

Check `fireTransition`'s criterion at `:681-683` too: it passes `ctx: ctx, txID: txID`, both of which are that function's inputs, and nothing can have segmented before the criterion runs there (processors run after criteria). Leave it, and add a one-line comment recording why it is already correct.

- [ ] **Step 4: Confirm it passes**

Run: `go test ./internal/domain/workflow/ -v`
Expected: PASS.

- [ ] **Step 5: Add the E2E case**

In `internal/e2e/`, find the existing joined-callback test (grep for `cyodatxtoken` or `newCallbackHarness` with a criteria callback) and add a sibling that: registers a CBD processor and a criteria callback which performs a joined entity write using the txID it received; drives a create that segments; and asserts the joined write landed rather than failing with a transaction-not-found error.

- [ ] **Step 6: Run the E2E suite**

Run: `go test ./internal/e2e/ -run Callback -v`
Expected: PASS. Requires Docker.

- [ ] **Step 7: Commit**

```bash
git add internal/domain/workflow/ internal/e2e/
git commit -m "fix(workflow): hand a post-segment criterion the open segment's txID

The cascade-entry txID names a committed transaction once a
COMMIT_BEFORE_DISPATCH segment has flushed, and it is what reaches the compute
node as its join token."
```

---

## Task 3: `txScope`

A value type that owns transaction lifecycle for the entity service's write flows. This task builds and unit-tests it; Task 4 converts the call sites.

**Files:**
- Create: `internal/domain/entity/txscope.go`
- Create: `internal/domain/entity/txscope_test.go`
- Modify: `internal/domain/entity/handler.go` (add `classifyBeginErr` / `storageUnavailable`; `rollbackOwned` stays until Task 4 removes its last caller)
- Modify: `internal/common/error_codes.go` (add `ErrCodeStorageUnavailable`)
- Create: `cmd/cyoda/help/content/errors/STORAGE_UNAVAILABLE.md`
- Modify: `cmd/cyoda/help/content/errors.md` (index topic)

**Interfaces:**
- Consumes: `common.RollbackContext` (Task 1).
- Produces, all used by Task 4:
  - `func (h *Handler) beginScope(ctx context.Context) (*txScope, error)`
  - `func (s *txScope) Ctx() context.Context`
  - `func (s *txScope) TxID() string`
  - `func (s *txScope) Owned() bool`
  - `func (s *txScope) Advance(ctx context.Context, txID string)`
  - `func (s *txScope) Commit() error`
  - `func (s *txScope) Release()`
  - `func classifyBeginErr(err error) *common.AppError`
  - `func storageUnavailable(err error) *common.AppError` — returns nil when `err` carries no storage-unavailable marker.

- [ ] **Step 1: Write the failing tests**

Create `internal/domain/entity/txscope_test.go`. These are coverage rows 8, 8a and 8b.

```go
package entity

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/txgate"
)

// scopeTxMgr records lifecycle calls and the context each one arrived on.
type scopeTxMgr struct {
	spi.TransactionManager
	mu         sync.Mutex
	rolledBack []string
	committed  []string
	rbCtxErr   []error
	onRollback func()
}

func (m *scopeTxMgr) Rollback(ctx context.Context, txID string) error {
	if m.onRollback != nil {
		m.onRollback()
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.rolledBack = append(m.rolledBack, txID)
	m.rbCtxErr = append(m.rbCtxErr, ctx.Err())
	return nil
}

func (m *scopeTxMgr) Commit(_ context.Context, txID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.committed = append(m.committed, txID)
	return nil
}

func newScopeHandler(m *scopeTxMgr) *Handler {
	return &Handler{txMgr: m, gate: txgate.New()}
}

// TestTxScope_OwnedRelease_RollsBack — the base case the 40 deleted rollbackOwned
// calls were doing by hand.
func TestTxScope_OwnedRelease_RollsBack(t *testing.T) {
	m := &scopeTxMgr{}
	s := &txScope{h: newScopeHandler(m), entryTxID: "tx-1", ctx: context.Background(), txID: "tx-1", owned: true}
	s.Release()
	if len(m.rolledBack) != 1 || m.rolledBack[0] != "tx-1" {
		t.Fatalf("owned scope did not roll back: %v", m.rolledBack)
	}
}

// TestTxScope_JoinedRelease_DoesNotRollBackOwnersTx is coverage row 3. A joined
// callback must never roll back the transaction its owner will commit.
func TestTxScope_JoinedRelease_DoesNotRollBackOwnersTx(t *testing.T) {
	m := &scopeTxMgr{}
	s := &txScope{h: newScopeHandler(m), entryTxID: "tx-owner", ctx: context.Background(), txID: "tx-owner", owned: false}
	s.Release()
	if len(m.rolledBack) != 0 {
		t.Fatalf("joined scope rolled back the owner's transaction: %v", m.rolledBack)
	}
}

// TestTxScope_JoinedRelease_RollsBackEngineOpenedSegment is coverage row 8b. A
// joined call that unexpectedly segments holds a transaction that is nobody
// else's — the engine opened it during this call. It is a can't-happen branch;
// fail-closed says handle it anyway.
func TestTxScope_JoinedRelease_RollsBackEngineOpenedSegment(t *testing.T) {
	m := &scopeTxMgr{}
	s := &txScope{h: newScopeHandler(m), entryTxID: "tx-owner", ctx: context.Background(), txID: "tx-owner", owned: false}
	s.Advance(context.Background(), "tx-post")
	s.Release()
	if len(m.rolledBack) != 1 || m.rolledBack[0] != "tx-post" {
		t.Fatalf("engine-opened segment leaked on a joined call: %v", m.rolledBack)
	}
}

// TestTxScope_ReleaseAfterCommit_IsNoOp — no path rolls back after a commit,
// successful or not, and aborting a commit another goroutine is running would
// trip memory's ErrTxCommitInProgress path.
func TestTxScope_ReleaseAfterCommit_IsNoOp(t *testing.T) {
	for _, name := range []string{"successful commit", "failed commit"} {
		t.Run(name, func(t *testing.T) {
			m := &scopeTxMgr{}
			s := &txScope{h: newScopeHandler(m), entryTxID: "tx-1", ctx: context.Background(), txID: "tx-1", owned: true}
			_ = s.Commit()
			s.Release()
			if len(m.rolledBack) != 0 {
				t.Fatalf("released after commit: %v", m.rolledBack)
			}
		})
	}
}

// TestTxScope_ReleaseOnCancelledContext_StillRollsBack is coverage row 8a. The
// UserContext verifyTenant reads must survive; the cancellation must not.
func TestTxScope_ReleaseOnCancelledContext_StillRollsBack(t *testing.T) {
	m := &scopeTxMgr{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	s := &txScope{h: newScopeHandler(m), entryTxID: "tx-1", ctx: ctx, txID: "tx-1", owned: true}
	s.Release()
	if len(m.rolledBack) != 1 {
		t.Fatalf("cancelled request abandoned its transaction: %v", m.rolledBack)
	}
	if m.rbCtxErr[0] != nil {
		t.Fatalf("rollback ran on a cancelled context: %v", m.rbCtxErr[0])
	}
}

// TestTxScope_Release_HoldsTheGate is coverage row 8. Ten of the rollbacks this
// replaces ran inside h.gate.Acquire today; the property that must survive is
// mutual exclusion on the underlying transaction handle.
func TestTxScope_Release_HoldsTheGate(t *testing.T) {
	h := newScopeHandler(nil)
	contended := make(chan struct{})
	var heldDuringRollback bool

	m := &scopeTxMgr{onRollback: func() {
		// A competing acquirer must not get in while the rollback runs.
		go func() {
			release := h.gate.Acquire("tx-1")
			release()
			close(contended)
		}()
		select {
		case <-contended:
			heldDuringRollback = false
		case <-time.After(100 * time.Millisecond):
			heldDuringRollback = true
		}
	}}
	h.txMgr = m

	s := &txScope{h: h, entryTxID: "tx-1", ctx: context.Background(), txID: "tx-1", owned: true}
	s.Release()
	<-contended

	if !heldDuringRollback {
		t.Fatal("Release rolled back without holding the per-tx gate")
	}
}

// TestClassifyBeginErr_StorageUnavailable — the plugin owns the acquire context,
// so it returns a marker rather than a bare context.DeadlineExceeded, which
// pool.BeginTx also returns when the CALLER's context expired.
func TestClassifyBeginErr_StorageUnavailable(t *testing.T) {
	appErr := classifyBeginErr(fmt.Errorf("Begin: %w", stubUnavailable{}))
	if appErr.Status != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", appErr.Status)
	}
	if appErr.Code != common.ErrCodeStorageUnavailable {
		t.Fatalf("code = %q, want %q", appErr.Code, common.ErrCodeStorageUnavailable)
	}
	if !appErr.Retryable {
		t.Fatal("pool exhaustion is transient contention; it must advertise as retryable")
	}
}

// TestClassifyBeginErr_OtherFailureStaysInternal is coverage row 11's unit half.
func TestClassifyBeginErr_OtherFailureStaysInternal(t *testing.T) {
	appErr := classifyBeginErr(errors.New("connection refused"))
	if appErr.Status != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", appErr.Status)
	}
}

type stubUnavailable struct{}

func (stubUnavailable) Error() string             { return "acquire timed out" }
func (stubUnavailable) StorageUnavailable() bool  { return true }
```

Add the `fmt`, `net/http` and `internal/common` imports the last three tests need.

- [ ] **Step 2: Run them and confirm they fail**

Run: `go test ./internal/domain/entity/ -run 'TestTxScope|TestClassifyBeginErr' -v`
Expected: FAIL — `undefined: txScope`, `undefined: classifyBeginErr`, `undefined: common.ErrCodeStorageUnavailable`.

Add the constant in `internal/common/error_codes.go` beside `ErrCodeConflict`:

```go
	// ErrCodeStorageUnavailable is returned when the storage layer cannot supply
	// a connection within its acquire deadline, or when an operation finds its
	// transaction aborted by the idle-in-transaction ceiling. Transient
	// contention — the same request may well succeed on a second attempt.
	ErrCodeStorageUnavailable = "STORAGE_UNAVAILABLE"
```

`TestErrCode_Parity` is a strict bijection, so the help topic lands in **this** task rather than in Task 8 — a task must not leave the suite red for a later task to fix. Create `cmd/cyoda/help/content/errors/STORAGE_UNAVAILABLE.md` following `CONFLICT.md`'s structure exactly:

```markdown
---
topic: errors.STORAGE_UNAVAILABLE
title: "STORAGE_UNAVAILABLE — storage could not serve the request in time"
stability: stable
see_also:
  - errors
  - errors.CONFLICT
  - config.database
---

# errors.STORAGE_UNAVAILABLE

## NAME

STORAGE_UNAVAILABLE — the storage layer could not supply a connection, or the transaction was reclaimed by the idle-in-transaction ceiling.

## SYNOPSIS

HTTP: `503` `Service Unavailable`. Retryable: `yes`.

## DESCRIPTION

Raised in two cases, both transient contention:

- The connection pool could not supply a connection within `CYODA_POSTGRES_ACQUIRE_TIMEOUT` (default `10s`). Writes fail fast here rather than queueing behind a saturated pool.
- An operation found its transaction already aborted because the connection sat idle inside it for longer than `CYODA_POSTGRES_IDLE_IN_TX_TIMEOUT` (default `5m`). The usual cause is a workflow processor whose `responseTimeoutMs` exceeds that ceiling.

Retryable. The same request may well succeed on a second attempt. Repeated occurrences mean the pool is undersized for the offered load, or a workflow holds transactions open across a callout longer than the ceiling allows.

See `cyoda help config.database` for the pool and ceiling settings.

## SEE ALSO

- errors
- errors.CONFLICT
- config.database
```

Add the code to the `cmd/cyoda/help/content/errors.md` index topic in its alphabetical position. The env vars this topic names arrive in Task 9; the topic is written against the settled values from Global Constraints, so it needs no later revision.

- [ ] **Step 3: Implement `txScope`**

Create `internal/domain/entity/txscope.go`:

```go
package entity

import (
	"context"
	"errors"
	"log/slog"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/common"
)

// txScope owns the lifecycle of the transaction an entity write flow runs in.
// One deferred Release replaces the per-error-branch rollback calls a panic
// unwound straight past, leaving the transaction neither committed nor rolled
// back and its pooled connection never returned.
//
// Usage:
//
//	scope, err := h.beginScope(ctx)
//	if err != nil {
//	    return nil, classifyBeginErr(err)
//	}
//	defer scope.Release()
//	...
//	scope.Advance(result.FinalCtx, result.FinalTxID) // first statement after every engine call
//	...
//	err := scope.Commit()
//
// beginScope deliberately does NOT touch the joined gate. Flows acquire it
// themselves and register `defer releaseGate()` AFTER `defer scope.Release()`,
// so LIFO frees the gate first. Folding the gate into the scope would leave it
// permanently held on the joined path, where Release is a no-op — and it is a
// non-reentrant mutex, so every later joined callback on that txID would block
// forever.
type txScope struct {
	h *Handler

	// entryTxID is the transaction beginScope returned. It never changes, and
	// distinguishing it from txID is what lets a joined call release a segment
	// the engine opened without releasing its owner's transaction.
	entryTxID string

	// ctx and txID name the segment currently open. Advance moves them when the
	// engine segments via COMMIT_BEFORE_DISPATCH; Release always targets these,
	// never entryTxID.
	ctx  context.Context
	txID string

	owned bool
	done  bool
}

// beginScope begins a transaction, or joins one already on ctx (a routed
// compute-node callback). It performs no gating — see the type comment.
func (h *Handler) beginScope(ctx context.Context) (*txScope, error) {
	txID, txCtx, owned, err := h.beginOrJoin(ctx)
	if err != nil {
		return nil, err
	}
	return &txScope{h: h, entryTxID: txID, ctx: txCtx, txID: txID, owned: owned}, nil
}

func (s *txScope) Ctx() context.Context { return s.ctx }
func (s *txScope) TxID() string         { return s.txID }
func (s *txScope) Owned() bool          { return s.owned }

// Advance moves the scope onto whichever segment the engine left open.
//
// It must be the FIRST statement after an engine call's `if err != nil` check —
// it cannot go before it, because the engine returns a nil EngineResult on every
// error path, so reading result.FinalCtx there would nil-dereference. The panic
// window between the call and the advance is therefore not closable here; the
// engine's own guard covers it, which is the correct place since the segment is
// the engine's until it is handed back.
func (s *txScope) Advance(ctx context.Context, txID string) {
	if ctx == nil || txID == "" {
		return
	}
	s.ctx, s.txID = ctx, txID
}

// Commit commits when this flow owns the transaction, and marks the scope done
// regardless of outcome. No path rolls back after a failed commit: the commit
// may be partially applied, and aborting one another goroutine is running trips
// memory's ErrTxCommitInProgress path.
func (s *txScope) Commit() error {
	s.done = true
	return s.h.commitOwned(s.ctx, s.txID, s.owned)
}

// Release rolls back the segment currently open, unless the scope is already
// done or the transaction belongs to somebody else.
//
// A joined callback never rolls back its owner's transaction — an error on the
// joined path surfaces to the owner, which decides its fate. The exception is a
// segment the engine opened during this call: that one is nobody else's, so it
// is released regardless of ownership whenever the scope has advanced past its
// entry txID.
func (s *txScope) Release() {
	if s.done {
		return
	}
	s.done = true
	if s.txID == "" {
		return
	}
	if !s.owned && s.txID == s.entryTxID {
		return
	}

	rbCtx, cancel := common.RollbackContext(s.ctx)
	defer cancel()

	// Acquire the per-tx gate so the rollback is mutually exclusive with any
	// joined callback's access to the same transaction handle. No self-deadlock:
	// every `defer h.gate.Acquire(...)()` site in this package is inside an IIFE,
	// so the gate is free by outer-defer time.
	//
	// What this does NOT preserve is failed-Save-then-rollback as one atomic
	// gated section: a joined callback can win the gate in the window between an
	// IIFE releasing it and Release re-acquiring, Save successfully, return 200
	// to its caller, and then have its write discarded by this rollback. That is
	// strictly better than the alternative it replaces — a leaked transaction —
	// and the joined caller's write was doomed either way once the owner failed.
	defer s.h.gate.Acquire(s.txID)()

	if err := s.h.txMgr.Rollback(rbCtx, s.txID); err != nil && !errors.Is(err, spi.ErrTxNotFound) {
		slog.Warn("failed to roll back transaction", "pkg", "entity", "txID", s.txID, "err", err)
	}
}
```

- [ ] **Step 4: Implement the Begin-error classifiers**

In `internal/domain/entity/handler.go`, beside `classifyValidateOrExtendErr`:

```go
// storageUnavailable returns a 503 AppError when err carries the storage
// layer's transient-unavailability marker, or nil when it does not.
//
// Matched with errors.As on an interface rather than a concrete type: every
// Begin error is already wrapped by the time a classifier sees it, and the
// marker is a plugin-side type this module must not import. A storage plugin
// opts in by returning an error whose chain satisfies the interface — no SPI
// change, so no coordinated cross-repo release.
func storageUnavailable(err error) *common.AppError {
	var su interface{ StorageUnavailable() bool }
	if errors.As(err, &su) && su.StorageUnavailable() {
		return common.Operational(
			http.StatusServiceUnavailable,
			common.ErrCodeStorageUnavailable,
			"storage is temporarily unavailable — retry",
		).AsRetryable()
	}
	return nil
}

// classifyBeginErr maps a transaction-Begin failure to a status code. It must
// run BEFORE common.Internal, which fixes the status at 500 — AppError.Unwrap
// means a later errors.As would still find the cause, but by then the status is
// already wrong.
func classifyBeginErr(err error) *common.AppError {
	if appErr := storageUnavailable(err); appErr != nil {
		return appErr
	}
	return common.Internal("failed to begin transaction", err)
}
```

- [ ] **Step 5: Confirm the scope tests pass**

Run: `go test ./internal/domain/entity/ -run 'TestTxScope|TestClassifyBeginErr' -v && go test ./cmd/cyoda/help/... -v`
Expected: PASS both. `TestErrCode_Parity` is a strict bijection and must be green when this task ends — the suite is never left red for a later task to fix.

- [ ] **Step 6: Commit**

```bash
git add internal/domain/entity/txscope.go internal/domain/entity/txscope_test.go \
        internal/domain/entity/handler.go internal/common/error_codes.go \
        cmd/cyoda/help/content/errors/
git commit -m "feat(entity): add txScope, the deferred transaction lifecycle owner"
```

---

## Task 4: Convert the eight write flows to `txScope`

Delete all 40 `rollbackOwned` call sites — replaced, not duplicated. This is the largest mechanical change in the plan and the one whose defects are least visible: the existing write suites assert response codes and never observe transaction state, so a single defect in `Release` would leave a transaction open on every error path with no test noticing. Coverage row 5a exists for exactly that.

**Files:**
- Modify: `internal/domain/entity/service.go` — `CreateEntity` (`:190`), `DeleteEntity` (`:643`), `DeleteAllEntities` (`:778`), `DeleteEntitiesConditional` (`:878`), `CreateEntityCollection` (`:1088`), `updateEntityCore` (`:1328`, reached by `UpdateEntity` and `PatchEntity`), `UpdateEntityCollection` (`:1655`)
- Modify: `internal/domain/entity/handler.go` — delete `rollbackOwned` (`:107-115`)
- Test: `internal/domain/entity/service_rollback_test.go` (new)
- Test: `internal/e2e/tx_lifecycle_e2e_test.go` (new)

**Interfaces:**
- Consumes: `beginScope`, `Advance`, `Commit`, `Release`, `classifyBeginErr` (Task 3); the engine guard's guarantee (Task 1).
- Produces: `func newTinyPoolHarness(t *testing.T, maxConns int32) *callbackHarness` in `internal/e2e/tx_lifecycle_e2e_test.go` — Tasks 10 and 11 reuse it.

- [ ] **Step 1: Write the failing per-flow rollback tests**

Create `internal/domain/entity/service_rollback_test.go`. Coverage row 5a: one case per converted flow, asserting the transaction is **gone**, not just that the status code was right.

```go
package entity

// TestFlows_ErrorPathsReleaseTheTransaction asserts that every converted flow
// leaves no open transaction behind on an ordinary (non-panic) error.
//
// This is the highest-value test in the change. The existing write suites assert
// response codes and never observe transaction state, so a defect in Release
// would leave a transaction open on every error path in this file with nothing
// noticing.
func TestFlows_ErrorPathsReleaseTheTransaction(t *testing.T) {
	cases := []struct {
		name string
		// drive provokes an error inside the flow after the transaction is open.
		drive func(t *testing.T, h *Handler, ctx context.Context) error
	}{
		{"CreateEntity", driveCreateFailure},
		{"DeleteEntity", driveDeleteFailure},
		{"DeleteAllEntities", driveDeleteAllFailure},
		{"DeleteEntitiesConditional", driveDeleteConditionalFailure},
		{"CreateEntityCollection", driveCreateCollectionFailure},
		{"updateEntityCore", driveUpdateFailure},
		{"UpdateEntityCollection", driveUpdateCollectionFailure},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, tracker := newTrackingHandler(t) // memory backend + a TransactionManager wrapper
			if err := tc.drive(t, h, testCtx(t)); err == nil {
				t.Fatal("expected the flow to fail")
			}
			if open := tracker.openTxIDs(); len(open) != 0 {
				t.Fatalf("%s left %d transaction(s) open on an error path: %v", tc.name, len(open), open)
			}
		})
	}
}

// TestFlows_PanicReleasesTheTransaction is coverage row 1's unit half: a panic
// between Begin and Commit must not leave the transaction open.
func TestFlows_PanicReleasesTheTransaction(t *testing.T) {
	h, tracker := newTrackingHandler(t)
	h.engine = panickingEngine(t) // criteria callback panics; see localproc note below

	func() {
		defer func() { _ = recover() }()
		_, _ = h.CreateEntity(testCtx(t), validCreateInput(t))
	}()

	if open := tracker.openTxIDs(); len(open) != 0 {
		t.Fatalf("panic leaked %d transaction(s): %v", len(open), open)
	}
}

// TestJoinedCallbackPanic_DoesNotRollBackOwner is coverage row 3's unit half.
func TestJoinedCallbackPanic_DoesNotRollBackOwner(t *testing.T) {
	h, tracker := newTrackingHandler(t)
	ownerTxID, ownerCtx := tracker.beginOwner(t)

	func() {
		defer func() { _ = recover() }()
		_, _ = h.CreateEntity(joinedCtx(ownerCtx), panickingCreateInput(t))
	}()

	if tracker.wasRolledBack(ownerTxID) {
		t.Fatal("a joined callback rolled back its owner's transaction")
	}
	if !tracker.isOpen(ownerTxID) {
		t.Fatal("owner's transaction is no longer open; the owner must decide its fate")
	}
}

// TestPanickingWrite_ReleasesBufferedState is coverage row 6. On memory and
// sqlite the harm is not a held connection — sqlite opens no *sql.Tx at all —
// but a leaked buffer plus a pinned committedLog prune floor, which makes every
// later commit's conflict scan slower without bound.
func TestPanickingWrite_ReleasesBufferedState(t *testing.T) {
	for _, backend := range []string{"memory", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
			h, tracker := newTrackingHandlerFor(t, backend)
			floorBefore := tracker.pruneFloor(t)

			func() {
				defer func() { _ = recover() }()
				_, _ = h.CreateEntity(testCtx(t), panickingCreateInput(t))
			}()

			if open := tracker.openTxIDs(); len(open) != 0 {
				t.Fatalf("leaked buffer for %v", open)
			}
			if got := tracker.pruneFloor(t); got.Before(floorBefore) {
				t.Fatal("committedLog prune floor pinned by an abandoned transaction")
			}
		})
	}
}
```

Write `newTrackingHandler` / `newTrackingHandlerFor` in the same file: build the real memory (or sqlite) factory, wrap its `spi.TransactionManager` in a recorder exposing `openTxIDs()`, `isOpen(id)`, `wasRolledBack(id)`, `beginOrJoin`-compatible `beginOwner`, and `pruneFloor(t)` (read via the plugin's test export; add one if absent). Build the `Handler` with `New(factory, tracker, uuids, engine, txgate.New(), nil)`.

Panic injection: `localproc.DispatchProcessor` and `DispatchFunction` recover panics and convert them to errors (`localproc.go:104-108`, `:149-153`); only `DispatchCriteria` (`:114-135`) has no recover. So a panicking **criteria** callback is the one that reaches the handler intact — use that. Where a test needs the panic on the processor side specifically, add an explicit opt-in `RegisterPanickingProcessor` to `internal/testing/localproc` that skips the recover; it is test-only code in a test-only package with nothing compiled into the binary.

- [ ] **Step 2: Run them and confirm they fail**

Run: `go test ./internal/domain/entity/ -run 'TestFlows_|TestJoinedCallbackPanic|TestPanickingWrite' -v`
Expected: FAIL — the panic cases leak transactions. Some error-path cases may already pass (the explicit `rollbackOwned` calls cover them today); those must stay passing.

- [ ] **Step 3: Convert `CreateEntity`**

This is the reference conversion; the other six follow the same shape. Replace `service.go:262-270`:

```go
	scope, err := h.beginScope(ctx)
	if err != nil {
		return nil, classifyBeginErr(err)
	}
	// Registered BEFORE the joined gate's release so LIFO frees the gate first —
	// Release re-acquires it, and it is not reentrant.
	defer scope.Release()

	txID, txCtx, owned := scope.TxID(), scope.Ctx(), scope.Owned()
	if !owned {
		var releaseGate func()
		txCtx, releaseGate = h.acquireJoinedGate(txCtx, txID)
		defer releaseGate()
	}
```

Replace the engine call at `:304-309`:

```go
	result, err := h.engine.Execute(txCtx, entity, "")
	if err != nil {
		slog.Error("workflow execution failed", "error", err.Error(), "entityId", entity.Meta.ID)
		return nil, classifyWorkflowError(err)
	}
	// FIRST statement after the error check. It cannot go before it: the engine
	// returns a nil EngineResult on every error path.
	scope.Advance(result.FinalCtx, result.FinalTxID)
```

Delete the dead guards at `:321` and `:325`'s neighbours. `result != nil` sits after the error check and the engine never returns `(nil, nil)`; `:325` already dereferences unguarded, so the guard is provably dead:

```go
	if result.StopReason == "" {
		entity.Meta.TransitionForLatestSave = "loopback"
	}

	finalCtx, finalTxID := scope.Ctx(), scope.TxID()
```

Leave the joined-segmented check at `:330-333` returning `common.Internal` as it does — the scope now rolls the orphaned segment back on the way out, which is the behaviour that was missing:

```go
	if !owned && finalTxID != txID {
		// Can't-happen: a participating call has no commit boundaries of its own.
		// The scope has advanced onto the engine-opened segment, so Release will
		// return it — that segment is nobody else's.
		return nil, common.Internal("joined callback unexpectedly segmented transaction",
			fmt.Errorf("entry txID %s advanced to %s on a joined call", txID, finalTxID))
	}
```

Delete `h.rollbackOwned(...)` at `:306`, `:341`, `:356`. Replace `h.commitOwned(finalCtx, finalTxID, owned)` at `:359` with `scope.Commit()`.

- [ ] **Step 4: Convert the remaining six flows**

Apply the same four edits to each. Sites, in file order:

| Flow | `beginOrJoin` | `rollbackOwned` calls to delete | `commitOwned` → `scope.Commit()` | `Advance` after |
|---|---|---|---|---|
| `DeleteEntity` | `:645` | `:657`, `:664`, `:680` | `:684` | — (no engine call) |
| `DeleteAllEntities` | `:785` | `:797`, `:807`, `:811`, `:822`, `:833` | `:837` | — |
| `DeleteEntitiesConditional` | `:909` | `:921`, `:925`, `:935`, `:944` | `:979` | — |
| `CreateEntityCollection` | `:1172` | `:1234`, `:1279`, `:1283` | `:1302` | `:1232` engine call |
| `updateEntityCore` | `:1363` | `:1376`, `:1382`, `:1389`, `:1398`, `:1404`, `:1413`, `:1418`, `:1428`, `:1473`, `:1490`, `:1517`, `:1548`, `:1556`, `:1567` | `:1576` | `:1471` and `:1488` engine calls |
| `UpdateEntityCollection` | `:1712` | `:1732`, `:1738`, `:1745`, `:1753`, `:1768`, `:1828`, `:1872`, `:1911` | `:1938` | `:1805`/`:1807` engine calls |

Loop flows (`CreateEntityCollection`, `UpdateEntityCollection`) keep their `currentCtx` / `currentTxID` locals for readability but must re-read them from the scope after each `Advance`, so the two never drift:

```go
		result, err := h.engine.Execute(currentCtx, entity, "")
		if err != nil {
			slog.Error("workflow execution failed", "error", err.Error(), "entityId", entity.Meta.ID, "itemIndex", i)
			return nil, classifyWorkflowError(fmt.Errorf("item %d: %w", i, err))
		}
		scope.Advance(result.FinalCtx, result.FinalTxID)
		currentCtx, currentTxID = scope.Ctx(), scope.TxID()
```

Delete the now-dead `if result != nil` guards at `:1248` and `:1256` the same way as in `CreateEntity`.

- [ ] **Step 5: Delete `rollbackOwned`**

`internal/domain/entity/handler.go:107-115`. `go build ./...` proves no caller remains.

Run: `go build ./... && grep -rn 'rollbackOwned' internal/ || echo "no callers remain"`
Expected: build succeeds; grep prints the "no callers" line.

- [ ] **Step 6: Confirm the unit tests pass**

Run: `go test ./internal/domain/entity/... -v`
Expected: PASS, including everything that passed at Step 2.

- [ ] **Step 7: Write the E2E tests and the tiny-pool harness**

Create `internal/e2e/tx_lifecycle_e2e_test.go`. Coverage rows 1, 2, 3, 4, 4a, 5a, 8a.

The shared E2E suite builds one `app.App` and one pool in `TestMain` with `CYODA_POSTGRES_MAX_CONNS=5` (`e2e_test.go:106`, `:156`). These scenarios need their own app with a deliberately tiny pool: run against the shared one they cannot isolate, and Task 10's saturation test would stall the rest of the suite for a full acquire timeout. Reuse `newCallbackHarnessConfigured` (`callback_harness_test.go:175`) rather than inventing a second harness.

```go
// newTinyPoolHarness builds a postgres-backed app with a pool small enough that
// a handful of leaked connections exhausts it, so "did the connection come back"
// is observable rather than inferred.
func newTinyPoolHarness(t *testing.T, maxConns int32) *callbackHarness {
	t.Helper()
	return newCallbackHarnessConfigured(t, func(cfg *app.Config) {
		t.Setenv("CYODA_POSTGRES_MAX_CONNS", strconv.Itoa(int(maxConns)))
		t.Setenv("CYODA_POSTGRES_MIN_CONNS", "0")
	})
}

// poolStat opens an independent pool against the same DSN and reads Stat().
//
// App.StoreFactory() is NOT a route to the pool: it returns the
// *modelcache.CachingStoreFactory wrapper, which holds the real factory in an
// unexported field and forwards only the spi.StoreFactory methods, so a type
// assertion to interface{ Pool() *pgxpool.Pool } fails.
func poolStat(t *testing.T) *pgxpool.Stat {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), os.Getenv("CYODA_POSTGRES_URL"))
	if err != nil {
		t.Fatalf("open observer pool: %v", err)
	}
	defer pool.Close()
	return pool.Stat()
}

// acquiredConns reports how many connections the server currently holds out of
// the pool, read from PostgreSQL rather than from the observer pool's own Stat
// (which only sees its own connections).
func acquiredConns(t *testing.T) int {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), os.Getenv("CYODA_POSTGRES_URL"))
	if err != nil {
		t.Fatalf("open observer pool: %v", err)
	}
	defer pool.Close()
	var n int
	err = pool.QueryRow(context.Background(),
		`SELECT count(*) FROM pg_stat_activity
		 WHERE datname = current_database()
		   AND pid <> pg_backend_pid()
		   AND state IN ('idle in transaction', 'active')`).Scan(&n)
	if err != nil {
		t.Fatalf("read activity: %v", err)
	}
	return n
}

// waitFor polls cond until it holds or the deadline passes. Fault tests assert
// consistency (the connection came back), not a precise interleave.
func waitFor(t *testing.T, within time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("condition not met within %v", within)
}
```

```go
// TestE2E_PanicInOwnedWrite_ReturnsConnection is coverage row 1.
func TestE2E_PanicInOwnedWrite_ReturnsConnection(t *testing.T) {
	h := newTinyPoolHarness(t, 3)
	h.RegisterCriteria("boom", func(...) (bool, string, error) { panic("injected") })
	// ...workflow whose transition criterion is FUNCTION "boom"...

	before := acquiredConns(t)
	resp := h.POST(t, "/api/entity/JSON/panicky/1", body)
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 with a ticket", resp.StatusCode)
	}
	waitFor(t, 5*time.Second, func() bool { return acquiredConns(t) == before })
}

// TestE2E_RepeatedPanics_NodeKeepsServing is coverage row 2. It asserts request
// handling continues — NOT that health is green. recovery.go:23 stores false on
// the health flag and nothing resets it, so the health endpoint reports 503 DOWN
// after the first recovered panic. That is the existing contract and is
// deliberate: a node that has panicked has unknown state.
func TestE2E_RepeatedPanics_NodeKeepsServing(t *testing.T) {
	h := newTinyPoolHarness(t, 3)
	// ...register the panicking criterion...
	for i := 0; i < 10; i++ { // well beyond pool size
		_ = h.POST(t, "/api/entity/JSON/panicky/1", body)
	}
	resp := h.POST(t, "/api/entity/JSON/healthy/1", body) // a model with no panicking criterion
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("node stopped serving after repeated panics: %d", resp.StatusCode)
	}
}

// TestE2E_PanicAfterSegmentation_RollsBackTXPost is coverage row 4's E2E half.
// The unit half lives in the workflow package; this proves it end-to-end on a
// real pool, where a leaked TX_post is observable as a connection that never
// comes back.
func TestE2E_PanicAfterSegmentation_RollsBackTXPost(t *testing.T) {
	h := newTinyPoolHarness(t, 3)
	// Workflow: a COMMIT_BEFORE_DISPATCH processor segments, then the next
	// automated transition's criteria callback panics.
	h.RegisterCriteria("boom", func(...) (bool, string, error) { panic("injected") })

	before := acquiredConns(t)
	resp := h.POST(t, "/api/entity/JSON/segmenting/1", body)
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 with a ticket", resp.StatusCode)
	}
	// TX_pre committed durably before the callout; TX_post must be gone.
	waitFor(t, 5*time.Second, func() bool { return acquiredConns(t) == before })
	assertEntityAtPreSegmentState(t, h) // the segment's work is visible, TX_post's is not
}

// TestE2E_CriterionCalloutFailsMidCascade_RollsBackTXPost is coverage row 4a's
// E2E half — the non-panic case, which is the one reachable in ordinary
// operation by a compute node being unavailable.
func TestE2E_CriterionCalloutFailsMidCascade_RollsBackTXPost(t *testing.T) {
	h := newTinyPoolHarness(t, 3)
	h.RegisterCriteria("gatekeeper", func(...) (bool, string, error) {
		return false, "", errors.New("compute node unavailable")
	})

	before := acquiredConns(t)
	resp := h.POST(t, "/api/entity/JSON/segmenting/1", body)
	if resp.StatusCode == http.StatusOK {
		t.Fatal("cascade reported success despite the criterion callout failing")
	}
	waitFor(t, 5*time.Second, func() bool { return acquiredConns(t) == before })
}

// TestE2E_ClientCancelledRequest_StillRollsBack is coverage row 8a's E2E half.
// WithoutCancel keeps the UserContext verifyTenant reads, so the rollback runs
// even though the request that opened the transaction is gone.
func TestE2E_ClientCancelledRequest_StillRollsBack(t *testing.T) {
	h := newTinyPoolHarness(t, 3)
	h.RegisterCriteria("slow", func(ctx context.Context, e *spi.Entity, c json.RawMessage) (bool, string, error) {
		time.Sleep(2 * time.Second) // outlive the client below
		return true, "", nil
	})

	before := acquiredConns(t)

	reqCtx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	req := h.NewRequest(t, reqCtx, http.MethodPost, "/api/entity/JSON/slowmodel/1", body)
	if _, err := http.DefaultClient.Do(req); err == nil {
		t.Fatal("expected the client to give up first")
	}

	waitFor(t, 10*time.Second, func() bool { return acquiredConns(t) == before })
}
```

- [ ] **Step 8: Run the E2E suite**

Run: `go test ./internal/e2e/ -run 'TestE2E_PanicInOwnedWrite|TestE2E_RepeatedPanics|TestE2E_ClientCancelled' -v`
Expected: PASS. Requires Docker.

- [ ] **Step 9: Run the full root suite for regressions**

Run: `go test ./... 2>&1 | tail -40`
Expected: PASS except `cmd/cyoda/help`'s `TestErrCode_Parity`, which stays red until Task 8.

- [ ] **Step 10: Commit**

```bash
git add internal/domain/entity/ internal/e2e/ internal/testing/localproc/
git commit -m "fix(entity): own transaction lifecycle with a deferred scope

Replaces 40 hand-written rollback calls, none of them deferred, that a panic
between Begin and Commit unwound straight past — leaving the transaction
neither committed nor rolled back and its pooled connection never returned."
```

---

## Task 5: Collection update isolates per item only when the conflict is isolable

`UpdateEntityCollection`'s per-item isolation treats **any** `spi.ErrConflict` from the engine as an If-Match precondition failure and `continue`s the loop. Two different failures produce that error, and only one of them is isolable:

| Source | When | Isolable? |
|---|---|---|
| `flushAndCommitSegment`'s `CompareAndSave` (`engine_processors.go:384`, reached from `:286` and `:315`) | **Before** TX_pre commits and before any dispatch fires | **Yes** — nothing durable happened; the item fails its precondition and the batch continues |
| `executeCommitBeforeDispatch`'s apply-result CAS (`engine_processors.go:354`) | **After** TX_pre committed and the external dispatch already fired | **No** — the segment is gone; the loop's cursor was never advanced, so every later item saves into a committed transaction and is silently lost behind a 200 |

**The original plan gated this on `engineResult.Segmented`, which cannot work.** Every failure path in `executeCommitBeforeDispatch` is `return nil, "", err`, and every engine entry point turns that into `return nil, err` — so `engineResult` is nil for *both* rows above. A condition reading `engineResult != nil && !engineResult.Segmented` is false in both cases and aborts the batch on a cleanly-isolable precondition failure, breaking the documented first-segment-flush contract and its shipped E2E test (`TestUpdateCollection_IfMatch_CBDStaleAbortsBeforeDispatch`).

The engine must distinguish the two shapes, because only the engine knows which side of the commit it was on. Mark the post-commit site with a sentinel, mirroring `ErrCommitBeforeDispatchInfra` (`engine_processors.go:20`), which already exists in that file for exactly this purpose — letting a handler classify an error the engine raised.

`errors.Join` keeps `errors.Is(err, spi.ErrConflict)` true, so `updateEntityCore`'s single-entity 412 mapping is unaffected. Only the collection loop's isolation branch consults the new sentinel.

**Files:**
- Modify: `internal/domain/workflow/engine_processors.go` — declare `ErrPostSegmentConflict`; join it at the apply-result CAS (`:354`)
- Modify: `internal/domain/entity/service.go` — `UpdateEntityCollection`'s engine-side isolation branch
- Test: `internal/domain/workflow/engine_segment_guard_test.go` (extend)
- Test: `internal/domain/entity/service_rollback_test.go` (extend)

**Interfaces:**
- Consumes: Task 1's engine guard, Task 4's `txScope` conversion of this flow.
- Produces: `var wfengine.ErrPostSegmentConflict error` — no later task consumes it.

- [ ] **Step 1: Write the failing tests**

Engine-side, in `internal/domain/workflow/engine_segment_guard_test.go`:

```go
// TestCBD_ApplyResultConflict_CarriesPostSegmentMarker: the apply-result CAS runs
// after TX_pre committed and after the dispatch fired, so a conflict there is not
// something a caller can isolate and retry past. The marker is how the caller
// tells it apart from a first-segment-flush precondition failure, which is.
func TestCBD_ApplyResultConflict_CarriesPostSegmentMarker(t *testing.T) {
	h := newSegmentGuardHarness(t, "memory")
	h.registerCBDProcessor("segmenter")
	h.failCompareAndSave(spi.ErrConflict) // the apply-result CAS, post-commit

	_, err := h.engine.Execute(h.begin(t))
	if !errors.Is(err, spi.ErrConflict) {
		t.Fatalf("lost the conflict sentinel: %v", err)
	}
	if !errors.Is(err, ErrPostSegmentConflict) {
		t.Fatalf("post-commit conflict not marked; a caller cannot tell it from an isolable one: %v", err)
	}
}

// TestCBD_FirstFlushConflict_HasNoPostSegmentMarker guards the other side. This
// one IS isolable and must stay that way.
func TestCBD_FirstFlushConflict_HasNoPostSegmentMarker(t *testing.T) {
	h := newSegmentGuardHarness(t, "memory")
	h.registerCBDProcessor("segmenter")
	h.failFirstFlushCAS(spi.ErrConflict) // flushAndCommitSegment, pre-commit

	_, err := h.engine.Execute(h.begin(t))
	if !errors.Is(err, spi.ErrConflict) {
		t.Fatalf("lost the conflict sentinel: %v", err)
	}
	if errors.Is(err, ErrPostSegmentConflict) {
		t.Fatal("pre-commit precondition failure wrongly marked post-segment; the batch would abort instead of isolating the item")
	}
}
```

Handler-side, in `internal/domain/entity/service_rollback_test.go` — the two from the original plan, retargeted:

```go
// TestUpdateCollection_PostSegmentConflict_AbortsBatch: a post-commit apply-result
// conflict leaves no segment to continue into, so isolating it would let every
// later item save into a transaction the engine already committed — losing them
// behind a 200.
func TestUpdateCollection_PostSegmentConflict_AbortsBatch(t *testing.T) { ... }

// TestUpdateCollection_FirstFlushConflict_StillIsolates: the precondition failed
// before TX_pre committed and before any dispatch fired. That item is cleanly
// isolable and the batch continues — without this, the fix could be "abort on
// every conflict", which would break per-item isolation entirely.
func TestUpdateCollection_FirstFlushConflict_StillIsolates(t *testing.T) { ... }
```

Both handler tests build a two-item batch: item 0 provokes the conflict, item 1 is an ordinary update that must be observably lost (abort case) or observably saved (isolate case).

- [ ] **Step 2: Run them and confirm they fail**

Run: `go test ./internal/domain/workflow/ -run 'TestCBD_.*Conflict' -v && go test ./internal/domain/entity/ -run TestUpdateCollection_ -v`
Expected: the two engine tests FAIL on `undefined: ErrPostSegmentConflict`; `TestUpdateCollection_PostSegmentConflict_AbortsBatch` FAILS reporting the batch succeeded while item 1 was written into a committed transaction; `TestUpdateCollection_FirstFlushConflict_StillIsolates` PASSES already and must keep passing.

- [ ] **Step 3: Declare and join the sentinel**

In `internal/domain/workflow/engine_processors.go`, beside `ErrCommitBeforeDispatchInfra`:

```go
// ErrPostSegmentConflict marks a CAS conflict raised AFTER a
// COMMIT_BEFORE_DISPATCH segment has committed and its external dispatch has
// fired — the apply-result CAS below. It is not a precondition failure a caller
// can isolate and skip past: the segment that would have carried the rest of the
// work is gone, and the caller's cascade cursor was never advanced.
//
// A conflict from the FIRST-segment flush is deliberately left unmarked. That one
// happens before any commit and before any dispatch, so it is cleanly isolable —
// which is the whole distinction this sentinel exists to draw.
//
// Joined, not wrapped, so errors.Is(err, spi.ErrConflict) stays true and the
// single-entity 412 mapping is unaffected.
var ErrPostSegmentConflict = errors.New("conflict after a committed segment")
```

At the apply-result CAS (`:354`):

```go
	if _, saveErr := es.CompareAndSave(newCtx, entity, tPre); saveErr != nil {
		return nil, "", errors.Join(ErrPostSegmentConflict, saveErr)
	}
```

- [ ] **Step 4: Gate the handler's isolation on it**

In `UpdateEntityCollection`'s `if engineErr != nil` block:

```go
		if engineErr != nil {
			// Per-item isolation applies only to a conflict the engine raised
			// BEFORE committing a segment — a first-segment-flush precondition
			// failure, which fires before any external dispatch and leaves
			// nothing durable behind. That item is cleanly isolable and the
			// batch continues.
			//
			// A conflict marked ErrPostSegmentConflict arrived after TX_pre
			// committed and the dispatch fired. There is no segment to continue
			// into, and the loop's cursor was never advanced — isolating it
			// would let every later item save into a committed transaction and
			// be lost behind a 200.
			if item.ifMatch != "" && errors.Is(engineErr, spi.ErrConflict) &&
				!errors.Is(engineErr, wfengine.ErrPostSegmentConflict) {
				slog.Info("collection update item precondition failed",
					"source", "engine", "entityId", updated.Meta.ID, "itemIndex", i)
				failed = append(failed, UpdateCollectionItemFailure{
					EntityID:  updated.Meta.ID,
					Code:      common.ErrCodeEntityModified,
					Message:   "entity has been modified since last read",
					ItemIndex: i,
				})
				continue
			}
			slog.Error("workflow execution failed", "error", engineErr.Error(), "entityId", updated.Meta.ID, "transition", item.transition)
			return nil, classifyWorkflowError(fmt.Errorf("item %d: %w", i, engineErr))
		}
```

Leave the handler-side `applyHandlerCAS` isolation further down untouched — it already gates on `segmented`, which is meaningful there because a result exists on that path.

- [ ] **Step 5: Confirm all four pass, plus the shipped contract**

Run: `go test ./internal/domain/workflow/... ./internal/domain/entity/... -v`
Expected: PASS.

Then the E2E test this fix must not regress:

Run: `go test ./internal/e2e/ -run TestUpdateCollection_IfMatch_CBDStaleAbortsBeforeDispatch -v`
Expected: PASS. It covers the first-segment-flush contract; a fix that aborts on every engine conflict breaks it.

- [ ] **Step 6: Full suite**

Run: `go test ./... -count=1`
Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add internal/domain/workflow/ internal/domain/entity/
git commit -m "fix(entity): abort a collection update only on a post-segment conflict

Two different failures surface as ErrConflict from the engine: a first-segment
flush precondition failure, which happens before any commit or dispatch and is
cleanly isolable, and an apply-result CAS conflict, which happens after TX_pre
committed and leaves no segment to continue into. Isolating the second let every
later item save into a committed transaction and vanish behind a 200. The engine
marks it, because only the engine knows which side of the commit it was on."
```

---

## Task 6: gRPC panic-recovery interceptor

A panic in a gRPC handler **kills the process**: `internal/grpc/server.go:69` installs auth and tx-route interceptors only, and grpc-go does not recover handler panics — there is no equivalent of net/http's per-connection recover. Several operations are genuinely gRPC-only (`internal/grpc/model.go:85`, `:119`, `:156`, `:186`, `:202`, `:240`), so this is not merely a second door onto HTTP-reachable code.

This must not land before Tasks 1 and 3–4. On its own it would convert a process crash — which PostgreSQL cleans up by killing every session — into a silent connection leak.

**Files:**
- Create: `internal/grpc/recovery.go`
- Create: `internal/grpc/recovery_test.go`
- Modify: `internal/grpc/server.go:68-71`
- Modify: `app/app.go` — pass the health flag into `internalgrpc.NewServer`

**Interfaces:**
- Consumes: Task 4's rollback guarantee.
- Produces:
  - `func UnaryRecoveryInterceptor(healthFlag *atomic.Bool) googlegrpc.UnaryServerInterceptor`
  - `func StreamRecoveryInterceptor(healthFlag *atomic.Bool) googlegrpc.StreamServerInterceptor`
  - `NewServer` gains a trailing `healthFlag *atomic.Bool` parameter.

- [ ] **Step 1: Write the failing test**

Create `internal/grpc/recovery_test.go`. Coverage row 7.

```go
package grpc

import (
	"context"
	"sync/atomic"
	"testing"

	googlegrpc "google.golang.org/grpc"
)

func TestUnaryRecoveryInterceptor_RecoversAndMarksUnhealthy(t *testing.T) {
	var health atomic.Bool
	health.Store(true)

	interceptor := UnaryRecoveryInterceptor(&health)
	handler := func(ctx context.Context, req any) (any, error) { panic("injected") }

	resp, err := interceptor(context.Background(), nil,
		&googlegrpc.UnaryServerInfo{FullMethod: "/cyoda.CloudEventsService/Test"}, handler)

	if err == nil {
		t.Fatal("panic was not converted to an error; the process would have died")
	}
	if resp != nil {
		t.Fatalf("resp = %v, want nil", resp)
	}
	if health.Load() {
		t.Fatal("health flag not marked; a node that has panicked has unknown state")
	}
	if strings.Contains(err.Error(), "injected") {
		t.Fatal("panic value leaked to the client")
	}
}

func TestStreamRecoveryInterceptor_RecoversAndMarksUnhealthy(t *testing.T) {
	var health atomic.Bool
	health.Store(true)

	interceptor := StreamRecoveryInterceptor(&health)
	handler := func(srv any, ss googlegrpc.ServerStream) error { panic("injected") }

	err := interceptor(nil, nil,
		&googlegrpc.StreamServerInfo{FullMethod: "/cyoda.CloudEventsService/Stream"}, handler)

	if err == nil {
		t.Fatal("panic was not converted to an error; the process would have died")
	}
	if health.Load() {
		t.Fatal("health flag not marked")
	}
}

// TestServer_HandlerPanic_ProcessSurvives drives a real gRPC round trip so the
// interceptor is proven to be WIRED, not merely to exist. Without it this whole
// test binary would die on the panic rather than report a failure.
func TestServer_HandlerPanic_ProcessSurvives(t *testing.T) {
	var health atomic.Bool
	health.Store(true)

	srv, addr := startTestServer(t, &health) // NewServer(..., &health) on a 127.0.0.1:0 listener
	defer srv.GracefulStop()

	client := newTestClient(t, addr)
	// A model operation whose handler is made to panic via an injected stub.
	_, err := client.PanickingOperation(context.Background(), req)
	if err == nil {
		t.Fatal("expected an error from the recovered panic")
	}
	if health.Load() {
		t.Fatal("health flag not marked through the real server path")
	}

	// The server is still answering: the panic did not take the process down.
	if _, err := client.HealthyOperation(context.Background(), req2); err != nil {
		t.Fatalf("server stopped serving after a recovered panic: %v", err)
	}
}
```

- [ ] **Step 2: Run them and confirm they fail**

Run: `go test ./internal/grpc/ -run Recovery -v`
Expected: FAIL — `undefined: UnaryRecoveryInterceptor`.

- [ ] **Step 3: Implement the interceptors**

Create `internal/grpc/recovery.go`:

```go
package grpc

import (
	"context"
	"fmt"
	"log/slog"
	"runtime/debug"
	"sync/atomic"

	googlegrpc "google.golang.org/grpc"

	"github.com/cyoda-platform/cyoda-go/internal/common"
)

// UnaryRecoveryInterceptor converts a handler panic into an error instead of
// letting it kill the process. grpc-go does not recover handler panics — there
// is no equivalent of net/http's per-connection recover — so without this a
// single panic on any gRPC-only operation takes the node down.
//
// Mirrors internal/api/middleware/recovery.go: log with stack, mark the health
// flag, return a generic internal error carrying a ticket UUID. Marking health
// means the first recovered panic on any door takes the node to 503 DOWN
// permanently, which under a liveness probe is a restart. That is the existing
// HTTP contract, deliberately extended: a node that has panicked has unknown
// state, and restarting it is the correct response.
func UnaryRecoveryInterceptor(healthFlag *atomic.Bool) googlegrpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *googlegrpc.UnaryServerInfo, handler googlegrpc.UnaryHandler) (resp any, err error) {
		defer func() {
			if rec := recover(); rec != nil {
				resp = nil
				err = recoverPanic(rec, info.FullMethod, healthFlag)
			}
		}()
		return handler(ctx, req)
	}
}

// StreamRecoveryInterceptor is UnaryRecoveryInterceptor for streaming methods.
func StreamRecoveryInterceptor(healthFlag *atomic.Bool) googlegrpc.StreamServerInterceptor {
	return func(srv any, ss googlegrpc.ServerStream, info *googlegrpc.StreamServerInfo, handler googlegrpc.StreamHandler) (err error) {
		defer func() {
			if rec := recover(); rec != nil {
				err = recoverPanic(rec, info.FullMethod, healthFlag)
			}
		}()
		return handler(srv, ss)
	}
}

func recoverPanic(rec any, method string, healthFlag *atomic.Bool) error {
	panicErr := fmt.Errorf("panic: %v", rec)
	slog.Error("panic recovered", "pkg", "grpc", "method", method,
		"err", panicErr, "stack", string(debug.Stack()))
	if healthFlag != nil {
		healthFlag.Store(false)
	}
	// Generic message plus a ticket UUID; the panic value stays in the log.
	appErr := common.Fatal("internal server error", panicErr)
	appErr.Detail = "panic recovered; check server logs for details"
	return appErr
}
```

- [ ] **Step 4: Wire them, outermost**

`internal/grpc/server.go`, add `healthFlag *atomic.Bool` as the final `NewServer` parameter and chain recovery first so it covers the auth and tx-route interceptors too:

```go
	// Recovery runs first so it also covers a panic inside auth or tx-routing.
	opts = append(opts,
		googlegrpc.ChainUnaryInterceptor(
			UnaryRecoveryInterceptor(healthFlag),
			UnaryAuthInterceptor(authSvc),
			txRoute.unary(),
		),
		googlegrpc.ChainStreamInterceptor(
			StreamRecoveryInterceptor(healthFlag),
			StreamAuthInterceptor(authSvc),
			txRoute.stream(),
		),
	)
```

Update the call site in `app/app.go` to pass the same `healthFlag` the HTTP `Recovery` middleware uses, and update any test constructors that call `NewServer`.

- [ ] **Step 5: Confirm they pass**

Run: `go test ./internal/grpc/... -v`
Expected: PASS.

- [ ] **Step 6: Verify the tx rollback half of coverage row 7**

Extend `TestServer_HandlerPanic_ProcessSurvives` (or add an E2E in `internal/e2e/`) to drive a gRPC entity write whose criteria callback panics, and assert the envelope reports `Success=false` and no transaction is left open.

Run: `go test ./internal/grpc/... ./internal/e2e/ -run 'Panic' -v`
Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add internal/grpc/ app/app.go
git commit -m "fix(grpc): recover handler panics instead of killing the process

grpc-go does not recover handler panics. Several operations are gRPC-only, so
this is not a second door onto HTTP-reachable code."
```

---

## Task 7: Recover the remaining doors — HTTP mux and the async-search goroutine

`middleware.Recovery` wraps only the `/` catch-all (`app/app.go:729`), and **every** pattern more specific than `/` wins over it. That is not a short list: the peer scheduler RPC (`:747`, `:760`) and cluster dispatch (`:746`, `:759`), entity-transition and grouped-stats routes (`:671`, `:672`, `:718`), the four admin log-level and trace-sampler routes (`:663-666`), health (`:641`), `/.well-known/` and `POST /oauth/token` (`:657-658`), and the discovery and help routes (`:735`, `:739`, `:756`, `:760`).

A panic on these does not take the node down — `net/http` recovers handler panics itself (`server.go:1904`) — but it loses the project's own contract: no ProblemDetail 500 with a ticket, no `healthFlag` marking, and a connection dropped mid-response instead of an error body. The scheduler RPC matters most: it opens a transaction and runs a full fire plus cascade including compute-node callouts (`internal/cluster/scheduler_rpc.go:273` → `fire_scheduled.go:112`), reachable by any peer.

Enumerating doors is the wrong shape of fix — a route added later would silently join the list. Wrap `a.handler` once, after the mux is assembled.

`internal/domain/search/service.go:433` runs the async search job unrecovered, so a panic there *does* take the process down — same class as the gRPC door, and inconsistent with the scheduler's own dispatch goroutine, which recovers (`internal/scheduler/service.go:189-194`).

**Files:**
- Modify: `app/app.go:729` (delete the `/`-specific wrap) and the block after `a.handler` is assigned (`:722-762`)
- Modify: `internal/domain/search/service.go:433`
- Test: `internal/e2e/tx_lifecycle_e2e_test.go` (extend)
- Test: `internal/domain/search/service_test.go` (extend)

**Interfaces:**
- Consumes: Tasks 1, 3–4.
- Produces: no new exported symbols.

- [ ] **Step 1: Write the failing tests**

Coverage row 7a plus the search-goroutine case.

```go
// TestE2E_SchedulerRPCPanic_RecoveredAndRolledBack is coverage row 7a. The peer
// scheduler RPC opens a transaction and runs a full fire plus cascade including
// compute-node callouts, and is reachable by any peer — but it is registered at
// a pattern more specific than "/", so today it escapes middleware.Recovery.
func TestE2E_SchedulerRPCPanic_RecoveredAndRolledBack(t *testing.T) {
	h := newTinyPoolHarness(t, 3)
	// ...arm a scheduled transition whose criterion callback panics...
	resp := h.postSchedulerRPC(t, firePayload)

	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d, want a ProblemDetail 500 with a ticket", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "application/problem+json") {
		t.Fatalf("content-type = %q; the connection was dropped instead of an error body", ct)
	}
	waitFor(t, 5*time.Second, func() bool { return acquiredConns(t) == 0 })
}

// TestAsyncSearchJob_PanicIsRecovered: the job goroutine runs on
// context.Background() with no recover, so a panic there takes the process down.
func TestAsyncSearchJob_PanicIsRecovered(t *testing.T) {
	svc := newSearchServiceWithPanickingSearch(t)
	jobID, err := svc.StartAsync(testCtx(t), ref, cond, opts)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	waitFor(t, 5*time.Second, func() bool {
		job, _ := svc.GetJob(testCtx(t), jobID)
		return job.Status == "FAILED"
	})
	// If the panic were unrecovered the test binary would already be gone.
}
```

- [ ] **Step 2: Run them and confirm they fail**

Run: `go test ./internal/e2e/ -run SchedulerRPCPanic -v; go test ./internal/domain/search/ -run AsyncSearchJob_Panic -v`
Expected: FAIL — the scheduler-RPC case gets a dropped connection rather than a ProblemDetail; the search case crashes the test binary with an unrecovered panic.

- [ ] **Step 3: Wrap the assembled handler once**

In `app/app.go`, delete the `middleware.Recovery(healthFlag)` wrap at `:729`, leaving:

```go
	mux.Handle("/", middleware.Auth(a.authService)(txJoinMW(apiHandler)))
```

Then, at the point where `a.handler` has been assigned in both the context-path and no-context-path branches — and **before** the cluster routing middleware is applied as the outermost layer — wrap once:

```go
	// Recovery wraps the fully assembled mux rather than the "/" catch-all.
	// Every pattern more specific than "/" wins over it, which silently excluded
	// the peer scheduler RPC, cluster dispatch, health, discovery, help, the
	// admin log-level routes and more — and would have excluded any route added
	// later. One call site instead of a dozen, with no way to escape it.
	a.handler = middleware.Recovery(healthFlag)(a.handler)
```

Confirm ordering against the comment at `:762` ("Cluster routing middleware — outermost layer, before auth and recovery"): recovery must sit inside cluster routing, matching what that comment already asserts.

- [ ] **Step 4: Recover the async-search goroutine**

`internal/domain/search/service.go:433`:

```go
	go func() {
		defer func() {
			if rec := recover(); rec != nil {
				slog.Error("panic recovered in async search job", "pkg", "search",
					"jobID", jobID, "err", fmt.Errorf("panic: %v", rec),
					"stack", string(debug.Stack()))
				// The job is the caller's own work — record the failure without
				// revealing internals (the panic value stays in the log).
				_ = s.searchStore.UpdateJobStatus(bgCtx, jobID, "FAILED", 0,
					"search failed unexpectedly", time.Now(), 0)
			}
		}()
		start := time.Now()
		...
	}()
```

Mirror `internal/scheduler/service.go:189-194`, which already does this for the scheduler's dispatch goroutine.

- [ ] **Step 5: Confirm they pass**

Run: `go test ./internal/domain/search/... -v && go test ./internal/e2e/ -run 'Panic' -v`
Expected: PASS.

- [ ] **Step 6: Confirm no route regressions**

Run: `go test ./internal/e2e/... 2>&1 | tail -20 && go test ./app/... ./internal/api/... -v`
Expected: PASS. Watch specifically for discovery, help, health and OAuth-token routes — they now pass through `Recovery`, which must not alter their responses.

- [ ] **Step 7: Commit**

```bash
git add app/app.go internal/domain/search/service.go internal/e2e/
git commit -m "fix(http): apply panic recovery to every route, not just the catch-all

Every pattern more specific than \"/\" won over the Recovery wrap, silently
excluding the peer scheduler RPC, cluster dispatch, health, discovery and help.
The async-search goroutine, which runs unrecovered, gets the same treatment."
```

---

## Task 8: Declare the 503 in OpenAPI

`503` becomes newly *reachable* on the entity write operations. The E2E validator runs `ValidateResponse` with `IncludeResponseStatus=true` (`internal/e2e/openapivalidator/validator.go:168`), so an undeclared 503 fails conformance.

No status code changes to any existing endpoint. No existing entry is modified; the added 503 is the only change to any operation's response set.

**Files:**
- Modify: `api/openapi.yaml` — nine operations
- Test: `internal/e2e/` conformance (already automatic)

**Interfaces:**
- Consumes: `common.ErrCodeStorageUnavailable` and its help topic (Task 3).
- Produces: the declared 503 that Tasks 10–11 rely on for conformance.

- [ ] **Step 1: Confirm the current state fails conformance for an undeclared 503**

Write a temporary conformance probe, or reason from `validator.go:168` and record it: with `IncludeResponseStatus=true`, a response whose status is absent from the operation's declared set fails validation. Tasks 10 and 11 return 503 from these operations, so without this task their E2E tests fail conformance rather than passing.

Run: `go test ./internal/e2e/ -run Conformance -v`
Expected: PASS today (nothing returns 503 yet). This task is what keeps it passing once Tasks 10–11 land — note that in the commit message rather than manufacturing a red state.

- [ ] **Step 2: Declare 503 on the nine write operations**

Add to each operation's `responses:` block in `api/openapi.yaml`, immediately before `default:`. Use the shape already present at `:1648`:

```yaml
        "503":
          description: Storage could not supply a connection within the acquire
            timeout, or the transaction was reclaimed by the idle-in-transaction
            ceiling. Retryable.
          content:
            application/problem+json:
              schema:
                $ref: "#/components/schemas/ProblemDetail"
              example:
                type: about:blank
                title: Service Unavailable
                status: 503
                detail: storage is temporarily unavailable — retry
                instance: /api/entity/JSON/order/1
```

| Operation | Method + path | Declared today |
|---|---|---|
| `create` | POST `/entity/{format}/{entityName}/{modelVersion}` | 200, 400, 401, 403, 404, 409, 422 |
| `createCollection` | POST `/entity/{format}` | 200, 400, 401, 403, 404, 409, 422 |
| `updateSingle` | PUT `/entity/{format}/{entityId}/{transition}` | 200, 400, 401, 403, 404, 409, 412, 422 |
| `updateSingleWithLoopback` | PUT `/entity/{format}/{entityId}` | 200, 400, 401, 403, 404, 409, 412, 422 |
| `updateCollection` | PUT `/entity/{format}` | 200, 400, 401, 403, 404, 409, 422 |
| `patchSingle` | PATCH `/entity/{format}/{entityId}/{transition}` | 200, 400, 401, 403, 404, 409, 412, 415, 422, 428, 501 |
| `patchSingleWithLoopback` | PATCH `/entity/{format}/{entityId}` | 200, 400, 401, 403, 404, 409, 412, 415, 422, 428, 501 |
| `deleteSingleEntity` | DELETE `/entity/{entityId}` | 200, 400, 401, 403, 404 |
| `deleteEntities` | DELETE `/entity/{entityName}/{modelVersion}` | 200, 400, 401, 403, 404 |

Every one also carries `default: InternalServerError` (500), unchanged.

Do **not** touch `fetchEntityTransitions`, which lacks the 503 its documented alias `getEntityTransitions` declares (`api/openapi.yaml:1584`, `transitions_handler.go:117`). That is pre-existing drift, unrelated to this change's mechanism, and is recorded in the spec's out-of-scope list so it is not mistaken for something introduced here.

- [ ] **Step 3: Regenerate and verify the spec still loads**

Run: `go generate ./api/... 2>/dev/null; go build ./... && go test ./internal/e2e/ -run Conformance -v`
Expected: PASS. The spec is embedded via `//go:embed` (`embedded-spec: false`), so no codegen change is expected from adding response entries — confirm `git status` shows only `api/openapi.yaml` and the help content.

- [ ] **Step 4: Commit**

```bash
git add api/openapi.yaml
git commit -m "feat(api): declare 503 STORAGE_UNAVAILABLE on the entity write operations

Reachable from the acquire timeout and the idle-in-transaction ceiling. The
e2e validator runs ValidateResponse with IncludeResponseStatus=true, so an
undeclared 503 fails conformance the moment those paths can return one."
```

---

## Task 9: PostgreSQL GUC ceilings

| Var | Default | Limits | Mechanism |
|---|---|---|---|
| `CYODA_POSTGRES_STATEMENT_TIMEOUT` | `5m` | how long one SQL statement may run | server GUC |
| `CYODA_POSTGRES_IDLE_IN_TX_TIMEOUT` | `5m` | how long a connection may sit inside an open transaction doing nothing | server GUC |

Set via `ConnConfig.RuntimeParams` at connect time — no `AfterConnect` round-trip. `0` disables either, matching PostgreSQL's own convention.

The idle limit is what plugs the leak: an abandoned transaction is idle by definition. It must clear the longest legitimate idle gap, which is a compute-node callout — `responseTimeoutMs` defaults to 30s (`internal/grpc/dispatch.go:32`), so 5m clears it tenfold. All cluster timeouts sit well under it (`CYODA_TX_TOKEN_TTL` 90s, proxy and dispatch-forward 30s).

**Files:**
- Create: `plugins/postgres/ceilings.go`
- Create: `plugins/postgres/ceilings_test.go`
- Modify: `plugins/postgres/config.go` (`config` struct `:17-24`, `parseConfig` `:29-46`, `newPool` `:130-148`)
- Modify: `plugins/postgres/plugin.go` `ConfigVars()` `:16-25`
- Modify: `cmd/cyoda/help/content/config/database.md`
- Test: `plugins/postgres/ceilings_e2e_test.go` (live server, for row 11b)

**Interfaces:**
- Consumes: nothing.
- Produces, used by Tasks 10, 12, 13:
  - `func pgDurationMillis(d time.Duration) string`
  - `func envCeiling(getenv func(string) string, key string, dflt time.Duration) (time.Duration, bool, error)` — returns (value, explicitly-set, error)
  - `func applyCeiling(params map[string]string, name string, d time.Duration, explicit bool)`
  - `config` gains `StatementTimeout time.Duration` + `StatementTimeoutSet bool`, `IdleInTxTimeout time.Duration` + `IdleInTxTimeoutSet bool`, and `AcquireTimeout`, `MigrateLockTimeout`, `SearchStatementTimeout time.Duration`. Only the two GUCs written into `RuntimeParams` need the explicit-set flag; the other three have no DSN channel to defer to.

- [ ] **Step 1: Write the failing unit tests**

Create `plugins/postgres/ceilings_test.go`. Coverage rows 11a, 11g, 11h.

```go
package postgres

import (
	"strings"
	"testing"
	"time"
)

// TestPgDurationMillis is coverage row 11a. These values go into the startup
// packet, so a malformed one fails pool.Ping at boot for every deployment.
// PostgreSQL's time units are us/ms/s/min/h/d — "m" is NOT among them — and
// Go's (5*time.Minute).String() is "5m0s", which is also invalid. Bare integer
// milliseconds is the default unit for all three GUCs this renders.
func TestPgDurationMillis(t *testing.T) {
	cases := []struct {
		in   time.Duration
		want string
	}{
		{5 * time.Minute, "300000"},
		{30 * time.Minute, "1800000"},
		{10 * time.Second, "10000"},
		{time.Millisecond, "1"},
		{0, "0"}, // PostgreSQL's own convention for "disabled"
	}
	for _, tc := range cases {
		if got := pgDurationMillis(tc.in); got != tc.want {
			t.Errorf("pgDurationMillis(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestPgDurationMillis_NeverEmitsGoDurationSyntax(t *testing.T) {
	got := pgDurationMillis(5 * time.Minute)
	for _, bad := range []string{"m", "s", "h", "5m0s"} {
		if strings.Contains(got, bad) {
			t.Fatalf("rendered %q contains %q; PostgreSQL rejects it in the startup packet", got, bad)
		}
	}
}

// TestEnvCeiling_RejectsSubMillisecond is coverage row 11g. A value in (0, 1ms)
// truncates to "0", which PostgreSQL reads as DISABLED — the exact inversion of
// intent, so it is rejected rather than silently removing a ceiling.
func TestEnvCeiling_RejectsSubMillisecond(t *testing.T) {
	env := func(string) string { return "500us" }
	if _, _, err := envCeiling(env, "CYODA_POSTGRES_STATEMENT_TIMEOUT", 5*time.Minute); err == nil {
		t.Fatal("sub-millisecond ceiling accepted; it would truncate to 0 and disable the limit")
	}
}

func TestEnvCeiling_ZeroDisablesExplicitly(t *testing.T) {
	env := func(string) string { return "0" }
	d, set, err := envCeiling(env, "CYODA_POSTGRES_STATEMENT_TIMEOUT", 5*time.Minute)
	if err != nil {
		t.Fatalf("0 must be accepted as an explicit disable: %v", err)
	}
	if d != 0 || !set {
		t.Fatalf("got (%v, %v), want (0, true)", d, set)
	}
}

func TestEnvCeiling_UnsetReportsNotExplicit(t *testing.T) {
	d, set, err := envCeiling(func(string) string { return "" }, "X", 5*time.Minute)
	if err != nil || d != 5*time.Minute || set {
		t.Fatalf("got (%v, %v, %v), want (5m, false, nil)", d, set, err)
	}
}

func TestEnvCeiling_RejectsMalformed(t *testing.T) {
	if _, _, err := envCeiling(func(string) string { return "banana" }, "X", time.Minute); err == nil {
		t.Fatal("malformed duration accepted")
	}
}

// TestApplyCeilings_Precedence is coverage row 11h.
func TestApplyCeilings_Precedence(t *testing.T) {
	t.Run("neither set — the documented default applies", func(t *testing.T) {
		params := map[string]string{}
		applyCeiling(params, "statement_timeout", 5*time.Minute, false)
		if params["statement_timeout"] != "300000" {
			t.Fatalf("default not applied: %v", params)
		}
	})
	t.Run("DSN only — the operator's value survives", func(t *testing.T) {
		params := map[string]string{"statement_timeout": "90000"}
		applyCeiling(params, "statement_timeout", 5*time.Minute, false)
		if params["statement_timeout"] != "90000" {
			t.Fatalf("a default the operator never set overrode their DSN value: %v", params)
		}
	})
	t.Run("both set — the env var wins", func(t *testing.T) {
		params := map[string]string{"statement_timeout": "90000"}
		applyCeiling(params, "statement_timeout", 2*time.Minute, true)
		if params["statement_timeout"] != "120000" {
			t.Fatalf("explicit env var did not win: %v", params)
		}
	})
}
```

The "both set" case must also log at WARN. Assert that with an `slog` handler captured into a buffer.

- [ ] **Step 2: Run them and confirm they fail**

Run: `cd plugins/postgres && go test ./... -run 'TestPgDuration|TestEnvCeiling|TestApplyCeilings' -v`
Expected: FAIL — `undefined: pgDurationMillis`.

- [ ] **Step 3: Implement `ceilings.go`**

```go
package postgres

import (
	"fmt"
	"log/slog"
	"strconv"
	"time"
)

// pgDurationMillis renders a Go duration in the form PostgreSQL's
// statement_timeout / idle_in_transaction_session_timeout / lock_timeout GUCs
// accept in the startup packet: a bare integer count of milliseconds, which is
// their default unit.
//
// Nothing may pass a Go duration through verbatim. PostgreSQL's units are
// us/ms/s/min/h/d — "m" is not among them — and Go renders five minutes as
// "5m0s", which is invalid twice over. A malformed value here fails pool.Ping at
// boot for every deployment, so a test asserts the rendered form.
func pgDurationMillis(d time.Duration) string {
	return strconv.FormatInt(d.Milliseconds(), 10)
}

// envCeiling parses a PostgreSQL-ceiling env var. It reports whether the
// operator set it explicitly, which applyCeiling needs in order to decide
// against a value supplied in the DSN.
func envCeiling(getenv func(string) string, key string, dflt time.Duration) (time.Duration, bool, error) {
	v := getenv(key)
	if v == "" {
		return dflt, false, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, false, fmt.Errorf("%s=%q is not a valid duration: %w", key, v, err)
	}
	if d < 0 {
		return 0, false, fmt.Errorf("%s=%q must not be negative", key, v)
	}
	if d > 0 && d < time.Millisecond {
		// Truncating to "0" would tell PostgreSQL "no limit" — the exact
		// inversion of the operator's intent — so this is rejected rather than
		// silently removing a ceiling.
		return 0, false, fmt.Errorf(
			"%s=%q is below the 1ms resolution of the PostgreSQL setting it configures; "+
				"use 0 to disable the limit explicitly, or a value of at least 1ms", key, v)
	}
	return d, true, nil
}

// applyCeiling writes a ceiling into the pool's startup RuntimeParams.
//
// pgxpool.ParseConfig folds unrecognised DSN keys into RuntimeParams, so a value
// the operator set in CYODA_POSTGRES_URL is already present here. Since these
// settings now have non-zero defaults, writing unconditionally would let a
// default nobody set override a value somebody did. So: an explicitly set env
// var always wins (and says so when it is overriding), a DSN-only value is left
// alone, and the default applies when neither is present.
func applyCeiling(params map[string]string, name string, d time.Duration, explicit bool) {
	dsnValue, inDSN := params[name]
	if inDSN && !explicit {
		return
	}
	if inDSN && explicit {
		slog.Warn("overriding a PostgreSQL setting supplied in CYODA_POSTGRES_URL with the environment variable",
			"pkg", "postgres", "setting", name, "dsnValue", dsnValue, "envValue", pgDurationMillis(d))
	}
	params[name] = pgDurationMillis(d)
}
```

- [ ] **Step 4: Extend `config` and `parseConfig`**

`plugins/postgres/config.go`:

```go
type config struct {
	URL                     string
	MaxConns                int32
	MinConns                int32
	MaxConnIdleTime         time.Duration
	AutoMigrate             bool
	SchemaSavepointInterval int

	// StatementTimeout caps a single SQL statement. IdleInTxTimeout caps how
	// long a connection may sit inside an open transaction doing nothing — the
	// one that plugs an abandoned transaction, which is idle by definition. Each
	// *Set field records whether the operator set the var explicitly; see
	// applyCeiling.
	StatementTimeout    time.Duration
	StatementTimeoutSet bool
	IdleInTxTimeout     time.Duration
	IdleInTxTimeoutSet  bool

	// AcquireTimeout bounds the wait for a free pooled connection. It is a
	// Go-side deadline rather than a fourth GUC because pgxpool.Config has no
	// AcquireTimeout field.
	AcquireTimeout time.Duration

	// MigrateLockTimeout bounds a migration's lock waits; SearchStatementTimeout
	// is the async-search path's own statement ceiling.
	MigrateLockTimeout     time.Duration
	SearchStatementTimeout time.Duration
}
```

In `parseConfig`, after the existing fields:

```go
	var err error
	if cfg.StatementTimeout, cfg.StatementTimeoutSet, err = envCeiling(getenv, "CYODA_POSTGRES_STATEMENT_TIMEOUT", 5*time.Minute); err != nil {
		return config{}, err
	}
	if cfg.IdleInTxTimeout, cfg.IdleInTxTimeoutSet, err = envCeiling(getenv, "CYODA_POSTGRES_IDLE_IN_TX_TIMEOUT", 5*time.Minute); err != nil {
		return config{}, err
	}
	if cfg.AcquireTimeout, _, err = envCeiling(getenv, "CYODA_POSTGRES_ACQUIRE_TIMEOUT", 10*time.Second); err != nil {
		return config{}, err
	}
	if cfg.MigrateLockTimeout, _, err = envCeiling(getenv, "CYODA_POSTGRES_MIGRATE_LOCK_TIMEOUT", 5*time.Minute); err != nil {
		return config{}, err
	}
	if cfg.SearchStatementTimeout, _, err = envCeiling(getenv, "CYODA_POSTGRES_SEARCH_STATEMENT_TIMEOUT", 30*time.Minute); err != nil {
		return config{}, err
	}
```

Note this is the first error return `parseConfig` produces for a malformed non-URL var; existing helpers (`envDuration`, `envInt`) silently fall back to the default. Do not change those — the ceilings differ because a silently-defaulted ceiling is a silently-removed safety limit.

In `newPool`, after `poolCfg.MaxConnIdleTime = cfg.MaxConnIdleTime`:

```go
	if poolCfg.ConnConfig.RuntimeParams == nil {
		poolCfg.ConnConfig.RuntimeParams = map[string]string{}
	}
	applyCeiling(poolCfg.ConnConfig.RuntimeParams, "statement_timeout", cfg.StatementTimeout, cfg.StatementTimeoutSet)
	applyCeiling(poolCfg.ConnConfig.RuntimeParams, "idle_in_transaction_session_timeout", cfg.IdleInTxTimeout, cfg.IdleInTxTimeoutSet)
```

Extend `DBConfig` / `toInternal` so test fixtures inherit the same defaults.

- [ ] **Step 5: Declare the vars**

`plugins/postgres/plugin.go` `ConfigVars()` — all five, so the name-parity scan (`TestConfigAll_Complete`, `cmd/cyoda/help/config_registry_test.go:73`) passes:

```go
		{Name: "CYODA_POSTGRES_STATEMENT_TIMEOUT", Description: "Maximum run time for a single SQL statement; 0 disables", Default: "5m"},
		{Name: "CYODA_POSTGRES_IDLE_IN_TX_TIMEOUT", Description: "Maximum time a connection may sit idle inside an open transaction; 0 disables", Default: "5m"},
		{Name: "CYODA_POSTGRES_ACQUIRE_TIMEOUT", Description: "Maximum wait for a free pooled connection before failing with 503; 0 disables", Default: "10s"},
		{Name: "CYODA_POSTGRES_MIGRATE_LOCK_TIMEOUT", Description: "Maximum lock wait during schema migration; 0 disables", Default: "5m"},
		{Name: "CYODA_POSTGRES_SEARCH_STATEMENT_TIMEOUT", Description: "Statement ceiling for async search scans; 0 disables", Default: "30m"},
```

Document all five in `cmd/cyoda/help/content/config/database.md`, stating the relationship the operator needs: the idle ceiling must clear the longest legitimate idle gap, which is a compute-node callout bounded by `responseTimeoutMs`.

- [ ] **Step 6: Confirm the unit tests pass**

Run: `cd plugins/postgres && go test ./... -short -v`
Expected: PASS.

- [ ] **Step 7: Write and run the cascade-margin E2E**

Coverage row 11b. The margin is per-gap, not cumulative — a deep cascade (`maxCascadeDepth` is 100, `engine.go:97`) can spend far more than 5m in total across many callouts. It stays safe because the postgres audit store issues a real `INSERT` inside the transaction between every processor (`engine_processors.go:129` → `plugins/postgres/sm_audit_store.go:39`), which resets the idle timer. That is load-bearing and was previously undesigned, so it is asserted:

```go
// TestE2E_DeepCascade_ExceedsIdleCeilingInTotal_StillCommits is coverage row 11b.
// Set the idle ceiling deliberately low and run a multi-processor cascade whose
// TOTAL callout time exceeds it while no single gap does. It commits because the
// per-processor audit INSERT resets the idle timer.
func TestE2E_DeepCascade_ExceedsIdleCeilingInTotal_StillCommits(t *testing.T) {
	h := newCallbackHarnessConfigured(t, func(cfg *app.Config) {
		t.Setenv("CYODA_POSTGRES_IDLE_IN_TX_TIMEOUT", "3s")
	})
	// Five SYNC processors, each sleeping 1s: 5s total, 1s per gap.
	...
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d; the per-processor audit INSERT should have reset the idle timer", resp.StatusCode)
	}
}
```

Run: `go test ./internal/e2e/ -run DeepCascade_ExceedsIdleCeiling -v`
Expected: PASS.

- [ ] **Step 8: Commit**

```bash
git add plugins/postgres/ cmd/cyoda/help/content/config/database.md internal/e2e/
git commit -m "feat(postgres): bound statements and idle-in-transaction time

An abandoned transaction is idle by definition, so the idle ceiling is what
plugs the leak underneath the application-side guard. Values render as bare
integer milliseconds: PostgreSQL's units have no \"m\" and Go's own duration
syntax is invalid in the startup packet."
```

---

## Task 10: Acquire timeout, `StorageUnavailable`, and `classifyBeginErr` wiring

`pgxpool.Config` has no `AcquireTimeout` field, so this is a Go-side deadline. **It must be scoped to the acquire alone, inside the plugin.** `Begin` returns `spi.WithTransaction(ctx, …)` derived from its *input* context (`transaction_manager.go:126`), so wrapping that input in `context.WithTimeout` would give every later operation on the transaction a 10s deadline and cancel it the moment `Begin` returns. This is the single easiest thing in the change to implement wrongly, and it fails in a way ordinary tests would not catch — hence coverage row 11e.

**Files:**
- Modify: `plugins/postgres/transaction_manager.go` (`Begin` `:71-127`)
- Modify: `plugins/postgres/model_store.go:359`
- Modify: `plugins/postgres/ceilings.go` (add `acquireTimeoutError`)
- Modify: `internal/domain/entity/service.go` — seven `common.Internal("failed to begin transaction", err)` sites → `classifyBeginErr`
- Modify: `internal/domain/entity/service.go:1989` `classifyWorkflowError` — a storage-unavailable branch
- Test: `plugins/postgres/acquire_test.go` (new)
- Test: `internal/e2e/storage_ceilings_e2e_test.go` (new)

**Interfaces:**
- Consumes: `envCeiling` (Task 9); `classifyBeginErr` / `storageUnavailable` (Task 3); `newTinyPoolHarness` (Task 4).
- Produces: `acquireTimeoutError` satisfying `interface{ StorageUnavailable() bool }`; Task 11 reuses the same marker for `25P03`.

- [ ] **Step 1: Write the failing tests**

Coverage rows 10, 11, 11e.

```go
// TestBegin_AcquireDeadlineDoesNotLeakIntoTheTransaction is coverage row 11e and
// the most important test in this task. If the deadline is applied to Begin's
// input context, every later operation on the returned transaction inherits it
// and the transaction dies the moment the acquire window closes — which ordinary
// tests, all of which finish in milliseconds, would never notice.
func TestBegin_AcquireDeadlineDoesNotLeakIntoTheTransaction(t *testing.T) {
	tm := newTestTxManager(t, withAcquireTimeout(200*time.Millisecond))
	txID, txCtx, err := tm.Begin(tenantCtx(t))
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	time.Sleep(500 * time.Millisecond) // well past the acquire deadline

	if err := txCtx.Err(); err != nil {
		t.Fatalf("transaction context expired with the acquire deadline: %v", err)
	}
	if err := tm.Commit(txCtx, txID); err != nil {
		t.Fatalf("transaction unusable past the acquire timeout: %v", err)
	}
}

// TestBegin_PoolSaturated_ReportsStorageUnavailable is coverage row 10's unit half.
func TestBegin_PoolSaturated_ReportsStorageUnavailable(t *testing.T) {
	tm := newTestTxManager(t, withMaxConns(1), withAcquireTimeout(200*time.Millisecond))
	if _, _, err := tm.Begin(tenantCtx(t)); err != nil {
		t.Fatalf("first begin: %v", err)
	}
	start := time.Now()
	_, _, err := tm.Begin(tenantCtx(t))
	if err == nil {
		t.Fatal("second begin succeeded on a one-connection pool")
	}
	var su interface{ StorageUnavailable() bool }
	if !errors.As(err, &su) || !su.StorageUnavailable() {
		t.Fatalf("pool exhaustion not marked storage-unavailable: %v", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("waited %v; the write should fail fast rather than queue", elapsed)
	}
}

// TestBegin_CallerCancelled_IsNotStorageUnavailable is coverage row 11.
// pool.BeginTx returns context.DeadlineExceeded both when OUR acquire wait
// expired and when the CALLER's request context expired. Mislabelling a client
// timeout as a retryable server 503 is wrong, so the plugin must distinguish
// them rather than classify on the sentinel alone.
func TestBegin_CallerCancelled_IsNotStorageUnavailable(t *testing.T) {
	tm := newTestTxManager(t)
	ctx, cancel := context.WithCancel(tenantCtx(t))
	cancel()

	_, _, err := tm.Begin(ctx)
	if err == nil {
		t.Fatal("begin on a cancelled context succeeded")
	}
	var su interface{ StorageUnavailable() bool }
	if errors.As(err, &su) && su.StorageUnavailable() {
		t.Fatal("a client-cancelled request was reported as a retryable server condition")
	}
}
```

- [ ] **Step 2: Run them and confirm they fail**

Run: `cd plugins/postgres && go test ./... -run TestBegin_ -v`
Expected: FAIL — pool exhaustion currently blocks indefinitely rather than reporting; `undefined: withAcquireTimeout`.

- [ ] **Step 3: Add the marker error**

Append to `plugins/postgres/ceilings.go`:

```go
// acquireTimeoutError marks a failure caused by this plugin's own
// connection-acquire deadline expiring: the pool could not supply a connection
// in time. It carries the StorageUnavailable marker the application layer
// matches with errors.As — an interface, not a concrete type, so no
// cyoda-go-spi change and therefore no coordinated cross-repo release. Another
// backend can opt in by returning the same shape.
type acquireTimeoutError struct{ cause error }

func (e *acquireTimeoutError) Error() string {
	return "could not acquire a database connection within the configured timeout: " + e.cause.Error()
}
func (e *acquireTimeoutError) Unwrap() error            { return e.cause }
func (e *acquireTimeoutError) StorageUnavailable() bool { return true }
```

- [ ] **Step 4: Scope the deadline to the acquire**

`plugins/postgres/transaction_manager.go`. Store `acquireTimeout` on the manager at construction (thread it from `config` through `newStoreFactory` → `initTransactionManager`), then:

```go
func (tm *TransactionManager) Begin(ctx context.Context) (string, context.Context, error) {
	tenantID, err := resolveTenant(ctx)
	if err != nil {
		return "", nil, fmt.Errorf("Begin: %w", err)
	}

	txID := uuid.UUID(tm.uuids.NewTimeUUID()).String()

	// The deadline bounds the acquire ONLY. It must not reach the context this
	// function returns: that one is derived from the caller's ctx below and
	// carries the transaction for its whole life, so a deadline on it would
	// cancel the transaction the moment the acquire window closed.
	//
	// pool.BeginTx and the set_config round-trip both return before the caller
	// touches the transaction, so bounding them leaks nothing into the handle.
	acquireCtx, cancelAcquire := tm.acquireContext(ctx)
	defer cancelAcquire()

	pgxTx, err := tm.pool.BeginTx(acquireCtx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead})
	if err != nil {
		return "", nil, tm.classifyAcquireErr(ctx, acquireCtx, err)
	}

	if _, err := pgxTx.Exec(acquireCtx, "SELECT set_config('app.current_tenant', $1, true)", string(tenantID)); err != nil {
		_ = pgxTx.Rollback(context.WithoutCancel(ctx))
		return "", nil, tm.classifyAcquireErr(ctx, acquireCtx, fmt.Errorf("Begin: failed to set tenant: %w", err))
	}

	// ... unchanged from here: registry.Register, origins, txStates ...

	return txID, spi.WithTransaction(ctx, txSpiState), nil // derived from the CALLER's ctx
}

// acquireContext returns the acquire-only deadline context. A zero timeout
// disables the deadline, matching the convention the GUC ceilings use.
func (tm *TransactionManager) acquireContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if tm.acquireTimeout <= 0 {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(ctx, tm.acquireTimeout)
}

// classifyAcquireErr distinguishes "our acquire deadline expired" from "the
// caller's request context expired". pool.BeginTx surfaces
// context.DeadlineExceeded for both, and reporting a client timeout as a
// retryable server 503 would be wrong — so the caller's context is checked
// first, and only the plugin's own deadline produces the marker.
func (tm *TransactionManager) classifyAcquireErr(callerCtx, acquireCtx context.Context, err error) error {
	if callerCtx.Err() == nil && errors.Is(acquireCtx.Err(), context.DeadlineExceeded) {
		return fmt.Errorf("Begin: %w", &acquireTimeoutError{cause: err})
	}
	return fmt.Errorf("Begin: failed to start transaction: %w", err)
}
```

Note the rollback on the `set_config` failure path now uses `context.WithoutCancel(ctx)` rather than the expired `acquireCtx` — otherwise the cleanup itself would fail on a timed-out acquire.

- [ ] **Step 5: Apply the same scoping at `model_store.go:359`**

```go
	acquireCtx, cancelAcquire := s.acquireContext(ctx)
	tx, err := s.pool.Begin(acquireCtx)
	cancelAcquire() // Begin has returned; the handle must not inherit the deadline
	if err != nil {
		return fmt.Errorf("failed to begin self-wrap tx for ExtendSchema(%s): %w", ref, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
```

It self-wraps only when there is no ambient transaction (`:355`) and already has `defer tx.Rollback` (`:364`), so it cannot nest. Its only reachable callers are `ValidateOrExtend`'s five entity/workflow call sites — model import does **not** reach it.

Its acquire failure surfaces as **500, not 503**: `validate.go:124` wraps the error in `ErrInternalSchema` and `classifyValidateOrExtendErr` (`handler.go:203`) maps that to `common.Internal`. Leave it. Threading it through the schema classifier is not worth it — this is the schema-extension path reporting that it could not extend the schema, and the cause is in the log either way. Task 8's table is unaffected. Record that in a comment at the call site so the next reader does not "fix" it.

- [ ] **Step 6: Do NOT apply it to `Query` / `Exec` / `QueryRow` / `CopyFrom`**

Add a comment in `store_factory.go` next to `resolveRaw` recording why, so the omission reads as a decision rather than an oversight:

```go
// The acquire deadline is deliberately NOT applied to pool.Query / Exec /
// QueryRow / CopyFrom. For Query, pgxpool holds the connection for the returned
// pgx.Rows under the same context, so a deadline there would cap statement
// execution and row iteration too — breaking search_store.go's CopyFrom of a
// whole async-search result set and every non-transactional read routed through
// this fallback. (Exec/QueryRow/CopyFrom release before returning, so the
// objection is narrower for them, but splitting the rule by method would be a
// trap for the next reader.) Bounding these properly means an explicit
// Acquire/Release restructure that reimplements what pgxpool already does
// internally, with a connection-leak failure mode of its own — and it is
// unnecessary: these statements are bounded server-side by statement_timeout,
// so the connection returns within that ceiling regardless.
```

No exemption for the engine's segment `Begin` either. `executeCommitBeforeDispatch` opens TX_post through the same `Begin` (`engine_processors.go:333`, `:409`) after the external dispatch has fired, so a failed acquire there leaves the side effect executed and the post-dispatch state unapplied. That outcome is not introduced by the deadline — `:335` already errors if `Begin` fails for any reason — and exempting it is not free: `plugins/postgres/go.mod` depends only on `cyoda-go-spi`, with no path back to the cyoda-go root module, so there is no channel for the engine to signal "skip the deadline" short of a new SPI context key plus a coordinated cross-repo release. The deadline is generous relative to this seam anyway: TX_pre's connection was returned by `flushAndCommitSegment` immediately before, so the segment `Begin` contends for a connection the same goroutine just released.

- [ ] **Step 7: Classify at the ten wrap sites**

Replace `common.Internal("failed to begin transaction", err)` with `classifyBeginErr(err)` at `service.go:264`, `:647`, `:787`, `:911`, `:1174`, `:1365`, `:1713` — all seven are the `beginScope` error return introduced in Task 4.

`common.Internal(...)` fixes the status at 500, so the classification has to run *before* it. `AppError.Unwrap` (`internal/common/errors.go:48`) means a later `errors.As` would still find the cause, but by then the status is already wrong.

For the three workflow-package sites the marker must survive the wrap and be picked up downstream. `fire_scheduled.go:114` and `engine_processors.go:335`, `:411` already wrap with `%w` / `errors.Join`, so the chain is intact; add the branch that acts on it in `classifyWorkflowError` (`service.go:1989`), placed **before** the `ErrCommitBeforeDispatchInfra` branch, which would otherwise win and return 500:

```go
func classifyWorkflowError(err error) *common.AppError {
	var appErr *common.AppError
	if errors.As(err, &appErr) {
		return appErr
	}
	// Before the infra branches below: a segment Begin that could not acquire a
	// connection is transient contention, not an unexplained engine failure, and
	// ErrCommitBeforeDispatchInfra would otherwise claim it as a 500.
	if suErr := storageUnavailable(err); suErr != nil {
		return suErr
	}
	if errors.Is(err, contract.ErrNoMatchingMember) {
		...
```

`fire_scheduled.go:114` has no HTTP status to map — it returns `OutcomeDropped` to the scheduler, which logs it. The `%w` wrap keeps the marker inspectable; no status mapping exists on that path, which is why Task 8's table is unaffected. Leave the site as-is and add a one-line comment saying so.

- [ ] **Step 8: Confirm the plugin tests pass**

Run: `cd plugins/postgres && go test ./... -v`
Expected: PASS.

- [ ] **Step 9: Write and run the E2E and gRPC halves**

Create `internal/e2e/storage_ceilings_e2e_test.go`. Coverage row 10 (HTTP and gRPC):

```go
// TestE2E_SaturatedPool_WriteReturns503 is coverage row 10. It uses its own
// tiny-pool app: run against the shared suite's pool it could not isolate, and
// it would stall every other test for a full acquire timeout.
func TestE2E_SaturatedPool_WriteReturns503(t *testing.T) {
	h := newTinyPoolHarness(t, 1)
	t.Setenv("CYODA_POSTGRES_ACQUIRE_TIMEOUT", "500ms")

	hold := h.holdConnection(t) // a criteria callback that blocks until released
	defer hold.release()

	start := time.Now()
	resp := h.POST(t, "/api/entity/JSON/order/1", body)
	elapsed := time.Since(start)

	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", resp.StatusCode)
	}
	assertProblemDetailCode(t, resp, "STORAGE_UNAVAILABLE")
	assertRetryable(t, resp)
	if elapsed > 3*time.Second {
		t.Fatalf("queued for %v instead of failing fast", elapsed)
	}
}

// TestE2E_SaturatedPool_GRPCEnvelope asserts the gRPC surface reports the same
// failure. HTTP and gRPC are separate entry points and both must be covered.
func TestE2E_SaturatedPool_GRPCEnvelope(t *testing.T) {
	h := newTinyPoolHarness(t, 1)
	t.Setenv("CYODA_POSTGRES_ACQUIRE_TIMEOUT", "500ms")

	hold := h.holdConnection(t)
	defer hold.release()

	resp := h.GRPCCreateEntity(t, entityReq)
	if resp.Success {
		t.Fatal("gRPC create succeeded on a saturated one-connection pool")
	}
	if resp.Error.GetCode() != "STORAGE_UNAVAILABLE" {
		t.Fatalf("Error.Code = %q, want STORAGE_UNAVAILABLE", resp.Error.GetCode())
	}
}
```

These are concurrency/fault tests: isolated single-backend E2E, asserting consistency (one 503, fast, correct code) rather than a precise interleave.

Run: `go test ./internal/e2e/ -run 'SaturatedPool' -v`
Expected: PASS.

- [ ] **Step 10: Commit**

```bash
git add plugins/postgres/ internal/domain/entity/ internal/e2e/
git commit -m "feat(postgres): fail writes fast on a saturated pool with 503

The deadline is scoped to the acquire alone: Begin returns a context derived
from the caller's, so a deadline on the input would cancel the transaction the
moment the acquire window closed. A caller-cancelled request is distinguished
from pool exhaustion rather than classified on context.DeadlineExceeded alone."
```

---

## Task 11: Classify the server-side aborts

Two server-side aborts need classifying, or the ceilings from Task 9 surface as opaque 500s.

- **`idle_in_transaction_session_timeout`** → SQLSTATE `25P03`. It *terminates the session*, it does not merely abort the transaction, so the next operation may return either a `*pgconn.PgError` carrying `25P03` **or** a transport error (unexpected EOF, broken pipe) depending on whether pgx reads the buffered `ErrorResponse` before noticing the closed socket. Classify **both** shapes. Which one actually arrives is settled by test, not by reading.
- **`statement_timeout`** → SQLSTATE `57014` (`query_canceled`), on any statement, on any endpoint including reads. Nothing classifies it today, so it falls through `classifyError` (`transaction_manager.go:507`) as an unexplained error.

They are classified **differently**, because they differ in whether retrying helps. `25P03` and pool exhaustion are transient contention → 503, retryable. `57014` → 500 with a ticket UUID: re-running a statement that just exceeded the ceiling will exceed it again, so advertising it as retryable would be a lie. Every operation already declares `default: InternalServerError`, so this adds no wire-contract change and keeps Task 8's table at nine rows. What changes is that the log line names the ceiling instead of reporting an unexplained failure.

`25P03` also needs `cleanupTx`. PostgreSQL kills the *session*, but `cleanupTx` (`transaction_manager.go:321`) runs only from `Commit`/`Rollback`, so the `registry`, `tenants`, `origins` and `txStates` entries — the last carrying the read and write sets — would survive indefinitely. The DB-side ceiling reclaims the connection; only Task 4's guard, or an explicit cleanup here, reclaims the application-side state.

**Files:**
- Modify: `plugins/postgres/transaction_manager.go` (`classifyError` `:507-521`)
- Modify: `plugins/postgres/ceilings.go` (add `idleInTxAbortError`)
- Test: `plugins/postgres/classify_test.go` (new)
- Test: `internal/e2e/storage_ceilings_e2e_test.go` (extend)

**Interfaces:**
- Consumes: the `StorageUnavailable` marker convention (Task 10); `classifyBeginErr` / `classifyWorkflowError` wiring (Task 10).
- Produces:
  - `func isIdleInTxAbort(err error) bool`
  - `func isStatementTimeout(err error) bool`
  - `idleInTxAbortError` satisfying `interface{ StorageUnavailable() bool }`

- [ ] **Step 1: Write the failing tests**

Coverage rows 9, 11f, 12.

```go
// TestClassifyError_IdleInTxAbort_BothShapes: 25P03 terminates the SESSION, so
// the next operation returns either a PgError carrying 25P03 or a transport
// error, depending on whether pgx read the buffered ErrorResponse before
// noticing the closed socket. Both must classify.
func TestClassifyError_IdleInTxAbort_BothShapes(t *testing.T) {
	cases := []struct {
		name string
		err  error
	}{
		{"server error response", &pgconn.PgError{Code: pgerrcode.IdleInTransactionSessionTimeout}},
		{"connection closed first", errors.New("unexpected EOF")},
		{"broken pipe", &net.OpError{Op: "write", Err: errors.New("broken pipe")}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if !isIdleInTxAbort(markSessionKilled(tc.err)) {
				t.Fatalf("not classified as an idle-in-transaction abort: %v", tc.err)
			}
		})
	}
}

// TestClassifyError_StatementTimeout_IsNotRetryable is coverage row 12.
func TestClassifyError_StatementTimeout_IsNotRetryable(t *testing.T) {
	err := classifyError(&pgconn.PgError{Code: pgerrcode.QueryCanceled, Message: "canceling statement due to statement timeout"})
	var su interface{ StorageUnavailable() bool }
	if errors.As(err, &su) && su.StorageUnavailable() {
		t.Fatal("statement timeout advertised as retryable; re-running it would exceed the ceiling again")
	}
	if !isStatementTimeout(err) {
		t.Fatal("statement timeout not recognised; it would surface as an unexplained failure")
	}
}

// TestClassifyError_StatementTimeoutIsNotAConflict guards the boundary against
// the existing 40001/40P01 mapping.
func TestClassifyError_StatementTimeoutIsNotAConflict(t *testing.T) {
	err := classifyError(&pgconn.PgError{Code: pgerrcode.QueryCanceled})
	if errors.Is(err, spi.ErrConflict) {
		t.Fatal("statement timeout mapped to a retryable conflict")
	}
}

// TestIdleInTxAbort_ClearsPerTransactionState is coverage row 11f. PostgreSQL
// kills the session; cleanupTx only runs from Commit/Rollback, so without an
// explicit cleanup here the registry, tenants, origins and txStates entries —
// the last carrying the read and write sets — survive indefinitely.
func TestIdleInTxAbort_ClearsPerTransactionState(t *testing.T) {
	tm := newTestTxManager(t)
	txID, txCtx, err := tm.Begin(tenantCtx(t))
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	killSessionFor(t, tm, txID) // terminate the backend out from under the tx

	if _, err := tm.doSomething(txCtx, txID); err == nil {
		t.Fatal("expected the next operation to fail on a killed session")
	}
	assertNoResidue(t, tm, txID) // registry, tenants, origins, txStates all empty
}
```

- [ ] **Step 2: Run them and confirm they fail**

Run: `cd plugins/postgres && go test ./... -run 'TestClassifyError_Idle|TestClassifyError_Statement|TestIdleInTxAbort' -v`
Expected: FAIL — `undefined: isIdleInTxAbort`.

- [ ] **Step 3: Implement the classifiers**

Append to `plugins/postgres/ceilings.go`:

```go
// idleInTxAbortError marks an operation that found its transaction gone because
// idle_in_transaction_session_timeout fired. Transient contention — the same
// request may well succeed on a second attempt — so it carries the same
// StorageUnavailable marker as pool exhaustion.
type idleInTxAbortError struct{ cause error }

func (e *idleInTxAbortError) Error() string {
	return "transaction was reclaimed after exceeding the idle-in-transaction ceiling: " + e.cause.Error()
}
func (e *idleInTxAbortError) Unwrap() error            { return e.cause }
func (e *idleInTxAbortError) StorageUnavailable() bool { return true }

// isIdleInTxAbort reports whether err is PostgreSQL reclaiming a transaction
// that sat idle past the ceiling.
//
// Two shapes, because 25P03 terminates the SESSION rather than merely aborting
// the transaction: pgx may read the buffered ErrorResponse and surface a
// PgError, or notice the closed socket first and surface a transport error.
// Which one arrives is settled by test, not by reading the driver.
func isIdleInTxAbort(err error) bool {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code == pgerrcode.IdleInTransactionSessionTimeout
	}
	return isConnectionTorn(err)
}

// isStatementTimeout reports whether err is PostgreSQL cancelling a statement
// that exceeded statement_timeout. Reachable on any statement and any endpoint,
// including reads.
func isStatementTimeout(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == pgerrcode.QueryCanceled
}
```

`isConnectionTorn` covers `io.ErrUnexpectedEOF`, `net.ErrClosed`, `syscall.EPIPE`, `syscall.ECONNRESET` and pgx's `*pgconn.ConnectError`. Determine the exact set from the live-server test in Step 5 rather than guessing — that is the point of settling it by test.

Extend `classifyError` (`transaction_manager.go:507`):

```go
func classifyError(err error) error {
	if err == nil {
		return nil
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch {
		case pgErr.Code == pgerrcode.SerializationFailure || pgErr.Code == pgerrcode.DeadlockDetected:
			return fmt.Errorf("%w: %w", spi.ErrConflict, err)
		case pgErr.Code == pgerrcode.UniqueViolation && pgErr.ConstraintName == "unique_claims_uq":
			return fmt.Errorf("%w: %w", spi.ErrUniqueViolation, err)
		case pgErr.Code == pgerrcode.IdleInTransactionSessionTimeout:
			// Transient contention: the transaction is gone, but a retry on a
			// fresh one may well succeed.
			return &idleInTxAbortError{cause: err}
		case pgErr.Code == pgerrcode.QueryCanceled:
			// NOT retryable and deliberately not marked so: re-running a
			// statement that just exceeded the ceiling will exceed it again.
			// The 500 it becomes carries a ticket; this log line is what turns
			// an unexplained failure into a named cause.
			slog.Warn("statement cancelled after exceeding the configured ceiling",
				"pkg", "postgres", "setting", "statement_timeout", "err", err)
			return err
		}
	}
	return err
}
```

- [ ] **Step 4: Clear per-transaction state on a `25P03` abort**

Wherever `classifyError` runs on an operation against a live transaction, an `idleInTxAbortError` means the session is gone and the bookkeeping must go with it. Add a single funnel rather than sprinkling calls:

```go
// classifyTxError is classifyError for errors raised against a specific
// transaction. When the session was killed by the idle ceiling, the transaction
// no longer exists server-side and cleanupTx will never be reached from
// Commit/Rollback — so the registry, tenant, origin and txState entries (the
// last holding the read and write sets) are reclaimed here.
func (tm *TransactionManager) classifyTxError(txID string, err error) error {
	classified := classifyError(err)
	if isIdleInTxAbort(classified) {
		tm.cleanupTx(txID)
	}
	return classified
}
```

Route the transaction-scoped call sites in `transaction_manager.go` and the stores through it. Find them with:

Run: `cd plugins/postgres && grep -rn 'classifyError(' --include='*.go' | grep -v _test.go`

- [ ] **Step 5: Confirm the unit tests pass, then settle the shape question with a live server**

Run: `cd plugins/postgres && go test ./... -short -v`
Expected: PASS.

Then write the live-server test that determines which error shape actually arrives, and fix `isConnectionTorn` to match what it observes:

```go
// TestLive_IdleInTxCeiling_ErrorShape settles by observation which of the two
// documented shapes pgx surfaces. Set the ceiling to 1s, begin, sleep 2s, then
// operate.
func TestLive_IdleInTxCeiling_ErrorShape(t *testing.T) {
	if testing.Short() {
		t.Skip("requires a live PostgreSQL")
	}
	...
	t.Logf("observed error shape: %#v", err) // record it in the test's comment
	if !isIdleInTxAbort(err) {
		t.Fatalf("shape not classified: %v", err)
	}
}
```

Run: `cd plugins/postgres && go test ./... -run TestLive_IdleInTxCeiling -v`
Expected: PASS. Record the observed shape in a comment above `isConnectionTorn`.

- [ ] **Step 6: Write the E2E and gRPC halves**

Coverage rows 9 and 12, in `internal/e2e/storage_ceilings_e2e_test.go`:

```go
// TestE2E_IdleInTxCeiling_Returns503 is coverage row 9.
func TestE2E_IdleInTxCeiling_Returns503(t *testing.T) {
	h := newCallbackHarnessConfigured(t, func(cfg *app.Config) {
		t.Setenv("CYODA_POSTGRES_IDLE_IN_TX_TIMEOUT", "1s")
	})
	// A SYNC processor that sleeps 3s with no intervening audit write, so the
	// connection genuinely sits idle inside the transaction.
	resp := h.POST(t, "/api/entity/JSON/slow/1", body)

	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", resp.StatusCode)
	}
	assertProblemDetailCode(t, resp, "STORAGE_UNAVAILABLE")
	assertRetryable(t, resp)
}

// TestE2E_IdleInTxCeiling_GRPCEnvelope — same failure, second entry point.
func TestE2E_IdleInTxCeiling_GRPCEnvelope(t *testing.T) {
	h := newCallbackHarnessConfigured(t, func(cfg *app.Config) {
		t.Setenv("CYODA_POSTGRES_IDLE_IN_TX_TIMEOUT", "1s")
	})
	// Same 3s-sleeping SYNC processor with no intervening audit write.
	resp := h.GRPCCreateEntity(t, slowEntityReq)
	if resp.Success {
		t.Fatal("create succeeded despite the transaction being reclaimed")
	}
	if resp.Error.GetCode() != "STORAGE_UNAVAILABLE" {
		t.Fatalf("Error.Code = %q, want STORAGE_UNAVAILABLE", resp.Error.GetCode())
	}
}

// TestE2E_StatementTimeout_Returns500WithTicket is coverage row 12.
func TestE2E_StatementTimeout_Returns500WithTicket(t *testing.T) {
	h := newCallbackHarnessConfigured(t, func(cfg *app.Config) {
		t.Setenv("CYODA_POSTGRES_STATEMENT_TIMEOUT", "100ms")
	})
	resp := h.GET(t, "/api/entity/JSON/big/1") // a read heavy enough to exceed 100ms
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 — re-running the statement would exceed the ceiling again", resp.StatusCode)
	}
	assertTicketUUID(t, resp)
	assertNoInternalDetail(t, resp) // no pgx text, no SQL, no connection string
}
```

Run: `go test ./internal/e2e/ -run 'IdleInTxCeiling|StatementTimeout' -v`
Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add plugins/postgres/ internal/e2e/
git commit -m "feat(postgres): classify the ceilings' server-side aborts

25P03 terminates the session, so both the PgError and the transport-error shape
classify, and the per-transaction bookkeeping is reclaimed — Commit/Rollback
will never run to do it. 57014 stays a 500: re-running a statement that just
exceeded the ceiling will exceed it again."
```

---

## Task 12: Async search gets its own statement ceiling

Async search is the one workload whose purpose is to run long, and on postgres it is bounded by **nothing at all** today: the scan budget raising `spi.ErrScanBudgetExhausted` exists only in `plugins/sqlite/searcher.go:104` and `:240` — `internal/domain/search/service.go:227` is just the error mapping and `plugins/postgres/searcher.go` has no equivalent — while the job goroutine (`service.go:433`) runs on `context.Background()`. So it is the strongest case for a ceiling and the worst fit for a shared one: a single knob would force operators to choose between fast-failing interactive writes and long analytical scans.

The async-search path is pool-direct and separable (`search_store.go`), so it gets its own: `CYODA_POSTGRES_SEARCH_STATEMENT_TIMEOUT`, default `30m`, applied as `SET LOCAL statement_timeout` on that path rather than as a second pool. The interactive ceiling stays at 5m.

**Files:**
- Modify: `plugins/postgres/searcher.go` — apply the ceiling on the async-search scan path
- Modify: `plugins/postgres/search_store.go` — thread the configured value
- Modify: `internal/domain/search/service.go:451` — sanitize the persisted failure message
- Test: `internal/e2e/storage_ceilings_e2e_test.go` (extend)

**Interfaces:**
- Consumes: `config.SearchStatementTimeout` (Task 9); `isStatementTimeout` (Task 11).
- Produces: no new exported symbols.

- [ ] **Step 1: Write the failing test**

Coverage row 11c.

```go
// TestE2E_AsyncSearch_CeilingExceeded_RecordsSanitizedFailure is coverage row 11c.
//
// The async search job is the deliberate exception to the sanitization rule:
// internal/domain/search/service.go:451 persists searchErr.Error() into the job
// record, which GetJob serves back. A raw pgx string there would leak internals,
// so the ceiling produces a fixed, non-revealing message naming the ceiling and
// nothing else — the job is the caller's own work, and knowing it hit the search
// ceiling is exactly what they need.
func TestE2E_AsyncSearch_CeilingExceeded_RecordsSanitizedFailure(t *testing.T) {
	h := newCallbackHarnessConfigured(t, func(cfg *app.Config) {
		t.Setenv("CYODA_POSTGRES_SEARCH_STATEMENT_TIMEOUT", "100ms")
	})
	seedManyEntities(t, h, 5000)

	jobID := h.startAsyncSearch(t, broadCondition)
	job := waitForTerminal(t, h, jobID, 30*time.Second)

	if job.Status != "FAILED" {
		t.Fatalf("status = %q, want FAILED", job.Status)
	}
	if !strings.Contains(job.Error, "search statement ceiling") {
		t.Fatalf("error %q does not name the ceiling the caller hit", job.Error)
	}
	for _, leak := range []string{"pgx", "SELECT", "SQLSTATE", "57014", "host=", "password"} {
		if strings.Contains(job.Error, leak) {
			t.Fatalf("job error leaked internals (%q): %s", leak, job.Error)
		}
	}
}

// TestE2E_InteractiveCeilingUnchangedByTheSearchCeiling guards the split: the
// two knobs must not collapse into one. SET LOCAL scopes the search ceiling to
// its own transaction, so an interactive write on the same pool keeps the 5m
// ceiling — a single shared knob would force operators to choose between
// fast-failing writes and long analytical scans.
func TestE2E_InteractiveCeilingUnchangedByTheSearchCeiling(t *testing.T) {
	h := newCallbackHarnessConfigured(t, func(cfg *app.Config) {
		t.Setenv("CYODA_POSTGRES_SEARCH_STATEMENT_TIMEOUT", "30m")
		t.Setenv("CYODA_POSTGRES_STATEMENT_TIMEOUT", "200ms")
	})
	// The interactive ceiling still applies to an ordinary write path.
	resp := h.POST(t, "/api/entity/JSON/slowwrite/1", body) // a write exceeding 200ms
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d; the search ceiling leaked onto the interactive path", resp.StatusCode)
	}
	// And the search path is not capped at 200ms.
	jobID := h.startAsyncSearch(t, broadCondition)
	job := waitForTerminal(t, h, jobID, 60*time.Second)
	if job.Status != "COMPLETED" {
		t.Fatalf("async search status = %q; the interactive ceiling leaked onto the search path", job.Status)
	}
}
```

- [ ] **Step 2: Run it and confirm it fails**

Run: `go test ./internal/e2e/ -run AsyncSearch_CeilingExceeded -v`
Expected: FAIL — the scan runs to completion; nothing bounds it.

- [ ] **Step 3: Apply `SET LOCAL statement_timeout` on the search scan**

In `plugins/postgres/searcher.go`, on the async-search scan path, wrap the scan in a transaction whose first statement raises the ceiling:

```go
	// The async-search path gets its own statement ceiling. SET LOCAL scopes it to
	// this transaction, so the interactive 5m ceiling the pool carries is
	// unaffected — a single shared knob would force operators to choose between
	// fast-failing interactive writes and long analytical scans.
	//
	// pgDurationMillis, not a Go duration string: PostgreSQL has no "m" unit.
	if _, err := tx.Exec(ctx, "SET LOCAL statement_timeout = "+pgDurationMillis(s.searchStatementTimeout)); err != nil {
		return nil, fmt.Errorf("set search statement ceiling: %w", err)
	}
```

Thread `searchStatementTimeout` from `config` through the store factory the same way Task 10 threaded `acquireTimeout`.

- [ ] **Step 4: Produce the fixed, non-revealing message**

Where the async job's error is mapped, convert a statement-timeout into a fixed string before it reaches `UpdateJobStatus`:

```go
// searchCeilingMessage is what a caller sees when their own job exceeded the
// search statement ceiling. Fixed and non-revealing: GetJob serves this string
// straight back, so a raw driver error here would leak internals.
const searchCeilingMessage = "search exceeded the search statement ceiling"
```

In `internal/domain/search/service.go`, in the goroutine at `:450`:

```go
		if searchErr != nil {
			msg := searchErr.Error()
			if errors.Is(searchErr, spi.ErrScanBudgetExhausted) || isSearchCeilingErr(searchErr) {
				msg = searchCeilingMessage
			}
			if err := s.searchStore.UpdateJobStatus(bgCtx, jobID, "FAILED", 0, msg, finishTime, calcTimeMs); err != nil {
				slog.Error("failed to update search job status", "pkg", "search", "jobID", jobID, "err", err)
			}
			// Full detail stays server-side.
			slog.Warn("async search job failed", "pkg", "search", "jobID", jobID, "err", searchErr)
			return
		}
```

`isSearchCeilingErr` is a small domain-side predicate; have the plugin surface the condition as a sentinel it can test (mirror how `spi.ErrScanBudgetExhausted` is already mapped at `service.go:227`) rather than string-matching pgx output.

- [ ] **Step 5: Confirm it passes**

Run: `go test ./internal/e2e/ -run AsyncSearch -v && cd plugins/postgres && go test ./... -v`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add plugins/postgres/ internal/domain/search/ internal/e2e/
git commit -m "feat(postgres): give async search its own statement ceiling

On postgres the async scan is bounded by nothing today — the scan budget exists
only in sqlite and the job goroutine runs on context.Background(). Its own knob,
applied with SET LOCAL, keeps the interactive ceiling at 5m."
```

---

## Task 13: Migration-connection settings

`openDB` builds the migration connection from `pool.Config().ConnConfig` (`migrate.go:23`), so it would inherit the app pool's `statement_timeout` from Task 9 and kill a legitimate long index build. It needs the opposite settings.

| Setting | Value |
|---|---|
| `lock_timeout` | `CYODA_POSTGRES_MIGRATE_LOCK_TIMEOUT`, default `5m` |
| `statement_timeout` | `0` |
| `idle_in_transaction_session_timeout` | `0` |

Doing work for a long time is fine; **waiting** is what must be bounded. `5m` rather than something tighter because a migration's own DDL lock waits are legitimate during a rolling upgrade — an old node's in-flight write transaction is itself bounded by Task 9's ceilings, so a bounded wait succeeds where a 30s one would abort a healthy upgrade.

The `lock_timeout` is also what bounds golang-migrate's advisory lock. Serialisation across nodes is not new: `Lock()` already blocks on `SELECT pg_advisory_lock($1)` (`golang-migrate/v4@v4.19.1 database/pgx/v5/pgx.go:229`) and followers get `ErrNoChange` once the winner finishes. What is missing is a *bound* — that call uses `context.Background()`, so the wait is indefinite at the Go level. Advisory locks go through PostgreSQL's regular lock manager, so `lock_timeout` aborts the wait. A single-node install is unaffected because its lock is uncontended. **This claim is load-bearing and is proven by test, not taken from documentation.**

**Files:**
- Modify: `plugins/postgres/migrate.go` — `openDB` `:23-25`, `runMigrations` `:30`, `RunMigrateWithDSN` `:88-122`, `migrateDown` `:231`
- Modify: `plugins/postgres/plugin.go:42-53` — pass `cfg.MigrateLockTimeout`
- Create: `plugins/postgres/migrate_concurrency_test.go`
- Modify: `plugins/postgres/export_test.go` — test wrappers

**Interfaces:**
- Consumes: `pgDurationMillis`, `config.MigrateLockTimeout` (Task 9).
- Produces, used by Task 14:
  - `func openDB(pool *pgxpool.Pool, lockTimeout time.Duration) *sql.DB`
  - `func migrationRuntimeParams(lockTimeout time.Duration) map[string]string`
  - `func runMigrations(ctx context.Context, pool *pgxpool.Pool, lockTimeout time.Duration) error`

- [ ] **Step 1: Write the failing tests**

Coverage rows 13 and 14.

```go
// TestMigrationRuntimeParams_DoNotInheritAppCeilings is coverage row 14. A
// migration's DDL may legitimately run for a long time; inheriting the app
// pool's statement_timeout would kill a long index build.
func TestMigrationRuntimeParams_DoNotInheritAppCeilings(t *testing.T) {
	pool := newPoolWithCeilings(t, 5*time.Minute, 5*time.Minute)
	db := openDB(pool, 5*time.Minute)
	defer db.Close()

	for setting, want := range map[string]string{
		"statement_timeout":                   "0",
		"idle_in_transaction_session_timeout": "0",
		"lock_timeout":                        "300000",
	} {
		var got string
		if err := db.QueryRow("SHOW " + setting).Scan(&got); err != nil {
			t.Fatalf("SHOW %s: %v", setting, err)
		}
		if normalizePgTime(got) != want {
			t.Errorf("%s = %q, want %q", setting, got, want)
		}
	}
}

// TestOpenDB_DoesNotLeakIntoTheAppPool: pool.Config() deep-copies RuntimeParams
// (pgxpool/pool.go:711 -> pgconn/config.go:162), so the migration overrides
// cannot travel back. Asserted rather than assumed.
func TestOpenDB_DoesNotLeakIntoTheAppPool(t *testing.T) {
	pool := newPoolWithCeilings(t, 5*time.Minute, 5*time.Minute)
	_ = openDB(pool, 5*time.Minute)

	conn, err := pool.Acquire(context.Background())
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer conn.Release()
	var got string
	if err := conn.QueryRow(context.Background(), "SHOW statement_timeout").Scan(&got); err != nil {
		t.Fatalf("SHOW: %v", err)
	}
	if normalizePgTime(got) == "0" {
		t.Fatal("migration override leaked into the app pool; the app ceiling is gone")
	}
}

// TestLockTimeout_AbortsAnAdvisoryLockWait is coverage row 13. The entire
// single-migrator bound rests on this: golang-migrate's Lock() uses
// context.Background(), so lock_timeout is the only thing that ends the wait.
// Advisory locks go through PostgreSQL's regular lock manager — proven here, not
// taken from documentation. Needs a live server; not a unit test.
func TestLockTimeout_AbortsAnAdvisoryLockWait(t *testing.T) {
	if testing.Short() {
		t.Skip("requires a live PostgreSQL")
	}
	const lockID = 424242

	holder := acquireConn(t)      // session A
	mustExec(t, holder, "SELECT pg_advisory_lock($1)", lockID)
	defer mustExec(t, holder, "SELECT pg_advisory_unlock($1)", lockID)

	waiter := acquireConn(t)      // session B, with a short lock_timeout
	mustExec(t, waiter, "SET lock_timeout = '300ms'")

	start := time.Now()
	_, err := waiter.Exec(context.Background(), "SELECT pg_advisory_lock($1)", lockID)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("advisory lock wait was NOT aborted by lock_timeout; the single-migrator bound does not exist")
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != pgerrcode.LockNotAvailable {
		t.Fatalf("aborted with %v, want SQLSTATE 55P03 (lock_not_available)", err)
	}
	if elapsed > 5*time.Second {
		t.Fatalf("waited %v; the timeout did not apply", elapsed)
	}
}
```

- [ ] **Step 2: Run them and confirm they fail**

Run: `cd plugins/postgres && go test ./... -run 'TestMigrationRuntimeParams|TestOpenDB_DoesNotLeak|TestLockTimeout_Aborts' -v`
Expected: FAIL on the first two — `openDB` takes one argument. `TestLockTimeout_AbortsAnAdvisoryLockWait` should PASS immediately: it characterises PostgreSQL, not our code. If it fails, **stop** — the whole single-migrator design in Task 14 rests on it, and the spec says so.

- [ ] **Step 3: Implement the migration settings**

`plugins/postgres/migrate.go`:

```go
// migrationRuntimeParams returns the connection settings a migration needs —
// the inverse of the app pool's.
//
// Doing work for a long time is fine: a migration's DDL may legitimately run for
// minutes, so the statement and idle-in-transaction ceilings are disabled
// outright. WAITING is what must be bounded. lock_timeout caps both a DDL lock
// wait and golang-migrate's pg_advisory_lock wait, which is otherwise unbounded
// at the Go level because its Lock() uses context.Background().
//
// 5m rather than something tighter: a migration's own DDL lock waits are
// legitimate during a rolling upgrade, and an old node's in-flight write
// transaction is itself bounded by the app-pool ceilings — so a bounded wait
// succeeds where a 30s one would abort a healthy upgrade.
func migrationRuntimeParams(lockTimeout time.Duration) map[string]string {
	return map[string]string{
		"lock_timeout":                        pgDurationMillis(lockTimeout),
		"statement_timeout":                   "0",
		"idle_in_transaction_session_timeout": "0",
	}
}

// openDB creates an independent *sql.DB from the pool's config, with the
// migration settings applied.
//
// pool.Config() deep-copies RuntimeParams, so these overrides cannot leak back
// into the app pool — asserted by TestOpenDB_DoesNotLeakIntoTheAppPool.
func openDB(pool *pgxpool.Pool, lockTimeout time.Duration) *sql.DB {
	connCfg := *pool.Config().ConnConfig
	if connCfg.RuntimeParams == nil {
		connCfg.RuntimeParams = map[string]string{}
	}
	for k, v := range migrationRuntimeParams(lockTimeout) {
		connCfg.RuntimeParams[k] = v
	}
	return stdlib.OpenDB(connCfg)
}
```

Applying it at `openDB` covers every caller: `runMigrations` (via `plugin.go:50`), `checkSchemaCompat`'s own handle (`plugin.go:42`), and the test-only `migrateDown` (`migrate.go:231`).

`RunMigrateWithDSN` — the `cyoda migrate` subcommand — builds an independent pool (`migrate.go:88`) that inherits nothing from the app pool, though it *does* inherit any `RuntimeParams` embedded in the DSN, so it sets the same three explicitly:

```go
	poolCfg.MaxConns = 2
	poolCfg.MinConns = 0
	// This pool inherits nothing from the app pool, but it does inherit
	// RuntimeParams embedded in the DSN — so the migration settings are applied
	// explicitly rather than relying on openDB alone.
	if poolCfg.ConnConfig.RuntimeParams == nil {
		poolCfg.ConnConfig.RuntimeParams = map[string]string{}
	}
	for k, v := range migrationRuntimeParams(lockTimeout) {
		poolCfg.ConnConfig.RuntimeParams[k] = v
	}
```

`RunMigrateWithDSN` needs the lock timeout. Read it the same way the plugin does — `envCeiling(os.Getenv, "CYODA_POSTGRES_MIGRATE_LOCK_TIMEOUT", 5*time.Minute)` — so the CLI and the server agree without a second source of truth.

Thread `lockTimeout` through `runMigrations`, `Migrate`, `migrateDown` and the `plugin.go` call sites.

- [ ] **Step 4: Confirm the tests pass**

Run: `cd plugins/postgres && go test ./... -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add plugins/postgres/
git commit -m "feat(postgres): give migrations the inverse connection settings

A migration may legitimately run long, so its statement and idle ceilings are
disabled; what gets bounded is waiting. lock_timeout is also what bounds
golang-migrate's advisory lock, whose Lock() uses context.Background()."
```

---

## Task 14: One shared boot sequence, migrations before the compat check

golang-migrate sets `dirty=true` **before** each migration step and clears it after (`migrate.go:738`, `:750`) in separate committed transactions, with the migration body itself running in a third (`pgx.go:283`) — that non-atomicity is what the flag is for. A booting node that reads `dirty=true` gets *"schema compat: database migration state is dirty at version %d — manual intervention required"* (`plugins/postgres/migrate.go:196`) and exits (`plugin.go:45-48`, `cmd/cyoda/migrate.go:84-91`) — a false alarm that invites an operator to hand-edit `schema_migrations` while a live migration is running.

The bug exists only because `checkSchemaCompat` runs **before** `runMigrations` (`plugin.go:42` then `:50`) and therefore reads `dirty` outside any lock. **Swap the order when `AutoMigrate` is true.** That is the whole fix and it needs no lock of our own: `m.Up()` takes golang-migrate's advisory lock *before* reading the dirty flag, so a follower blocks until the winner has finished and cleared it, then applies nothing and gets `ErrNoChange`. The subsequent compat check runs on a settled schema, where `dirty == true` unambiguously means a migration genuinely died and stays fatal exactly as today.

**The window is a race, not the general case.** `pgxmigrate.WithInstance` calls `ensureVersionTable`, which takes the same advisory lock unconditionally (`pgx.go:437-440`) before checking whether the table exists — so `checkSchemaCompat`'s own `WithInstance` (`migrate.go:170`) already blocks behind a migrating peer for the duration of its `m.Up()`. To lose, node B must clear that lock/unlock and then have node A acquire the lock and stamp `dirty` before B's `m.Version()` round-trip (`:188`) returns. "Boot two nodes concurrently" would therefore pass green today and prove nothing, which is why the test below is a fault-injected interleave.

Two behaviour changes to state rather than discover:

- Task 13's `lock_timeout` now bounds the pre-existing wait inside `checkSchemaCompat`. A migration legitimately running longer than 5m turns a slow-but-successful concurrent boot into a startup failure. That is the correct trade — a bounded, logged, supervisor-retried failure beats an unbounded stall.
- With migrations first, `m.Up()` locks, reads the version, and returns `ErrDirty` *before applying anything* (`migrate.go:265-277`), so a genuinely dirty schema now fails inside `runMigrations` with golang-migrate's bare *"Dirty database version N. Fix and force version."* — and `checkSchemaCompat`'s actionable message becomes dead code on the auto-migrate path, taking with it the pointer to the INVALID-index recovery procedure. `runMigrations` must translate it.

With `AutoMigrate=false` the ordering is unchanged, because nothing is migrating from this binary. A dirty read in that window is accurate information — the schema really is mid-migration under someone else's control, and this node should not start.

Rejected alternatives, recorded so they are not revisited: a second cyoda-owned advisory lock around the pair (more machinery, and its own "which connection holds it" question), and recomputing golang-migrate's lock id via `database.GenerateAdvisoryLockId` to probe `pg_locks` — that id is a 32-bit CRC over `schema\x00table\x00database` (`database/util.go:10-20`), so it couples us to the hash, its argument order, and the driver's internal name derivation (`pgx.go:75-108`), and it collides across unrelated databases on the same cluster.

**Files:**
- Modify: `plugins/postgres/migrate.go` — add `ensureSchema` / `ensureSchemaWith`, `dirtySchemaError`; translate `ErrDirty` in `runMigrations`; `RunMigrateWithDSN` `:112-121`
- Modify: `plugins/postgres/plugin.go:42-54`
- Modify: `plugins/postgres/export_test.go`
- Test: `plugins/postgres/migrate_concurrency_test.go` (extend)

**Interfaces:**
- Consumes: `openDB(pool, lockTimeout)`, `runMigrations(ctx, pool, lockTimeout)` (Task 13).
- Produces:
  - `func ensureSchema(ctx context.Context, pool *pgxpool.Pool, autoMigrate bool, lockTimeout time.Duration) error`
  - `func ensureSchemaWith(ctx context.Context, pool *pgxpool.Pool, autoMigrate bool, lockTimeout time.Duration, afterMigratorBuilt func()) error`
  - `func dirtySchemaError(version int) error`
  - `export_test.go`: `EnsureSchemaWithForTest`, `RunMigrationsForTest`

- [ ] **Step 1: Extract `ensureSchema` with the CURRENT ordering — refactor only**

Two copies exist today and they are separate, not a shared helper: `plugins/postgres/plugin.go:42-53` and `plugins/postgres/migrate.go:114-121`. Collapse them into one so the claim "the CLI is covered by the same sequence" becomes structurally true rather than maintained by hand. **Do not change the order yet** — this step must be behaviour-preserving so the failing test in Step 3 proves the ordering, not the extraction.

```go
// ensureSchema is the single startup schema sequence, shared by the plugin
// factory and the `cyoda migrate` subcommand so the ordering guarantee holds for
// both by construction.
func ensureSchema(ctx context.Context, pool *pgxpool.Pool, autoMigrate bool, lockTimeout time.Duration) error {
	return ensureSchemaWith(ctx, pool, autoMigrate, lockTimeout, nil)
}

// ensureSchemaWith is ensureSchema with a test seam. afterMigratorBuilt, when
// non-nil, runs at the analogous point in each phase: right after a migrator has
// been constructed (which itself takes and releases golang-migrate's advisory
// lock inside ensureVersionTable) and before that phase's first unlocked
// observation of the schema. That gap is the entire concurrent-boot race window
// and it cannot be reached from outside these functions. Production passes nil.
func ensureSchemaWith(ctx context.Context, pool *pgxpool.Pool, autoMigrate bool, lockTimeout time.Duration, afterMigratorBuilt func()) error {
	db := openDB(pool, lockTimeout)
	defer db.Close()
	if err := checkSchemaCompatWith(ctx, db, autoMigrate, afterMigratorBuilt); err != nil {
		return err
	}
	if autoMigrate {
		if err := runMigrationsWith(ctx, pool, lockTimeout, afterMigratorBuilt); err != nil {
			return fmt.Errorf("postgres migrate: %w", err)
		}
	}
	return nil
}
```

Split `checkSchemaCompat` and `runMigrations` into `…With` variants taking the seam, invoked immediately after their `migrate.NewWithInstance` call and before `m.Version()` / `m.Up()` respectively. The public `checkSchemaCompat` / `runMigrations` keep their signatures and pass nil.

Replace both copies with `ensureSchema`:

`plugins/postgres/plugin.go`:
```go
	if err := ensureSchema(ctx, pool, cfg.AutoMigrate, cfg.MigrateLockTimeout); err != nil {
		pool.Close()
		return nil, err
	}
```

`plugins/postgres/migrate.go` `RunMigrateWithDSN` — autoMigrate is true here; we are the migration process:
```go
	return ensureSchema(ctx, pool, true, lockTimeout)
```

Add `export_test.go` wrappers.

- [ ] **Step 2: Confirm the extraction changed nothing**

Run: `cd plugins/postgres && go test ./... -v && go test ./... -run TestMigrate -v`
Expected: PASS — every existing migration test, unchanged.

```bash
git add plugins/postgres/
git commit -m "refactor(postgres): collapse the two copies of the startup schema sequence"
```

- [ ] **Step 3: Write the failing interleave test**

Coverage rows 15, 15a, 16, 16a, 17. In `plugins/postgres/migrate_concurrency_test.go`:

```go
// transientMigratingPeer models the peer whose in-flight migration the booting
// node used to false-alarm on: it takes golang-migrate's advisory lock and
// stamps dirty exactly as golang-migrate does before a step (migrate.go:738),
// holds both for `hold`, then clears and releases (:750).
//
// It uses the driver's own Lock/SetVersion rather than recomputing the lock id,
// which would couple the test to a 32-bit CRC over an internally-derived name.
func transientMigratingPeer(t *testing.T, dsn string, version int, hold time.Duration) (started <-chan struct{}, done <-chan struct{}) {
	t.Helper()
	ready, finished := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(finished)
		db := openStdlibDB(t, dsn)
		defer db.Close()
		drv, err := pgxmigrate.WithInstance(db, &pgxmigrate.Config{})
		if err != nil {
			t.Error(err)
			close(ready)
			return
		}
		if err := drv.Lock(); err != nil {
			t.Error(err)
			close(ready)
			return
		}
		_ = drv.SetVersion(version, true)
		close(ready)
		time.Sleep(hold)
		_ = drv.SetVersion(version, false)
		_ = drv.Unlock()
	}()
	return ready, finished
}

// TestEnsureSchema_ConcurrentMigratorIsNotReportedAsDirty is coverage row 15.
//
// MUST FAIL before the ordering swap. The seam fires after this node has built
// its migrator (advisory lock taken and released) and before its first unlocked
// look at the schema — the exact window the race lives in. "Boot two nodes
// concurrently" without the seam passes today and proves nothing, because
// WithInstance's own lock serialises them.
func TestEnsureSchema_ConcurrentMigratorIsNotReportedAsDirty(t *testing.T) {
	if testing.Short() {
		t.Skip("requires a live PostgreSQL")
	}
	dsn := freshDatabase(t)
	migrateToHead(t, dsn)

	var once sync.Once
	var peerDone <-chan struct{}
	seam := func() {
		once.Do(func() {
			var ready <-chan struct{}
			ready, peerDone = transientMigratingPeer(t, dsn, headVersion(t), 500*time.Millisecond)
			<-ready // the peer now holds the lock and has stamped dirty
		})
	}

	pool := openPool(t, dsn)
	defer pool.Close()

	err := postgres.EnsureSchemaWithForTest(context.Background(), pool, true, 5*time.Minute, seam)
	if peerDone != nil {
		<-peerDone
	}
	if err != nil {
		t.Fatalf("a peer's in-flight migration was reported as a fatal dirty schema: %v", err)
	}
}

// TestEnsureSchema_GenuinelyDirtySchemaStillFailsFast is coverage row 16. No
// concurrent migrator: the flag means a migration really died.
func TestEnsureSchema_GenuinelyDirtySchemaStillFailsFast(t *testing.T) {
	dsn := freshDatabase(t)
	migrateToHead(t, dsn)
	stampDirty(t, dsn, headVersion(t))

	err := postgres.EnsureSchemaForTest(context.Background(), openPool(t, dsn), true, 5*time.Minute)
	if err == nil {
		t.Fatal("a genuinely dirty schema was allowed to start")
	}
	// Coverage row 13a: the actionable message, not golang-migrate's bare
	// "Dirty database version N. Fix and force version." With migrations first,
	// m.Up() reports dirty before applying anything, so this is where an
	// operator now meets it — and the pointer to the INVALID-index recovery
	// procedure must survive the reorder.
	if !strings.Contains(err.Error(), "manual intervention required") ||
		!strings.Contains(err.Error(), "cyoda help cli.migrate") {
		t.Fatalf("message lost its guidance: %v", err)
	}
}

// TestEnsureSchema_DatabaseNewerThanBinaryStillRefuses is coverage row 16a.
// Running migrations first must not weaken the newer-than-code guard: m.Up() on
// a database ahead of the binary finds nothing to apply and returns ErrNoChange,
// and the compat check that follows still refuses.
func TestEnsureSchema_DatabaseNewerThanBinaryStillRefuses(t *testing.T) {
	dsn := freshDatabase(t)
	migrateToHead(t, dsn)
	stampVersion(t, dsn, headVersion(t)+5, false)

	err := postgres.EnsureSchemaForTest(context.Background(), openPool(t, dsn), true, 5*time.Minute)
	if err == nil || !strings.Contains(err.Error(), "newer than this binary") {
		t.Fatalf("started against a database newer than the binary: %v", err)
	}
}

// TestEnsureSchema_SingleNodeMigratesItself is coverage row 17 — the
// uncontended case must stay boring.
func TestEnsureSchema_SingleNodeMigratesItself(t *testing.T) {
	dsn := freshDatabase(t)
	if err := postgres.EnsureSchemaForTest(context.Background(), openPool(t, dsn), true, 5*time.Minute); err != nil {
		t.Fatalf("single-node install failed to migrate itself: %v", err)
	}
	if v, dirty := schemaState(t, dsn); v != headVersion(t) || dirty {
		t.Fatalf("schema state = (%d, dirty=%v)", v, dirty)
	}
}

// TestRunMigrateWithDSN_ConcurrentWithNodeBoot is coverage row 15a. Both entry
// points go through ensureSchema, so the CLI inherits the same ordering — this
// asserts that behaviourally rather than by inspection.
func TestRunMigrateWithDSN_ConcurrentWithNodeBoot(t *testing.T) {
	dsn := freshDatabase(t)
	var wg sync.WaitGroup
	errs := make([]error, 2)
	wg.Add(2)
	go func() { defer wg.Done(); errs[0] = postgres.RunMigrateWithDSN(context.Background(), dsn) }()
	go func() { defer wg.Done(); errs[1] = postgres.EnsureSchemaForTest(context.Background(), openPool(t, dsn), true, 5*time.Minute) }()
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Errorf("participant %d failed: %v", i, err)
		}
	}
}
```

- [ ] **Step 4: Run them and confirm the interleave test fails**

Run: `cd plugins/postgres && go test ./... -run TestEnsureSchema -v`
Expected: `TestEnsureSchema_ConcurrentMigratorIsNotReportedAsDirty` FAILS with *"a peer's in-flight migration was reported as a fatal dirty schema"*. The other four should PASS already; they are the guards that must survive the swap.

If the interleave test passes here, **stop and diagnose**: the injection is not reaching the window and the test would prove nothing (Gate 1).

- [ ] **Step 5: Swap the order**

```go
func ensureSchemaWith(ctx context.Context, pool *pgxpool.Pool, autoMigrate bool, lockTimeout time.Duration, afterMigratorBuilt func()) error {
	// Migrations FIRST when this binary is the one migrating.
	//
	// m.Up() takes golang-migrate's advisory lock before reading the dirty flag,
	// so a node booting alongside a peer that is mid-migration blocks until that
	// peer has finished and cleared it, then applies nothing and gets
	// ErrNoChange. Once m.Up() returns, the schema is settled: any peer that
	// would stamp dirty must first acquire the same lock, and can only do so
	// before or after this whole call — and if after, it finds nothing to apply.
	//
	// Reading the flag first, as the sequence used to, reads it outside any lock
	// and reports a peer's in-progress migration as a fatal dirty schema,
	// inviting an operator to hand-edit schema_migrations mid-migration.
	//
	// With autoMigrate=false the ordering is moot — nothing here migrates — and a
	// dirty read is accurate information about a migration running under someone
	// else's control, which this node must not start against.
	if autoMigrate {
		if err := runMigrationsWith(ctx, pool, lockTimeout, afterMigratorBuilt); err != nil {
			return fmt.Errorf("postgres migrate: %w", err)
		}
	}

	// The compat check now runs on a settled schema. dirty == true here
	// unambiguously means a migration genuinely died, and stays fatal.
	//
	// Running migrations first does not weaken the newer-than-code guard: m.Up()
	// on a database ahead of the binary finds nothing to apply and returns
	// ErrNoChange, and this check still refuses to start.
	db := openDB(pool, lockTimeout)
	defer db.Close()
	return checkSchemaCompatWith(ctx, db, autoMigrate, afterMigratorBuilt)
}
```

- [ ] **Step 6: Translate `ErrDirty` so the actionable message stays reachable**

Add to `plugins/postgres/migrate.go`:

```go
// dirtySchemaError is the operator-facing message for a schema left mid-
// migration. Both routes into that state — golang-migrate's own pre-flight check
// inside m.Up(), and the compat check's version read — produce this exact text,
// so the recovery procedure is reachable from either. Without the translation,
// running migrations first would leave an operator with golang-migrate's bare
// "Dirty database version N. Fix and force version.", which says nothing about
// the INVALID index a failed CREATE INDEX CONCURRENTLY leaves behind.
func dirtySchemaError(version int) error {
	return fmt.Errorf(
		"database migration state is dirty at version %d — manual intervention required; "+
			"run `cyoda help cli.migrate` for the recovery procedure", version)
}
```

In `runMigrationsWith`'s goroutine:

```go
		err := m.Up()
		var dirty migrate.ErrDirty
		switch {
		case errors.Is(err, migrate.ErrNoChange):
			err = nil
		case errors.As(err, &dirty):
			err = dirtySchemaError(dirty.Version)
		}
		done <- err
```

And in `checkSchemaCompatWith`, reuse it:

```go
	if dirty {
		return fmt.Errorf("schema compat: %w", dirtySchemaError(int(dbVersion)))
	}
```

- [ ] **Step 7: Confirm everything passes**

Run: `cd plugins/postgres && go test ./... -v`
Expected: PASS — including the four guard tests from Step 4, which must not have regressed.

- [ ] **Step 8: Run the multi-node E2E**

The known dirty-postgres-migration flake in `TestMultiNode` is the symptom this fixes. Run it several times:

Run: `go test ./internal/e2e/... -run MultiNode -count=5 -v`
Expected: PASS, five for five.

- [ ] **Step 9: Commit**

```bash
git add plugins/postgres/
git commit -m "fix(postgres): migrate before checking schema compatibility

Reading the dirty flag first reads it outside any lock, so a node booting
alongside a peer's in-flight migration reports a fatal dirty schema and invites
an operator to hand-edit schema_migrations mid-migration. m.Up() takes the
advisory lock before reading the flag, so with migrations first a follower
blocks, applies nothing, and checks compatibility on a settled schema."
```

---

## Task 15: Index-migration guard and the recovery procedure

Migrations `000001`–`000006` are **not** modified. The one migration whose index could block live writers is `000002`, which adds `entities_state_idx` to `entities` — a table that already holds data by then, and whose writers a non-concurrent `CREATE INDEX` locks out for the duration of the build (SHARE conflicts with the ROW EXCLUSIVE every INSERT/UPDATE/DELETE holds). But `000002` only runs on a database below schema version 2, meaning an instance last on v0.7.x; v0.8.1 is version 2, v0.8.2 is 3, v0.8.3 is 6. There are none, so the case is not worth engineering for. Nothing else in `000003`–`000006` blocks a hot table: `000003` and `000004` create their own new tables, `000005` is functions only, and `000006` adds two defaulted columns to `scheduled_tasks`, needing only a brief AccessExclusive lock that `lock_timeout` now bounds.

The exposure that remains is the *next* index migration.

**Files:**
- Create: `plugins/postgres/migration_index_guard_test.go`
- Modify: `cmd/cyoda/help/content/cli/migrate.md`

**Interfaces:**
- Consumes: `dirtySchemaError` (Task 14) — the message points at this topic.
- Produces: no code symbols; a static rule plus operator documentation.

- [ ] **Step 1: Write the failing guard test**

Coverage row 18. This is a static rule over the migration files, not a database test.

```go
package postgres

// TestMigrations_IndexesOnExistingTablesAreConcurrent enforces two clauses.
//
// (a) An index added to a table created in an EARLIER migration must be
// CONCURRENTLY. A plain CREATE INDEX takes SHARE, which conflicts with the ROW
// EXCLUSIVE every INSERT/UPDATE/DELETE holds — it locks writers out for the
// whole build. An index created in the same migration as its own table need not
// be: that table is empty and unreachable by writers, which is why 000001's
// indexes pass on their merits rather than by exemption.
//
// (b) A file containing CREATE INDEX CONCURRENTLY must contain no other
// statement. The driver sends the whole file through one Exec with
// MultiStatementEnabled false (pgx.go:270), and PostgreSQL wraps a
// multi-statement simple query in an implicit transaction, in which CREATE INDEX
// CONCURRENTLY cannot run. 000002_grouped_stats.up.sql is the proof — a function
// plus an index in one file — and is the sole grandfathered entry.
func TestMigrations_IndexesOnExistingTablesAreConcurrent(t *testing.T) {
	// The only pre-existing violation. Adding to this list is a decision, not a
	// convenience: it means shipping a migration that locks writers out of a
	// populated table for the duration of an index build.
	grandfathered := map[string]bool{
		"000002_grouped_stats.up.sql": true,
	}

	files := upMigrations(t) // sorted by version
	tableOrigin := map[string]string{} // table name -> file that created it

	for _, f := range files {
		body := readMigration(t, f)
		for _, tbl := range createdTables(body) {
			tableOrigin[tbl] = f
		}

		for _, idx := range createIndexStatements(body) {
			if idx.concurrent {
				// Clause (b).
				if statementCount(body) > 1 {
					t.Errorf("%s: CREATE INDEX CONCURRENTLY shares a file with %d other statement(s); "+
						"the driver sends the file as one simple query, whose implicit transaction "+
						"forbids CONCURRENTLY at runtime", f, statementCount(body)-1)
				}
				continue
			}
			// Clause (a).
			if origin, ok := tableOrigin[idx.table]; ok && origin == f {
				continue // same migration as its own table: empty, unreachable
			}
			if grandfathered[f] {
				continue
			}
			t.Errorf("%s: CREATE INDEX on %q, which was created in an earlier migration — "+
				"use CREATE INDEX CONCURRENTLY, alone in its own migration file",
				f, idx.table)
		}
	}
}

// TestMigrations_GuardRejectsANewNonConcurrentIndex proves the guard has teeth
// by running the same rule over a synthetic file set.
func TestMigrations_GuardRejectsANewNonConcurrentIndex(t *testing.T) {
	violations := checkIndexRules([]migrationFile{
		{name: "000001_init.up.sql", body: "CREATE TABLE entities (id uuid);"},
		{name: "000007_new_index.up.sql", body: "CREATE INDEX entities_foo_idx ON entities (foo);"},
	}, nil)
	if len(violations) != 1 {
		t.Fatalf("guard did not reject a non-concurrent index on a hot table: %v", violations)
	}
}

// TestMigrations_GuardRejectsConcurrentIndexSharingAFile covers clause (b).
func TestMigrations_GuardRejectsConcurrentIndexSharingAFile(t *testing.T) {
	violations := checkIndexRules([]migrationFile{
		{name: "000007_two.up.sql", body: "CREATE FUNCTION f() RETURNS int AS $$ SELECT 1 $$ LANGUAGE sql;\nCREATE INDEX CONCURRENTLY x ON entities (foo);"},
	}, nil)
	if len(violations) != 1 {
		t.Fatalf("guard accepted CONCURRENTLY sharing a file: %v", violations)
	}
}
```

Factor the rule into `checkIndexRules(files []migrationFile, grandfathered map[string]bool) []string` so both the real-file test and the synthetic ones exercise the same code. Keep the SQL parsing deliberately simple — case-insensitive regexes over `CREATE TABLE`, `CREATE INDEX [CONCURRENTLY]`, and semicolon-terminated statement counting with comments stripped. It guards a convention, not arbitrary SQL.

- [ ] **Step 2: Run them and confirm they fail**

Run: `cd plugins/postgres && go test ./... -run TestMigrations_ -v`
Expected: FAIL — `undefined: checkIndexRules`.

- [ ] **Step 3: Implement the rule and confirm the current tree is clean**

Run: `cd plugins/postgres && go test ./... -run TestMigrations_ -v`
Expected: PASS, with `000002_grouped_stats.up.sql` matched by the grandfather entry and every `000001` index passing clause (a) on its merits.

- [ ] **Step 4: Document the pattern and the recovery procedure**

A failed `CREATE INDEX CONCURRENTLY` leaves an INVALID index and a dirty version. Add to `cmd/cyoda/help/content/cli/migrate.md`:

```markdown
## Adding an index migration

An index on a table that already holds data must be built with
`CREATE INDEX CONCURRENTLY`, alone in its own migration file. A plain
`CREATE INDEX` takes a lock that conflicts with every insert, update and delete
on that table for the whole build.

`CONCURRENTLY` must be the file's only statement. The migration driver sends a
file as a single simple query, and PostgreSQL wraps a multi-statement simple
query in an implicit transaction — inside which `CONCURRENTLY` cannot run.

An index created in the same migration as its own table needs no `CONCURRENTLY`:
that table is empty and no writer can reach it yet.

## Recovering from a failed concurrent index build

A `CREATE INDEX CONCURRENTLY` that fails leaves an INVALID index behind and the
migration state marked dirty. Startup then refuses with
"database migration state is dirty at version N".

1. Find the invalid index:

       SELECT i.indexrelid::regclass AS index_name
       FROM pg_index i
       WHERE NOT i.indisvalid;

2. Drop it — concurrently, so live traffic is unaffected:

       DROP INDEX CONCURRENTLY <index_name>;

3. Clear the dirty flag by forcing the version back to the last good one:

       cyoda migrate force <N-1>

4. Re-run the migration:

       cyoda migrate

Do not hand-edit `schema_migrations` while any node may be migrating. If the
dirty flag appeared during a rolling upgrade, confirm no peer is mid-migration
first — a node that has finished migrating clears the flag itself.
```

Confirm `cyoda migrate force` matches the actual subcommand surface; if the CLI has no `force`, document the equivalent `UPDATE schema_migrations SET version = N-1, dirty = false;` and say plainly that it must be run with no migrator active.

- [ ] **Step 5: Verify the help topic renders and is linked**

Run: `go test ./cmd/cyoda/help/... -v && go run ./cmd/cyoda help cli.migrate`
Expected: PASS; the topic renders with both new sections. `dirtySchemaError` (Task 14) points at this topic, so the pointer now resolves.

- [ ] **Step 6: Commit**

```bash
git add plugins/postgres/migration_index_guard_test.go cmd/cyoda/help/content/cli/migrate.md
git commit -m "test(postgres): guard the concurrent-index migration convention

Migrations 000001-000006 are unchanged; the exposure is the next index
migration. Two clauses: an index on a pre-existing table must be CONCURRENTLY,
and CONCURRENTLY must be alone in its file or the driver's single-Exec send
makes it fail at runtime."
```

---

## Task 16: Remove `internal/cluster/lifecycle`

It is dead **structurally**, not merely unused. `Register` is the only writer to the `active` map and it has zero callers, so `ReapExpired` cannot reap anything — not "does not today", but cannot, for any input. Production reaches exactly three methods: `NewManager` (`app/app.go:440`), `SetTransactionManager` (`:444`) and `ReapExpired` (`:467`) — a constructor, a setter that exists only to serve the reaper, and a loop over a map nothing can populate. `RecordOutcome`, `IsAlive`, `GetOutcome`, `ListByNode` and `Remove` have no callers at all.

It was never used here; there is no moment of disuse to find. The package arrived whole in `d1f6875` (2026-04-14), a root commit with no parents — "Initial import from cyoda-light-go @ `ab90677`" — with all eight methods, a test file, and `app.go` already constructing it, exposing `TxLifecycle()` and running the reaper goroutine, and already with no caller of `Register`. It then became *more* convincing rather than less, which is why the docs are wrong in good faith: `b665800` added the `txMgr` field, `SetTransactionManager`, and the `tm.Rollback(ctx, txID)` loop, moving the rollback outside the mutex "to avoid holding it across network I/O" — careful work on a path that cannot execute.

Even if a transaction were registered, `ReapExpired` calls `tm.Rollback(context.Background(), …)`, which `verifyTenant` rejects on all three in-tree plugins (`plugins/postgres/transaction_manager.go:486`, `plugins/memory/txmanager.go:513`, `plugins/sqlite/txmanager.go:567`) — a background context carries no `UserContext`. The reaper goroutine only starts when `cfg.Cluster.Enabled` (`app/app.go:445`), so single-node never had one at all. Transaction affinity does not use it: routing is driven by a signed token carrying node ID and expiry (`internal/cluster/proxy/http.go`, `internal/cluster/token`) with its own live knob `CYODA_TX_TOKEN_TTL`. When a node dies, its PostgreSQL sessions die with it and the server rolls those transactions back.

The commercial cassandra backend cannot reference it — Go forbids importing another module's `internal/` — and does not: it has its own unrelated `internal/tx.TxLifecycle` enum and its own `CYODA_CASSANDRA_TX_*` timers, and from cyoda-go's root module it imports only `app.Config`, `app.DefaultConfig`, `app.LoadEnvFiles`, `app.New`, `app.ProfileBanner`.

Wiring the reaper is rejected: `tm.Rollback` → `cleanupTx` → `registry.Remove` means a transaction reaped out from under a still-running handler makes the handler's remaining writes fall through to the pool (`resolveRaw` returns `f.pool` on a registry miss) and auto-commit as standalone statements. Partial, non-atomic application is worse than the leak, and making it safe needs a cancel handshake the DB-side ceiling does not.

**Files:**
- Delete: `internal/cluster/lifecycle/manager.go`, `internal/cluster/lifecycle/manager_test.go`
- Modify: `app/app.go` — import `:28`, `txLifecycle` field `:70`, construction/wiring `:440-444`, reaper goroutine `:459-477`, `stopReaper` field `:72` and its close `:887-888`, `TxLifecycle()` `:822`
- Modify: `internal/cluster/config.go:16,17,19`
- Modify: `app/config.go:298,299,301`
- Modify: `internal/cluster/integration_test.go` — `TestEndToEnd_LifecycleTracking` `:103-128` and its import `:11`
- Modify: `cmd/cyoda/help/config_registry.go:59-61`
- Modify: `app/config_registry_binding_test.go:101-103`
- Modify: `cmd/cyoda/help/content/config.md:86-93`
- Modify: `cmd/cyoda/help/config_registry_test.go:22`
- Modify: `scripts/multi-node-docker/start-cluster.sh:418`

**Interfaces:**
- Consumes: nothing.
- Produces: `App.TxLifecycle()` and `cluster.Config.TxTTL` / `.TxReapInterval` / `.OutcomeTTL` cease to exist.

- [ ] **Step 1: Confirm the structural claim before deleting anything**

Run:
```bash
grep -rn 'lifecycle\.' --include='*.go' . | grep -v '_test.go'
grep -rn '\.Register(' internal/cluster/lifecycle/
grep -rn 'TxTTL\|TxReapInterval\|OutcomeTTL' --include='*.go' . 
grep -rn 'CYODA_TX_TTL\|CYODA_TX_REAP_INTERVAL\|CYODA_TX_OUTCOME_TTL' -r . --include='*.go' --include='*.md' --include='*.sh' --include='*.yaml'
```
Expected: `Register` has no caller outside the package's own test; the three config fields are read only by `app.go`'s construction and the reaper. If any of that is untrue, **stop** — the premise for deletion has changed and this is a decision for the maintainer, not a plan step.

- [ ] **Step 2: Delete the package and its wiring**

```bash
git rm -r internal/cluster/lifecycle/
```

In `app/app.go` remove: the import (`:28` — a hard compile break if missed), the `txLifecycle *lifecycle.Manager` field (`:70`), `stopReaper chan struct{}` (`:72`), the construction and `SetTransactionManager` call (`:440-444`), the reaper goroutine and its `stopReaper` channel creation (`:459-477`), the `close(a.stopReaper)` in `Shutdown` (`:887-888`), and `func (a *App) TxLifecycle() *lifecycle.Manager` (`:822`).

Keep `stopSearchReaper` — it is unrelated and live.

In `internal/cluster/config.go` remove `TxTTL`, `TxReapInterval` and `OutcomeTTL` (`:16`, `:17`, `:19`). Keep `ProxyTimeout` (`:18`).

In `app/config.go` remove the three bindings (`:298`, `:299`, `:301`). Keep `ProxyTimeout` at `:300`.

- [ ] **Step 3: Remove the test that will not compile**

`internal/cluster/integration_test.go` — delete `TestEndToEnd_LifecycleTracking` (`:103-128`) and the `lifecycle` import at `:11`. It constructs `lifecycle.NewManager` and asserts `OutcomeRolledBack`, so it is a compile break otherwise, not a behavioural loss.

- [ ] **Step 4: Remove the config surface — in one commit with the code**

The config surface is CI-atomic in both directions: `TestConfig_EnvVarCoverage` (`cmd/cyoda/help/help_test.go:488`) and `TestRootConfigVars_MatchDefaults` (`app/config_registry_binding_test.go:171`) both fail on a partial removal, so all of this lands together.

- `cmd/cyoda/help/config_registry.go:59-61` — the three `// --- tx ---` entries and the section comment.
- `app/config_registry_binding_test.go:101-103` — the matching expectations.
- `cmd/cyoda/help/content/config.md:91-93` — the three bullets. **Rename** the `### Search and transaction internals` heading at `:86` rather than deleting it: `:88-90` are the three `CYODA_SEARCH_*` vars and they stay. `### Search internals` is the accurate replacement.
- `cmd/cyoda/help/config_registry_test.go:22` — drop `"tx": true` from the `validTopic` whitelist; it is dead once the three vars go.
- `scripts/multi-node-docker/start-cluster.sh:418` — delete the `CYODA_TX_TTL: "60s"` line it emits into the generated compose file for every cluster node.

- [ ] **Step 5: Build and test**

Run: `go build ./... && go vet ./... && go test ./... 2>&1 | tail -30`
Expected: PASS. The compile is the strongest signal here — a missed reference cannot hide.

- [ ] **Step 6: Confirm cluster behaviour is unchanged**

Run: `go test ./internal/cluster/... -v && go test ./internal/e2e/... -run MultiNode -v`
Expected: PASS. Nothing is lost: the one capability forgone is a test's ability to assert *which* node holds a transaction (`e2e/parity/multinode/cbd_tx_pinning.go:54`), and that test already asserts the observable signature through version history.

- [ ] **Step 7: Commit**

```bash
git add -u && git add app/ internal/cluster/ cmd/cyoda/help/ scripts/
git commit -m "refactor(cluster): remove the transaction lifecycle manager

Register is the only writer to the active map and has zero callers, so
ReapExpired cannot reap anything — not \"does not today\", but cannot, for any
input. Wiring it is rejected: a transaction reaped out from under a running
handler makes its remaining writes auto-commit as standalone statements, which
is worse than the leak. The DB-side idle ceiling is the backstop instead."
```

Do not use `git add -A`: `go.work` is tracked in a CI-safe form and the local SPI `use` line must stay uncommitted.

---

## Task 17: Correct the claims in the tree that describe the reaper as working

These ship today and describe a capability that has never existed. A playbook that instructs an operator to rely on a reaper that does not run is worse than no playbook.

**Files:**
- Modify: `docs/PRD.md:319`, `:344-346`
- Modify: `docs/CONCURRENCY.md:63`, `:105`
- Modify: `docs/analysis/failure-modes/…-playbook.md:59`
- Modify: `e2e/parity/multinode/cbd_tx_pinning.go:54`

**Interfaces:**
- Consumes: Task 16's deletion; Task 9's ceilings (the replacement mechanism to describe).
- Produces: nothing.

- [ ] **Step 1: Locate every remaining claim**

Run:
```bash
grep -rn -i 'reaper\|TTL reaper\|lifecycle manager\|CYODA_TX_TTL\|CYODA_TX_REAP_INTERVAL\|CYODA_TX_OUTCOME_TTL' docs/ e2e/ README.md CONTRIBUTING.md
```
Expected: the sites listed below, plus `docs/ARCHITECTURE.md` (Task 18) and `docs/plans/` (historical, not touched).

- [ ] **Step 2: Rewrite the PRD**

- `docs/PRD.md:344-346` — the `### Transaction Timeout and Reaper` heading and "A background reaper goroutine periodically scans for expired transactions and rolls them back." Replace with a description of what actually bounds a transaction: the application-side deferred rollback on every exit path including panics, and underneath it PostgreSQL's `idle_in_transaction_session_timeout` and `statement_timeout`. Rename the heading to `### Transaction Timeouts`.
- `docs/PRD.md:319` — the state diagram's `ROLLBACK ◄──── timeout (TTL reaper)` edge. Relabel to the real mechanism (`ROLLBACK ◄──── deferred release / DB ceiling`) rather than deleting the edge; a transaction really can end that way.

- [ ] **Step 3: Rewrite `docs/CONCURRENCY.md`**

`:63` and `:105`. The latter cites a path that stops existing, so it cannot be softened — describe the guard and the ceiling, or delete the sentence.

- [ ] **Step 4: Rewrite the operational playbook**

`docs/analysis/failure-modes/…-playbook.md:59`. This is the actionable companion to the analysis document, so it must be correct even though its sibling is historical: replace the reaper instruction with the real recovery path — check the pool's connection count, confirm `idle_in_transaction_session_timeout` is set, and note that a node that has recovered a panic reports `503 DOWN` and is expected to be restarted by its supervisor.

- [ ] **Step 5: Fix the parity comment**

`e2e/parity/multinode/cbd_tx_pinning.go:54` — "not yet wired into the runtime". Comment-only, no compile dependency. Replace with a statement of what the test actually asserts (the observable pinning signature through version history) and why no node-identity handle exists.

- [ ] **Step 6: Record the disposition of the failure-mode analysis**

`docs/analysis/failure-modes/2026-06-29-operational-failure-mode-analysis.md:288,311` names `lifecycle.Manager` as remediation R1 for this exact issue. That directory is a historical record like `docs/plans/`, so it is **not** rewritten. The disposition is recorded in the spec so a future reader does not re-propose the wired reaper — verify the spec's §4.3 paragraph is present and accurate, and leave the analysis document alone.

- [ ] **Step 7: Verify no stale references remain**

Run:
```bash
grep -rn -i 'reaper' docs/ e2e/ README.md | grep -v 'docs/plans/' | grep -v 'search' | grep -v 'ARCHITECTURE.md'
```
Expected: no transaction-reaper hits. Search-snapshot reaper references are unrelated and stay.

- [ ] **Step 8: Commit**

```bash
git add docs/ e2e/
git commit -m "docs: correct the claims describing a transaction reaper that never ran"
```

---

## Task 18: `docs/ARCHITECTURE.md` — full audit

The removal touches six places in this document, which is the wrong way to treat it. `ARCHITECTURE.md` is the reference a reader trusts to know what the system does; a stale claim there is worse than no claim, because it is acted on. So the whole document is audited, not just the lifecycle references.

This runs as its **own commit**, separate from Task 16, so the mechanical deletion stays reviewable on its own.

**Scope.** All 14 sections, claim by claim, against the code. A claim that cannot be verified is **deleted**, not softened — an unverifiable assertion in a reference document is a liability, and the project's own stance is to fail closed.

**Editing rule: current state only, present tense.** Describe the system as it is. No "previously X, now Y", no "this was changed", no migration notes. The delta belongs in `CHANGELOG.md`; the state belongs here. This applies to the lifecycle removal specifically — §3.4 and its references come out cleanly, with the DB-side ceilings described on their own terms rather than as a replacement for something.

**Files:**
- Modify: `docs/ARCHITECTURE.md` (1755 lines, 14 sections)

**Interfaces:**
- Consumes: Tasks 9–16.
- Produces: nothing.

- [ ] **Step 1: Fix what is already known wrong**

Each was verified while writing the spec.

- **§3.4 "Transaction Lifecycle Manager" (`:365-380`)** and the package-tree entry (`:123`, `lifecycle/ Transaction lifecycle manager (TTL, reaper, outcomes)`) — describe a component that no longer exists. Delete both. Removing §3.4 orphans the unrelated COMMIT_BEFORE_DISPATCH paragraph at `:382` into §3.3 — rehome it, do not leave it dangling — and breaks the live cross-references at `:566` and `:742`, which need repointing, not just removal.
- **`:1425-1426`, `:1428`** — the three env vars presented as live knobs. Delete. `:1427` is `CYODA_PROXY_TIMEOUT` and stays. Add the five new `CYODA_POSTGRES_*` ceiling vars in their place.
- **`:1650-1651`** — "Workflow chains that exceed TTL are reaped. Long-running processors must complete within this window", plus the companion row advising that `idle_in_transaction_session_timeout` should *exceed* the TTL. Both are false today and inverted by the ceilings: the DB-side limit is now the authority, and a processor's `responseTimeoutMs` must fit *under* it. Rewrite both. `:1652-1655` are unrelated rows and stay.
- **DD-2 (`:1569`)** — "The lifecycle manager provides TTL, registry, and observability" is false. Its other premise, "PostgreSQL rolls back the transaction automatically via idle timeout", is *also* false today (no such timeout is set anywhere) and becomes true for the first time under this change. The decision itself — fencing tokens not required — stands on the `pgx.Tx` single-owner property alone; rewrite the rationale to say so.
- **§12 "Planned Features (Not Yet Implemented)" (`:1529`)** — at least three of six rows ship today: batch `SaveResults` with `pgx.CopyFrom` (`search_store.go:113`), the `cyoda-go-spi/spitest/` conformance harness (present in the pinned SPI), and multi-node E2E tests with proxy routing (`e2e/parity/multinode/`). Re-verify every row. The section's framing — "items carried forward from the `cyoda-light-go` predecessor repository" — is exactly the historical narrative this document must not carry, and goes; a list of what the system does *not* do is current-state information and stays.

- [ ] **Step 2: Audit the remaining sections claim by claim**

Work through §1–§14 in order. For each factual claim, find the code that backs it. Verified → keep, in present tense. Contradicted → correct. Unverifiable → **delete**.

§13 "Design Decisions Log" **stays**: a design-decisions log records why the system is shaped as it is, which is current-state rationale, not a change narrative. What it must not do is justify a decision with a component that does not exist — hence the DD-2 rewrite.

Pay particular attention to §3 (Transaction Model) and §9 (Configuration Reference), which this change alters most, and to §4 (Multi-Node Routing) — cluster mode is the primary operating target, so a stale claim there is costly. `CYODA_CLUSTER_ENABLED=false` being the default is onboarding convenience, not a statement that cluster features are secondary; do not let the audit introduce language implying otherwise.

- [ ] **Step 3: Add the ceilings on their own terms**

In §3, describe what now bounds a transaction, written as current state with no reference to what it replaced:

- Every write flow releases its transaction on every exit path, including a panic, via a deferred scope; the workflow engine does the same for the segments it opens itself.
- PostgreSQL bounds any statement at `CYODA_POSTGRES_STATEMENT_TIMEOUT` and any idle-in-transaction connection at `CYODA_POSTGRES_IDLE_IN_TX_TIMEOUT`.
- A write waits at most `CYODA_POSTGRES_ACQUIRE_TIMEOUT` for a pooled connection and then fails with `503 STORAGE_UNAVAILABLE`, retryable.
- Async search carries its own, higher, statement ceiling.
- A processor's `responseTimeoutMs` must fit under the idle ceiling. It is currently uncapped, so a workflow may configure a callout longer than the ceiling and have its transaction aborted; that surfaces as `503 STORAGE_UNAVAILABLE`.

- [ ] **Step 4: Verify every code reference the document still makes**

Run:
```bash
grep -oE '`?[a-z_/]+\.(go|sql|yaml|md)`?(:[0-9]+)?' docs/ARCHITECTURE.md | sort -u
```
Check each path still exists; check each cited line still says what the document claims. A path that has moved gets updated; a claim that cannot be located gets deleted.

- [ ] **Step 5: Verify the table of contents and internal links**

`:13-36`. Section numbering shifts when §3.4 is removed. Confirm every ToC entry resolves and every internal cross-reference points at a section that exists.

Run: `grep -n '](#' docs/ARCHITECTURE.md` and verify each anchor.

- [ ] **Step 6: Commit separately**

```bash
git add docs/ARCHITECTURE.md
git commit -m "docs(architecture): audit the whole document against the code

Present tense, current state only. Unverifiable claims deleted rather than
softened. Includes the transaction lifecycle manager's section and references,
the three removed env vars, the inverted idle-timeout guidance, DD-2's
rationale, and the planned-features section, three of whose rows ship today."
```

---

## Task 19: Plugin default-value parity test

Name-level parity is already enforced: `TestConfigAll_Complete` (`cmd/cyoda/help/config_registry_test.go:73`) scans `cmd`, `app`, `plugins` and `internal` for `CYODA_*` and diffs against the merged registry, so adding a var to `parseConfig` alone fails CI. The real gap is **default-value** parity — `plugin.go:19`'s `"25"` and `config.go:36`'s `25` are unbound literals, and root vars get `TestRootConfigVars_MatchDefaults` (`app/config_registry_binding_test.go:171`) while plugin vars get no equivalent. Task 9 added five vars with documented defaults; without this, a documented default that drifts from the code misinforms rather than fails.

**Files:**
- Create: `plugins/postgres/config_defaults_test.go`

**Interfaces:**
- Consumes: `ConfigVars()`, `parseConfig` (Task 9).
- Produces: nothing.

- [ ] **Step 1: Write the failing test**

```go
package postgres

// TestConfigVars_DefaultsMatchParseConfig asserts every default advertised in
// ConfigVars() is the value parseConfig actually applies when the var is unset.
//
// Name-level parity is already enforced repo-wide; this is the value-level
// counterpart root vars get from TestRootConfigVars_MatchDefaults and plugin
// vars did not. A documented default that drifts from the code misinforms an
// operator who is reading it precisely because they cannot see the code.
func TestConfigVars_DefaultsMatchParseConfig(t *testing.T) {
	cfg, err := parseConfig(func(k string) string {
		if k == "CYODA_POSTGRES_URL" {
			return "postgres://u:p@localhost:5432/db" // required; not a defaulted var
		}
		return ""
	})
	if err != nil {
		t.Fatalf("parseConfig with only the required var set: %v", err)
	}

	actual := map[string]string{
		"CYODA_POSTGRES_MAX_CONNS":                strconv.Itoa(int(cfg.MaxConns)),
		"CYODA_POSTGRES_MIN_CONNS":                strconv.Itoa(int(cfg.MinConns)),
		"CYODA_POSTGRES_MAX_CONN_IDLE_TIME":       cfg.MaxConnIdleTime.String(),
		"CYODA_POSTGRES_AUTO_MIGRATE":             strconv.FormatBool(cfg.AutoMigrate),
		"CYODA_SCHEMA_SAVEPOINT_INTERVAL":         strconv.Itoa(cfg.SchemaSavepointInterval),
		"CYODA_POSTGRES_STATEMENT_TIMEOUT":        cfg.StatementTimeout.String(),
		"CYODA_POSTGRES_IDLE_IN_TX_TIMEOUT":       cfg.IdleInTxTimeout.String(),
		"CYODA_POSTGRES_ACQUIRE_TIMEOUT":          cfg.AcquireTimeout.String(),
		"CYODA_POSTGRES_MIGRATE_LOCK_TIMEOUT":     cfg.MigrateLockTimeout.String(),
		"CYODA_POSTGRES_SEARCH_STATEMENT_TIMEOUT": cfg.SearchStatementTimeout.String(),
	}

	for _, v := range (&plugin{}).ConfigVars() {
		if v.Required {
			continue
		}
		got, ok := actual[v.Name]
		if !ok {
			t.Errorf("%s is advertised in ConfigVars() but this test does not cover its default; add it", v.Name)
			continue
		}
		if !sameDefault(v.Default, got) {
			t.Errorf("%s: ConfigVars() says %q, parseConfig applies %q", v.Name, v.Default, got)
		}
	}
}
```

`sameDefault` normalises the two spellings a duration can take (`"5m"` in the registry vs Go's `"5m0s"`) by parsing both as durations when both parse, and comparing strings otherwise. It must **not** normalise so aggressively that a real mismatch (`5m` vs `50m`) passes.

Add a companion asserting the covered set is complete in the other direction — every non-required entry in `ConfigVars()` appears in `actual` — so a var added later without a default assertion fails rather than being silently skipped. (The `!ok` branch above already does this; verify it by adding a var locally and watching it fail, then reverting.)

- [ ] **Step 2: Run it and confirm it either fails or reveals a real drift**

Run: `cd plugins/postgres && go test ./... -run TestConfigVars_Defaults -v`
Expected: FAIL initially (`undefined: sameDefault`). Once implemented, if it reports a genuine pre-existing mismatch, fix the mismatch — that is exactly what the test is for (Gate 6).

- [ ] **Step 3: Confirm it passes**

Run: `cd plugins/postgres && go test ./... -v`
Expected: PASS.

- [ ] **Step 4: Apply the same to the other plugins**

`plugins/memory` and `plugins/sqlite` each have their own `ConfigVars()` and their own `go.mod`. Add the equivalent test to each so the gap closes repo-wide rather than only where this change touched.

Run: `make test-all`
Expected: PASS across root and all three plugin submodules. Docker required for the postgres testcontainers.

- [ ] **Step 5: Commit**

```bash
git add plugins/
git commit -m "test(plugins): assert advertised config defaults match the code

Name-level parity was already enforced; value-level parity existed for root vars
only. A documented default that drifts now fails rather than misinforms."
```

---

## Task 20: `CHANGELOG.md` and `COMPATIBILITY.md`

**Files:**
- Modify: `CHANGELOG.md`
- Modify: `COMPATIBILITY.md`

**Interfaces:**
- Consumes: every preceding task.
- Produces: nothing.

- [ ] **Step 1: Write the `### Breaking` section**

Three env vars are removed and three ceilings that did not exist now apply by default. Per `COMPATIBILITY.md`, that section — not the version digit — is what consumers are told to read. v0.8.4 remains a PATCH: the HTTP/wire API is unchanged.

```markdown
## [Unreleased]

### Breaking

- Removed `CYODA_TX_TTL`, `CYODA_TX_REAP_INTERVAL` and `CYODA_TX_OUTCOME_TTL`. They configured a transaction reaper that never ran — nothing ever registered a transaction with it — and are rejected as unknown configuration if set. Transaction lifetime is now bounded by a deferred rollback on every exit path plus the PostgreSQL ceilings below.
- PostgreSQL connections now carry `statement_timeout` and `idle_in_transaction_session_timeout`, both defaulting to `5m`. A statement or an idle-in-transaction connection that exceeds its ceiling is aborted by the server. Set `CYODA_POSTGRES_STATEMENT_TIMEOUT=0` / `CYODA_POSTGRES_IDLE_IN_TX_TIMEOUT=0` to disable either. A workflow processor whose `responseTimeoutMs` exceeds the idle ceiling will have its transaction aborted; the default `responseTimeoutMs` of 30s sits well under it.
- Entity writes now wait at most `CYODA_POSTGRES_ACQUIRE_TIMEOUT` (default `10s`) for a pooled connection and then fail with `503 STORAGE_UNAVAILABLE` instead of queueing.
- On startup with `CYODA_POSTGRES_AUTO_MIGRATE=true`, migrations run before the schema-compatibility check. A node booting alongside a peer's in-flight migration now waits for it rather than exiting with a dirty-schema error.

### Added

- `STORAGE_UNAVAILABLE` (`503`, retryable), declared on the entity write operations. `cyoda help errors.STORAGE_UNAVAILABLE`.
- `CYODA_POSTGRES_STATEMENT_TIMEOUT` (`5m`), `CYODA_POSTGRES_IDLE_IN_TX_TIMEOUT` (`5m`), `CYODA_POSTGRES_ACQUIRE_TIMEOUT` (`10s`), `CYODA_POSTGRES_MIGRATE_LOCK_TIMEOUT` (`5m`), `CYODA_POSTGRES_SEARCH_STATEMENT_TIMEOUT` (`30m`). `cyoda help config.database`.
- Panic recovery on the gRPC server, on every HTTP route (previously only the catch-all), and on the async-search goroutine. A recovered panic marks the node unhealthy, which reports `503 DOWN` and is expected to be restarted by its supervisor.

### Fixed

- Entity write flows release their transaction on every exit path, including a panic. Previously a panic between begin and commit left the transaction neither committed nor rolled back, with its pooled connection never returned.
- The workflow engine releases segments it opens itself. A `FUNCTION` criterion callout failing mid-cascade left the post-segment transaction open with no panic involved.
- A criterion evaluated after a `COMMIT_BEFORE_DISPATCH` segment receives that segment's transaction id rather than the committed cascade-entry id, so a compute node's callback can join it.
- A collection update whose engine conflicted after segmenting aborts the batch instead of isolating the item and writing every later item into an already-committed transaction.
- `statement_timeout` (SQLSTATE `57014`) and `idle_in_transaction_session_timeout` (SQLSTATE `25P03`) are classified rather than surfacing as unexplained errors, and a `25P03` abort releases the per-transaction bookkeeping the killed session left behind.
```

No issue numbers anywhere in this file's shipped text.

- [ ] **Step 2: Update `COMPATIBILITY.md`**

Confirm what actually changed against the matrix's axes: `cyoda-go` × `cyoda-go-spi` × in-tree plugins × chart × out-of-tree plugins.

This change makes **no** `cyoda-go-spi` change — deliberately. The `StorageUnavailable` marker is an interface matched with `errors.As`, so a storage backend opts in by returning the shape, with no SPI tag and no coordinated cross-repo release. Record that explicitly: the commercial backend continues to work unchanged and can adopt the marker whenever it chooses.

If the SPI pin, chart `version:`/`appVersion:`, or out-of-tree pin guidance are untouched, say so in the entry rather than leaving the reader to infer it.

- [ ] **Step 3: Check the rest of Gate 4**

Run:
```bash
grep -rn 'CYODA_TX_TTL\|CYODA_TX_REAP_INTERVAL\|CYODA_TX_OUTCOME_TTL' README.md CONTRIBUTING.md COMPATIBILITY.md cmd/cyoda/help/content/ docs/ scripts/ | grep -v 'docs/plans/' | grep -v CHANGELOG
```
Expected: no hits outside `CHANGELOG.md`, which documents the removal.

Confirm `README.md`'s configuration reference lists the five new vars if it enumerates `CYODA_POSTGRES_*` at all.

- [ ] **Step 4: Commit**

```bash
git add CHANGELOG.md COMPATIBILITY.md README.md
git commit -m "docs: record the transaction lifecycle changes and the breaking config removals"
```

---

## Final verification

Run before opening the PR, in this order. Do not claim completion until every one is green and you have seen the output.

- [ ] **Step 1: Vet and build**

```bash
go build ./... && go vet ./...
(cd plugins/memory && go vet ./...) && (cd plugins/sqlite && go vet ./...) && (cd plugins/postgres && go vet ./...)
```

- [ ] **Step 2: Full test suite, root plus every plugin submodule**

`go test ./...` from the root does **not** cross module boundaries.

```bash
make test-all
```
Expected: PASS. Docker required.

- [ ] **Step 3: Coverage-matrix walk**

Confirm each row has a test that exists and passes. Run each named test and record the result; a missing cell blocks merge unless waived with a one-line reason.

| Row | Scenario | Task |
|---|---|---|
| 1 | Panic in an owned write path rolls back; pool returns to baseline | 4 |
| 2 | Repeated panics beyond pool size leave the node serving | 4 |
| 3 | Panic in a joined callback does not roll back the owner's tx | 3, 4 |
| 4 | Panic after segmentation rolls back TX_post, not the entry tx | 1 (unit), 4 (postgres e2e) |
| 4a | Non-panic engine error after segmentation rolls back TX_post | 1 (unit), 4 (postgres e2e) |
| 4b | Every `executeCommitBeforeDispatch` failure path rolls its segment back | 1 |
| 4c | Criterion after a CBD segment carries the current segment's txID | 2 |
| 5 | Committed-transaction behaviour unchanged | existing suites |
| 5a | Ordinary error paths still roll back, one case per converted flow | 4 |
| 6 | Panicking write on memory/sqlite releases its tx state | 4 |
| 7 | gRPC handler panic recovered; process survives; tx rolled back | 6 |
| 7a | Peer scheduler-RPC panic recovered; fire tx rolled back | 7 |
| 8 | `Release` holds the per-tx gate while rolling back | 3 |
| 8a | `Release` on a cancelled request context still rolls back | 3, 4 |
| 8b | Joined call that segments rolls the engine-opened segment back | 3 |
| 8c | Engine-side guard on memory and sqlite | 1 |
| 9 | Idle-in-tx beyond the ceiling → 503 `STORAGE_UNAVAILABLE` (HTTP + gRPC) | 11 |
| 10 | Saturated pool → write returns 503 within the acquire timeout (HTTP + gRPC) | 10 |
| 11 | Caller-cancelled request not mislabelled `STORAGE_UNAVAILABLE` | 10 |
| 11a | GUC values render as bare ms integers, never `5m` | 9 |
| 11b | Deep cascade exceeding the idle ceiling in total still commits | 9 |
| 11c | Async search aborted by its ceiling records FAILED, non-revealing | 12 |
| 11e | Transaction usable past the acquire timeout | 10 |
| 11f | `25P03` clears registry/tenants/origins/txStates | 11 |
| 11g | Sub-millisecond ceilings rejected at parse time | 9 |
| 11h | DSN-only ceiling survives; both → WARN and env wins | 9 |
| 12 | `57014` → 500 with a ticket, cause named in the log | 11 |
| 13 | `lock_timeout` aborts a `pg_advisory_lock` wait | 13 |
| 13a | Dirty schema reports the actionable message, not bare `ErrDirty` | 14 |
| 14 | Migration connection inherits neither ceiling from the app pool | 13 |
| 15 | Fault-injected interleave proceeds instead of reporting dirty | 14 |
| 15a | `cyoda migrate` concurrent with a node boot | 14 |
| 16 | Genuinely dirty schema still fails fast | 14 |
| 16a | Database newer than the binary still refuses to start | 14 |
| 17 | Single-node install migrates itself | 14 |
| 18 | Guard test rejects a new non-concurrent index on a hot table | 15 |
| 19 | `STORAGE_UNAVAILABLE` declared in OpenAPI on every write op | 8 |

- [ ] **Step 4: Race detector — once, here, not during iteration**

```bash
make race
```
CI runs the identical target (every package except `internal/e2e`), so a local pass strongly predicts a CI pass. If `internal/e2e` race coverage is wanted for this change specifically: `go test -race -timeout=20m ./internal/e2e/...`.

- [ ] **Step 5: Gate 4 sweep**

```bash
make todos
grep -rn '#4[0-9][0-9]\|#3[0-9][0-9]' --include='*.go' --include='*.md' \
  internal/ plugins/ app/ cmd/cyoda/help/content/ api/openapi.yaml
```
Expected: no new TODOs; no issue numbers in any shipped artefact. Matches inside `docs/superpowers/` and `docs/plans/` are fine.

- [ ] **Step 6: Verify before claiming done**

Use `superpowers:verification-before-completion`. Evidence before assertions: paste the actual command output, do not assert from memory.

- [ ] **Step 7: Review gates**

Both need a **fresh-context** reviewer, dispatched as a subagent — this is a standing request from the maintainer, not something to ask about again. If anything prevents dispatching one, say so and stop; never drop a gate silently.

1. `superpowers:requesting-code-review`
2. `antigravity-bundle-security-developer:cc-skill-security-review` — with particular attention to Gate 3: the `57014` and async-search messages must not leak internals, the recovery paths must not put a panic value or stack into a response, and the WARN log for a DSN-supplied ceiling must not print a connection string.

Then `superpowers:finishing-a-development-branch`.
