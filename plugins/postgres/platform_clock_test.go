package postgres_test

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// fixedClock is a business clock that reads one instant.
type fixedClock struct{ t time.Time }

func (c fixedClock) Now() time.Time { return c.t }

// clockStart is years away from real time, so a business date taken from the
// database clock instead of the supplied one is plain to see.
var clockStart = time.Date(2015, 3, 1, 0, 0, 0, 0, time.UTC)

// openPluginFactory builds a factory through the plugin's own NewFactory — the
// production wiring, which runs the migrations — on a database of its own, so
// a stamp floor moved by another test cannot reach it. The returned func
// closes the factory and drops the database. It takes no *testing.T: the
// conformance harness calls it from its subtests through ClockFactory.
func openPluginFactory(opts ...spi.FactoryOption) (spi.StoreFactory, func(), error) {
	base := os.Getenv("CYODA_TEST_DB_URL")
	if base == "" {
		return nil, nil, errors.New("CYODA_TEST_DB_URL is empty — TestMain must provision a database")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	name := "cyoda_clock_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:16]
	quoted := pgx.Identifier{name}.Sanitize()
	admin, err := pgx.Connect(ctx, base)
	if err != nil {
		return nil, nil, fmt.Errorf("open admin connection: %w", err)
	}
	defer func() { _ = admin.Close(context.Background()) }()
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+quoted); err != nil {
		return nil, nil, fmt.Errorf("create %s: %w", name, err)
	}
	drop := func() {
		dropCtx, dropCancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer dropCancel()
		c, err := pgx.Connect(dropCtx, base)
		if err != nil {
			return
		}
		defer func() { _ = c.Close(context.Background()) }()
		_, _ = c.Exec(dropCtx, "DROP DATABASE IF EXISTS "+quoted+" WITH (FORCE)")
	}

	u, err := url.Parse(base)
	if err != nil {
		drop()
		// errors.Unwrap: *url.Error renders the URL, which may carry a password.
		return nil, nil, fmt.Errorf("parse CYODA_TEST_DB_URL: %w", errors.Unwrap(err))
	}
	u.Path = "/" + name
	env := map[string]string{
		"CYODA_POSTGRES_URL":             u.String(),
		"CYODA_POSTGRES_MAX_CONNS":       "10",
		"CYODA_POSTGRES_MIN_CONNS":       "0",
		"CYODA_POSTGRES_SCHEDULER_CONNS": "2",
	}
	p, ok := spi.GetPlugin("postgres")
	if !ok {
		drop()
		return nil, nil, errors.New("postgres plugin not registered")
	}
	f, err := p.NewFactory(ctx, func(k string) string { return env[k] }, opts...)
	if err != nil {
		drop()
		return nil, nil, fmt.Errorf("NewFactory: %w", err)
	}
	return f, func() { _ = f.Close(); drop() }, nil
}

func mustOpenPluginFactory(t *testing.T, opts ...spi.FactoryOption) spi.StoreFactory {
	t.Helper()
	f, closeF, err := openPluginFactory(opts...)
	if err != nil {
		t.Fatalf("open plugin factory: %v", err)
	}
	t.Cleanup(closeF)
	return f
}

// Placeholder dates fill a row until the commit stamp replaces them, and a
// read inside the same transaction sees them. Under a business clock they are
// that clock's time, not the database's.
func TestClock_PlaceholderDatesInsideTx(t *testing.T) {
	f := mustOpenPluginFactory(t, spi.WithClock(fixedClock{clockStart}))
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
