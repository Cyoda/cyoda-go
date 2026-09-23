package postgres_test

import (
	"context"
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/plugins/postgres"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// commit_instant_test.go — a write is dated when its transaction COMMITTED,
// not when it started.
//
// CURRENT_TIMESTAMP is fixed at transaction start, so a transaction that
// begins before an instant and commits after it used to write rows dated
// before that instant: a point-in-time read served at that instant could
// change after it had been answered. Every test here pins one half of the
// repair.
//
// Instants always come from the DATABASE clock, never time.Now() — see
// pit_time_test.go for why the two are different clocks.

// commitInstantFixture gives a test a migrated factory, its transaction
// manager, the pool for direct column reads, and a tenant-scoped context.
func commitInstantFixture(t *testing.T, tenant spi.TenantID) (*postgres.StoreFactory, *postgres.TransactionManager, *pgxpool.Pool, context.Context) {
	t.Helper()
	factory, tm := setupEntityTestWithTM(t)
	return factory, tm, postgres.PoolForTest(factory), ctxWithTenant(tenant)
}

// clockNow reads the database's clock_timestamp() — the instant NOW, unlike
// dbNow's CURRENT_TIMESTAMP, which inside a transaction is that
// transaction's start. On the pool each statement is its own transaction, so
// the two agree there; clock_timestamp() is used here to say explicitly which
// instant the test means.
func clockNow(t *testing.T, ctx context.Context, pool *pgxpool.Pool) time.Time {
	t.Helper()
	var now time.Time
	if err := pool.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
		t.Fatalf("read DB clock: %v", err)
	}
	return now
}

