# Stream BP — PostgreSQL: migration, scheduler pool, the §10.2 statements

Spec sections: §10.1, §10.2, §11 (`CYODA_POSTGRES_SCHEDULER_CONNS`), §16 V5.
§13 rows this stream makes pass on PostgreSQL through the `spitest`
ScheduledTasks suite (stream S), plus the PG-specific rows listed in the
coverage table at the end.

## Facts settled by reading the code (used by the tasks below)

1. **Task rows already join the entity transaction.** `scheduledTaskStore`
   holds a `ctxQuerier` (`plugins/postgres/scheduled_task_store.go:12-23`,
   `store_factory.go:162-164`), which resolves the `pgx.Tx` on `ctx` for every
   call (`store_factory.go:144-158`). The entity transaction is
   `REPEATABLE READ` (`transaction_manager.go:129`), and its snapshot is taken
   by the `set_config` statement inside `Begin` (`transaction_manager.go:137`).
   So C1, C2 and C6 follow from PostgreSQL itself: a write to a task row that
   another transaction committed after the snapshot raises 40001; a row this
   transaction wrote stays locked until it ends.
2. **40001 and 40P01 already map to `spi.ErrConflict` at statement level.**
   `ctxQuerier` classifies every statement (`classifying_querier.go:19-24,
   39-55`) through `classifySQLState` (`transaction_manager.go:918-944`, the
   40001/40P01 branch at `:925-926`). Nothing maps 55P03 today, and nothing
   marks SQLSTATE classes 22/23/42.
3. **There is one pool.** `newPool` (`config.go:180-207`) builds it; its
   ceilings travel in the startup packet (`config.go:192-196`). The only
   never-joining querier is `unjoinedQuerier` (`unjoined_querier.go:69-142`),
   which bounds the acquire **only** inside a transaction. The async-search
   heartbeat and claim use it through `poolQuerier`
   (`store_factory.go:176-178`, `search_store.go:207-225, 559-608`), so on a
   saturated main pool they block until the caller's context ends.
4. **`NewStoreFactory(nil)` is legal in tests** and `AsyncSearchStore()` must
   still succeed on it (`store_factory_test.go:125-137`). So the scheduler
   pools are opened lazily, on first use, from the main pool's own config
   (`pgxpool.Pool.Config()` returns a deep copy, RuntimeParams included).
   `Plugin.NewFactory` opens them eagerly, so a deployment that cannot connect
   them fails at startup.
5. **Test schema resets kill stray connections.** `dropSchema` terminates every
   other backend of the database (`migrate.go:372-375`). A test factory whose
   scheduler pools are not closed therefore leaks goroutines, not live
   sessions. The fixtures below close them anyway.
6. **The index guard.** `TestMigrations_IndexesOnExistingTablesAreConcurrent`
   (`migration_index_guard_test.go:32-133`) fails any plain `CREATE INDEX` on a
   table made by an earlier migration unless the file is grandfathered.
   `scheduled_tasks` comes from 000004, and the new migration has many
   statements, so `CONCURRENTLY` cannot run in it (clause (b)). The file is
   grandfathered with its own lock profile, as 000011–000013 were.
7. **The `(tenant_id, entity_id)` index already exists** as
   `scheduled_tasks_entity_idx` (`000004_scheduled_tasks.up.sql:34-35`). It is
   kept, not recreated. The `(tenant_id, scheduled_time, id)` index is built on
   `id COLLATE "C"`, so the query order and the cursor compare task ids
   byte-wise, as Go does in memory and SQLite.
8. **The 000004 comment is wrong and immutable.** It says every write carries a
   tenant predicate (`000004_scheduled_tasks.up.sql:11-14`), but `Delete`
   matched on id alone (`scheduled_task_store.go:68-74`). Applied migrations
   are not edited. The correction goes into the header of the new migration,
   the `scheduledTaskStore` godoc and `docs/plugins/POSTGRES.md` (BP-6).
9. **The plugin is its own module.** Until stream S's SPI branch is on the
   worktree's `go.work` (uncommitted `use` line), the plugin builds against the
   pinned SPI. BP-1 to BP-3 do not touch the SPI surface and run with
   `GOWORK=off`. From BP-4 on, the package only compiles against S's SPI, so
   those tasks run with the workspace.
10. **Tests start PostgreSQL themselves.** `TestMain` starts
    `postgres:17-alpine` through testcontainers unless `CYODA_TEST_DB_URL` is
    set (`main_test.go:29-71`). `TestConformance` runs every `spitest` suite
    (`conformance_test.go:155-206`); S registers `runScheduledTasksSuite` there, so
    its cases run as `TestConformance/ScheduledTasks/...`. The PG harness
    sleeps at most 100 ms per `AdvanceClock` (`conformance_test.go:181-183`).

## Working rules for this stream

- Every command `cd`s explicitly. `ROOT` below is
  `/Users/paul/go-projects/cyoda-light/cyoda-go/.worktrees/598-scheduler-ownership`.
- Run `make preflight` from `ROOT` once before the first test run. Docker is
  required.
- Never `git add -A`. From BP-4 on, `git status --short go.work` must show
  ` M go.work` (unstaged) before every commit, and
  `git diff --cached --name-only` must not list it.
- Never log or put a claim token, arm token or owner id in an error message.
  Task ids are hashes and may appear.

---

### Task BP-1: `CYODA_POSTGRES_SCHEDULER_CONNS`

**Spec:** §10.2 "Scheduler pool (C4)"; §11 row `CYODA_POSTGRES_SCHEDULER_CONNS`
(default 10, ≥ 2, startup fails on an invalid value).

**Files:**
- Modify: `plugins/postgres/config.go` (`config` struct `:17-44`, defaults
  `:50-56`, `parseConfig` `:61-96`, `DBConfig.toInternal` `:222-244`)
- Modify: `plugins/postgres/store_factory.go` (`defaultStoreConfig` `:54-69`)
- Modify: `plugins/postgres/plugin.go` (`ConfigVars` `:17-31`)
- Modify: `plugins/postgres/config_defaults_test.go` (map at `:27-38`)
- Test: `plugins/postgres/config_scheduler_conns_test.go` (new)
- Docs: `plugins/postgres/doc.go` (`:12-16`),
  `cmd/cyoda/help/content/config/database.md` (after `:63`),
  `docs/plugins/POSTGRES.md` (config table, after the `CYODA_POSTGRES_AUTO_MIGRATE` row at `:320`)

**Interfaces:**
- Consumes: nothing.
- Produces: `config.SchedulerConns int32` (binding name, `interfaces.md:255`);
  `const defaultSchedulerConns int32 = 10`; `func envSchedulerConns(getenv func(string) string) (int32, error)`.

- [ ] **Step 1: Write the failing tests**

`plugins/postgres/config_scheduler_conns_test.go`:

```go
package postgres

import (
	"strings"
	"testing"
)

// schedulerConnsEnv is a getenv with the required URL and one value for
// CYODA_POSTGRES_SCHEDULER_CONNS ("" = unset).
func schedulerConnsEnv(v string) func(string) string {
	return func(k string) string {
		switch k {
		case "CYODA_POSTGRES_URL":
			return "postgres://test"
		case "CYODA_POSTGRES_SCHEDULER_CONNS":
			return v
		}
		return ""
	}
}

func TestParseConfig_SchedulerConns(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want int32
	}{
		{"", 10},
		{"2", 2},
		{"32", 32},
	} {
		cfg, err := parseConfig(schedulerConnsEnv(tc.in))
		if err != nil {
			t.Fatalf("CYODA_POSTGRES_SCHEDULER_CONNS=%q: %v", tc.in, err)
		}
		if cfg.SchedulerConns != tc.want {
			t.Errorf("CYODA_POSTGRES_SCHEDULER_CONNS=%q: SchedulerConns = %d, want %d", tc.in, cfg.SchedulerConns, tc.want)
		}
	}
}

// An invalid value fails startup; it never falls back to the default.
func TestParseConfig_SchedulerConns_InvalidFailsStartup(t *testing.T) {
	for _, in := range []string{"1", "0", "-4", "ten", "2.5", "2147483648"} {
		_, err := parseConfig(schedulerConnsEnv(in))
		if err == nil {
			t.Errorf("CYODA_POSTGRES_SCHEDULER_CONNS=%q was accepted", in)
			continue
		}
		if !strings.Contains(err.Error(), "CYODA_POSTGRES_SCHEDULER_CONNS") {
			t.Errorf("CYODA_POSTGRES_SCHEDULER_CONNS=%q: error %q does not name the variable", in, err)
		}
	}
}

// The fixture configs connect the way a deployment does.
func TestSchedulerConns_FixtureConfigsCarryTheDefault(t *testing.T) {
	if got := defaultStoreConfig().SchedulerConns; got != 10 {
		t.Errorf("defaultStoreConfig().SchedulerConns = %d, want 10", got)
	}
	if got := (DBConfig{URL: "postgres://test"}).toInternal().SchedulerConns; got != 10 {
		t.Errorf("DBConfig.toInternal().SchedulerConns = %d, want 10", got)
	}
}
```

In `plugins/postgres/config_defaults_test.go`, add one entry to the `actual`
map of `TestConfigVars_DefaultsMatchParseConfig` (after the
`CYODA_POSTGRES_SEARCH_STATEMENT_TIMEOUT` line, `:37`):

```go
		"CYODA_POSTGRES_SCHEDULER_CONNS":          strconv.Itoa(int(cfg.SchedulerConns)),
```

- [ ] **Step 2: Run the tests to verify they fail**

```bash
cd /Users/paul/go-projects/cyoda-light/cyoda-go/.worktrees/598-scheduler-ownership/plugins/postgres && \
GOWORK=off go test -run 'TestParseConfig_SchedulerConns|TestSchedulerConns_|TestConfigVars_DefaultsMatchParseConfig' .
```

Expected: build failure, `cfg.SchedulerConns undefined (type config has no field or method SchedulerConns)`.

- [ ] **Step 3: Implement**

In `plugins/postgres/config.go`, add to the `config` struct after
`SearchStatementTimeout` (`:43`):

```go
	// SchedulerConns is the size of the scheduler's own pool (scheduler_pool.go):
	// claims, run bookkeeping and the async-search heartbeat and claim.
	// Heartbeat has one more connection of its own on top of it.
	SchedulerConns int32
```

Add after the ceiling-default block (`:50-56`):

```go
// defaultSchedulerConns sizes the scheduler pool for the default
// CYODA_SCHEDULER_MAX_RUNS (8) plus the claim loop and the async-search
// heartbeat and claim.
const defaultSchedulerConns int32 = 10

// envSchedulerConns reads CYODA_POSTGRES_SCHEDULER_CONNS. Unlike envInt32, a
// malformed or too-small value is an error: a scheduler pool the operator did
// not ask for is not a safe fallback. Two is the floor — one connection for
// the claim loop and one for a run's bookkeeping.
func envSchedulerConns(getenv func(string) string) (int32, error) {
	const key = "CYODA_POSTGRES_SCHEDULER_CONNS"
	v := getenv(key)
	if v == "" {
		return defaultSchedulerConns, nil
	}
	n, err := strconv.ParseInt(v, 10, 32)
	if err != nil {
		return 0, fmt.Errorf("%s=%q is not a valid connection count: %w", key, v, err)
	}
	if n < 2 {
		return 0, fmt.Errorf("%s=%q must be at least 2", key, v)
	}
	return int32(n), nil
}
```

In `parseConfig`, before the final `return cfg, nil` (`:95`):

```go
	if cfg.SchedulerConns, err = envSchedulerConns(getenv); err != nil {
		return config{}, err
	}
```

In `DBConfig.toInternal`, add to the returned literal after
`SearchStatementTimeout: defaultSearchStatementTimeout,` (`:242`):

```go
		SchedulerConns:         defaultSchedulerConns,
```

In `plugins/postgres/store_factory.go` `defaultStoreConfig`, add after
`SearchStatementTimeout: defaultSearchStatementTimeout,` (`:67`):

```go
		SchedulerConns:         defaultSchedulerConns,
```

In `plugins/postgres/plugin.go` `ConfigVars`, add after the
`CYODA_POSTGRES_SEARCH_STATEMENT_TIMEOUT` entry (`:28`):

```go
		{Name: "CYODA_POSTGRES_SCHEDULER_CONNS", Description: "Connections in the scheduler's own pool (claims, run bookkeeping, async-search heartbeats); at least 2. The scheduler heartbeat has one more of its own", Default: "10"},
```

- [ ] **Step 4: Run the tests to verify they pass**

```bash
cd /Users/paul/go-projects/cyoda-light/cyoda-go/.worktrees/598-scheduler-ownership/plugins/postgres && \
GOWORK=off go test -run 'TestParseConfig_|TestSchedulerConns_|TestConfigVars_|TestDefaultStoreConfig_|TestDBConfigToInternal_' .
```

Expected: `ok`.

- [ ] **Step 5: Documentation (Gate 4)**

`plugins/postgres/doc.go`, after the `CYODA_POSTGRES_AUTO_MIGRATE` line (`:16`):

```go
//	CYODA_POSTGRES_SCHEDULER_CONNS    default 10    (scheduler's own pool; at least 2)
```

`cmd/cyoda/help/content/config/database.md`, after the
`CYODA_POSTGRES_AUTO_MIGRATE` bullet (`:63`):

```markdown
- `CYODA_POSTGRES_SCHEDULER_CONNS` — connections in the scheduler's own pool, kept apart from the main pool so that entity transactions cannot starve scheduled-task claims, run bookkeeping, or the async-search heartbeat and claim. The scheduler heartbeat opens one more connection of its own. At least `2`; a smaller or malformed value fails startup (default: `10`)
```

`docs/plugins/POSTGRES.md`, config table, after the
`CYODA_POSTGRES_AUTO_MIGRATE` row:

```markdown
| `CYODA_POSTGRES_SCHEDULER_CONNS` | `10` | Size of the scheduler pool (see "Scheduled tasks and the scheduler pool"). At least `2`; an invalid value fails startup. The scheduler heartbeat has one more connection of its own. |
```

Run the root coverage guard. It scans source files, so `GOWORK=off` (the
pinned plugin build) is enough:

```bash
cd /Users/paul/go-projects/cyoda-light/cyoda-go/.worktrees/598-scheduler-ownership && \
GOWORK=off go test -run 'TestConfig_EnvVarCoverage' ./cmd/cyoda/help/
```

Expected: `ok`. `TestConfigAll_Complete` reads the *compiled* plugin's
`ConfigVars()`. Under `GOWORK=off` that is the pinned plugin, so it can only
pass once the root builds against the workspace. It runs in `make test` at
the merge of this stream.

- [ ] **Step 6: Commit**

```bash
cd /Users/paul/go-projects/cyoda-light/cyoda-go/.worktrees/598-scheduler-ownership && \
git add plugins/postgres/config.go plugins/postgres/store_factory.go plugins/postgres/plugin.go \
  plugins/postgres/config_defaults_test.go plugins/postgres/config_scheduler_conns_test.go \
  plugins/postgres/doc.go cmd/cyoda/help/content/config/database.md docs/plugins/POSTGRES.md && \
git commit -m "feat(postgres): CYODA_POSTGRES_SCHEDULER_CONNS sizes the scheduler pool

Default 10, at least 2; a malformed or smaller value fails startup.

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task BP-2: The scheduler pool and its heartbeat connection

**Spec:** §10.2 "Scheduler pool (C4)": READ COMMITTED, `statement_timeout`
30 s, `idle_in_transaction_session_timeout` 10 s, `lock_timeout` 2 s, 5 s
acquire timeout, `Heartbeat` on one extra connection. §13 row "a
scheduler-pool statement blocked on a task-row lock gives up after
`lock_timeout`" (the pool half; BP-4 has the store half).

**Files:**
- Create: `plugins/postgres/scheduler_pool.go`
- Modify: `plugins/postgres/store_factory.go` (`StoreFactory` struct `:13-24`, `Close` `:279-285`)
- Modify: `plugins/postgres/plugin.go` (`NewFactory` `:53-61`)
- Modify: `plugins/postgres/export_test.go` (append)
- Modify: `plugins/postgres/conformance_test.go` (`newConformancePool`, after the pool-close cleanup at `:104-117`)
- Modify: `plugins/postgres/metrics.go` (`registerPoolMetrics` `:15-82`) — Steps 6-10, the `pool` attribute (README C-R6)
- Test: `plugins/postgres/scheduler_pool_test.go` (new, package `postgres`)
- Test: `plugins/postgres/metrics_test.go` (`TestRegisterPoolMetrics_ReportsPoolStat` `:23-86`), `internal/e2e/pool_metrics_test.go` (`:25-34`) — Steps 6-10

**Interfaces:**
- Consumes: `config.SchedulerConns` (BP-1); `pgDurationMillis`, `newAcquireContext`, `classifyAcquireErr` (`ceilings.go:27-29, 100-105, 143-148`); `releasingRows`, `releasingRow`, `acquireFailedRow` (`unjoined_querier.go:152-192`).
- Produces (package-internal, used by BP-3 and BP-4):
  ```go
  type schedulerQuerier struct{ factory *StoreFactory; heartbeat bool; what string } // implements Querier; never joins
  func (q schedulerQuerier) begin(ctx context.Context) (pgx.Tx, error)             // READ COMMITTED, bounded acquire
  func (f *StoreFactory) schedulerQuerier(what string) schedulerQuerier
  func (f *StoreFactory) heartbeatQuerier() schedulerQuerier
  func (f *StoreFactory) schedulerPools() (work, heartbeat *pgxpool.Pool, err error)
  func (f *StoreFactory) closeSchedulerPools()
  ```
  Test exports: `SchedulerPoolForTest(t testing.TB, f *StoreFactory) *pgxpool.Pool`,
  `CloseSchedulerPoolsForTest(f *StoreFactory)`.
- Produces (Steps 6-10): `func registerPoolMetrics(meter metric.Meter, pool *pgxpool.Pool, sched *schedulerPools) (func(), error)`,
  `func (p *schedulerPools) snapshot() (work, heartbeat *pgxpool.Pool)`; test
  export `RegisterPoolMetricsForTest(meter metric.Meter, f *StoreFactory) (func(), error)`.
  The gauge `cyoda.storage.pool.connections` carries `pool` = `main`,
  `scheduler` or `heartbeat`; the other pool instruments stay the main pool's,
  unlabelled by pool.

- [ ] **Step 1: Write the failing tests**

`plugins/postgres/scheduler_pool_test.go` (reuses `openCeilingPool`,
`ceilingEnv`, `dsnWithParam`, `gucMillis` and `testDBURL` from
`ceilings_e2e_test.go:28-99`):

```go
package postgres

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgerrcode"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// newSchedulerTestFactory builds a factory over a small main pool and closes
// its scheduler pools when the test ends.
func newSchedulerTestFactory(t *testing.T, getenv func(string) string) *StoreFactory {
	t.Helper()
	f := newStoreFactoryWithConfig(openCeilingPool(t, getenv), defaultStoreConfig())
	t.Cleanup(f.closeSchedulerPools)
	return f
}

