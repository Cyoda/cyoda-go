package entity

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/txcoord"
	"github.com/cyoda-platform/cyoda-go/plugins/memory"
)

// scopeTxMgr records lifecycle calls and the context each one arrived on.
type scopeTxMgr struct {
	spi.TransactionManager
	mu         sync.Mutex
	rolledBack []string
	committed  []string
	rbCtxErr   []error
	rbDeadline []time.Time
	commitErr  error
	commitCtx  context.Context
	onRollback func()
	onCommit   func(ctx context.Context)
}

func (m *scopeTxMgr) Rollback(ctx context.Context, txID string) error {
	if m.onRollback != nil {
		m.onRollback()
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.rolledBack = append(m.rolledBack, txID)
	m.rbCtxErr = append(m.rbCtxErr, ctx.Err())
	dl, _ := ctx.Deadline()
	m.rbDeadline = append(m.rbDeadline, dl)
	return nil
}

func (m *scopeTxMgr) rolledBackTxs() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.rolledBack)
}

func (m *scopeTxMgr) committedTxs() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.committed)
}

func (m *scopeTxMgr) Commit(ctx context.Context, txID string) error {
	if m.onCommit != nil {
		m.onCommit(ctx)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.committed = append(m.committed, txID)
	m.commitCtx = ctx
	return m.commitErr
}

func newScopeHandler(m *scopeTxMgr) *Handler {
	return &Handler{txMgr: m, coord: txcoord.New(m)}
}

// liveScope is an owned scope on a transaction of the memory plugin, whose
// coordination can open callback scopes in it. Rollback and Commit are only
// recorded (scopeTxMgr); the rest is the plugin's.
type liveScope struct {
	s       *txScope
	m       *scopeTxMgr
	userCtx context.Context
}

func newLiveScope(t *testing.T) *liveScope {
	t.Helper()
	f := memory.NewStoreFactory()
	t.Cleanup(func() { _ = f.Close() })
	f.NewTransactionManager(common.NewDefaultUUIDGenerator())
	m := &scopeTxMgr{TransactionManager: f.GetTransactionManager()}
	userCtx := spi.WithUserContext(context.Background(), &spi.UserContext{
		UserID: "u1", Kind: spi.PrincipalUser, Tenant: spi.Tenant{ID: "tenant-a"},
	})
	txID, txCtx, err := m.Begin(userCtx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	return &liveScope{s: &txScope{h: newScopeHandler(m), ctx: txCtx, txID: txID, owned: true}, m: m, userCtx: userCtx}
}

// openCallback opens a callback scope in the transaction, under a callout the
// owner made, as the join layer does for a joined request. end ends the scope
// with o, then the callout.
func (l *liveScope) openCallback(t *testing.T) (end func(o *common.Outcome)) {
	t.Helper()
	coord := l.s.h.coord
	callout := "callout-" + l.s.txID
	_, endCallout := coord.BeginCallout(l.s.ctx, l.s.txID, callout)
	coord.Advance(callout, 1)
	joined, err := l.m.Join(l.userCtx, l.s.txID)
	if err != nil {
		t.Fatalf("join: %v", err)
	}
	sc, _, err := coord.Enter(joined, l.s.txID, []txcoord.Pair{{Callout: callout, Major: 1}}, txcoord.Write, 0)
	if err != nil {
		t.Fatalf("enter: %v", err)
	}
	return func(o *common.Outcome) {
		sc.End(o)
		endCallout()
	}
}

// markRoot makes the transaction rollback-only as a failed joined write does.
func (l *liveScope) markRoot(t *testing.T) {
	t.Helper()
	l.openCallback(t)(&common.Outcome{Status: http.StatusBadRequest, Code: common.ErrCodeWorkflowFailed})
}

// assertForgotten asserts the coordination holds nothing of the transaction:
// a seal of it now is the seal of a transaction never seen, which a kept
// marked transaction would refuse.
func (l *liveScope) assertForgotten(t *testing.T) {
	t.Helper()
	finish, err := l.s.h.coord.Seal(l.userCtx, l.s.txID)
	finish()
	if err != nil {
		t.Fatalf("the coordination still holds the transaction after its rollback: a new seal answered %v", err)
	}
}

// TestTxScope_OwnedRelease_RollsBack — the base case the 40 deleted rollbackOwned
// calls were doing by hand.
func TestTxScope_OwnedRelease_RollsBack(t *testing.T) {
	m := &scopeTxMgr{}
	s := &txScope{h: newScopeHandler(m), ctx: context.Background(), txID: "tx-1", owned: true}
	s.Release()
	if len(m.rolledBack) != 1 || m.rolledBack[0] != "tx-1" {
		t.Fatalf("owned scope did not roll back: %v", m.rolledBack)
	}
}

// TestTxScope_JoinedRelease_DoesNotRollBackOwnersTx is coverage row 3. A joined
// callback must never roll back the transaction its owner will commit.
func TestTxScope_JoinedRelease_DoesNotRollBackOwnersTx(t *testing.T) {
	m := &scopeTxMgr{}
	s := &txScope{h: newScopeHandler(m), ctx: context.Background(), txID: "tx-owner", owned: false}
	s.Release()
	if len(m.rolledBack) != 0 {
		t.Fatalf("joined scope rolled back the owner's transaction: %v", m.rolledBack)
	}
}

// scopeCtxKey tags the contexts the Advance tests hand around, so a scope that
// moved onto the wrong one is identifiable rather than merely non-nil.
type scopeCtxKey struct{}

// TestTxScope_Advance_IgnoresIncompleteSegment pins Advance's guard. An engine
// result that names no complete segment must leave the scope on the one it
// already holds.
//
// The empty-txID row is the load-bearing one and its failure mode is silent:
// without the guard, Advance would set s.txID = "", Release's own txID == ""
// early return would then decline to roll anything back, and the segment would
// leak with no error anywhere — the exact bug class this scope exists to close.
func TestTxScope_Advance_IgnoresIncompleteSegment(t *testing.T) {
	for _, tc := range []struct {
		name string
		ctx  context.Context
		txID string
	}{
		{"nil ctx", nil, "tx-post"},
		{"empty txID", context.WithValue(context.Background(), scopeCtxKey{}, "segment"), ""},
		{"neither", nil, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := &scopeTxMgr{}
			entryCtx := context.WithValue(context.Background(), scopeCtxKey{}, "entry")
			s := &txScope{h: newScopeHandler(m), ctx: entryCtx, txID: "tx-1", owned: true}

			s.Advance(tc.ctx, tc.txID)

			if s.TxID() != "tx-1" {
				t.Fatalf("txID = %q, want the prior segment %q", s.TxID(), "tx-1")
			}
			if s.Ctx() != entryCtx {
				t.Fatalf("ctx moved off the prior segment: %v", s.Ctx().Value(scopeCtxKey{}))
			}

			// The consequence: the prior segment must still be releasable.
			s.Release()
			if len(m.rolledBack) != 1 || m.rolledBack[0] != "tx-1" {
				t.Fatalf("prior segment leaked after an incomplete advance: %v", m.rolledBack)
			}
		})
	}
}

// TestTxScope_ReleaseWithoutSegment_IsNoOp pins Release's txID == "" guard. An
// empty txID names no segment, and txgate hands out a no-op gate for it, so a
// rollback here would be both meaningless and ungated.
func TestTxScope_ReleaseWithoutSegment_IsNoOp(t *testing.T) {
	m := &scopeTxMgr{}
	s := &txScope{h: newScopeHandler(m), ctx: context.Background(), txID: "", owned: true}
	s.Release()
	if len(m.rolledBack) != 0 {
		t.Fatalf("rolled back a scope that names no segment: %d rollback(s) %q", len(m.rolledBack), m.rolledBack)
	}
}

// TestTxScope_Release_SealWaitDoesNotConsumeRollbackBudget pins the ordering
// common.RollbackContext documents: the budget "bounds the Rollback call
// itself. It does NOT bound the wait to reach it". Deriving the deadline before
// the seal would charge the seal's wait for a callback in progress against the
// rollback, and a rollback handed an already-expired context fails — on
// postgres destroying the pooled connection instead of returning it.
//
// Deterministic by construction, not by timing: the test stamps scopeEndedAt
// before it ends the callback scope the seal waits for, so a deadline derived
// AFTER the seal is necessarily at least a full budget past that stamp, while
// one derived before the seal is necessarily short by however long the scope
// stayed open.
func TestTxScope_Release_SealWaitDoesNotConsumeRollbackBudget(t *testing.T) {
	l := newLiveScope(t)

	// Read the budget off RollbackContext itself rather than restating it.
	// Measuring it after the fact under-reports slightly, which only widens the
	// margin in the passing direction.
	probeCtx, probeCancel := common.RollbackContext(context.Background())
	probeDeadline, _ := probeCtx.Deadline()
	probeCancel()
	budget := time.Until(probeDeadline)

	endCallback := l.openCallback(t)
	done := make(chan struct{})
	go func() {
		defer close(done)
		l.s.Release()
	}()

	// Wait until Release is provably parked in the seal — observed, not slept.
	waitForSealWait(t, done)
	// Hold it a little longer so a deadline derived before the seal is
	// unambiguously short, rather than short by a clock tick.
	time.Sleep(50 * time.Millisecond)

	scopeEndedAt := time.Now()
	endCallback(nil)
	<-done

	if len(l.m.rbDeadline) != 1 {
		t.Fatalf("expected exactly one rollback, got %d", len(l.m.rbDeadline))
	}
	if got := l.m.rbDeadline[0].Sub(scopeEndedAt); got < budget {
		t.Fatalf("rollback budget consumed by the seal's wait: %v left after the scope ended, want the full %v", got, budget)
	}
}

// TestTxScope_ReleaseAfterCommit_IsNoOp — no path rolls back after a commit,
// successful or not, and aborting a commit another goroutine is running would
// trip memory's ErrTxCommitInProgress path.
func TestTxScope_ReleaseAfterCommit_IsNoOp(t *testing.T) {
	for _, tc := range []struct {
		name      string
		commitErr error
	}{
		{"successful commit", nil},
		{"failed commit", errors.New("commit failed")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := &scopeTxMgr{commitErr: tc.commitErr}
			s := &txScope{h: newScopeHandler(m), ctx: context.Background(), txID: "tx-1", owned: true}
			_ = s.Commit()
			s.Release()
			if len(m.rolledBack) != 0 {
				t.Fatalf("released after commit: %v", m.rolledBack)
			}
		})
	}
}

