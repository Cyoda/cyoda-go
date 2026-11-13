package postgres

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// TestExtendSchema_InEntityTx_NeverConflicts — two entity transactions that
// stay open each extend the same model at the same time. An extension
// never joins the caller's transaction, so neither waits for the other's
// transaction to end and neither fails: both return nil and both deltas
// are in the model.
func TestExtendSchema_InEntityTx_NeverConflicts(t *testing.T) {
	fx := newPGFixtureSized(t, 64, 10)
	fx.store.applyFunc = setUnionApplyFunc
	tm := fx.txManager(t)
	ref := spi.ModelRef{EntityName: "E", ModelVersion: "1"}
	fx.SaveModel(t, ref, []byte(`[]`))

	var txCtxs [2]context.Context
	for i := range txCtxs {
		_, txCtxs[i] = beginGuarded(t, tm, fx.ctx)
		fx.pinSnapshot(t, txCtxs[i])
	}

	var errs [2]error
	var wg sync.WaitGroup
	for i := range txCtxs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(txCtxs[i], 5*time.Second)
			defer cancel()
			errs[i] = fx.store.ExtendSchema(ctx, ref, spi.SchemaDelta(fmt.Sprintf(`"f%d"`, i)))
		}()
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Errorf("ExtendSchema from open entity transaction %d: %v", i, err)
		}
	}

	got, err := fx.store.Get(fx.ctx, ref)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	for _, f := range []string{"f0", "f1"} {
		if !bytes.Contains(got.Schema, []byte(f)) {
			t.Errorf("delta %q missing from the model; schema = %s", f, got.Schema)
		}
	}
}

// TestExtendSchema_SavepointUnderLock_KeepsEveryLowerSeqDelta — 24 entity
// transactions, each with a snapshot taken before any extension, extend
// one model concurrently with a savepoint interval of 2, so savepoints are
// folded mid-storm. A savepoint is folded under the model's row lock from
// the latest committed state, never from a caller's snapshot, so every
// savepoint holds every delta with a lower seq, and the final fold holds
// every field.
func TestExtendSchema_SavepointUnderLock_KeepsEveryLowerSeqDelta(t *testing.T) {
	const (
		n        = 24
		interval = 2
	)
	fx := newPGFixtureSized(t, interval, n+6)
	fx.store.applyFunc = setUnionApplyFunc
	tm := fx.txManager(t)
	ref := spi.ModelRef{EntityName: "E", ModelVersion: "1"}
	fx.SaveModel(t, ref, []byte(`[]`))

	txIDs := make([]string, n)
	txCtxs := make([]context.Context, n)
	for i := 0; i < n; i++ {
		txIDs[i], txCtxs[i] = beginGuarded(t, tm, fx.ctx)
		fx.pinSnapshot(t, txCtxs[i])
	}

	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(txCtxs[i], 30*time.Second)
			defer cancel()
			if err := fx.store.ExtendSchema(ctx, ref, spi.SchemaDelta(fmt.Sprintf(`"d%02d"`, i))); err != nil {
				errs[i] = fmt.Errorf("ExtendSchema: %w", err)
				return
			}
			if err := tm.Commit(ctx, txIDs[i]); err != nil {
				errs[i] = fmt.Errorf("Commit: %w", err)
			}
		}()
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Errorf("extender %d: %v", i, err)
		}
	}

	// Every savepoint holds every delta with a lower seq.
	rows, err := fx.db.Query(context.Background(), `
		SELECT seq, kind, payload FROM model_schema_extensions
		WHERE tenant_id = $1 AND model_name = $2 AND model_version = $3
		ORDER BY seq`,
		string(fx.tenantID), ref.EntityName, ref.ModelVersion)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	var below []string
	savepoints := 0
	for rows.Next() {
		var (
			seq     int64
			kind    string
			payload []byte
		)
		if err := rows.Scan(&seq, &kind, &payload); err != nil {
			t.Fatalf("scan log: %v", err)
		}
		if kind == "delta" {
			var tok string
			if err := json.Unmarshal(payload, &tok); err != nil {
				t.Fatalf("decode delta %s: %v", payload, err)
			}
			below = append(below, tok)
			continue
		}
		savepoints++
		for _, tok := range below {
			if !bytes.Contains(payload, []byte(`"`+tok+`"`)) {
				t.Errorf("savepoint at seq %d misses lower-seq delta %q; payload = %s", seq, tok, payload)
			}
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read log: %v", err)
	}
	if savepoints == 0 {
		t.Error("no savepoint was folded; the test did not exercise the savepoint path")
	}

	got, err := fx.store.Get(fx.ctx, ref)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	for i := 0; i < n; i++ {
		token := fmt.Sprintf("d%02d", i)
		if !bytes.Contains(got.Schema, []byte(token)) {
			t.Errorf("committed delta %q lost from fold; folded schema = %s", token, got.Schema)
		}
	}
}

