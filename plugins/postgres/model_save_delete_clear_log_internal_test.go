package postgres

import (
	"context"
	"errors"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// insertDeltaRow writes one delta row to ref's extension log directly, as a
// row an extension left behind.
func (f *pgFixture) insertDeltaRow(t *testing.T, ref spi.ModelRef, delta string) {
	t.Helper()
	if _, err := f.db.Exec(context.Background(), `
		INSERT INTO model_schema_extensions (tenant_id, model_name, model_version, kind, payload)
		VALUES ($1, $2, $3, 'delta', $4)`,
		string(f.tenantID), ref.EntityName, ref.ModelVersion, []byte(delta)); err != nil {
		t.Fatalf("insert delta row: %v", err)
	}
}

// TestSave_ClearsExtensionLog — Save replaces the model wholesale: rows in
// its extension log go with it, so a Get returns the saved schema alone.
func TestSave_ClearsExtensionLog(t *testing.T) {
	fx := newPGFixture(t)
	fx.store.applyFunc = setUnionApplyFunc
	ref := spi.ModelRef{EntityName: "E", ModelVersion: "1"}
	fx.saveModel(t, ref, []byte(`["base"]`), spi.ModelUnlocked)
	fx.insertDeltaRow(t, ref, `"left"`)

	fx.saveModel(t, ref, []byte(`["imported"]`), spi.ModelUnlocked)

	got, err := fx.store.Get(fx.ctx, ref)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if string(got.Schema) != `["imported"]` {
		t.Errorf("schema after Save = %s, want [\"imported\"]", got.Schema)
	}
	if n := fx.countRows(t, ref, ""); n != 0 {
		t.Errorf("extension rows after Save = %d, want 0", n)
	}
}

// TestDelete_ClearsExtensionLog — Delete removes the model and its extension
// log: a model saved again under the same reference starts from its saved
// schema alone.
func TestDelete_ClearsExtensionLog(t *testing.T) {
	fx := newPGFixture(t)
	fx.store.applyFunc = setUnionApplyFunc
	ref := spi.ModelRef{EntityName: "E", ModelVersion: "1"}
	fx.saveModel(t, ref, []byte(`["base"]`), spi.ModelUnlocked)
	fx.insertDeltaRow(t, ref, `"left"`)

	if err := fx.store.Delete(fx.ctx, ref); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if n := fx.countRows(t, ref, ""); n != 0 {
		t.Errorf("extension rows after Delete = %d, want 0", n)
	}
	if err := fx.store.Delete(fx.ctx, ref); !errors.Is(err, spi.ErrNotFound) {
		t.Errorf("second Delete = %v, want spi.ErrNotFound", err)
	}
}
