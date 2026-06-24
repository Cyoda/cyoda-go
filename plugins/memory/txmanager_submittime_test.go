package memory_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/plugins/memory"
)

func TestGetSubmitTime_CommittedTx(t *testing.T) {
	factory := memory.NewStoreFactory()
	uuids := newTestUUIDGenerator()
	tm := factory.NewTransactionManager(uuids)
	ctx := ctxWithTenant("tenant-A")

	before := time.Now()

	txID, _, err := tm.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin failed: %v", err)
	}
	if err := tm.Commit(ctx, txID); err != nil {
		t.Fatalf("Commit failed: %v", err)
	}

	after := time.Now()

	submitTime, err := tm.GetSubmitTime(ctx, txID)
	if err != nil {
		t.Fatalf("GetSubmitTime failed: %v", err)
	}

	if submitTime.Before(before) || submitTime.After(after) {
		t.Fatalf("submitTime %v not between %v and %v", submitTime, before, after)
	}
}

func TestGetSubmitTime_ActiveTx(t *testing.T) {
	factory := memory.NewStoreFactory()
	uuids := newTestUUIDGenerator()
	tm := factory.NewTransactionManager(uuids)
	ctx := ctxWithTenant("tenant-A")

	txID, _, err := tm.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin failed: %v", err)
	}

	_, err = tm.GetSubmitTime(ctx, txID)
	if err == nil {
		t.Fatal("expected error for active (uncommitted) transaction")
	}
	if !strings.Contains(err.Error(), "not yet committed") {
		t.Fatalf("expected 'not yet committed' error, got: %v", err)
	}
}

func TestGetSubmitTime_NotFound(t *testing.T) {
	factory := memory.NewStoreFactory()
	uuids := newTestUUIDGenerator()
	tm := factory.NewTransactionManager(uuids)
	ctx := ctxWithTenant("tenant-A")

	_, err := tm.GetSubmitTime(ctx, "nonexistent-tx-id")
	if err == nil {
		t.Fatal("expected error for nonexistent transaction")
	}
	if !errors.Is(err, spi.ErrTxNotFound) {
		t.Fatalf("expected ErrTxNotFound, got: %v", err)
	}
}