// TestExtendSchema_MissingModel_ErrNotFound_NoRows — the lock statement
// finds no model row: ErrNotFound, and no orphan delta row is appended.
func TestExtendSchema_MissingModel_ErrNotFound_NoRows(t *testing.T) {
	fx := newPGFixture(t)
	fx.store.applyFunc = setUnionApplyFunc
	ref := spi.ModelRef{EntityName: "Ghost", ModelVersion: "1"}

	err := fx.store.ExtendSchema(fx.ctx, ref, spi.SchemaDelta(`"d0"`))
	if !errors.Is(err, spi.ErrNotFound) {
		t.Fatalf("ExtendSchema on missing model = %v, want spi.ErrNotFound", err)
	}

	var count int
	if err := fx.db.QueryRow(fx.ctx,
		`SELECT COUNT(*) FROM model_schema_extensions
		 WHERE tenant_id = $1 AND model_name = $2 AND model_version = $3`,
		string(fx.tenantID), ref.EntityName, ref.ModelVersion).Scan(&count); err != nil {
		t.Fatalf("count extension rows: %v", err)
	}
	if count != 0 {
		t.Errorf("missing model accreted %d orphan extension rows, want 0", count)
	}
}

// TestExtendSchema_SetsTenantGUCForRLS — the extension's transaction must
// set app.current_tenant like every other transaction this plugin opens
// (TransactionManager.Begin, the async-search scan), so the
// tenant-isolation RLS policies on models and model_schema_extensions apply
// to its statements.
//
// The usual fixtures cannot see this: the test role owns the tables (and
// the container's bootstrap user is superuser), so RLS never applies and
// a missing set_config is invisible. Row-level security is not supported
// (rls_test.go), but every transaction of the plugin sets the tenant; to
// check that this one does, this test runs ExtendSchema through a
// dedicated non-superuser role, for which ENABLE ROW LEVEL SECURITY
// applies. An unset GUC evaluates the policies to NULL and the lock
// statement sees zero rows: fail-closed ErrNotFound instead of an extension.
func TestExtendSchema_SetsTenantGUCForRLS(t *testing.T) {
	fx := newPGFixture(t)
	fx.store.applyFunc = setUnionApplyFunc
	ref := spi.ModelRef{EntityName: "E", ModelVersion: "1"}
	fx.SaveModel(t, ref, []byte(`[]`)) // seeded as owner: RLS does not apply

	const probeRole = "cyoda_rls_probe"
	for _, stmt := range []string{
		`DROP ROLE IF EXISTS ` + probeRole,
		`CREATE ROLE ` + probeRole + ` LOGIN PASSWORD 'probe' NOSUPERUSER`,
		// The fixture recreates schema public (dropSchema), which strips the
		// default PUBLIC grants — USAGE must be granted explicitly.
		`GRANT USAGE ON SCHEMA public TO ` + probeRole,
		`GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO ` + probeRole,
		`GRANT USAGE, SELECT, UPDATE ON ALL SEQUENCES IN SCHEMA public TO ` + probeRole, // UPDATE: cyoda_stamp raises cyoda_stamp_floor with setval
	} {
		if _, err := fx.db.Exec(fx.ctx, stmt); err != nil {
			t.Fatalf("provision probe role: %v", err)
		}
	}
	t.Cleanup(func() {
		// Runs before the fixture's dropSchema cleanup (LIFO), while the
		// granted objects still exist.
		_, _ = fx.db.Exec(context.Background(), `DROP OWNED BY `+probeRole)
		_, _ = fx.db.Exec(context.Background(), `DROP ROLE IF EXISTS `+probeRole)
	})

	probeURL, err := url.Parse(testDBURL(t))
	if err != nil {
		t.Fatalf("parse CYODA_TEST_DB_URL: %v", err)
	}
	probeURL.User = url.UserPassword(probeRole, "probe")
	probePool, err := pgxpool.New(context.Background(), probeURL.String())
	if err != nil {
		t.Fatalf("create probe pool: %v", err)
	}
	t.Cleanup(probePool.Close)

	factory := NewStoreFactory(probePool)
	factory.SetApplyFunc(setUnionApplyFunc)
	t.Cleanup(factory.closeModelPool)
	ms, err := factory.ModelStore(fx.ctx)
	if err != nil {
		t.Fatalf("probe ModelStore: %v", err)
	}

	if err := ms.ExtendSchema(fx.ctx, ref, spi.SchemaDelta(`"d0"`)); err != nil {
		t.Fatalf("ExtendSchema under RLS (non-owner role): %v", err)
	}

	// Assert through the owner pool: the delta really landed.
	var count int
	if err := fx.db.QueryRow(fx.ctx,
		`SELECT COUNT(*) FROM model_schema_extensions
		 WHERE tenant_id = $1 AND model_name = $2 AND model_version = $3 AND kind = 'delta'`,
		string(fx.tenantID), ref.EntityName, ref.ModelVersion).Scan(&count); err != nil {
		t.Fatalf("count delta rows: %v", err)
	}
	if count != 1 {
		t.Errorf("delta row count = %d, want 1", count)
	}
}