// The scheduler's ceilings are fixed. A statement_timeout in the DSN reaches
// the main pool (applyCeiling) but never the scheduler's pools.
func TestSchedulerPools_SessionCeilings(t *testing.T) {
	dsn := dsnWithParam(t, testDBURL(t), "statement_timeout", "7000")
	f := newSchedulerTestFactory(t, ceilingEnv(dsn, nil))
	work, heartbeat, err := f.schedulerPools()
	if err != nil {
		t.Fatalf("schedulerPools: %v", err)
	}
	if got := work.Config().MaxConns; got != 10 {
		t.Errorf("work pool MaxConns = %d, want 10 (CYODA_POSTGRES_SCHEDULER_CONNS default)", got)
	}
	if got := heartbeat.Config().MaxConns; got != 1 {
		t.Errorf("heartbeat pool MaxConns = %d, want 1", got)
	}
	for name, p := range map[string]*pgxpool.Pool{"work": work, "heartbeat": heartbeat} {
		t.Run(name, func(t *testing.T) {
			if got := gucMillis(t, p, "statement_timeout"); got != 30000 {
				t.Errorf("statement_timeout = %d ms, want 30000", got)
			}
			if got := gucMillis(t, p, "idle_in_transaction_session_timeout"); got != 10000 {
				t.Errorf("idle_in_transaction_session_timeout = %d ms, want 10000", got)
			}
			if got := gucMillis(t, p, "lock_timeout"); got != 2000 {
				t.Errorf("lock_timeout = %d ms, want 2000", got)
			}
			var iso string
			if err := p.QueryRow(context.Background(),
				`SELECT current_setting('default_transaction_isolation')`).Scan(&iso); err != nil {
				t.Fatalf("read default_transaction_isolation: %v", err)
			}
			if iso != "read committed" {
				t.Errorf("default_transaction_isolation = %q, want %q", iso, "read committed")
			}
		})
	}
}

// A statement on the scheduler pool that waits on a row lock gives up after
// lock_timeout with 55P03, which callers retry.
func TestSchedulerPools_LockWaitEndsAtLockTimeout(t *testing.T) {
	f := newSchedulerTestFactory(t, ceilingEnv(testDBURL(t), nil))
	ctx := context.Background()
	if _, err := f.pool.Exec(ctx,
		`CREATE TABLE IF NOT EXISTS sched_lock_probe (id int PRIMARY KEY);
		 INSERT INTO sched_lock_probe VALUES (1) ON CONFLICT DO NOTHING`); err != nil {
		t.Fatalf("create probe table: %v", err)
	}
	t.Cleanup(func() { _, _ = f.pool.Exec(context.Background(), `DROP TABLE IF EXISTS sched_lock_probe`) })

	holder, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin holder: %v", err)
	}
	defer func() { _ = holder.Rollback(context.Background()) }()
	if _, err := holder.Exec(ctx, `UPDATE sched_lock_probe SET id = id WHERE id = 1`); err != nil {
		t.Fatalf("holder lock: %v", err)
	}

	start := time.Now()
	_, err = f.schedulerQuerier("lock probe").Exec(ctx, `UPDATE sched_lock_probe SET id = id WHERE id = 1`)
	elapsed := time.Since(start)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != pgerrcode.LockNotAvailable {
		t.Fatalf("err = %v, want SQLSTATE 55P03 lock_not_available", err)
	}
	if elapsed < 1500*time.Millisecond || elapsed > 10*time.Second {
		t.Errorf("gave up after %s, want about the 2s lock_timeout", elapsed)
	}
}

// The acquire is bounded at 5s even outside any transaction, and a timeout
// carries the storage-unavailable marker.
func TestSchedulerPools_AcquireIsBounded(t *testing.T) {
	f := newSchedulerTestFactory(t, ceilingEnv(testDBURL(t), nil))
	work, _, err := f.schedulerPools()
	if err != nil {
		t.Fatalf("schedulerPools: %v", err)
	}
	ctx := context.Background()
	for i := int32(0); i < work.Config().MaxConns; i++ {
		c, err := work.Acquire(ctx)
		if err != nil {
			t.Fatalf("hold connection %d: %v", i, err)
		}
		t.Cleanup(c.Release) // runs before closeSchedulerPools (LIFO)
	}

	start := time.Now()
	_, err = f.schedulerQuerier("acquire probe").Exec(ctx, `SELECT 1`)
	elapsed := time.Since(start)
	var su interface{ StorageUnavailable() bool }
	if !errors.As(err, &su) || !su.StorageUnavailable() {
		t.Fatalf("err = %v, want the storage-unavailable acquire-timeout marker", err)
	}
	if elapsed < 4*time.Second || elapsed > 9*time.Second {
		t.Errorf("acquire gave up after %s, want about 5s", elapsed)
	}
}

// Plugin.NewFactory opens and pings both scheduler pools; Close closes them.
func TestNewFactory_OpensTheSchedulerPools(t *testing.T) {
	dsn := testDBURL(t)
	reset := openCeilingPool(t, ceilingEnv(dsn, nil))
	if err := dropSchema(reset); err != nil {
		t.Fatalf("reset schema: %v", err)
	}
	t.Cleanup(func() { _ = dropSchema(reset) })

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	f, err := (&plugin{}).NewFactory(ctx, ceilingEnv(dsn, map[string]string{"CYODA_POSTGRES_MIN_CONNS": "0"}))
	if err != nil {
		t.Fatalf("NewFactory: %v", err)
	}
	sf := f.(*StoreFactory)
	if sf.sched.work == nil || sf.sched.heartbeat == nil {
		t.Fatal("NewFactory did not open the scheduler pools")
	}
	if sf.sched.work.Stat().TotalConns() < 1 || sf.sched.heartbeat.Stat().TotalConns() < 1 {
		t.Error("NewFactory opened the scheduler pools without connecting them")
	}
	if err := f.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if sf.sched.work != nil || sf.sched.heartbeat != nil {
		t.Error("Close left the scheduler pools open")
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

```bash
cd /Users/paul/go-projects/cyoda-light/cyoda-go/.worktrees/598-scheduler-ownership/plugins/postgres && \
GOWORK=off go test -run 'TestSchedulerPools_|TestNewFactory_OpensTheSchedulerPools' .
```

Expected: build failure, `f.schedulerPools undefined`, `f.closeSchedulerPools undefined`, `sf.sched undefined`.

- [ ] **Step 3: Implement**

`plugins/postgres/scheduler_pool.go`:

```go
package postgres

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// The scheduler pool's session ceilings. They are fixed, not operator
// settings: the scheduler's timing rules (the STALE_AFTER validation, the
// watchdog margin) are derived from them.
const (
	schedulerStatementTimeout = 30 * time.Second
	schedulerIdleInTxTimeout  = 10 * time.Second
	schedulerLockTimeout      = 2 * time.Second
	schedulerAcquireTimeout   = 5 * time.Second
)

// schedulerPools are the scheduler's own connections, kept apart from the main
// pool so that entity transactions, however many are open, cannot starve a
// claim, a heartbeat or a run's bookkeeping (C4).
//
// work carries every never-joining ScheduledTaskStore method except Query and
// Heartbeat, and the async-search heartbeat and claim. heartbeat is one
// connection used only by ScheduledTaskStore.Heartbeat, so a busy work pool
// cannot delay the liveness record either.
//
// Both are derived from the main pool's configuration and opened on first use,
// so a test factory that never schedules anything opens nothing, and a factory
// built without a pool fails only when asked for one. Plugin.NewFactory opens
// them eagerly, so a deployment that cannot connect them fails at startup.
type schedulerPools struct {
	mu        sync.Mutex
	work      *pgxpool.Pool
	heartbeat *pgxpool.Pool
}

// schedulerPoolConfig derives a scheduler pool from the main pool's config.
// The ceilings overwrite any value from the DSN: they are the scheduler's
// contract, not the operator's.
func schedulerPoolConfig(base *pgxpool.Config, maxConns int32) *pgxpool.Config {
	c := base.Copy()
	c.MaxConns = maxConns
	c.MinConns = 0
	c.MinIdleConns = 0
	if c.ConnConfig.RuntimeParams == nil {
		c.ConnConfig.RuntimeParams = map[string]string{}
	}
	p := c.ConnConfig.RuntimeParams
	p["statement_timeout"] = pgDurationMillis(schedulerStatementTimeout)
	p["idle_in_transaction_session_timeout"] = pgDurationMillis(schedulerIdleInTxTimeout)
	p["lock_timeout"] = pgDurationMillis(schedulerLockTimeout)
	p["default_transaction_isolation"] = "read committed"
	return c
}

// schedulerPools returns the two scheduler pools, creating them on first use.
func (f *StoreFactory) schedulerPools() (work, heartbeat *pgxpool.Pool, err error) {
	f.sched.mu.Lock()
	defer f.sched.mu.Unlock()
	if f.sched.work != nil {
		return f.sched.work, f.sched.heartbeat, nil
	}
	if f.pool == nil {
		return nil, nil, errors.New("scheduler pool: the store factory has no connection pool")
	}
	base := f.pool.Config()
	work, err = pgxpool.NewWithConfig(context.Background(), schedulerPoolConfig(base, f.cfg.SchedulerConns))
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create the scheduler pool: %w", err)
	}
	heartbeat, err = pgxpool.NewWithConfig(context.Background(), schedulerPoolConfig(base, 1))
	if err != nil {
		work.Close()
		return nil, nil, fmt.Errorf("failed to create the scheduler heartbeat pool: %w", err)
	}
	f.sched.work, f.sched.heartbeat = work, heartbeat
	return work, heartbeat, nil
}

// openSchedulerPools creates both pools and connects one session in each.
func (f *StoreFactory) openSchedulerPools(ctx context.Context) error {
	work, heartbeat, err := f.schedulerPools()
	if err != nil {
		return err
	}
	if err := work.Ping(ctx); err != nil {
		f.closeSchedulerPools()
		return fmt.Errorf("failed to connect the scheduler pool: %w", err)
	}
	if err := heartbeat.Ping(ctx); err != nil {
		f.closeSchedulerPools()
		return fmt.Errorf("failed to connect the scheduler heartbeat pool: %w", err)
	}
	return nil
}

// closeSchedulerPools closes whichever scheduler pools are open.
func (f *StoreFactory) closeSchedulerPools() {
	f.sched.mu.Lock()
	defer f.sched.mu.Unlock()
	if f.sched.work != nil {
		f.sched.work.Close()
		f.sched.work = nil
	}
	if f.sched.heartbeat != nil {
		f.sched.heartbeat.Close()
		f.sched.heartbeat = nil
	}
}

// schedulerQuerier runs a statement on a scheduler pool. It never joins a
// transaction on ctx. Unlike unjoinedQuerier it bounds every acquire, inside a
// transaction or not: the scheduler retries a failed write with a backoff, and
// an unbounded wait would stall that loop instead.
//
// Errors pass through the plain funnel (classifyError), as for unjoinedQuerier:
// the statement is not part of the caller's transaction.
type schedulerQuerier struct {
	factory   *StoreFactory
	heartbeat bool
	what      string
}

func (f *StoreFactory) schedulerQuerier(what string) schedulerQuerier {
	return schedulerQuerier{factory: f, what: what}
}

func (f *StoreFactory) heartbeatQuerier() schedulerQuerier {
	return schedulerQuerier{factory: f, heartbeat: true, what: "scheduler heartbeat"}
}

func (q schedulerQuerier) pool() (*pgxpool.Pool, error) {
	work, heartbeat, err := q.factory.schedulerPools()
	if err != nil {
		return nil, err
	}
	if q.heartbeat {
		return heartbeat, nil
	}
	return work, nil
}

func (q schedulerQuerier) acquire(ctx context.Context, verb string) (*pgxpool.Conn, error) {
	p, err := q.pool()
	if err != nil {
		return nil, err
	}
	acquireCtx, cancel := newAcquireContext(ctx, schedulerAcquireTimeout)
	defer cancel()
	conn, err := p.Acquire(acquireCtx)
	if err != nil {
		return nil, classifyAcquireErr(ctx, acquireCtx, q.what+" "+verb, err)
	}
	return conn, nil
}

// begin opens a READ COMMITTED transaction on the pool. The acquire deadline
// never reaches the returned transaction (see newAcquireContext).
func (q schedulerQuerier) begin(ctx context.Context) (pgx.Tx, error) {
	p, err := q.pool()
	if err != nil {
		return nil, err
	}
	acquireCtx, cancel := newAcquireContext(ctx, schedulerAcquireTimeout)
	defer cancel()
	tx, err := p.BeginTx(acquireCtx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return nil, classifyAcquireErr(ctx, acquireCtx, q.what+" begin", err)
	}
	return tx, nil
}

func (q schedulerQuerier) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	conn, err := q.acquire(ctx, "exec")
	if err != nil {
		return pgconn.CommandTag{}, err
	}
	defer conn.Release()
	tag, err := conn.Exec(ctx, sql, args...)
	return tag, classifyError(err)
}

func (q schedulerQuerier) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	conn, err := q.acquire(ctx, "query")
	if err != nil {
		return nil, err
	}
	rows, err := conn.Query(ctx, sql, args...)
	if err != nil {
		conn.Release()
		return nil, classifyError(err)
	}
	return wrapRows(&releasingRows{Rows: rows, release: conn.Release}, classifyError), nil
}

func (q schedulerQuerier) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	conn, err := q.acquire(ctx, "row read")
	if err != nil {
		return acquireFailedRow{err: err}
	}
	return &classifyingRow{
		inner:    &releasingRow{inner: conn.QueryRow(ctx, sql, args...), release: conn.Release},
		classify: classifyError,
	}
}
```

`plugins/postgres/store_factory.go` — add to the `StoreFactory` struct after
`uuids spi.UUIDGenerator` (`:23`):

```go
	// sched holds the scheduler's own pools (scheduler_pool.go), opened on
	// first use and closed by Close.
	sched schedulerPools
```

and replace `Close` (`:279-285`) with:

```go
func (f *StoreFactory) Close() error {
	if f.unregisterMetrics != nil {
		f.unregisterMetrics()
	}
	f.closeSchedulerPools()
	f.pool.Close()
	return nil
}
```

`plugins/postgres/plugin.go` — replace the body from
`factory := newStoreFactory(pool, cfg)` (`:53`) to the end of `NewFactory`:

```go
	factory := newStoreFactory(pool, cfg)
	if err := factory.openSchedulerPools(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("postgres: %w", err)
	}
	factory.initTransactionManager(&defaultUUIDGenerator{})
	unregister, err := registerPoolMetrics(otel.Meter(meterName), pool)
	if err != nil {
		factory.closeSchedulerPools()
		pool.Close()
		return nil, fmt.Errorf("postgres: %w", err)
	}
	factory.unregisterMetrics = unregister
	return factory, nil
}
```

`plugins/postgres/export_test.go` — append (add `"testing"` to its imports):

```go
// SchedulerPoolForTest returns the factory's scheduler work pool, opening it
// if needed. Test-only.
func SchedulerPoolForTest(t testing.TB, f *StoreFactory) *pgxpool.Pool {
	t.Helper()
	work, _, err := f.schedulerPools()
	if err != nil {
		t.Fatalf("scheduler pools: %v", err)
	}
	return work
}

// CloseSchedulerPoolsForTest closes the scheduler pools a test factory
// opened. Fixtures that close only the main pool call it. Test-only.
func CloseSchedulerPoolsForTest(f *StoreFactory) { f.closeSchedulerPools() }
```

`plugins/postgres/conformance_test.go` — in `newConformancePool`, directly
after the `t.Cleanup` that closes `pool` (ends `:117`):

```go
	// Registered after the main pool's cleanup, so it runs first (LIFO).
	t.Cleanup(func() { postgres.CloseSchedulerPoolsForTest(factory) })
```

- [ ] **Step 4: Run the tests to verify they pass**

```bash
cd /Users/paul/go-projects/cyoda-light/cyoda-go/.worktrees/598-scheduler-ownership/plugins/postgres && \
GOWORK=off go test -run 'TestSchedulerPools_|TestNewFactory_OpensTheSchedulerPools|TestPostgresStoreFactory_' .
```

Expected: `ok` (about 10 s: the lock and acquire bounds are waited out).

- [ ] **Step 5: Commit**

```bash
cd /Users/paul/go-projects/cyoda-light/cyoda-go/.worktrees/598-scheduler-ownership && \
git add plugins/postgres/scheduler_pool.go plugins/postgres/scheduler_pool_test.go \
  plugins/postgres/store_factory.go plugins/postgres/plugin.go plugins/postgres/export_test.go \
  plugins/postgres/conformance_test.go && \
git commit -m "feat(postgres): the scheduler has its own pool and heartbeat connection

READ COMMITTED; statement_timeout 30s, idle-in-transaction 10s,
lock_timeout 2s; every acquire bounded at 5s. Derived from the main
pool's config, opened on first use, opened eagerly by NewFactory.

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

- [ ] **Step 6: Write the failing tests for the `pool` attribute (README C-R6)**

The gauge `cyoda.storage.pool.connections` reports every pool of the
factory, each under `pool` = `main`, `scheduler` or `heartbeat`, so an
operator can tell a saturated scheduler pool from a saturated main pool.

In `plugins/postgres/metrics_test.go`, replace
`TestRegisterPoolMetrics_ReportsPoolStat` (`:14-86`, its comment included) with:

```go
// The callback reports each pool's current state under backend="postgres"
// and pool="main", "scheduler" or "heartbeat", and unregistering stops it.
//
// This lives in the postgres_test package (not postgres) so it can share
// newTestPool (migrate_test.go) — which carries the pgx v5.9.1
// HealthCheckPeriod-hang workaround — instead of duplicating pool
// construction. registerPoolMetrics and meterName are unexported production
// symbols reached here through the export_test.go idiom the rest of this
// plugin already uses (RegisterPoolMetricsForTest, MeterNameForTest).
func TestRegisterPoolMetrics_ReportsPoolStat(t *testing.T) {
	pool := newTestPool(t)
	f := postgres.NewStoreFactory(pool)
	postgres.SchedulerPoolForTest(t, f) // opens the scheduler and heartbeat pools
	t.Cleanup(func() { postgres.CloseSchedulerPoolsForTest(f) })
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	unregister, err := postgres.RegisterPoolMetricsForTest(mp.Meter(postgres.MeterNameForTest), f)
	if err != nil {
		t.Fatal(err)
	}

	conn, err := pool.Acquire(context.Background()) // one acquired connection
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	found := map[string]bool{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			found[m.Name] = true
			if m.Name == "cyoda.storage.pool.connections" {
				g := m.Data.(metricdata.Gauge[int64])
				var acquired int64 = -1
				pools := map[string]bool{}
				for _, dp := range g.DataPoints {
					state, _ := dp.Attributes.Value(attribute.Key("state"))
					backend, _ := dp.Attributes.Value(attribute.Key("backend"))
					name, _ := dp.Attributes.Value(attribute.Key("pool"))
					if backend.AsString() != "postgres" {
						t.Fatalf("data point without backend=postgres: %v", dp.Attributes)
					}
					pools[name.AsString()] = true
					if name.AsString() == "main" && state.AsString() == "acquired" {
						acquired = dp.Value
					}
				}
				if acquired < 1 {
					t.Fatalf("acquired connections of the main pool = %d, want >= 1", acquired)
				}
				for _, want := range []string{"main", "scheduler", "heartbeat"} {
					if !pools[want] {
						t.Errorf("no data point with pool=%s; got pools %v", want, pools)
					}
				}
				if len(pools) != 3 {
					t.Errorf("pools = %v, want exactly main, scheduler and heartbeat", pools)
				}
			}
		}
	}
	for _, want := range []string{
		"cyoda.storage.pool.connections", "cyoda.storage.pool.max_connections",
		"cyoda.storage.pool.acquires", "cyoda.storage.pool.empty_acquires",
		"cyoda.storage.pool.canceled_acquires", "cyoda.storage.pool.acquire_duration",
		"cyoda.storage.pool.empty_acquire_wait",
	} {
		if !found[want] {
			t.Errorf("instrument %s not reported", want)
		}
	}

	unregister()
	rm = metricdata.ResourceMetrics{}
	_ = reader.Collect(context.Background(), &rm)
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name == "cyoda.storage.pool.connections" && len(m.Data.(metricdata.Gauge[int64]).DataPoints) > 0 {
				t.Fatal("callback still reporting after unregister")
			}
		}
	}
}
```

In `plugins/postgres/export_test.go`, replace `var RegisterPoolMetricsForTest = registerPoolMetrics`
(`:253`) with the function below, change the first line of its comment
(`:246`) to "RegisterPoolMetricsForTest exposes registerPoolMetrics for a
factory's pools to the external", and add
`"go.opentelemetry.io/otel/metric"` to the imports:

```go
func RegisterPoolMetricsForTest(meter metric.Meter, f *StoreFactory) (func(), error) {
	return registerPoolMetrics(meter, f.pool, &f.sched)
}
```

In `internal/e2e/pool_metrics_test.go`, replace the two `connections` lines
(`:26-27`) with:

```go
		`cyoda_storage_pool_connections{backend="postgres",pool="main",state="acquired"}`,
		`cyoda_storage_pool_connections{backend="postgres",pool="main",state="idle"}`,
		`cyoda_storage_pool_connections{backend="postgres",pool="scheduler",state="idle"}`,
		`cyoda_storage_pool_connections{backend="postgres",pool="heartbeat",state="idle"}`,
