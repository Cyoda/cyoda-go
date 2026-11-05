package postgres_test

import (
	"bytes"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

const lifecycleDelta = `[{"kind":"broaden_type","path":"x","payload":["NULL"]}]`

// Unlock keeps the schema the model had while locked — extensions
// included — and clears the extension log.
func TestUnlock_FoldsExtensionsIntoSchema(t *testing.T) {
	factory := setupModelExtTest(t)
	factory.SetApplyFunc(jsonRecordingApplyFunc())
	ctx := ctxWithTenant("t1")
	ms, err := factory.ModelStore(ctx)
	if err != nil {
		t.Fatalf("ModelStore: %v", err)
	}
	ref := spi.ModelRef{EntityName: "Book", ModelVersion: "1"}
	seedBaseModel(t, ms, ctx, ref, `{"base":1}`, spi.ChangeLevelStructural)
	if err := ms.Lock(ctx, ref); err != nil {
		t.Fatalf("Lock: %v", err)
	}
	if err := ms.ExtendSchema(ctx, ref, spi.SchemaDelta(lifecycleDelta)); err != nil {
		t.Fatalf("ExtendSchema: %v", err)
	}
	before, err := ms.Get(ctx, ref)
	if err != nil {
		t.Fatalf("Get before Unlock: %v", err)
	}

	if err := ms.Unlock(ctx, ref); err != nil {
		t.Fatalf("Unlock: %v", err)
	}

	after, err := ms.Get(ctx, ref)
	if err != nil {
		t.Fatalf("Get after Unlock: %v", err)
	}
	if after.State != spi.ModelUnlocked {
		t.Errorf("state after Unlock = %s, want UNLOCKED", after.State)
	}
	if !bytes.Equal(after.Schema, before.Schema) {
		t.Errorf("schema after Unlock = %s, want the locked model's schema %s", after.Schema, before.Schema)
	}
	if n := countExtensionRows(t, factory.Pool(), "t1", ref); n != 0 {
		t.Errorf("extension log after Unlock has %d rows, want 0", n)
	}
}

// Delete removes the model's extension log with the model.
func TestDelete_ClearsExtensionLog(t *testing.T) {
	factory := setupModelExtTest(t)
	factory.SetApplyFunc(jsonRecordingApplyFunc())
	ctx := ctxWithTenant("t1")
	ms, err := factory.ModelStore(ctx)
	if err != nil {
		t.Fatalf("ModelStore: %v", err)
	}
	ref := spi.ModelRef{EntityName: "Book", ModelVersion: "1"}
	seedBaseModel(t, ms, ctx, ref, `{"base":1}`, spi.ChangeLevelStructural)
	if err := ms.ExtendSchema(ctx, ref, spi.SchemaDelta(lifecycleDelta)); err != nil {
		t.Fatalf("ExtendSchema: %v", err)
	}

	if err := ms.Delete(ctx, ref); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	if n := countExtensionRows(t, factory.Pool(), "t1", ref); n != 0 {
		t.Errorf("extension log after Delete has %d rows, want 0", n)
	}
}
