package postgres_test

import (
	"context"
	"errors"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/plugins/postgres"
)

// A non-transactional CompareAndSave must read the current transaction ID and
// write the new version as one indivisible step. Outside a transaction every
// statement the store issues is separately auto-committed, so a check taken
// on its own leaves a check-then-write window: several callers naming the same
// expected transaction ID can all read it, all pass the check, and all write —
// each silently clobbering the last instead of getting ErrConflict.
func TestNonTxCompareAndSave_ExactlyOneWinner(t *testing.T) {
	factory := setupEntityTest(t)
	ctx := ctxWithTenant("tenant-cas")
	ref := spi.ModelRef{EntityName: "m-cas", ModelVersion: "1"}

	store, err := factory.EntityStore(ctx)
	if err != nil {
		t.Fatalf("EntityStore: %v", err)
	}

	const seedTxID = "tx-seed"
	if _, err := store.Save(ctx, &spi.Entity{
		Meta: spi.EntityMeta{
			ID: "e-cas", TenantID: "tenant-cas", ModelRef: ref,
			State: "open", TransactionID: seedTxID,
		},
		Data: []byte(`{"n":0}`),
	}); err != nil {
		t.Fatalf("seed Save: %v", err)
	}

	const racers = 8
	var wg sync.WaitGroup
	results := make([]error, racers)
	start := make(chan struct{})
	for i := range racers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, results[i] = store.CompareAndSave(ctx, &spi.Entity{
				Meta: spi.EntityMeta{
					ID: "e-cas", TenantID: "tenant-cas", ModelRef: ref,
					State: "open", TransactionID: "tx-writer",
				},
				Data: []byte(`{"n":1}`),
			}, seedTxID)
		}()
	}
	close(start)
	wg.Wait()

	var wins, conflicts int
	for i, err := range results {
		switch {
		case err == nil:
			wins++
		case errors.Is(err, spi.ErrConflict):
			conflicts++
		default:
			t.Fatalf("racer %d: unexpected error: %v", i, err)
		}
	}
	if wins != 1 || conflicts != racers-1 {
		t.Fatalf("got %d winners and %d conflicts, want exactly 1 winner and %d conflicts", wins, conflicts, racers-1)
	}
}