```

and its comment (`:13-14`) with "Pool statistics are exported on the metrics
endpoint with the rendered Prometheus names, labelled backend="postgres"; the
connections gauge also carries the pool: main, scheduler or heartbeat."

- [ ] **Step 7: Run the tests to verify they fail**

```bash
cd /Users/paul/go-projects/cyoda-light/cyoda-go/.worktrees/598-scheduler-ownership/plugins/postgres && \
GOWORK=off go test -run 'TestRegisterPoolMetrics_ReportsPoolStat' .
```

Expected: build failure — `too many arguments in call to registerPoolMetrics`
(from `export_test.go`).

```bash
cd /Users/paul/go-projects/cyoda-light/cyoda-go/.worktrees/598-scheduler-ownership && make preflight && \
go test ./internal/e2e/ -run 'TestMetrics_PostgresPoolSeriesAreExported'
```

This run uses the workspace, so the root module builds against this
worktree's `plugins/postgres` (the SPI `use` line comes only at BP-4 Step 0).
Expected: FAIL — `metrics output lacks "cyoda_storage_pool_connections{backend=\"postgres\",pool=\"main\",state=\"acquired\"}"`
and the three other new lines.

- [ ] **Step 8: Implement**

In `plugins/postgres/scheduler_pool.go`, add after `closeSchedulerPools`:

```go
// snapshot returns the scheduler pools that are open now; nil for one that is
// not. The metrics callback reads them at each scrape.
func (p *schedulerPools) snapshot() (work, heartbeat *pgxpool.Pool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.work, p.heartbeat
}
```

In `plugins/postgres/metrics.go`, replace the comment and signature of
`registerPoolMetrics` (`:15-22`) with:

```go
// registerPoolMetrics exports pgxpool.Stat on every scrape as observable
// instruments. Pool saturation is the dominant outage mode of this design;
// empty_acquire_wait (time callers spent waiting because the pool was
// empty) is the signal to alarm on. The connections gauge reports each pool
// under pool="main", "scheduler" or "heartbeat", so a saturated scheduler
// pool is told apart from a saturated main pool; the other instruments are
// the main pool's. sched may be nil, and a scheduler pool that is not open
// is not reported. One factory per process is assumed: two factories would
// both observe backend="postgres" and the last observation per cycle would
// win. The returned func unregisters the callback and must run before the
// pools are closed.
func registerPoolMetrics(meter metric.Meter, pool *pgxpool.Pool, sched *schedulerPools) (func(), error) {
```

and replace the attribute block and the callback (`:59-77`) with:

```go
	backend := attribute.String("backend", "postgres")
	plain := metric.WithAttributes(backend)
	observeConnections := func(o metric.Observer, name string, st *pgxpool.Stat) {
		p := attribute.String("pool", name)
		o.ObserveInt64(connections, int64(st.AcquiredConns()), metric.WithAttributes(backend, p, attribute.String("state", "acquired")))
		o.ObserveInt64(connections, int64(st.IdleConns()), metric.WithAttributes(backend, p, attribute.String("state", "idle")))
		o.ObserveInt64(connections, int64(st.ConstructingConns()), metric.WithAttributes(backend, p, attribute.String("state", "constructing")))
	}

	reg, err := meter.RegisterCallback(func(_ context.Context, o metric.Observer) error {
		st := pool.Stat()
		observeConnections(o, "main", st)
		if sched != nil {
			work, heartbeat := sched.snapshot()
			if work != nil {
				observeConnections(o, "scheduler", work.Stat())
			}
			if heartbeat != nil {
				observeConnections(o, "heartbeat", heartbeat.Stat())
			}
		}
		o.ObserveInt64(maxConns, int64(st.MaxConns()), plain)
		o.ObserveInt64(acquires, st.AcquireCount(), plain)
		o.ObserveInt64(emptyAcquires, st.EmptyAcquireCount(), plain)
		o.ObserveInt64(canceled, st.CanceledAcquireCount(), plain)
		o.ObserveFloat64(acquireDuration, st.AcquireDuration().Seconds(), plain)
		o.ObserveFloat64(emptyWait, st.EmptyAcquireWaitTime().Seconds(), plain)
		return nil
	}, connections, maxConns, acquires, emptyAcquires, canceled, acquireDuration, emptyWait)
```

In `plugins/postgres/plugin.go` (the `NewFactory` body Step 3 wrote), change
`registerPoolMetrics(otel.Meter(meterName), pool)` to
`registerPoolMetrics(otel.Meter(meterName), pool, &factory.sched)`. The
scheduler pools are open by then (`openSchedulerPools` runs first).

- [ ] **Step 9: Run the tests to verify they pass**

```bash
cd /Users/paul/go-projects/cyoda-light/cyoda-go/.worktrees/598-scheduler-ownership/plugins/postgres && \
GOWORK=off go test -run 'TestRegisterPoolMetrics_|TestSchedulerPools_|TestNewFactory_OpensTheSchedulerPools' . && \
GOWORK=off go vet .
cd /Users/paul/go-projects/cyoda-light/cyoda-go/.worktrees/598-scheduler-ownership && \
go test ./internal/e2e/ -run 'TestMetrics_PostgresPoolSeriesAreExported'
```

Expected: `ok` for both; vet clean.

- [ ] **Step 10: Commit**

```bash
cd /Users/paul/go-projects/cyoda-light/cyoda-go/.worktrees/598-scheduler-ownership && \
git add plugins/postgres/metrics.go plugins/postgres/metrics_test.go plugins/postgres/scheduler_pool.go \
  plugins/postgres/plugin.go plugins/postgres/export_test.go internal/e2e/pool_metrics_test.go && \
git commit -m "feat(postgres): the pool gauge names its pool: main, scheduler, heartbeat

cyoda.storage.pool.connections gains a pool attribute and reports the
scheduler pool and the heartbeat connection beside the main pool.

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task BP-3: The async-search heartbeat and claim move to the scheduler pool

**Spec:** §10.2 "What runs on it: … the async-search heartbeat and claim".
§13 row "async-search heartbeats and claims run on the scheduler pool and are
not starved" (the E cell is stream T's; this is the plugin-level proof).

**Files:**
- Modify: `plugins/postgres/search_store.go` (struct `:21-43`, `Heartbeat` `:203-225`, `ClaimStale` `:543-608`)
- Modify: `plugins/postgres/store_factory.go` (`poolQuerier` godoc `:166-175`, `AsyncSearchStore` `:258-268`)
- Modify: `plugins/postgres/unjoined_querier.go` (godoc `:55-59`)
- Modify: `plugins/postgres/search_store_test.go` (`setupSearchTest` `:15-26`), `plugins/postgres/search_store_fencing_test.go` (fixture ending `:312`)
- Test: `plugins/postgres/search_store_scheduler_pool_test.go` (new)

**Interfaces:**
- Consumes: `schedulerQuerier`, `CloseSchedulerPoolsForTest` (BP-2).
- Produces: `asyncSearchStore.sched Querier`. No SPI change.

- [ ] **Step 1: Write the failing test**

`plugins/postgres/search_store_scheduler_pool_test.go`:

```go
package postgres_test

import (
	"context"
	"testing"
	"time"

	"github.com/cyoda-platform/cyoda-go/plugins/postgres"
)

// With every main-pool connection held by an open entity transaction, the
// executor's liveness writes still land: they run on the scheduler pool.
func TestPGSearchStore_LivenessSurvivesAnExhaustedMainPool(t *testing.T) {
	pool := newTestPoolSized(t, 2)
	if err := postgres.DropSchemaForTest(pool); err != nil {
		t.Fatalf("reset schema: %v", err)
	}
	if err := postgres.Migrate(pool); err != nil {
		t.Fatalf("migration failed: %v", err)
	}
	t.Cleanup(func() { _ = postgres.DropSchemaForTest(pool) })
	factory := postgres.NewStoreFactory(pool)
	factory.InitTransactionManager(newTestUUIDGenerator())
	t.Cleanup(func() { postgres.CloseSchedulerPoolsForTest(factory) })

	ctx := ctxWithTenant("starve-tenant")
	store, err := factory.AsyncSearchStore(ctx)
	if err != nil {
		t.Fatalf("AsyncSearchStore: %v", err)
	}
	if err := store.CreateJob(ctx, newRunningJob("job-starve", "starve-tenant", time.Now())); err != nil {
		t.Fatalf("CreateJob: %v", err)
	}

	tm, err := factory.TransactionManager(ctx)
	if err != nil {
		t.Fatalf("TransactionManager: %v", err)
	}
	for i := 0; i < 2; i++ {
		postgres.BeginGuardedForTest(t, tm, ctx) // holds one main-pool connection each
	}

	liveCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if err := store.Heartbeat(liveCtx, "job-starve", 1); err != nil {
		t.Fatalf("Heartbeat with the main pool exhausted: %v", err)
	}
	if _, err := store.ClaimStale(liveCtx, time.Hour, 10); err != nil {
		t.Fatalf("ClaimStale with the main pool exhausted: %v", err)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

```bash
cd /Users/paul/go-projects/cyoda-light/cyoda-go/.worktrees/598-scheduler-ownership/plugins/postgres && \
GOWORK=off go test -run 'TestPGSearchStore_LivenessSurvivesAnExhaustedMainPool' .
```

Expected: FAIL, `Heartbeat with the main pool exhausted: failed to heartbeat search job job-starve: … context deadline exceeded` after about 3 s.

- [ ] **Step 3: Implement**

`plugins/postgres/search_store.go` — add to the struct after the `q` field
(`:27`):

```go
	// sched carries the executor's liveness statements, Heartbeat and
	// ClaimStale, on the scheduler pool (scheduler_pool.go), so a main pool
	// exhausted by entity transactions cannot starve them. Like q, it never
	// joins a transaction on ctx.
	sched Querier
```

In `Heartbeat`, replace `s.q.Exec(` (`:213`) with `s.sched.Exec(` and
`s.probeFenced(ctx, s.q, jobID, tid, epoch, false)` (`:222`) with
`s.probeFenced(ctx, s.sched, jobID, tid, epoch, false)`. In `ClaimStale`,
replace `rows, err := s.q.Query(ctx,` (`:570`) with
`rows, err := s.sched.Query(ctx,`. Append to the `Heartbeat` godoc
(`:203-206`) and to the `ClaimStale` godoc (`:543-558`) the line:

```go
// It runs on the scheduler pool (see the sched field).
```

`plugins/postgres/store_factory.go` — in `AsyncSearchStore` (`:262-267`), add
the field:

```go
		sched:                  f.schedulerQuerier("async search liveness"),
```

In the `poolQuerier` godoc (`:173-175`), replace
`Outside a transaction (the reaper, the heartbeat, the job goroutine's own
writes) it is the plain unbounded pool, unchanged.` with:

```go
// Outside a transaction (the reaper, the job goroutine's own writes) it is the
// plain unbounded pool. The heartbeat and the claim are not here: they run on
// the scheduler pool.
```

`plugins/postgres/unjoined_querier.go` — in the godoc at `:57-59`, replace
`(the reaper, the heartbeat, the job goroutine's terminal write)` with
`(the reaper, the job goroutine's terminal write)`.

`plugins/postgres/search_store_test.go` — `setupSearchTest` (`:15-26`),
replace its last line with:

```go
	f := postgres.NewStoreFactory(pool)
	t.Cleanup(func() { postgres.CloseSchedulerPoolsForTest(f) })
	return f
```

`plugins/postgres/search_store_fencing_test.go` — replace
`return postgres.NewStoreFactory(pool), pool` (`:312`) with:

```go
	f := postgres.NewStoreFactory(pool)
	t.Cleanup(func() { postgres.CloseSchedulerPoolsForTest(f) })
	return f, pool
```

- [ ] **Step 4: Run the tests to verify they pass**

```bash
cd /Users/paul/go-projects/cyoda-light/cyoda-go/.worktrees/598-scheduler-ownership/plugins/postgres && \
GOWORK=off go test -run 'TestPGSearchStore_|TestPostgresStoreFactory_|TestConformance/AsyncSearch' .
```

Expected: `ok`.

- [ ] **Step 5: Commit**

```bash
cd /Users/paul/go-projects/cyoda-light/cyoda-go/.worktrees/598-scheduler-ownership && \
git add plugins/postgres/search_store.go plugins/postgres/store_factory.go plugins/postgres/unjoined_querier.go \
  plugins/postgres/search_store_test.go plugins/postgres/search_store_fencing_test.go \
  plugins/postgres/search_store_scheduler_pool_test.go && \
git commit -m "feat(postgres): async-search heartbeat and claim run on the scheduler pool

An exhausted main pool no longer starves the executor's liveness writes.

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task BP-4: Migration 000014 and the store on the new contract

**Spec:** §10.1 (every method, C1–C6), §10.2 (the migration, `ClaimDue`,
`MarkUnsafe`, `RecordAttempt`, `Fail`, `GiveBackIdle`, the deletes, tenant
scoping, the 55P03/40P01/unique-violation handling), §16 V5, §15 exit check
for the plugin. It makes the S suite pass on PostgreSQL except the
`spi.ErrStoreRejected` case(s), which BP-5 turns green.

**Files:**
- Create: `plugins/postgres/migrations/000014_scheduled_run_ownership.up.sql`, `…down.sql`
- Rewrite: `plugins/postgres/scheduled_task_store.go`
- Modify: `plugins/postgres/store_factory.go` (`ScheduledTaskStore` `:270-277`)
- Modify: `plugins/postgres/migration_index_guard_test.go` (grandfathered map, after `:132`)
- Modify: `plugins/postgres/rls_test.go` (`:49-59`)
- Rewrite: `plugins/postgres/scheduled_task_store_test.go`
- Delete: `plugins/postgres/scheduled_task_attribution_internal_test.go` (its case moves into the backfill test)
- Test: `plugins/postgres/scheduled_task_migration_test.go` (new), `plugins/postgres/scheduled_task_claim_race_internal_test.go` (new)

**Interfaces:**
- Consumes (SPI, stream S — `interfaces.md:7-153`): `spi.ScheduledTaskStore`,
  `spi.ScheduledTask` and its new fields, `spi.TaskRef`, `spi.TaskClaim`,
  `spi.ClaimRequest`, `spi.Attempt`, `spi.Failure`, `spi.ScheduledTaskQuery`,
  `spi.ScheduledTaskCursor`, `spi.ScheduledTaskPage`, the status and reason
  constants, `spi.ErrStaleClaim`, `spi.ErrMarkedByAnotherClaim`,
  `spi.ErrTaskBusy`, `spi.ErrStoreRejected`, `spi.ErrTxTenantMismatch`,
  `ScheduledTask.ClaimedFromLostOwner` (README C-S1); `runScheduledTasksSuite`
  registered in `spitest.StoreFactoryConformance`.
  From BP-2: `schedulerQuerier`, `SchedulerPoolForTest`, `CloseSchedulerPoolsForTest`.
- Produces: `*scheduledTaskStore` satisfying `spi.ScheduledTaskStore`;
  `func lostClaimRace(err error) bool`; `func isLockNotAvailable(err error) bool`;
  `func joinTenant(ctx context.Context, tenant spi.TenantID) error`.
  `ClaimDue` sets `ClaimedFromLostOwner` on each task it took from a stale or
  missing owner (README C-S1). Every joining method refuses a tenant that is
  not the transaction's with `spi.ErrTxTenantMismatch` (README C-S5), as
  memory and SQLite do. `last_error` carries
  `CHECK (octet_length(last_error) <= 1024)` (README C-S3).

- [ ] **Step 0: Put S's SPI on the workspace**

```bash
cd /Users/paul/go-projects/cyoda-light/cyoda-go/.worktrees/598-scheduler-ownership && \
grep -q cyoda-go-spi go.work || go work use /Users/paul/go-projects/cyoda-light/cyoda-go-spi; \
git status --short go.work
```

Expected: ` M go.work`. The line is never committed. Check that
`/Users/paul/go-projects/cyoda-light/cyoda-go-spi` is on S's branch
(`git -C /Users/paul/go-projects/cyoda-light/cyoda-go-spi log --oneline -1`)
and that `grep -n 'ErrTaskBusy' /Users/paul/go-projects/cyoda-light/cyoda-go-spi/errors.go`
prints a line. If not, stop: S has not landed.

- [ ] **Step 1: Write the failing migration tests**

`plugins/postgres/scheduled_task_migration_test.go`:

```go
package postgres_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgerrcode"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/plugins/postgres"
)

func stringSet(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) map[string]bool {
	t.Helper()
	rows, err := pool.Query(context.Background(), sql, args...)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out[s] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	return out
}

func mustGet(t *testing.T, sts spi.ScheduledTaskStore, tenant spi.TenantID, id string) spi.ScheduledTask {
	t.Helper()
	got, found, err := sts.Get(context.Background(), tenant, id)
	if err != nil || !found {
		t.Fatalf("Get(%s, %s): found=%v err=%v", tenant, id, found, err)
	}
	return *got
}

func TestMigration14_ScheduledTaskSchema(t *testing.T) {
	pool := newTestPool(t)
	if err := postgres.DropSchemaForTest(pool); err != nil {
		t.Fatalf("reset schema: %v", err)
	}
	if err := postgres.Migrate(pool); err != nil {
		t.Fatalf("migration failed: %v", err)
	}
	t.Cleanup(func() { _ = postgres.DropSchemaForTest(pool) })
	ctx := context.Background()

	cols := stringSet(t, pool, `SELECT column_name FROM information_schema.columns
		WHERE table_schema = 'public' AND table_name = 'scheduled_tasks'`)
	for _, c := range []string{"arm_token", "status", "next_attempt_time", "attempts", "lost_owners",
		"last_attempt_time", "last_error", "failure_reason", "failed_time", "partial_commit",
		"claim_token", "claim_owner"} {
		if !cols[c] {
			t.Errorf("scheduled_tasks lacks column %s", c)
		}
	}
	for _, c := range []string{"redispatch_after", "attempt_count"} {
		if cols[c] {
			t.Errorf("scheduled_tasks still has column %s", c)
		}
	}

	idx := stringSet(t, pool, `SELECT indexname FROM pg_indexes
		WHERE schemaname = 'public' AND tablename = 'scheduled_tasks'`)
	for _, i := range []string{"scheduled_tasks_waiting_due_idx", "scheduled_tasks_running_owner_idx",
		"scheduled_tasks_one_running_per_entity_uq", "scheduled_tasks_query_idx",
		"scheduled_tasks_model_idx", "scheduled_tasks_entity_idx"} {
		if !idx[i] {
			t.Errorf("scheduled_tasks lacks index %s", i)
		}
	}
	if idx["scheduled_tasks_due_idx"] {
		t.Error("scheduled_tasks_due_idx was not dropped")
	}
	for _, table := range []string{"scheduled_task_marks", "scheduler_owners"} {
		var exists bool
		if err := pool.QueryRow(ctx, `SELECT to_regclass($1) IS NOT NULL`, "public."+table).Scan(&exists); err != nil || !exists {
			t.Errorf("table %s missing (err=%v)", table, err)
		}
	}

	insert := `INSERT INTO scheduled_tasks (id, tenant_id, type, scheduled_time, entity_id, model_name,
		model_version, transition, source_state, armed_at, arm_token, status, next_attempt_time,
		claim_token, claim_owner)
		VALUES ($1, 't', 'fire-transition', 1, 'e', 'M', 1, $2, 'S', 0, gen_random_uuid(), 'RUNNING', 1,
		        gen_random_uuid(), gen_random_uuid())`
	if _, err := pool.Exec(ctx, insert, "a", "T1"); err != nil {
		t.Fatalf("first RUNNING task: %v", err)
	}
	_, err := pool.Exec(ctx, insert, "b", "T2")
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != pgerrcode.UniqueViolation ||
		pgErr.ConstraintName != "scheduled_tasks_one_running_per_entity_uq" {
		t.Errorf("second RUNNING task of one entity: err = %v, want the one-running-per-entity unique violation", err)
	}
	_, err = pool.Exec(ctx, `INSERT INTO scheduled_tasks (id, tenant_id, type, scheduled_time, entity_id,
		model_name, model_version, transition, source_state, armed_at, arm_token, status, next_attempt_time)
		VALUES ('c', 't', 'fire-transition', 1, 'f', 'M', 1, 'T', 'S', 0, gen_random_uuid(), 'RUNNING', 1)`)
	if !errors.As(err, &pgErr) || pgErr.Code != pgerrcode.CheckViolation || pgErr.ConstraintName != "scheduled_tasks_claim_chk" {
		t.Errorf("RUNNING without a claim: err = %v, want scheduled_tasks_claim_chk", err)
	}

	// last_error holds at most 1 024 bytes, counted in bytes, not characters.
	withError := `INSERT INTO scheduled_tasks (id, tenant_id, type, scheduled_time, entity_id,
		model_name, model_version, transition, source_state, armed_at, arm_token, status, next_attempt_time,
		last_error)
		VALUES ($1, 't', 'fire-transition', 1, $1, 'M', 1, 'T', 'S', 0, gen_random_uuid(), 'WAITING', 1, $2)`
	atLimit := strings.Repeat("é", 512) // 1 024 bytes
	if _, err := pool.Exec(ctx, withError, "g", atLimit); err != nil {
		t.Errorf("last_error of 1 024 bytes: %v", err)
	}
	_, err = pool.Exec(ctx, withError, "h", atLimit+"x")
	if !errors.As(err, &pgErr) || pgErr.Code != pgerrcode.CheckViolation ||
		pgErr.ConstraintName != "scheduled_tasks_last_error_len_chk" {
		t.Errorf("last_error of 1 025 bytes: err = %v, want scheduled_tasks_last_error_len_chk", err)
	}
}

// Rows armed before 000014 become WAITING lives, due at their scheduled time,
// each with its own arm token. A row that predates the attribution columns
// reads back with the zero Principal.
func TestMigration14_ExistingTasksBecomeWaitingLives(t *testing.T) {
	pool := newTestPool(t)
	if err := postgres.DropSchemaForTest(pool); err != nil {
		t.Fatalf("reset schema: %v", err)
	}
	t.Cleanup(func() { _ = postgres.DropSchemaForTest(pool) })
	if err := postgres.MigrateToVersionForTest(pool, 13); err != nil {
		t.Fatalf("migrate to 13: %v", err)
	}
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `INSERT INTO scheduled_tasks
		(id, tenant_id, type, scheduled_time, timeout_ms, redispatch_after, entity_id, model_name,
		 model_version, transition, source_state, armed_at, attempt_count, armed_by_id, armed_by_kind)
		VALUES ('a:S:T', 'tenant-A', 'fire-transition', 1000, 60000, 5000, 'a', 'M', 1, 'T', 'S', 10, 2, 'u1', 'user'),
		       ('legacy:S:T', 'tenant-A', 'fire-transition', 2000, NULL, NULL, 'legacy', 'M', 1, 'T', 'S', 20, 0, '', '')`); err != nil {
		t.Fatalf("seed version-13 rows: %v", err)
	}
	if err := postgres.MigrateToVersionForTest(pool, 14); err != nil {
		t.Fatalf("migrate to 14: %v", err)
	}

	f := postgres.NewStoreFactory(pool)
	t.Cleanup(func() { postgres.CloseSchedulerPoolsForTest(f) })
	sts, err := f.ScheduledTaskStore(ctx)
	if err != nil {
		t.Fatalf("ScheduledTaskStore: %v", err)
	}
	a := mustGet(t, sts, "tenant-A", "a:S:T")
	legacy := mustGet(t, sts, "tenant-A", "legacy:S:T")
	for _, task := range []spi.ScheduledTask{a, legacy} {
		if task.Status != spi.ScheduledTaskWaiting || task.NextAttemptTime != task.ScheduledTime ||
			task.Attempts != 0 || task.LostOwners != 0 || task.Claim != nil || task.ArmToken == uuid.Nil ||
			task.PartialCommit || task.UnsafeMarked || task.FailureReason != "" || task.LastError != "" {
			t.Errorf("%s after 000014 = %+v, want a fresh WAITING life", task.ID, task)
		}
	}
	if a.ArmToken == legacy.ArmToken {
		t.Error("two rows share one arm token")
	}
	if a.ArmedBy != (spi.Principal{ID: "u1", Kind: spi.PrincipalUser}) {
		t.Errorf("ArmedBy = %+v, want the stored principal", a.ArmedBy)
	}
	if legacy.ArmedBy != (spi.Principal{}) {
		t.Errorf("legacy ArmedBy = %+v, want the zero Principal", legacy.ArmedBy)
	}
	if a.TimeoutMs == nil || *a.TimeoutMs != 60000 || legacy.TimeoutMs != nil {
		t.Errorf("TimeoutMs = %v / %v, want 60000 / nil", a.TimeoutMs, legacy.TimeoutMs)
	}
}

// The down migration restores the version-13 shape and removes FAILED tasks,
// which that shape would make due again.
func TestMigration14_DownRestoresTheVersion13Shape(t *testing.T) {
	pool := newTestPool(t)
	if err := postgres.DropSchemaForTest(pool); err != nil {
		t.Fatalf("reset schema: %v", err)
	}
	t.Cleanup(func() { _ = postgres.DropSchemaForTest(pool) })
	if err := postgres.MigrateToVersionForTest(pool, 14); err != nil {
		t.Fatalf("migrate to 14: %v", err)
	}
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `INSERT INTO scheduled_tasks (id, tenant_id, type, scheduled_time, entity_id,
		model_name, model_version, transition, source_state, armed_at, arm_token, status, next_attempt_time,
		failure_reason)
		VALUES ('w', 't', 'fire-transition', 1, 'e1', 'M', 1, 'T', 'S', 0, gen_random_uuid(), 'WAITING', 1, ''),
		       ('f', 't', 'fire-transition', 1, 'e2', 'M', 1, 'T', 'S', 0, gen_random_uuid(), 'FAILED', 1, 'RUN_PANICKED')`); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := postgres.MigrateToVersionForTest(pool, 13); err != nil {
		t.Fatalf("migrate down to 13: %v", err)
	}
	ids := stringSet(t, pool, `SELECT id FROM scheduled_tasks`)
	if !ids["w"] || ids["f"] {
		t.Errorf("tasks after down = %v, want only the WAITING one", ids)
	}
	cols := stringSet(t, pool, `SELECT column_name FROM information_schema.columns
		WHERE table_schema = 'public' AND table_name = 'scheduled_tasks'`)
	if !cols["attempt_count"] || !cols["redispatch_after"] || cols["arm_token"] {
		t.Errorf("columns after down = %v, want the version-13 set", cols)
	}
}
```

- [ ] **Step 2: Write the failing store tests**

Replace `plugins/postgres/scheduled_task_store_test.go` with:

```go
package postgres_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgerrcode"
	"github.com/jackc/pgx/v5/pgconn"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/plugins/postgres"
)

// newTaskStore is a migrated schema, a factory with a transaction manager, and
// its scheduled-task store. maxConns sizes the main pool.
func newTaskStore(t *testing.T, maxConns int32) (*postgres.StoreFactory, spi.ScheduledTaskStore) {
	t.Helper()
	pool := newTestPoolSized(t, maxConns)
	if err := postgres.DropSchemaForTest(pool); err != nil {
		t.Fatalf("reset schema: %v", err)
	}
	if err := postgres.Migrate(pool); err != nil {
		t.Fatalf("migration failed: %v", err)
	}
	t.Cleanup(func() { _ = postgres.DropSchemaForTest(pool) })
	f := postgres.NewStoreFactory(pool)
	f.InitTransactionManager(newTestUUIDGenerator())
	t.Cleanup(func() { postgres.CloseSchedulerPoolsForTest(f) })
	sts, err := f.ScheduledTaskStore(context.Background())
	if err != nil {
		t.Fatalf("ScheduledTaskStore: %v", err)
	}
	return f, sts
}

func taskSpec(tenant spi.TenantID, entityID, state, transition string, scheduledTime int64) spi.ScheduledTask {
	return spi.ScheduledTask{
		ID: entityID + ":" + state + ":" + transition, TenantID: tenant,
		Type: spi.ScheduledTaskFireTransition, ScheduledTime: scheduledTime,
		EntityID: entityID, ModelName: "M", ModelVersion: 1, SourceState: state, Transition: transition,
	}
}

// arm arms one entity's tasks outside any transaction, so the arm commits.
func arm(t *testing.T, sts spi.ScheduledTaskStore, tenant spi.TenantID, entityID, state string, tasks ...spi.ScheduledTask) {
	t.Helper()
	if _, err := sts.ReconcileForEntity(context.Background(), spi.ReconcileRequest{
		TenantID: tenant, EntityID: entityID, CurrentState: state, Arm: tasks,
	}); err != nil {
		t.Fatalf("ReconcileForEntity: %v", err)
	}
}

func claimRequest(owner uuid.UUID) spi.ClaimRequest {
	return spi.ClaimRequest{Owner: owner, NowMs: time.Now().UnixMilli(), StaleAfter: time.Minute, Limit: 10, PerTenantLimit: 10}
}

func claimAll(t *testing.T, sts spi.ScheduledTaskStore) []spi.ScheduledTask {
	t.Helper()
	got, err := sts.ClaimDue(context.Background(), claimRequest(uuid.New()))
	if err != nil {
		t.Fatalf("ClaimDue: %v", err)
	}
	return got
}

func refOf(task spi.ScheduledTask) spi.TaskRef {
	return spi.TaskRef{TenantID: task.TenantID, ID: task.ID, ArmToken: task.ArmToken, ClaimToken: task.Claim.Token}
}

// beginEntityTx opens an entity transaction (REPEATABLE READ, snapshot taken
// inside Begin) and returns its context and an early rollback.
func beginEntityTx(t *testing.T, f *postgres.StoreFactory, tenant spi.TenantID) (context.Context, func()) {
	t.Helper()
	tm, err := f.TransactionManager(context.Background())
	if err != nil {
		t.Fatalf("TransactionManager: %v", err)
	}
	txID, txCtx := postgres.BeginGuardedForTest(t, tm, ctxWithTenant(tenant))
	return txCtx, func() {
		if err := tm.Rollback(txCtx, txID); err != nil {
			t.Fatalf("Rollback: %v", err)
		}
	}
}

func sqlState(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code
	}
	return ""
}

// An arm issued inside an entity transaction is part of it.
func TestPostgres_ScheduledTaskArm_RollbackIsAtomic(t *testing.T) {
	f, sts := newTaskStore(t, 5)
	txCtx, rollback := beginEntityTx(t, f, "tenant-A")
	if _, err := sts.ReconcileForEntity(txCtx, spi.ReconcileRequest{
		TenantID: "tenant-A", EntityID: "e1", CurrentState: "S",
		Arm: []spi.ScheduledTask{taskSpec("tenant-A", "e1", "S", "T", 1000)},
	}); err != nil {
		t.Fatalf("ReconcileForEntity: %v", err)
	}
	rollback()
	if _, found, err := sts.Get(context.Background(), "tenant-A", "e1:S:T"); err != nil || found {
		t.Fatalf("after rollback: found=%v err=%v, want absent", found, err)
	}
}

// ClaimDue is cross-tenant: one call claims every tenant's due tasks.
func TestPostgres_ScheduledTaskStore_ClaimDueIsCrossTenant(t *testing.T) {
	_, sts := newTaskStore(t, 5)
	for _, tenant := range []spi.TenantID{"tenant-A", "tenant-B", "tenant-C"} {
		arm(t, sts, tenant, "e", "S", taskSpec(tenant, "e", "S", "T", 1000))
	}
	seen := map[spi.TenantID]bool{}
	for _, task := range claimAll(t, sts) {
		seen[task.TenantID] = true
	}
	if len(seen) != 3 {
		t.Fatalf("ClaimDue claimed tenants %v, want all three", seen)
	}
}

// None of the scheduler's tables is under row-level security: ClaimDue,
// GiveBackIdle, the owner methods and the sweeps are cross-tenant.
func TestPostgres_ScheduledTaskTables_NotRLSEnrolled(t *testing.T) {
	f, _ := newTaskStore(t, 5)
	pool := postgres.PoolForTest(f)
	for _, table := range []string{"scheduled_tasks", "scheduled_task_marks", "scheduler_owners"} {
		var rls bool
		if err := pool.QueryRow(context.Background(),
			`SELECT relrowsecurity FROM pg_class WHERE relname = $1`, table).Scan(&rls); err != nil {
			t.Fatalf("read relrowsecurity for %s: %v", table, err)
		}
		if rls {
			t.Errorf("%s has row-level security enabled; the scheduler's cross-tenant methods read it with no tenant set", table)
		}
	}
}

// Every tenant-facing method filters on the tenant it is given.
func TestPostgres_ScheduledTaskStore_TenantFacingMethodsFilterOnTenant(t *testing.T) {
	_, sts := newTaskStore(t, 5)
	ctx := context.Background()
	arm(t, sts, "tenant-A", "e1", "S", taskSpec("tenant-A", "e1", "S", "T", 1000))
	claimed := claimAll(t, sts)
	if len(claimed) != 1 {
		t.Fatalf("claimed %d tasks, want 1", len(claimed))
	}
	a := claimed[0]
	wrong := refOf(a)
	wrong.TenantID = "tenant-B"

	if _, found, err := sts.Get(ctx, "tenant-B", a.ID); err != nil || found {
		t.Errorf("Get as tenant-B: found=%v err=%v", found, err)
	}
	fenced := []struct {
		name string
		call func() error
	}{
		{"StampSegment", func() error { return sts.StampSegment(ctx, wrong, true) }},
		{"MarkUnsafe", func() error { return sts.MarkUnsafe(ctx, wrong) }},
		{"RecordAttempt", func() error {
			return sts.RecordAttempt(ctx, wrong, spi.Attempt{Error: "x", AtMs: 1, NextAttemptTime: 1, ClearOwnMark: true})
		}},
		{"Fail", func() error {
			return sts.Fail(ctx, wrong, spi.Failure{Reason: spi.FailureRunPanicked, Error: "x", AtMs: 1})
		}},
	}
	for _, c := range fenced {
		if err := c.call(); !errors.Is(err, spi.ErrStaleClaim) {
			t.Errorf("%s as tenant-B: err = %v, want ErrStaleClaim", c.name, err)
		}
	}
	if err := sts.RemoveLife(ctx, "tenant-B", a.ID, a.ArmToken); err != nil {
		t.Errorf("RemoveLife as tenant-B: %v", err)
	}
	if err := sts.DeleteForEntities(ctx, "tenant-B", []string{"e1"}); err != nil {
		t.Errorf("DeleteForEntities as tenant-B: %v", err)
	}
	if err := sts.DeleteForModel(ctx, "tenant-B", "M", 1, nil); err != nil {
		t.Errorf("DeleteForModel as tenant-B: %v", err)
	}
	if removed, err := sts.ReconcileForEntity(ctx, spi.ReconcileRequest{
		TenantID: "tenant-B", EntityID: "e1", CurrentState: "S",
	}); err != nil || len(removed) != 0 {
		t.Errorf("ReconcileForEntity as tenant-B: removed=%v err=%v", removed, err)
	}
	if page, err := sts.Query(ctx, "tenant-B", spi.ScheduledTaskQuery{Limit: 10}); err != nil || len(page.Items) != 0 {
		t.Errorf("Query as tenant-B: items=%v err=%v", page.Items, err)
	}

	got, found, err := sts.Get(ctx, "tenant-A", a.ID)
	if err != nil || !found {
		t.Fatalf("tenant-A's task was removed through tenant-B: found=%v err=%v", found, err)
	}
	if got.Status != spi.ScheduledTaskRunning || got.Claim == nil || got.Claim.Token != a.Claim.Token ||
		got.PartialCommit || got.UnsafeMarked || got.LastError != "" {
		t.Errorf("tenant-A's task was changed through tenant-B: %+v", got)
	}
}

// A joining write whose tenant is not the tenant of the transaction on ctx is
// refused before any statement runs, as on memory and SQLite: a task row of
// tenant B never enters tenant A's transaction.
func TestPostgres_ScheduledTaskStore_JoiningWriteOfAnotherTenantRefused(t *testing.T) {
	f, sts := newTaskStore(t, 5)
	arm(t, sts, "tenant-B", "e1", "S", taskSpec("tenant-B", "e1", "S", "T", 1000))
	claimed := claimAll(t, sts)
	if len(claimed) != 1 {
		t.Fatalf("claimed %d tasks, want 1", len(claimed))
	}
	b := claimed[0]
	txCtx, rollback := beginEntityTx(t, f, "tenant-A")
	defer rollback()

	joining := []struct {
		name string
		call func() error
	}{
		{"ReconcileForEntity", func() error {
			_, err := sts.ReconcileForEntity(txCtx, spi.ReconcileRequest{
				TenantID: "tenant-B", EntityID: "e1", CurrentState: "S",
				Arm: []spi.ScheduledTask{taskSpec("tenant-B", "e1", "S", "T", 1000)},
			})
			return err
		}},
		{"RemoveLife", func() error { return sts.RemoveLife(txCtx, "tenant-B", b.ID, b.ArmToken) }},
		{"StampSegment", func() error { return sts.StampSegment(txCtx, refOf(b), true) }},
		{"DeleteForEntities", func() error { return sts.DeleteForEntities(txCtx, "tenant-B", []string{"e1"}) }},
		{"DeleteForModel", func() error { return sts.DeleteForModel(txCtx, "tenant-B", "M", 1, nil) }},
		{"Fail", func() error {
			return sts.Fail(txCtx, refOf(b), spi.Failure{Reason: spi.FailureRunPanicked, Error: "x", AtMs: 1})
		}},
	}
	for _, c := range joining {
		if err := c.call(); !errors.Is(err, spi.ErrTxTenantMismatch) {
			t.Errorf("%s for tenant-B in tenant-A's transaction: err = %v, want ErrTxTenantMismatch", c.name, err)
		}
	}
	got := mustGet(t, sts, "tenant-B", b.ID)
	if got.Status != spi.ScheduledTaskRunning || got.Claim == nil || got.Claim.Token != b.Claim.Token || got.PartialCommit {
		t.Errorf("tenant-B's task was changed through tenant-A's transaction: %+v", got)
	}
}

// C1 and C5: a task row that a claim changed after the entity transaction's
// snapshot fails the transaction's write with ErrConflict, at the statement.
func TestPostgres_ScheduledTaskStore_RowChangedAfterSnapshotIsErrConflict(t *testing.T) {
	f, sts := newTaskStore(t, 5)
	arm(t, sts, "tenant-A", "e1", "S", taskSpec("tenant-A", "e1", "S", "T", 1000))
	before := mustGet(t, sts, "tenant-A", "e1:S:T")

	txCtx, _ := beginEntityTx(t, f, "tenant-A")
	if len(claimAll(t, sts)) != 1 {
		t.Fatal("the claim did not take the task")
	}
	err := sts.RemoveLife(txCtx, "tenant-A", before.ID, before.ArmToken)
	if !errors.Is(err, spi.ErrConflict) || sqlState(err) != pgerrcode.SerializationFailure {
		t.Fatalf("RemoveLife after a concurrent claim: err = %v, want ErrConflict over 40001", err)
	}
}

// C6: a row an open transaction wrote answers ErrTaskBusy at once (NOWAIT).
func TestPostgres_MarkUnsafe_RowWrittenByAnOpenTxIsBusyAtOnce(t *testing.T) {
	f, sts := newTaskStore(t, 5)
	arm(t, sts, "tenant-A", "e1", "S", taskSpec("tenant-A", "e1", "S", "T", 1000))
	task := claimAll(t, sts)[0]
	txCtx, _ := beginEntityTx(t, f, "tenant-A")
	if err := sts.StampSegment(txCtx, refOf(task), false); err != nil {
		t.Fatalf("StampSegment: %v", err)
	}
	start := time.Now()
	err := sts.MarkUnsafe(context.Background(), refOf(task))
	if !errors.Is(err, spi.ErrTaskBusy) {
		t.Fatalf("MarkUnsafe: err = %v, want ErrTaskBusy", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("MarkUnsafe waited %s; NOWAIT must answer at once", elapsed)
	}
}

// A bookkeeping write blocked by a row lock gives up at lock_timeout with
// 55P03 — a retryable error, not a refusal and not a rejection.
func TestPostgres_ScheduledTaskStore_RowLockWaitEndsAtLockTimeout(t *testing.T) {
	f, sts := newTaskStore(t, 5)
	arm(t, sts, "tenant-A", "e1", "S", taskSpec("tenant-A", "e1", "S", "T", 1000))
	task := claimAll(t, sts)[0]
	txCtx, rollback := beginEntityTx(t, f, "tenant-A")
	if err := sts.StampSegment(txCtx, refOf(task), false); err != nil {
		t.Fatalf("StampSegment: %v", err)
	}
	attempt := spi.Attempt{Error: "x", AtMs: 1, NextAttemptTime: 1}
	start := time.Now()
	err := sts.RecordAttempt(context.Background(), refOf(task), attempt)
	elapsed := time.Since(start)
	if sqlState(err) != pgerrcode.LockNotAvailable {
		t.Fatalf("RecordAttempt under a row lock: err = %v, want 55P03", err)
	}
	if errors.Is(err, spi.ErrStaleClaim) || errors.Is(err, spi.ErrConflict) || errors.Is(err, spi.ErrStoreRejected) {
		t.Errorf("55P03 carries a refusal or rejection sentinel: %v", err)
	}
	if elapsed < 1500*time.Millisecond || elapsed > 10*time.Second {
		t.Errorf("gave up after %s, want about the 2s lock_timeout", elapsed)
	}
	rollback()
	if err := sts.RecordAttempt(context.Background(), refOf(task), attempt); err != nil {
		t.Fatalf("RecordAttempt after the lock was released: %v", err)
	}
}

// C4: with every main-pool connection held by an entity transaction, the
// scheduler's never-joining methods still run.
func TestPostgres_ScheduledTaskStore_NotStarvedByAnExhaustedMainPool(t *testing.T) {
	f, sts := newTaskStore(t, 2)
	arm(t, sts, "tenant-A", "e1", "S", taskSpec("tenant-A", "e1", "S", "T", 1000))
	for i := 0; i < 2; i++ {
		beginEntityTx(t, f, "tenant-A")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	owner := uuid.New()
	if err := sts.Heartbeat(ctx, owner); err != nil {
		t.Fatalf("Heartbeat: %v", err)
	}
	claimed, err := sts.ClaimDue(ctx, claimRequest(owner))
	if err != nil || len(claimed) != 1 {
		t.Fatalf("ClaimDue: claimed=%d err=%v", len(claimed), err)
	}
	if err := sts.MarkUnsafe(ctx, refOf(claimed[0])); err != nil {
		t.Fatalf("MarkUnsafe: %v", err)
	}
	now := time.Now().UnixMilli()
	if err := sts.RecordAttempt(ctx, refOf(claimed[0]),
		spi.Attempt{Error: "x", AtMs: now, NextAttemptTime: now, ClearOwnMark: true}); err != nil {
		t.Fatalf("RecordAttempt: %v", err)
	}
	if n, err := sts.GiveBackIdle(ctx, owner, nil); err != nil || n != 0 {
		t.Fatalf("GiveBackIdle: n=%d err=%v", n, err)
	}
}

// Heartbeat has a connection of its own: a saturated scheduler pool does not
// delay it.
func TestPostgres_ScheduledTaskHeartbeat_HasItsOwnConnection(t *testing.T) {
	f, sts := newTaskStore(t, 5)
	work := postgres.SchedulerPoolForTest(t, f)
	for i := int32(0); i < work.Config().MaxConns; i++ {
		c, err := work.Acquire(context.Background())
		if err != nil {
			t.Fatalf("hold scheduler connection %d: %v", i, err)
		}
		t.Cleanup(c.Release)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := sts.Heartbeat(ctx, uuid.New()); err != nil {
		t.Fatalf("Heartbeat with the scheduler pool saturated: %v", err)
	}
}

// V5: a claim that loses a race for a sibling task rolls back and claims
// nothing, within lock_timeout, whether the rival commits or stalls.
func TestPostgres_ClaimDue_LosingASiblingRaceClaimsNothing(t *testing.T) {
	cases := []struct {
		name       string
		rivalHolds time.Duration
		within     time.Duration
	}{
		{"rival commits while the claim waits", 300 * time.Millisecond, 1500 * time.Millisecond},
		{"rival outlasts lock_timeout", 4 * time.Second, 3500 * time.Millisecond},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f, sts := newTaskStore(t, 5)
			bg := context.Background()
			// T2 is due first, so the claim ranks T2.
			arm(t, sts, "tenant-A", "e1", "S",
				taskSpec("tenant-A", "e1", "S", "T1", 1000),
				taskSpec("tenant-A", "e1", "S", "T2", 500))

			// Another pnode has claimed T1 and not committed. The ranking cannot
			// see it, so the claim takes T2 and waits on the unique index.
			rival, err := postgres.PoolForTest(f).Begin(bg)
			if err != nil {
				t.Fatalf("begin rival: %v", err)
			}
			if _, err := rival.Exec(bg, `UPDATE scheduled_tasks
				SET status = 'RUNNING', claim_token = gen_random_uuid(), claim_owner = gen_random_uuid()
				WHERE id = 'e1:S:T1'`); err != nil {
				t.Fatalf("rival claim: %v", err)
			}
			rivalDone := make(chan error, 1)
			go func() {
				time.Sleep(tc.rivalHolds)
				rivalDone <- rival.Commit(context.Background())
			}()

			start := time.Now()
			claimed, claimErr := sts.ClaimDue(bg, claimRequest(uuid.New()))
			elapsed := time.Since(start)
			if err := <-rivalDone; err != nil {
				t.Fatalf("rival commit: %v", err)
			}
			if claimErr != nil {
				t.Fatalf("ClaimDue lost the race and returned an error: %v", claimErr)
			}
			if len(claimed) != 0 {
				t.Fatalf("ClaimDue claimed %d tasks, want none", len(claimed))
			}
			if elapsed > tc.within {
				t.Errorf("ClaimDue took %s, want under %s", elapsed, tc.within)
			}
			if got := mustGet(t, sts, "tenant-A", "e1:S:T2"); got.Status != spi.ScheduledTaskWaiting || got.Claim != nil {
				t.Errorf("T2 after the lost race = %+v, want WAITING and unclaimed", got)
			}
		})
	}
}
```

`plugins/postgres/scheduled_task_claim_race_internal_test.go`:

```go
package postgres

import (
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgerrcode"
	"github.com/jackc/pgx/v5/pgconn"
)

func TestLostClaimRace(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"sibling claimed", &pgconn.PgError{Code: pgerrcode.UniqueViolation, ConstraintName: "scheduled_tasks_one_running_per_entity_uq"}, true},
		{"crossed waits", fmt.Errorf("claim: %w", &pgconn.PgError{Code: pgerrcode.DeadlockDetected}), true},
		{"rival stalled past lock_timeout", &pgconn.PgError{Code: pgerrcode.LockNotAvailable}, true},
		{"another unique index", &pgconn.PgError{Code: pgerrcode.UniqueViolation, ConstraintName: "scheduled_task_marks_pkey"}, false},
		{"serialization failure", &pgconn.PgError{Code: pgerrcode.SerializationFailure}, false},
		{"no server answer", errors.New("connection refused"), false},
		{"nil", nil, false},
	} {
		if got := lostClaimRace(tc.err); got != tc.want {
			t.Errorf("%s: lostClaimRace = %v, want %v", tc.name, got, tc.want)
		}
	}
}
```

Delete `plugins/postgres/scheduled_task_attribution_internal_test.go`
(`git rm`); `TestMigration14_ExistingTasksBecomeWaitingLives` covers its row.

- [ ] **Step 3: Run the tests to verify they fail**

```bash
cd /Users/paul/go-projects/cyoda-light/cyoda-go/.worktrees/598-scheduler-ownership/plugins/postgres && \
go test -run 'TestMigration14_|TestPostgres_ScheduledTask|TestPostgres_MarkUnsafe_|TestPostgres_ClaimDue_|TestLostClaimRace' .
```

Expected: build failure — `*scheduledTaskStore does not implement spi.ScheduledTaskStore (missing method ClaimDue)`, `undefined: lostClaimRace`.

- [ ] **Step 4: Write the migration**

`plugins/postgres/migrations/000014_scheduled_run_ownership.up.sql`:

```sql
-- One owner per scheduled run.
--
-- scheduled_tasks now carries each task's life (arm_token, drawn on every
-- arm), its status (WAITING, RUNNING, FAILED), the claim that owns a RUNNING
-- run (claim_token, claim_owner), and what the scheduler records about its
-- attempts (next_attempt_time, attempts, lost_owners, last_attempt_time,
-- last_error, failure_reason, failed_time, partial_commit). redispatch_after
-- and attempt_count are dropped.
--
-- Existing rows become WAITING lives, due at their scheduled time, each with
-- its own arm token: gen_random_uuid() is volatile, so ADD COLUMN rewrites the
-- table and evaluates it per row.
--
-- scheduled_task_marks holds one row per life whose owner is about to dispatch
-- a processor that is not safe to repeat. It is written outside the entity
-- transaction, so it survives that transaction's rollback. scheduler_owners
-- holds one liveness row per pnode incarnation, stamped by the database clock.
--
-- Tenant isolation. None of the three tables is under row-level security.
-- ClaimDue, GiveBackIdle, the owner methods and the sweeps are cross-tenant
-- and run with no tenant set; no API reaches them. Every tenant-facing
-- statement filters on tenant_id. This corrects the note in 000004, which said
-- every write carried a tenant predicate: its Upsert and Delete matched on id
-- alone. Applied migrations are not edited, so the correction is made here.
--
-- Lock profile. ALTER TABLE takes ACCESS EXCLUSIVE on scheduled_tasks, and the
-- whole file runs as one implicit transaction, so readers and writers of
-- scheduled_tasks wait until the file commits: across the rewrite, the
-- backfill and the five index builds. No other table is locked. CONCURRENTLY
-- cannot run here (many statements), and a separate file would not help:
-- golang-migrate's advisory lock spans the whole Up() run (see 000013).
ALTER TABLE scheduled_tasks
    DROP COLUMN redispatch_after,
    DROP COLUMN attempt_count,
    ADD COLUMN arm_token         UUID    NOT NULL DEFAULT gen_random_uuid(),
    ADD COLUMN status            TEXT    NOT NULL DEFAULT 'WAITING',
    ADD COLUMN next_attempt_time BIGINT,
    ADD COLUMN attempts          INT     NOT NULL DEFAULT 0,
    ADD COLUMN lost_owners       INT     NOT NULL DEFAULT 0,
    ADD COLUMN last_attempt_time BIGINT,
    ADD COLUMN last_error        TEXT    NOT NULL DEFAULT '',
    ADD COLUMN failure_reason    TEXT    NOT NULL DEFAULT '',
    ADD COLUMN failed_time       BIGINT,
    ADD COLUMN partial_commit    BOOLEAN NOT NULL DEFAULT false,
    ADD COLUMN claim_token       UUID,
    ADD COLUMN claim_owner       UUID;

