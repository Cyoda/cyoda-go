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