// The transaction CompareAndSave opens for itself must set app.current_tenant,
// as every other transaction this plugin opens does (TransactionManager.Begin,
// ExtendSchema's self-wrap, the async-search scan). The owner role bypasses the
// RLS policies, so the ordinary fixtures cannot see a missing GUC at all — this
// test runs the compare-and-save through a dedicated non-superuser role, which
// is production's stated posture (rls_test.go). With the GUC unset the policies
// evaluate to NULL: the locking check reads no row and the write is rejected.
func TestNonTxCompareAndSave_SetsTenantGUCForRLS(t *testing.T) {
	factory := setupEntityTest(t)
	const tenant spi.TenantID = "tenant-cas-rls"
	ctx := ctxWithTenant(tenant)
	ref := spi.ModelRef{EntityName: "m-cas-rls", ModelVersion: "1"}

	owner := postgres.PoolForTest(factory)
	ownerStore, err := factory.EntityStore(ctx)
	if err != nil {
		t.Fatalf("owner EntityStore: %v", err)
	}
	const seedTxID = "tx-seed"
	if _, err := ownerStore.Save(ctx, &spi.Entity{
		Meta: spi.EntityMeta{
			ID: "e-cas-rls", TenantID: tenant, ModelRef: ref,
			State: "open", TransactionID: seedTxID,
		},
		Data: []byte(`{"n":0}`),
	}); err != nil {
		t.Fatalf("seed Save (as owner: RLS does not apply): %v", err)
	}

	const probeRole = "cyoda_cas_rls_probe"
	for _, stmt := range []string{
		`DROP ROLE IF EXISTS ` + probeRole,
		`CREATE ROLE ` + probeRole + ` LOGIN PASSWORD 'probe' NOSUPERUSER`,
		// The fixture recreates schema public (dropSchema), which strips the
		// default PUBLIC grants — USAGE must be granted explicitly.
		`GRANT USAGE ON SCHEMA public TO ` + probeRole,
		`GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO ` + probeRole,
		`GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA public TO ` + probeRole,
	} {
		if _, err := owner.Exec(ctx, stmt); err != nil {
			t.Fatalf("provision probe role: %v", err)
		}
	}
	t.Cleanup(func() {
		// Runs before the fixture's dropSchema cleanup (LIFO), while the
		// granted objects still exist.
		_, _ = owner.Exec(context.Background(), `DROP OWNED BY `+probeRole)
		_, _ = owner.Exec(context.Background(), `DROP ROLE IF EXISTS `+probeRole)
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

	probeStore, err := postgres.NewStoreFactory(probePool).EntityStore(ctx)
	if err != nil {
		t.Fatalf("probe EntityStore: %v", err)
	}
	if _, err := probeStore.CompareAndSave(ctx, &spi.Entity{
		Meta: spi.EntityMeta{
			ID: "e-cas-rls", TenantID: tenant, ModelRef: ref,
			State: "open", TransactionID: "tx-writer",
		},
		Data: []byte(`{"n":1}`),
	}, seedTxID); err != nil {
		t.Fatalf("compare-and-save under RLS (non-owner role): %v", err)
	}

	// Assert through the owner pool: the new version really landed.
	var got string
	if err := owner.QueryRow(ctx,
		`SELECT doc->'_meta'->>'transaction_id' FROM entities
		 WHERE tenant_id = $1 AND entity_id = $2`,
		string(tenant), "e-cas-rls").Scan(&got); err != nil {
		t.Fatalf("read back entity: %v", err)
	}
	if got != "tx-writer" {
		t.Errorf("stored transaction ID = %q, want %q", got, "tx-writer")
	}
}

// A compare-and-save that queued on the row lock must stamp its write at a time
// at or after the write it waited for. CURRENT_TIMESTAMP is the transaction's
// START time, fixed before the FOR UPDATE wait: a caller that waited half a
// second on the lock would otherwise date its own version half a second before
// the version it just read and superseded, and a point-in-time read would order
// the two backwards.
func TestNonTxCompareAndSave_StampsAfterTheLockWait(t *testing.T) {
	factory := setupEntityTest(t)
	const tenant spi.TenantID = "tenant-cas-stamp"
	ctx := ctxWithTenant(tenant)
	ref := spi.ModelRef{EntityName: "m-cas-stamp", ModelVersion: "1"}
	pool := postgres.PoolForTest(factory)

	store, err := factory.EntityStore(ctx)
	if err != nil {
		t.Fatalf("EntityStore: %v", err)
	}
	const seedTxID = "tx-seed"
	if _, err := store.Save(ctx, &spi.Entity{
		Meta: spi.EntityMeta{
			ID: "e-cas-stamp", TenantID: tenant, ModelRef: ref,
			State: "open", TransactionID: seedTxID,
		},
		Data: []byte(`{"n":0}`),
	}); err != nil {
		t.Fatalf("seed Save: %v", err)
	}

	// Hold the row lock the compare-and-save must queue behind.
	holder, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin lock holder: %v", err)
	}
	defer func() { _ = holder.Rollback(context.Background()) }()
	if _, err := holder.Exec(ctx,
		`SELECT 1 FROM entities WHERE tenant_id = $1 AND entity_id = $2 FOR UPDATE`,
		string(tenant), "e-cas-stamp"); err != nil {
		t.Fatalf("take the row lock: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		_, err := store.CompareAndSave(ctx, &spi.Entity{
			Meta: spi.EntityMeta{
				ID: "e-cas-stamp", TenantID: tenant, ModelRef: ref,
				State: "open", TransactionID: "tx-writer",
			},
			Data: []byte(`{"n":1}`),
		}, seedTxID)
		done <- err
	}()

	// Wait until the compare-and-save is demonstrably blocked on the lock
	// rather than sleeping a guessed interval: releasing the lock before the
	// writer ever queued would pass the assertion without exercising anything.
	waitStart := time.Now()
	for {
		var blocked int
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM pg_stat_activity
			 WHERE datname = current_database() AND wait_event_type = 'Lock'`).Scan(&blocked); err != nil {
			t.Fatalf("poll for a blocked backend: %v", err)
		}
		if blocked > 0 {
			break
		}
		if time.Since(waitStart) > 10*time.Second {
			t.Fatal("no backend ever blocked on the row lock")
		}
		time.Sleep(20 * time.Millisecond)
	}

	// Hold it a little longer once the writer is demonstrably queued, so the
	// gap the assertion measures is wider than the clock's own resolution.
	time.Sleep(100 * time.Millisecond)

	// The database's own clock, read while the lock is still held: everything
	// the waiting compare-and-save does happens after this instant.
	var heldUntil time.Time
	if err := holder.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&heldUntil); err != nil {
		t.Fatalf("read the holder's clock: %v", err)
	}
	if err := holder.Rollback(ctx); err != nil {
		t.Fatalf("release the row lock: %v", err)
	}

	if err := <-done; err != nil {
		t.Fatalf("compare-and-save after the lock wait: %v", err)
	}

	var validTime time.Time
	if err := pool.QueryRow(ctx,
		`SELECT valid_time FROM entity_versions
		 WHERE tenant_id = $1 AND entity_id = $2 ORDER BY version DESC LIMIT 1`,
		string(tenant), "e-cas-stamp").Scan(&validTime); err != nil {
		t.Fatalf("read back the new version's valid_time: %v", err)
	}
	if validTime.Before(heldUntil) {
		t.Fatalf("the write that waited on the lock is stamped %v before the lock was released (%v vs %v)",
			heldUntil.Sub(validTime), validTime, heldUntil)
	}
}

// TestNonTxCompareAndSave_OwnTxGuardPreventsSelfDeadlock guards against a
// regression that shipped and was caught only by a full-suite run hanging
// until go test's own 10-minute binary timeout: CompareAndSave's non-tx
// branch opens its own pgx.Tx and calls into save(ctx, ...), but save's
// non-tx branch used to decide whether to open a transaction using only
// spi.GetTransaction(ctx) and s.pool — both still read as "no ambient
// transaction, pool available" on CompareAndSave's txStore, since
// spi.GetTransaction sees only a transaction the CALLER opened through
// TransactionManager, never a raw pgx.Tx this plugin holds for itself. So
// save opened a SECOND transaction on the same pool while the first still
// held the row lock compareTxID's FOR UPDATE took, and the nested
// transaction's own upsert queued behind that lock forever: a self-deadlock
// only the blocked caller could have broken, and never would.
//
// The fix is structural rather than a call-site convention: entityStore
// carries an ownTx flag, set true on CompareAndSave's txStore copy, and
// save's (and Delete's) own transaction-opening branch checks !s.ownTx
// before opening one — see ownTx's doc comment on the entityStore struct.
// CompareAndSave therefore calls txStore.save(...) directly, the exact same
// method a first, non-transactional Save reaches, and it falls straight
// through to saveOn instead of reopening a transaction. That makes this
// call return in milliseconds with no contention at all, so a short context
// deadline is not needed to make the GOOD case pass — it is here so a
// REGRESSED case (the ownTx guard removed, or a copy that forgets to set
// it) fails fast and loud instead of hanging for the better part of ten
// minutes: the nested transaction's blocked query has its context canceled
// a few seconds in, and this test reports a clear timeout/cancellation
// failure instead of silently eating a multi-minute hang.
//
// Also reads the write back and checks it landed at the version
// CompareAndSave returned, so this pins the write itself, not just the
// absence of a hang.
func TestNonTxCompareAndSave_OwnTxGuardPreventsSelfDeadlock(t *testing.T) {
	factory := setupEntityTest(t)
	const tenant spi.TenantID = "tenant-cas-no-nested-tx"
	ctx := ctxWithTenant(tenant)
	ref := spi.ModelRef{EntityName: "m-cas-no-nested-tx", ModelVersion: "1"}
	pool := postgres.PoolForTest(factory)

	store, err := factory.EntityStore(ctx)
	if err != nil {
		t.Fatalf("EntityStore: %v", err)
	}
	const seedTxID = "tx-seed"
	const id = "e-cas-no-nested-tx"
	if _, err := store.Save(ctx, &spi.Entity{
		Meta: spi.EntityMeta{
			ID: id, TenantID: tenant, ModelRef: ref,
			State: "open", TransactionID: seedTxID,
		},
		Data: []byte(`{"n":0}`),
	}); err != nil {
		t.Fatalf("seed Save: %v", err)
	}

	// A few seconds, not the default test binary timeout: bounds a regressed
	// self-deadlock to a fast, specific failure instead of a multi-minute hang.
	deadlineCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	version, err := store.CompareAndSave(deadlineCtx, &spi.Entity{
		Meta: spi.EntityMeta{
			ID: id, TenantID: tenant, ModelRef: ref,
			State: "open", TransactionID: "tx-writer",
		},
		Data: []byte(`{"n":1}`),
	}, seedTxID)
	if err != nil {
		t.Fatalf("compare-and-save did not complete within its deadline — "+
			"likely reopened a transaction on itself and self-deadlocked: %v", err)
	}
	if version != 2 {
		t.Errorf("CompareAndSave returned version %d, want 2", version)
	}

	var storedTxID string
	var storedVersion int64
	if err := pool.QueryRow(ctx,
		`SELECT doc->'_meta'->>'transaction_id', version FROM entities WHERE tenant_id = $1 AND entity_id = $2`,
		string(tenant), id).Scan(&storedTxID, &storedVersion); err != nil {
		t.Fatalf("read back entity: %v", err)
	}
	if storedTxID != "tx-writer" {
		t.Errorf("stored transaction ID = %q, want %q", storedTxID, "tx-writer")
	}
	if storedVersion != version {
		t.Errorf("stored version = %d, want %d (CompareAndSave's return value)", storedVersion, version)
	}
}