UPDATE scheduled_tasks SET next_attempt_time = scheduled_time;

-- The store always writes arm_token and status itself; the defaults above
-- exist only to backfill.
ALTER TABLE scheduled_tasks
    ALTER COLUMN next_attempt_time SET NOT NULL,
    ALTER COLUMN arm_token DROP DEFAULT,
    ALTER COLUMN status DROP DEFAULT,
    ADD CONSTRAINT scheduled_tasks_status_chk
        CHECK (status IN ('WAITING', 'RUNNING', 'FAILED')),
    ADD CONSTRAINT scheduled_tasks_claim_chk
        CHECK ((status = 'RUNNING') = (claim_token IS NOT NULL AND claim_owner IS NOT NULL)),
    ADD CONSTRAINT scheduled_tasks_failed_chk
        CHECK ((status = 'FAILED') = (failure_reason <> '')),
    -- A recorded error text is at most 1 024 bytes on every backend; the
    -- store refuses a longer one as a deterministic rejection (SQLSTATE 23514).
    ADD CONSTRAINT scheduled_tasks_last_error_len_chk
        CHECK (octet_length(last_error) <= 1024);

DROP INDEX IF EXISTS scheduled_tasks_due_idx;

-- ClaimDue: due WAITING tasks; RUNNING tasks by owner; at most one RUNNING
-- task per entity, against concurrent claimers too.
CREATE INDEX scheduled_tasks_waiting_due_idx
    ON scheduled_tasks (next_attempt_time) WHERE status = 'WAITING';
