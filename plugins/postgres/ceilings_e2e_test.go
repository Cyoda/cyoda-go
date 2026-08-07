package postgres

// ceilings_e2e_test.go — live-server coverage for the connection ceilings.
//
// The unit tests in ceilings_test.go pin the rendered string; only a real server
// can confirm PostgreSQL accepts it. A malformed value in the startup packet
// fails pool.Ping at boot for every deployment, so "the pool opened at all" is
// itself part of what these assert.

import (
	"context"
	"errors"
	"net/url"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

func skipIfNoLiveDB(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("CYODA_TEST_DB_URL")
	if dsn == "" {
		t.Skip("CYODA_TEST_DB_URL not set — skipping PostgreSQL test")
	}
	return dsn
}

// ceilingEnv builds a getenv over CYODA_TEST_DB_URL plus the given overrides.
func ceilingEnv(dsn string, overrides map[string]string) func(string) string {
	return func(k string) string {
		if k == "CYODA_POSTGRES_URL" {
			return dsn
		}
		return overrides[k]
	}
}

// dsnWithParam returns dsn carrying an extra query parameter. pgxpool.ParseConfig
// folds any key it does not recognise into ConnConfig.RuntimeParams, which is the
// channel applyCeiling defers to.
func dsnWithParam(t *testing.T, dsn, key, value string) string {
	t.Helper()
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse CYODA_TEST_DB_URL: %v", err)
	}
	q := u.Query()
	q.Set(key, value)
	u.RawQuery = q.Encode()
	return u.String()
}

