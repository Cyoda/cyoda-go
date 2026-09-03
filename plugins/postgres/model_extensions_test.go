package postgres_test

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/plugins/postgres"
)

// countExtensionRows returns the number of rows in model_schema_extensions
// for the given tenant + ref — used by Save/Unlock assertion tests.
func countExtensionRows(t *testing.T, pool *pgxpool.Pool, tenantID string, ref spi.ModelRef) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM model_schema_extensions
		 WHERE tenant_id = $1 AND model_name = $2 AND model_version = $3`,
		tenantID, ref.EntityName, ref.ModelVersion).Scan(&n); err != nil {
		t.Fatalf("count extensions: %v", err)
	}
	return n
}

// jsonRecordingApplyFunc returns a test ApplyFunc whose output is
// always valid JSON (so it can be persisted into the payload JSONB
// column as a savepoint) and whose output reflects both the base and
// the delta, so callers can assert Apply actually ran. It embeds the
// delta bytes under a dedicated "$$applied" array on the base object —
// deep-merging is outside the scope of these plugin tests.
func jsonRecordingApplyFunc() postgres.ApplyFunc {
	return func(base []byte, delta spi.SchemaDelta) ([]byte, error) {
		var m map[string]json.RawMessage
		if len(base) == 0 {
			m = map[string]json.RawMessage{}
		} else if err := json.Unmarshal(base, &m); err != nil {
			// Base isn't an object — wrap under "$$base".
			m = map[string]json.RawMessage{"$$base": json.RawMessage(base)}
		}
		var applied []json.RawMessage
		if raw, ok := m["$$applied"]; ok {
			if err := json.Unmarshal(raw, &applied); err != nil {
				return nil, err
			}
		}
		applied = append(applied, json.RawMessage(delta))
		encoded, err := json.Marshal(applied)
		if err != nil {
			return nil, err
		}
		m["$$applied"] = encoded
		return json.Marshal(m)
	}
}

func setupModelExtTest(t *testing.T) *postgres.StoreFactory {
	t.Helper()
	pool := newTestPool(t)
	if err := postgres.DropSchemaForTest(pool); err != nil {
		t.Fatalf("reset schema: %v", err)
	}
	if err := postgres.Migrate(pool); err != nil {
		t.Fatalf("migration failed: %v", err)
	}
	t.Cleanup(func() { _ = postgres.DropSchemaForTest(pool) })
	return postgres.NewStoreFactory(pool)
}

func seedBaseModel(t *testing.T, ms spi.ModelStore, ctx context.Context, ref spi.ModelRef, schemaJSON string, cl spi.ChangeLevel) {
	t.Helper()
	desc := &spi.ModelDescriptor{
		Ref:         ref,
		State:       spi.ModelUnlocked,
		ChangeLevel: cl,
		Schema:      []byte(schemaJSON),
		UpdateDate:  time.Now().UTC().Truncate(time.Millisecond),
	}
	if err := ms.Save(ctx, desc); err != nil {
		t.Fatalf("Save: %v", err)
	}
}

func TestExtendSchema_AppendAndFold(t *testing.T) {
	factory := setupModelExtTest(t)
	factory.SetApplyFunc(jsonRecordingApplyFunc())

	ctx := ctxWithTenant("t1")
	ms, err := factory.ModelStore(ctx)
	if err != nil {
		t.Fatalf("ModelStore: %v", err)
	}
	ref := spi.ModelRef{EntityName: "Book", ModelVersion: "1"}
	seedBaseModel(t, ms, ctx, ref, `{"name":"title"}`, spi.ChangeLevelStructural)

	delta := spi.SchemaDelta(`[{"kind":"add_property","path":"","name":"isbn","payload":{}}]`)
	if err := ms.ExtendSchema(ctx, ref, delta); err != nil {
		t.Fatalf("ExtendSchema: %v", err)
	}

	desc, err := ms.Get(ctx, ref)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !bytes.Contains(desc.Schema, []byte("isbn")) {
		t.Errorf("expected folded schema to contain delta bytes; got %s", desc.Schema)
	}
}

func TestExtendSchema_EmptyDeltaIsNoop(t *testing.T) {
	factory := setupModelExtTest(t)
	factory.SetApplyFunc(jsonRecordingApplyFunc())

	ctx := ctxWithTenant("t1")
	ms, err := factory.ModelStore(ctx)
	if err != nil {
		t.Fatalf("ModelStore: %v", err)
	}
	ref := spi.ModelRef{EntityName: "Book", ModelVersion: "1"}
	seedBaseModel(t, ms, ctx, ref, `{"a":1}`, spi.ChangeLevelStructural)

	if err := ms.ExtendSchema(ctx, ref, nil); err != nil {
		t.Fatalf("nil delta: %v", err)
	}
	if err := ms.ExtendSchema(ctx, ref, spi.SchemaDelta{}); err != nil {
		t.Fatalf("empty delta: %v", err)
	}

	pool := factory.Pool()
	var cnt int
	if err := pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM model_schema_extensions WHERE tenant_id=$1 AND model_name=$2 AND model_version=$3`,
		"t1", ref.EntityName, ref.ModelVersion).Scan(&cnt); err != nil {
		t.Fatalf("count: %v", err)
	}
	if cnt != 0 {
		t.Errorf("expected 0 extension rows, got %d", cnt)
	}
}

