package sqlite_test

import (
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// A savepoint survives its own rollback, so the restored buffer must not alias
// the snapshot: a write after the first rollback must not leak into the second.
func TestSavepoint_RestoredTwiceIsNotAliased(t *testing.T) {
	factory, ctx := newTxMgrForTenantTest(t)
	tm, err := factory.TransactionManager(ctx)
	if err != nil {
		t.Fatalf("TransactionManager failed: %v", err)
	}

	txID, txCtx, err := tm.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin failed: %v", err)
	}
	tx := spi.GetTransaction(txCtx)
	tx.Buffer["e"] = &spi.Entity{
		Meta: spi.EntityMeta{ID: "e", TenantID: "tenant-A", ChangeType: "CREATED"},
		Data: []byte(`{"v":"pre"}`),
	}
	tx.WriteSet["e"] = true

	spA, err := tm.Savepoint(ctx, txID)
	if err != nil {
		t.Fatalf("Savepoint failed: %v", err)
	}
	if err := tm.RollbackToSavepoint(ctx, txID, spA); err != nil {
		t.Fatalf("first RollbackToSavepoint failed: %v", err)
	}

	tx.Buffer["e"] = &spi.Entity{
		Meta: spi.EntityMeta{ID: "e", TenantID: "tenant-A", ChangeType: "UPDATED"},
		Data: []byte(`{"v":"post"}`),
	}
	tx.Buffer["extra"] = &spi.Entity{Meta: spi.EntityMeta{ID: "extra", TenantID: "tenant-A"}}
	tx.WriteSet["extra"] = true

	if err := tm.RollbackToSavepoint(ctx, txID, spA); err != nil {
		t.Fatalf("second RollbackToSavepoint failed: %v", err)
	}
	if got := string(tx.Buffer["e"].Data); got != `{"v":"pre"}` {
		t.Errorf("e = %s after second rollback, want the pre-savepoint content", got)
	}
	if _, ok := tx.Buffer["extra"]; ok || tx.WriteSet["extra"] {
		t.Error("extra written between the two rollbacks must be gone")
	}
}