CREATE INDEX scheduled_tasks_running_owner_idx
    ON scheduled_tasks (claim_owner) WHERE status = 'RUNNING';
CREATE UNIQUE INDEX scheduled_tasks_one_running_per_entity_uq
    ON scheduled_tasks (tenant_id, entity_id) WHERE status = 'RUNNING';
-- Query pages in (scheduled_time, id) order; ids compare byte-wise.
CREATE INDEX scheduled_tasks_query_idx
    ON scheduled_tasks (tenant_id, scheduled_time, id COLLATE "C");
-- DeleteForModel. (tenant_id, entity_id) is scheduled_tasks_entity_idx (000004).
CREATE INDEX scheduled_tasks_model_idx
    ON scheduled_tasks (tenant_id, model_name, model_version);

CREATE TABLE scheduled_task_marks (
    task_id     TEXT NOT NULL,
    arm_token   UUID NOT NULL,
    claim_token UUID NOT NULL,
    PRIMARY KEY (task_id, arm_token)
);

CREATE TABLE scheduler_owners (
    owner        UUID        PRIMARY KEY,
    heartbeat_at TIMESTAMPTZ NOT NULL
);
```

`plugins/postgres/migrations/000014_scheduled_run_ownership.down.sql`:

```sql
-- Reverses 000014. FAILED tasks are removed first: the version-13 shape has no
-- status, so a kept FAILED task would be due again and could repeat work that
-- must not be repeated.
DELETE FROM scheduled_tasks WHERE status = 'FAILED';
DROP TABLE IF EXISTS scheduler_owners;
DROP TABLE IF EXISTS scheduled_task_marks;
DROP INDEX IF EXISTS scheduled_tasks_model_idx;
DROP INDEX IF EXISTS scheduled_tasks_query_idx;
DROP INDEX IF EXISTS scheduled_tasks_one_running_per_entity_uq;
DROP INDEX IF EXISTS scheduled_tasks_running_owner_idx;
DROP INDEX IF EXISTS scheduled_tasks_waiting_due_idx;
ALTER TABLE scheduled_tasks
    DROP CONSTRAINT scheduled_tasks_last_error_len_chk,
    DROP CONSTRAINT scheduled_tasks_failed_chk,
    DROP CONSTRAINT scheduled_tasks_claim_chk,
    DROP CONSTRAINT scheduled_tasks_status_chk,
    DROP COLUMN claim_owner,
    DROP COLUMN claim_token,
    DROP COLUMN partial_commit,
    DROP COLUMN failed_time,
    DROP COLUMN failure_reason,
    DROP COLUMN last_error,
    DROP COLUMN last_attempt_time,
    DROP COLUMN lost_owners,
    DROP COLUMN attempts,
    DROP COLUMN next_attempt_time,
    DROP COLUMN status,
    DROP COLUMN arm_token,
    ADD COLUMN redispatch_after BIGINT,
    ADD COLUMN attempt_count INT NOT NULL DEFAULT 0;
CREATE INDEX IF NOT EXISTS scheduled_tasks_due_idx ON scheduled_tasks (scheduled_time);
```

The index guard must reject this file until it is grandfathered. The package
does not build before Step 5, so run the guard right after Step 5, before
editing it:

```bash
cd /Users/paul/go-projects/cyoda-light/cyoda-go/.worktrees/598-scheduler-ownership/plugins/postgres && \
go test -run 'TestMigrations_IndexesOnExistingTablesAreConcurrent' .
```

Expected: FAIL with five violations
`000014_scheduled_run_ownership.up.sql: CREATE INDEX on "scheduled_tasks", which was created in an earlier migration`.
Then add to the `grandfathered` map in `migration_index_guard_test.go`, after
the `000013` entry (`:132`):

```go
		// scheduled_tasks' five new indexes, in a file that also alters the
		// table (drops two columns, adds twelve, backfills one, adds four
		// CHECK constraints) and creates two tables. Many statements under one
		// implicit transaction, so CONCURRENTLY cannot run here (clause (b));
		// a separate file would not help, because golang-migrate's advisory
		// lock spans the whole Up() run — the cycle proven for 000008.
		//
		// Lock profile, derived for this file: ALTER TABLE takes ACCESS
		// EXCLUSIVE on scheduled_tasks and holds it until the file commits,
		// across the table rewrite (the volatile arm_token default), the
		// backfill and the five index builds. Readers AND writers of
		// scheduled_tasks wait for that span; no other table is locked. The
		// table holds one row per armed timer.
		"000014_scheduled_run_ownership.up.sql": true,
