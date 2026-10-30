package postgres_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/plugins/postgres"
)

// fixedClock is a business clock that reads one instant.
type fixedClock struct{ t time.Time }

func (c fixedClock) Now() time.Time { return c.t }

// clockStart is years away from real time, so a business date taken from the
// database clock instead of the supplied one is plain to see.
var clockStart = time.Date(2015, 3, 1, 0, 0, 0, 0, time.UTC)

// openClockFactory builds a factory with business clock c on a database of
// its own, so a stamp floor moved by another test cannot reach it. The pool
// has the conformance pool's settings — health check off, for the pgxpool
// close race newConformancePool describes — and the clock goes in through the
// internal constructor Plugin.NewFactory uses; TestPlugin_NewFactoryPassesClock
// covers that wiring. The returned func closes the factory and drops the
// database. It takes no *testing.T: the conformance harness calls it from its
// subtests through ClockFactory.
func openClockFactory(c spi.Clock) (*postgres.StoreFactory, func() error, error) {
	dsn, drop, err := postgres.CreateTestDatabase(os.Getenv("CYODA_TEST_DB_URL"), "cyoda_clock_")
	if err != nil {
		return nil, nil, err
	}
	fail := func(err error) (*postgres.StoreFactory, func() error, error) {
		return nil, nil, errors.Join(err, drop())
	}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return fail(errors.New("parse the clock database DSN"))
	}
	cfg.MaxConns = 10
	cfg.MinConns = 0
	cfg.MaxConnIdleTime = 30 * time.Second
	cfg.HealthCheckPeriod = 24 * time.Hour
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		return fail(fmt.Errorf("create the clock pool: %w", err))
	}
	if err := postgres.Migrate(pool); err != nil {
		pool.Close()
		return fail(fmt.Errorf("migrate the clock database: %w", err))
	}
	f := postgres.NewStoreFactoryWithClockForTest(pool, c)
	f.InitTransactionManager(newTestUUIDGenerator())
	closeF := func() error {
		postgres.CloseSchedulerPoolsForTest(f)
		pool.Close()
		return drop()
	}
	return f, closeF, nil
}

// mustOpenClockFactory is openClockFactory for a test of its own.
func mustOpenClockFactory(t *testing.T, c spi.Clock) *postgres.StoreFactory {
	t.Helper()
	f, closeF, err := openClockFactory(c)
	if err != nil {
		t.Fatalf("open clock factory: %v", err)
	}
	t.Cleanup(func() {
		if err := closeF(); err != nil {
			t.Errorf("close clock factory: %v", err)
		}
	})
	return f
}

// mustOpenPluginFactory builds a factory through the plugin's own NewFactory —
// the production wiring, which runs the migrations — with opts, on a database
// of its own.
func mustOpenPluginFactory(t *testing.T, opts ...spi.FactoryOption) spi.StoreFactory {
	t.Helper()
	dsn, drop, err := postgres.CreateTestDatabase(testDBURL(t), "cyoda_plugin_")
	if err != nil {
		t.Fatalf("fresh database: %v", err)
	}
	t.Cleanup(func() {
		if err := drop(); err != nil {
			t.Errorf("%v", err)
		}
	})
	env := map[string]string{
		"CYODA_POSTGRES_URL":             dsn,
		"CYODA_POSTGRES_MAX_CONNS":       "10",
		"CYODA_POSTGRES_MIN_CONNS":       "0",
		"CYODA_POSTGRES_SCHEDULER_CONNS": "2",
	}
	p, ok := spi.GetPlugin("postgres")
	if !ok {
		t.Fatal("postgres plugin not registered")
	}
	f, err := p.NewFactory(context.Background(), func(k string) string { return env[k] }, opts...)
	if err != nil {
		t.Fatalf("NewFactory: %v", err)
	}
	t.Cleanup(func() {
		if err := f.Close(); err != nil {
			t.Errorf("close factory: %v", err)
		}
	})
	return f
}

// Plugin.NewFactory hands spi.WithClock's clock to every store that takes
// business time from it: the transaction manager (a commit's stamp), the
// entity store (the stamp of a write with no transaction) and the async
// search store (the reaping cutoff).
func TestPlugin_NewFactoryPassesClock(t *testing.T) {
	f := mustOpenPluginFactory(t, spi.WithClock(fixedClock{clockStart}))
	ctx := ctxWithTenant(ctTenant)
	near := func(what string, got time.Time) {
		t.Helper()
		if d := got.Sub(clockStart); d < -time.Second || d > time.Second {
			t.Errorf("%s = %v, want within 1s of the clock's %v", what, got, clockStart)
		}
	}

	tm, err := f.TransactionManager(ctx)
	if err != nil {
		t.Fatalf("TransactionManager: %v", err)
	}
	txID, txCtx, err := tm.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	txStore, err := f.EntityStore(txCtx)
	if err != nil {
		t.Fatalf("EntityStore: %v", err)
	}
	if _, err := txStore.Save(txCtx, &spi.Entity{
		Meta: spi.EntityMeta{ID: uuid.NewString(), ModelRef: ctModel}, Data: []byte(`{"n":1}`),
	}); err != nil {
		t.Fatalf("Save in a transaction: %v", err)
	}
	if err := tm.Commit(ctx, txID); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	submit, err := tm.GetSubmitTime(ctx, txID)
	if err != nil {
		t.Fatalf("GetSubmitTime: %v", err)
	}
	near("the transaction manager's commit stamp", submit)

	es, err := f.EntityStore(ctx)
	if err != nil {
		t.Fatalf("EntityStore: %v", err)
	}
	id := uuid.NewString()
	if _, err := es.Save(ctx, &spi.Entity{
		Meta: spi.EntityMeta{ID: id, ModelRef: ctModel}, Data: []byte(`{"n":1}`),
	}); err != nil {
		t.Fatalf("Save without a transaction: %v", err)
	}
	got, err := es.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	near("the entity store's own commit stamp", got.Meta.CreationDate)

	// A job finished at the clock's now is not expired by a one-hour TTL on
	// that clock; on physical time it would be years old.
	as, err := f.AsyncSearchStore(ctx)
	if err != nil {
		t.Fatalf("AsyncSearchStore: %v", err)
	}
	jobID := uuid.NewString()
	if err := as.CreateJob(ctx, &spi.SearchJob{
		ID: jobID, TenantID: ctTenant, Status: "RUNNING", ModelRef: ctModel,
		PointInTime: clockStart, CreateTime: clockStart,
	}); err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	if err := as.UpdateJobStatus(ctx, jobID, 1, "SUCCESSFUL", 0, "", clockStart, 0); err != nil {
		t.Fatalf("UpdateJobStatus: %v", err)
	}
	if _, err := as.ReapExpired(context.Background(), time.Hour); err != nil {
		t.Fatalf("ReapExpired: %v", err)
	}
	if _, err := as.GetJob(ctx, jobID); err != nil {
		t.Errorf("the async search store reaped a job finished at the clock's now: %v", err)
	}
}

