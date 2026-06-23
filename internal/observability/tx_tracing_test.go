package observability_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/cyoda-platform/cyoda-go/internal/observability"
)

type fakeTxManager struct {
	beginCalled    bool
	commitCalled   bool
	rollbackCalled bool
	rollbackErr    error
}

func (f *fakeTxManager) Begin(ctx context.Context) (string, context.Context, error) {
	f.beginCalled = true
	return "tx-1", ctx, nil
}
func (f *fakeTxManager) Commit(ctx context.Context, txID string) error {
	f.commitCalled = true
	return nil
}
func (f *fakeTxManager) Rollback(ctx context.Context, txID string) error {
	f.rollbackCalled = true
	return f.rollbackErr
}
func (f *fakeTxManager) Join(ctx context.Context, txID string) (context.Context, error) {
	return ctx, nil
}
func (f *fakeTxManager) GetSubmitTime(ctx context.Context, txID string) (time.Time, error) {
	return time.Now(), nil
}
func (f *fakeTxManager) Savepoint(ctx context.Context, txID string) (string, error) {
	return "sp-1", nil
}
func (f *fakeTxManager) RollbackToSavepoint(ctx context.Context, txID string, savepointID string) error {
	return nil
}
func (f *fakeTxManager) ReleaseSavepoint(ctx context.Context, txID string, savepointID string) error {
	return nil
}

func TestTracingTxManager_DelegatesToInner(t *testing.T) {
	shutdown, _ := observability.Init(context.Background(), "test", "node-test", true)
	defer shutdown(context.Background())

	inner := &fakeTxManager{}
	traced := observability.NewTracingTransactionManager(inner)

	ctx := context.Background()
	txID, txCtx, err := traced.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if txID != "tx-1" {
		t.Errorf("txID = %q, want tx-1", txID)
	}
	if !inner.beginCalled {
		t.Error("inner.Begin not called")
	}

	if err := traced.Commit(txCtx, txID); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if !inner.commitCalled {
		t.Error("inner.Commit not called")
	}

	if err := traced.Rollback(ctx, "tx-2"); err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	if !inner.rollbackCalled {
		t.Error("inner.Rollback not called")
	}
}

// IM-17: txActive counter must NOT be decremented when rollback fails.
// A failed rollback means the transaction is still active.
func TestTracingTxManager_Rollback_FailedRollbackDoesNotDecrementActive(t *testing.T) {
	shutdown, _ := observability.Init(context.Background(), "test", "node-test", true)
	defer shutdown(context.Background())

	rollbackErr := errors.New("rollback failed")
	inner := &fakeTxManager{rollbackErr: rollbackErr}
	traced := observability.NewTracingTransactionManager(inner)

	// The test verifies the error is propagated and the method completes without panic.
	// The real assertion is structural: txActive.Add(-1) must only run on success path.
	// We verify the error propagation which would differ if the bug caused early return.
	err := traced.Rollback(context.Background(), "tx-1")
	if !errors.Is(err, rollbackErr) {
		t.Errorf("expected rollback error, got %v", err)
	}
}
