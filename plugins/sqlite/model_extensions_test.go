package sqlite_test

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/plugins/sqlite"
)

func recordingApplyFunc() sqlite.ApplyFunc {
	return func(base []byte, deltas []spi.SchemaDelta) ([]byte, error) {
		// Return valid JSON that carries both base and delta so tests can
		// assert Apply was invoked. Store {"base": <base>, "delta": <delta>},
		// nested once per delta. This isn't semantically meaningful; it's
		// just observable.
		for _, delta := range deltas {
			if len(base) == 0 {
				base = []byte(`{"base":null,"delta":` + string(delta) + `}`)
				continue
			}
			base = []byte(`{"base":` + string(base) + `,"delta":` + string(delta) + `}`)
		}
		return base, nil
	}
}

func extTestCtx(tenant string) context.Context {
	uc := &spi.UserContext{
		UserID: "test-user",
		Tenant: spi.Tenant{ID: spi.TenantID(tenant)},
	}
	return spi.WithUserContext(context.Background(), uc)
}

func setupSQLiteExt(t *testing.T) (*sqlite.StoreFactory, string) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "test.db")
	f, err := sqlite.NewStoreFactoryForTest(context.Background(), dbPath, sqlite.WithApplyFunc(recordingApplyFunc()))
	if err != nil {
		t.Fatalf("NewStoreFactoryForTest: %v", err)
	}
	t.Cleanup(func() { _ = f.Close() })
	return f, dbPath
}

