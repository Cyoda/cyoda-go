package postgres_test

// consistency_time_test.go — white-box proof of the postgres consistency time:
// the stamp floor, the in-flight marker every commit holds from its stamp to
// its COMMIT, and ConsistencyTime's "reserve, then wait". The cross-backend
// conformance group (spitest ConsistencyTime) asserts the contract; these
// tests pin the mechanism that meets it — that the wait happens, for whom,
// for how long, and what each failure is reported as.
//
// Waits are observed in pg_locks rather than guessed with sleeps: a backend
// queued for a ShareLock on an advisory key in the two-int form
// (objsubid = 2) is ConsistencyTime waiting for a marker, and nothing else in
// this plugin takes that lock.

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgconn/ctxwatch"
	"github.com/jackc/pgx/v5/pgxpool"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/plugins/postgres"
)

const ctTenant spi.TenantID = "ct-tenant"

var ctModel = spi.ModelRef{EntityName: "consistency-time", ModelVersion: "1"}

// newCTPool opens a pool on dsn with the package's pgx workaround
// (HealthCheckPeriod disabled; see newTestPoolSized). configure, when not nil,
// adjusts the config before the pool opens.
func newCTPool(t *testing.T, dsn string, maxConns int32, configure func(*pgxpool.Config)) *pgxpool.Pool {
	t.Helper()
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse pool config: %v", err)
	}
	cfg.MaxConns = maxConns
	cfg.MinConns = 0
	cfg.MaxConnIdleTime = 60 * time.Second
	cfg.HealthCheckPeriod = 24 * time.Hour
	if configure != nil {
		configure(cfg)
	}
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("create pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// resetSchema gives the shared test database a freshly migrated schema for
// this test, and drops it afterwards.
func resetSchema(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	if err := postgres.DropSchemaForTest(pool); err != nil {
		t.Fatalf("reset schema: %v", err)
	}
	if err := postgres.Migrate(pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	t.Cleanup(func() { _ = postgres.DropSchemaForTest(pool) })
}

// newCTFactory is a migrated factory on the shared test database, wired by
// the production path (InitTransactionManager over the shipped config), and a
// context for ctTenant.
func newCTFactory(t *testing.T) (*postgres.StoreFactory, context.Context) {
	t.Helper()
	pool := newCTPool(t, testDBURL(t), 10, nil)
	resetSchema(t, pool)
	f := postgres.NewStoreFactory(pool)
	f.InitTransactionManager(newTestUUIDGenerator())
	return f, ctxWithTenant(ctTenant)
}

// newCTFactoryWithStatementTimeout is newCTFactory with the configured
// statement timeout set to d. Only the manager reads it (as the wait budget):
// the pool's own connections keep the server's statement_timeout, so the
// budget, not the server, is what ends a wait.
func newCTFactoryWithStatementTimeout(t *testing.T, d time.Duration) (*postgres.StoreFactory, context.Context) {
	t.Helper()
	pool := newCTPool(t, testDBURL(t), 10, nil)
	resetSchema(t, pool)
	f := postgres.NewStoreFactoryWithStatementTimeoutForTest(pool, d)
	f.InitTransactionManager(newTestUUIDGenerator())
	return f, ctxWithTenant(ctTenant)
}

// freshCTDatabase creates an empty database on the test server and returns
// its DSN. A test that moves the stamp floor takes one: the floor is a
// database object, and moving it in the shared database would leak into the
// next test's stamps until that test reset the schema.
func freshCTDatabase(t *testing.T) string {
	t.Helper()
	base := testDBURL(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	admin, err := pgxpool.New(ctx, base)
	if err != nil {
		t.Fatalf("open admin pool: %v", err)
	}
	t.Cleanup(admin.Close)

	name := "cyoda_ct_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:16]
	quoted := pgx.Identifier{name}.Sanitize()
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+quoted); err != nil {
		t.Fatalf("create %s: %v", name, err)
	}
	t.Cleanup(func() {
		dropCtx, dropCancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer dropCancel()
		if _, err := admin.Exec(dropCtx, "DROP DATABASE IF EXISTS "+quoted+" WITH (FORCE)"); err != nil {
			t.Errorf("drop %s: %v", name, err)
		}
	})

	u, err := url.Parse(base)
	if err != nil {
		// errors.Unwrap: *url.Error renders the URL, which may carry a password.
		t.Fatalf("parse CYODA_TEST_DB_URL: %v", errors.Unwrap(err))
	}
	u.Path = "/" + name
	return u.String()
}

// newCTFactoryOwnDB is newCTFactory on a database of its own.
func newCTFactoryOwnDB(t *testing.T) (*postgres.StoreFactory, context.Context) {
	t.Helper()
	pool := newCTPool(t, freshCTDatabase(t), 10, nil)
	if err := postgres.Migrate(pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	f := postgres.NewStoreFactory(pool)
	f.InitTransactionManager(newTestUUIDGenerator())
	return f, ctxWithTenant(ctTenant)
}

// newCTFactoryTinyPool is a factory over a pool of one connection with a
// 200 ms acquire deadline.
func newCTFactoryTinyPool(t *testing.T) (*postgres.StoreFactory, context.Context) {
	t.Helper()
	pool := newCTPool(t, testDBURL(t), 1, nil)
	resetSchema(t, pool)
	f := postgres.NewStoreFactoryWithAcquireTimeoutForTest(pool, 200*time.Millisecond)
	f.InitTransactionManager(newTestUUIDGenerator())
	return f, ctxWithTenant(ctTenant)
}

// commitOneEntityErr begins a transaction, saves one new entity in it and
// commits, returning the transaction id and the Commit (or earlier) error.
func commitOneEntityErr(t *testing.T, f *postgres.StoreFactory, ctx context.Context) (string, error) {
	t.Helper()
	tm, err := f.TransactionManager(ctx)
	if err != nil {
		t.Fatalf("TransactionManager: %v", err)
	}
	txID, txCtx, err := tm.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	es, err := f.EntityStore(txCtx)
	if err != nil {
		t.Fatalf("EntityStore: %v", err)
	}
	if _, err := es.Save(txCtx, &spi.Entity{
		Meta: spi.EntityMeta{ID: uuid.NewString(), ModelRef: ctModel}, Data: []byte(`{"n":1}`),
	}); err != nil {
		_ = tm.Rollback(txCtx, txID)
		t.Fatalf("Save: %v", err)
	}
	return txID, tm.Commit(txCtx, txID)
}

// commitOneEntity is commitOneEntityErr for a commit that must succeed.
func commitOneEntity(t *testing.T, f *postgres.StoreFactory, ctx context.Context) string {
	t.Helper()
	txID, err := commitOneEntityErr(t, f, ctx)
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}
	return txID
}

// heldStamp is a transaction that has called cyoda_stamp and not ended: a
// commit frozen between its stamp and its COMMIT.
type heldStamp struct {
	stamp  time.Time
	pid    int
	commit func()
}

// holdStamp opens a transaction on pool, stamps it for tenant and leaves it
// open. Cleanup rolls it back unless commit ran.
func holdStamp(t *testing.T, pool *pgxpool.Pool, tenant spi.TenantID) heldStamp {
	t.Helper()
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin holder: %v", err)
	}
	done := false
	t.Cleanup(func() {
		if !done {
			_ = tx.Rollback(context.Background())
		}
	})
	var h heldStamp
	if err := tx.QueryRow(ctx, `SELECT cyoda_stamp($1), pg_backend_pid()`, string(tenant)).Scan(&h.stamp, &h.pid); err != nil {
		t.Fatalf("stamp holder: %v", err)
	}
	h.commit = func() {
		done = true
		if err := tx.Commit(context.Background()); err != nil {
			t.Errorf("commit holder: %v", err)
		}
	}
	return h
}

// markerWaiters counts backends of this database queued for a marker:
// ConsistencyTime's shared lock on an in-flight marker, not yet granted.
func markerWaiters(t *testing.T, pool *pgxpool.Pool) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM pg_locks
		  WHERE locktype = 'advisory' AND objsubid = 2 AND mode = 'ShareLock' AND NOT granted
		    AND database = (SELECT oid FROM pg_database WHERE datname = current_database())`).Scan(&n); err != nil {
		t.Fatalf("poll pg_locks: %v", err)
	}
	return n
}

// waitForMarkerWaiter blocks until a backend is queued for a marker.
func waitForMarkerWaiter(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for markerWaiters(t, pool) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("no backend ever queued for an in-flight marker")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

type ctResult struct {
	c   time.Time
	err error
}

// consistencyTimeAsync runs ConsistencyTime on its own goroutine.
func consistencyTimeAsync(ctx context.Context, tm spi.TransactionManager) <-chan ctResult {
	out := make(chan ctResult, 1)
	go func() {
		c, err := tm.ConsistencyTime(ctx)
		out <- ctResult{c: c, err: err}
	}()
	return out
}

// prewarm opens n connections on pool and returns them idle, so a timed
// section draws on open connections instead of dialling under load.
func prewarm(t *testing.T, pool *pgxpool.Pool, n int) {
	t.Helper()
	conns := make([]*pgxpool.Conn, 0, n)
	for i := 0; i < n; i++ {
		conns = append(conns, acquireCT(t, pool))
	}
	for _, c := range conns {
		c.Release()
	}
}

// acquireCT takes a connection from pool, released at cleanup unless the
// caller releases it first.
func acquireCT(t *testing.T, pool *pgxpool.Pool) *pgxpool.Conn {
	t.Helper()
	c, err := pool.Acquire(context.Background())
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	t.Cleanup(c.Release)
	return c
}

func ctTM(t *testing.T, f *postgres.StoreFactory, ctx context.Context) spi.TransactionManager {
	t.Helper()
	tm, err := f.TransactionManager(ctx)
	if err != nil {
		t.Fatalf("TransactionManager: %v", err)
	}
	return tm
}

// A transaction that has called cyoda_stamp and not committed makes
// ConsistencyTime wait; COMMIT releases it, and C is at or above the held
// stamp.
func TestConsistencyTime_WaitsForAStampedTransaction(t *testing.T) {
	f, ctx := newCTFactory(t)
	pool := postgres.PoolForTest(f)
	held := holdStamp(t, pool, ctTenant)

	got := consistencyTimeAsync(ctx, ctTM(t, f, ctx))
	waitForMarkerWaiter(t, pool)
	select {
	case r := <-got:
		t.Fatalf("ConsistencyTime returned while a stamped transaction was open: %v, %v", r.c, r.err)
	default:
	}

	held.commit()
	r := <-got
	if r.err != nil {
		t.Fatalf("ConsistencyTime: %v", r.err)
	}
	if r.c.Before(held.stamp) {
		t.Fatalf("C %v is below the stamp %v it waited for", r.c, held.stamp)
	}
}

// The marker is taken by TransactionManager.Commit itself and held through
// its COMMIT: ConsistencyTime waits for a real commit stopped after its stamp,
// and C covers that commit's submit time.
//
// The commit is stopped by holding, uncommitted, a submit_times row with the
// committing transaction's key, so the commit's own submit_times INSERT waits
// on it — a deliberate breach of the design rule (nothing after the stamp may
// wait on a lock). cyoda_stamp's 2 s lock_timeout covers the rest of the
// commit, so the holder must let go within 2 s of the commit blocking; it does
// so as soon as ConsistencyTime is seen queued.
func TestConsistencyTime_WaitsForACommitInItsCommitPhase(t *testing.T) {
	f, ctx := newCTFactory(t)
	pool := postgres.PoolForTest(f)
	tm := ctTM(t, f, ctx)
	// The transaction, the blocker, the pollers and ConsistencyTime each take a
	// connection; opened now, none is dialled inside the 2 s window.
	prewarm(t, pool, 5)

	txID, txCtx, err := tm.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	es, err := f.EntityStore(txCtx)
	if err != nil {
		t.Fatalf("EntityStore: %v", err)
	}
	if _, err := es.Save(txCtx, &spi.Entity{
		Meta: spi.EntityMeta{ID: uuid.NewString(), ModelRef: ctModel}, Data: []byte(`{"n":1}`),
	}); err != nil {
		t.Fatalf("Save: %v", err)
	}

	blocker, err := pool.Begin(context.Background())
	if err != nil {
		t.Fatalf("begin blocker: %v", err)
	}
	t.Cleanup(func() { _ = blocker.Rollback(context.Background()) })
	if _, err := blocker.Exec(context.Background(),
		`INSERT INTO submit_times (tenant_id, tx_id, submit_time) VALUES ($1, $2, now())`,
		string(ctTenant), txID); err != nil {
		t.Fatalf("blocker insert: %v", err)
	}

	committed := make(chan error, 1)
	go func() { committed <- tm.Commit(txCtx, txID) }()

	deadline := time.Now().Add(10 * time.Second)
	for {
		var n int
		if err := pool.QueryRow(context.Background(),
			`SELECT count(*) FROM pg_stat_activity
			  WHERE datname = current_database() AND wait_event_type = 'Lock'
			    AND query LIKE '%INSERT INTO submit_times%'`).Scan(&n); err != nil {
			t.Fatalf("poll pg_stat_activity: %v", err)
		}
		if n > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the commit never blocked on its submit_times insert")
		}
		time.Sleep(10 * time.Millisecond)
	}

	got := consistencyTimeAsync(ctx, tm)
	waitForMarkerWaiter(t, pool)
	select {
	case r := <-got:
		t.Fatalf("ConsistencyTime returned while a commit was in its commit phase: %v, %v", r.c, r.err)
	default:
	}

	if err := blocker.Rollback(context.Background()); err != nil {
		t.Fatalf("release blocker: %v", err)
	}
	if err := <-committed; err != nil {
		t.Fatalf("Commit: %v", err)
	}
	r := <-got
	if r.err != nil {
		t.Fatalf("ConsistencyTime: %v", r.err)
	}
	submit, err := tm.GetSubmitTime(ctx, txID)
	if err != nil {
		t.Fatalf("GetSubmitTime: %v", err)
	}
	if submit.After(r.c) {
		t.Fatalf("C %v is below the submit time %v of the commit it waited for", r.c, submit)
	}
}

// Another tenant's held stamp does not delay this tenant's C. Waiting for it
// would block until the holder ended — at the latest when cyoda_stamp's 5 s
// idle-in-transaction limit aborts it — so returning within 2 s is the
// distinction.
func TestConsistencyTime_OtherTenantDoesNotDelay(t *testing.T) {
	f, ctx := newCTFactory(t)
	pool := postgres.PoolForTest(f)
	const other spi.TenantID = "some-other-tenant"
	var collide bool
	if err := pool.QueryRow(context.Background(), `SELECT hashtext($1) = hashtext($2)`,
		string(other), string(ctTenant)).Scan(&collide); err != nil {
		t.Fatalf("hashtext: %v", err)
	}
	if collide {
		t.Fatal("precondition: the two tenants' marker keys collide; pick another tenant name")
	}
	holdStamp(t, pool, other)

	start := time.Now()
	if _, err := ctTM(t, f, ctx).ConsistencyTime(ctx); err != nil {
		t.Fatalf("ConsistencyTime: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("ConsistencyTime took %v with only another tenant's stamp held", elapsed)
	}
}

// The SQL budget is enforced for the whole call, not per marker. Three markers
// are held; every 400 ms the one the call is waiting on is released. With a
// 1000 ms budget for the call it fails with 55P03 while waiting on the third
// (~1000 ms). A budget per marker would see each wait end inside 1000 ms and
// return C at ~1200 ms. The 400 ms step leaves the releaser up to 600 ms of
// slack for its first release on a loaded host; its queries and the call run
// on connections taken before the clock starts.
func TestConsistencyTimeSQL_BudgetIsPerCall(t *testing.T) {
	f, _ := newCTFactory(t)
	pool := postgres.PoolForTest(f)
	byPID := map[int]heldStamp{}
	for i := 0; i < 3; i++ {
		h := holdStamp(t, pool, ctTenant)
		byPID[h.pid] = h
	}
	checker := acquireCT(t, pool)
	poller := acquireCT(t, pool)

	stop := make(chan struct{})
	released := make(chan int, 1)
	go func() {
		n := 0
		defer func() { released <- n }()
		tick := time.NewTicker(400 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-stop:
				return
			case <-tick.C:
			}
			var pid int
			err := poller.QueryRow(context.Background(),
				`SELECT h.pid FROM pg_locks w JOIN pg_locks h
				    ON h.locktype = 'advisory' AND h.database = w.database AND h.classid = w.classid
				   AND h.objid = w.objid AND h.objsubid = 2 AND h.granted AND h.mode = 'ExclusiveLock'
				 WHERE w.locktype = 'advisory' AND w.objsubid = 2 AND w.mode = 'ShareLock' AND NOT w.granted
				   AND w.database = (SELECT oid FROM pg_database WHERE datname = current_database())
				 LIMIT 1`).Scan(&pid)
			if err != nil {
				continue // not waiting at this tick
			}
			if h, ok := byPID[pid]; ok {
				h.commit()
				delete(byPID, pid)
				n++
			}
		}
	}()

	start := time.Now()
	var c time.Time
	err := checker.QueryRow(context.Background(), `SELECT cyoda_consistency_time($1, 1000)`, string(ctTenant)).Scan(&c)
	elapsed := time.Since(start)
	close(stop)
	n := <-released

	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "55P03" {
		t.Fatalf("want SQLSTATE 55P03 from an exhausted budget, got C=%v err=%v (after %v, %d markers released)", c, err, elapsed, n)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("the budget ended the call after %v, not ~1000 ms", elapsed)
	}
	if n < 1 {
		t.Fatalf("no marker was released before the call failed; the test did not exercise a second wait")
	}
}

// The Go mapping: a budget overrun is ErrConsistencyTimeUnavailable, and the
// budget is the configured statement timeout when that is lower than 10 s;
// a caller that gives up gets its own context error.
func TestConsistencyTime_ErrorMapping(t *testing.T) {
	f, ctx := newCTFactoryWithStatementTimeout(t, 300*time.Millisecond)
	holdStamp(t, postgres.PoolForTest(f), ctTenant)
	tm := ctTM(t, f, ctx)

	start := time.Now()
	_, err := tm.ConsistencyTime(ctx)
	elapsed := time.Since(start)
	if !errors.Is(err, spi.ErrConsistencyTimeUnavailable) {
		t.Fatalf("want ErrConsistencyTimeUnavailable, got %v", err)
	}
	if elapsed < 250*time.Millisecond || elapsed > 5*time.Second {
		t.Fatalf("the 300 ms budget ended the wait after %v", elapsed)
	}

	cctx, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel()
	_, err = tm.ConsistencyTime(cctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want the caller's context error, got %v", err)
	}
	if errors.Is(err, spi.ErrConsistencyTimeUnavailable) {
		t.Fatalf("a caller that gave up must not read as an unavailable store: %v", err)
	}
}

// A caller's cancel can reach the server as a cancel request and come back as
// SQLSTATE 57014, the same code a statement_timeout raises. It is still the
// caller's context error. pgx's default context watcher breaks the socket
// instead, so this pool uses its cancel-request watcher to produce the 57014.
func TestConsistencyTime_ClientCancelAs57014IsTheContextError(t *testing.T) {
	pool := newCTPool(t, testDBURL(t), 10, func(c *pgxpool.Config) {
		c.ConnConfig.BuildContextWatcherHandler = func(pc *pgconn.PgConn) ctxwatch.Handler {
			return &pgconn.CancelRequestContextWatcherHandler{Conn: pc, DeadlineDelay: 5 * time.Second}
		}
	})
	resetSchema(t, pool)
	f := postgres.NewStoreFactory(pool)
	f.InitTransactionManager(newTestUUIDGenerator())
	ctx := ctxWithTenant(ctTenant)
	holdStamp(t, pool, ctTenant)

	cctx, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
	defer cancel()
	_, err := ctTM(t, f, ctx).ConsistencyTime(cctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want the caller's context error, got %v", err)
	}
	if errors.Is(err, spi.ErrConsistencyTimeUnavailable) {
		t.Fatalf("a caller that gave up must not read as an unavailable store: %v", err)
	}
}

// A server-side statement_timeout (57014) that ends the wait before the
// budget is also ErrConsistencyTimeUnavailable, not a ticketed 500.
func TestConsistencyTime_StatementTimeoutIsUnavailable(t *testing.T) {
	pool := newCTPool(t, testDBURL(t), 10, func(c *pgxpool.Config) {
		c.ConnConfig.RuntimeParams["statement_timeout"] = "300"
	})
	resetSchema(t, pool)
	f := postgres.NewStoreFactory(pool)
	f.InitTransactionManager(newTestUUIDGenerator())
	ctx := ctxWithTenant(ctTenant)
	holdStamp(t, pool, ctTenant)

	_, err := ctTM(t, f, ctx).ConsistencyTime(ctx)
	if !errors.Is(err, spi.ErrConsistencyTimeUnavailable) {
		t.Fatalf("want ErrConsistencyTimeUnavailable, got %v", err)
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "57014" {
		t.Fatalf("precondition: the wait must have been ended by statement_timeout (57014), got %v", err)
	}
}

// An error closes ConsistencyTime's connection instead of returning it to the
// pool: the next connection the pool hands out is a different backend.
func TestConsistencyTime_ErrorClosesConnection(t *testing.T) {
	ctPool := newCTPool(t, testDBURL(t), 1, nil)
	resetSchema(t, ctPool)
	f := postgres.NewStoreFactoryWithStatementTimeoutForTest(ctPool, 300*time.Millisecond)
	f.InitTransactionManager(newTestUUIDGenerator())
	ctx := ctxWithTenant(ctTenant)
	holdStamp(t, newCTPool(t, testDBURL(t), 2, nil), ctTenant)

	backend := func() int {
		var pid int
		if err := ctPool.QueryRow(context.Background(), `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
			t.Fatalf("read backend pid: %v", err)
		}
		return pid
	}
	before := backend()
	if _, err := ctTM(t, f, ctx).ConsistencyTime(ctx); !errors.Is(err, spi.ErrConsistencyTimeUnavailable) {
		t.Fatalf("want ErrConsistencyTimeUnavailable, got %v", err)
	}
	if after := backend(); after == before {
		t.Fatalf("the connection that failed (backend %d) went back to the pool", before)
	}
}

