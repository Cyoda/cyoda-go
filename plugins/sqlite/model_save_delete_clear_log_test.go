package sqlite_test

import (
	"errors"
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// insertDeltaRow writes one delta row to ref's extension log directly, as a
// row an extension left behind.
func insertDeltaRow(t *testing.T, dbPath, tenant string, ref spi.ModelRef, delta string) {
	t.Helper()
	db := openRawDB(t, dbPath)
	if _, err := db.Exec(
		`INSERT INTO model_schema_extensions
		    (tenant_id, model_name, model_version, seq, kind, payload)
		 VALUES (?, ?, ?, 1, 'delta', ?)`,
		tenant, ref.EntityName, ref.ModelVersion, []byte(delta)); err != nil {
		t.Fatalf("insert delta row: %v", err)
	}
}

func unlockedModel(ref spi.ModelRef, schema string) *spi.ModelDescriptor {
	return &spi.ModelDescriptor{
		Ref: ref, State: spi.ModelUnlocked, ChangeLevel: spi.ChangeLevelStructural,
		Schema: []byte(schema), UpdateDate: time.Now().UTC(),
	}
}

// TestSQLite_Save_ClearsExtensionLog — Save replaces the model wholesale:
// rows in its extension log go with it, so a Get returns the saved schema
// alone.
func TestSQLite_Save_ClearsExtensionLog(t *testing.T) {
	f, dbPath := setupSQLiteExt(t)
	ctx := extTestCtx("t1")
	ms, err := f.ModelStore(ctx)
	if err != nil {
		t.Fatalf("ModelStore: %v", err)
	}
	ref := spi.ModelRef{EntityName: "Book", ModelVersion: "1"}
	if err := ms.Save(ctx, unlockedModel(ref, `{"base":true}`)); err != nil {
		t.Fatalf("Save: %v", err)
	}
	insertDeltaRow(t, dbPath, "t1", ref, `{"left":true}`)

	if err := ms.Save(ctx, unlockedModel(ref, `{"imported":true}`)); err != nil {
		t.Fatalf("Save over the model: %v", err)
	}
	got, err := ms.Get(ctx, ref)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if string(got.Schema) != `{"imported":true}` {
		t.Errorf("schema after Save = %s, want {\"imported\":true}", got.Schema)
	}
	if n := countExtensionRows(t, openRawDB(t, dbPath), "t1", ref, "delta"); n != 0 {
		t.Errorf("delta rows after Save = %d, want 0", n)
	}
}

// TestSQLite_Delete_ClearsExtensionLog — Delete removes the model and its
// extension log: a model saved again under the same reference starts from
// its saved schema alone.
func TestSQLite_Delete_ClearsExtensionLog(t *testing.T) {
	f, dbPath := setupSQLiteExt(t)
	ctx := extTestCtx("t1")
	ms, err := f.ModelStore(ctx)
	if err != nil {
		t.Fatalf("ModelStore: %v", err)
	}
	ref := spi.ModelRef{EntityName: "Book", ModelVersion: "1"}
	if err := ms.Save(ctx, unlockedModel(ref, `{"base":true}`)); err != nil {
		t.Fatalf("Save: %v", err)
	}
	insertDeltaRow(t, dbPath, "t1", ref, `{"left":true}`)

	if err := ms.Delete(ctx, ref); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if n := countExtensionRows(t, openRawDB(t, dbPath), "t1", ref, "delta"); n != 0 {
		t.Errorf("delta rows after Delete = %d, want 0", n)
	}
	if err := ms.Delete(ctx, ref); !errors.Is(err, spi.ErrNotFound) {
		t.Errorf("second Delete = %v, want spi.ErrNotFound", err)
	}
}