// Placeholder dates fill a row until the commit stamp replaces them, and a
// read inside the same transaction sees them. Under a business clock they are
// that clock's time, not the database's.
func TestClock_PlaceholderDatesInsideTx(t *testing.T) {
	f := mustOpenClockFactory(t, fixedClock{clockStart})
	ctx := ctxWithTenant(ctTenant)
	tm, err := f.TransactionManager(ctx)
	if err != nil {
		t.Fatalf("TransactionManager: %v", err)
	}
	txID, txCtx, err := tm.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	t.Cleanup(func() { _ = tm.Rollback(ctx, txID) })
	es, err := f.EntityStore(txCtx)
	if err != nil {
		t.Fatalf("EntityStore: %v", err)
	}
	id := uuid.NewString()
	if _, err := es.Save(txCtx, &spi.Entity{
		Meta: spi.EntityMeta{ID: id, ModelRef: ctModel}, Data: []byte(`{"n":1}`),
	}); err != nil {
		t.Fatalf("Save: %v", err)
	}

	near := func(what string, got time.Time) {
		t.Helper()
		if d := got.Sub(clockStart); d < -time.Second || d > time.Second {
			t.Errorf("%s inside the transaction = %v, want within 1s of the clock's %v", what, got, clockStart)
		}
	}
	got, err := es.Get(txCtx, id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	near("CreationDate", got.Meta.CreationDate)
	near("LastModifiedDate", got.Meta.LastModifiedDate)

	// The version row's transaction_time is its LastModifiedDate.
	v, err := es.GetVersionByTransaction(txCtx, id, txID)
	if err != nil {
		t.Fatalf("GetVersionByTransaction: %v", err)
	}
	near("version CreationDate", v.Entity.Meta.CreationDate)
	near("version LastModifiedDate (transaction_time)", v.Entity.Meta.LastModifiedDate)
	near("version Timestamp (valid_time)", v.Timestamp)
}

// Without a clock — production — commit stamps and consistency times come
// from the database clock, exactly as before the business clock existed.
func TestNoClock_StampFunctionsUnchanged(t *testing.T) {
	f := mustOpenPluginFactory(t)
	ctx := ctxWithTenant(ctTenant)
	tm, err := f.TransactionManager(ctx)
	if err != nil {
		t.Fatalf("TransactionManager: %v", err)
	}
	pool := mustPoolOf(t, f)
	dbNow := func() time.Time {
		t.Helper()
		var now time.Time
		if err := pool.QueryRow(context.Background(), `SELECT clock_timestamp()`).Scan(&now); err != nil {
			t.Fatalf("read the database clock: %v", err)
		}
		return now
	}
	near := func(what string, got, want time.Time) {
		t.Helper()
		if d := got.Sub(want); d < -time.Second || d > time.Second {
			t.Errorf("%s = %v, want within 1s of the database clock %v", what, got, want)
		}
	}

	txID, txCtx, err := tm.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	es, err := f.EntityStore(txCtx)
	if err != nil {
		t.Fatalf("EntityStore: %v", err)
	}
	id := uuid.NewString()
	if _, err := es.Save(txCtx, &spi.Entity{
		Meta: spi.EntityMeta{ID: id, ModelRef: ctModel}, Data: []byte(`{"n":1}`),
	}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if err := tm.Commit(ctx, txID); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	var stamp time.Time
	if err := pool.QueryRow(context.Background(),
		`SELECT transaction_time FROM entity_versions WHERE tenant_id = $1 AND entity_id = $2`,
		string(ctTenant), id).Scan(&stamp); err != nil {
		t.Fatalf("read the commit stamp: %v", err)
	}
	near("the commit stamp", stamp, dbNow())

	c, err := tm.ConsistencyTime(ctx)
	if err != nil {
		t.Fatalf("ConsistencyTime: %v", err)
	}
	near("the consistency time", c, dbNow())
}

// mustPoolOf returns the pool of a factory the plugin built.
func mustPoolOf(t *testing.T, f spi.StoreFactory) *pgxpool.Pool {
	t.Helper()
	pf, ok := f.(interface{ Pool() *pgxpool.Pool })
	if !ok {
		t.Fatalf("factory %T exposes no pool", f)
	}
	return pf.Pool()
}
