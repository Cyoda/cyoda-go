package sqlite_test

import (
	"bytes"
	"database/sql"
	"errors"
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// extensionLogRows counts every row of ref's extension log, deltas and
// savepoints alike.
func extensionLogRows(t *testing.T, db *sql.DB, tenant string, ref spi.ModelRef) int {
	t.Helper()
	return countExtensionRows(t, db, tenant, ref, "delta") +
		countExtensionRows(t, db, tenant, ref, "savepoint")
}

const lifecycleDelta = `[{"kind":"broaden_type","path":"x","payload":["NULL"]}]`

func saveLifecycleModel(t *testing.T, ms spi.ModelStore, ref spi.ModelRef, schema string) {
	t.Helper()
	if err := ms.Save(extTestCtx("t1"), &spi.ModelDescriptor{
		Ref: ref, State: spi.ModelUnlocked, ChangeLevel: spi.ChangeLevelStructural,
		Schema: []byte(schema), UpdateDate: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("Save: %v", err)
	}
}

// Save replaces the model's schema: what Get returns afterwards is the
// saved schema alone, with nothing of an earlier extension folded onto it.
func TestSQLite_Save_ReplacesExtendedSchema(t *testing.T) {
	f, dbPath := setupSQLiteExt(t)
	ctx := extTestCtx("t1")
	ms, err := f.ModelStore(ctx)
	if err != nil {
		t.Fatalf("ModelStore: %v", err)
	}
	ref := spi.ModelRef{EntityName: "Book", ModelVersion: "1"}
	saveLifecycleModel(t, ms, ref, `{"base":1}`)
	if err := ms.Lock(ctx, ref); err != nil {
		t.Fatalf("Lock: %v", err)
	}
	if err := ms.ExtendSchema(ctx, ref, spi.SchemaDelta(lifecycleDelta)); err != nil {
		t.Fatalf("ExtendSchema: %v", err)
	}

	if err := ms.Unlock(ctx, ref); err != nil {
		t.Fatalf("Unlock: %v", err)
	}
	saveLifecycleModel(t, ms, ref, `{"base":2}`)

	got, err := ms.Get(ctx, ref)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !bytes.Equal(got.Schema, []byte(`{"base":2}`)) {
		t.Errorf("Get after Save = %s, want the saved schema {\"base\":2}", got.Schema)
	}
	if n := extensionLogRows(t, openRawDB(t, dbPath), "t1", ref); n != 0 {
		t.Errorf("extension log after Save has %d rows, want 0", n)
	}
}

// Unlock keeps the schema the model had while locked — extensions
// included — and clears the extension log.
func TestSQLite_Unlock_FoldsExtensionsIntoSchema(t *testing.T) {
	f, dbPath := setupSQLiteExt(t)
	ctx := extTestCtx("t1")
	ms, err := f.ModelStore(ctx)
	if err != nil {
		t.Fatalf("ModelStore: %v", err)
	}
	ref := spi.ModelRef{EntityName: "Book", ModelVersion: "1"}
	saveLifecycleModel(t, ms, ref, `{"base":1}`)
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
	if n := extensionLogRows(t, openRawDB(t, dbPath), "t1", ref); n != 0 {
		t.Errorf("extension log after Unlock has %d rows, want 0", n)
	}
}

// Delete removes the model's extension log with the model.
func TestSQLite_Delete_ClearsExtensionLog(t *testing.T) {
	f, dbPath := setupSQLiteExt(t)
	ctx := extTestCtx("t1")
	ms, err := f.ModelStore(ctx)
	if err != nil {
		t.Fatalf("ModelStore: %v", err)
	}
	ref := spi.ModelRef{EntityName: "Book", ModelVersion: "1"}
	saveLifecycleModel(t, ms, ref, `{"base":1}`)
	if err := ms.Lock(ctx, ref); err != nil {
		t.Fatalf("Lock: %v", err)
	}
	if err := ms.ExtendSchema(ctx, ref, spi.SchemaDelta(lifecycleDelta)); err != nil {
		t.Fatalf("ExtendSchema: %v", err)
	}

	if err := ms.Unlock(ctx, ref); err != nil {
		t.Fatalf("Unlock: %v", err)
	}
	if err := ms.Delete(ctx, ref); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	if n := extensionLogRows(t, openRawDB(t, dbPath), "t1", ref); n != 0 {
		t.Errorf("extension log after Delete has %d rows, want 0", n)
	}
}

// Save over a LOCKED model is refused and leaves the stored model and its
// extension log untouched.
func TestModelStore_SaveAfterLockRefused(t *testing.T) {
	f, dbPath := setupSQLiteExt(t)
	ctx := extTestCtx("t1")
	ms, err := f.ModelStore(ctx)
	if err != nil {
		t.Fatalf("ModelStore: %v", err)
	}
	ref := spi.ModelRef{EntityName: "Book", ModelVersion: "1"}
	saveLifecycleModel(t, ms, ref, `{"base":1}`)
	stale, err := ms.Get(ctx, ref) // the UNLOCKED descriptor a racing writer holds
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if err := ms.Lock(ctx, ref); err != nil {
		t.Fatalf("Lock: %v", err)
	}
	if err := ms.ExtendSchema(ctx, ref, spi.SchemaDelta(lifecycleDelta)); err != nil {
		t.Fatalf("ExtendSchema: %v", err)
	}
	before, err := ms.Get(ctx, ref)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	rowsBefore := extensionLogRows(t, openRawDB(t, dbPath), "t1", ref)

	stale.Schema = []byte(`{"base":2}`)
	if err := ms.Save(ctx, stale); !errors.Is(err, spi.ErrModelLocked) {
		t.Fatalf("Save over LOCKED = %v, want ErrModelLocked", err)
	}

	after, err := ms.Get(ctx, ref)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if after.State != spi.ModelLocked || !bytes.Equal(after.Schema, before.Schema) {
		t.Errorf("stored model changed by a refused Save: state=%s schema=%s, want LOCKED %s", after.State, after.Schema, before.Schema)
	}
	if n := extensionLogRows(t, openRawDB(t, dbPath), "t1", ref); n != rowsBefore {
		t.Errorf("extension log rows = %d after refused Save, want %d", n, rowsBefore)
	}
}