func TestExtendSchema_SavepointEvery64(t *testing.T) {
	factory := setupModelExtTest(t)
	factory.SetApplyFunc(jsonRecordingApplyFunc())

	ctx := ctxWithTenant("t1")
	ms, err := factory.ModelStore(ctx)
	if err != nil {
		t.Fatalf("ModelStore: %v", err)
	}
	ref := spi.ModelRef{EntityName: "Book", ModelVersion: "1"}
	seedBaseModel(t, ms, ctx, ref, `{"base":1}`, spi.ChangeLevelStructural)

	d := spi.SchemaDelta(json.RawMessage(`[{"kind":"broaden_type","path":"x","payload":["NULL"]}]`))
	for i := 0; i < 64; i++ {
		if err := ms.ExtendSchema(ctx, ref, d); err != nil {
			t.Fatalf("ExtendSchema #%d: %v", i, err)
		}
	}

	pool := factory.Pool()
	var savepointCount int
	if err := pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM model_schema_extensions
		 WHERE tenant_id=$1 AND model_name=$2 AND model_version=$3 AND kind='savepoint'`,
		"t1", ref.EntityName, ref.ModelVersion).Scan(&savepointCount); err != nil {
		t.Fatalf("savepoint count: %v", err)
	}
	if savepointCount != 1 {
		t.Errorf("expected 1 savepoint after 64 deltas, got %d", savepointCount)
	}

	// Get must still succeed and fold everything.
	if _, err := ms.Get(ctx, ref); err != nil {
		t.Fatalf("Get after 64 deltas: %v", err)
	}
}

func TestGet_NoExtensions_ReturnsBase(t *testing.T) {
	// Intentionally no ApplyFunc — with zero deltas, fold must not error.
	factory := setupModelExtTest(t)

	ctx := ctxWithTenant("t1")
	ms, err := factory.ModelStore(ctx)
	if err != nil {
		t.Fatalf("ModelStore: %v", err)
	}
	ref := spi.ModelRef{EntityName: "Book", ModelVersion: "1"}
	seedBaseModel(t, ms, ctx, ref, `{"t":1}`, spi.ChangeLevelStructural)

	desc, err := ms.Get(ctx, ref)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !bytes.Contains(desc.Schema, []byte(`"t":1`)) {
		t.Errorf("expected base schema returned verbatim; got %s", desc.Schema)
	}
}

func TestGet_WithExtensions_ButNoApplyFunc_Errors(t *testing.T) {
	factory := setupModelExtTest(t)
	factory.SetApplyFunc(jsonRecordingApplyFunc())

	ctx := ctxWithTenant("t1")
	ms, err := factory.ModelStore(ctx)
	if err != nil {
		t.Fatalf("ModelStore: %v", err)
	}
	ref := spi.ModelRef{EntityName: "Book", ModelVersion: "1"}
	seedBaseModel(t, ms, ctx, ref, `{"base":1}`, spi.ChangeLevelStructural)
	if err := ms.ExtendSchema(ctx, ref, spi.SchemaDelta(`[{"kind":"broaden_type","path":"x","payload":["NULL"]}]`)); err != nil {
		t.Fatalf("ExtendSchema: %v", err)
	}

	// Build a SECOND factory sharing the same pool but with no ApplyFunc.
	factory2 := postgres.NewStoreFactory(factory.Pool())
	ms2, err := factory2.ModelStore(ctx)
	if err != nil {
		t.Fatalf("ModelStore 2: %v", err)
	}
	if _, err := ms2.Get(ctx, ref); err == nil {
		t.Error("expected error when ApplyFunc is not wired but deltas exist")
	}
}

func TestStoreFactory_SetApplyFuncTwicePanics(t *testing.T) {
	factory := postgres.NewStoreFactory(nil)
	factory.SetApplyFunc(jsonRecordingApplyFunc())
	defer func() {
		if r := recover(); r == nil {
			t.Error("expected panic on second SetApplyFunc call")
		}
	}()
	factory.SetApplyFunc(jsonRecordingApplyFunc())
}

// --- D2: Save/Unlock clear extension log + dev-time assertion ---

func TestSave_AfterPriorExtensions_ClearsLog_ProductionMode(t *testing.T) {
	// Default debugMode == false → production path: Save logs a warn
	// but succeeds; the log is cleared unconditionally.
	factory := setupModelExtTest(t)
	factory.SetApplyFunc(jsonRecordingApplyFunc())

	ctx := ctxWithTenant("t1")
	ms, err := factory.ModelStore(ctx)
	if err != nil {
		t.Fatalf("ModelStore: %v", err)
	}
	ref := spi.ModelRef{EntityName: "Book", ModelVersion: "1"}
	seedBaseModel(t, ms, ctx, ref, `{"base":1}`, spi.ChangeLevelStructural)

	// Create an extension row.
	if err := ms.ExtendSchema(ctx, ref, spi.SchemaDelta(`[{"kind":"broaden_type","path":"x","payload":["NULL"]}]`)); err != nil {
		t.Fatalf("ExtendSchema: %v", err)
	}

	// Save again (simulating operator-misuse: Save while stale rows exist).
	if err := ms.Save(ctx, &spi.ModelDescriptor{
		Ref:         ref,
		State:       spi.ModelUnlocked,
		ChangeLevel: spi.ChangeLevelStructural,
		Schema:      []byte(`{"base":2}`),
		UpdateDate:  time.Now().UTC().Truncate(time.Millisecond),
	}); err != nil {
		t.Fatalf("Save: %v", err)
	}

	if n := countExtensionRows(t, factory.Pool(), "t1", ref); n != 0 {
		t.Errorf("expected 0 extension rows after Save, got %d", n)
	}
}

func TestSave_AfterPriorExtensions_DevMode_ReturnsError(t *testing.T) {
	postgres.SetDebugMode(true)
	t.Cleanup(func() { postgres.SetDebugMode(false) })

	factory := setupModelExtTest(t)
	factory.SetApplyFunc(jsonRecordingApplyFunc())

	ctx := ctxWithTenant("t1")
	ms, err := factory.ModelStore(ctx)
	if err != nil {
		t.Fatalf("ModelStore: %v", err)
	}
	ref := spi.ModelRef{EntityName: "Book", ModelVersion: "1"}
	seedBaseModel(t, ms, ctx, ref, `{"base":1}`, spi.ChangeLevelStructural)

	if err := ms.ExtendSchema(ctx, ref, spi.SchemaDelta(`[{"kind":"broaden_type","path":"x","payload":["NULL"]}]`)); err != nil {
		t.Fatalf("ExtendSchema: %v", err)
	}

	err = ms.Save(ctx, &spi.ModelDescriptor{
		Ref:         ref,
		State:       spi.ModelUnlocked,
		ChangeLevel: spi.ChangeLevelStructural,
		Schema:      []byte(`{"base":2}`),
		UpdateDate:  time.Now().UTC().Truncate(time.Millisecond),
	})
	if err == nil {
		t.Fatal("expected dev-mode assertion error on stale extension rows")
	}
	if !strings.Contains(err.Error(), "operator-contract violation") {
		t.Errorf("want operator-contract error, got: %v", err)
	}
}

func TestUnlock_WithLiveExtensions_DevMode_ReturnsError(t *testing.T) {
	postgres.SetDebugMode(true)
	t.Cleanup(func() { postgres.SetDebugMode(false) })

	factory := setupModelExtTest(t)
	factory.SetApplyFunc(jsonRecordingApplyFunc())

	ctx := ctxWithTenant("t1")
	ms, err := factory.ModelStore(ctx)
	if err != nil {
		t.Fatalf("ModelStore: %v", err)
	}
	ref := spi.ModelRef{EntityName: "Book", ModelVersion: "1"}
	seedBaseModel(t, ms, ctx, ref, `{"base":1}`, spi.ChangeLevelStructural)

	if err := ms.ExtendSchema(ctx, ref, spi.SchemaDelta(`[{"kind":"broaden_type","path":"x","payload":["NULL"]}]`)); err != nil {
		t.Fatalf("ExtendSchema: %v", err)
	}

	err = ms.Unlock(ctx, ref)
	if err == nil {
		t.Fatal("expected dev-mode assertion error on live extension rows")
	}
	if !strings.Contains(err.Error(), "operator-contract violation") {
		t.Errorf("want operator-contract error, got: %v", err)
	}
}

func TestUnlock_CleanState_Succeeds(t *testing.T) {
	postgres.SetDebugMode(true)
	t.Cleanup(func() { postgres.SetDebugMode(false) })

	factory := setupModelExtTest(t)
	factory.SetApplyFunc(jsonRecordingApplyFunc())

	ctx := ctxWithTenant("t1")
	ms, err := factory.ModelStore(ctx)
	if err != nil {
		t.Fatalf("ModelStore: %v", err)
	}
	ref := spi.ModelRef{EntityName: "Book", ModelVersion: "1"}
	seedBaseModel(t, ms, ctx, ref, `{"base":1}`, spi.ChangeLevelStructural)
	// No ExtendSchema — log is empty.

	if err := ms.Unlock(ctx, ref); err != nil {
		t.Fatalf("Unlock: %v", err)
	}
}

// --- D5: conformance extras — rollback invisibility, multi-delta fold, tenant isolation ---

// setupModelExtTestWithTM is like setupModelExtTest but also wires a
// TransactionManager onto the factory so tests can Begin/Commit/Rollback
// and exercise tx-bound visibility of ExtendSchema.
func setupModelExtTestWithTM(t *testing.T) *postgres.StoreFactory {
	t.Helper()
	pool := newTestPool(t)
	if err := postgres.DropSchemaForTest(pool); err != nil {
		t.Fatalf("reset schema: %v", err)
	}
	if err := postgres.Migrate(pool); err != nil {
		t.Fatalf("migration failed: %v", err)
	}
	t.Cleanup(func() { _ = postgres.DropSchemaForTest(pool) })
	factory := postgres.NewStoreFactory(pool)
	factory.InitTransactionManager(newTestUUIDGenerator())
	return factory
}

// TestExtendSchema_RolledBack_NotVisible asserts that ExtendSchema called
// inside a tx that is subsequently rolled back leaves no observable effect
// on disk or on Get — visibility must be commit-bound.
func TestExtendSchema_RolledBack_NotVisible(t *testing.T) {
	factory := setupModelExtTestWithTM(t)
	factory.SetApplyFunc(jsonRecordingApplyFunc())

	ctx := ctxWithTenant("t1")
	ms, err := factory.ModelStore(ctx)
	if err != nil {
		t.Fatalf("ModelStore: %v", err)
	}
	ref := spi.ModelRef{EntityName: "Book", ModelVersion: "1"}
	seedBaseModel(t, ms, ctx, ref, `{"base":1}`, spi.ChangeLevelStructural)

	tm, err := factory.TransactionManager(ctx)
	if err != nil {
		t.Fatalf("TransactionManager: %v", err)
	}
	txID, txCtx := beginGuarded(t, tm, ctx)
	// ExtendSchema must be invoked via a store built for the tx-carrying
	// context so that subsequent plugin-internal resolutions still see the
	// tx (the ctxQuerier will re-resolve on each call anyway, but we reuse
	// the same ms because resolveRaw inspects the call-time context).
	if err := ms.ExtendSchema(txCtx, ref, spi.SchemaDelta(`[{"kind":"broaden_type","path":"x","payload":["NULL"]}]`)); err != nil {
		t.Fatalf("ExtendSchema: %v", err)
	}
	if err := tm.Rollback(ctx, txID); err != nil {
		t.Fatalf("Rollback: %v", err)
	}

	// Post-rollback: extension row must not exist.
	if n := countExtensionRows(t, factory.Pool(), "t1", ref); n != 0 {
		t.Errorf("after rollback expected 0 extension rows, got %d", n)
	}

	// And Get returns the base schema untouched — the applied-delta marker
	// inserted by jsonRecordingApplyFunc must not appear.
	desc, err := ms.Get(ctx, ref)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if bytes.Contains(desc.Schema, []byte(`"$$applied"`)) {
		t.Errorf("rolled-back delta leaked into Get result: %s", desc.Schema)
	}
}

// TestExtendSchema_Commit_VisibleAfterCommit asserts the happy path: the
// delta written inside the tx becomes visible once the tx commits.
func TestExtendSchema_Commit_VisibleAfterCommit(t *testing.T) {
	factory := setupModelExtTestWithTM(t)
	factory.SetApplyFunc(jsonRecordingApplyFunc())

	ctx := ctxWithTenant("t1")
	ms, err := factory.ModelStore(ctx)
	if err != nil {
		t.Fatalf("ModelStore: %v", err)
	}
	ref := spi.ModelRef{EntityName: "Book", ModelVersion: "1"}
	seedBaseModel(t, ms, ctx, ref, `{"base":1}`, spi.ChangeLevelStructural)

	tm, err := factory.TransactionManager(ctx)
	if err != nil {
		t.Fatalf("TransactionManager: %v", err)
	}
	txID, txCtx := beginGuarded(t, tm, ctx)
	if err := ms.ExtendSchema(txCtx, ref, spi.SchemaDelta(`[{"kind":"broaden_type","path":"x","payload":["NULL"]}]`)); err != nil {
		t.Fatalf("ExtendSchema: %v", err)
	}
	if err := tm.Commit(ctx, txID); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	if n := countExtensionRows(t, factory.Pool(), "t1", ref); n != 1 {
		t.Errorf("after commit expected 1 extension row, got %d", n)
	}
}

// TestExtendSchema_MultiDeltaFold asserts three sequential ExtendSchema
// calls all survive fold: the folded schema contains markers from every
// delta.
func TestExtendSchema_MultiDeltaFold(t *testing.T) {
	factory := setupModelExtTest(t)
	factory.SetApplyFunc(jsonRecordingApplyFunc())

	ctx := ctxWithTenant("t1")
	ms, err := factory.ModelStore(ctx)
	if err != nil {
		t.Fatalf("ModelStore: %v", err)
	}
	ref := spi.ModelRef{EntityName: "Book", ModelVersion: "1"}
	seedBaseModel(t, ms, ctx, ref, `{"base":1}`, spi.ChangeLevelStructural)

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

// TestExtendSchema_CrossTenantIsolation asserts that ExtendSchema on one
// tenant's model never leaks into another tenant's same-ref model.
func TestExtendSchema_CrossTenantIsolation(t *testing.T) {
	factory := setupModelExtTest(t)
	factory.SetApplyFunc(jsonRecordingApplyFunc())

	ctxA := ctxWithTenant("tenantA")
	ctxB := ctxWithTenant("tenantB")

	msA, err := factory.ModelStore(ctxA)
	if err != nil {
		t.Fatalf("ModelStore A: %v", err)
	}
	msB, err := factory.ModelStore(ctxB)
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
	if err := msB.Save(ctxB, &spi.ModelDescriptor{
		Ref: ref, State: spi.ModelUnlocked, ChangeLevel: spi.ChangeLevelStructural,
		Schema: []byte(`{"t":"B"}`), UpdateDate: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("Save B: %v", err)
	}

	if err := msA.ExtendSchema(ctxA, ref, spi.SchemaDelta(`[{"kind":"broaden_type","path":"x","payload":["A_DELTA"]}]`)); err != nil {
		t.Fatalf("ExtendSchema A: %v", err)
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
	// Per-tenant extension counts.
	if n := countExtensionRows(t, factory.Pool(), "tenantA", ref); n != 1 {
		t.Errorf("tenantA: expected 1 extension row, got %d", n)
	}
	if n := countExtensionRows(t, factory.Pool(), "tenantB", ref); n != 0 {
		t.Errorf("tenantB: expected 0 extension rows, got %d", n)
	}
}