// TestTxScope_Commit_PreCommitCheckFailsClosed pins the spec-D2 pre-commit
// check: when the owned scope's ctx is already expired, Commit must fail
// closed with ctx.Err() BEFORE marking the scope done, so the deferred
// Release still rolls back — that ordering IS the "nothing committed"
// guarantee the 408 contract depends on.
func TestTxScope_Commit_PreCommitCheckFailsClosed(t *testing.T) {
	m := &scopeTxMgr{}
	h := newScopeHandler(m)
	ctx, cancel := common.WithRequestTimeout(context.Background(), 1)
	defer cancel()
	<-ctx.Done() // deadline expires before commit

	s := &txScope{h: h, ctx: ctx, txID: "tx-1", owned: true}
	err := s.Commit()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want DeadlineExceeded from pre-commit check, got %v", err)
	}
	if len(m.committed) != 0 {
		t.Fatalf("commit ran despite an already-expired ctx: %v", m.committed)
	}

	// Scope must NOT be done: Release must still roll back.
	s.Release()
	if len(m.rolledBack) != 1 || m.rolledBack[0] != "tx-1" {
		t.Fatalf("scope was not still releasable after a failed pre-commit check: %v", m.rolledBack)
	}
}

// TestTxScope_Commit_Joined_SkipsPreCommitCheck pins that the pre-commit
// check applies only to the owned path. A joined callback never commits (the
// owner does), so an expired ctx on the joined path must not surface as an
// error here — commitOwned's existing owned==false no-op is unaffected.
func TestTxScope_Commit_Joined_SkipsPreCommitCheck(t *testing.T) {
	m := &scopeTxMgr{}
	h := newScopeHandler(m)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	s := &txScope{h: h, ctx: ctx, txID: "tx-owner", owned: false}
	if err := s.Commit(); err != nil {
		t.Fatalf("joined Commit must stay a no-op regardless of ctx state, got %v", err)
	}
	if len(m.committed) != 0 {
		t.Fatalf("joined Commit must never call txMgr.Commit: %v", m.committed)
	}
}