```

- [ ] **Step 5: Write the store**

Replace `plugins/postgres/scheduled_task_store.go` with:

```go
package postgres

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgerrcode"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// scheduledTaskStore implements spi.ScheduledTaskStore on PostgreSQL.
//
// The SPI says which methods join the transaction on ctx; the querier follows:
//
//   - q joins it (ctxQuerier): ReconcileForEntity, RemoveLife, StampSegment,
//     DeleteForEntities, DeleteForModel, Fail, and Get. Task rows are written
//     straight into the open entity transaction, which runs at REPEATABLE
//     READ. A task row that another transaction changed after this one's
//     snapshot raises 40001, which the querier maps to spi.ErrConflict (C1,
//     C5). A row this transaction wrote stays locked until it ends, so
//     ClaimDue's SKIP LOCKED passes it over and MarkUnsafe's NOWAIT answers
//     ErrTaskBusy (C6).
//   - query never joins and runs on the main pool: Query.
//   - sched never joins and runs on the scheduler pool (READ COMMITTED,
//     lock_timeout 2s): ClaimDue, MarkUnsafe, RecordAttempt, GiveBackIdle,
//     RetireOwner, SweepOwners, SweepMarks.
//   - heartbeat never joins and has one connection of its own: Heartbeat.
//
// Tenant scoping. Every tenant-facing statement filters on tenant_id, and every
// joining method refuses a tenant that is not the transaction's (joinTenant). ClaimDue,
// GiveBackIdle, the owner methods and the sweeps are cross-tenant; no API
// reaches them. None of the tables is under row-level security (000014).
//
// A lock wait that reaches lock_timeout (55P03) or statement_timeout (57014)
// is returned as is: the scheduler retries both.
type scheduledTaskStore struct {
	q         Querier
	query     Querier
	sched     schedulerQuerier
	heartbeat schedulerQuerier
}

var _ spi.ScheduledTaskStore = (*scheduledTaskStore)(nil)

// taskColumns is every column scanTask reads, in order, on the alias st.
const taskColumns = `st.id, st.tenant_id, st.type, st.scheduled_time, st.timeout_ms, st.entity_id,
	st.model_name, st.model_version, st.transition, st.source_state, st.armed_at,
	st.armed_by_id, st.armed_by_kind, st.arm_token, st.status, st.next_attempt_time,
	st.attempts, st.lost_owners, st.last_attempt_time, st.last_error, st.failure_reason,
	st.failed_time, st.partial_commit, st.claim_token, st.claim_owner`

// markedColumn reports whether a mark exists for the row's current life.
const markedColumn = `EXISTS (SELECT 1 FROM scheduled_task_marks m
	WHERE m.task_id = st.id AND m.arm_token = st.arm_token)`

// scanTask reads taskColumns, then any extra destinations. scan is a pgx.Row's
// or pgx.Rows' Scan. scheduled_tasks_claim_chk guarantees claim_token and
// claim_owner are both set or both NULL.
func scanTask(scan func(dest ...any) error, extra ...any) (spi.ScheduledTask, error) {
	var (
		t                                  spi.ScheduledTask
		tenantID, taskType, status, reason string
		armedByID, armedByKind             string
		claimToken, claimOwner             *uuid.UUID
	)
	dest := append([]any{
		&t.ID, &tenantID, &taskType, &t.ScheduledTime, &t.TimeoutMs, &t.EntityID,
		&t.ModelName, &t.ModelVersion, &t.Transition, &t.SourceState, &t.ArmedAt,
		&armedByID, &armedByKind, &t.ArmToken, &status, &t.NextAttemptTime,
		&t.Attempts, &t.LostOwners, &t.LastAttemptTime, &t.LastError, &reason,
		&t.FailedTime, &t.PartialCommit, &claimToken, &claimOwner,
	}, extra...)
	if err := scan(dest...); err != nil {
		return spi.ScheduledTask{}, err
	}
	t.TenantID = spi.TenantID(tenantID)
	t.Type = spi.ScheduledTaskType(taskType)
	t.ArmedBy = spi.Principal{ID: armedByID, Kind: spi.PrincipalKind(armedByKind)}
	t.Status = spi.ScheduledTaskStatus(status)
	t.FailureReason = spi.ScheduledTaskFailureReason(reason)
	if claimToken != nil {
		t.Claim = &spi.TaskClaim{Token: *claimToken, Owner: *claimOwner}
	}
	return t, nil
}