// A cancel during the wait leaves no advisory lock behind, and a later stamp
// proceeds at once.
func TestConsistencyTime_CancelLeavesNoLock(t *testing.T) {
	f, ctx := newCTFactory(t)
	pool := postgres.PoolForTest(f)
	held := holdStamp(t, pool, ctTenant)

	cctx, cancel := context.WithTimeout(ctx, 150*time.Millisecond)
	defer cancel()
	if _, err := ctTM(t, f, ctx).ConsistencyTime(cctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want the wait ended by the caller's deadline, got %v", err)
	}
	held.commit()

	// Polled, not read once: the cancelled backend releases its locks when it
	// processes the cancel, which may land just after the client returned. A
	// lock leaked on a live pooled session would never go.
	deadline := time.Now().Add(5 * time.Second)
	for {
		var n int
		if err := pool.QueryRow(context.Background(),
			`SELECT count(*) FROM pg_locks WHERE locktype = 'advisory' AND objsubid = 2 AND granted
			    AND database = (SELECT oid FROM pg_database WHERE datname = current_database())`).Scan(&n); err != nil {
			t.Fatalf("read pg_locks: %v", err)
		}
		if n == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d advisory locks outlived a cancelled call", n)
		}
		time.Sleep(20 * time.Millisecond)
	}

	// A leaked floor mutex would hold this stamp for its 2 s lock_timeout and
	// then fail it; 1 s separates the two with room for a loaded host.
	start := time.Now()
	holdStamp(t, pool, ctTenant).commit()
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("a stamp after the cancelled call took %v", elapsed)
	}
}

