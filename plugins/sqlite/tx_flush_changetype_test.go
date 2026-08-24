package sqlite_test

import (
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// TestFlushToSQLite_ChangeTypeMatchesStoredValue is the regression test for
// the tx-commit flush path deriving the WRONG change type. Each business
// transaction below is committed on its own (Begin/Save/Commit), exactly how
// the workflow engine drives entity writes (internal/domain/workflow/engine.go),
// so this exercises flushToSQLite's per-transaction isNew/hasPrior handling,
// not just deriveChangeType's pure logic (already pinned by
// TestDeriveChangeType in change_type_test.go).
//
// Before the fix, flushToSQLite passed a "hasPrior" bool (true = a prior row
// exists) directly into deriveChangeType's "isNew" parameter (true = NO prior
// row exists) without negating it — an inverted-boolean bug. That rotated the
// labels: the CREATE's own version came back "UPDATED" and both UPDATEs came
// back "CREATED", exactly matching the memory/postgres divergence surfaced by
// e2e/parity's Attribution* scenarios and HistoryReadsChangesMetadataAndTransactionLookup.
func TestFlushToSQLite_ChangeTypeMatchesStoredValue(t *testing.T) {
	factory, tm := newAttrFactory(t)
	ctx := attrCtx("tenant-repro", "u1", spi.PrincipalUser)

	store, err := factory.EntityStore(ctx)
	if err != nil {
		t.Fatalf("EntityStore: %v", err)
	}

	doSave := func(changeType, data string) {
		t.Helper()
		txID, txCtx, err := tm.Begin(ctx)
		if err != nil {
			t.Fatalf("Begin: %v", err)
		}
		txStore, err := factory.EntityStore(txCtx)
		if err != nil {
			t.Fatalf("EntityStore(txCtx): %v", err)
		}
		if _, err := txStore.Save(txCtx, &spi.Entity{
			Meta: spi.EntityMeta{
				ID:         "e-flush-changetype",
				TenantID:   "tenant-repro",
				ModelRef:   spi.ModelRef{EntityName: "m", ModelVersion: "1"},
				ChangeType: changeType,
			},
			Data: []byte(data),
		}); err != nil {
			t.Fatalf("Save: %v", err)
		}
		if err := tm.Commit(txCtx, txID); err != nil {
			t.Fatalf("Commit: %v", err)
		}
	}

	// Mirrors the caller-supplied ChangeType values the engine actually
	// stamps: "CREATED" on the first save, "UPDATED" on later saves (see
	// internal/domain/entity/service.go lines setting ChangeType).
	doSave("CREATED", `{"v":1}`)
	doSave("UPDATED", `{"v":2}`)
	doSave("UPDATED", `{"v":3}`)

	metas, err := store.GetVersionMetadata(ctx, "e-flush-changetype", spi.VersionMetadataOptions{})
	if err != nil {
		t.Fatalf("GetVersionMetadata: %v", err)
	}
	if len(metas) != 3 {
		t.Fatalf("expected 3 versions, got %d: %+v", len(metas), metas)
	}

	// metas is newest-first (Version DESC): version 3, 2, 1.
	wantByVersion := map[int64]string{1: "CREATED", 2: "UPDATED", 3: "UPDATED"}
	for _, m := range metas {
		want, ok := wantByVersion[m.Version]
		if !ok {
			t.Fatalf("unexpected version %d in result: %+v", m.Version, metas)
		}
		if m.ChangeType != want {
			t.Errorf("version %d: changeType = %q, want %q (must match the caller-supplied value on flush, not row-existence rotated)", m.Version, m.ChangeType, want)
		}
	}
}

// TestFlushToSQLite_ChangeTypeSameTxDoubleSave covers the supersededSaves
// interaction called out for this fix: two Save calls to the SAME entity ID
// inside ONE business transaction (a same-tx double-save) must flush as two
// consecutive entity_versions rows sharing the transaction's ID — see
// supersededSaves's field godoc — with the first row CREATED (the entity
// truly has no prior row before this transaction) and the second row UPDATED
// (a prior row now exists from the first row flushed just before it, within
// the same commit). The fix must reset curIsNew to false only AFTER each
// staged item is processed, not before the loop starts.
func TestFlushToSQLite_ChangeTypeSameTxDoubleSave(t *testing.T) {
	factory, tm := newAttrFactory(t)
	ctx := attrCtx("tenant-repro-2", "u1", spi.PrincipalUser)

	store, err := factory.EntityStore(ctx)
	if err != nil {
		t.Fatalf("EntityStore: %v", err)
	}

	txID, txCtx, err := tm.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	txStore, err := factory.EntityStore(txCtx)
	if err != nil {
		t.Fatalf("EntityStore(txCtx): %v", err)
	}

	entity := &spi.Entity{
		Meta: spi.EntityMeta{
			ID:         "e-same-tx-double-save",
			TenantID:   "tenant-repro-2",
			ModelRef:   spi.ModelRef{EntityName: "m", ModelVersion: "1"},
			ChangeType: "CREATED",
		},
		Data: []byte(`{"v":1}`),
	}
	if _, err := txStore.Save(txCtx, entity); err != nil {
		t.Fatalf("Save 1: %v", err)
	}
	entity.Data = []byte(`{"v":2}`)
	entity.Meta.ChangeType = "UPDATED"
	if _, err := txStore.Save(txCtx, entity); err != nil {
		t.Fatalf("Save 2: %v", err)
	}
	if err := tm.Commit(txCtx, txID); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	metas, err := store.GetVersionMetadata(ctx, "e-same-tx-double-save", spi.VersionMetadataOptions{})
	if err != nil {
		t.Fatalf("GetVersionMetadata: %v", err)
	}
	if len(metas) != 2 {
		t.Fatalf("expected 2 versions (one per same-tx save), got %d: %+v", len(metas), metas)
	}

	wantByVersion := map[int64]string{1: "CREATED", 2: "UPDATED"}
	for _, m := range metas {
		want, ok := wantByVersion[m.Version]
		if !ok {
			t.Fatalf("unexpected version %d in result: %+v", m.Version, metas)
		}
		if m.ChangeType != want {
			t.Errorf("version %d: changeType = %q, want %q", m.Version, m.ChangeType, want)
		}
		if m.TransactionID != txID {
			t.Errorf("version %d: transactionID = %q, want %q (both rows share the one business tx)", m.Version, m.TransactionID, txID)
		}
	}
}