func scanTasks(rows pgx.Rows) ([]spi.ScheduledTask, error) {
	defer rows.Close()
	var out []spi.ScheduledTask
	for rows.Next() {
		t, err := scanTask(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func queryIDs(ctx context.Context, q Querier, sql string, args ...any) ([]string, error) {
	rows, err := q.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func staleClaim(verb, id string) error {
	return fmt.Errorf("%s scheduled task %s: %w", verb, id, spi.ErrStaleClaim)
}

// joinTenant refuses a joining write whose tenant is not the tenant of the
// transaction on ctx, before any statement runs: a task row of tenant B never
// enters tenant A's transaction. Memory and SQLite refuse the same way.
// Without a transaction on ctx there is nothing to compare.
func joinTenant(ctx context.Context, tenant spi.TenantID) error {
	if tx := spi.GetTransaction(ctx); tx != nil && tx.TenantID != tenant {
		return fmt.Errorf("scheduledTaskStore: %w (txID=%s)", spi.ErrTxTenantMismatch, tx.ID)
	}
	return nil
}

// armTaskSQL arms one task as a new life: a new arm token, WAITING, due at its
// scheduled time, every counter and record cleared, no claim. The WHERE keeps
// a colliding id of another tenant untouched; RETURNING then yields no row.
const armTaskSQL = `INSERT INTO scheduled_tasks AS st (
	id, tenant_id, type, scheduled_time, timeout_ms, entity_id, model_name, model_version,
	transition, source_state, armed_at, armed_by_id, armed_by_kind,
	arm_token, status, next_attempt_time)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, gen_random_uuid(), 'WAITING', $4)
ON CONFLICT (id) DO UPDATE SET
	type = excluded.type, scheduled_time = excluded.scheduled_time, timeout_ms = excluded.timeout_ms,
	entity_id = excluded.entity_id, model_name = excluded.model_name,
	model_version = excluded.model_version, transition = excluded.transition,
	source_state = excluded.source_state, armed_at = excluded.armed_at,
	armed_by_id = excluded.armed_by_id, armed_by_kind = excluded.armed_by_kind,
	arm_token = excluded.arm_token, status = 'WAITING', next_attempt_time = excluded.next_attempt_time,
	attempts = 0, lost_owners = 0, last_attempt_time = NULL, last_error = '', failure_reason = '',
	failed_time = NULL, partial_commit = false, claim_token = NULL, claim_owner = NULL
WHERE st.tenant_id = excluded.tenant_id
RETURNING st.id`

// ReconcileForEntity arms req.Arm, each as a new life, removes req.Cancel,
// then removes every other task of the entity and returns those. The tenant
// and entity come from req, never from the task structs.
func (s *scheduledTaskStore) ReconcileForEntity(ctx context.Context, req spi.ReconcileRequest) ([]spi.ScheduledTask, error) {
	if err := joinTenant(ctx, req.TenantID); err != nil {
		return nil, err
	}
	armIDs := make([]string, 0, len(req.Arm))
	for _, t := range req.Arm {
		var id string
		err := s.q.QueryRow(ctx, armTaskSQL,
			t.ID, string(req.TenantID), string(t.Type), t.ScheduledTime, t.TimeoutMs, req.EntityID,
			t.ModelName, t.ModelVersion, t.Transition, t.SourceState, t.ArmedAt,
			t.ArmedBy.ID, string(t.ArmedBy.Kind)).Scan(&id)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("arm scheduled task %s: id conflict: %w", t.ID, spi.ErrStoreRejected)
		}
		if err != nil {
			return nil, fmt.Errorf("failed to arm scheduled task %s: %w", t.ID, err)
		}
		armIDs = append(armIDs, t.ID)
	}
	if len(req.Cancel) > 0 {
		if _, err := s.q.Exec(ctx,
			`DELETE FROM scheduled_tasks WHERE tenant_id = $1 AND entity_id = $2 AND id = ANY($3::text[])`,
			string(req.TenantID), req.EntityID, req.Cancel); err != nil {
			return nil, fmt.Errorf("failed to cancel scheduled tasks of %s: %w", req.EntityID, err)
		}
	}
	rows, err := s.q.Query(ctx, `DELETE FROM scheduled_tasks st
		WHERE st.tenant_id = $1 AND st.entity_id = $2 AND NOT (st.id = ANY($3::text[]))
		RETURNING `+taskColumns, string(req.TenantID), req.EntityID, armIDs)
	if err != nil {
		return nil, fmt.Errorf("failed to remove scheduled tasks of %s: %w", req.EntityID, err)
	}
	removed, err := scanTasks(rows)
	if err != nil {
		return nil, fmt.Errorf("failed to remove scheduled tasks of %s: %w", req.EntityID, err)
	}
	return removed, nil
}

func (s *scheduledTaskStore) RemoveLife(ctx context.Context, tenant spi.TenantID, id string, armToken uuid.UUID) error {
	if err := joinTenant(ctx, tenant); err != nil {
		return err
	}
	if _, err := s.q.Exec(ctx,
		`DELETE FROM scheduled_tasks WHERE tenant_id = $1 AND id = $2 AND arm_token = $3`,
		string(tenant), id, armToken); err != nil {
		return fmt.Errorf("failed to remove scheduled task %s: %w", id, err)
	}
	return nil
}

// StampSegment always writes the row, partial or not: the write is what puts
// the segment's commit under first-committer-wins on this row (C1).
func (s *scheduledTaskStore) StampSegment(ctx context.Context, ref spi.TaskRef, partial bool) error {
	if err := joinTenant(ctx, ref.TenantID); err != nil {
		return err
	}
	tag, err := s.q.Exec(ctx, `UPDATE scheduled_tasks SET partial_commit = partial_commit OR $5
		WHERE tenant_id = $1 AND id = $2 AND arm_token = $3 AND claim_token = $4 AND status = 'RUNNING'`,
		string(ref.TenantID), ref.ID, ref.ArmToken, ref.ClaimToken, partial)
	if err != nil {
		return fmt.Errorf("failed to stamp scheduled task %s: %w", ref.ID, err)
	}
	if tag.RowsAffected() == 0 {
		return staleClaim("stamp", ref.ID)
	}
	return nil
}

func (s *scheduledTaskStore) DeleteForEntities(ctx context.Context, tenant spi.TenantID, entityIDs []string) error {
	if err := joinTenant(ctx, tenant); err != nil {
		return err
	}
	if len(entityIDs) == 0 {
		return nil
	}
	if _, err := s.q.Exec(ctx,
		`DELETE FROM scheduled_tasks WHERE tenant_id = $1 AND entity_id = ANY($2::text[])`,
		string(tenant), entityIDs); err != nil {
		return fmt.Errorf("failed to remove scheduled tasks of %d entities: %w", len(entityIDs), err)
	}
	return nil
}

// DeleteForModel removes the model's tasks whose (source state, transition)
// keep does not retain. A nil keep retains nothing.
func (s *scheduledTaskStore) DeleteForModel(ctx context.Context, tenant spi.TenantID, modelName string, modelVersion int,
	keep func(sourceState, transition string) bool) error {
	if err := joinTenant(ctx, tenant); err != nil {
		return err
	}
	rows, err := s.q.Query(ctx, `SELECT DISTINCT source_state, transition FROM scheduled_tasks
		WHERE tenant_id = $1 AND model_name = $2 AND model_version = $3`,
		string(tenant), modelName, modelVersion)
	if err != nil {
		return fmt.Errorf("failed to read scheduled tasks of model %s/%d: %w", modelName, modelVersion, err)
	}
	var states, transitions []string
	for rows.Next() {
		var state, transition string
		if err := rows.Scan(&state, &transition); err != nil {
			rows.Close()
			return fmt.Errorf("failed to read scheduled tasks of model %s/%d: %w", modelName, modelVersion, err)
		}
		if keep != nil && keep(state, transition) {
			continue
		}
		states = append(states, state)
		transitions = append(transitions, transition)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("failed to read scheduled tasks of model %s/%d: %w", modelName, modelVersion, err)
	}
	if len(states) == 0 {
		return nil
	}
	if _, err := s.q.Exec(ctx, `DELETE FROM scheduled_tasks st
		USING unnest($4::text[], $5::text[]) AS gone(source_state, transition)
		WHERE st.tenant_id = $1 AND st.model_name = $2 AND st.model_version = $3
		  AND st.source_state = gone.source_state AND st.transition = gone.transition`,
		string(tenant), modelName, modelVersion, states, transitions); err != nil {
		return fmt.Errorf("failed to remove scheduled tasks of model %s/%d: %w", modelName, modelVersion, err)
	}
	return nil
}

// Get joins the transaction on ctx when there is one; a caller that must not
// join passes a context without a transaction.
func (s *scheduledTaskStore) Get(ctx context.Context, tenant spi.TenantID, id string) (*spi.ScheduledTask, bool, error) {
	var marked bool
	t, err := scanTask(s.q.QueryRow(ctx, `SELECT `+taskColumns+`, `+markedColumn+`
		FROM scheduled_tasks st WHERE st.tenant_id = $1 AND st.id = $2`, string(tenant), id).Scan, &marked)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("failed to get scheduled task %s: %w", id, err)
	}
	t.UnsafeMarked = marked
	return &t, true, nil
}

// Query pages the tenant's tasks in (scheduled_time, id) order, ids compared
// byte-wise. It reads one row past the page to know whether another follows.
func (s *scheduledTaskStore) Query(ctx context.Context, tenant spi.TenantID, q spi.ScheduledTaskQuery) (spi.ScheduledTaskPage, error) {
	statuses := make([]string, 0, len(q.Statuses))
	for _, st := range q.Statuses {
		statuses = append(statuses, string(st))
	}
	var afterTime *int64
	afterID := ""
	if q.After != nil {
		afterTime, afterID = &q.After.ScheduledTime, q.After.ID
	}
	rows, err := s.query.Query(ctx, `SELECT `+taskColumns+`, `+markedColumn+`
		  FROM scheduled_tasks st
		 WHERE st.tenant_id = $1
		   AND (cardinality($2::text[]) = 0 OR st.status = ANY($2::text[]))
		   AND ($3::text = '' OR st.model_name = $3)
		   AND ($4::int = 0 OR st.model_version = $4)
		   AND ($5::text = '' OR st.entity_id = $5)
		   AND ($6::bigint IS NULL OR (st.scheduled_time, st.id COLLATE "C") > ($6, $7::text COLLATE "C"))
		 ORDER BY st.scheduled_time, st.id COLLATE "C"
		 LIMIT $8`,
		string(tenant), statuses, q.ModelName, q.ModelVersion, q.EntityID, afterTime, afterID, q.Limit+1)
	if err != nil {
		return spi.ScheduledTaskPage{}, fmt.Errorf("failed to query scheduled tasks: %w", err)
	}
	defer rows.Close()
	items := make([]spi.ScheduledTask, 0, q.Limit)
	for rows.Next() {
		var marked bool
		t, err := scanTask(rows.Scan, &marked)
		if err != nil {
			return spi.ScheduledTaskPage{}, fmt.Errorf("failed to query scheduled tasks: %w", err)
		}
		t.UnsafeMarked = marked
		items = append(items, t)
	}
	if err := rows.Err(); err != nil {
		return spi.ScheduledTaskPage{}, fmt.Errorf("failed to query scheduled tasks: %w", err)
	}
	page := spi.ScheduledTaskPage{Items: items}
	if len(items) > q.Limit {
		page.Items = items[:q.Limit]
		last := page.Items[q.Limit-1]
		page.Next = &spi.ScheduledTaskCursor{ScheduledTime: last.ScheduledTime, ID: last.ID}
	}
	return page, nil
}

// claimableCondition is the row's own claim condition, on alias st, with
// $1 = NowMs, $2 = AllowLostOwner, $3 = StaleAfter in microseconds. A RUNNING
// row qualifies when its owner's liveness record is missing or older than
// StaleAfter by the database clock.
const claimableCondition = `(
	(st.status = 'WAITING' AND st.next_attempt_time <= $1)
	OR ($2::boolean AND st.status = 'RUNNING'
	    AND NOT EXISTS (SELECT 1 FROM scheduler_owners o
	                     WHERE o.owner = st.claim_owner
	                       AND o.heartbeat_at >= now() - ($3::bigint * interval '1 microsecond'))))`

// lockClaimableSQL is ClaimDue's steps 1 and 2. The ranking sits in a CTE,
// because FOR UPDATE cannot share a query level with a window function:
// candidates are due WAITING tasks and, with AllowLostOwner, RUNNING tasks of
// a stale or missing owner; an entity with another RUNNING task is excluded;
// DISTINCT ON keeps one task per entity; row_number() gives each tenant its
// turns, and a tenant's turns are capped at PerTenantLimit minus its runs in
// progress ($4, $5, $6). Tenants take turns; within a tenant, due order. The
// outer SELECT locks the ranked rows in id order, skipping any row another
// transaction holds (C6), and repeats the claim condition. It returns each
// locked row's status before the claim: RUNNING means the claim takes the task
// from a stale or missing owner (ClaimedFromLostOwner). The row lock holds that
// status until claimSQL runs.
const lockClaimableSQL = `WITH due AS (
	SELECT st.id, st.tenant_id, st.entity_id, st.next_attempt_time
	  FROM scheduled_tasks st
	 WHERE st.status = 'WAITING' AND st.next_attempt_time <= $1
	UNION ALL
	SELECT st.id, st.tenant_id, st.entity_id, st.next_attempt_time
	  FROM scheduled_tasks st
	 WHERE $2::boolean AND st.status = 'RUNNING'
	   AND NOT EXISTS (SELECT 1 FROM scheduler_owners o
	                    WHERE o.owner = st.claim_owner
	                      AND o.heartbeat_at >= now() - ($3::bigint * interval '1 microsecond'))
), free AS (
	SELECT d.id, d.tenant_id, d.entity_id, d.next_attempt_time
	  FROM due d
	 WHERE NOT EXISTS (SELECT 1 FROM scheduled_tasks r
	                    WHERE r.tenant_id = d.tenant_id AND r.entity_id = d.entity_id
	                      AND r.status = 'RUNNING' AND r.id <> d.id)
), one_per_entity AS (
	SELECT DISTINCT ON (tenant_id, entity_id) id, tenant_id, next_attempt_time
	  FROM free
	 ORDER BY tenant_id, entity_id, next_attempt_time, id
), ranked AS (
	SELECT id, tenant_id, next_attempt_time,
	       row_number() OVER (PARTITION BY tenant_id ORDER BY next_attempt_time, id) AS turn
	  FROM one_per_entity
), chosen AS (
	SELECT r.id
	  FROM ranked r
	  LEFT JOIN unnest($5::text[], $6::int[]) AS busy(tenant_id, runs) ON busy.tenant_id = r.tenant_id
	 WHERE r.turn <= $4::int - COALESCE(busy.runs, 0)
	 ORDER BY r.turn, r.next_attempt_time, r.id
	 LIMIT $7
)
SELECT st.id, st.status
  FROM scheduled_tasks st
 WHERE st.id IN (SELECT id FROM chosen)
   AND ` + claimableCondition + `
 ORDER BY st.id
 FOR UPDATE OF st SKIP LOCKED`

// claimSQL is ClaimDue's step 3. A new statement, so it sees claims other
// pnodes committed after step 1; the full condition, including "no other
// RUNNING task of the entity", closes that race. A concurrent claim of a
// sibling that has not committed yet meets this one at the unique index.
const claimSQL = `UPDATE scheduled_tasks st
   SET status      = 'RUNNING',
       claim_token = gen_random_uuid(),
       claim_owner = $5,
       lost_owners = st.lost_owners + CASE WHEN st.status = 'RUNNING' THEN 1 ELSE 0 END
 WHERE st.id = ANY($4::text[])
   AND ` + claimableCondition + `
   AND NOT EXISTS (SELECT 1 FROM scheduled_tasks r
                    WHERE r.tenant_id = st.tenant_id AND r.entity_id = st.entity_id
                      AND r.status = 'RUNNING' AND r.id <> st.id)
RETURNING ` + taskColumns

// claimedMarksSQL is ClaimDue's step 4, read while the row locks are held (C3).
const claimedMarksSQL = `SELECT m.task_id
  FROM scheduled_task_marks m
  JOIN unnest($1::text[], $2::uuid[]) AS c(task_id, arm_token)
    ON m.task_id = c.task_id AND m.arm_token = c.arm_token`

// ClaimDue claims due tasks in one READ COMMITTED transaction on the scheduler
// pool. A claim that loses a race for a sibling task rolls back and claims
// nothing (see lostClaimRace); that is logged at DEBUG, not returned.
func (s *scheduledTaskStore) ClaimDue(ctx context.Context, req spi.ClaimRequest) ([]spi.ScheduledTask, error) {
	if req.Limit < 1 || req.PerTenantLimit < 1 {
		return nil, fmt.Errorf("claim scheduled tasks: Limit and PerTenantLimit must be >= 1, got %d and %d",
			req.Limit, req.PerTenantLimit)
	}
	claimed, err := s.claimDue(ctx, req)
	if lostClaimRace(err) {
		slog.Debug("scheduled task claim met a concurrent claim of a sibling task; claiming nothing this tick",
			"pkg", "postgres", "err", err)
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to claim scheduled tasks: %w", err)
	}
	return claimed, nil
}

func (s *scheduledTaskStore) claimDue(ctx context.Context, req spi.ClaimRequest) ([]spi.ScheduledTask, error) {
	tenants := make([]string, 0, len(req.TenantInProgress))
	running := make([]int, 0, len(req.TenantInProgress))
	for tenant, n := range req.TenantInProgress {
		tenants = append(tenants, string(tenant))
		running = append(running, n)
	}
	stale := req.StaleAfter.Microseconds()

	tx, err := s.sched.begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	q := classifiedQuerier{inner: tx}

	lockRows, err := q.Query(ctx, lockClaimableSQL,
		req.NowMs, req.AllowLostOwner, stale, req.PerTenantLimit, tenants, running, req.Limit)
	if err != nil {
		return nil, err
	}
	var locked []string
	fromLostOwner := make(map[string]bool)
	for lockRows.Next() {
		var id, status string
		if err := lockRows.Scan(&id, &status); err != nil {
			lockRows.Close()
			return nil, err
		}
		locked = append(locked, id)
		fromLostOwner[id] = status == string(spi.ScheduledTaskRunning)
	}
	lockRows.Close()
	if err := lockRows.Err(); err != nil || len(locked) == 0 {
		return nil, err
	}
	rows, err := q.Query(ctx, claimSQL, req.NowMs, req.AllowLostOwner, stale, locked, req.Owner)
	if err != nil {
		return nil, err
	}
	claimed, err := scanTasks(rows)
	if err != nil || len(claimed) == 0 {
		return nil, err
	}
	for i := range claimed {
		claimed[i].ClaimedFromLostOwner = fromLostOwner[claimed[i].ID]
	}

	ids := make([]string, len(claimed))
	arms := make([]uuid.UUID, len(claimed))
	at := make(map[string]int, len(claimed))
	for i, t := range claimed {
		ids[i], arms[i], at[t.ID] = t.ID, t.ArmToken, i
	}
	marked, err := queryIDs(ctx, q, claimedMarksSQL, ids, arms)
	if err != nil {
		return nil, err
	}
	for _, id := range marked {
		claimed[at[id]].UnsafeMarked = true
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, classifyError(err)
	}
	return claimed, nil
}

// lostClaimRace reports a claim that met a concurrent claim of a sibling task
// of the same entity (spec §10.2, V5): the one-RUNNING-task-per-entity index
// refused it after the rival committed (23505), the rival held its index
// entry past lock_timeout (55P03), or two claims waited on each other's index
// entries (40P01). The transaction has rolled back; nothing was claimed.
func lostClaimRace(err error) bool {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return false
	}
	switch pgErr.Code {
	case pgerrcode.DeadlockDetected, pgerrcode.LockNotAvailable:
		return true
	case pgerrcode.UniqueViolation:
		return pgErr.ConstraintName == "scheduled_tasks_one_running_per_entity_uq"
	}
	return false
}

func isLockNotAvailable(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == pgerrcode.LockNotAvailable
}

// MarkUnsafe records, before an unsafe dispatch, that this claim of this life
// is about to hand work off. One READ COMMITTED transaction on the scheduler
// pool, never the caller's, so the mark survives the run's rollback.
//
// Step 1 share-locks the task row without waiting: no row means the claim is
// stale; 55P03 means another transaction holds the row (C6), or a claim is in
// progress on it. Either way the mark and a claim serialise (C3): a claim that
// committed first changed the tokens; a claim in progress holds the row; a
// mark that holds its share lock first makes ClaimDue skip the row, and the
// next scan sees the mark.
func (s *scheduledTaskStore) MarkUnsafe(ctx context.Context, ref spi.TaskRef) error {
	tx, err := s.sched.begin(ctx)
	if err != nil {
		return fmt.Errorf("failed to mark scheduled task %s: %w", ref.ID, err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	q := classifiedQuerier{inner: tx}

	var one int
	err = q.QueryRow(ctx, `SELECT 1 FROM scheduled_tasks
		WHERE tenant_id = $1 AND id = $2 AND arm_token = $3 AND claim_token = $4 AND status = 'RUNNING'
		FOR SHARE NOWAIT`, string(ref.TenantID), ref.ID, ref.ArmToken, ref.ClaimToken).Scan(&one)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return staleClaim("mark", ref.ID)
	case isLockNotAvailable(err):
		return fmt.Errorf("mark scheduled task %s: %w: %w", ref.ID, spi.ErrTaskBusy, err)
	case err != nil:
		return fmt.Errorf("failed to mark scheduled task %s: %w", ref.ID, err)
	}

	var holder uuid.UUID
	err = q.QueryRow(ctx, `INSERT INTO scheduled_task_marks (task_id, arm_token, claim_token)
		VALUES ($1, $2, $3) ON CONFLICT (task_id, arm_token) DO NOTHING RETURNING claim_token`,
		ref.ID, ref.ArmToken, ref.ClaimToken).Scan(&holder)
	if errors.Is(err, pgx.ErrNoRows) {
		err = q.QueryRow(ctx, `SELECT claim_token FROM scheduled_task_marks WHERE task_id = $1 AND arm_token = $2`,
			ref.ID, ref.ArmToken).Scan(&holder)
	}
	if err != nil {
		return fmt.Errorf("failed to mark scheduled task %s: %w", ref.ID, err)
	}
	if holder != ref.ClaimToken {
		return fmt.Errorf("mark scheduled task %s: %w", ref.ID, spi.ErrMarkedByAnotherClaim)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("failed to commit the mark of scheduled task %s: %w", ref.ID, classifyError(err))
	}
	return nil
}

// recordAttemptSQL sets the task WAITING, counts the attempt unless $5 = 0,
// records the error, clears the claim, and — with $9 — removes this claim's
// mark, all in one statement.
const recordAttemptSQL = `WITH attempt AS (
	UPDATE scheduled_tasks
	   SET status = 'WAITING', attempts = attempts + $5, last_error = $6,
	       last_attempt_time = $7, next_attempt_time = $8,
	       claim_token = NULL, claim_owner = NULL
	 WHERE tenant_id = $1 AND id = $2 AND arm_token = $3 AND claim_token = $4 AND status = 'RUNNING'
	RETURNING id, arm_token
), cleared AS (
	DELETE FROM scheduled_task_marks m
	 USING attempt a
	 WHERE $9::boolean AND m.task_id = a.id AND m.arm_token = a.arm_token AND m.claim_token = $4
)
SELECT count(*) FROM attempt`

func (s *scheduledTaskStore) RecordAttempt(ctx context.Context, ref spi.TaskRef, a spi.Attempt) error {
	counted := 1
	if a.NotCounted {
		counted = 0
	}
	var n int
	if err := s.sched.QueryRow(ctx, recordAttemptSQL,
		string(ref.TenantID), ref.ID, ref.ArmToken, ref.ClaimToken,
		counted, a.Error, a.AtMs, a.NextAttemptTime, a.ClearOwnMark).Scan(&n); err != nil {
		return fmt.Errorf("failed to record an attempt of scheduled task %s: %w", ref.ID, err)
	}
	if n == 0 {
		return staleClaim("record an attempt of", ref.ID)
	}
	return nil
}

// Fail joins the transaction on ctx, so the FAILED status and its audit event
// commit together (spec §5.7). The mark, if any, stays with the life.
func (s *scheduledTaskStore) Fail(ctx context.Context, ref spi.TaskRef, f spi.Failure) error {
	if err := joinTenant(ctx, ref.TenantID); err != nil {
		return err
	}
	tag, err := s.q.Exec(ctx, `UPDATE scheduled_tasks
		   SET status = 'FAILED', failure_reason = $5, last_error = $6, failed_time = $7,
		       claim_token = NULL, claim_owner = NULL
		 WHERE tenant_id = $1 AND id = $2 AND arm_token = $3 AND claim_token = $4 AND status = 'RUNNING'`,
		string(ref.TenantID), ref.ID, ref.ArmToken, ref.ClaimToken, string(f.Reason), f.Error, f.AtMs)
	if err != nil {
		return fmt.Errorf("failed to fail scheduled task %s: %w", ref.ID, err)
	}
	if tag.RowsAffected() == 0 {
		return staleClaim("fail", ref.ID)
	}
	return nil
}

// GiveBackIdle returns to WAITING, uncounted, every task owner holds RUNNING
// whose claim is not in keep. Zero rows is its normal result.
func (s *scheduledTaskStore) GiveBackIdle(ctx context.Context, owner uuid.UUID, keep []uuid.UUID) (int, error) {
	live := append(make([]uuid.UUID, 0, len(keep)), keep...) // never NULL: <> ALL(NULL) matches nothing
	tag, err := s.sched.Exec(ctx, `UPDATE scheduled_tasks
		   SET status = 'WAITING', claim_token = NULL, claim_owner = NULL
		 WHERE status = 'RUNNING' AND claim_owner = $1 AND claim_token <> ALL($2::uuid[])`, owner, live)
	if err != nil {
		return 0, fmt.Errorf("failed to give back idle scheduled tasks: %w", err)
	}
	return int(tag.RowsAffected()), nil
}

// Heartbeat stamps the owner's liveness record with the database clock. It is
// an upsert, so a record swept during a long outage comes back.
func (s *scheduledTaskStore) Heartbeat(ctx context.Context, owner uuid.UUID) error {
	if _, err := s.heartbeat.Exec(ctx, `INSERT INTO scheduler_owners (owner, heartbeat_at) VALUES ($1, now())
		ON CONFLICT (owner) DO UPDATE SET heartbeat_at = now()`, owner); err != nil {
		return fmt.Errorf("failed to record the scheduler heartbeat: %w", err)
	}
	return nil
}

func (s *scheduledTaskStore) RetireOwner(ctx context.Context, owner uuid.UUID) error {
	if _, err := s.sched.Exec(ctx, `DELETE FROM scheduler_owners WHERE owner = $1`, owner); err != nil {
		return fmt.Errorf("failed to retire the scheduler owner: %w", err)
	}
	return nil
}

// SweepOwners removes liveness records older than deadFor that no RUNNING task
// references.
func (s *scheduledTaskStore) SweepOwners(ctx context.Context, deadFor time.Duration) error {
	if _, err := s.sched.Exec(ctx, `DELETE FROM scheduler_owners o
		WHERE o.heartbeat_at < now() - ($1::bigint * interval '1 microsecond')
		  AND NOT EXISTS (SELECT 1 FROM scheduled_tasks st
		                   WHERE st.status = 'RUNNING' AND st.claim_owner = o.owner)`,
		deadFor.Microseconds()); err != nil {
		return fmt.Errorf("failed to sweep scheduler owners: %w", err)
	}
	return nil
}

// SweepMarks removes the marks of ended lives: no task row carries their
// (task id, arm token) any more.
func (s *scheduledTaskStore) SweepMarks(ctx context.Context) error {
	if _, err := s.sched.Exec(ctx, `DELETE FROM scheduled_task_marks m
		WHERE NOT EXISTS (SELECT 1 FROM scheduled_tasks st
		                   WHERE st.id = m.task_id AND st.arm_token = m.arm_token)`); err != nil {
		return fmt.Errorf("failed to sweep scheduled task marks: %w", err)
	}
	return nil
}
```

`plugins/postgres/store_factory.go` — replace the `ScheduledTaskStore` godoc
and body (`:270-277`):

```go
// ScheduledTaskStore returns the scheduled-task store. It resolves no tenant:
// tenant-facing methods take the tenant as an argument, and ClaimDue,
// GiveBackIdle, the owner methods and the sweeps are cross-tenant. See
// scheduledTaskStore for which methods join the transaction on ctx.
func (f *StoreFactory) ScheduledTaskStore(_ context.Context) (spi.ScheduledTaskStore, error) {
	return &scheduledTaskStore{
		q:         f.querier(),
		query:     unjoinedQuerier{pool: f.pool, acquireTimeout: f.cfg.AcquireTimeout, what: "scheduled task query"},
		sched:     f.schedulerQuerier("scheduled task"),
		heartbeat: f.heartbeatQuerier(),
	}, nil
}
```

`plugins/postgres/rls_test.go` — replace the comment and the exempt entry at
`:49-59` with:

```go
	// One table is deliberately not enrolled: scheduled_tasks is read and
	// written by ClaimDue, GiveBackIdle and the sweeps, which are cross-tenant
	// and run with no tenant set, so scoping it to one tenant would break the
	// scheduler the moment enforcement is strengthened (FORCE + a non-owner
	// role). TestPostgres_ScheduledTaskTables_NotRLSEnrolled pins it from the
	// other side; the two tests must agree.
	exempt := map[string]string{
		"scheduled_tasks": "the scheduler's claim and sweeps are cross-tenant",
	}
```

- [ ] **Step 6: Run the tests to verify they pass**

```bash
cd /Users/paul/go-projects/cyoda-light/cyoda-go/.worktrees/598-scheduler-ownership/plugins/postgres && \
go test -run 'TestMigration14_|TestMigrations_|TestPostgres_ScheduledTask|TestPostgres_MarkUnsafe_|TestPostgres_ClaimDue_|TestLostClaimRace|TestRLS_' .
```

Expected: `ok` (about 15 s: two lock_timeout waits and the 4 s rival).

```bash
cd /Users/paul/go-projects/cyoda-light/cyoda-go/.worktrees/598-scheduler-ownership/plugins/postgres && \
go test -run 'TestConformance/ScheduledTasks' .
```

Expected: every subtest passes, `Claim/LostOwnerFlagged` and
`Tenant/JoiningWriteOtherTenantRefused` included, except the case(s) that
assert `spi.ErrStoreRejected`, which fail with the raw SQLSTATE class-22/23
error (the 1 025-byte text meets `scheduled_tasks_last_error_len_chk`, class 23).
BP-5 turns them green. Any other failure is a defect in this task: fix it
here.

- [ ] **Step 7: Plugin exit check (spec §15)**

```bash
cd /Users/paul/go-projects/cyoda-light/cyoda-go/.worktrees/598-scheduler-ownership && \
git grep -n -e RedispatchAfter -e MarkRedispatch -e AttemptCount -e ScanDue -e redispatch_after \
  -e attempt_count -e '\.Upsert(ctx, task' -e 'sts\.Delete(' -- plugins/postgres ':!*/migrations/*' \
  ':!*migration*_test.go'
```

Expected: no output. The migration tests (`scheduled_task_migration_test.go`)
must name the old columns to build version-13 rows, so the check excludes
them, as spec §15 does.

- [ ] **Step 8: Commit**

```bash
cd /Users/paul/go-projects/cyoda-light/cyoda-go/.worktrees/598-scheduler-ownership && \
git status --short go.work && \
git rm plugins/postgres/scheduled_task_attribution_internal_test.go && \
git add plugins/postgres/migrations/000014_scheduled_run_ownership.up.sql \
  plugins/postgres/migrations/000014_scheduled_run_ownership.down.sql \
  plugins/postgres/scheduled_task_store.go plugins/postgres/store_factory.go \
  plugins/postgres/migration_index_guard_test.go plugins/postgres/rls_test.go \
  plugins/postgres/scheduled_task_store_test.go plugins/postgres/scheduled_task_migration_test.go \
  plugins/postgres/scheduled_task_claim_race_internal_test.go && \
git diff --cached --name-only | grep -c go.work; \
git commit -m "feat(postgres): scheduled tasks are claimed, fenced and marked

Migration 000014: arm/claim tokens, status, retry record, a 1024-byte
limit on last_error, one RUNNING task per entity, marks and owner
liveness tables. The store implements
the new ScheduledTaskStore: joining writes in the entity transaction,
never-joining writes on the scheduler pool, ClaimDue in four steps with
SKIP LOCKED, MarkUnsafe with FOR SHARE NOWAIT. A claim that loses a
sibling race rolls back and claims nothing. A claim from a lost owner is
flagged on the result; a joining write of another tenant than the
transaction's is refused.

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

Expected: `git status` shows ` M go.work`; the `grep -c` prints `0`.

---

### Task BP-5: Deterministic rejections carry `spi.ErrStoreRejected`

**Spec:** §5.6 "PostgreSQL sets it for SQLSTATE classes 22, 23 and 42"; §10.1
"New errors … `ErrStoreRejected`"; §13 row "`spi.ErrStoreRejected` → ERROR
with ticket, node latched (every backend sets the marker)" (the S cell on PG).

**Files:**
- Modify: `plugins/postgres/transaction_manager.go` (`classifySQLState` `:918-944`; `classifyError` godoc `:876-895`)
- Test: `plugins/postgres/store_rejected_test.go` (new, package `postgres`)
- Modify: `plugins/postgres/scheduled_task_store_test.go` (append one test)

**Interfaces:**
- Consumes: `spi.ErrStoreRejected` (S).
- Produces: every PostgreSQL statement error of SQLSTATE class 22, 23 or 42
  satisfies `errors.Is(err, spi.ErrStoreRejected)`, in every store of the
  plugin, joined or not; the `*pgconn.PgError` stays in the chain. The
  `unique_claims_uq` branch keeps its own sentinel, `spi.ErrUniqueViolation`,
  unchanged.

- [ ] **Step 1: Write the failing tests**

`plugins/postgres/store_rejected_test.go`:

```go
package postgres

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

func TestClassifyError_DeterministicRejectionsCarryErrStoreRejected(t *testing.T) {
	for _, code := range []string{"22021", "22001", "22P02", "23502", "23505", "23514", "42P01", "42703"} {
		err := classifyError(fmt.Errorf("statement: %w", &pgconn.PgError{Code: code}))
		if !errors.Is(err, spi.ErrStoreRejected) {
			t.Errorf("SQLSTATE %s: %v does not carry ErrStoreRejected", code, err)
		}
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != code {
			t.Errorf("SQLSTATE %s: the PgError left the chain: %v", code, err)
		}
	}
	for _, code := range []string{"40001", "40P01", "55P03", "57014", "25P03", "08006", "53300"} {
		if err := classifyError(&pgconn.PgError{Code: code}); errors.Is(err, spi.ErrStoreRejected) {
			t.Errorf("SQLSTATE %s is retryable but carries ErrStoreRejected: %v", code, err)
		}
	}
	twice := classifyError(classifyError(&pgconn.PgError{Code: "22021"}))
	if n := strings.Count(twice.Error(), spi.ErrStoreRejected.Error()); n != 1 {
		t.Errorf("classifying twice marked %d times: %v", n, twice)
	}
}
```

Append to `plugins/postgres/scheduled_task_store_test.go`:

```go
// A write the database refuses deterministically is marked, so the scheduler
// latches instead of retrying it forever. The task is left unchanged.
func TestPostgres_ScheduledTaskStore_DeterministicRejectionIsMarked(t *testing.T) {
	_, sts := newTaskStore(t, 5)
	arm(t, sts, "tenant-A", "e1", "S", taskSpec("tenant-A", "e1", "S", "T", 1000))
	task := claimAll(t, sts)[0]
	err := sts.RecordAttempt(context.Background(), refOf(task),
		spi.Attempt{Error: "nul\x00byte", AtMs: 1, NextAttemptTime: 1})
	if !errors.Is(err, spi.ErrStoreRejected) {
		t.Fatalf("RecordAttempt with a NUL in the error text: err = %v, want ErrStoreRejected", err)
	}
	if s := sqlState(err); len(s) != 5 || s[:2] != "22" {
		t.Errorf("SQLSTATE = %q, want class 22", s)
	}
	got := mustGet(t, sts, "tenant-A", task.ID)
	if got.Status != spi.ScheduledTaskRunning || got.Claim == nil || got.Claim.Token != task.Claim.Token {
		t.Errorf("task after the rejected write = %+v, want it unchanged", got)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

```bash
cd /Users/paul/go-projects/cyoda-light/cyoda-go/.worktrees/598-scheduler-ownership/plugins/postgres && \
go test -run 'TestClassifyError_DeterministicRejections|TestPostgres_ScheduledTaskStore_DeterministicRejectionIsMarked' .
```

Expected: FAIL — `SQLSTATE 22021: … does not carry ErrStoreRejected` (one line
per class-22/23/42 code) and `RecordAttempt with a NUL in the error text: err = … (SQLSTATE 22021), want ErrStoreRejected`.

- [ ] **Step 3: Implement**

In `plugins/postgres/transaction_manager.go`, `classifySQLState`: add as the
first statement inside the function body, before `var pgErr`:

```go
	if errors.Is(err, spi.ErrStoreRejected) {
		return err, true // already classified
	}
```

and add a last `case` to the `switch` (after the `QueryCanceled` case,
before its closing brace at `:941`):

```go
	case len(pgErr.Code) == 5 && (pgErr.Code[:2] == "22" || pgErr.Code[:2] == "23" || pgErr.Code[:2] == "42"):
		// Data exception, integrity-constraint violation, syntax or access
		// rule: the database will refuse the same statement again. The marker
		// tells a caller that retries by default (the scheduler's bookkeeping)
		// to stop. The unique_claims_uq case above keeps its own sentinel.
		return fmt.Errorf("%w: %w", spi.ErrStoreRejected, err), true
```

Append to the `classifyError` godoc (`:895`):

```go
//
// SQLSTATE classes 22, 23 and 42 carry spi.ErrStoreRejected: a deterministic
// rejection, which a retry cannot clear.
```

- [ ] **Step 4: Run the tests to verify they pass**

```bash
cd /Users/paul/go-projects/cyoda-light/cyoda-go/.worktrees/598-scheduler-ownership/plugins/postgres && \
go test -run 'TestClassifyError_|TestClassify|TestPostgres_ScheduledTaskStore_|TestConformance/ScheduledTasks' .
```

Expected: `ok`; the whole ScheduledTasks suite now passes.

- [ ] **Step 5: Whole plugin, as `make test-full` runs it**

```bash
cd /Users/paul/go-projects/cyoda-light/cyoda-go/.worktrees/598-scheduler-ownership && \
(cd plugins/postgres && go test -json -timeout 20m ./...) | go run ./scripts/testreport -must-run plugins/postgres && \
(cd plugins/postgres && go vet ./... && test -z "$(gofmt -l .)")
```

Expected: the report shows no failures; vet and gofmt print nothing. A
failure elsewhere in the plugin that asserts an exact error string for a
class-22/23/42 error is fixed here, by asserting the sentinel or the SQLSTATE
instead of the text.

- [ ] **Step 6: Commit**

```bash
cd /Users/paul/go-projects/cyoda-light/cyoda-go/.worktrees/598-scheduler-ownership && \
git status --short go.work && \
git add plugins/postgres/transaction_manager.go plugins/postgres/store_rejected_test.go \
  plugins/postgres/scheduled_task_store_test.go && \
git commit -m "feat(postgres): SQLSTATE classes 22, 23 and 42 carry ErrStoreRejected

A deterministic rejection is recognisable, so the scheduler latches on
it instead of retrying a write the database will always refuse.

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task BP-6: Plugin documentation

**Spec:** §10.2, §11 (`plugins/postgres/doc.go`, `docs/plugins/POSTGRES.md`),
§10.2 "Tenant scoping … It is corrected".

**TDD waiver:** documentation only; no behaviour changes. Checked by the
greps in Step 2.

**Files:**
- Modify: `docs/plugins/POSTGRES.md` (new section before `## Data model and schema` at `:115`; schema table `:246-256`; tenant note `:230-242`; migration notes after `:416`)
- Modify: `plugins/postgres/doc.go` (after `:63`)

**Interfaces:** none.

- [ ] **Step 1: Write the documentation**

`docs/plugins/POSTGRES.md` — insert before `## Data model and schema`:

```markdown
## Scheduled tasks and the scheduler pool

Scheduled tasks live in `scheduled_tasks`. Each task has a life (its arm
token, drawn on every arm), a status (`WAITING`, `RUNNING`, `FAILED`) and,
while `RUNNING`, a claim (claim token and owner incarnation). Two more tables
serve the scheduler: `scheduled_task_marks` records, per life, that an owner
was about to hand off a processor that is not safe to repeat, and
`scheduler_owners` holds one liveness record per pnode incarnation, stamped by
the database clock.

**Writes in the entity transaction.** Arming, cancelling, removing a fired
task, the segment stamp and `Fail` write task rows straight into the open
entity transaction (`REPEATABLE READ`). A task row that another transaction
changed after the snapshot raises `40001`, mapped to `spi.ErrConflict`; `40P01`
maps the same way. A row the transaction wrote stays locked until it ends.

**The scheduler pool.** Claims, marks, attempt records, give-backs, owner
records and sweeps never join the caller's transaction. They run on a pool of
their own, `CYODA_POSTGRES_SCHEDULER_CONNS` connections (default `10`), so
entity transactions cannot starve them. The async-search heartbeat and claim
run there too. The scheduler heartbeat has one more connection of its own.
Every scheduler connection uses `READ COMMITTED`, `statement_timeout` 30 s,
`idle_in_transaction_session_timeout` 10 s and `lock_timeout` 2 s, whatever
the DSN says; every acquire is bounded at 5 s. `GET /scheduled-tasks` reads
on the main pool.

**`ClaimDue`** is one transaction: rank the due tasks (one per entity, each
tenant within its limit, tenants taking turns), lock them with
`FOR UPDATE SKIP LOCKED`, claim them with a conditional `UPDATE`, and read
their marks while the locks are held. A row an open transaction wrote is
skipped. A partial unique index allows one `RUNNING` task per entity; a claim
that meets a concurrent claim of a sibling rolls back and claims nothing that
tick (unique violation, `40P01`, or `55P03` after `lock_timeout`).

**`MarkUnsafe`** share-locks the task row with `NOWAIT` and inserts the mark.
A row held by another transaction answers `spi.ErrTaskBusy` at once.

**Errors.** A lock wait past `lock_timeout` returns `55P03`, and the scheduler
retries it. SQLSTATE classes `22`, `23` and `42` carry `spi.ErrStoreRejected`
in every store of the plugin: the database will refuse the same statement
again.

**Tenant isolation.** Every tenant-facing statement filters on `tenant_id`.
`ClaimDue`, `GiveBackIdle`, the owner methods and the sweeps are cross-tenant
and no API reaches them, so none of the three tables is under row-level
security. The comment in migration `000004` claims every write carried a
tenant predicate; that was not so, and `000014` records the correction.
```

Schema table (`:246-256`) — add three rows after `submit_times`:

```markdown
| `scheduled_tasks` | Scheduled tasks: life, status, claim, attempt record | `id` (a hash of tenant, entity, state and transition) |
| `scheduled_task_marks` | Unsafe-dispatch marks, one per life | `(task_id, arm_token)` |
| `scheduler_owners` | Scheduler liveness, one row per pnode incarnation | `owner` |
```

Tenant note (`:230-235`) — after "The live mechanism is the explicit
`WHERE tenant_id = $1` predicate every statement carries;", no change to the
sentence; append to the paragraph:

```markdown
The scheduler's three tables are not under RLS at all; see "Scheduled tasks
and the scheduler pool".
```

Operational notes — add after the `000013` sub-bullet (`:413-416`):

```markdown
- **Migration `000014` blocks readers and writers of `scheduled_tasks`** while
  it runs. It alters the table (a rewrite, for the per-row arm token), backfills
  it and builds five indexes in one implicit transaction under
  `ACCESS EXCLUSIVE`. No other table is locked. The table holds one row per
  armed timer.
```

`plugins/postgres/doc.go` — append before `// Registration:` (`:65`):

```go
// # Scheduler pool
//
// The scheduled-task store's never-joining methods, and the async-search
// heartbeat and claim, run on a pool of their own (CYODA_POSTGRES_SCHEDULER_CONNS)
// with fixed ceilings: READ COMMITTED, statement_timeout 30s,
// idle_in_transaction_session_timeout 10s, lock_timeout 2s, and a 5s acquire.
// The scheduler heartbeat has one more connection of its own. See
// scheduler_pool.go.
//
```

- [ ] **Step 2: Check the documentation against the code**

```bash
cd /Users/paul/go-projects/cyoda-light/cyoda-go/.worktrees/598-scheduler-ownership && \
grep -n 'schedulerStatementTimeout\|schedulerIdleInTxTimeout\|schedulerLockTimeout\|schedulerAcquireTimeout' plugins/postgres/scheduler_pool.go | head -4 && \
grep -n 'defaultSchedulerConns int32 = 10' plugins/postgres/config.go && \
grep -c 'scheduled_task_marks\|scheduler_owners' docs/plugins/POSTGRES.md && \
(cd plugins/postgres && go vet ./... && test -z "$(gofmt -l .)")
```

Expected: the four constants at 30 s, 10 s, 2 s, 5 s; the default line; a
count ≥ 4; vet and gofmt silent.

- [ ] **Step 3: Commit**

```bash
cd /Users/paul/go-projects/cyoda-light/cyoda-go/.worktrees/598-scheduler-ownership && \
git add docs/plugins/POSTGRES.md plugins/postgres/doc.go && \
git commit -m "docs(postgres): scheduled tasks, the scheduler pool, migration 000014

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

## V5 — resolved

**Question (spec §16):** in `ClaimDue`, does the loser of a unique-index wait
roll back within `lock_timeout` and claim nothing?

**Answer: yes.** The loser's wait on the one-RUNNING-task-per-entity index is
a wait on the rival transaction's lock, which `lock_timeout` bounds. Three
outcomes, all handled by `lostClaimRace` in BP-4:
- the rival commits (claims take milliseconds) → `23505` on
  `scheduled_tasks_one_running_per_entity_uq`, at once;
- the rival stalls → `55P03` after 2 s;
- two claimers wait on each other's entries → `40P01` after
  `deadlock_timeout` (1 s by default, below `lock_timeout`).

Each one rolls back the claim transaction and returns no tasks, with a DEBUG
line. Proof: `TestPostgres_ClaimDue_LosingASiblingRaceClaimsNothing` (both
live outcomes, with bounds) and `TestLostClaimRace` (all three codes, and the
codes that must not match).

## Coverage carried forward (§13 rows this stream proves on PostgreSQL)

| §13 row | Where on PostgreSQL |
|---|---|
| every S-column row | `TestConformance/ScheduledTasks/...` (stream S's suite), green after BP-5 |
| two due siblings, two pnodes at once: one wins, the other claims nothing | `TestPostgres_ClaimDue_LosingASiblingRaceClaimsNothing` (BP-4) |
| a row written by an open transaction is not claimable (C6); `ErrTaskBusy` | `TestPostgres_MarkUnsafe_RowWrittenByAnOpenTxIsBusyAtOnce` (BP-4) + S |
| a reclaimed or re-armed task makes the old run's commit fail (C1) | `TestPostgres_ScheduledTaskStore_RowChangedAfterSnapshotIsErrConflict` (BP-4) + S |
| heartbeats are not starved with every main-pool connection busy (C4) | `TestPostgres_ScheduledTaskStore_NotStarvedByAnExhaustedMainPool`, `…Heartbeat_HasItsOwnConnection` (BP-4); the E cell is T's |
| async-search heartbeats and claims run on the scheduler pool | `TestPGSearchStore_LivenessSurvivesAnExhaustedMainPool` (BP-3); the E cell is T's |
| a scheduler-pool statement blocked on a task-row lock gives up after `lock_timeout` | `TestSchedulerPools_LockWaitEndsAtLockTimeout` (BP-2), `TestPostgres_ScheduledTaskStore_RowLockWaitEndsAtLockTimeout` (BP-4) |
| `spi.ErrStoreRejected` (every backend sets the marker) | `TestClassifyError_DeterministicRejectionsCarryErrStoreRejected`, `…DeterministicRejectionIsMarked` (BP-5) + S |
| `DeleteForModel` in tenant A leaves tenant B's tasks; every tenant-facing method | `TestPostgres_ScheduledTaskStore_TenantFacingMethodsFilterOnTenant` (BP-4) + S |
| a joining write whose tenant is not the transaction's is refused | `TestPostgres_ScheduledTaskStore_JoiningWriteOfAnotherTenantRefused` (BP-4) + S `Tenant/JoiningWriteOtherTenantRefused` |
| `lastError` over 1 024 bytes is `ErrStoreRejected` | `TestMigration14_ScheduledTaskSchema` (`scheduled_tasks_last_error_len_chk`, BP-4) + S `ErrorText/StoreRejected` |
| a lost-owner claim is flagged for `cyoda.scheduler.claims{reason=owner_lost}` | S `Claim/LostOwnerFlagged` (BP-4's `lockClaimableSQL` status) |
| the pool gauge names its pool (README C-R6) | `TestRegisterPoolMetrics_ReportsPoolStat` (BP-2), `TestMetrics_PostgresPoolSeriesAreExported` (BP-2, e2e) |
| each new variable: its default and its validation failure (`CYODA_POSTGRES_SCHEDULER_CONNS`) | `TestParseConfig_SchedulerConns*` (BP-1) |

## Stream interface summary

**BP produces:**

```go
// plugins/postgres — satisfies spi.ScheduledTaskStore (interfaces.md:118-136)
func (f *StoreFactory) ScheduledTaskStore(ctx context.Context) (spi.ScheduledTaskStore, error)

// config (interfaces.md:255)
config.SchedulerConns int32 // CYODA_POSTGRES_SCHEDULER_CONNS; default 10; >= 2; invalid → startup error

// package-internal
type schedulerQuerier struct{ /* … */ }        // never joins; 5s acquire; READ COMMITTED pool
func (f *StoreFactory) schedulerPools() (work, heartbeat *pgxpool.Pool, err error)
func lostClaimRace(err error) bool
func joinTenant(ctx context.Context, tenant spi.TenantID) error
func registerPoolMetrics(meter metric.Meter, pool *pgxpool.Pool, sched *schedulerPools) (func(), error)

// metrics (README C-R6)
cyoda.storage.pool.connections{backend="postgres", pool="main"|"scheduler"|"heartbeat", state}

// test exports (export_test.go)
func SchedulerPoolForTest(t testing.TB, f *StoreFactory) *pgxpool.Pool
func CloseSchedulerPoolsForTest(f *StoreFactory)
```

Behaviour other streams can rely on (PostgreSQL):
- `Get` joins the transaction on `ctx` when there is one. A caller that needs
  the non-joining re-read of §5.2 passes a context without a transaction.
- Joined task-row writes fail with `spi.ErrConflict` (40001/40P01) at the
  statement, before the commit.
- `ClaimDue` returns `nil, nil` when it loses a sibling race; it never
  returns that race as an error. It returns an error for `Limit < 1` or
  `PerTenantLimit < 1`. It sets `ClaimedFromLostOwner` on each task it took
  from a stale or missing owner (README C-S1).
- `ReconcileForEntity`, `RemoveLife`, `StampSegment`, `DeleteForEntities`,
  `DeleteForModel` and `Fail` return `spi.ErrTxTenantMismatch` when their
  tenant is not the tenant of the transaction on `ctx`, as memory and SQLite do.
- `last_error` holds at most 1 024 bytes (`scheduled_tasks_last_error_len_chk`).
- `MarkUnsafe` never waits on a row lock: `spi.ErrTaskBusy` at once.
- Every other scheduler-pool write waits at most `lock_timeout` (2 s) and then
  returns the raw `55P03`, unmarked; `statement_timeout` (57014) likewise.
- SQLSTATE classes 22/23/42 carry `spi.ErrStoreRejected` in every store of the
  plugin, including the audit write in `Fail`'s transaction.
- `RecordAttempt` stores `Attempt.Error` whether or not `NotCounted`; `Fail`
  stores `Failure.Error` as `lastError` as given.
- `DeleteForModel` with a nil `keep` removes all the model's tasks.
- `ReconcileForEntity` takes tenant and entity from the request, never from
  the `Arm` items; its returned slice excludes `req.Cancel` ids.

**BP consumes:**
- S: every name in `interfaces.md:7-153`, and `runScheduledTasksSuite` registered
  in `spitest.StoreFactoryConformance`.
- R: `CYODA_POSTGRES_SCHEDULER_CONNS` is documented by BP-1 in
  `help/config/database.md`; R's config task must not add a second bullet.
- Shared files (merge by hand, no semantic overlap):
  `cmd/cyoda/help/content/config/database.md` (one bullet),
  `docs/plugins/POSTGRES.md` (BP owns it).

## Open points

1. **The PG harness cannot fast-forward the store clock.** `AdvanceClock`
   sleeps at most 100 ms (`conformance_test.go:181-183`), and staleness is
   judged by `now()`. S's stale-owner, lost-owner and `SweepOwners` cases must
   use a `StaleAfter` / `deadFor` well under 100 ms, or they cannot pass on
   PostgreSQL. For S to confirm.
2. **Behaviours S should pin, so BM and BQ match:** `ClaimDue` with
   `Limit < 1` or `PerTenantLimit < 1` is an error; `DeleteForModel` with a
   nil `keep` keeps nothing; `RecordAttempt{NotCounted}` still records the
   error; `Fail` overwrites `lastError` with `Failure.Error`, even when empty;
   `ReconcileForEntity` ignores `TenantID`/`EntityID` on the `Arm` items.
3. **`ErrStoreRejected` scope.** BP-5 sets it in the plugin's one classifier,
   so it covers every PostgreSQL store, not only scheduled tasks. That is what
   makes a rejected audit write in `Fail`'s §5.7 transaction latch the node
   rather than retry forever. BM and BQ must cover their audit write in the
   same way. For the lead to confirm across the streams.
4. **Connection budget in multi-node tests.** Each pnode now opens up to
   `SchedulerConns + 1` (11) more connections. The test containers run with
   PostgreSQL's default `max_connections` of 100 (`internal/testpg/testpg.go:45-48`
   sets no limit). A multi-node fixture with several pnodes can reach it. T's
   fixtures should set `CYODA_POSTGRES_SCHEDULER_CONNS=2` or raise
   `max_connections`. For T.
5. **Closed (README C-R6).** BP-2 Steps 6-10 report the scheduler and
   heartbeat pools on `cyoda.storage.pool.connections` with
   `pool=scheduler|heartbeat`, and the main pool with `pool=main`. D-6
   documents the attribute.
6. **Migration 000014 locks `scheduled_tasks` for readers too**, for the
   whole file. It is grandfathered in the index guard with its own lock
   profile. Acceptable because there are no production instances; stated in
   `POSTGRES.md` for operators.
7. **The down migration deletes FAILED tasks.** The version-13 shape has no
   status; keeping them would make them due again.