// The floor survives a DB clock behind it (own database): with the floor an
// hour ahead, C and the next stamps on both stamp paths are at or above it.
func TestConsistencyTime_FloorAheadOfClock(t *testing.T) {
	f, ctx := newCTFactoryOwnDB(t)
	pool := postgres.PoolForTest(f)
	ahead := time.Now().Add(time.Hour).UnixMicro()
	if _, err := pool.Exec(context.Background(), `SELECT setval('cyoda_stamp_floor', $1, true)`, ahead); err != nil {
		t.Fatalf("setval: %v", err)
	}
	tm := ctTM(t, f, ctx)
	c, err := tm.ConsistencyTime(ctx)
	if err != nil {
		t.Fatalf("ConsistencyTime: %v", err)
	}
	if c.UnixMicro() < ahead {
		t.Fatalf("C %v is below the floor", c)
	}

	// The transaction path: TransactionManager.Commit's stamp.
	txID := commitOneEntity(t, f, ctx)
	submit, err := tm.GetSubmitTime(ctx, txID)
	if err != nil {
		t.Fatalf("GetSubmitTime: %v", err)
	}
	if !submit.After(c) {
		t.Fatalf("a commit after C was stamped %v, not above C %v", submit, c)
	}

	// The non-transactional path: a save's own transaction stamps itself.
	es, err := f.EntityStore(ctx)
	if err != nil {
		t.Fatalf("EntityStore: %v", err)
	}
	id := uuid.NewString()
	if _, err := es.Save(ctx, &spi.Entity{
		Meta: spi.EntityMeta{ID: id, ModelRef: ctModel}, Data: []byte(`{"n":1}`),
	}); err != nil {
		t.Fatalf("non-transactional Save: %v", err)
	}
	var stamped time.Time
	if err := pool.QueryRow(context.Background(),
		`SELECT transaction_time FROM entity_versions WHERE tenant_id = $1 AND entity_id = $2`,
		string(ctTenant), id).Scan(&stamped); err != nil {
		t.Fatalf("read the version's stamp: %v", err)
	}
	if !stamped.After(submit) {
		t.Fatalf("a non-transactional save after the commit was stamped %v, not above %v", stamped, submit)
	}
}