// TestCommit_DatesWritesAtCommitNotAtStart proves a read at an instant does
// not change after it has been served. A transaction opens, writes, and only
// then commits; an instant captured between the write and the commit must not
// admit the write afterwards.
func TestCommit_DatesWritesAtCommitNotAtStart(t *testing.T) {
	factory, tm, pool, ctx := commitInstantFixture(t, "commit-instant-tenant")
	store, err := factory.EntityStore(ctx)
	if err != nil {
		t.Fatalf("EntityStore: %v", err)
	}
	mref := spi.ModelRef{EntityName: "commit-instant", ModelVersion: "1"}

	txID, txCtx, err := tm.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	id := uuid.NewString()
	if _, err := store.Save(txCtx, &spi.Entity{
		Meta: spi.EntityMeta{ID: id, ModelRef: mref}, Data: []byte(`{"n":1}`),
	}); err != nil {
		t.Fatalf("Save in tx: %v", err)
	}

	// An instant taken while the transaction is still open, from the database
	// clock (never a process clock — the two are different clocks).
	instant := clockNow(t, ctx, pool)

	if err := tm.Commit(txCtx, txID); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	got, err := store.Search(ctx, spi.Filter{}, spi.SearchOptions{
		ModelName: mref.EntityName, ModelVersion: mref.ModelVersion,
		Limit: 10, PointInTime: &instant,
	})
	if err != nil {
		t.Fatalf("Search at the instant: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("a write committed AFTER the instant must not appear in the snapshot at it: got %d", len(got))
	}

	// The positive half: the same search after the commit instant must find
	// it, so the assertion above cannot pass because the write is invisible
	// for some unrelated reason.
	after := clockNow(t, ctx, pool)
	later, err := store.Search(ctx, spi.Filter{}, spi.SearchOptions{
		ModelName: mref.EntityName, ModelVersion: mref.ModelVersion,
		Limit: 10, PointInTime: &after,
	})
	if err != nil {
		t.Fatalf("Search after the commit: %v", err)
	}
	if len(later) != 1 {
		t.Fatalf("the committed write must appear at an instant after the commit: got %d", len(later))
	}
}

// TestCommit_OneInstantForEveryEntity asserts a transaction's entities share
// one instant, so a point-in-time cut cannot tear it — and that the submit
// time the manager reports IS that instant.
func TestCommit_OneInstantForEveryEntity(t *testing.T) {
	const tenant spi.TenantID = "commit-shared-tenant"
	factory, tm, pool, ctx := commitInstantFixture(t, tenant)
	store, err := factory.EntityStore(ctx)
	if err != nil {
		t.Fatalf("EntityStore: %v", err)
	}
	mref := spi.ModelRef{EntityName: "commit-shared", ModelVersion: "1"}

	txID, txCtx, err := tm.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	ids := []string{uuid.NewString(), uuid.NewString(), uuid.NewString()}
	for _, id := range ids {
		if _, err := store.Save(txCtx, &spi.Entity{
			Meta: spi.EntityMeta{ID: id, ModelRef: mref}, Data: []byte(`{"n":1}`),
		}); err != nil {
			t.Fatalf("Save %s: %v", id, err)
		}
		// Separate the writes in wall-clock time, so "they share one instant"
		// is a property of the stamp rather than of three statements running
		// too fast to tell apart.
		time.Sleep(2 * time.Millisecond)
	}
	if err := tm.Commit(txCtx, txID); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	var distinct int
	if err := pool.QueryRow(ctx,
		`SELECT count(DISTINCT valid_time) FROM entity_versions
		  WHERE tenant_id = $1 AND entity_id = ANY($2)`,
		string(tenant), ids).Scan(&distinct); err != nil {
		t.Fatalf("count distinct: %v", err)
	}
	if distinct != 1 {
		t.Errorf("a transaction's writes must share one instant: got %d distinct valid_time values", distinct)
	}

	submit, err := tm.GetSubmitTime(ctx, txID)
	if err != nil {
		t.Fatalf("GetSubmitTime: %v", err)
	}
	var stamped, stampedTxTime time.Time
	if err := pool.QueryRow(ctx,
		`SELECT valid_time, transaction_time FROM entity_versions WHERE tenant_id = $1 AND entity_id = $2`,
		string(tenant), ids[0]).Scan(&stamped, &stampedTxTime); err != nil {
		t.Fatalf("read valid_time: %v", err)
	}
	if !submit.Equal(stamped) {
		t.Errorf("the recorded submit time must be the instant stamped on the rows: submit=%s stamped=%s", submit, stamped)
	}
	if !stampedTxTime.Equal(stamped) {
		t.Errorf("valid_time and transaction_time must be the same commit instant: valid=%s transaction=%s", stamped, stampedTxTime)
	}

	// The durable record of the same instant: a submit time that outlives the
	// process that produced it.
	var durable time.Time
	if err := pool.QueryRow(ctx,
		`SELECT submit_time FROM submit_times WHERE tenant_id = $1 AND tx_id = $2`,
		string(tenant), txID).Scan(&durable); err != nil {
		t.Fatalf("read submit_times: %v", err)
	}
	if !durable.Equal(stamped) {
		t.Errorf("the durable submit time must be the stamped instant: durable=%s stamped=%s", durable, stamped)
	}
}

// TestCommit_CreationDateIsOneInstantAcrossACreateAndUpdate pins the first
// direction of the creation_date CASE. A transaction that creates an entity
// and then updates it writes two version rows; the second one carried the
// provisional value it read inside the transaction, so stamping only version
// 1 would leave the two disagreeing about when the entity was created.
func TestCommit_CreationDateIsOneInstantAcrossACreateAndUpdate(t *testing.T) {
	const tenant spi.TenantID = "commit-creation-tenant"
	factory, tm, pool, ctx := commitInstantFixture(t, tenant)
	store, err := factory.EntityStore(ctx)
	if err != nil {
		t.Fatalf("EntityStore: %v", err)
	}
	mref := spi.ModelRef{EntityName: "commit-creation", ModelVersion: "1"}
	id := uuid.NewString()

	txID, txCtx, err := tm.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	for _, payload := range []string{`{"n":1}`, `{"n":2}`} {
		if _, err := store.Save(txCtx, &spi.Entity{
			Meta: spi.EntityMeta{ID: id, ModelRef: mref}, Data: []byte(payload),
		}); err != nil {
			t.Fatalf("Save %s: %v", payload, err)
		}
		time.Sleep(2 * time.Millisecond)
	}
	if err := tm.Commit(txCtx, txID); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	var distinct int
	if err := pool.QueryRow(ctx,
		`SELECT count(DISTINCT creation_date) FROM entity_versions
		  WHERE tenant_id = $1 AND entity_id = $2`,
		string(tenant), id).Scan(&distinct); err != nil {
		t.Fatalf("count distinct creation_date: %v", err)
	}
	if distinct != 1 {
		t.Errorf("every version of an entity created in this transaction must report one creation instant: got %d distinct values", distinct)
	}

	submit, err := tm.GetSubmitTime(ctx, txID)
	if err != nil {
		t.Fatalf("GetSubmitTime: %v", err)
	}
	var entityCreation, versionCreation time.Time
	if err := pool.QueryRow(ctx,
		`SELECT e.creation_date, v.creation_date
		   FROM entities e JOIN entity_versions v
		     ON v.tenant_id = e.tenant_id AND v.entity_id = e.entity_id AND v.version = 2
		  WHERE e.tenant_id = $1 AND e.entity_id = $2`,
		string(tenant), id).Scan(&entityCreation, &versionCreation); err != nil {
		t.Fatalf("read creation dates: %v", err)
	}
	if !entityCreation.Equal(submit) {
		t.Errorf("a newly created entity's creation_date must be its transaction's commit instant: got %s, want %s", entityCreation, submit)
	}
	if !versionCreation.Equal(submit) {
		t.Errorf("the second version's creation_date must be the same commit instant: got %s, want %s", versionCreation, submit)
	}
}

// TestCommit_CreationDateSurvivesALaterTransaction pins the OTHER direction
// of the same CASE. Replacing it with an unconditional SET creation_date
// would restamp every updated entity's creation as that update's instant —
// history would then report when a revision was written rather than when the
// entity was created, and postgres would diverge from the memory backend,
// which preserves the original.
func TestCommit_CreationDateSurvivesALaterTransaction(t *testing.T) {
	const tenant spi.TenantID = "commit-creation-later-tenant"
	factory, tm, pool, ctx := commitInstantFixture(t, tenant)
	store, err := factory.EntityStore(ctx)
	if err != nil {
		t.Fatalf("EntityStore: %v", err)
	}
	mref := spi.ModelRef{EntityName: "commit-creation-later", ModelVersion: "1"}
	id := uuid.NewString()

	createTx, createCtx, err := tm.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin (create): %v", err)
	}
	if _, err := store.Save(createCtx, &spi.Entity{
		Meta: spi.EntityMeta{ID: id, ModelRef: mref}, Data: []byte(`{"n":1}`),
	}); err != nil {
		t.Fatalf("Save (create): %v", err)
	}
	if err := tm.Commit(createCtx, createTx); err != nil {
		t.Fatalf("Commit (create): %v", err)
	}
	created, err := tm.GetSubmitTime(ctx, createTx)
	if err != nil {
		t.Fatalf("GetSubmitTime (create): %v", err)
	}

	time.Sleep(5 * time.Millisecond)

	updateTx, updateCtx, err := tm.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin (update): %v", err)
	}
	if _, err := store.Save(updateCtx, &spi.Entity{
		Meta: spi.EntityMeta{ID: id, ModelRef: mref}, Data: []byte(`{"n":2}`),
	}); err != nil {
		t.Fatalf("Save (update): %v", err)
	}
	if err := tm.Commit(updateCtx, updateTx); err != nil {
		t.Fatalf("Commit (update): %v", err)
	}
	updated, err := tm.GetSubmitTime(ctx, updateTx)
	if err != nil {
		t.Fatalf("GetSubmitTime (update): %v", err)
	}
	if !updated.After(created) {
		t.Fatalf("the two transactions must commit at distinct instants for this test to discriminate: created=%s updated=%s", created, updated)
	}

	var entityCreation, v2Creation, v2Valid time.Time
	if err := pool.QueryRow(ctx,
		`SELECT e.creation_date, v.creation_date, v.valid_time
		   FROM entities e JOIN entity_versions v
		     ON v.tenant_id = e.tenant_id AND v.entity_id = e.entity_id AND v.version = 2
		  WHERE e.tenant_id = $1 AND e.entity_id = $2`,
		string(tenant), id).Scan(&entityCreation, &v2Creation, &v2Valid); err != nil {
		t.Fatalf("read dates: %v", err)
	}
	if !entityCreation.Equal(created) {
		t.Errorf("an update must not restamp the entity's creation_date: got %s, want the creating transaction's instant %s", entityCreation, created)
	}
	if !v2Creation.Equal(created) {
		t.Errorf("the updated version must report the ORIGINAL creation instant: got %s, want %s", v2Creation, created)
	}
	if !v2Valid.Equal(updated) {
		t.Errorf("the updated version's valid_time must be the UPDATING transaction's commit instant: got %s, want %s", v2Valid, updated)
	}
}

