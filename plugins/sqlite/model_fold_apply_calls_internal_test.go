package sqlite

import (
	"sync/atomic"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// TestFold_OneApplyCall: the apply function decodes and re-encodes the whole
// schema on every call, so the fold makes one call for all the deltas after
// the savepoint, and an extension makes two — the fold and the fold plus the
// new delta — however long the log is, a savepoint included (it stores the
// second result).
func TestFold_OneApplyCall(t *testing.T) {
	setup := func(t *testing.T, interval int) (*sqliteFixture, *atomic.Int64, spi.ModelRef) {
		fx := newSQLiteFixtureWithInterval(t, interval)
		calls := &atomic.Int64{}
		fx.store.applyFunc = func(base []byte, deltas []spi.SchemaDelta) ([]byte, error) {
			calls.Add(1)
			return setUnionApplyFunc(base, deltas)
		}
		ref := spi.ModelRef{EntityName: "fold-calls", ModelVersion: "1"}
		fx.SaveModel(t, ref, []byte(`[]`))
		if err := fx.store.Lock(fx.ctx, ref); err != nil {
			t.Fatalf("Lock: %v", err)
		}
		return fx, calls, ref
	}
	countRows := func(t *testing.T, fx *sqliteFixture, ref spi.ModelRef, kind string) int {
		t.Helper()
		var n int
		if err := fx.store.db.QueryRowContext(fx.ctx,
			`SELECT COUNT(*) FROM model_schema_extensions
			 WHERE tenant_id = ? AND model_name = ? AND model_version = ? AND kind = ?`,
			string(fx.tenantID), ref.EntityName, ref.ModelVersion, kind).Scan(&n); err != nil {
			t.Fatalf("count %s rows: %v", kind, err)
		}
		return n
	}
	extend := func(t *testing.T, fx *sqliteFixture, ref spi.ModelRef, ds ...string) {
		t.Helper()
		for _, d := range ds {
			if err := fx.store.ExtendSchema(fx.ctx, ref, spi.SchemaDelta(d)); err != nil {
				t.Fatalf("ExtendSchema %s: %v", d, err)
			}
		}
	}

	t.Run("log", func(t *testing.T) {
		fx, calls, ref := setup(t, 1000)
		extend(t, fx, ref, `"d1"`, `"d2"`, `"d3"`, `"d4"`, `"d5"`)
		if got := countRows(t, fx, ref, "delta"); got != 5 {
			t.Fatalf("delta rows = %d, want 5", got)
		}

		calls.Store(0)
		desc, err := fx.store.Get(fx.ctx, ref)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if got := calls.Load(); got != 1 {
			t.Errorf("Get over 5 deltas made %d apply calls, want 1", got)
		}
		if string(desc.Schema) != `["d1","d2","d3","d4","d5"]` {
			t.Errorf("folded schema = %s", desc.Schema)
		}

		calls.Store(0)
		extend(t, fx, ref, `"d6"`)
		if got := calls.Load(); got != 2 {
			t.Errorf("an extension over 5 deltas made %d apply calls, want 2", got)
		}

		calls.Store(0)
		extend(t, fx, ref, `"d3"`)
		if got := calls.Load(); got != 2 {
			t.Errorf("a no-op extension over 6 deltas made %d apply calls, want 2", got)
		}
		if got := countRows(t, fx, ref, "delta"); got != 6 {
			t.Errorf("delta rows = %d, want 6 (the held delta writes nothing)", got)
		}
	})

	t.Run("savepoint", func(t *testing.T) {
		fx, calls, ref := setup(t, 3)
		extend(t, fx, ref, `"d1"`, `"d2"`)
		calls.Store(0)
		extend(t, fx, ref, `"d3"`)
		if got := countRows(t, fx, ref, "savepoint"); got != 1 {
			t.Fatalf("savepoint rows = %d, want 1", got)
		}
		if got := calls.Load(); got != 2 {
			t.Errorf("an extension writing a savepoint made %d apply calls, want 2", got)
		}
		desc, err := fx.store.Get(fx.ctx, ref)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if string(desc.Schema) != `["d1","d2","d3"]` {
			t.Errorf("schema from the savepoint = %s", desc.Schema)
		}
	})
}