// openRawDB opens a read-only handle on the plugin DB so tests can
// inspect the model_schema_extensions table directly. Using a second
// handle avoids reaching into unexported fields on StoreFactory while
// still letting the external test file assert log-level invariants.
func openRawDB(t *testing.T, dbPath string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func countExtensionRows(t *testing.T, db *sql.DB, tenant string, ref spi.ModelRef, kind string) int {
	t.Helper()
	var n int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM model_schema_extensions
		 WHERE tenant_id = ? AND model_name = ? AND model_version = ? AND kind = ?`,
		tenant, ref.EntityName, ref.ModelVersion, kind).Scan(&n); err != nil {
		t.Fatalf("count %s rows: %v", kind, err)
	}
	return n
}

// TestSQLite_ExtendSchema_AppendsToLog asserts that ExtendSchema writes a
// delta row to model_schema_extensions (rather than mutating models.doc)
// and that Get returns the folded result.
func TestSQLite_ExtendSchema_AppendsToLog(t *testing.T) {
	f, dbPath := setupSQLiteExt(t)
	ctx := extTestCtx("t1")
	ms, err := f.ModelStore(ctx)
	if err != nil {
		t.Fatalf("ModelStore: %v", err)
	}
	ref := spi.ModelRef{EntityName: "Book", ModelVersion: "1"}

	// Seed.
	if err := ms.Save(ctx, &spi.ModelDescriptor{
		Ref: ref, State: spi.ModelUnlocked, ChangeLevel: spi.ChangeLevelStructural,
		Schema: []byte(`{"initial":true}`), UpdateDate: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if err := ms.Lock(ctx, ref); err != nil {
		t.Fatalf("Lock: %v", err)
	}

	delta := spi.SchemaDelta(`[{"kind":"broaden_type","path":"x","payload":["NULL"]}]`)
	if err := ms.ExtendSchema(ctx, ref, delta); err != nil {
		t.Fatalf("ExtendSchema: %v", err)
	}

	// A delta row must now exist in the log.
	db := openRawDB(t, dbPath)
	if got := countExtensionRows(t, db, "t1", ref, "delta"); got != 1 {
		t.Errorf("expected 1 delta row, got %d", got)
	}

	// The base row in models.doc must NOT have been mutated — apply-in-place
	// is explicitly out. Verify by scanning the raw doc directly.
	var doc []byte
	if err := db.QueryRow(
		`SELECT json(doc) FROM models WHERE tenant_id = ? AND model_name = ? AND model_version = ?`,
		"t1", ref.EntityName, ref.ModelVersion).Scan(&doc); err != nil {
		t.Fatalf("read doc: %v", err)
	}
	if bytes.Contains(doc, []byte(`"delta"`)) {
		t.Errorf("models.doc leaked delta payload (apply-in-place regression): %s", doc)
	}

	// Get must fold: the folded schema should carry both base and delta
	// markers (recordingApplyFunc preserves both).
	got, err := ms.Get(ctx, ref)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !bytes.Contains(got.Schema, []byte(`"delta"`)) || !bytes.Contains(got.Schema, []byte(`"initial"`)) {
		t.Errorf("expected folded schema to contain both initial and delta, got %s", got.Schema)
	}
}

func TestSQLite_ExtendSchema_EmptyDeltaIsNoop(t *testing.T) {
	f, dbPath := setupSQLiteExt(t)
	ctx := extTestCtx("t1")
	ms, _ := f.ModelStore(ctx)
	ref := spi.ModelRef{EntityName: "Book", ModelVersion: "1"}
	_ = ms.Save(ctx, &spi.ModelDescriptor{
		Ref: ref, State: spi.ModelUnlocked, ChangeLevel: spi.ChangeLevelStructural,
		Schema: []byte(`{"s":1}`), UpdateDate: time.Now().UTC(),
	})

	if err := ms.ExtendSchema(ctx, ref, nil); err != nil {
		t.Fatalf("nil delta: %v", err)
	}
	if err := ms.ExtendSchema(ctx, ref, spi.SchemaDelta{}); err != nil {
		t.Fatalf("empty delta: %v", err)
	}
	// No delta row should have been appended.
	db := openRawDB(t, dbPath)
	if got := countExtensionRows(t, db, "t1", ref, "delta"); got != 0 {
		t.Errorf("expected 0 delta rows after empty extends, got %d", got)
	}
	// Get still returns the base (no deltas to fold).
	got, _ := ms.Get(ctx, ref)
	if !bytes.Equal(got.Schema, []byte(`{"s":1}`)) {
		t.Errorf("expected schema unchanged, got %s", got.Schema)
	}
}

func TestSQLite_ExtendSchema_MissingApplyFunc_Errors(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	f, err := sqlite.NewStoreFactoryForTest(context.Background(), dbPath) // no WithApplyFunc
	if err != nil {
		t.Fatalf("NewStoreFactoryForTest: %v", err)
	}
	defer f.Close()
	ctx := extTestCtx("t1")
	ms, _ := f.ModelStore(ctx)
	ref := spi.ModelRef{EntityName: "Book", ModelVersion: "1"}
	_ = ms.Save(ctx, &spi.ModelDescriptor{
		Ref: ref, State: spi.ModelUnlocked, ChangeLevel: spi.ChangeLevelStructural,
		Schema: []byte(`{}`), UpdateDate: time.Now().UTC(),
	})
	if err := ms.Lock(ctx, ref); err != nil {
		t.Fatalf("Lock: %v", err)
	}
	err = ms.ExtendSchema(ctx, ref, spi.SchemaDelta(`[{"kind":"broaden_type","path":"x","payload":["NULL"]}]`))
	if err == nil {
		t.Fatal("expected error when ApplyFunc not wired")
	}
	if errors.Is(err, spi.ErrModelNotLocked) {
		t.Fatalf("model is locked, error must be the wiring error, got %v", err)
	}
}

func TestSQLite_ExtendSchema_ModelNotFound(t *testing.T) {
	f, _ := setupSQLiteExt(t)
	ctx := extTestCtx("t1")
	ms, _ := f.ModelStore(ctx)
	ref := spi.ModelRef{EntityName: "Missing", ModelVersion: "1"}
	err := ms.ExtendSchema(ctx, ref, spi.SchemaDelta(`[{"kind":"broaden_type","path":"x","payload":["NULL"]}]`))
	if !errors.Is(err, spi.ErrNotFound) {
		t.Fatalf("err = %v, want errors.Is(_, spi.ErrNotFound)", err)
	}
}

// TestSQLite_ExtendSchema_MultiDeltaFold asserts three sequential ExtendSchema
// calls each write a delta row and the folded Get surfaces all three payloads.
func TestSQLite_ExtendSchema_MultiDeltaFold(t *testing.T) {
	f, dbPath := setupSQLiteExt(t)
	ctx := extTestCtx("t1")
	ms, err := f.ModelStore(ctx)
	if err != nil {
		t.Fatalf("ModelStore: %v", err)
	}
	ref := spi.ModelRef{EntityName: "Book", ModelVersion: "1"}

	if err := ms.Save(ctx, &spi.ModelDescriptor{
		Ref: ref, State: spi.ModelUnlocked, ChangeLevel: spi.ChangeLevelStructural,
		Schema: []byte(`{"base":1}`), UpdateDate: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if err := ms.Lock(ctx, ref); err != nil {
		t.Fatalf("Lock: %v", err)
	}

	deltas := []spi.SchemaDelta{
		spi.SchemaDelta(`[{"kind":"broaden_type","path":"x","payload":["NULL"]}]`),
		spi.SchemaDelta(`[{"kind":"broaden_type","path":"y","payload":["STRING"]}]`),
		spi.SchemaDelta(`[{"kind":"broaden_type","path":"z","payload":["BOOLEAN"]}]`),
	}
	for i, d := range deltas {
		if err := ms.ExtendSchema(ctx, ref, d); err != nil {
			t.Fatalf("delta %d: %v", i, err)
		}
	}

	// Three log rows must exist.
	db := openRawDB(t, dbPath)
	if got := countExtensionRows(t, db, "t1", ref, "delta"); got != 3 {
		t.Errorf("expected 3 delta rows, got %d", got)
	}

	desc, err := ms.Get(ctx, ref)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	for _, marker := range []string{"NULL", "STRING", "BOOLEAN"} {
		if !bytes.Contains(desc.Schema, []byte(marker)) {
			t.Errorf("expected %q in folded schema, got %s", marker, desc.Schema)
		}
	}
}

// TestSQLite_ExtendSchema_CrossTenantIsolation asserts extending one tenant's
// model never affects another tenant's same-ref model; the invariant is
// preserved under log-based semantics (delta rows are tenant-scoped).
func TestSQLite_ExtendSchema_CrossTenantIsolation(t *testing.T) {
	f, dbPath := setupSQLiteExt(t)
	ctxA := extTestCtx("tenantA")
	ctxB := extTestCtx("tenantB")

	msA, err := f.ModelStore(ctxA)
	if err != nil {
		t.Fatalf("ModelStore A: %v", err)
	}
	msB, err := f.ModelStore(ctxB)
	if err != nil {
		t.Fatalf("ModelStore B: %v", err)
	}

	ref := spi.ModelRef{EntityName: "Shared", ModelVersion: "1"}
	if err := msA.Save(ctxA, &spi.ModelDescriptor{
		Ref: ref, State: spi.ModelUnlocked, ChangeLevel: spi.ChangeLevelStructural,
		Schema: []byte(`{"t":"A"}`), UpdateDate: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("Save A: %v", err)
	}
	if err := msA.Lock(ctxA, ref); err != nil {
		t.Fatalf("Lock A: %v", err)
	}
	if err := msB.Save(ctxB, &spi.ModelDescriptor{
		Ref: ref, State: spi.ModelUnlocked, ChangeLevel: spi.ChangeLevelStructural,
		Schema: []byte(`{"t":"B"}`), UpdateDate: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("Save B: %v", err)
	}
	if err := msB.Lock(ctxB, ref); err != nil {
		t.Fatalf("Lock B: %v", err)
	}

	if err := msA.ExtendSchema(ctxA, ref, spi.SchemaDelta(`[{"kind":"broaden_type","path":"x","payload":["A_DELTA"]}]`)); err != nil {
		t.Fatalf("ExtendSchema A: %v", err)
	}

	// Log-row-level isolation: tenantA has exactly one delta row;
	// tenantB has zero.
	db := openRawDB(t, dbPath)
	if got := countExtensionRows(t, db, "tenantA", ref, "delta"); got != 1 {
		t.Errorf("tenantA: expected 1 delta row, got %d", got)
	}
	if got := countExtensionRows(t, db, "tenantB", ref, "delta"); got != 0 {
		t.Errorf("tenantB: expected 0 delta rows (isolation breach), got %d", got)
	}

	descA, err := msA.Get(ctxA, ref)
	if err != nil {
		t.Fatalf("Get A: %v", err)
	}
	descB, err := msB.Get(ctxB, ref)
	if err != nil {
		t.Fatalf("Get B: %v", err)
	}

	if !bytes.Contains(descA.Schema, []byte("A_DELTA")) {
		t.Errorf("tenant A: expected A_DELTA, got %s", descA.Schema)
	}
	if bytes.Contains(descB.Schema, []byte("A_DELTA")) {
		t.Errorf("tenant isolation broken: tenant B sees A's delta: %s", descB.Schema)
	}
	if !bytes.Contains(descA.Schema, []byte(`"t":"A"`)) {
		t.Errorf("tenant A lost base: %s", descA.Schema)
	}
	if !bytes.Contains(descB.Schema, []byte(`"t":"B"`)) {
		t.Errorf("tenant B lost base: %s", descB.Schema)
	}
}