// The migration seeds the floor from every stamp already stored, so a
// database migrated with data never issues a stamp or C below one of them.
// search_jobs.point_in_time is not a stamp: before this migration it held the
// caller's pointInTime as sent, with no check against the future, so seeding
// from it would let one old async submit dated far ahead push every tenant's
// stamps there for good. A job instant this migration finds is ignored.
func TestConsistencyTimeMigration_SeedsFloorFromStoredStamps(t *testing.T) {
	cases := []struct {
		name   string
		seed   string
		raises bool
	}{
		{"entity_versions", `
			INSERT INTO entities (tenant_id, entity_id, model_name, model_version, version, doc)
			VALUES ('seed', 'e1', 'm', '1', 1, '{}');
			INSERT INTO entity_versions (tenant_id, entity_id, model_name, model_version, version, valid_time, transaction_time, doc)
			VALUES ('seed', 'e1', 'm', '1', 1, now(), now() + interval '2 hours', '{}')`, true},
		{"submit_times", `
			INSERT INTO submit_times (tenant_id, tx_id, submit_time) VALUES ('seed', 'tx1', now() + interval '2 hours')`, true},
		{"search_jobs is ignored", `
			INSERT INTO search_jobs (id, tenant_id, model_name, model_ver, point_in_time)
			VALUES ('j1', 'seed', 'm', '1', '9999-01-01T00:00:00Z')`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pool := newCTPool(t, freshCTDatabase(t), 10, nil)
			if err := postgres.MigrateToVersionForTest(pool, 15); err != nil {
				t.Fatalf("migrate to 15: %v", err)
			}
			if _, err := pool.Exec(context.Background(), tc.seed); err != nil {
				t.Fatalf("seed: %v", err)
			}
			if err := postgres.Migrate(pool); err != nil {
				t.Fatalf("migrate: %v", err)
			}
			f := postgres.NewStoreFactory(pool)
			f.InitTransactionManager(newTestUUIDGenerator())
			ctx := ctxWithTenant(ctTenant)
			c, err := ctTM(t, f, ctx).ConsistencyTime(ctx)
			if err != nil {
				t.Fatalf("ConsistencyTime: %v", err)
			}
			if tc.raises && c.Before(time.Now().Add(90*time.Minute)) {
				t.Fatalf("C %v is below the stored stamp two hours ahead", c)
			}
			if !tc.raises && c.After(time.Now().Add(time.Hour)) {
				t.Fatalf("C %v was raised by a search job's instant, which is not a stamp", c)
			}
		})
	}
}