// TestCommit_AuditEventsShareTheTransactionsInstant pins the audit trail onto
// the same instant as the version history, so the two cannot drift apart or
// invert. The event is recorded with a deliberately wrong timestamp — an hour
// in the past, standing in for an engine process clock that disagrees with
// the database — and the committed event must report the commit instant.
func TestCommit_AuditEventsShareTheTransactionsInstant(t *testing.T) {
	const tenant spi.TenantID = "commit-audit-tenant"
	factory, tm, _, ctx := commitInstantFixture(t, tenant)
	store, err := factory.EntityStore(ctx)
	if err != nil {
		t.Fatalf("EntityStore: %v", err)
	}
	mref := spi.ModelRef{EntityName: "commit-audit", ModelVersion: "1"}
	id := uuid.NewString()

	txID, txCtx, err := tm.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if _, err := store.Save(txCtx, &spi.Entity{
		Meta: spi.EntityMeta{ID: id, ModelRef: mref}, Data: []byte(`{"n":1}`),
	}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	audit, err := factory.StateMachineAuditStore(txCtx)
	if err != nil {
		t.Fatalf("StateMachineAuditStore: %v", err)
	}
	processClock := time.Now().UTC().Add(-time.Hour)
	if err := audit.Record(txCtx, id, spi.StateMachineEvent{
		EventType:     spi.SMEventTransitionMade,
		EntityID:      id,
		State:         "NEW",
		TransactionID: txID,
		Details:       "commit-instant",
		Timestamp:     processClock,
	}); err != nil {
		t.Fatalf("Record: %v", err)
	}
	if err := tm.Commit(txCtx, txID); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	submit, err := tm.GetSubmitTime(ctx, txID)
	if err != nil {
		t.Fatalf("GetSubmitTime: %v", err)
	}
	committedAudit, err := factory.StateMachineAuditStore(ctx)
	if err != nil {
		t.Fatalf("StateMachineAuditStore (committed): %v", err)
	}
	events, err := committedAudit.GetEvents(ctx, id)
	if err != nil {
		t.Fatalf("GetEvents: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 audit event, got %d", len(events))
	}
	if !events[0].Timestamp.Equal(submit) {
		t.Errorf("an audit event must report its transaction's commit instant, not the recording process's clock: got %s, want %s",
			events[0].Timestamp, submit)
	}
}

// TestGetVersionMetadata_WindowBoundsOnCommitInstant asserts the history
// window filters on the instant a revision COMMITTED. A revision written by a
// transaction that started before the window and committed inside it must be
// included; the transaction-start instant would exclude it.
func TestGetVersionMetadata_WindowBoundsOnCommitInstant(t *testing.T) {
	factory, tm, pool, ctx := commitInstantFixture(t, "history-window-tenant")
	store, err := factory.EntityStore(ctx)
	if err != nil {
		t.Fatalf("EntityStore: %v", err)
	}
	id := uuid.NewString()
	mref := spi.ModelRef{EntityName: "history-window", ModelVersion: "1"}

	txID, txCtx, err := tm.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if _, err := store.Save(txCtx, &spi.Entity{
		Meta: spi.EntityMeta{ID: id, ModelRef: mref}, Data: []byte(`{"n":1}`),
	}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	// The window opens AFTER the transaction started but BEFORE it committed.
	from := clockNow(t, ctx, pool)
	if err := tm.Commit(txCtx, txID); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	metas, err := store.GetVersionMetadata(ctx, id, spi.VersionMetadataOptions{From: &from})
	if err != nil {
		t.Fatalf("GetVersionMetadata: %v", err)
	}
	if len(metas) != 1 {
		t.Fatalf("a revision committed inside the window must be in it: got %d versions", len(metas))
	}
}

// TestNonTxSave_StampedAtItsOwnCommit is the non-transactional half of the
// same contract. A non-transactional Save opens its own transaction with no
// SPI transaction and no transaction id, so the commit-phase stamp never sees
// it: without a stamp of its own the row keeps the column default —
// CURRENT_TIMESTAMP, its transaction's START — as its permanent recorded time.
//
// The property is proven deterministically rather than by racing clocks: a
// manually held row lock blocks the save's own transaction after that
// transaction has already fixed its start time, and the instant read while
// the lock is still held is therefore later than that start. A write stamped
// at transaction start is dated BEFORE that instant; one stamped at its own
// commit is dated after it.
func TestNonTxSave_StampedAtItsOwnCommit(t *testing.T) {
	const tenant spi.TenantID = "nontx-save-stamp-tenant"
	factory, _, pool, ctx := commitInstantFixture(t, tenant)
	store, err := factory.EntityStore(ctx)
	if err != nil {
		t.Fatalf("EntityStore: %v", err)
	}
	mref := spi.ModelRef{EntityName: "nontx-save-stamp", ModelVersion: "1"}
	id := uuid.NewString()
	if _, err := store.Save(ctx, &spi.Entity{
		Meta: spi.EntityMeta{ID: id, ModelRef: mref}, Data: []byte(`{"n":0}`),
	}); err != nil {
		t.Fatalf("seed Save: %v", err)
	}

	heldUntil := runBlockedByRowLock(t, ctx, pool, string(tenant), id, func() error {
		_, err := store.Save(ctx, &spi.Entity{
			Meta: spi.EntityMeta{ID: id, ModelRef: mref}, Data: []byte(`{"n":1}`),
		})
		return err
	})

	var validTime, txTime, lastModified time.Time
	if err := pool.QueryRow(ctx,
		`SELECT v.valid_time, v.transaction_time, e.last_modified
		   FROM entity_versions v JOIN entities e
		     ON e.tenant_id = v.tenant_id AND e.entity_id = v.entity_id
		  WHERE v.tenant_id = $1 AND v.entity_id = $2 AND v.version = 2`,
		string(tenant), id).Scan(&validTime, &txTime, &lastModified); err != nil {
		t.Fatalf("read back the new version: %v", err)
	}
	if validTime.Before(heldUntil) {
		t.Errorf("a non-transactional save must be dated at its own commit, not at its transaction's start: valid_time %s precedes the still-held lock at %s",
			validTime, heldUntil)
	}
	if !txTime.Equal(validTime) {
		t.Errorf("valid_time and transaction_time must be the same instant: valid=%s transaction=%s", validTime, txTime)
	}
	if lastModified.Before(heldUntil) {
		t.Errorf("the entity's last_modified must be its write's commit instant: %s precedes the still-held lock at %s", lastModified, heldUntil)
	}
}

// TestNonTxDelete_StampedAtItsOwnCommit is the same contract for a
// non-transactional delete, which writes its tombstone through a different
// statement pair (deleteOn, not saveOn).
func TestNonTxDelete_StampedAtItsOwnCommit(t *testing.T) {
	const tenant spi.TenantID = "nontx-delete-stamp-tenant"
	factory, _, pool, ctx := commitInstantFixture(t, tenant)
	store, err := factory.EntityStore(ctx)
	if err != nil {
		t.Fatalf("EntityStore: %v", err)
	}
	mref := spi.ModelRef{EntityName: "nontx-delete-stamp", ModelVersion: "1"}
	id := uuid.NewString()
	if _, err := store.Save(ctx, &spi.Entity{
		Meta: spi.EntityMeta{ID: id, ModelRef: mref}, Data: []byte(`{"n":0}`),
	}); err != nil {
		t.Fatalf("seed Save: %v", err)
	}
	var created time.Time
	if err := pool.QueryRow(ctx,
		`SELECT creation_date FROM entities WHERE tenant_id = $1 AND entity_id = $2`,
		string(tenant), id).Scan(&created); err != nil {
		t.Fatalf("read creation_date: %v", err)
	}

	heldUntil := runBlockedByRowLock(t, ctx, pool, string(tenant), id, func() error {
		return store.Delete(ctx, id)
	})

	var validTime, tombstoneCreation, entityCreation time.Time
	if err := pool.QueryRow(ctx,
		`SELECT v.valid_time, v.creation_date, e.creation_date
		   FROM entity_versions v JOIN entities e
		     ON e.tenant_id = v.tenant_id AND e.entity_id = v.entity_id
		  WHERE v.tenant_id = $1 AND v.entity_id = $2 AND v.version = 2`,
		string(tenant), id).Scan(&validTime, &tombstoneCreation, &entityCreation); err != nil {
		t.Fatalf("read back the tombstone: %v", err)
	}
	if validTime.Before(heldUntil) {
		t.Errorf("a non-transactional delete must be dated at its own commit: valid_time %s precedes the still-held lock at %s", validTime, heldUntil)
	}
	if !tombstoneCreation.Equal(created) {
		t.Errorf("a tombstone must report when the entity was CREATED, not when it was deleted: got %s, want %s", tombstoneCreation, created)
	}
	if !entityCreation.Equal(created) {
		t.Errorf("a delete must not restamp the entity's creation_date: got %s, want %s", entityCreation, created)
	}
}

// runBlockedByRowLock runs write while a manually held FOR UPDATE lock on the
// entity's row blocks it, and returns an instant read from the database clock
// while that lock is still held — so everything the blocked write goes on to
// do happens after the returned instant, while its own transaction started
// before it.
//
// It waits for a backend to be demonstrably blocked rather than sleeping a
// guessed interval: releasing the lock before the writer ever queued would
// pass the caller's assertion without exercising anything. The same technique
// as TestNonTxCompareAndSave_StampsAfterTheLockWait.
func runBlockedByRowLock(t *testing.T, ctx context.Context, pool *pgxpool.Pool, tenant, entityID string, write func() error) time.Time {
	t.Helper()

	holder, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin lock holder: %v", err)
	}
	defer func() { _ = holder.Rollback(context.Background()) }()
	if _, err := holder.Exec(ctx,
		`SELECT 1 FROM entities WHERE tenant_id = $1 AND entity_id = $2 FOR UPDATE`,
		tenant, entityID); err != nil {
		t.Fatalf("take the row lock: %v", err)
	}
	// The holder's own backend is excluded from the poll below, and the poll
	// is narrowed to a statement against `entities`. A bare "is any backend in
	// this database waiting on a lock" poll would be satisfied by any other
	// lock wait in the database — including one the holder itself entered —
	// and would then release the lock before the writer had queued, passing
	// the caller's assertion without exercising anything.
	var holderPID int
	if err := holder.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&holderPID); err != nil {
		t.Fatalf("read the holder's backend pid: %v", err)
	}

	done := make(chan error, 1)
	go func() { done <- write() }()

	waitStart := time.Now()
	for {
		var blocked int
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM pg_stat_activity
			 WHERE datname = current_database() AND wait_event_type = 'Lock'
			   AND pid <> $1 AND query LIKE '%entities%'`, holderPID).Scan(&blocked); err != nil {
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

	var heldUntil time.Time
	if err := holder.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&heldUntil); err != nil {
		t.Fatalf("read the holder's clock: %v", err)
	}
	if err := holder.Rollback(ctx); err != nil {
		t.Fatalf("release the row lock: %v", err)
	}
	if err := <-done; err != nil {
		t.Fatalf("the write that waited on the lock: %v", err)
	}
	return heldUntil
}