// openCeilingPool parses a config from the given env and opens the pool. Pool
// creation includes a Ping, so a value PostgreSQL rejects fails here.
func openCeilingPool(t *testing.T, getenv func(string) string) *pgxpool.Pool {
	t.Helper()
	cfg, err := parseConfig(getenv)
	if err != nil {
		t.Fatalf("parseConfig: %v", err)
	}
	cfg.MaxConns, cfg.MinConns = 2, 0
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	pool, err := newPool(ctx, cfg)
	if err != nil {
		t.Fatalf("newPool: %v; the rendered ceiling was rejected in the startup packet", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// gucMillis reads a timeout GUC as the raw integer count of milliseconds
// pg_settings stores it in, and also returns SHOW's human-readable rendering
// (logged so the observed values are on the record).
func gucMillis(t *testing.T, pool *pgxpool.Pool, name string) int64 {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var raw, shown string
	if err := pool.QueryRow(ctx,
		`SELECT setting, current_setting($1) FROM pg_settings WHERE name = $1`, name).Scan(&raw, &shown); err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	ms, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		t.Fatalf("%s = %q, which is not an integer millisecond count", name, raw)
	}
	t.Logf("%s: pg_settings.setting=%s ms, SHOW=%s", name, raw, shown)
	return ms
}

// TestE2E_Ceilings_DefaultsAppliedOnLiveConnection is the live half of coverage
// row 11a: the values this plugin renders are accepted by a real server and take
// effect as the documented defaults.
func TestE2E_Ceilings_DefaultsAppliedOnLiveConnection(t *testing.T) {
	pool := openCeilingPool(t, ceilingEnv(skipIfNoLiveDB(t), nil))

	if got := gucMillis(t, pool, "statement_timeout"); got != 300000 {
		t.Errorf("statement_timeout = %d ms, want 300000 (5m)", got)
	}
	if got := gucMillis(t, pool, "idle_in_transaction_session_timeout"); got != 300000 {
		t.Errorf("idle_in_transaction_session_timeout = %d ms, want 300000 (5m)", got)
	}
}

// TestE2E_Ceilings_DSNValueSurvives is the live half of coverage row 11h's
// middle case: a value the operator put in CYODA_POSTGRES_URL is not clobbered
// by a default nobody set.
func TestE2E_Ceilings_DSNValueSurvives(t *testing.T) {
	dsn := dsnWithParam(t, skipIfNoLiveDB(t), "statement_timeout", "7000")
	pool := openCeilingPool(t, ceilingEnv(dsn, nil))

	if got := gucMillis(t, pool, "statement_timeout"); got != 7000 {
		t.Errorf("statement_timeout = %d ms, want the DSN's 7000", got)
	}
	// The setting the DSN said nothing about still gets its default.
	if got := gucMillis(t, pool, "idle_in_transaction_session_timeout"); got != 300000 {
		t.Errorf("idle_in_transaction_session_timeout = %d ms, want 300000 (5m)", got)
	}
}

// TestE2E_Ceilings_EnvOverridesDSN is row 11h's third case on a live server.
func TestE2E_Ceilings_EnvOverridesDSN(t *testing.T) {
	dsn := dsnWithParam(t, skipIfNoLiveDB(t), "statement_timeout", "7000")
	pool := openCeilingPool(t, ceilingEnv(dsn, map[string]string{
		"CYODA_POSTGRES_STATEMENT_TIMEOUT": "11s",
	}))

	if got := gucMillis(t, pool, "statement_timeout"); got != 11000 {
		t.Errorf("statement_timeout = %d ms, want the env var's 11000", got)
	}
}

// TestE2E_Ceilings_ZeroDisables — 0 is PostgreSQL's own convention for "no
// limit", and an explicit 0 must reach the server as such rather than being
// treated as "unset, apply the default".
func TestE2E_Ceilings_ZeroDisables(t *testing.T) {
	pool := openCeilingPool(t, ceilingEnv(skipIfNoLiveDB(t), map[string]string{
		"CYODA_POSTGRES_STATEMENT_TIMEOUT":  "0",
		"CYODA_POSTGRES_IDLE_IN_TX_TIMEOUT": "0",
	}))

	if got := gucMillis(t, pool, "statement_timeout"); got != 0 {
		t.Errorf("statement_timeout = %d ms, want 0 (disabled)", got)
	}
	if got := gucMillis(t, pool, "idle_in_transaction_session_timeout"); got != 0 {
		t.Errorf("idle_in_transaction_session_timeout = %d ms, want 0 (disabled)", got)
	}
}

// TestE2E_Ceilings_IdleInTransactionActuallyReclaims proves the setting does the
// job it was added for: a transaction left open with nothing happening is
// terminated by the server, so an abandoned transaction cannot hold a connection
// (and its locks) indefinitely. Without this the ceiling could be a well-formed
// string that never fires.
func TestE2E_Ceilings_IdleInTransactionActuallyReclaims(t *testing.T) {
	pool := openCeilingPool(t, ceilingEnv(skipIfNoLiveDB(t), map[string]string{
		"CYODA_POSTGRES_IDLE_IN_TX_TIMEOUT": "300ms",
	}))

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var one int
	if err := tx.QueryRow(ctx, "SELECT 1").Scan(&one); err != nil {
		t.Fatalf("first statement in tx: %v", err)
	}

	// Sit idle inside the open transaction for well past the ceiling.
	time.Sleep(2 * time.Second)

	err = tx.QueryRow(ctx, "SELECT 1").Scan(&one)
	if err == nil {
		t.Fatal("the transaction survived an idle gap past idle_in_transaction_session_timeout; the ceiling never fired")
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code != "25P03" {
		t.Errorf("SQLSTATE = %s (%s), want 25P03 idle_in_transaction_session_timeout", pgErr.Code, pgErr.Message)
	}
	t.Logf("idle transaction reclaimed as expected: %v", err)
}

// TestE2E_Ceilings_StatementTimeoutActuallyFires — the other GUC, same argument.
func TestE2E_Ceilings_StatementTimeoutActuallyFires(t *testing.T) {
	pool := openCeilingPool(t, ceilingEnv(skipIfNoLiveDB(t), map[string]string{
		"CYODA_POSTGRES_STATEMENT_TIMEOUT": "300ms",
	}))

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	_, err := pool.Exec(ctx, "SELECT pg_sleep(3)")
	if err == nil {
		t.Fatal("a 3s statement completed under a 300ms statement_timeout; the ceiling never fired")
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code != "57014" {
		t.Errorf("SQLSTATE = %s (%s), want 57014 query_canceled", pgErr.Code, pgErr.Message)
	}
	t.Logf("long statement cancelled as expected: %v", err)
}

// --- the async-search scan's own ceiling ------------------------------------
//
// Async search is the one workload whose purpose is to run long, so it carries
// its own statement ceiling instead of sharing the interactive one — a single
// knob would force operators to choose between fast-failing interactive writes
// and long analytical scans. The ceiling is applied with SET LOCAL inside the
// scan's own transaction, so the two scenarios below are each other's mirror:
// whichever of the two ceilings is the small one, only the matching workload
// may die on it.

const (
	searchCeilingTenant = "search-ceiling-tenant"
	searchCeilingModel  = "search-ceiling-model"

	// searchCeilingSeedRows is not a stress figure — it is what gives the scan a
	// cost the two ceilings can be placed either side of. Every async search is a
	// point-in-time read (SubmitAsync stamps one), so the scan reads every seeded
	// version through the bi-temporal DISTINCT ON and takes hundreds of
	// milliseconds. How many hundreds is a property of the machine, which is why
	// the scenario needing a ceiling below the scan measures instead of assuming —
	// see searchCeilingInteractiveFor.
	searchCeilingSeedRows = 50000

	// searchCeilingGenerous is the "not this one" side of each pairing below:
	// large enough that the ceiling under test is unambiguously the one that
	// fired, even on a machine slow enough to take seconds over the seeded scan.
	searchCeilingGenerous = "60s"

	// searchCeilingPreambleFloor is the smallest interactive ceiling these
	// scenarios may use. The async scan's transaction opens with a BEGIN and a
	// set_config binding the tenant, and both run BEFORE the SET LOCAL that
	// raises the ceiling — so the interactive ceiling bounds the scan's own
	// preamble as well as the scan. Measured against a live server with every
	// core saturated: at 1ms about a quarter of those preambles are cancelled,
	// at 25ms roughly one in three hundred, and at 50ms and above none. The
	// floor keeps a margin over that, so a failure in these scenarios is the
	// scan's to have and never the preamble's.
	searchCeilingPreambleFloor = 100 * time.Millisecond
)

// searchCeilingInteractiveFor derives the interactive ceiling the scenario below
// runs under from what the seeded scan actually costs on this machine.
//
// A constant cannot do this job. The value has to sit above the cost of the
// scan's preamble — two trivial round trips, but ones whose cost is a property
// of how loaded the machine is — and below the cost of the scan, which is a
// property of how fast it is. The two are far apart, but not at an address a
// constant can name: a 1ms ceiling sits below the preamble, and the scenario
// then failed in its own setup under load while reporting it as the scan's
// failure.
//
// So: time the scan with both ceilings out of the way and take the faster of two
// runs. The first scan after seeding is the slow one, and calibrating on it
// would leave the ceiling too high for the runs that follow. A quarter of the
// settled figure is the ceiling; if that leaves less than a threefold margin
// under the scan, the seed no longer discriminates on this machine and the
// scenario says so rather than pretending to prove something.
func searchCeilingInteractiveFor(t *testing.T, dsn string) time.Duration {
	t.Helper()
	es, _ := searchCeilingStores(t, dsn, map[string]string{
		"CYODA_POSTGRES_STATEMENT_TIMEOUT":        searchCeilingGenerous,
		"CYODA_POSTGRES_SEARCH_STATEMENT_TIMEOUT": searchCeilingGenerous,
	})

	var scan time.Duration
	for i := 0; i < 2; i++ {
		start := time.Now()
		if _, err := searchCeilingScan(t, es, searchCeilingCtx()); err != nil {
			t.Fatalf("calibration scan: %v", err)
		}
		if d := time.Since(start); i == 0 || d < scan {
			scan = d
		}
	}

	ceiling := max(searchCeilingPreambleFloor, scan/4).Round(time.Millisecond)
	if scan < 3*ceiling {
		t.Fatalf("the seeded scan settles at %v, which leaves no ceiling both clear of the scan's "+
			"preamble (at least %v) and far enough below the scan to stop it; raise "+
			"searchCeilingSeedRows until it does", scan, searchCeilingPreambleFloor)
	}
	t.Logf("calibrated: seeded scan settles at %v, interactive ceiling %v", scan, ceiling)
	return ceiling
}

// searchCeilingCtx is a tenant-carrying context for the scenarios below.
func searchCeilingCtx() context.Context {
	return spi.WithUserContext(context.Background(), &spi.UserContext{
		UserID: "search-ceiling-user",
		Tenant: spi.Tenant{ID: searchCeilingTenant},
	})
}

// seedSearchCeilingModel migrates a clean schema and puts searchCeilingSeedRows
// entity versions behind one model. It runs on a plain pool so neither the
// migration nor the seeding is subject to the ceilings a scenario configures.
//
// The versions are copies of one real Save, so the stored document is exactly
// the shape the scanner expects rather than hand-written JSON that could drift.
func seedSearchCeilingModel(t *testing.T, dsn string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("open seeding pool: %v", err)
	}
	// Registered BEFORE the drop below so LIFO runs the drop first and closes
	// the pool second. A defer here would instead close the pool when this
	// helper returns, leaving the drop to run against a closed one — and its
	// error is discarded, so the seeded rows would silently accumulate.
	t.Cleanup(pool.Close)

	if err := dropSchema(pool); err != nil {
		t.Fatalf("reset schema: %v", err)
	}
	if err := Migrate(pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	t.Cleanup(func() {
		if err := dropSchema(pool); err != nil {
			t.Errorf("drop seeded schema: %v", err)
		}
	})

	store, err := newStoreFactoryWithConfig(pool, defaultStoreConfig()).EntityStore(searchCeilingCtx())
	if err != nil {
		t.Fatalf("EntityStore: %v", err)
	}
	if _, err := store.Save(searchCeilingCtx(), &spi.Entity{
		Meta: spi.EntityMeta{
			ID:       "seed",
			ModelRef: spi.ModelRef{EntityName: searchCeilingModel, ModelVersion: "1"},
			State:    "NEW",
		},
		Data: []byte(`{"name":"Alice","city":"Berlin"}`),
	}); err != nil {
		t.Fatalf("seed Save: %v", err)
	}

	if _, err := pool.Exec(ctx, `
		INSERT INTO entity_versions (tenant_id, entity_id, model_name, model_version, version,
		                             valid_time, transaction_time, wall_clock_time, doc)
		SELECT tenant_id, entity_id || '-' || g, model_name, model_version, version,
		       valid_time, transaction_time, wall_clock_time,
		       jsonb_set(doc, '{_meta,id}', to_jsonb(entity_id || '-' || g))
		FROM entity_versions, generate_series(1, $1) AS g
		WHERE tenant_id = $2 AND entity_id = 'seed'`,
		searchCeilingSeedRows, searchCeilingTenant); err != nil {
		t.Fatalf("seed %d versions: %v", searchCeilingSeedRows, err)
	}
}

// searchCeilingStores opens a pool under the given ceiling overrides and returns
// the entity store the scan runs through plus the async-search store that marks
// a context as belonging to that scan.
func searchCeilingStores(t *testing.T, dsn string, overrides map[string]string) (spi.EntityStore, spi.AsyncSearchStore) {
	t.Helper()
	getenv := ceilingEnv(dsn, overrides)
	cfg, err := parseConfig(getenv)
	if err != nil {
		t.Fatalf("parseConfig: %v", err)
	}
	pool := openCeilingPool(t, getenv)
	f := newStoreFactoryWithConfig(pool, cfg)
	es, err := f.EntityStore(searchCeilingCtx())
	if err != nil {
		t.Fatalf("EntityStore: %v", err)
	}
	ass, err := f.AsyncSearchStore(searchCeilingCtx())
	if err != nil {
		t.Fatalf("AsyncSearchStore: %v", err)
	}
	return es, ass
}

// searchCeilingScan runs the scan an async search job runs: a point-in-time read
// over the seeded model, matching everything.
func searchCeilingScan(t *testing.T, es spi.EntityStore, ctx context.Context) ([]*spi.Entity, error) {
	t.Helper()
	now := time.Now()
	return es.(spi.Searcher).Search(ctx, spi.Filter{}, spi.SearchOptions{
		ModelName:    searchCeilingModel,
		ModelVersion: "1",
		PointInTime:  &now,
	})
}

// asyncScanMarker is the opt-in the domain uses: an AsyncSearchStore whose
// backend bounds the async scan separately hands back a context the scan
// recognises.
type asyncScanMarker interface {
	AsyncScanContext(ctx context.Context) context.Context
}

// TestE2E_SearchCeiling_BoundsTheScanTheInteractiveCeilingWouldNot is the half
// that matters most: with the interactive ceiling set below what the scan needs,
// the async scan still completes because SET LOCAL raised it to the search
// ceiling for that transaction alone. Without the raise, the scan would die on
// the ceiling the pool carries — which the unmarked control below proves it does.
func TestE2E_SearchCeiling_BoundsTheScanTheInteractiveCeilingWouldNot(t *testing.T) {
	dsn := skipIfNoLiveDB(t)
	seedSearchCeilingModel(t, dsn)

	interactive := searchCeilingInteractiveFor(t, dsn)
	es, ass := searchCeilingStores(t, dsn, map[string]string{
		"CYODA_POSTGRES_STATEMENT_TIMEOUT":        interactive.String(),
		"CYODA_POSTGRES_SEARCH_STATEMENT_TIMEOUT": searchCeilingGenerous,
	})
	marker, ok := ass.(asyncScanMarker)
	if !ok {
		t.Fatal("the postgres AsyncSearchStore does not mark a context as an async scan")
	}

	got, err := searchCeilingScan(t, es, marker.AsyncScanContext(searchCeilingCtx()))
	if err != nil {
		t.Fatalf("the async scan died under the interactive ceiling: %v", err)
	}
	if len(got) != searchCeilingSeedRows+1 {
		t.Fatalf("scan returned %d entities, want %d", len(got), searchCeilingSeedRows+1)
	}

	// The control. The same scan on an unmarked context gets the pool's
	// interactive ceiling, so it is cancelled — which is what makes the pass
	// above evidence of the raise rather than of a fast machine.
	if _, err := searchCeilingScan(t, es, searchCeilingCtx()); err == nil {
		t.Fatalf("an unmarked scan completed under a %v interactive ceiling; the scenario above proves nothing", interactive)
	} else if !isStatementTimeout(err) {
		t.Fatalf("unmarked scan failed with %v, want the interactive ceiling's cancellation", err)
	}
}

// TestE2E_SearchCeiling_FiresOnTheScanAndNowhereElse is the mirror: the small
// ceiling is now the search one, so the scan dies on it while the pool's own
// statements — the interactive path — are untouched. SET LOCAL is what keeps the
// two apart; a plain SET would have poisoned the connection for everything that
// borrowed it next.
func TestE2E_SearchCeiling_FiresOnTheScanAndNowhereElse(t *testing.T) {
	dsn := skipIfNoLiveDB(t)
	seedSearchCeilingModel(t, dsn)

	// The 1ms is safe on this side: the scan's preamble runs under the generous
	// interactive ceiling, and the search ceiling only takes effect from the
	// SET LOCAL onwards — so the scan itself is the first statement it bounds.
	es, ass := searchCeilingStores(t, dsn, map[string]string{
		"CYODA_POSTGRES_STATEMENT_TIMEOUT":        searchCeilingGenerous,
		"CYODA_POSTGRES_SEARCH_STATEMENT_TIMEOUT": "1ms",
	})
	marker, ok := ass.(asyncScanMarker)
	if !ok {
		t.Fatal("the postgres AsyncSearchStore does not mark a context as an async scan")
	}

	_, err := searchCeilingScan(t, es, marker.AsyncScanContext(searchCeilingCtx()))
	if err == nil {
		t.Fatal("the async scan completed under a 1ms search ceiling; nothing bounded it")
	}
	var exceeded interface{ SearchCeilingExceeded() bool }
	if !errors.As(err, &exceeded) || !exceeded.SearchCeilingExceeded() {
		t.Fatalf("scan failed with %v, which the domain cannot recognise as the search ceiling firing", err)
	}

	// The interactive path is unaffected: the same store, on an unmarked
	// context, runs the same scan to completion under the pool's generous ceiling.
	got, err := searchCeilingScan(t, es, searchCeilingCtx())
	if err != nil {
		t.Fatalf("the search ceiling leaked onto the interactive path: %v", err)
	}
	if len(got) != searchCeilingSeedRows+1 {
		t.Fatalf("interactive scan returned %d entities, want %d", len(got), searchCeilingSeedRows+1)
	}
}
