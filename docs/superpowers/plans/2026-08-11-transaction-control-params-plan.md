# Transaction-Control Params (#379) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Honor `transactionTimeoutMillis` (→ reachable 408, "408 ⇔ nothing committed"), `transactionSize` (version-guarded batched deletes), and re-added search `timeoutMillis`, uniformly across HTTP + gRPC and all backends.

**Architecture:** A feature-owned deadline is attached at the handler seam and classified back to 408 only from its own error chain; every commit (final, CBD segment, `newMessage` Save) is shielded via a `CommitContext` (WithoutCancel + budget) with a pre-commit expiry check, making the 408 guarantee structural. Batched deletes resolve ids + versions once in a resolution transaction, then commit per batch with a per-id version guard. Joined (tx-token) requests reject every transaction-control param with 400. Spec: `docs/superpowers/specs/2026-08-11-transaction-control-params-design.md` — read it first; its D-numbers are referenced throughout.

**Tech Stack:** Go 1.26, `oapi-codegen` (`api/generated.go` via `go generate ./api`), `go-jsonschema` (`scripts/generate-events.sh`), testcontainers e2e.

## Global Constraints

- v0.8.4 is a PATCH: absent param ⇒ behavior unchanged (spec's two declared alignments excepted).
- No `default:` anywhere for these params — spec defaults are being REMOVED, never implemented.
- No SPI change. No new issue IDs in code/errors/help (PR bodies/commits/specs only).
- TDD: every task RED → GREEN. Scoped tests during iteration (`go test ./internal/...`); full suite + `make race` + per-plugin tests only at final verification (Task 23).
- Plugins `plugins/memory|sqlite` are separate Go modules — run their tests from inside the plugin dir.
- New error codes need `cmd/cyoda/help/content/errors/<CODE>.md` (TestErrCode_Parity enforces bijection).
- Commit after every task; message style `feat(scope): …` / `fix(scope): …` / `test(scope): …` / `docs(scope): …`.
- Error-code prose: 4xx carries full domain detail; never leak internals. `log/slog` only.

## Stream map (parallelizable)

- **Stream A (foundation, serial):** T1 → T2 → T3 → T4
- **Stream B (HTTP write timeout):** T5 → T6 → T7 (needs A)
- **Stream C (batched deletes):** T8, T9 → T10 (T8 independent; T9/T10 need T2 for validation helpers only)
- **Stream D (search timeout):** T11 → T12, T13 (needs T1, T2)
- **Stream E (gRPC):** T14, T15, T16 (needs A; T15 also needs T10)
- **Stream F (independent fixes/tests):** T17, T18 (no deps)
- **Verification tail (serial):** T19 → T20 → T21 → T22 → T23

---

### Task 1: Error codes `TRANSACTION_TIMEOUT` + `SEARCH_TIMEOUT`

**Files:**
- Modify: `internal/common/error_codes.go` (transaction block `:52-58`, search block `:74-84`)
- Create: `cmd/cyoda/help/content/errors/TRANSACTION_TIMEOUT.md`, `cmd/cyoda/help/content/errors/SEARCH_TIMEOUT.md`
- Modify: `cmd/cyoda/help/content/errors.md` (ERROR CODE INDEX, `:59-115`)

**Interfaces:**
- Produces: `common.ErrCodeTransactionTimeout = "TRANSACTION_TIMEOUT"`, `common.ErrCodeSearchTimeout = "SEARCH_TIMEOUT"` — used by T2, T5, T7, T11, T14, T16.

- [ ] **Step 1: RED** — run `go test ./cmd/cyoda/help/ -run TestErrCode_Parity -v` after adding ONLY the two constants (`ErrCodeTransactionTimeout = "TRANSACTION_TIMEOUT"` in the `:52` transaction const block; `ErrCodeSearchTimeout = "SEARCH_TIMEOUT"` in the `:74` search block). Expected: FAIL — missing `errors/<CODE>.md` topics.
- [ ] **Step 2: GREEN** — add the two topic files. Copy the front-matter/section shape of `cmd/cyoda/help/content/errors/TRANSACTION_EXPIRED.md` exactly (`topic: errors.TRANSACTION_TIMEOUT`, `title`, `stability`, `see_also`, then `# errors.<CODE>` / `## NAME` / `## SYNOPSIS` / `## DESCRIPTION` / `## SEE ALSO`). SYNOPSIS: `HTTP status: 408` / `Retryable: yes`. DESCRIPTION for TRANSACTION_TIMEOUT (compact, per spec D2/D3): the client-supplied `transactionTimeoutMillis` expired before the first commit; nothing was committed; on multi-commit operations (chunked collections, commit-before-dispatch workflows) the timeout applies only until the first commit — afterwards failures surface through the per-chunk contract. For SEARCH_TIMEOUT: client-supplied `timeoutMillis` expired before the search result set was collected; no partial results are returned. `see_also` must resolve to real topics (e.g. `errors.TRANSACTION_EXPIRED`, `crud`; verify with the tests below).
- [ ] **Step 3: Verify** — `go test ./cmd/cyoda/help/ -v` (runs parity + path-mismatch + see-also + markdown linter). Expected: PASS. Also add both codes to the `## ERROR CODE INDEX` in `errors.md` (format: `` - `errors.TRANSACTION_TIMEOUT` — `408` — retryable — client-requested transaction timeout expired before commit``).
- [ ] **Step 4: Commit** — `feat(errors): add TRANSACTION_TIMEOUT and SEARCH_TIMEOUT codes`

---

### Task 2: `internal/common` request-timeout helpers + `CommitContext`

**Files:**
- Create: `internal/common/reqtimeout.go`, `internal/common/reqtimeout_test.go`
- Modify: `internal/common/rollback.go` (add `CommitContext` beside `RollbackContext`)

**Interfaces:**
- Produces (exact signatures — all later tasks consume these):

```go
// ValidateRequestTimeoutMillis returns a 400 AppError unless 1 <= millis and
// time.Duration(millis)*time.Millisecond does not overflow.
func ValidateRequestTimeoutMillis(millis int64) *AppError

// WithRequestTimeout attaches a feature-owned deadline. Caller must have validated millis.
func WithRequestTimeout(ctx context.Context, millis int64) (context.Context, context.CancelFunc)

// HasRequestTimeout reports whether ctx carries a feature-attached deadline marker.
func HasRequestTimeout(ctx context.Context) bool

// ClassifyRequestTimeout returns Operational(408, code, …).AsRetryable() iff
// ctx carries the feature marker AND err's chain — unwrapping *AppError causes —
// contains context.DeadlineExceeded. Otherwise nil (caller falls through to its
// existing classification). Never inspects ctx.Err() state alone (spec D2 pinned rule).
func ClassifyRequestTimeout(ctx context.Context, err error, code string) *AppError

// CommitContext derives the context a commit must run on: caller's values,
// no cancellation, bounded budget (commitBudget = 30 * time.Second).
func CommitContext(ctx context.Context) (context.Context, context.CancelFunc)
```

- [ ] **Step 1: RED** — write `internal/common/reqtimeout_test.go`:

```go
package common

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"testing"
	"time"
)

func TestValidateRequestTimeoutMillis(t *testing.T) {
	for _, tc := range []struct {
		millis int64
		wantOK bool
	}{{1, true}, {60000, true}, {0, false}, {-5, false}, {math.MaxInt64 / int64(time.Millisecond) + 1, false}} {
		err := ValidateRequestTimeoutMillis(tc.millis)
		if tc.wantOK && err != nil {
			t.Errorf("millis=%d: unexpected %v", tc.millis, err)
		}
		if !tc.wantOK {
			if err == nil {
				t.Fatalf("millis=%d: want 400", tc.millis)
			}
			if err.Status != http.StatusBadRequest || err.Code != ErrCodeBadRequest {
				t.Errorf("millis=%d: got status=%d code=%s", tc.millis, err.Status, err.Code)
			}
		}
	}
}

func TestClassifyRequestTimeout_OursExpired(t *testing.T) {
	ctx, cancel := WithRequestTimeout(context.Background(), 1)
	defer cancel()
	<-ctx.Done()
	err := fmt.Errorf("failed to commit transaction: %w", ctx.Err())
	appErr := ClassifyRequestTimeout(ctx, err, ErrCodeTransactionTimeout)
	if appErr == nil {
		t.Fatal("want 408, got nil")
	}
	if appErr.Status != http.StatusRequestTimeout || appErr.Code != ErrCodeTransactionTimeout || !appErr.Retryable {
		t.Errorf("got %+v", appErr)
	}
}

func TestClassifyRequestTimeout_UnwrapsAppErrorCause(t *testing.T) {
	ctx, cancel := WithRequestTimeout(context.Background(), 1)
	defer cancel()
	<-ctx.Done()
	wrapped := Internal("failed to commit transaction", fmt.Errorf("segment: %w", context.DeadlineExceeded))
	if got := ClassifyRequestTimeout(ctx, wrapped, ErrCodeTransactionTimeout); got == nil {
		t.Fatal("must unwrap AppError cause chain")
	}
}

func TestClassifyRequestTimeout_NeverFromCtxStateAlone(t *testing.T) {
	ctx, cancel := WithRequestTimeout(context.Background(), 1)
	defer cancel()
	<-ctx.Done()
	// Deadline HAS expired, but the error is an unrelated conflict — must pass through (nil).
	if got := ClassifyRequestTimeout(ctx, errors.New("transaction conflict"), ErrCodeTransactionTimeout); got != nil {
		t.Fatalf("ctx-state-alone classification forbidden, got %v", got)
	}
}

func TestClassifyRequestTimeout_NoMarkerNo408(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Nanosecond)
	defer cancel()
	<-ctx.Done()
	if got := ClassifyRequestTimeout(ctx, ctx.Err(), ErrCodeTransactionTimeout); got != nil {
		t.Fatalf("foreign deadline must not classify as 408, got %v", got)
	}
}

func TestClassifyRequestTimeout_CanceledIsNot408(t *testing.T) {
	ctx, cancel := WithRequestTimeout(context.Background(), 60000)
	cancel() // client disconnect, not deadline
	if got := ClassifyRequestTimeout(ctx, ctx.Err(), ErrCodeTransactionTimeout); got != nil {
		t.Fatalf("context.Canceled must not be 408, got %v", got)
	}
}

func TestCommitContext_DetachedAndBudgeted(t *testing.T) {
	parent, cancel := WithRequestTimeout(context.Background(), 1)
	defer cancel()
	<-parent.Done()
	cctx, ccancel := CommitContext(parent)
	defer ccancel()
	if cctx.Err() != nil {
		t.Fatal("commit ctx must survive an expired parent")
	}
	if _, ok := cctx.Deadline(); !ok {
		t.Fatal("commit ctx must carry its own budget")
	}
}
```

Run: `go test ./internal/common/ -run 'TestValidateRequestTimeout|TestClassifyRequestTimeout|TestCommitContext' -v` → FAIL (undefined functions).
- [ ] **Step 2: GREEN** — create `internal/common/reqtimeout.go`:

```go
package common

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"time"
)

// reqTimeoutKey marks a context whose deadline was attached from a
// client-supplied transactionTimeoutMillis / timeoutMillis param. The marker is
// what lets ClassifyRequestTimeout distinguish OUR deadline from every other
// deadline source (postgres statement_timeout stays 500, dispatch time.After
// stays 503) — spec D8.
type reqTimeoutKey struct{}

const maxRequestTimeoutMillis = math.MaxInt64 / int64(time.Millisecond)

func ValidateRequestTimeoutMillis(millis int64) *AppError {
	if millis < 1 || millis > maxRequestTimeoutMillis {
		return Operational(http.StatusBadRequest, ErrCodeBadRequest,
			fmt.Sprintf("timeout must be a positive number of milliseconds not exceeding %d", maxRequestTimeoutMillis))
	}
	return nil
}

func WithRequestTimeout(ctx context.Context, millis int64) (context.Context, context.CancelFunc) {
	ctx = context.WithValue(ctx, reqTimeoutKey{}, true)
	return context.WithTimeout(ctx, time.Duration(millis)*time.Millisecond)
}

func HasRequestTimeout(ctx context.Context) bool {
	return ctx.Value(reqTimeoutKey{}) != nil
}

// ClassifyRequestTimeout maps err to Operational(408, code).AsRetryable() only
// when the feature's own deadline is in err's chain (spec D2 pinned rule):
// the marker must be on ctx AND the chain — unwrapping *AppError causes —
// must contain context.DeadlineExceeded. context.Canceled never matches.
func ClassifyRequestTimeout(ctx context.Context, err error, code string) *AppError {
	if err == nil || !HasRequestTimeout(ctx) {
		return nil
	}
	if !chainHasDeadlineExceeded(err) {
		return nil
	}
	return Operational(http.StatusRequestTimeout, code,
		"operation exceeded the client-requested timeout before completing; nothing was committed").
		AsRetryable()
}

func chainHasDeadlineExceeded(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var appErr *AppError
	if errors.As(err, &appErr) && appErr.Err != nil {
		return errors.Is(appErr.Err, context.DeadlineExceeded)
	}
	return false
}
```

Note: check whether `*AppError` implements `Unwrap()` (look in `internal/common/errors.go`). If it does, plain `errors.Is(err, context.DeadlineExceeded)` already reaches the cause and `chainHasDeadlineExceeded` collapses to it — keep the helper but let the test prove which branch is needed. In `rollback.go` add beside `rollbackBudget`:

```go
// commitBudget bounds the commit call itself once a flow decides to commit.
// Sibling of rollbackBudget with the same shape and rationale: the commit must
// not be cancellable by the request's deadline or disconnect (an interrupted
// commit is an in-doubt outcome — spec D2), but it must not hang forever
// either. 30s comfortably covers a large flush; PostgreSQL's own statement
// ceiling still applies underneath.
const commitBudget = 30 * time.Second

// CommitContext derives the context a commit runs on: the caller's values
// without the caller's cancellation, under a bounded deadline. WithoutCancel
// is load-bearing for the same reason as RollbackContext (tenant checks read
// UserContext off the ctx).
func CommitContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), commitBudget)
}
```

- [ ] **Step 3: Verify** — same test command → PASS. Also `go vet ./internal/common/`.
- [ ] **Step 4: Commit** — `feat(common): request-timeout helpers and CommitContext commit shield`

---

### Task 3: Commit shield + pre-commit check in `txScope`

**Files:**
- Modify: `internal/domain/entity/txscope.go:93-96` (`Commit`), `internal/domain/entity/handler.go:100-105` (`commitOwned`)
- Test: `internal/domain/entity/txscope_test.go` (extend; if absent, create — the package has an existing test harness with fake stores, follow `handler_test.go` setup)

**Interfaces:**
- Consumes: `common.CommitContext` (T2).
- Produces: `txScope.Commit()` now (a) fails closed with `ctx.Err()` BEFORE marking done when the owned scope's ctx is expired/cancelled (so the deferred `Release` still rolls back), (b) runs `txMgr.Commit` on `CommitContext`. All flows calling `scope.Commit()` get both for free.

- [ ] **Step 1: RED** — add to the entity package tests (use the package's existing fake `spi.TransactionManager`; if none records Commit ctx, add a minimal one):

```go
// fakeTxMgr records the ctx its Commit receives and can be pre-seeded.
type commitRecordingTxMgr struct {
	spi.TransactionManager // embed the package's existing fake
	commitCtx context.Context
}

func (m *commitRecordingTxMgr) Commit(ctx context.Context, txID string) error {
	m.commitCtx = ctx
	return m.TransactionManager.Commit(ctx, txID)
}

func TestTxScope_Commit_PreCommitCheckFailsClosed(t *testing.T) {
	h := newTestHandler(t) // package's existing constructor pattern
	ctx, cancel := common.WithRequestTimeout(testCtx(t), 1)
	defer cancel()
	scope, err := h.beginScope(ctx)
	if err != nil { t.Fatal(err) }
	<-scope.Ctx().Done() // deadline expires before commit
	err = scope.Commit()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want DeadlineExceeded from pre-commit check, got %v", err)
	}
	// Scope must NOT be done: Release must still roll back.
	scope.Release() // must invoke txMgr.Rollback — assert via the fake's rollback recorder
}

func TestTxScope_Commit_RunsOnShieldedCtx(t *testing.T) {
	// Begin with a live feature deadline; assert the ctx handed to txMgr.Commit
	// has no inherited cancellation from the request ctx (its Deadline differs
	// and cancelling the request ctx does not cancel it).
}
```

Run: `go test ./internal/domain/entity/ -run TestTxScope_Commit -v` → FAIL.
- [ ] **Step 2: GREEN** — in `txscope.go` replace `Commit`:

```go
// Commit commits when this flow owns the transaction. Before committing it
// performs the spec-D2 pre-commit check: an expired/cancelled context fails
// closed WITHOUT marking the scope done, so the deferred Release still rolls
// the transaction back — the 408 contract ("nothing committed") depends on it.
// The commit itself runs on a CommitContext (WithoutCancel + budget) so no
// deadline or disconnect can interrupt a commit in flight (in-doubt outcome).
// After a commit ATTEMPT the scope is done regardless of outcome: the commit
// may be partially applied, and a rollback would trip memory's
// ErrTxCommitInProgress path.
func (s *txScope) Commit() error {
	if s.owned {
		if err := s.ctx.Err(); err != nil {
			return err
		}
	}
	s.done = true
	return s.h.commitOwned(s.ctx, s.txID, s.owned)
}
```

and in `handler.go` `commitOwned`:

```go
func (h *Handler) commitOwned(ctx context.Context, txID string, owned bool) error {
	if !owned {
		return nil
	}
	commitCtx, cancel := common.CommitContext(ctx)
	defer cancel()
	return h.txMgr.Commit(commitCtx, txID)
}
```

- [ ] **Step 3: Verify** — `go test ./internal/domain/entity/ -v` (whole package — the existing flows must stay green). Expected: PASS.
- [ ] **Step 4: Commit** — `feat(entity): pre-commit deadline check and shielded commit in txScope`

---

### Task 4: Commit shield in CBD `flushAndCommitSegment` + cascade loop check

**Files:**
- Modify: `internal/domain/workflow/engine_processors.go:435-457` (`flushAndCommitSegment`), `internal/domain/workflow/engine.go:857` (cascadeAutomated loop head)
- Test: `internal/domain/workflow/` (extend the engine test harness; CBD tests exist — grep `COMMIT_BEFORE_DISPATCH` in `engine_processors_test.go`)

**Interfaces:**
- Consumes: `common.CommitContext` (T2).
- Produces: segment commits are shielded + pre-checked; `cascadeAutomated` observes ctx between transitions (naturally inert post-CBD, spec D3/D9).

- [ ] **Step 1: RED** — engine-level tests: (a) a workflow whose ctx is already expired when `flushAndCommitSegment` would run its commit → the segment commit is NOT attempted, error chain contains `context.DeadlineExceeded`; (b) after a CBD segment commit, an expired original deadline does NOT abort the continuing cascade (post-CBD ctx is `WithoutCancel` — assert the entity reaches its final state); (c) `cascadeAutomated` with a pre-cancelled ctx returns an error rather than continuing (memory-backend uniformity, spec D9). Use the package's existing fake stores/dispatcher.
- [ ] **Step 2: GREEN** — in `flushAndCommitSegment`, replace the commit call (`engine_processors.go:453`):

```go
	// Spec D2/D3: pre-commit check + shielded commit. An expired context here
	// means no segment has committed yet on this path — fail closed so the
	// caller rolls back and the request maps to 408. The commit itself must
	// never be interrupted mid-flight (in-doubt outcome).
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("commit-before-dispatch: context expired before segment commit: %w", err)
	}
	commitCtx, cancel := common.CommitContext(ctx)
	defer cancel()
	if err := e.txMgr.Commit(commitCtx, txID); err != nil {
		return fmt.Errorf("commit-before-dispatch: commit TX_pre: %w", errors.Join(ErrCommitBeforeDispatchInfra, err))
	}
```

Note the pre-commit-check error is deliberately NOT wrapped with `ErrCommitBeforeDispatchInfra` — it must reach the handler-seam classifier as a plain deadline chain (T5 maps it to 408), not as a ticketed 500. At the top of `cascadeAutomated`'s transition loop (`engine.go:857`) add:

```go
		if err := ctx.Err(); err != nil {
			return retCtx, retTxID, fmt.Errorf("cascade aborted: %w", err)
		}
```

(match the function's actual return shape — read the surrounding code; route the error the same way an `attemptTransition` failure routes, so the deferred rollback guards run).
- [ ] **Step 3: Verify** — `go test ./internal/domain/workflow/ -v` → PASS.
- [ ] **Step 4: Commit** — `feat(workflow): shielded segment commit and cancellation-aware cascade`

---

### Task 5: HTTP entity write handlers — resolve param, attach deadline, classify 408, reject joined

**Files:**
- Modify: `internal/domain/entity/handler.go` — `Create` (:229), `CreateCollection` (:691), `UpdateCollection` (:750), `UpdateSingleWithLoopback` (:857), `UpdateSingle` (:890), `PatchSingleWithLoopback` (:924), `PatchSingle` (:929), plus the shared `h.patch` helper (:935) if both patch variants funnel through it. Add helper next to `resolveTransactionWindow` (:578).
- Test: `internal/domain/entity/handler_test.go` (extend; it has an httptest-style harness with fake stores)

**Interfaces:**
- Consumes: T1 codes, T2 helpers, T3 scope behavior.
- Produces: helper used by all eight ops:

```go
// resolveRequestTimeout validates transactionTimeoutMillis, rejects it on a
// joined (tx-token) request, and attaches the feature deadline. millis==nil
// ⇒ (ctx, no-op cancel, nil) — behavior unchanged (PATCH constraint).
func resolveRequestTimeout(ctx context.Context, millis *int64) (context.Context, context.CancelFunc, *common.AppError)
```

- [ ] **Step 1: RED** — handler tests (follow the package's existing `httptest.NewRequest` + fake-store harness):

```go
func TestCreate_TransactionTimeoutMillis_Invalid400(t *testing.T) {
	// ?transactionTimeoutMillis=0 → 400 BAD_REQUEST (also table-drive -1)
}

func TestCreate_TransactionTimeoutMillis_JoinedRejected400(t *testing.T) {
	// Put a joined tx on the request ctx: ctx = spi.WithTransaction(ctx, &spi.TransactionState{ID: "tx-1"})
	// (use the same helper the txjoin middleware uses — check internal/httpmw/txjoin_mw.go:33 for the exact call)
	// ?transactionTimeoutMillis=5000 → 400 BAD_REQUEST, body mentions the param name and joined transaction.
}

func TestCreate_TransactionTimeout_408NothingCommitted(t *testing.T) {
	// Fake store whose Save blocks until ctx.Done() and returns ctx.Err()
	// (deterministic — no wall clock). ?transactionTimeoutMillis=1.
	// Assert: 408, problem+json, properties.errorCode == "TRANSACTION_TIMEOUT",
	// properties.retryable == true, fake txMgr recorded a Rollback and NO Commit.
}

func TestCreate_CommitWins_DeadlineAfterCommit200(t *testing.T) {
	// Fake txMgr whose Commit succeeds but sleeps past the deadline via the
	// SHIELDED ctx (assert commit saw no cancellation): response is 200.
}
```

Replicate the 400-invalid + 400-joined pair for all eight ops (table-driven over op → request builder). Run → FAIL.
- [ ] **Step 2: GREEN** — add next to `resolveTransactionWindow`:

```go
// resolveRequestTimeout applies spec D7/D10 for the write ops: validate,
// reject on a joined transaction, attach the feature-owned deadline.
func resolveRequestTimeout(ctx context.Context, millis *int64) (context.Context, context.CancelFunc, *common.AppError) {
	if millis == nil {
		return ctx, func() {}, nil
	}
	if appErr := common.ValidateRequestTimeoutMillis(*millis); appErr != nil {
		return nil, nil, appErr
	}
	if spi.GetTransaction(ctx) != nil {
		return nil, nil, common.Operational(http.StatusBadRequest, common.ErrCodeBadRequest,
			"transactionTimeoutMillis is not supported on a request that joins an open transaction")
	}
	ctx, cancel := common.WithRequestTimeout(ctx, *millis)
	return ctx, cancel, nil
}
```

Wire each handler the same way (Create shown; the other seven are identical in shape — each already receives its params struct and calls the service with `r.Context()`):

```go
	opCtx, cancelTimeout, paramErr2 := resolveRequestTimeout(r.Context(), params.TransactionTimeoutMillis)
	if paramErr2 != nil {
		common.WriteError(w, r, paramErr2)
		return
	}
	defer cancelTimeout()
	// … pass opCtx instead of r.Context() to CreateEntity / runChunkedCreate /
	// CreateEntityCollection / UpdateEntity / PatchEntity / UpdateEntityCollection …
```

and at EVERY error-writing site in those handlers that follows a service call, classify ours-first:

```go
	if err != nil {
		if appErr := common.ClassifyRequestTimeout(opCtx, err, common.ErrCodeTransactionTimeout); appErr != nil {
			common.WriteError(w, r, appErr)
			return
		}
		common.WriteError(w, r, classifyError(err))
		return
	}
```

Chunked paths (`Create` array-body branch, `CreateCollection`, `UpdateCollection`): only the FIRST-chunk error path can 408 (that is already the only path returning a request-level error — `runChunkedCreate` returns `(results, nil)` with an error element for chunk ≥ 2, spec D3). Apply `ClassifyRequestTimeout` to `firstChunkErr` conversion: `runChunkedCreate` returns `*common.AppError` already classified, so classify BEFORE `classifyError` inside `runChunkedCreate` instead — change its chunk-error handling to:

```go
		result, err := h.CreateEntityCollection(ctx, items[start:end])
		if err != nil {
			var appErr *common.AppError
			if tErr := common.ClassifyRequestTimeout(ctx, err, common.ErrCodeTransactionTimeout); tErr != nil {
				appErr = tErr
			} else {
				appErr = classifyError(err)
			}
			if chunkIdx == 0 {
				return nil, appErr
			}
			// …existing error-element append (unchanged): later-chunk expiry
			// surfaces as a TRANSACTION_TIMEOUT-coded element, never 408 (spec D3).
```

- [ ] **Step 3: Verify** — `go test ./internal/domain/entity/ -v` → PASS.
- [ ] **Step 4: Commit** — `feat(api): honor transactionTimeoutMillis on entity write endpoints`

---

### Task 6: Domain loop cancellation checks + memory-backend regression tests

**Files:**
- Modify: `internal/domain/entity/handler.go:662` (`runChunkedCreate` loop head), `internal/domain/entity/service.go` — `CreateEntityCollection` per-item loop (~:1261), `UpdateEntityCollection` per-item loop (inside :1730-…), `DeleteEntitiesConditional` per-id loop (:1029), `UpdateCollection` handler chunk loop (:817 vicinity)
- Test: `internal/domain/entity/` package tests

**Interfaces:**
- Consumes: T5's `opCtx` plumbing.
- Produces: every unbounded per-item/per-chunk domain loop observes `ctx.Err()` at iteration head (generic — any cancellation, spec D9).

- [ ] **Step 1: RED** — (a) `TestRunChunkedCreate_CtxExpiredBetweenChunks` — 3 chunks, fake store; cancel the ctx after chunk 1 commits (hook the fake txMgr's Commit to cancel); expect: chunk 1 durable, result has a `TRANSACTION_TIMEOUT`-coded error element at chunkIndex 1, chunks 2-3 never attempted, HTTP 200 shape (per T5's runChunkedCreate change + this task's loop check). (b) `TestCreateEntity_MemoryBackend_PreExpiredDeadline408` — real memory plugin store (the package tests already construct one — follow existing patterns), `common.WithRequestTimeout(ctx, 1)`, wait `<-ctx.Done()`, call `h.CreateEntity(opCtx, …)`: expect an error whose chain has `context.DeadlineExceeded` and no committed entity (regression guard: memory has no cancellable syscall). Run → FAIL.
- [ ] **Step 2: GREEN** — add at each loop head (exact shape):

```go
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("operation aborted: %w", err)
		}
```

(match each site's return shape; in `runChunkedCreate` route through the same error-element path as a chunk failure so D3 semantics hold; in `DeleteEntitiesConditional`'s per-id loop, `break` out and let the existing commit path run is WRONG — fail the IIFE with the error so the tx rolls back, fail closed.)
- [ ] **Step 3: Verify** — `go test ./internal/domain/entity/ -v` → PASS.
- [ ] **Step 4: Commit** — `feat(entity): cancellation-aware collection and delete loops`

---

### Task 7: `newMessage` timeout — pre-Save check + shielded Save

**Files:**
- Modify: `internal/domain/messaging/handler.go:32-115` (`NewMessage`)
- Test: `internal/domain/messaging/handler_test.go`

**Interfaces:**
- Consumes: T1, T2. Messaging has no tx and no joined path — but the txjoin middleware is global, so still reject on `spi.GetTransaction(ctx) != nil` (uniform rule, spec D7).

- [ ] **Step 1: RED** — tests with a fake `spi.StoreFactory`/`MessageStore`: (a) `?transactionTimeoutMillis=0` → 400; (b) with joined tx on ctx + param → 400; (c) pre-expired deadline (`WithRequestTimeout(…, 1)`; block the fake factory's `MessageStore(ctx)` until ctx done, return ctx.Err()) → 408 `TRANSACTION_TIMEOUT`, nothing saved; (d) save-wins: fake Save asserts its ctx has no inherited cancellation (shielded) and succeeds after the deadline → 200. Run → FAIL.
- [ ] **Step 2: GREEN** — at the top of `NewMessage` (before body read):

```go
	opCtx := r.Context()
	if params.TransactionTimeoutMillis != nil {
		if appErr := common.ValidateRequestTimeoutMillis(*params.TransactionTimeoutMillis); appErr != nil {
			common.WriteError(w, r, appErr)
			return
		}
		if spi.GetTransaction(opCtx) != nil {
			common.WriteError(w, r, common.Operational(http.StatusBadRequest, common.ErrCodeBadRequest,
				"transactionTimeoutMillis is not supported on a request that joins an open transaction"))
			return
		}
		var cancel context.CancelFunc
		opCtx, cancel = common.WithRequestTimeout(opCtx, *params.TransactionTimeoutMillis)
		defer cancel()
	}
```

Use `opCtx` for `h.factory.MessageStore(opCtx)`. Replace the Save block (spec D2 newMessage bullet):

```go
	// Spec D2: the Save is this path's commit boundary — pre-check then shield,
	// so a 408 can only mean the save never began (save-wins).
	if err := opCtx.Err(); err != nil {
		common.WriteError(w, r, common.ClassifyRequestTimeout(opCtx, err, common.ErrCodeTransactionTimeout))
		return
	}
	saveCtx, saveCancel := common.CommitContext(opCtx)
	defer saveCancel()
	if err := store.Save(saveCtx, id.String(), header, metaData, strings.NewReader(payloadString)); err != nil {
		common.WriteError(w, r, common.Internal("failed to save message", err))
		return
	}
```

and classify ours-first on the store-factory error path:

```go
	store, err := h.factory.MessageStore(opCtx)
	if err != nil {
		if appErr := common.ClassifyRequestTimeout(opCtx, err, common.ErrCodeTransactionTimeout); appErr != nil {
			common.WriteError(w, r, appErr)
			return
		}
		common.WriteError(w, r, common.Internal("failed to get message store", err))
		return
	}
```

Guard: `ClassifyRequestTimeout` can return nil for a bare `context.Canceled` pre-check — in the pre-check branch fall back to `common.Internal("request cancelled before save", err)` when it returns nil.
- [ ] **Step 3: Verify** — `go test ./internal/domain/messaging/ -v` → PASS.
- [ ] **Step 4: Commit** — `feat(messaging): honor transactionTimeoutMillis on newMessage with shielded save`

---

### Task 8: sqlite `MessageStore.DeleteBatch` IN-list chunking (plugin)

**Files:**
- Modify: `plugins/sqlite/message_store.go:93-113`
- Test: `plugins/sqlite/message_store_test.go`

- [ ] **Step 1: RED** — in the sqlite plugin module: seed 1,200 messages (loop `Save`), call `DeleteBatch` with all 1,200 ids, assert all gone via `GetBatch`/`Get`. With the current single-statement build this passes under the 32766 wasm limit — so ALSO add a compile-time-visible chunk constant and a unit test that `deleteBatchChunks(ids, 3)` yields correct chunking, plus a `DeleteBatch` test forcing the chunk path by temporarily... no — keep it honest: make the chunk size a package const `deleteBatchChunkSize = 500` and test with 1,200 ids so ≥3 statements execute. Run from `plugins/sqlite/`: `go test ./... -run TestMessageStore_DeleteBatch -v` → FAIL (const undefined / behavior unchanged proves via statement-count recorder if the harness has one; otherwise the 1,200-id test passes both ways — then assert chunking structurally via the helper's unit test).
- [ ] **Step 2: GREEN**:

```go
// deleteBatchChunkSize bounds the parameterized IN list per statement, well
// under SQLite's bound-variable limit (32766 in this wasm build). Message
// delete is documented non-transactional, so statement chunking changes
// nothing observable.
const deleteBatchChunkSize = 500

func (s *messageStore) DeleteBatch(ctx context.Context, ids []string) error {
	for start := 0; start < len(ids); start += deleteBatchChunkSize {
		end := min(start+deleteBatchChunkSize, len(ids))
		chunk := ids[start:end]
		placeholders := make([]string, len(chunk))
		args := make([]any, 0, len(chunk)+1)
		args = append(args, string(s.tenantID))
		for i, id := range chunk {
			placeholders[i] = "?"
			args = append(args, id)
		}
		query := fmt.Sprintf(`DELETE FROM messages WHERE tenant_id = ? AND message_id IN (%s)`,
			strings.Join(placeholders, ", "))
		if _, err := s.db.ExecContext(ctx, query, args...); err != nil {
			return fmt.Errorf("failed to batch delete messages: %w", err)
		}
	}
	return nil
}
```

(The `len(ids)==0` guard is subsumed — the loop body never runs.)
- [ ] **Step 3: Verify** — `cd plugins/sqlite && go test ./... -v` → PASS.
- [ ] **Step 4: Commit** — `fix(sqlite): chunk MessageStore.DeleteBatch IN list`

---

### Task 9: `deleteMessages` batching + validation

**Files:**
- Modify: `internal/domain/messaging/handler.go:225-270` (`DeleteMessages`)
- Test: `internal/domain/messaging/handler_test.go`

**Interfaces:**
- Produces: with `?transactionSize=N`: one `DeleteBatch` call per chunk of ≤N ids, one response element per chunk `{entityIds, success}`; failed chunk ⇒ `success:false`, later chunks still attempted (spec D4). Absent ⇒ byte-identical current behavior.

- [ ] **Step 1: RED** — fake MessageStore recording `DeleteBatch` calls and failing on command: (a) `?transactionSize=0` → 400; joined-tx + param → 400 (uniform rule); (b) 5 ids + `transactionSize=2` → 3 calls `[2,2,1]`, 200 with 3 elements all `success:true`; (c) same but fail call 2 → 200, elements `[true,false,true]`, third chunk WAS attempted; (d) no param + store error → 500 (unchanged); (e) no param success → single call, single element (unchanged shape). Run → FAIL.
- [ ] **Step 2: GREEN** — after the uuid-validation loop:

```go
	batchSize := 0
	if params.TransactionSize != nil {
		if *params.TransactionSize < 1 {
			common.WriteError(w, r, common.Operational(http.StatusBadRequest, common.ErrCodeBadRequest,
				"transactionSize must be a positive integer"))
			return
		}
		if spi.GetTransaction(r.Context()) != nil {
			common.WriteError(w, r, common.Operational(http.StatusBadRequest, common.ErrCodeBadRequest,
				"transactionSize is not supported on a request that joins an open transaction"))
			return
		}
		batchSize = int(*params.TransactionSize)
	}
	// …store acquisition unchanged…
	if batchSize == 0 {
		// existing single-call path, byte-for-byte
		if err := store.DeleteBatch(r.Context(), ids); err != nil { …unchanged 500… }
		…unchanged single-element response…
		return
	}
	// Spec D4: one store call per chunk, one response element per chunk;
	// a failed chunk does not stop later chunks.
	resp := make([]map[string]any, 0)
	for start := 0; start < len(ids); start += batchSize {
		end := min(start+batchSize, len(ids))
		chunk := ids[start:end]
		err := store.DeleteBatch(r.Context(), chunk)
		if err != nil {
			slog.Warn("message delete batch failed", "pkg", "messaging", "batchStart", start, "err", err)
		}
		resp = append(resp, map[string]any{"entityIds": chunk, "success": err == nil})
	}
	common.WriteJSON(w, http.StatusOK, resp)
```

- [ ] **Step 3: Verify** — `go test ./internal/domain/messaging/ -v` → PASS.
- [ ] **Step 4: Commit** — `feat(messaging): honor transactionSize on deleteMessages (batched, per-batch report)`

---

### Task 10: Batched, version-guarded `deleteEntities`

**Files:**
- Modify: `internal/domain/entity/service.go:938-1055` (`DeleteEntitiesConditional` — new `batchSize int` last param; batched path added), `internal/domain/entity/handler.go:485-520` (`DeleteEntities` — resolve param)
- Test: `internal/domain/entity/` package tests

**Interfaces:**
- Consumes: `scope.Commit()` (shielded, T3), existing `h.searchSvc.Search`, `spi.EntityStore.Get/Delete`, `spi.Entity.Meta.Version` (verify exact field type in the SPI — `e.Meta.Version`; adjust comparisons to its type).
- Produces: `DeleteEntitiesConditional(ctx, entityName, modelVersion string, condBody []byte, pointInTime *time.Time, verbose bool, batchSize int)` — `batchSize<=0` ⇒ existing behavior byte-for-byte. Update ALL callers (grep `DeleteEntitiesConditional(` — handler.go:493 plus any tests; T15 adds a gRPC caller).

- [ ] **Step 1: RED** — package tests against the real memory plugin store (existing harness):

```go
func TestDeleteEntitiesConditional_Batched_HappyPath(t *testing.T) {
	// Seed 5 matching entities; batchSize=2 → RemovedCount=5, MatchedCount=5,
	// empty IDToError; fake/spy txMgr observed ≥3 Begin/Commit pairs beyond
	// the resolution tx (batch granularity is observable).
}

func TestDeleteEntitiesConditional_Batched_VersionGuard(t *testing.T) {
	// Seed 4 matching; after the resolution pass, mutate entity #3 (Save a new
	// version outside). batchSize=2 → #3 NOT deleted, present in IDToError,
	// RemovedCount=3; the modified entity still exists with its new payload.
	// Deterministic hook: wrap the store/txMgr so the mutation runs after the
	// resolution tx commits and before the second batch begins.
}

func TestDeleteEntitiesConditional_Batched_FailedBatchContinues(t *testing.T) {
	// Force batch 2's commit to fail (fake txMgr): its ids all land in
	// IDToError, batches 1 and 3 committed, RemovedCount reflects them.
}

func TestDeleteEntitiesConditional_Batched_DeleteAllPath(t *testing.T) {
	// Empty condBody + batchSize=2 over 5 entities → batched enumeration path,
	// RemovedCount=5, MatchedCount=5 (NOT the single-tx DeleteAll fast path —
	// spy store asserts DeleteAll was NOT called).
}

func TestDeleteEntitiesConditional_NoBatchSize_Unchanged(t *testing.T) {
	// batchSize=0 keeps today's single-tx behavior: exactly one Begin/Commit,
	// DeleteAll fast path used for empty cond.
}
```

Handler tests: `?transactionSize=0` → 400; joined + param → 400; `?transactionSize=2&verbose=true` happy path shape. Run → FAIL (signature change breaks compile first — fix call sites in the RED step so tests fail on behavior, not compile).
- [ ] **Step 2: GREEN** — handler `DeleteEntities` gains (before body read):

```go
	batchSize := 0
	if params.TransactionSize != nil {
		if *params.TransactionSize < 1 {
			common.WriteError(w, r, common.Operational(http.StatusBadRequest, common.ErrCodeBadRequest,
				"transactionSize must be a positive integer"))
			return
		}
		if spi.GetTransaction(r.Context()) != nil {
			common.WriteError(w, r, common.Operational(http.StatusBadRequest, common.ErrCodeBadRequest,
				"transactionSize is not supported on a request that joins an open transaction"))
			return
		}
		batchSize = int(*params.TransactionSize)
	}
```

pass `batchSize` through. Service — structure (keep the existing single-tx code path untouched for `batchSize<=0`; add):

```go
// deleteBatched implements spec D4: resolution tx reads matched ids + their
// CURRENT versions (condition evaluated as-at pointInTime; guard baseline is
// the current row), then successive owned transactions of ≤batchSize ids
// re-read each version and delete only unchanged ids. A failed batch commit
// maps its ids into IDToError; later batches still run. Joined requests never
// reach here (handler rejects the param).
func (h *Handler) deleteBatched(ctx context.Context, ref spi.ModelRef, cond predicate.Condition, pointInTime *time.Time, verbose bool, batchSize int) (*DeleteResult, error)
```

Resolution phase: `beginScope` → model check (reuse the existing code) → `matched, err := h.searchSvc.Search(txCtx, ref, cond, search.SearchOptions{PointInTime: pointInTime, Limit: -1})` — for `cond == nil` (delete-all with batching) use `entityStore.GetAll(txCtx, ref)` instead. Baseline versions: when `pointInTime == nil`, take `e.Meta.Version` from the matched envelope; when `pointInTime != nil`, per id `cur, err := entityStore.Get(txCtx, id)` — `spi.ErrNotFound` ⇒ `result.IDToError[id] = perIDDeleteError(id, err)`, skip. `scope.Commit()` the read-only resolution tx. Batch phase, per chunk:

```go
	for start := 0; start < len(targets); start += batchSize {
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("delete aborted between batches: %w", err)
		}
		chunk := targets[start:min(start+batchSize, len(targets))]
		if err := h.deleteOneBatch(ctx, ref, chunk, result); err != nil {
			return nil, err // begin failure only — batch-level store/commit errors go to IDToError inside
		}
	}
```

`deleteOneBatch`: own `beginScope`/`defer Release`; per target: `cur, err := entityStore.Get(txCtx, t.id)` → NotFound ⇒ IDToError; `cur.Meta.Version != t.baselineVersion` ⇒ `IDToError[t.id] = "entity modified after delete resolution; not deleted"`; else `Delete` (error ⇒ `perIDDeleteError`) else mark pending-removed. Gate + `scope.Commit()` mirroring the existing IIFE (`service.go:1025-1052`); on commit error map ALL this batch's pending-removed ids into IDToError (do NOT increment RemovedCount), on success `RemovedCount += pending`. `verbose` collects ids during resolution.
- [ ] **Step 3: Verify** — `go test ./internal/domain/entity/ -v` → PASS.
- [ ] **Step 4: Commit** — `feat(entity): version-guarded batched conditional delete (transactionSize)`

---

### Task 11: Search `timeoutMillis` — spec param + HTTP handler + fallback-loop check

**Files:**
- Modify: `api/openapi.yaml` (searchEntities params block `:7204-7268`), regen `api/generated.go` (`go generate ./api` or the project's generate target — `scripts/check-generated-in-sync.sh` verifies), `internal/domain/search/handler.go:79-170`, `internal/domain/search/service.go:400-412` (fallback match loop)
- Test: `internal/domain/search/` package tests

**Interfaces:**
- Consumes: T1 (`ErrCodeSearchTimeout`), T2.
- Produces: `SearchEntitiesParams.TimeoutMillis *int64` (generated); handler enforces per spec D5.

- [ ] **Step 1: Spec + regen** — add to `searchEntities` parameters (NO default, int64, optional):

```yaml
        - name: timeoutMillis
          in: query
          description: |
            Maximum time in milliseconds to wait for the search to complete.
            When exceeded, the request fails with 408 SEARCH_TIMEOUT and no
            partial results are returned. Absent means no server-side timeout.
          required: false
          schema:
            type: integer
            format: int64
```

and a 408 response on the op (problem+json, description "Search timeout exceeded — the client-supplied timeoutMillis elapsed"). Regenerate; `./scripts/check-generated-in-sync.sh` must pass.
- [ ] **Step 2: RED** — handler tests (existing fake `searchSvc` harness): (a) `?timeoutMillis=0` → 400; (b) joined-tx + param → 400; (c) fake `Search` blocks on ctx and returns `ctx.Err()`; `?timeoutMillis=1` → 408 `SEARCH_TIMEOUT` problem+json BEFORE any ndjson byte; (d) domain fallback loop: service-level test — pre-expired feature ctx + a store WITHOUT `spi.Searcher` (forces the `service.go:405` loop) returns DeadlineExceeded, not results. Run → FAIL.
- [ ] **Step 3: GREEN** — handler, after the limit block:

```go
	opCtx := r.Context()
	if params.TimeoutMillis != nil {
		if appErr := common.ValidateRequestTimeoutMillis(*params.TimeoutMillis); appErr != nil {
			common.WriteError(w, r, appErr)
			return
		}
		if spi.GetTransaction(opCtx) != nil {
			common.WriteError(w, r, common.Operational(http.StatusBadRequest, common.ErrCodeBadRequest,
				"timeoutMillis is not supported on a request that joins an open transaction"))
			return
		}
		var cancel context.CancelFunc
		opCtx, cancel = common.WithRequestTimeout(opCtx, *params.TimeoutMillis)
		defer cancel()
	}
	results, err := h.searchSvc.Search(opCtx, modelRef, cond, opts)
	if err != nil {
		if appErr := common.ClassifyRequestTimeout(opCtx, err, common.ErrCodeSearchTimeout); appErr != nil {
			common.WriteError(w, r, appErr)
			return
		}
		// …existing AppError-forward + Internal fallback unchanged…
	}
```

Fallback loop (`service.go:405-409`) becomes:

```go
	for i, e := range entities {
		if i&1023 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, fmt.Errorf("search aborted: %w", err)
			}
		}
		// …existing prepared.Match body…
	}
```

- [ ] **Step 4: Verify** — `go test ./internal/domain/search/ -v` and `go test ./api/... 2>/dev/null; ./scripts/check-generated-in-sync.sh` → PASS.
- [ ] **Step 5: Commit** — `feat(search): re-add timeoutMillis with real enforcement (408 SEARCH_TIMEOUT)`

---

### Task 12: memory plugin — cancellation-aware scan loops

**Files:**
- Modify: `plugins/memory/searcher.go` (loops at `:92-96`, `:103-113`, `:127-134` next closure, `:187-195` matchSortBounded, pre-sort check before `:203` sortByOrder), `plugins/memory/entity_store.go` (`GetAll` `:417-479`, `GetAllAsAt` and `getAllSnapshotUnlocked` `:110-134` — thread ctx through; it currently takes none)
- Test: `plugins/memory/` package tests

- [ ] **Step 1: RED** — in the memory plugin module: `TestSearch_PreExpiredCtxAborts` — seed 10 entities, expired ctx (`context.WithTimeout(ctx, 0)`; wait Done), `EntityStore.Search(...)` and `GetAll(...)` both return `context.DeadlineExceeded`-chained errors, zero results. Also an in-tx (RYW) variant hitting the `:92-113` loops. Run from `plugins/memory/` → FAIL.
- [ ] **Step 2: GREEN** — amortized check at each loop head (`if i&1023 == 0 { if err := ctx.Err(); err != nil { return nil, err } }`; where the loop has no index, add a counter). Thread `ctx` into `getAllSnapshotUnlocked` → rename param plumbing only, no locking changes (respect `.claude/rules/go-mutex-discipline.md` — checks go OUTSIDE held locks where possible; where a check must run under RLock that is acceptable, `ctx.Err()` is a lock-free atomic read). Pre-sort check before `sortByOrder` / `matchSortBounded`'s sort call. The `next` closures used with `spi.MergeBounded` gain the same counter check.
- [ ] **Step 3: Verify** — `cd plugins/memory && go test ./... -v` → PASS.
- [ ] **Step 4: Commit** — `feat(memory): cancellation-aware search scan loops`

---

### Task 13: sqlite plugin — cancellation-aware scan loops

**Files:**
- Modify: `plugins/sqlite/searcher.go` (committed loop `:113-138`, streamed `next` `:253-272`, tx-buffer loop `:277-287`, pre-sort at `:189-193`)
- Test: `plugins/sqlite/` package tests

- [ ] **Step 1: RED** — mirror T12's tests in the sqlite module (seed via the plugin store, expired ctx, assert deadline-chained error, including the tx-overlay path). Run from `plugins/sqlite/` → FAIL (the row loop today surfaces a driver error late or succeeds on buffered rows).
- [ ] **Step 2: GREEN** — same amortized-check shape as T12 at the three loops + pre-sort.
- [ ] **Step 3: Verify** — `cd plugins/sqlite && go test ./... -v` → PASS.
- [ ] **Step 4: Commit** — `feat(sqlite): cancellation-aware search scan loops`

---

### Task 14: gRPC entity writes — honor `transactionTimeoutMs`

**Files:**
- Modify: `internal/grpc/entity.go` — the five cases decoding `TransactionTimeoutMs`: EntityCreateRequest (:35), EntityUpdateRequest (:78), EntityPatchRequest (:126), EntityCreateCollectionRequest (:262), EntityUpdateCollectionRequest (:317)
- Test: `internal/grpc/` package tests (existing envelope-assertion harness)

**Interfaces:**
- Consumes: T1, T2; same domain-service seam as HTTP (already wired).
- Produces: shared helper in `internal/grpc`:

```go
// resolveEventTimeout applies spec D6/D10 for CloudEvent writes: validate,
// reject on a tx-token'd (joined) request, attach the feature deadline.
func resolveEventTimeout(ctx context.Context, millis *int) (context.Context, context.CancelFunc, error)
```

- [ ] **Step 1: RED** — gRPC tests: (a) create with `transactionTimeoutMs: 0` → envelope `Success:false`, `Error.Code == "CLIENT_ERROR"`, message prefixed `BAD_REQUEST:`; (b) tx-token'd create with the field set → same rejection; (c) create with `transactionTimeoutMs: 1` against a fake handler/store that blocks on ctx → `CLIENT_ERROR` with `TRANSACTION_TIMEOUT:` prefix and `Retryable: true`; (d) commit-wins: deadline expires after the domain call succeeded → `Success:true`. Follow the package's existing table-driven event tests. Run → FAIL.
- [ ] **Step 2: GREEN** — helper (in `internal/grpc/entity.go` or a small new `timeout.go`):

```go
func resolveEventTimeout(ctx context.Context, millis *int) (context.Context, context.CancelFunc, error) {
	if millis == nil {
		return ctx, func() {}, nil
	}
	if appErr := common.ValidateRequestTimeoutMillis(int64(*millis)); appErr != nil {
		return nil, nil, appErr
	}
	if spi.GetTransaction(ctx) != nil {
		return nil, nil, common.Operational(http.StatusBadRequest, common.ErrCodeBadRequest,
			"transactionTimeoutMs is not supported on a request that joins an open transaction")
	}
	tctx, cancel := common.WithRequestTimeout(ctx, int64(*millis))
	return tctx, cancel, nil
}
```

Each case: after decode, `opCtx, cancelT, terr := resolveEventTimeout(ctx, req.TransactionTimeoutMs)`; on terr return the existing error-envelope builder with terr (`entityTransactionError(ctx, ce.Id, terr)` — it maps operational AppErrors to CLIENT_ERROR already); `defer cancelT()`; pass `opCtx` to the handler call; on handler error classify ours-first:

```go
		if err != nil {
			if appErr := common.ClassifyRequestTimeout(opCtx, err, common.ErrCodeTransactionTimeout); appErr != nil {
				err = appErr
			}
			…existing entityTransactionError/… path with err…
		}
```

(The update-collection case loops per item — attach ONE deadline around the whole case, and add a loop-head `ctx.Err()` check consistent with D9.)
- [ ] **Step 3: Verify** — `go test ./internal/grpc/ -v` → PASS.
- [ ] **Step 4: Commit** — `feat(grpc): honor transactionTimeoutMs on entity write events`

---

### Task 15: gRPC delete-all — nullable `transactionSize`, batched path

**Files:**
- Modify: `docs/cyoda/schema/entity/EntityDeleteAllRequest.json:19-23` (drop `"default": 1000`), regen via `scripts/generate-events.sh` (→ `api/grpc/events/types.go`: `TransactionSize` becomes `*int` and the UnmarshalJSON default-stamping at `:1127-1128` disappears — verify), `internal/grpc/entity.go:395-424` (EntityDeleteAllRequest case)
- Test: `internal/grpc/` package tests

**Interfaces:**
- Consumes: T10's `DeleteEntitiesConditional(ctx, name, ver, nil, nil, false, batchSize)` for the batched path (empty condition ⇒ enumerate + batch).
- Produces: contract change recorded in the cloud-parity doc (T22).

- [ ] **Step 1: RED** — (a) after regen, a decode test: payload without `transactionSize` yields `req.TransactionSize == nil` (was 1000); (b) delete-all with `transactionSize: 2` over 5 seeded entities → batched path observable (spy: `DeleteAll` NOT called; ≥3 commits), envelope `NumDeleted: 5`; (c) `transactionSize: 0` → `CLIENT_ERROR` `BAD_REQUEST:` prefix; (d) tx-token'd + field → same; (e) absent field → existing `DeleteAllEntities` single-tx call (spy: `DeleteAll` called once). Run → FAIL (compile: `*int` vs `int`).
- [ ] **Step 2: GREEN** — schema edit + `./scripts/generate-events.sh`; fix any other compile fallout from `*int` (grep `TransactionSize` across the repo). Case body:

```go
		if req.TransactionSize != nil {
			size := *req.TransactionSize
			if size < 1 { …entityDeleteAllError with Operational(400, ErrCodeBadRequest, "transactionSize must be a positive integer")… }
			if spi.GetTransaction(ctx) != nil { …same builder, "transactionSize is not supported on a request that joins an open transaction"… }
			delRes, err := s.entityHandler.DeleteEntitiesConditional(ctx, req.Model.Name, fmt.Sprintf("%d", req.Model.Version), nil, nil, false, size)
			// map delRes → EntityDeleteAllResponseJson{ModelID: delRes.EntityModelID, NumDeleted: delRes.RemovedCount, EntityIds: []string{}}
		} else {
			…existing DeleteAllEntities call unchanged…
		}
```

- [ ] **Step 3: Verify** — `go test ./internal/grpc/ ./api/... -v` and `go build ./...` → PASS.
- [ ] **Step 4: Commit** — `feat(grpc): honor explicit transactionSize on delete-all (schema default removed)`

---

### Task 16: gRPC direct search — honor `timeoutMillis`

**Files:**
- Modify: `internal/grpc/search.go:314-370` (`handleDirectSearchRequest`)
- Test: `internal/grpc/` package tests

- [ ] **Step 1: RED** — (a) `timeoutMillis: 0` → search error envelope `BAD_REQUEST:` prefix; (b) tx-token'd + field → same; (c) blocking fake search service + `timeoutMillis: 1` → error envelope `SEARCH_TIMEOUT:` prefix, `Retryable: true`, no result elements streamed. Run → FAIL.
- [ ] **Step 2: GREEN** — reuse `resolveEventTimeout` (T14) with `req.TimeoutMillis`; wrap the `DirectSearch` call in `opCtx`; classify ours-first with `common.ErrCodeSearchTimeout` before the existing error-envelope send (`snapshotSearchError`-family builder used by this handler — check `internal/grpc/errors.go:288+` for the right one).
- [ ] **Step 3: Verify** — `go test ./internal/grpc/ -v` → PASS.
- [ ] **Step 4: Commit** — `feat(grpc): honor timeoutMillis on direct search`

---

### Task 17: Dispatch pending-request cleanup on all abandonment paths

**Files:**
- Modify: `internal/grpc/dispatch.go:108-164` (`dispatchCalloutToMember`), possibly `internal/grpc/members.go:96-110` (`CompleteRequest` — verify idempotency / add an `AbandonRequest` if Complete has send semantics)
- Test: `internal/grpc/` package tests

- [ ] **Step 1: RED** — read `members.go:69-130` first. Tests: (a) dispatch whose ctx is cancelled mid-wait → after return, the member's pending map no longer contains the requestID (expose via an existing test hook or a `PendingCount()` helper — add one if absent, test-only exported method is fine); (b) same for the `time.After` arm (use a 1ms timeout override); (c) same for a `Send` failure (fake member send error); (d) a LATE response arriving after abandonment is a no-op (no panic, no phantom delivery). Run → FAIL.
- [ ] **Step 2: GREEN** — in `dispatchCalloutToMember`, after `ch := member.TrackRequest(requestID)`:

```go
	// Spec D11: every exit that does not consume the response must clear the
	// tracking entry, or a late compute-node reply finds a dangling channel
	// and the map entry leaks. The response arm's normal completion path
	// already cleared it; clearing again is a no-op.
	defer member.AbandonRequest(requestID)
```

where `AbandonRequest` (new, in `members.go`) removes the entry if still present without touching the channel (mutex per the package's existing locking; follow `.claude/rules/go-mutex-discipline.md`). If reading the code shows `CompleteRequest` is already a safe idempotent remove-and-return, use it instead of adding a method.
- [ ] **Step 3: Verify** — `go test ./internal/grpc/ -v` → PASS.
- [ ] **Step 4: Commit** — `fix(grpc): clear pending dispatch entry on every abandonment path`

---

### Task 18: Proxy query-string preservation unit test

**Files:**
- Test: `internal/cluster/proxy/http_test.go` (extend)

- [ ] **Step 1: RED-ish (characterization)** — add `TestProxy_PreservesQueryString`: backend httptest server records `r.URL.RawQuery`; route a tx-token'd request with `?transactionTimeoutMillis=5000&transactionSize=2&x=%20y` through `HTTPRouting`'s proxy path; assert the backend saw the query verbatim. Follow the file's existing proxy test setup (`:81-141`). This documents the load-bearing D7 behavior; it should pass immediately — that is acceptable for a characterization test guarding a contract.
- [ ] **Step 2: Verify** — `go test ./internal/cluster/proxy/ -v` → PASS.
- [ ] **Step 3: Commit** — `test(proxy): pin query-string preservation across the forwarded hop`

---

### Task 19: e2e — running-backend coverage (`internal/e2e`)

**Files:**
- Create: `internal/e2e/transaction_control_test.go`
- Modify: `internal/e2e/zzz_errorcode_matrix_test.go` (`declaredGaps` `:99-116` + new-code declarations)

**Interfaces:**
- Consumes: the callback harness (`internal/e2e/callback_harness_test.go:174-643`; withheld-reply precedent `scheduled_function_test.go:397-423`); everything T1-T16 shipped.

Coverage cells (from the spec matrix — every cell here is e2e):
1. **408 per entity op (8 ops):** register a model + workflow with an externalized sync processor; connect the harness compute member; the processor WITHHOLDS its reply (release in test teardown BEFORE harness shutdown to avoid the 5s drain — `callback_harness_test.go:645-668`); issue each op with `?transactionTimeoutMillis=1000`. Deterministic: the request parks in the dispatch select until its `ctx.Done()` arm fires — the deadline always wins because the reply never comes; 1000ms leaves setup slack while staying prompt. Assert: 408, `application/problem+json`, `properties.errorCode == "TRANSACTION_TIMEOUT"`, `properties.retryable == true`, AND the entity does not exist afterwards (nothing committed). Assert conformance shape manually (harness stack bypasses the validator middleware).
2. **400 invalid per declaring op (10 ops incl. deletes + newMessage + search):** `=0`, `=-1`, `=abc` (binding-layer 400) — table-driven.
3. **Joined-request 400:** inside a callback-harness processor, perform the loopback HTTP write carrying the tx token WITH `?transactionTimeoutMillis=5000` → the callback's write gets 400; also `deleteEntities?transactionSize=2` with token → 400.
4. **Chunked create post-first-chunk:** array body of 3×`transactionWindow=1` chunks where the compute processor withholds only on the SECOND chunk's entity (tag/criteria-routed) + `transactionTimeoutMillis=1000` → 200, element[0] success, element[1] error code `TRANSACTION_TIMEOUT`, chunk 3 absent.
5. **Batched deleteEntities happy path:** seed 5, `?transactionSize=2&verbose=true` → 200, `numberOfEntititesRemoved == 5`, ids echoed, entities gone. The **version-guard** fires only for a mutation landing between resolution and a batch — a window no full-HTTP-stack test can hit deterministically; it is covered at the unit layer (T10) with a one-line waiver for the e2e cell (spec matrix updated accordingly).
6. **Batched deleteMessages:** seed 5 messages, `?transactionSize=2` → 3 elements, all success.
7. **Absent params unchanged:** one create + one deleteEntities + one deleteMessages without params asserting today's shapes (regression pin).
8. **Search:** `?timeoutMillis=0` → 400; `?timeoutMillis=60000` happy path → 200 ndjson.
9. **newMessage:** `?transactionTimeoutMillis=0` → 400; valid value happy path → 200.

- [ ] **Step 1: RED** — write the file; run `go test ./internal/e2e/ -run TestTransactionControl -v` (Docker required) → FAIL on unimplemented declarations first; fix `declaredGaps`/matrix declarations so the errorcode matrix test recognizes 408 on the ops (read that file's header comment for the declaration format).
- [ ] **Step 2: GREEN** — iterate until green. Reuse harness helpers; do NOT add wall-clock sleeps — the withheld-reply pattern is event-driven.
- [ ] **Step 3: Verify** — `go test ./internal/e2e/ -v` (full e2e package) → PASS.
- [ ] **Step 4: Commit** — `test(e2e): transaction-control params coverage (408/400/batching/joined)`

---

### Task 20: Cross-backend parity scenarios

**Files:**
- Modify: `e2e/parity/client/http.go` (add `transactionSize` support — the deliberate absence note at `:1038-1049` is superseded by this feature), `e2e/parity/registry.go` (+ `wantParityScenarioCount` bump wherever it is asserted), scenario files near `e2e/parity/externalapi/edge_message.go`

**Scenarios (backend-agnostic observable state only — runs on memory/sqlite/postgres + commercial):**
1. Batched `deleteEntities` final-state consistency: seed N=5, delete with `transactionSize=2`, assert all gone + response counts identical across backends.
2. Batched `deleteMessages`: seed 5, `transactionSize=2` → 3 elements all success, store empty.

- [ ] **Step 1: RED** — add scenarios + registry entries; run the parity suite on the in-tree backends (see `e2e/parity` README/Makefile for the runner) → count assertion fails first, then scenario failures if any.
- [ ] **Step 2: GREEN** — iterate to green on memory/sqlite/postgres.
- [ ] **Step 3: Note** — the commercial backend picks these up on its next dependency update (do not attempt to run it here; scenarios must not assume batch-boundary observability, only final state + response shape).
- [ ] **Step 4: Commit** — `test(parity): batched delete final-state scenarios`

---

### Task 21: Multinode e2e — the 400 crosses the forwarded hop

**Files:**
- Modify/extend: `e2e/parity/postgres/multinode_test.go` (follow its existing two-node setup)

- [ ] **Step 1: RED** — scenario: obtain a tx token bound to node A (follow how existing multinode tests mint/carry `X-Tx-Token`), send a write to node B with the token AND `?transactionTimeoutMillis=5000` → expect 400 BAD_REQUEST (the request forwards A-ward and the executing node rejects the param on the joined tx — spec D7/F1). Assert the response body names the param.
- [ ] **Step 2: GREEN** — should pass once T5 shipped; if the 400 does not cross the hop, the proxy error-passthrough is the bug to fix (fail closed).
- [ ] **Step 3: Verify** — run the multinode suite per its harness docs → PASS.
- [ ] **Step 4: Commit** — `test(multinode): transaction-control param rejected across the forwarded hop`

---

### Task 22: Contract + docs reconciliation (Gate 4 / Gate 7)

**Files:**
- Modify: `api/openapi.yaml`, `cmd/cyoda/help/content/crud.md`, `cmd/cyoda/help/content/messages.md`, `cmd/cyoda/help/content/search.md`, `CHANGELOG.md`
- Create: `docs/cloud-parity/transaction-control-params.md`

- [ ] **Step 1: openapi.yaml** — (a) delete `default: 10000` at `:2104, 2322, 2525, 2700, 2865, 3050, 3224, 3545` and `default: 1000` at `:1919, 3384` (line numbers pre-edit — re-grep); (b) unify the four `transactionTimeoutMillis` description variants to ONE wording: "Maximum time in milliseconds the server may spend before the first commit. When exceeded, the operation is rolled back and fails with 408 TRANSACTION_TIMEOUT; nothing is committed. Not supported on requests joining an open transaction (400). Absent means no server-side timeout."; (c) `transactionSize` descriptions: "Number of entities|messages to delete per transaction batch. Batches committed before a failure remain durable and per-id/batch failures are reported in the response. Not supported on requests joining an open transaction (400). Absent means a single transaction|call."; (d) add 408 responses to the 8 entity ops + newMessage (shared wording; problem+json ProblemDetail ref like the existing 4xx responses); (e) fix the `deleteMessages` summary `:3372-3374` to describe the now-real batching and the per-element `success` semantics; (f) run `./scripts/check-generated-in-sync.sh` (param schema changes without structural change should not alter generated.go except where T11 already did).
- [ ] **Step 2: Help topics** — `crud.md` (`:71-91, 168-198, 258-287, 326-352`): replace every "parsed but currently has no behavioural effect in cyoda-go" for these two params with the real semantics (compact — timeout: 408 before first commit, nothing committed, joined→400, multi-commit ops per-chunk after first commit; transactionSize on delete: version-guarded batches, partial-commit contract, joined→400). `messages.md` (`:72, 112-128, 153-161`): same for newMessage timeout + deleteMessages batching (note memory's `success:false` may be partially applied; message delete remains non-transactional); add 408 to the messaging error list. `search.md`: document `timeoutMillis` + `errors.SEARCH_TIMEOUT`. Keep prose compact (project rule).
- [ ] **Step 3: Cloud-parity doc** — `docs/cloud-parity/transaction-control-params.md`: contract deltas — fictional defaults removed (HTTP spec + EntityDeleteAllRequest schema; `TransactionSize` now `*int`, compile-breaking for Go importers of `api/grpc/events`, accepted pre-1.0); 408 + TRANSACTION_TIMEOUT/SEARCH_TIMEOUT added; search `timeoutMillis` re-added; joined-request 400 rule; batched-delete partial-commit + version-guard semantics; D3 first-commit boundary. One file, factual, present tense.
- [ ] **Step 4: CHANGELOG.md** — under the unreleased/v0.8.4 section: Added (params now honored — one bullet per param family; search timeoutMillis re-added), Fixed (dispatch pending-entry leak; sqlite DeleteBatch chunking), Changed (declared alignments: commits are now shielded from disconnect/deadline interruption — sqlite/postgres disconnect-at-flush previously aborted, now completes; disconnect now aborts in-flight loop work on memory/sqlite as on postgres). NO `### Breaking`.
- [ ] **Step 5: Verify** — `go test ./cmd/cyoda/help/ ./internal/e2e/ -run 'TestErrCode|TestContent|TestSeeAlso|Conformance' -v`; grep for leftover "no behavioural effect" mentions of these params → none.
- [ ] **Step 6: Commit** — `docs(api): transaction-control params — spec defaults removed, real semantics documented`

---

### Task 23: Final verification (Gate 5)

- [ ] **Step 1:** `go test ./... -v` (root module, includes e2e; Docker running) → all green.
- [ ] **Step 2:** `make test-all` (root + memory/sqlite/postgres plugin modules) → all green.
- [ ] **Step 3:** `go vet ./...` + per-plugin vet → clean.
- [ ] **Step 4:** `make race` (once, end-of-deliverable) → clean.
- [ ] **Step 5:** Cross-check the spec's coverage matrix cell-by-cell against shipped tests; any missing cell needs a one-line waiver in the PR description.
- [ ] **Step 6:** Commit any stragglers; do NOT create the PR yet — the workflow continues with requesting-code-review → security-review → finishing (PR targets `release/v0.8.4`, milestone v0.8.4; file the follow-up issue for `waitForConsistencyAfter` + gRPC delete-all's other inert fields at PR time; close-at-merge bookkeeping per release-branch practice).
