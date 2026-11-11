package txcoord

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/fence"
	"github.com/cyoda-platform/cyoda-go/plugins/memory"
)

// settle is how long a test waits before concluding that a call is blocked.
const settle = 50 * time.Millisecond

// watchdog bounds every wait: a deadlock fails the test instead of hanging the
// package.
const watchdog = 5 * time.Second

// recTM is the memory plugin's transaction manager, recording the savepoint
// calls txcoord makes and answering LostRace and ReleaseSavepoint as a test
// arranges.
type recTM struct {
	spi.TransactionManager

	lost        atomic.Bool
	failRelease atomic.Pointer[error]

	mu    sync.Mutex
	calls []string
}

func (m *recTM) record(call string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls = append(m.calls, call)
}

func (m *recTM) recorded() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.calls...)
}

func (m *recTM) Savepoint(ctx context.Context, txID string) (string, error) {
	sp, err := m.TransactionManager.Savepoint(ctx, txID)
	if err == nil {
		m.record("savepoint " + sp)
	}
	return sp, err
}

func (m *recTM) ReleaseSavepoint(ctx context.Context, txID, sp string) error {
	m.record("release " + sp)
	if p := m.failRelease.Load(); p != nil {
		return *p
	}
	return m.TransactionManager.ReleaseSavepoint(ctx, txID, sp)
}

func (m *recTM) RollbackToSavepoint(ctx context.Context, txID, sp string) error {
	m.record("rollback " + sp)
	return m.TransactionManager.RollbackToSavepoint(ctx, txID, sp)
}

func (m *recTM) LostRace(ctx context.Context, txID string) (bool, error) {
	if m.lost.Load() {
		return true, nil
	}
	return m.TransactionManager.LostRace(ctx, txID)
}

// env is one owner transaction on the memory plugin with its coordination.
type env struct {
	t        *testing.T
	tm       *recTM
	c        *inProcess
	userCtx  context.Context
	ownerCtx context.Context // the owner's chain: carries the transaction, no scope
	txID     string
}

func newEnv(t *testing.T) *env {
	t.Helper()
	f := memory.NewStoreFactory()
	t.Cleanup(func() { _ = f.Close() })
	f.NewTransactionManager(common.NewDefaultUUIDGenerator())
	tm := &recTM{TransactionManager: f.GetTransactionManager()}
	userCtx := spi.WithUserContext(context.Background(), &spi.UserContext{
		UserID: "u1", Kind: spi.PrincipalUser, Tenant: spi.Tenant{ID: "tenant-a"},
	})
	txID, ownerCtx, err := tm.Begin(userCtx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	return &env{t: t, tm: tm, c: New(tm).(*inProcess), userCtx: userCtx, ownerCtx: ownerCtx, txID: txID}
}

// callout begins calloutID from the scope on ctx and makes major 1 current.
func (e *env) callout(ctx context.Context, calloutID string) (context.Context, func()) {
	e.t.Helper()
	cctx, end := e.c.BeginCallout(ctx, e.txID, calloutID)
	e.c.Advance(calloutID, 1)
	return cctx, end
}

// enter opens a callback scope under pairs (own pair first) on a freshly
// joined context, as the join layer does.
func (e *env) enter(class RequestClass, pairs ...fence.Pair) (Scope, context.Context, error) {
	e.t.Helper()
	joined, err := e.tm.Join(e.userCtx, e.txID)
	if err != nil {
		e.t.Fatalf("join: %v", err)
	}
	return e.c.Enter(joined, e.txID, pairs, class, 0)
}

func (e *env) mustEnter(class RequestClass, pairs ...fence.Pair) (Scope, context.Context) {
	e.t.Helper()
	s, ctx, err := e.enter(class, pairs...)
	if err != nil {
		e.t.Fatalf("enter %v: %v", pairs, err)
	}
	return s, ctx
}

// seal seals the transaction and forgets it.
func (e *env) seal() error {
	e.t.Helper()
	finish, err := e.c.Seal(e.userCtx, e.txID)
	finish()
	return err
}

// rootMarked reads the root's mark.
func (e *env) rootMarked() bool {
	e.c.mu.Lock()
	defer e.c.mu.Unlock()
	tx := e.c.txs[e.txID]
	return tx != nil && tx.root.marked
}

func pair(calloutID string, major uint32) fence.Pair {
	return fence.Pair{Callout: calloutID, Major: major}
}

// failed is a non-retryable handler failure.
func failed(status int, code string) *common.Outcome {
	return &common.Outcome{Status: status, Code: code, Err: common.Operational(status, code, "handler failed")}
}

func mustCode(t *testing.T, err error, status int, code string, retryable bool) {
	t.Helper()
	var appErr *common.AppError
	if !errors.As(err, &appErr) {
		t.Fatalf("error = %v (%T); want *common.AppError %d %s", err, err, status, code)
	}
	if appErr.Status != status || appErr.Code != code || appErr.Retryable != retryable {
		t.Fatalf("answer = %d %s retryable=%v; want %d %s retryable=%v",
			appErr.Status, appErr.Code, appErr.Retryable, status, code, retryable)
	}
}

// mustCalls asserts the savepoint calls recorded, in order, with the
// savepoint ids replaced by their kind alone.
func mustCalls(t *testing.T, tm *recTM, want ...string) {
	t.Helper()
	var got []string
	for _, c := range tm.recorded() {
		got = append(got, strings.Fields(c)[0])
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("savepoint calls = %v; want %v", got, want)
	}
}

func mustFinish(t *testing.T, what string, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(watchdog):
		t.Fatalf("%s did not finish within %v", what, watchdog)
	}
}

func mustStillBlock(t *testing.T, what string, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
		t.Fatalf("%s returned while it should still wait", what)
	case <-time.After(settle):
	}
}

// async runs f on a goroutine and returns the channel closed when it returns.
func async(f func()) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		f()
	}()
	return done
}

// deadSession is a dead PostgreSQL session's error: it matches
// spi.ErrTxNotFound and carries the storage-unavailable marker.
type deadSession struct{}

func (deadSession) Error() string            { return "the session ended" }
func (deadSession) StorageUnavailable() bool { return true }
func (deadSession) Is(target error) bool     { return target == spi.ErrTxNotFound }
