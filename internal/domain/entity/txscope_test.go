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
	"github.com/cyoda-platform/cyoda-go/internal/txgate"
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
	onRollback func()

	// events is an ordered trace of gate-relevant moments. The gate test
	// asserts mutual exclusion by comparing this ordering, which is why
	// "rollback-end" is recorded inside Rollback — Release still holds the
	// per-tx gate at that point and only frees it once Rollback returns.
	events []string
}

func (m *scopeTxMgr) record(ev string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.events = append(m.events, ev)
}

func (m *scopeTxMgr) trace() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.events)
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
	m.events = append(m.events, "rollback-end")
	return nil
}

func (m *scopeTxMgr) Commit(_ context.Context, txID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.committed = append(m.committed, txID)
	return m.commitErr
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
			s := &txScope{h: newScopeHandler(m), entryTxID: "tx-1", ctx: entryCtx, txID: "tx-1", owned: true}

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
	s := &txScope{h: newScopeHandler(m), entryTxID: "", ctx: context.Background(), txID: "", owned: true}
	s.Release()
	if len(m.rolledBack) != 0 {
		t.Fatalf("rolled back a scope that names no segment: %d rollback(s) %q", len(m.rolledBack), m.rolledBack)
	}
}

// TestTxScope_Release_GateWaitDoesNotConsumeRollbackBudget pins the ordering
// common.RollbackContext documents: the budget "bounds the Rollback call
// itself. It does NOT bound the wait to reach it". Deriving the deadline before
// acquiring the gate would charge the queue against the rollback, and a rollback
// handed an already-expired context fails — on postgres destroying the pooled
// connection instead of returning it.
//
// Deterministic by construction, not by timing: the test stamps gateFreedAt
// before it frees the gate, so a deadline derived AFTER the acquire is
// necessarily at least a full budget past that stamp, while one derived before
// the acquire is necessarily short by however long the gate was held.
func TestTxScope_Release_GateWaitDoesNotConsumeRollbackBudget(t *testing.T) {
	m := &scopeTxMgr{}
	h := newScopeHandler(m)

	// Read the budget off RollbackContext itself rather than restating it.
	// Measuring it after the fact under-reports slightly, which only widens the
	// margin in the passing direction.
	probeCtx, probeCancel := common.RollbackContext(context.Background())
	probeDeadline, _ := probeCtx.Deadline()
	probeCancel()
	budget := time.Until(probeDeadline)

	s := &txScope{h: h, entryTxID: "tx-1", ctx: context.Background(), txID: "tx-1", owned: true}

	release := h.gate.Acquire("tx-1")
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.Release()
	}()

	// Wait until Release is provably parked on the gate — observed, not slept.
	waitForGateContention(t, done)
	// Hold it a little longer so a deadline derived before the acquire is
	// unambiguously short, rather than short by a clock tick.
	time.Sleep(50 * time.Millisecond)

	gateFreedAt := time.Now()
	release()
	<-done

	if len(m.rbDeadline) != 1 {
		t.Fatalf("expected exactly one rollback, got %d", len(m.rbDeadline))
	}
	if got := m.rbDeadline[0].Sub(gateFreedAt); got < budget {
		t.Fatalf("rollback budget consumed by the gate wait: %v left after the gate freed, want the full %v", got, budget)
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
//
// The assertion is an event ordering, not a sleep. A competing acquirer is
// launched from inside the rollback, and the rollback does not return until that
// competitor has reached a decided outcome: either it is parked inside
// txgate.Acquire (the gate is held — correct), or it walked straight through
// (the gate is not held — the defect). Both outcomes are observed, never waited
// out, so "competitor-acquired" lands after "rollback-end" if and only if
// Release held the gate across the whole rollback.
func TestTxScope_Release_HoldsTheGate(t *testing.T) {
	h := newScopeHandler(nil)
	acquired := make(chan struct{})

	var m *scopeTxMgr
	m = &scopeTxMgr{onRollback: func() {
		m.record("rollback-start")
		started := make(chan struct{})
		go func() {
			close(started)
			release := h.gate.Acquire("tx-1")
			m.record("competitor-acquired")
			release()
			close(acquired)
		}()
		<-started
		waitForGateContention(t, acquired)
	}}
	h.txMgr = m

	s := &txScope{h: h, entryTxID: "tx-1", ctx: context.Background(), txID: "tx-1", owned: true}
	s.Release()
	<-acquired

	want := []string{"rollback-start", "rollback-end", "competitor-acquired"}
	if got := m.trace(); !slices.Equal(got, want) {
		t.Fatalf("Release rolled back without holding the per-tx gate: events = %v, want %v", got, want)
	}
}

// waitForGateContention returns once a goroutine is provably parked on a mutex
// inside txgate.Registry.Acquire, or once acquired fires — meaning no gate was
// held and the competitor walked in, which the caller's ordering assertion then
// reports. Polling the goroutine dump is what removes the sleep: the caller
// resumes on an observed state, not on elapsed time. The deadline is a
// last-resort guard against a runtime symbol rename silently turning this into
// a hang, not a timing assumption.
func waitForGateContention(t *testing.T, acquired <-chan struct{}) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	buf := make([]byte, 1<<16)
	for {
		select {
		case <-acquired:
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
			if !strings.Contains(g, "txgate.(*Registry).Acquire") {
				continue
			}
			if strings.Contains(g, "sync.runtime_SemacquireMutex") || strings.Contains(g, "sync.(*Mutex).lockSlow") {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Errorf("no goroutine parked in txgate.Acquire within 10s; dump:\n%s", dump)
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