// The commit-phase stamp timing out on the floor mutex is retryable storage
// unavailability, not a 500 — on the transaction path and on a
// non-transactional save.
func TestStamp_LockTimeoutIsStorageUnavailable(t *testing.T) {
	f, ctx := newCTFactory(t)
	holdFloorMutex(t)

	assertRetryable := func(what string, err error) {
		t.Helper()
		if !storageUnavailable(err) {
			t.Fatalf("%s: want the storage-unavailable marker, got %v", what, err)
		}
		if errors.Is(err, spi.ErrConflict) {
			t.Fatalf("%s: a stamp timeout is not a conflict: %v", what, err)
		}
	}

	_, err := commitOneEntityErr(t, f, ctx)
	assertRetryable("Commit", err)

	es, err := f.EntityStore(ctx)
	if err != nil {
		t.Fatalf("EntityStore: %v", err)
	}
	_, err = es.Save(ctx, &spi.Entity{
		Meta: spi.EntityMeta{ID: uuid.NewString(), ModelRef: ctModel}, Data: []byte(`{"n":1}`),
	})
	assertRetryable("non-transactional Save", err)
}

// An acquire timeout while getting C carries the storage-unavailable marker.
func TestConsistencyTime_AcquireTimeoutIsStorageUnavailable(t *testing.T) {
	f, ctx := newCTFactoryTinyPool(t)
	held, err := postgres.PoolForTest(f).Acquire(context.Background())
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer held.Release()
	_, err = ctTM(t, f, ctx).ConsistencyTime(ctx)
	if !storageUnavailable(err) {
		t.Fatalf("want the storage-unavailable marker, got %v", err)
	}
}

