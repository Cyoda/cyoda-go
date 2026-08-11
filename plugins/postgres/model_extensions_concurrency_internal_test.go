package postgres

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// TestExtendSchema_OverlappingTx_CommittedDeltaSurvivesSavepointFold —
// the schema-fold delta-loss corruption path.
//
// Two overlapping REPEATABLE READ transactions extend the same
// auto-evolving model, and the slower one's delta crosses the savepoint
// interval. The savepoint fold then runs inside a snapshot taken before
// the other transaction committed, so without per-(tenant, model) write
// serialization it folds WITHOUT that committed delta — and because
// fold-on-read replays only deltas with seq greater than the savepoint's,
// the missed delta is excluded from every future fold: a committed
// additive field is silently and permanently lost.
//
// Contract under test: ExtendSchema serialises per (tenant, model). The
// overlapping writer must either fold correctly or fail with a conflict,
// in which case the caller retries in a fresh transaction (first
// committer wins, same as entity writes). Either way, once both writers
// have committed, a fold must contain BOTH fields.
func TestExtendSchema_OverlappingTx_CommittedDeltaSurvivesSavepointFold(t *testing.T) {
	const interval = 2
	fx := newPGFixtureWithInterval(t, interval)
	fx.store.applyFunc = setUnionApplyFunc
	fx.factory.InitTransactionManager(newTestUUIDGenerator())
	tm, err := fx.factory.TransactionManager(fx.ctx)
	if err != nil {
		t.Fatalf("TransactionManager: %v", err)
	}

	ref := spi.ModelRef{EntityName: "E", ModelVersion: "1"}
	fx.SaveModel(t, ref, []byte{})

	// T1 begins first and pins its REPEATABLE READ snapshot with a read,
	// so everything T2 commits from here on is invisible to T1.
	tx1ID, tx1Ctx, err := tm.Begin(fx.ctx)
	if err != nil {
		t.Fatalf("Begin T1: %v", err)
	}
	if _, err := fx.store.Get(tx1Ctx, ref); err != nil {
		t.Fatalf("T1 snapshot-pinning Get: %v", err)
	}

	// T2 adds "fieldB" and commits. One delta < interval: no savepoint.
	tx2ID, tx2Ctx, err := tm.Begin(fx.ctx)
	if err != nil {
		t.Fatalf("Begin T2: %v", err)
	}
	if err := fx.store.ExtendSchema(tx2Ctx, ref, spi.SchemaDelta(`"fieldB"`)); err != nil {
		t.Fatalf("T2 ExtendSchema: %v", err)
	}
	if err := tm.Commit(fx.ctx, tx2ID); err != nil {
		t.Fatalf("T2 Commit: %v", err)
	}

	// T1 adds "fieldA": its delta crosses the savepoint interval, so the
	// fold fires inside T1's stale snapshot — which cannot see "fieldB".
	err = fx.store.ExtendSchema(tx1Ctx, ref, spi.SchemaDelta(`"fieldA"`))
	if err == nil {
		err = tm.Commit(fx.ctx, tx1ID)
	} else {
		_ = tm.Rollback(fx.ctx, tx1ID)
	}
	if err != nil {
		// The serialised contract may reject the overlapping writer, but
		// only as spi.ErrConflict — that classification is what the kernel
		// turns into a retryable 409; an unclassified error would surface
		// as a 500 with no retry guidance. The caller's remedy is a retry
		// in a fresh transaction, which must then succeed.
		if !errors.Is(err, spi.ErrConflict) {
			t.Fatalf("overlapping writer rejected with a non-conflict error: %v", err)
		}
		retryID, retryCtx, beginErr := tm.Begin(fx.ctx)
		if beginErr != nil {
			t.Fatalf("Begin retry tx: %v", beginErr)
		}
		if err := fx.store.ExtendSchema(retryCtx, ref, spi.SchemaDelta(`"fieldA"`)); err != nil {
			_ = tm.Rollback(fx.ctx, retryID)
			t.Fatalf("retry ExtendSchema: %v", err)
		}
		if err := tm.Commit(fx.ctx, retryID); err != nil {
			t.Fatalf("retry Commit: %v", err)
		}
	}

	// Both writers have committed: the fold must contain both fields.
	got, err := fx.store.Get(fx.ctx, ref)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	for _, field := range []string{"fieldA", "fieldB"} {
		if !bytes.Contains(got.Schema, []byte(field)) {
			t.Errorf("committed delta %q lost from fold; folded schema = %s", field, got.Schema)
		}
	}
}