// TestTxScope_Commit_RunsOnShieldedCtx pins that the commit itself runs on a
// common.CommitContext derivative: WithoutCancel + its own bounded budget, so
// no deadline or disconnect on the request ctx can interrupt a commit in
// flight (an interrupted commit is an in-doubt outcome).
//
// The assertion runs from inside txMgr.Commit (via onCommit), i.e. while the
// commit is genuinely in flight. Checking after s.Commit() returns would be
// confounded: commitOwned's own `defer cancel()` fires the instant Commit
// returns, so the recorded ctx would already read Done() regardless of
// whether the request ctx ever influenced it.
func TestTxScope_Commit_RunsOnShieldedCtx(t *testing.T) {
	m := &scopeTxMgr{}
	h := newScopeHandler(m)
	reqCtx, reqCancel := common.WithRequestTimeout(context.Background(), 1000)
	defer reqCancel()
	reqDeadline, _ := reqCtx.Deadline()

	m.onCommit = func(ctx context.Context) {
		// Cancel the request ctx while the commit is "in flight" and prove
		// the shielded ctx does not observe it.
		reqCancel()
		if err := ctx.Err(); err != nil {
			t.Errorf("commit ctx observed the request cancellation mid-commit: %v", err)
		}
		if dl, ok := ctx.Deadline(); !ok || dl.Equal(reqDeadline) {
			t.Errorf("commit ctx did not carry its own bounded budget distinct from the request deadline")
		}
	}

	s := &txScope{h: h, ctx: reqCtx, txID: "tx-1", owned: true}
	if err := s.Commit(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(m.committed) != 1 {
		t.Fatalf("commit did not run: %v", m.committed)
	}
}

// TestTxScope_ReleaseOnCancelledContext_StillRollsBack is coverage row 8a. The
// UserContext verifyTenant reads must survive; the cancellation must not.
func TestTxScope_ReleaseOnCancelledContext_StillRollsBack(t *testing.T) {
	m := &scopeTxMgr{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	s := &txScope{h: newScopeHandler(m), ctx: ctx, txID: "tx-1", owned: true}
	s.Release()
	if len(m.rolledBack) != 1 {
		t.Fatalf("cancelled request abandoned its transaction: %v", m.rolledBack)
	}
	if m.rbCtxErr[0] != nil {
		t.Fatalf("rollback ran on a cancelled context: %v", m.rbCtxErr[0])
	}
}

// TestTxScope_Release_WaitsForTheCallbackInProgress: the rollback does not
// run under a callback scope still in progress — the seal it starts with waits
// for the scope to end. That is what keeps a joined request's writes and the
// owner's rollback apart now that the owner takes no gate.
func TestTxScope_Release_WaitsForTheCallbackInProgress(t *testing.T) {
	l := newLiveScope(t)
	endCallback := l.openCallback(t)
	done := make(chan struct{})
	go func() {
		defer close(done)
		l.s.Release()
	}()

	waitForSealWait(t, done)
	if got := l.m.rolledBackTxs(); len(got) != 0 {
		t.Fatalf("rolled back %v under a callback scope still in progress", got)
	}
	endCallback(nil)
	<-done
	if got := l.m.rolledBackTxs(); len(got) != 1 || got[0] != l.s.txID {
		t.Fatalf("rolled back %v after the scope ended; want %s once", got, l.s.txID)
	}
	l.assertForgotten(t)
}

// TestTxScope_RefusedSeal_RollsBackAndForgets: a joined write failed, so the
// seal refuses with 409 TRANSACTION_ROLLBACK_ONLY. Commit gives that refusal
// without committing, the deferred Release rolls back, and the coordination
// holds nothing of the transaction afterwards.
func TestTxScope_RefusedSeal_RollsBackAndForgets(t *testing.T) {
	for _, tc := range []struct {
		name     string
		sealedBy func(*txScope) error
	}{
		{"seal before the final writes", func(s *txScope) error { return s.Seal() }},
		{"commit without a seal of its own", func(s *txScope) error { return s.Commit() }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l := newLiveScope(t)
			l.markRoot(t)

			err := tc.sealedBy(l.s)
			var appErr *common.AppError
			if !errors.As(err, &appErr) || appErr.Code != common.ErrCodeTransactionRollbackOnly {
				t.Fatalf("err = %v; want TRANSACTION_ROLLBACK_ONLY", err)
			}
			if err := l.s.Commit(); !errors.As(err, &appErr) || appErr.Code != common.ErrCodeTransactionRollbackOnly {
				t.Fatalf("Commit after the refused seal = %v; want the seal's TRANSACTION_ROLLBACK_ONLY", err)
			}
			if got := l.m.committedTxs(); len(got) != 0 {
				t.Fatalf("committed %v although the seal refused", got)
			}
			l.s.Release()
			if got := l.m.rolledBackTxs(); len(got) != 1 || got[0] != l.s.txID {
				t.Fatalf("rolled back %v; want %s once", got, l.s.txID)
			}
			l.assertForgotten(t)
		})
	}
}

// TestTxScope_JoinedSeal_IsNoOp: a joined scope does not seal the owner's
// transaction — it would wait for its own scope — and so is not refused by a
// mark either: the join layer's scope answers for the request.
func TestTxScope_JoinedSeal_IsNoOp(t *testing.T) {
	l := newLiveScope(t)
	l.markRoot(t)
	joined := &txScope{h: l.s.h, ctx: l.s.ctx, txID: l.s.txID, owned: false}
	if err := joined.Seal(); err != nil {
		t.Fatalf("joined Seal = %v; want nil", err)
	}
}

// waitForSealWait returns once a goroutine is provably parked in txcoord's
// Seal, waiting for a callback scope to end, or once done fires, meaning the
// seal did not wait — which the caller's assertions then report. Polling the
// goroutine dump is what removes the sleep: the caller resumes on an observed
// state, not on elapsed time. The deadline is a last-resort guard against a
// runtime symbol rename silently turning this into a hang, not a timing
// assumption.
func waitForSealWait(t *testing.T, done <-chan struct{}) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	buf := make([]byte, 1<<16)
	for {
		select {
		case <-done:
			return
		default:
		}
		var dump string
		for {
			n := runtime.Stack(buf, true)
			if n < len(buf) {
				dump = string(buf[:n])
				break
			}
			buf = make([]byte, 2*len(buf))
		}
		for _, g := range strings.Split(dump, "\n\n") {
			if strings.Contains(g, "txcoord.(*inProcess).Seal") && strings.Contains(g, "[chan receive") {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Errorf("no goroutine parked in txcoord's Seal within 10s; dump:\n%s", dump)
			return
		}
		runtime.Gosched()
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
	// The cause stays reachable so the server-side log records WHY the pool
	// failed; a 503 with no breadcrumb is undiagnosable.
	if !errors.Is(appErr, stubUnavailable{}) {
		t.Fatal("classifier dropped the cause; the 503 leaves no server-side breadcrumb")
	}
	// ...but it never reaches the client. A pool error can carry the DSN.
	if strings.Contains(appErr.Message, stubUnavailableDSN) {
		t.Fatalf("client-facing message leaked the cause: %q", appErr.Message)
	}
}

// TestClassifyBeginErr_OtherFailureStaysInternal is coverage row 11's unit half.
func TestClassifyBeginErr_OtherFailureStaysInternal(t *testing.T) {
	appErr := classifyBeginErr(errors.New("connection refused"))
	if appErr.Status != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", appErr.Status)
	}
}

// stubUnavailableDSN stands in for the kind of connection detail a real pool
// error carries. Synthetic — it exists only so the leak assertion has something
// recognisable to look for.
const stubUnavailableDSN = "postgres://u:p@db/cyoda"

type stubUnavailable struct{}

func (stubUnavailable) Error() string            { return "acquire timed out: " + stubUnavailableDSN }
func (stubUnavailable) StorageUnavailable() bool { return true }

// suMarked is a storage plugin's marker for a transient outage.
type suMarked struct{}

func (suMarked) Error() string            { return "the session ended" }
func (suMarked) StorageUnavailable() bool { return true }

// TestClassifyCommitErr: what a refused seal or a failed commit answers. The
// seal's own refusal passes through; a lost race and a first-committer-wins
// refusal are the retryable 409; a transaction broken by a dead session keeps
// the storage plugin's retryable 503; anything else is a 500.
func TestClassifyCommitErr(t *testing.T) {
	for _, tc := range []struct {
		name      string
		err       error
		status    int
		code      string
		retryable bool
	}{
		{"rollback-only", common.TransactionRollbackOnly(common.ErrCodeWorkflowFailed), http.StatusConflict, common.ErrCodeTransactionRollbackOnly, false},
		{"lost race", fmt.Errorf("the transaction lost a write race: %w", spi.ErrTxAborted), http.StatusConflict, common.ErrCodeConflict, true},
		{"commit conflict", fmt.Errorf("commit: %w", spi.ErrConflict), http.StatusConflict, common.ErrCodeConflict, true},
		{"broken by a dead session", fmt.Errorf("the transaction is unusable: %w", suMarked{}), http.StatusServiceUnavailable, common.ErrCodeStorageUnavailable, true},
		{"anything else", errors.New("disk on fire"), http.StatusInternalServerError, common.ErrCodeServerError, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := classifyCommitErr(tc.err)
			if got.Status != tc.status || got.Code != tc.code || got.Retryable != tc.retryable {
				t.Fatalf("classifyCommitErr = %d %s retryable=%t; want %d %s retryable=%t", got.Status, got.Code, got.Retryable, tc.status, tc.code, tc.retryable)
			}
		})
	}
}
