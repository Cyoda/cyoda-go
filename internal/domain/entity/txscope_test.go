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
}

// TestClassifyBeginErr_OtherFailureStaysInternal is coverage row 11's unit half.
func TestClassifyBeginErr_OtherFailureStaysInternal(t *testing.T) {
	appErr := classifyBeginErr(errors.New("connection refused"))
	if appErr.Status != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", appErr.Status)
	}
}

type stubUnavailable struct{}

func (stubUnavailable) Error() string            { return "acquire timed out" }
func (stubUnavailable) StorageUnavailable() bool { return true }