// TestExtendSchema_ConcurrentSelfWrap_SavepointCrossing_NoLostDelta —
// the self-wrap (no ambient transaction) flavour of the same corruption
// path, driven as a stress test: N concurrent writers each add a distinct
// token with a savepoint interval small enough that many folds fire
// mid-storm. Every committed delta must survive into the final fold; a
// savepoint that folded over a delta it could not yet see loses that
// token permanently.
func TestExtendSchema_ConcurrentSelfWrap_SavepointCrossing_NoLostDelta(t *testing.T) {
	const (
		n        = 24
		interval = 3
	)
	fx := newPGFixtureWithInterval(t, interval)
	fx.store.applyFunc = setUnionApplyFunc

	ref := spi.ModelRef{EntityName: "E", ModelVersion: "1"}
	fx.SaveModel(t, ref, []byte{})

	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		i := i
		go func() {
			defer wg.Done()
			delta := spi.SchemaDelta(fmt.Sprintf(`"d%02d"`, i))
			if err := fx.store.ExtendSchema(fx.ctx, ref, delta); err != nil {
				t.Errorf("ExtendSchema #%d: %v", i, err)
			}
		}()
	}
	wg.Wait()

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

// TestExtendSchema_MissingModel_UnwiredApplyFunc_ErrNotFound — the
// write-claim's zero-row arm. With applyFunc wired, the pre-persist
// check already reported ErrNotFound for a missing model; with it
// unwired, ExtendSchema used to append an orphan delta row for a model
// that does not exist. The claim UPDATE closes that hole: zero rows
// matched means no model to extend, ErrNotFound, and nothing persisted.
func TestExtendSchema_MissingModel_UnwiredApplyFunc_ErrNotFound(t *testing.T) {
	fx := newPGFixture(t)
	fx.store.applyFunc = nil
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

// TestExtendSchema_SelfWrap_SetsTenantGUCForRLS — the self-wrap
// transaction must set app.current_tenant like every other transaction
// this plugin opens (TransactionManager.Begin, the async-search scan),
// so the tenant-isolation RLS policies on models and
// model_schema_extensions apply to its statements.
//
// The usual fixtures cannot see this: the test role owns the tables (and
// the container's bootstrap user is superuser), so RLS never applies and
// a missing set_config is invisible. Production's stated RLS posture is
// a non-owner application role (rls_test.go), for which ENABLE ROW LEVEL
// SECURITY applies as-is — so this test runs ExtendSchema through a
// dedicated non-superuser role. An unset GUC evaluates the policies to
// NULL and the write-claim UPDATE sees zero rows: fail-closed
// ErrNotFound instead of an extension.
func TestExtendSchema_SelfWrap_SetsTenantGUCForRLS(t *testing.T) {
	fx := newPGFixture(t)
	fx.store.applyFunc = setUnionApplyFunc
	ref := spi.ModelRef{EntityName: "E", ModelVersion: "1"}
	fx.SaveModel(t, ref, []byte{}) // seeded as owner: RLS does not apply

	const probeRole = "cyoda_rls_probe"
	for _, stmt := range []string{
		`DROP ROLE IF EXISTS ` + probeRole,
		`CREATE ROLE ` + probeRole + ` LOGIN PASSWORD 'probe' NOSUPERUSER`,
		// The fixture recreates schema public (dropSchema), which strips the
		// default PUBLIC grants — USAGE must be granted explicitly.
		`GRANT USAGE ON SCHEMA public TO ` + probeRole,
		`GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO ` + probeRole,
		`GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA public TO ` + probeRole,
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

	probeURL, err := url.Parse(os.Getenv("CYODA_TEST_DB_URL"))
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
	ms, err := factory.ModelStore(fx.ctx)
	if err != nil {
		t.Fatalf("probe ModelStore: %v", err)
	}

	if err := ms.ExtendSchema(fx.ctx, ref, spi.SchemaDelta(`"d0"`)); err != nil {
		t.Fatalf("self-wrap ExtendSchema under RLS (non-owner role): %v", err)
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