// holdFloorMutex takes the floor mutex (0, 0) on a session of a pool of its
// own and keeps it until cleanup.
func holdFloorMutex(t *testing.T) {
	t.Helper()
	side, err := newCTPool(t, testDBURL(t), 1, nil).Acquire(context.Background())
	if err != nil {
		t.Fatalf("acquire side connection: %v", err)
	}
	if _, err := side.Exec(context.Background(), `SELECT pg_advisory_lock(0, 0)`); err != nil {
		t.Fatalf("take the floor mutex: %v", err)
	}
	t.Cleanup(func() {
		_, _ = side.Exec(context.Background(), `SELECT pg_advisory_unlock(0, 0)`)
		side.Release()
	})
}

// A cyoda_stamp error closes the connection it ran on instead of returning it
// to the pool, on the transaction path and on a non-transactional save. The
// pool has one connection, so a different backend on the next acquire means
// that connection was closed and replaced.
func TestStamp_ErrorClosesConnection(t *testing.T) {
	for _, tc := range []struct {
		name  string
		write func(t *testing.T, f *postgres.StoreFactory, ctx context.Context, id, txID string) error
	}{
		{"transaction", func(t *testing.T, f *postgres.StoreFactory, ctx context.Context, _, _ string) error {
			_, err := commitOneEntityErr(t, f, ctx)
			return err
		}},
		{"non-transactional save", func(t *testing.T, f *postgres.StoreFactory, ctx context.Context, _, _ string) error {
			es, err := f.EntityStore(ctx)
			if err != nil {
				t.Fatalf("EntityStore: %v", err)
			}
			_, err = es.Save(ctx, &spi.Entity{
				Meta: spi.EntityMeta{ID: uuid.NewString(), ModelRef: ctModel}, Data: []byte(`{"n":1}`),
			})
			return err
		}},
		{"non-transactional delete", func(t *testing.T, f *postgres.StoreFactory, ctx context.Context, id, _ string) error {
			es, err := f.EntityStore(ctx)
			if err != nil {
				t.Fatalf("EntityStore: %v", err)
			}
			return es.Delete(ctx, id)
		}},
		{"non-transactional compare-and-save", func(t *testing.T, f *postgres.StoreFactory, ctx context.Context, id, txID string) error {
			es, err := f.EntityStore(ctx)
			if err != nil {
				t.Fatalf("EntityStore: %v", err)
			}
			_, err = es.CompareAndSave(ctx, &spi.Entity{
				Meta: spi.EntityMeta{ID: id, ModelRef: ctModel}, Data: []byte(`{"n":2}`),
			}, txID)
			return err
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := newCTPool(t, testDBURL(t), 1, nil)
			resetSchema(t, pool)
			f := postgres.NewStoreFactory(pool)
			f.InitTransactionManager(newTestUUIDGenerator())
			ctx := ctxWithTenant(ctTenant)
			backend := func() int {
				var pid int
				if err := pool.QueryRow(context.Background(), `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
					t.Fatalf("read backend pid: %v", err)
				}
				return pid
			}

			// An entity committed by a transaction, for the delete and
			// compare-and-save cases to target; seeded before the mutex is held.
			tm := ctTM(t, f, ctx)
			txID, txCtx, err := tm.Begin(ctx)
			if err != nil {
				t.Fatalf("Begin: %v", err)
			}
			seedStore, err := f.EntityStore(txCtx)
			if err != nil {
				t.Fatalf("EntityStore: %v", err)
			}
			id := uuid.NewString()
			if _, err := seedStore.Save(txCtx, &spi.Entity{
				Meta: spi.EntityMeta{ID: id, ModelRef: ctModel}, Data: []byte(`{"n":1}`),
			}); err != nil {
				t.Fatalf("seed Save: %v", err)
			}
			if err := tm.Commit(txCtx, txID); err != nil {
				t.Fatalf("seed Commit: %v", err)
			}

			before := backend()
			holdFloorMutex(t)

			if err := tc.write(t, f, ctx, id, txID); !storageUnavailable(err) {
				t.Fatalf("precondition: want the stamp to fail on the held mutex, got %v", err)
			}
			if after := backend(); after == before {
				t.Fatalf("the connection whose stamp failed (backend %d) went back to the pool", before)
			}
		})
	}
}

// The floor-mutex wait in cyoda_consistency_time is bounded by the budget: with
// the mutex held elsewhere the call fails with 55P03 in about the budget,
// rather than waiting for statement_timeout or the caller.
func TestConsistencyTimeSQL_MutexWaitIsUnderTheBudget(t *testing.T) {
	f, _ := newCTFactory(t)
	conn, err := postgres.PoolForTest(f).Acquire(context.Background())
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer conn.Release()
	holdFloorMutex(t)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	start := time.Now()
	var c time.Time
	err = conn.QueryRow(ctx, `SELECT cyoda_consistency_time($1, 300)`, string(ctTenant)).Scan(&c)
	elapsed := time.Since(start)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "55P03" {
		t.Fatalf("want SQLSTATE 55P03 from the budget, got C=%v err=%v after %v", c, err, elapsed)
	}
	if elapsed > 3*time.Second {
		t.Fatalf("the 300 ms budget ended the mutex wait after %v", elapsed)
	}
}
