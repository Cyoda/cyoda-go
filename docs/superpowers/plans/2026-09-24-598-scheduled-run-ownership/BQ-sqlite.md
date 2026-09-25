# Stream BQ — SQLite store

Spec sections: §10.1 (the contract), §10.3 (memory and SQLite, SQLite
durability), §10.2 (the column and index list this migration follows), §5.2,
§5.5, §5.6, §6.1, §6.2. §13 rows with an **S** tick run on this backend
through `spitest` (stream S); the "1" rows (single-node SQLite restart) are
stream T's, on top of the durability this stream provides.

Needs: stream S merged on its SPI branch, and the worktree's `go.work` `use`
line pointing at it. The store follows BM's design; where the two differ, the
difference is SQL and is named in the task.

## Facts settled by reading the code

1. **Schema today.** `scheduled_tasks` has `PRIMARY KEY (id)`, the columns
   `redispatch_after` and `attempt_count`, and the indexes
   `idx_scheduled_tasks_due (scheduled_time)` and
   `idx_scheduled_tasks_entity (tenant_id, entity_id)`
   (`plugins/sqlite/migrations/000003_scheduled_tasks.up.sql`); `000004` adds
   `armed_by_id` and `armed_by_kind` with `DEFAULT ''`. The latest migration is
   `000008_sm_audit_tx_index`. Migrations run with `NoTxWrap: true`
   (`migrate.go:24-26`) and are embedded (`:17-18`).
2. **Staging today** mirrors memory: `scheduledTaskOp{kind, id, task}`
   (`scheduled_task_store.go:11-30`), staged on
   `transactionManager.scheduledTaskOps` (`txmanager.go:89-101`, `:182-198`),
   applied at the end of `flushToSQLite` inside its `sqlTx`
   (`:885-893`), truncated by savepoints (`:1137-1138`). `Get` and
   `ReconcileForEntity` read committed state only (`scheduled_task_store.go:185-196`,
   `:243-264`). `stage` does not check `tx.Closed` (`:118-131`), while
   `entityStore.Save` does (`entity_store.go:297`).
3. **The conflict check orders by submit time.** `committedTx` has no sequence
   number (`txmanager.go:18-23`). The check is
   `!committed.submitTime.Before(tx.SnapshotTime)` (`:524-525`). Begin
   floors the snapshot to `lastSubmitTime` and **reserves** it as the new floor
   (`:427-431`); a commit stamps `max(now, floor + 1 µs)` (`:358-367`). Step 6
   appends and prunes by submit time (`:576-622`).
4. **The false conflict this gives under a frozen clock.** Clock at T. A
   long-lived transaction H begins (snapshot T). Transaction A begins
   (snapshot T), writes entity X, commits at T+1 µs; H keeps the entry. B
   begins: snapshot = floor = T+1 µs. B writes X and commits: the entry's
   T+1 µs is not before B's T+1 µs, so B conflicts with a commit that preceded
   it. A never-joining claim logged the same way would make a run conflict
   with its own claim. BQ-3 replaces the comparison with a sequence number.
5. **The commit gate.** A one-slot channel (`txmanager.go:69-72`, `:313-347`)
   held by Commit from step 2 to its return (`:510-511`), by Begin while it
   floors its snapshot (`:420-424`), and by direct entity writes
   (`entity_store.go:360`, `:573`, `:783`). Lock order: `tx.OpMu` →
   commit gate → `mu`.
6. **One writer connection.** `db.SetMaxOpenConns(1)` (`store_factory.go:106`,
   the writer pool); reads that must not queue behind it use `readDB`, a
   `query_only` pool (`store_factory.go`, the `readDB` block). An open `*Rows`
   on `db` blocks every other statement on `db` until it is closed
   (`search_store.go:447-450`).
7. **Claim precedent.** `asyncSearchStore.ClaimStale` scans candidates, closes
   the rows, then updates each one (`search_store.go:404-496`). `ClaimDue`
   keeps that shape inside one `sqlTx` under the commit gate.
8. **Error mapping today.** `classifyError` maps `BUSY` and
   `CONSTRAINT_UNIQUE`/`CONSTRAINT_PRIMARYKEY` to `ErrConflict` and passes
   everything else through (`errors.go:39-67`). The driver's `*Error` answers
   `errors.Is` for both an `ErrorCode` and an `ExtendedErrorCode`
   (`github.com/ncruces/go-sqlite3@v0.35.4/error.go:66-74`).
9. **Test exports** live in `export_test.go` (`package sqlite`), for example
   `DBForTest` and `ClassifyErrorForTest`.

## V1, resolved for SQLite

**The entity check is not disturbed, and it stops depending on the clock.**
BQ-3 gives `committedTx` a `seq` and the manager a `commitSeq` and
`txSnapshotSeq`, assigned under the commit gate and `mu`, exactly as memory
does (`plugins/memory/txmanager.go:89-105`). The entity loop keeps its body;
only its guard changes from the submit-time comparison to
`committed.seq > txSnapshotSeq[txID]`. Because Begin and every commit hold the
commit gate, the sequence number is exact: a commit either finished before
Begin (its seq is at or below the snapshot's) or starts after it. BQ-3's test
is the frozen-clock case of fact 4.

**Task rows are a separate set.** BQ-4 adds `committedTx.taskWrites
map[taskKey]bool` and a second loop, as BM-3 does.

**Every check is evaluated before anything is applied.**

1. *At staging.* A joining write runs its check in
   `scheduledTaskStore.write`, holding `tx.OpMu` (read), against a `taskView`:
   the committed rows read from `db`, then the transaction's earlier staged
   ops. It stages post-images. The staging read does not hold the commit
   gate: a write that lands between the read and the append is logged after
   this transaction's Begin, so step 3 refuses the commit.
2. *At commit, step 3, before `flushToSQLite` writes anything.* The conflict
   check refuses the commit if an entry with `seq > txSnapshotSeq[txID]`
   wrote one of the transaction's task rows. Every task-row write reaches the
   log: a transaction's at step 6, and a write that commits on its own in
   `commitTaskWrites`, before it releases the commit gate.
3. *So `flushToSQLite` evaluates nothing.* It upserts or deletes the
   post-images. A statement there can still fail for an infrastructure reason;
   the flush is one `sqlTx`, so the entity rows and the task rows roll back
   together, as today.

The worked example of BM applies unchanged, with "entry" meaning an element of
the in-memory `committedLog` that the commit gate orders.

## Working rules for this stream

- Every command runs from the plugin module:
  `cd /Users/paul/go-projects/cyoda-light/cyoda-go/.worktrees/598-scheduler-ownership/plugins/sqlite`.
- Never stage `go.work`. Stage files by name.
- Every task-row write that commits on its own holds the commit gate from its
  first read to its log entry: take it with
  `_ = s.tm.acquireCommitGate(context.Background())` and
  `defer s.tm.releaseCommitGate()` on the next line, as `saveDirectly` does
  (`entity_store.go:360-361`).
- Close every `*sql.Rows` on `db` before the next statement on `db` (fact 6).
- No token appears in an error message or a log line.
- Between BQ-1 and BQ-2 the never-joining methods answer
  `errors.ErrUnsupported`. `TestConformance` runs only in BQ-8; earlier steps
  run `go test ./... -skip 'TestConformance'`.

---

### Task BQ-1: Migration 000009, rows per life, staged post-images, joining writes and reads

**Spec:** §10.3 "SQLite durability"; §10.2 table columns and indexes; §10.1
rows `ReconcileForEntity`, `RemoveLife`, `StampSegment`, `DeleteForEntities`,
`DeleteForModel`, `Get`, `Query`, `Fail`; C2; staged removals expanded when
staged; §7 "Arm".

**Files:**
- Create: `plugins/sqlite/migrations/000009_scheduled_run_ownership.up.sql`
- Create: `plugins/sqlite/migrations/000009_scheduled_run_ownership.down.sql`
- Replace: `plugins/sqlite/scheduled_task_store.go` (all of `:1-286`)
- Create: `plugins/sqlite/scheduled_task_claims.go` (interim; BQ-2 replaces it)
- Modify: `plugins/sqlite/txmanager.go` (`:182-198`; step 4.5 `:549-554`;
  flush `:885-893`; field doc `:89-101`)
- Modify: `plugins/sqlite/store_factory.go` (accessor `:405-411`)
- Replace: `plugins/sqlite/scheduled_task_store_test.go`
- Delete: `plugins/sqlite/scheduled_task_attribution_internal_test.go` (its
  legacy-row case moves into the migration test)
- Create: `plugins/sqlite/migration_000009_internal_test.go`

**Interfaces:**
- Consumes (stream S): as BM-1.
- Produces (package-internal): `taskKey`, `scheduledTaskOp{key, after, touch}`,
  `taskColumns`, `selectTaskSQL`, `upsertTaskSQL`, `taskArgs`, `scanTask`,
  `readTasks`, `applyTaskOp`, `taskView`, `fenced`, `newLife`,
  `copyScheduledTask`, `(*scheduledTaskStore).write`,
  `(*transactionManager).stagedTaskOps`, `stageTaskOps`, `commitTaskWrites`;
  tables `scheduled_task_marks`, `scheduler_owners`.

- [ ] **Step 1: Write the failing tests**

`plugins/sqlite/migration_000009_internal_test.go`:

```go
package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"

	"github.com/golang-migrate/migrate/v4"
	sqlitemigrate "github.com/golang-migrate/migrate/v4/database/sqlite"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	"github.com/google/uuid"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

func migrateTo(t *testing.T, db *sql.DB, version uint) {
	t.Helper()
	driver, err := sqlitemigrate.WithInstance(db, &sqlitemigrate.Config{NoTxWrap: true})
	if err != nil {
		t.Fatalf("migration driver: %v", err)
	}
	src, err := iofs.New(migrationFS, "migrations")
	if err != nil {
		t.Fatalf("migration source: %v", err)
	}
	m, err := migrate.NewWithInstance("iofs", src, "sqlite", driver)
	if err != nil {
		t.Fatalf("migrator: %v", err)
	}
	if err := m.Migrate(version); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		t.Fatalf("migrate to %d: %v", version, err)
	}
}

// A task pending before migration 9 stays pending after it, as a new life.
// The row is written the way version 8 wrote it, without armed_by columns.
func TestMigration9_KeepsPendingTasksAsNewLives(t *testing.T) {
	path := filepath.Join(t.TempDir(), "m9.db")
	db, err := sql.Open("sqlite3", "file:"+path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	db.SetMaxOpenConns(1)
	migrateTo(t, db, 8)
	if _, err := db.Exec(`INSERT INTO scheduled_tasks
		(id, tenant_id, type, scheduled_time, entity_id, model_name, model_version,
		 transition, source_state, armed_at, attempt_count, redispatch_after)
		VALUES ('legacy:S:T', 'tenant-A', ?, 1000, 'legacy', 'M', 1, 'T', 'S', 0, 2, 5000)`,
		string(spi.ScheduledTaskFireTransition)); err != nil {
		t.Fatalf("insert version-8 row: %v", err)
	}
	migrateTo(t, db, 9)
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	f, err := NewStoreFactoryForTest(context.Background(), path)
	if err != nil {
		t.Fatalf("NewStoreFactoryForTest: %v", err)
	}
	defer f.Close()
	sts, err := f.ScheduledTaskStore(context.Background())
	if err != nil {
		t.Fatalf("ScheduledTaskStore: %v", err)
	}
	got, found, err := sts.Get(context.Background(), "tenant-A", "legacy:S:T")
	if err != nil || !found {
		t.Fatalf("Get = %v, %v; want the migrated row", found, err)
	}
	if got.Status != spi.ScheduledTaskWaiting || got.ArmToken == uuid.Nil || got.NextAttemptTime != 1000 ||
		got.Attempts != 0 || got.Claim != nil || got.ArmedBy != (spi.Principal{}) {
		t.Fatalf("migrated row = %+v, want WAITING, a new arm token, due at 1000, no attempts, no claim, zero ArmedBy", got)
	}
}

func TestMigration9_Down(t *testing.T) {
	db, err := sql.Open("sqlite3", "file:"+filepath.Join(t.TempDir(), "m9down.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	migrateTo(t, db, 9)
	if _, err := db.Exec(`INSERT INTO scheduled_tasks
		(id, tenant_id, type, scheduled_time, entity_id, model_name, model_version,
		 transition, source_state, armed_at, arm_token, status, next_attempt_time)
		VALUES ('e1:S:T', 'tenant-A', 'FIRE_TRANSITION', 1000, 'e1', 'M', 1, 'T', 'S', 0, ?, 'WAITING', 1000)`,
		uuid.NewString()); err != nil {
		t.Fatalf("insert version-9 row: %v", err)
	}
	migrateTo(t, db, 8)
	var attempts int
	if err := db.QueryRow(`SELECT attempt_count FROM scheduled_tasks WHERE id = 'e1:S:T'`).Scan(&attempts); err != nil {
		t.Fatalf("read version-8 row: %v", err)
	}
	for _, table := range []string{"scheduled_task_marks", "scheduler_owners"} {
		var n int
		if err := db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE name = ?`, table).Scan(&n); err != nil || n != 0 {
			t.Fatalf("%s after down: count %d, err %v; want dropped", table, n, err)
		}
	}
}
```

Replace `plugins/sqlite/scheduled_task_store_test.go` with the BM-1 test file
(`plugins/memory/scheduled_task_store_test.go` as BM-1 writes it), changed in
exactly these places:

```go
package sqlite_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/plugins/sqlite"
)

type taskFixture struct {
	f     *sqlite.StoreFactory
	clock *sqlite.TestClock
	sts   spi.ScheduledTaskStore
	tm    spi.TransactionManager
	path  string
}

// newTaskFixture returns a factory on a frozen clock: nothing advances it
// unless the test does.
func newTaskFixture(t *testing.T) taskFixture {
	t.Helper()
	clock := sqlite.NewTestClockAt(time.UnixMilli(1_000_000))
	path := filepath.Join(t.TempDir(), "tasks.db")
	f, err := sqlite.NewStoreFactoryForTest(context.Background(), path, sqlite.WithClock(clock))
	if err != nil {
		t.Fatalf("NewStoreFactoryForTest: %v", err)
	}
	t.Cleanup(func() { _ = f.Close() })
	sts, err := f.ScheduledTaskStore(context.Background())
	if err != nil {
		t.Fatalf("ScheduledTaskStore: %v", err)
	}
	tm, err := f.TransactionManager(context.Background())
	if err != nil {
		t.Fatalf("TransactionManager: %v", err)
	}
	return taskFixture{f: f, clock: clock, sts: sts, tm: tm, path: path}
}

func tenantCtx(tenant spi.TenantID) context.Context { return testCtx(string(tenant)) }
```

and every `ctxWithTenant(` in the copied file becomes `tenantCtx(`. The test
functions, `taskTenantA`/`taskTenantB`, `begin`, `commit`, `rollback`,
`armTask`, `arm` and `getTask` are copied without other change. The file
therefore holds `TestTasks_ArmStartsANewLife`,
`TestTasks_ReconcileRemovesEveryOtherTaskOfTheEntity`,
`TestTasks_StagedArmIsDiscardedOnRollback`,
`TestTasks_SavepointTruncatesStagedOps`, `TestTasks_JoiningGetSeesStagedOps`,
`TestTasks_RemovalsAreExpandedWhenStaged`,
`TestTasks_StagingIntoACommittedTransactionIsRefused`,
`TestTasks_AWriteForAnotherTenantThanTheTransactionIsRefused`,
`TestTasks_GetIsTenantScoped`, `TestTasks_ArmTakesTenantAndEntityFromTheRequest`
and `TestTasks_QueryOrdersIdsByteWise`.

Delete `plugins/sqlite/scheduled_task_attribution_internal_test.go`.

- [ ] **Step 2: Run the tests and see them fail**

```
cd /Users/paul/go-projects/cyoda-light/cyoda-go/.worktrees/598-scheduler-ownership/plugins/sqlite && go test ./... -run 'TestTasks_|TestMigration9_'
```

Expected: `FAIL github.com/cyoda-platform/cyoda-go/plugins/sqlite [build failed]`
(`*scheduledTaskStore does not implement spi.ScheduledTaskStore`,
`t.RedispatchAfter undefined`).

- [ ] **Step 3: Write the implementation**

`plugins/sqlite/migrations/000009_scheduled_run_ownership.up.sql`:

```sql
-- One owner per scheduled run. A task row carries its life (arm_token), its
-- status and its run bookkeeping. Unsafe marks and owner liveness are tables
-- of their own, so a mark written before an unsafe dispatch survives a
-- process restart and the next claim sees it.
--
-- The table is rebuilt: the primary key becomes (tenant_id, id), and
-- redispatch_after and attempt_count go. A pending task is kept as a new
-- life: WAITING, due at its scheduled time, with a fresh random arm token.
CREATE TABLE scheduled_tasks_v9 (
    id                TEXT    NOT NULL,
    tenant_id         TEXT    NOT NULL,
    type              TEXT    NOT NULL,
    scheduled_time    INTEGER NOT NULL,
    timeout_ms        INTEGER,
    entity_id         TEXT    NOT NULL,
    model_name        TEXT    NOT NULL,
    model_version     INTEGER NOT NULL,
    transition        TEXT    NOT NULL,
    source_state      TEXT    NOT NULL,
    armed_at          INTEGER NOT NULL,
    armed_by_id       TEXT    NOT NULL DEFAULT '',
    armed_by_kind     TEXT    NOT NULL DEFAULT '',
    arm_token         TEXT    NOT NULL,
    status            TEXT    NOT NULL CHECK (status IN ('WAITING', 'RUNNING', 'FAILED')),
    next_attempt_time INTEGER NOT NULL,
    attempts          INTEGER NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    lost_owners       INTEGER NOT NULL DEFAULT 0 CHECK (lost_owners >= 0),
    last_attempt_time INTEGER,
    last_error        TEXT    NOT NULL DEFAULT '' CHECK (length(CAST(last_error AS BLOB)) <= 1024),
    failure_reason    TEXT    NOT NULL DEFAULT '' CHECK (failure_reason IN (
                          '', 'UNSAFE_WORK_NOT_COMPLETED', 'OWNER_LOST_REPEATEDLY',
                          'EXPIRED_AFTER_FAILED_ATTEMPTS', 'RUN_PANICKED',
                          'STOPPED_AFTER_PARTIAL_COMMIT')),
    failed_time       INTEGER,
    partial_commit    INTEGER NOT NULL DEFAULT 0 CHECK (partial_commit IN (0, 1)),
    claim_token       TEXT,
    claim_owner       TEXT,
    CHECK ((status = 'RUNNING') = (claim_token IS NOT NULL AND claim_owner IS NOT NULL)),
    CHECK ((status = 'FAILED') = (failure_reason <> '')),
    PRIMARY KEY (tenant_id, id)
) STRICT;

INSERT INTO scheduled_tasks_v9
    (id, tenant_id, type, scheduled_time, timeout_ms, entity_id, model_name,
     model_version, transition, source_state, armed_at, armed_by_id,
     armed_by_kind, arm_token, status, next_attempt_time)
SELECT id, tenant_id, type, scheduled_time, timeout_ms, entity_id, model_name,
       model_version, transition, source_state, armed_at, armed_by_id,
       armed_by_kind,
       lower(hex(randomblob(4)) || '-' || hex(randomblob(2)) || '-4' ||
             substr(hex(randomblob(2)), 2) || '-' ||
             substr('89ab', 1 + (abs(random()) % 4), 1) ||
             substr(hex(randomblob(2)), 2) || '-' || hex(randomblob(6))),
       'WAITING', scheduled_time
FROM scheduled_tasks;

DROP TABLE scheduled_tasks;
ALTER TABLE scheduled_tasks_v9 RENAME TO scheduled_tasks;

-- ClaimDue: due WAITING rows, and RUNNING rows by owner.
CREATE INDEX idx_scheduled_tasks_waiting ON scheduled_tasks (next_attempt_time) WHERE status = 'WAITING';
CREATE INDEX idx_scheduled_tasks_owner ON scheduled_tasks (claim_owner) WHERE status = 'RUNNING';
-- At most one RUNNING task per entity.
CREATE UNIQUE INDEX idx_scheduled_tasks_running_entity ON scheduled_tasks (tenant_id, entity_id) WHERE status = 'RUNNING';
-- Query pages in (scheduled_time, id) order per tenant.
CREATE INDEX idx_scheduled_tasks_query ON scheduled_tasks (tenant_id, scheduled_time, id);
-- DeleteForModel; ReconcileForEntity and DeleteForEntities.
CREATE INDEX idx_scheduled_tasks_model ON scheduled_tasks (tenant_id, model_name, model_version);
CREATE INDEX idx_scheduled_tasks_entity ON scheduled_tasks (tenant_id, entity_id);

-- One mark per life at most, naming the claim that wrote it. A mark outlives
-- the life it belongs to until SweepMarks removes it, so it has no foreign key.
CREATE TABLE scheduled_task_marks (
    tenant_id   TEXT NOT NULL,
    task_id     TEXT NOT NULL,
    arm_token   TEXT NOT NULL,
    claim_token TEXT NOT NULL,
    PRIMARY KEY (tenant_id, task_id, arm_token)
) STRICT, WITHOUT ROWID;

-- Owner liveness on the store clock, in microseconds.
CREATE TABLE scheduler_owners (
    owner        TEXT    NOT NULL PRIMARY KEY,
    heartbeat_at INTEGER NOT NULL
) STRICT, WITHOUT ROWID;
```

`plugins/sqlite/migrations/000009_scheduled_run_ownership.down.sql`:

```sql
DROP TABLE IF EXISTS scheduler_owners;
DROP TABLE IF EXISTS scheduled_task_marks;

CREATE TABLE scheduled_tasks_v8 (
    id                TEXT    NOT NULL,
    tenant_id         TEXT    NOT NULL,
    type              TEXT    NOT NULL,
    scheduled_time    INTEGER NOT NULL,
    timeout_ms        INTEGER,
    redispatch_after  INTEGER,
    entity_id         TEXT    NOT NULL,
    model_name        TEXT    NOT NULL,
    model_version     INTEGER NOT NULL,
    transition        TEXT    NOT NULL,
    source_state      TEXT    NOT NULL,
    armed_at          INTEGER NOT NULL,
    attempt_count     INTEGER NOT NULL DEFAULT 0,
    armed_by_id       TEXT    NOT NULL DEFAULT '',
    armed_by_kind     TEXT    NOT NULL DEFAULT '',
    PRIMARY KEY (id)
) STRICT;

INSERT INTO scheduled_tasks_v8
    (id, tenant_id, type, scheduled_time, timeout_ms, entity_id, model_name,
     model_version, transition, source_state, armed_at, armed_by_id, armed_by_kind)
SELECT id, tenant_id, type, scheduled_time, timeout_ms, entity_id, model_name,
       model_version, transition, source_state, armed_at, armed_by_id, armed_by_kind
FROM scheduled_tasks;

DROP TABLE scheduled_tasks;
ALTER TABLE scheduled_tasks_v8 RENAME TO scheduled_tasks;

CREATE INDEX idx_scheduled_tasks_due ON scheduled_tasks (scheduled_time);
CREATE INDEX idx_scheduled_tasks_entity ON scheduled_tasks (tenant_id, entity_id);
```

Replace `plugins/sqlite/scheduled_task_store.go` with:

```go
package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/google/uuid"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// taskKey identifies one task row: the table's primary key.
type taskKey struct {
	tenant spi.TenantID
	id     string
}

// scheduledTaskOp is one staged write to a task row: the row as it is after
// the write, or nil when the write removes it. A touch changes nothing; it
// only puts the row in the transaction's write set, so that a RemoveLife of a
// life replaced after the transaction began still fails the commit.
//
// Every check a write makes is evaluated when it is staged (see write). The
// commit's conflict check proves that no other writer changed those rows
// since the transaction began, so flushToSQLite writes the post-images as
// they are.
type scheduledTaskOp struct {
	key   taskKey
	after *spi.ScheduledTask
	touch bool
}

type queryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

const taskColumns = `id, tenant_id, type, scheduled_time, timeout_ms, entity_id, model_name,
	model_version, transition, source_state, armed_at, armed_by_id, armed_by_kind, arm_token,
	status, next_attempt_time, attempts, lost_owners, last_attempt_time, last_error,
	failure_reason, failed_time, partial_commit, claim_token, claim_owner`

// selectTaskSQL reads task rows as t, with UnsafeMarked: a mark exists for
// the row's current life.
const selectTaskSQL = `SELECT t.id, t.tenant_id, t.type, t.scheduled_time, t.timeout_ms,
	t.entity_id, t.model_name, t.model_version, t.transition, t.source_state, t.armed_at,
	t.armed_by_id, t.armed_by_kind, t.arm_token, t.status, t.next_attempt_time, t.attempts,
	t.lost_owners, t.last_attempt_time, t.last_error, t.failure_reason, t.failed_time,
	t.partial_commit, t.claim_token, t.claim_owner,
	EXISTS (SELECT 1 FROM scheduled_task_marks m
	        WHERE m.tenant_id = t.tenant_id AND m.task_id = t.id AND m.arm_token = t.arm_token)
	FROM scheduled_tasks t`

// upsertTaskSQL writes a whole row: every column but the key is replaced.
const upsertTaskSQL = `INSERT INTO scheduled_tasks (` + taskColumns + `)
	VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	ON CONFLICT (tenant_id, id) DO UPDATE SET
	  type = excluded.type, scheduled_time = excluded.scheduled_time,
	  timeout_ms = excluded.timeout_ms, entity_id = excluded.entity_id,
	  model_name = excluded.model_name, model_version = excluded.model_version,
	  transition = excluded.transition, source_state = excluded.source_state,
	  armed_at = excluded.armed_at, armed_by_id = excluded.armed_by_id,
	  armed_by_kind = excluded.armed_by_kind, arm_token = excluded.arm_token,
	  status = excluded.status, next_attempt_time = excluded.next_attempt_time,
	  attempts = excluded.attempts, lost_owners = excluded.lost_owners,
	  last_attempt_time = excluded.last_attempt_time, last_error = excluded.last_error,
	  failure_reason = excluded.failure_reason, failed_time = excluded.failed_time,
	  partial_commit = excluded.partial_commit, claim_token = excluded.claim_token,
	  claim_owner = excluded.claim_owner`

func copyInt64(p *int64) *int64 {
	if p == nil {
		return nil
	}
	v := *p
	return &v
}

// copyScheduledTask returns a copy of t that shares no pointer with it.
func copyScheduledTask(t spi.ScheduledTask) spi.ScheduledTask {
	cp := t
	cp.TimeoutMs = copyInt64(t.TimeoutMs)
	cp.LastAttemptTime = copyInt64(t.LastAttemptTime)
	cp.FailedTime = copyInt64(t.FailedTime)
	if t.Claim != nil {
		c := *t.Claim
		cp.Claim = &c
	}
	return cp
}

// newLife is the row an arm writes: the caller's schedule fields and a fresh
// life. The tenant and the entity come from the reconcile request; the
// status, the tokens and the run bookkeeping belong to the store. Whatever
// the caller set in those fields of a is ignored.
func newLife(tenant spi.TenantID, entityID string, a spi.ScheduledTask) spi.ScheduledTask {
	return spi.ScheduledTask{
		ID:              a.ID,
		TenantID:        tenant,
		Type:            a.Type,
		ScheduledTime:   a.ScheduledTime,
		TimeoutMs:       copyInt64(a.TimeoutMs),
		EntityID:        entityID,
		ModelName:       a.ModelName,
		ModelVersion:    a.ModelVersion,
		Transition:      a.Transition,
		SourceState:     a.SourceState,
		ArmedAt:         a.ArmedAt,
		ArmedBy:         a.ArmedBy,
		Status:          spi.ScheduledTaskWaiting,
		ArmToken:        uuid.New(),
		NextAttemptTime: a.ScheduledTime,
	}
}

func taskArgs(t spi.ScheduledTask) []any {
	var claimToken, claimOwner any
	if t.Claim != nil {
		claimToken, claimOwner = t.Claim.Token.String(), t.Claim.Owner.String()
	}
	partial := 0
	if t.PartialCommit {
		partial = 1
	}
	return []any{
		t.ID, string(t.TenantID), string(t.Type), t.ScheduledTime, t.TimeoutMs, t.EntityID,
		t.ModelName, t.ModelVersion, t.Transition, t.SourceState, t.ArmedAt, t.ArmedBy.ID,
		string(t.ArmedBy.Kind), t.ArmToken.String(), string(t.Status), t.NextAttemptTime,
		t.Attempts, t.LostOwners, t.LastAttemptTime, t.LastError, string(t.FailureReason),
		t.FailedTime, partial, claimToken, claimOwner,
	}
}

// scanTask scans one row of selectTaskSQL.
func scanTask(scan func(dest ...any) error) (spi.ScheduledTask, error) {
	var t spi.ScheduledTask
	var tenantID, taskType, armedByID, armedByKind, armToken, status, reason string
	var timeoutMs, lastAttempt, failedTime sql.NullInt64
	var claimToken, claimOwner sql.NullString
	var partial, marked int64
	if err := scan(&t.ID, &tenantID, &taskType, &t.ScheduledTime, &timeoutMs, &t.EntityID,
		&t.ModelName, &t.ModelVersion, &t.Transition, &t.SourceState, &t.ArmedAt, &armedByID,
		&armedByKind, &armToken, &status, &t.NextAttemptTime, &t.Attempts, &t.LostOwners,
		&lastAttempt, &t.LastError, &reason, &failedTime, &partial, &claimToken, &claimOwner,
		&marked); err != nil {
		return spi.ScheduledTask{}, err
	}
	var err error
	if t.ArmToken, err = uuid.Parse(armToken); err != nil {
		return spi.ScheduledTask{}, fmt.Errorf("failed to read the arm token of scheduled task %s: %w", t.ID, err)
	}
	if claimToken.Valid {
		token, err := uuid.Parse(claimToken.String)
		if err != nil {
			return spi.ScheduledTask{}, fmt.Errorf("failed to read the claim of scheduled task %s: %w", t.ID, err)
		}
		owner, err := uuid.Parse(claimOwner.String)
		if err != nil {
			return spi.ScheduledTask{}, fmt.Errorf("failed to read the claim owner of scheduled task %s: %w", t.ID, err)
		}
		t.Claim = &spi.TaskClaim{Token: token, Owner: owner}
	}
	if timeoutMs.Valid {
		t.TimeoutMs = &timeoutMs.Int64
	}
	if lastAttempt.Valid {
		t.LastAttemptTime = &lastAttempt.Int64
	}
	if failedTime.Valid {
		t.FailedTime = &failedTime.Int64
	}
	t.TenantID = spi.TenantID(tenantID)
	t.Type = spi.ScheduledTaskType(taskType)
	t.ArmedBy = spi.Principal{ID: armedByID, Kind: spi.PrincipalKind(armedByKind)}
	t.Status = spi.ScheduledTaskStatus(status)
	t.FailureReason = spi.ScheduledTaskFailureReason(reason)
	t.PartialCommit = partial == 1
	t.UnsafeMarked = marked == 1
	return t, nil
}

// readTasks runs a selectTaskSQL query and closes its rows before returning,
// so the caller may issue the next statement on the single writer connection.
func readTasks(ctx context.Context, q queryer, query string, args ...any) ([]spi.ScheduledTask, error) {
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
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

// applyTaskOp writes op through exec.
func applyTaskOp(ctx context.Context, exec execer, op scheduledTaskOp) error {
	switch {
	case op.touch:
		return nil
	case op.after == nil:
		_, err := exec.ExecContext(ctx, `DELETE FROM scheduled_tasks WHERE tenant_id = ? AND id = ?`,
			string(op.key.tenant), op.key.id)
		return err
	default:
		_, err := exec.ExecContext(ctx, upsertTaskSQL, taskArgs(*op.after)...)
		return err
	}
}

// taskView is the set of task rows one call sees: the committed rows, read
// from db, then staged, in order.
type taskView struct {
	ctx    context.Context
	db     *sql.DB
	staged []scheduledTaskOp
}

func (v taskView) get(k taskKey) (spi.ScheduledTask, bool, error) {
	var t spi.ScheduledTask
	found := false
	rows, err := readTasks(v.ctx, v.db, selectTaskSQL+` WHERE t.tenant_id = ? AND t.id = ?`, string(k.tenant), k.id)
	if err != nil {
		return spi.ScheduledTask{}, false, fmt.Errorf("failed to read scheduled task %s: %w", k.id, err)
	}
	if len(rows) == 1 {
		t, found = rows[0], true
	}
	for _, op := range v.staged {
		if op.key != k || op.touch {
			continue
		}
		if op.after == nil {
			found = false
			continue
		}
		t, found = copyScheduledTask(*op.after), true
	}
	return t, found, nil
}

// where returns the rows of tenant that match, as this view sees them, sorted
// by id. filter narrows the committed rows in SQL, with args; match decides
// on every row the view holds, staged ones included.
func (v taskView) where(tenant spi.TenantID, filter string, args []any, match func(spi.ScheduledTask) bool) ([]spi.ScheduledTask, error) {
	committed, err := readTasks(v.ctx, v.db, selectTaskSQL+` WHERE t.tenant_id = ? AND `+filter,
		append([]any{string(tenant)}, args...)...)
	if err != nil {
		return nil, fmt.Errorf("failed to read scheduled tasks: %w", err)
	}
	rows := make(map[taskKey]spi.ScheduledTask, len(committed))
	for _, t := range committed {
		rows[taskKey{tenant: t.TenantID, id: t.ID}] = t
	}
	for _, op := range v.staged {
		if op.key.tenant != tenant || op.touch {
			continue
		}
		if op.after == nil {
			delete(rows, op.key)
			continue
		}
		rows[op.key] = copyScheduledTask(*op.after)
	}
	var out []spi.ScheduledTask
	for _, t := range rows {
		if match(t) {
			out = append(out, t)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// fenced returns the row ref names if its current life and claim are ref's.
// Otherwise, or when the row is missing, the answer is spi.ErrStaleClaim.
func fenced(v taskView, ref spi.TaskRef) (spi.ScheduledTask, error) {
	t, ok, err := v.get(taskKey{tenant: ref.TenantID, id: ref.ID})
	if err != nil {
		return spi.ScheduledTask{}, err
	}
	if !ok || t.ArmToken != ref.ArmToken || t.Status != spi.ScheduledTaskRunning ||
		t.Claim == nil || t.Claim.Token != ref.ClaimToken {
		return spi.ScheduledTask{}, fmt.Errorf("scheduled task %s: %w", ref.ID, spi.ErrStaleClaim)
	}
	return t, nil
}

func removals(ts []spi.ScheduledTask) []scheduledTaskOp {
	ops := make([]scheduledTaskOp, 0, len(ts))
	for _, t := range ts {
		ops = append(ops, scheduledTaskOp{key: taskKey{tenant: t.TenantID, id: t.ID}})
	}
	return ops
}

type scheduledTaskStore struct {
	db     *sql.DB
	readDB *sql.DB
	tm     *transactionManager
	clock  Clock
}

var _ spi.ScheduledTaskStore = (*scheduledTaskStore)(nil)

// write runs one joining write. plan sees the rows as this write sees them
// and returns the ops to apply. With a transaction on ctx the ops are staged
// on it, and plan's view includes the transaction's earlier ops (C2). Without
// one they are written at once, under the commit gate, and commit on their own.
//
// Holding tx.OpMu (read) keeps Commit, Rollback and RollbackToSavepoint of this
// transaction out while plan reads its staged ops. plan does I/O on db, so the
// manager's mu is not held across it.
func (s *scheduledTaskStore) write(ctx context.Context, tenant spi.TenantID, plan func(v taskView) ([]scheduledTaskOp, error)) error {
	tx := spi.GetTransaction(ctx)
	if tx == nil {
		_ = s.tm.acquireCommitGate(context.Background())
		defer s.tm.releaseCommitGate()
		ops, err := plan(taskView{ctx: ctx, db: s.db})
		if err != nil {
			return err
		}
		return s.tm.commitTaskWrites(ctx, ops, nil)
	}

	tx.OpMu.RLock()
	defer tx.OpMu.RUnlock()
	if tx.RolledBack {
		return fmt.Errorf("scheduledTaskStore: %w (txID=%s)", spi.ErrTxRolledBack, tx.ID)
	}
	if tx.Closed {
		return fmt.Errorf("scheduledTaskStore: %w (txID=%s)", spi.ErrTxAlreadyCommitted, tx.ID)
	}
	if tx.TenantID != tenant {
		return fmt.Errorf("scheduledTaskStore: %w (txID=%s)", spi.ErrTxTenantMismatch, tx.ID)
	}
	ops, err := plan(taskView{ctx: ctx, db: s.db, staged: s.tm.stagedTaskOps(tx.ID)})
	if err != nil {
		return err
	}
	s.tm.stageTaskOps(tx.ID, ops)
	return nil
}

// ReconcileForEntity arms req.Arm, each as a new life, and removes every
// other task of the entity. It returns the removed tasks, except those named
// in req.Cancel, which the caller audits on their own.
func (s *scheduledTaskStore) ReconcileForEntity(ctx context.Context, req spi.ReconcileRequest) ([]spi.ScheduledTask, error) {
	var removed []spi.ScheduledTask
	err := s.write(ctx, req.TenantID, func(v taskView) ([]scheduledTaskOp, error) {
		removed = nil
		cancel := make(map[string]bool, len(req.Cancel))
		for _, id := range req.Cancel {
			cancel[id] = true
		}
		armed := make(map[string]bool, len(req.Arm))
		ops := make([]scheduledTaskOp, 0, len(req.Arm))
		for _, a := range req.Arm {
			row := newLife(req.TenantID, req.EntityID, a)
			armed[a.ID] = true
			ops = append(ops, scheduledTaskOp{key: taskKey{tenant: req.TenantID, id: a.ID}, after: &row})
		}
		current, err := v.where(req.TenantID, `t.entity_id = ?`, []any{req.EntityID},
			func(t spi.ScheduledTask) bool { return t.EntityID == req.EntityID })
		if err != nil {
			return nil, err
		}
		for _, t := range current {
			if armed[t.ID] {
				continue
			}
			ops = append(ops, scheduledTaskOp{key: taskKey{tenant: req.TenantID, id: t.ID}})
			if !cancel[t.ID] {
				removed = append(removed, t)
			}
		}
		return ops, nil
	})
	if err != nil {
		return nil, err
	}
	return removed, nil
}

// RemoveLife removes the task if its current life is armToken. Otherwise it
// changes nothing, but the row still enters the transaction's write set.
func (s *scheduledTaskStore) RemoveLife(ctx context.Context, tenant spi.TenantID, id string, armToken uuid.UUID) error {
	k := taskKey{tenant: tenant, id: id}
	return s.write(ctx, tenant, func(v taskView) ([]scheduledTaskOp, error) {
		t, ok, err := v.get(k)
		if err != nil {
			return nil, err
		}
		if ok && t.ArmToken == armToken {
			return []scheduledTaskOp{{key: k}}, nil
		}
		return []scheduledTaskOp{{key: k, touch: true}}, nil
	})
}

// StampSegment writes the task row of ref, and sets PartialCommit when partial.
func (s *scheduledTaskStore) StampSegment(ctx context.Context, ref spi.TaskRef, partial bool) error {
	return s.write(ctx, ref.TenantID, func(v taskView) ([]scheduledTaskOp, error) {
		t, err := fenced(v, ref)
		if err != nil {
			return nil, err
		}
		t.PartialCommit = t.PartialCommit || partial
		return []scheduledTaskOp{{key: taskKey{tenant: ref.TenantID, id: ref.ID}, after: &t}}, nil
	})
}

func (s *scheduledTaskStore) DeleteForEntities(ctx context.Context, tenant spi.TenantID, entityIDs []string) error {
	ids := make(map[string]bool, len(entityIDs))
	for _, id := range entityIDs {
		ids[id] = true
	}
	list, err := json.Marshal(entityIDs)
	if err != nil {
		return fmt.Errorf("failed to encode entity ids: %w", err)
	}
	return s.write(ctx, tenant, func(v taskView) ([]scheduledTaskOp, error) {
		ts, err := v.where(tenant, `t.entity_id IN (SELECT value FROM json_each(?))`, []any{string(list)},
			func(t spi.ScheduledTask) bool { return ids[t.EntityID] })
		if err != nil {
			return nil, err
		}
		return removals(ts), nil
	})
}

// DeleteForModel removes the model's tasks, except those whose (source state,
// transition) keep retains. A nil keep retains none.
func (s *scheduledTaskStore) DeleteForModel(ctx context.Context, tenant spi.TenantID, modelName string, modelVersion int, keep func(sourceState, transition string) bool) error {
	return s.write(ctx, tenant, func(v taskView) ([]scheduledTaskOp, error) {
		ts, err := v.where(tenant, `t.model_name = ? AND t.model_version = ?`, []any{modelName, modelVersion},
			func(t spi.ScheduledTask) bool {
				return t.ModelName == modelName && t.ModelVersion == modelVersion &&
					(keep == nil || !keep(t.SourceState, t.Transition))
			})
		if err != nil {
			return nil, err
		}
		return removals(ts), nil
	})
}

// Fail sets the task of ref to FAILED and clears its claim.
func (s *scheduledTaskStore) Fail(ctx context.Context, ref spi.TaskRef, f spi.Failure) error {
	return s.write(ctx, ref.TenantID, func(v taskView) ([]scheduledTaskOp, error) {
		t, err := fenced(v, ref)
		if err != nil {
			return nil, err
		}
		at := f.AtMs
		t.Status = spi.ScheduledTaskFailed
		t.FailureReason = f.Reason
		t.LastError = f.Error
		t.FailedTime = &at
		t.Claim = nil
		return []scheduledTaskOp{{key: taskKey{tenant: ref.TenantID, id: ref.ID}, after: &t}}, nil
	})
}

// Get reads one task of tenant. With a transaction on ctx it sees that
// transaction's staged ops (C2).
func (s *scheduledTaskStore) Get(ctx context.Context, tenant spi.TenantID, id string) (*spi.ScheduledTask, bool, error) {
	var staged []scheduledTaskOp
	if tx := spi.GetTransaction(ctx); tx != nil {
		tx.OpMu.RLock()
		defer tx.OpMu.RUnlock()
		staged = s.tm.stagedTaskOps(tx.ID)
	}
	t, ok, err := taskView{ctx: ctx, db: s.db, staged: staged}.get(taskKey{tenant: tenant, id: id})
	if err != nil || !ok {
		return nil, false, err
	}
	return &t, true, nil
}

// Query returns one page of tenant's committed tasks in (scheduled_time, id)
// order. id has SQLite's default BINARY collation, so ids compare byte-wise,
// as Go strings (memory) and PostgreSQL's COLLATE "C" do, and every backend
// pages the same way. It never joins a transaction and reads on readDB, so it
// never waits for the writer.
func (s *scheduledTaskStore) Query(ctx context.Context, tenant spi.TenantID, q spi.ScheduledTaskQuery) (spi.ScheduledTaskPage, error) {
	if q.Limit < 1 {
		return spi.ScheduledTaskPage{}, fmt.Errorf("query scheduled tasks: limit must be >= 1, got %d", q.Limit)
	}
	var where strings.Builder
	args := []any{string(tenant)}
	where.WriteString(` WHERE t.tenant_id = ?`)
	if len(q.Statuses) > 0 {
		statuses, err := json.Marshal(q.Statuses)
		if err != nil {
			return spi.ScheduledTaskPage{}, fmt.Errorf("failed to encode statuses: %w", err)
		}
		where.WriteString(` AND t.status IN (SELECT value FROM json_each(?))`)
		args = append(args, string(statuses))
	}
	if q.ModelName != "" {
		where.WriteString(` AND t.model_name = ?`)
		args = append(args, q.ModelName)
	}
	if q.ModelVersion != 0 {
		where.WriteString(` AND t.model_version = ?`)
		args = append(args, q.ModelVersion)
	}
	if q.EntityID != "" {
		where.WriteString(` AND t.entity_id = ?`)
		args = append(args, q.EntityID)
	}
	if q.After != nil {
		where.WriteString(` AND (t.scheduled_time > ? OR (t.scheduled_time = ? AND t.id > ?))`)
		args = append(args, q.After.ScheduledTime, q.After.ScheduledTime, q.After.ID)
	}
	args = append(args, q.Limit+1)
	rows, err := readTasks(ctx, s.readDB, selectTaskSQL+where.String()+` ORDER BY t.scheduled_time, t.id LIMIT ?`, args...)
	if err != nil {
		return spi.ScheduledTaskPage{}, fmt.Errorf("failed to query scheduled tasks: %w", err)
	}
	var page spi.ScheduledTaskPage
	if len(rows) > q.Limit {
		rows = rows[:q.Limit]
		last := rows[q.Limit-1]
		page.Next = &spi.ScheduledTaskCursor{ScheduledTime: last.ScheduledTime, ID: last.ID}
	}
	page.Items = rows
	return page, nil
}
```

Create `plugins/sqlite/scheduled_task_claims.go`:

```go
package sqlite

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// Not implemented yet: each of these answers errors.ErrUnsupported.

func (s *scheduledTaskStore) ClaimDue(context.Context, spi.ClaimRequest) ([]spi.ScheduledTask, error) {
	return nil, errors.ErrUnsupported
}

func (s *scheduledTaskStore) Heartbeat(context.Context, uuid.UUID) error { return errors.ErrUnsupported }

func (s *scheduledTaskStore) RetireOwner(context.Context, uuid.UUID) error {
	return errors.ErrUnsupported
}

func (s *scheduledTaskStore) SweepOwners(context.Context, time.Duration) error {
	return errors.ErrUnsupported
}

func (s *scheduledTaskStore) GiveBackIdle(context.Context, uuid.UUID, []uuid.UUID) (int, error) {
	return 0, errors.ErrUnsupported
}

func (s *scheduledTaskStore) MarkUnsafe(context.Context, spi.TaskRef) error {
	return errors.ErrUnsupported
}

func (s *scheduledTaskStore) RecordAttempt(context.Context, spi.TaskRef, spi.Attempt) error {
	return errors.ErrUnsupported
}

func (s *scheduledTaskStore) SweepMarks(context.Context) error { return errors.ErrUnsupported }
```

In `plugins/sqlite/txmanager.go`, replace `stageScheduledTaskOp` and
`scheduledTaskOpsFor` (`:182-198`) with:

```go
// stagedTaskOps returns a copy of the task-row ops staged for txID, in order.
// Protected by mu.
func (m *transactionManager) stagedTaskOps(txID string) []scheduledTaskOp {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]scheduledTaskOp(nil), m.scheduledTaskOps[txID]...)
}

// stageTaskOps appends ops to txID's staged task-row ops. flushToSQLite writes
// them in the commit's sqlTx; every abort path discards them. Protected by mu.
func (m *transactionManager) stageTaskOps(txID string, ops []scheduledTaskOp) {
	if len(ops) == 0 {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.scheduledTaskOps[txID] = append(m.scheduledTaskOps[txID], ops...)
}

// commitTaskWrites writes task-row ops that commit on their own — a
// never-joining method, or a joining one called without a transaction — in
// one sqlTx of their own. then, when not nil, runs in the same sqlTx after
// the ops. Caller holds the commit gate.
func (m *transactionManager) commitTaskWrites(ctx context.Context, ops []scheduledTaskOp, then func(*sql.Tx) error) error {
	if len(ops) == 0 && then == nil {
		return nil
	}
	sqlTx, err := m.factory.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("failed to begin a scheduled task write: %w", err)
	}
	defer sqlTx.Rollback()
	for _, op := range ops {
		if err := applyTaskOp(ctx, sqlTx, op); err != nil {
			return fmt.Errorf("failed to write scheduled task %s: %w", op.key.id, err)
		}
	}
	if then != nil {
		if err := then(sqlTx); err != nil {
			return err
		}
	}
	if err := sqlTx.Commit(); err != nil {
		return fmt.Errorf("failed to commit a scheduled task write: %w", err)
	}
	return nil
}
```

Replace step 4.5 (`:549-554`) with:

```go
	// 4.5. Snapshot the staged task-row ops. tx.OpMu.Lock (held since step
	// 1b) blocks every stageTaskOps, so the slice is stable.
	scheduledOps := m.stagedTaskOps(txID)
```

Replace the op loop in `flushToSQLite` (`:885-893`) with:

```go
	// Write the staged task-row post-images. Their checks ran when they were
	// staged, and step 3 proved that no other writer changed those rows since
	// this transaction began. Still inside sqlTx, so they commit atomically
	// with the entity write, and every early return rolls them back too.
	for _, op := range scheduledOps {
		if err := applyTaskOp(ctx, sqlTx, op); err != nil {
			return fmt.Errorf("apply scheduled task op %s: %w", op.key.id, err)
		}
	}
```

Rewrite the `scheduledTaskOps` field doc (`:89-101`) to: "holds the task-row
ops staged while the transaction is open, as post-images (see
`scheduledTaskOp`). Written by `flushToSQLite` in the commit's `sqlTx`;
discarded on Rollback and on every abort path; truncated by
`RollbackToSavepoint`. Protected by mu."

In `plugins/sqlite/store_factory.go`, replace the accessor (`:405-411`) with:

```go
// ScheduledTaskStore returns the scheduled-task store. No tenant is resolved
// from ctx: every tenant-facing method takes its tenant as an argument, and
// ClaimDue, GiveBackIdle and the owner and sweep methods are cross-tenant.
func (f *StoreFactory) ScheduledTaskStore(_ context.Context) (spi.ScheduledTaskStore, error) {
	return &scheduledTaskStore{db: f.db, readDB: f.readDB, tm: f.tm, clock: f.clock}, nil
}
```

- [ ] **Step 4: Run the tests and see them pass**

```
cd /Users/paul/go-projects/cyoda-light/cyoda-go/.worktrees/598-scheduler-ownership/plugins/sqlite && go test ./... -skip 'TestConformance'
```

Expected: `ok`. The existing transaction, savepoint and migration-compat tests
run too and stay green.

- [ ] **Step 5: Commit**

```
cd /Users/paul/go-projects/cyoda-light/cyoda-go/.worktrees/598-scheduler-ownership && git add plugins/sqlite/migrations/000009_scheduled_run_ownership.up.sql plugins/sqlite/migrations/000009_scheduled_run_ownership.down.sql plugins/sqlite/scheduled_task_store.go plugins/sqlite/scheduled_task_claims.go plugins/sqlite/txmanager.go plugins/sqlite/store_factory.go plugins/sqlite/scheduled_task_store_test.go plugins/sqlite/migration_000009_internal_test.go && git rm plugins/sqlite/scheduled_task_attribution_internal_test.go && git commit -m "feat(sqlite): scheduled tasks as lives; durable marks and owners

Migration 9 rebuilds scheduled_tasks with a (tenant_id, id) key, the
life and run columns and their checks, keeps pending tasks as new
lives, and adds scheduled_task_marks and scheduler_owners. A joining
write checks against the committed rows plus the transaction's staged
ops and stages the row's post-image; a joining Get sees them.

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task BQ-2: Never-joining methods — owners, claims, marks, attempts, sweeps

**Spec:** as BM-2, plus §10.3 "SQLite: the writer connection, with the claim
following `plugins/sqlite/search_store.go:404-496`" and "A mark survives a
process restart".

**Files:**
- Replace: `plugins/sqlite/scheduled_task_claims.go`
- Create: `plugins/sqlite/scheduled_task_claims_test.go`

**Interfaces:**
- Consumes: BQ-1's view, `fenced`, `readTasks`, `selectTaskSQL`,
  `commitTaskWrites`; the commit gate.
- Produces: `selectClaims(cands []spi.ScheduledTask, req spi.ClaimRequest)
  []spi.ScheduledTask` — the same function, byte for byte, as BM-2's.

- [ ] **Step 1: Write the failing tests**

`plugins/sqlite/scheduled_task_claims_test.go` holds the BM-2 external tests
(`plugins/memory/scheduled_task_claims_test.go` as BM-2 writes it) with
`package sqlite_test` and no other change: `claimDue`, `refOf`,
`TestTasks_ClaimTakesOneTaskPerEntity`,
`TestTasks_ClaimHonoursTenantLimitsAndTurns`,
`TestTasks_ClaimRejectsALimitBelowOne`,
`TestTasks_LostOwnerClaimUsesTheStoreClock`,
`TestTasks_ARetiredOwnerIsLostAtOnce`, `TestTasks_MarkUnsafe`,
`TestTasks_NeverJoiningMethodsIgnoreTheTransaction`, `TestTasks_RecordAttempt`,
`TestTasks_FailOverwritesLastError`, `TestTasks_GiveBackIdleKeepsLiveClaims`.
Append these three SQLite tests:

```go
// Marks and owner liveness are durable: a restart with a mark set ends
// FAILED at the next claim, it is never re-run.
func TestTasks_MarksAndOwnersSurviveARestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "restart.db")
	clock := sqlite.NewTestClockAt(time.UnixMilli(1_000_000))
	open := func() (*sqlite.StoreFactory, spi.ScheduledTaskStore) {
		f, err := sqlite.NewStoreFactoryForTest(context.Background(), path, sqlite.WithClock(clock))
		if err != nil {
			t.Fatalf("NewStoreFactoryForTest: %v", err)
		}
		sts, err := f.ScheduledTaskStore(context.Background())
		if err != nil {
			t.Fatalf("ScheduledTaskStore: %v", err)
		}
		return f, sts
	}
	bg := context.Background()

	f1, sts1 := open()
	arm(t, bg, sts1, taskTenantA, "e1", "T")
	owner := uuid.New()
	if err := sts1.Heartbeat(bg, owner); err != nil {
		t.Fatalf("Heartbeat: %v", err)
	}
	c := claimDue(t, sts1, owner, false)[0]
	if err := sts1.MarkUnsafe(bg, refOf(c)); err != nil {
		t.Fatalf("MarkUnsafe: %v", err)
	}
	if err := f1.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	f2, sts2 := open()
	defer f2.Close()
	got, _ := getTask(t, bg, sts2, taskTenantA, "e1:S:T")
	if got.Status != spi.ScheduledTaskRunning || got.Claim == nil || got.Claim.Owner != owner || !got.UnsafeMarked {
		t.Fatalf("after restart: %+v, want RUNNING under the old owner with the mark", got)
	}
	if again := claimDue(t, sts2, uuid.New(), true); len(again) != 0 {
		t.Fatalf("claimed %+v while the old owner's heartbeat is fresh", again)
	}
	clock.Advance(2 * time.Minute)
	re := claimDue(t, sts2, uuid.New(), true)
	if len(re) != 1 || re[0].LostOwners != 1 || !re[0].UnsafeMarked {
		t.Fatalf("reclaim after restart = %+v, want lostOwners 1 and the mark", re)
	}
}

func TestTasks_SweepsRemoveEndedLivesAndDeadUnusedOwners(t *testing.T) {
	fx := newTaskFixture(t)
	bg := context.Background()
	db := sqlite.DBForTest(fx.f)
	count := func(q string) int {
		t.Helper()
		var n int
		if err := db.QueryRow(q).Scan(&n); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		return n
	}

	arm(t, bg, fx.sts, taskTenantA, "e1", "T")
	busyOwner, idleOwner := uuid.New(), uuid.New()
	for _, o := range []uuid.UUID{busyOwner, idleOwner} {
		if err := fx.sts.Heartbeat(bg, o); err != nil {
			t.Fatalf("Heartbeat: %v", err)
		}
	}
	c := claimDue(t, fx.sts, busyOwner, false)[0]
	if err := fx.sts.MarkUnsafe(bg, refOf(c)); err != nil {
		t.Fatalf("MarkUnsafe: %v", err)
	}
	if err := fx.sts.SweepMarks(bg); err != nil {
		t.Fatalf("SweepMarks: %v", err)
	}
	if n := count(`SELECT count(*) FROM scheduled_task_marks`); n != 1 {
		t.Fatalf("SweepMarks left %d marks, want the live life's 1", n)
	}
	arm(t, bg, fx.sts, taskTenantA, "e1", "T")
	if err := fx.sts.SweepMarks(bg); err != nil {
		t.Fatalf("SweepMarks: %v", err)
	}
	if n := count(`SELECT count(*) FROM scheduled_task_marks`); n != 0 {
		t.Fatalf("SweepMarks kept %d marks of ended lives", n)
	}

	claimDue(t, fx.sts, busyOwner, false)
	fx.clock.Advance(time.Hour)
	if err := fx.sts.SweepOwners(bg, time.Minute); err != nil {
		t.Fatalf("SweepOwners: %v", err)
	}
	if n := count(`SELECT count(*) FROM scheduler_owners WHERE owner = '` + idleOwner.String() + `'`); n != 0 {
		t.Fatal("SweepOwners kept a dead owner no task references")
	}
	if n := count(`SELECT count(*) FROM scheduler_owners WHERE owner = '` + busyOwner.String() + `'`); n != 1 {
		t.Fatal("SweepOwners removed an owner a RUNNING task references")
	}
}

// The unique index on RUNNING rows holds: two claimers never leave an entity
// with two RUNNING tasks.
func TestTasks_ConcurrentClaimsAreDisjoint(t *testing.T) {
	fx := newTaskFixture(t)
	bg := context.Background()
	for i := 0; i < 20; i++ {
		arm(t, bg, fx.sts, taskTenantA, fmt.Sprintf("e%02d", i), "T1", "T2")
	}
	var mu sync.Mutex
	seen := make(map[string]int)
	var wg sync.WaitGroup
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, err := fx.sts.ClaimDue(bg, spi.ClaimRequest{Owner: uuid.New(), NowMs: 2_000, StaleAfter: time.Minute, Limit: 100, PerTenantLimit: 100})
			if err != nil {
				t.Errorf("ClaimDue: %v", err)
				return
			}
			mu.Lock()
			defer mu.Unlock()
			for _, c := range got {
				seen[c.EntityID]++
			}
		}()
	}
	wg.Wait()
	if len(seen) != 20 {
		t.Fatalf("claimed tasks of %d entities, want 20", len(seen))
	}
	for e, n := range seen {
		if n != 1 {
			t.Fatalf("entity %s got %d claims, want 1", e, n)
		}
	}
}
```

Its imports: `context`, `errors`, `fmt`, `path/filepath`, `sync`, `testing`,
`time`, `github.com/google/uuid`, `spi`, and
`github.com/cyoda-platform/cyoda-go/plugins/sqlite`.

- [ ] **Step 2: Run the tests and see them fail**

```
cd /Users/paul/go-projects/cyoda-light/cyoda-go/.worktrees/598-scheduler-ownership/plugins/sqlite && go test ./... -run 'TestTasks_'
```

Expected: the new tests fail with `ClaimDue: unsupported operation` or
`Heartbeat: unsupported operation`; the BQ-1 tests pass.

- [ ] **Step 3: Write the implementation**

Replace `plugins/sqlite/scheduled_task_claims.go` with:

```go
package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/google/uuid"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// The methods in this file never join a transaction: each ignores any
// transaction on ctx and commits on its own on the writer connection. That is
// how a mark survives the rollback of the run's transaction. Every one that
// reads a task row to decide a write holds the commit gate, which serialises
// it with every commit, with Begin, and with the others — MarkUnsafe with
// ClaimDue in particular (C3).

func (s *scheduledTaskStore) Heartbeat(ctx context.Context, owner uuid.UUID) error {
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO scheduler_owners (owner, heartbeat_at) VALUES (?, ?)
		 ON CONFLICT (owner) DO UPDATE SET heartbeat_at = excluded.heartbeat_at`,
		owner.String(), s.clock.Now().UnixMicro()); err != nil {
		return fmt.Errorf("failed to record a heartbeat: %w", err)
	}
	return nil
}

func (s *scheduledTaskStore) RetireOwner(ctx context.Context, owner uuid.UUID) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM scheduler_owners WHERE owner = ?`, owner.String()); err != nil {
		return fmt.Errorf("failed to retire an owner: %w", err)
	}
	return nil
}

// SweepOwners removes the liveness record of every owner that has not
// heartbeated for deadFor, once no RUNNING task references it.
func (s *scheduledTaskStore) SweepOwners(ctx context.Context, deadFor time.Duration) error {
	if _, err := s.db.ExecContext(ctx,
		`DELETE FROM scheduler_owners
		 WHERE heartbeat_at < ?
		   AND NOT EXISTS (SELECT 1 FROM scheduled_tasks t
		                   WHERE t.status = 'RUNNING' AND t.claim_owner = scheduler_owners.owner)`,
		s.clock.Now().Add(-deadFor).UnixMicro()); err != nil {
		return fmt.Errorf("failed to sweep owners: %w", err)
	}
	return nil
}

// ClaimDue claims due tasks for req.Owner, in one sqlTx under the commit
// gate: scan the candidates, choose, write the claims. A WAITING task is due
// when its next_attempt_time is at or before req.NowMs (the pnode clock).
// With AllowLostOwner, a RUNNING task whose owner is stale by the store clock
// is claimable too, and the claim adds 1 to its lost_owners. A task is never
// claimed while another task of its entity is RUNNING.
func (s *scheduledTaskStore) ClaimDue(ctx context.Context, req spi.ClaimRequest) ([]spi.ScheduledTask, error) {
	if req.Limit < 1 || req.PerTenantLimit < 1 {
		return nil, fmt.Errorf("claim due scheduled tasks: limit and per-tenant limit must be >= 1, got %d and %d", req.Limit, req.PerTenantLimit)
	}
	_ = s.tm.acquireCommitGate(context.Background())
	defer s.tm.releaseCommitGate()

	allowLost := 0
	if req.AllowLostOwner {
		allowLost = 1
	}
	cands, err := readTasks(ctx, s.db, selectTaskSQL+`
		WHERE ((t.status = 'WAITING' AND t.next_attempt_time <= ?)
		    OR (? = 1 AND t.status = 'RUNNING' AND NOT EXISTS (
		          SELECT 1 FROM scheduler_owners o
		          WHERE o.owner = t.claim_owner AND o.heartbeat_at >= ?)))
		  AND NOT EXISTS (
		          SELECT 1 FROM scheduled_tasks r
		          WHERE r.tenant_id = t.tenant_id AND r.entity_id = t.entity_id
		            AND r.status = 'RUNNING' AND r.id <> t.id)`,
		req.NowMs, allowLost, s.clock.Now().Add(-req.StaleAfter).UnixMicro())
	if err != nil {
		return nil, fmt.Errorf("failed to scan due scheduled tasks: %w", err)
	}

	chosen := selectClaims(cands, req)
	ops := make([]scheduledTaskOp, 0, len(chosen))
	for _, c := range chosen {
		t := copyScheduledTask(c)
		if t.Status == spi.ScheduledTaskRunning {
			t.LostOwners++
		}
		t.Status = spi.ScheduledTaskRunning
		t.Claim = &spi.TaskClaim{Token: uuid.New(), Owner: req.Owner}
		ops = append(ops, scheduledTaskOp{key: taskKey{tenant: t.TenantID, id: t.ID}, after: &t})
	}
	if err := s.tm.commitTaskWrites(ctx, ops, nil); err != nil {
		return nil, err
	}
	out := make([]spi.ScheduledTask, 0, len(ops))
	for _, op := range ops {
		out = append(out, copyScheduledTask(*op.after))
	}
	return out, nil
}

// selectClaims picks the tasks one ClaimDue call takes from cands: one per
// entity, at most PerTenantLimit − TenantInProgress per tenant, at most Limit
// in all. Within a tenant the order is (NextAttemptTime, ID). Tenants take
// turns, one task per turn; the tenant with the earliest candidate goes
// first, ties broken by tenant id. The memory plugin has the same function.
func selectClaims(cands []spi.ScheduledTask, req spi.ClaimRequest) []spi.ScheduledTask {
	sort.Slice(cands, func(i, j int) bool {
		a, b := cands[i], cands[j]
		if a.NextAttemptTime != b.NextAttemptTime {
			return a.NextAttemptTime < b.NextAttemptTime
		}
		if a.TenantID != b.TenantID {
			return a.TenantID < b.TenantID
		}
		return a.ID < b.ID
	})
	var tenants []spi.TenantID
	queues := make(map[spi.TenantID][]spi.ScheduledTask)
	for _, c := range cands {
		if _, ok := queues[c.TenantID]; !ok {
			tenants = append(tenants, c.TenantID)
		}
		queues[c.TenantID] = append(queues[c.TenantID], c)
	}
	quota := make(map[spi.TenantID]int, len(tenants))
	for _, tn := range tenants {
		quota[tn] = req.PerTenantLimit - req.TenantInProgress[tn]
	}

	type entityKey struct {
		tenant spi.TenantID
		id     string
	}
	seen := make(map[entityKey]bool)
	var out []spi.ScheduledTask
	for progress := true; progress && len(out) < req.Limit; {
		progress = false
		for _, tn := range tenants {
			if len(out) >= req.Limit {
				break
			}
			for quota[tn] > 0 && len(queues[tn]) > 0 {
				c := queues[tn][0]
				queues[tn] = queues[tn][1:]
				ek := entityKey{tenant: tn, id: c.EntityID}
				if seen[ek] {
					continue
				}
				seen[ek] = true
				quota[tn]--
				out = append(out, c)
				progress = true
				break
			}
		}
	}
	return out
}

// GiveBackIdle returns to WAITING, uncounted, every task RUNNING under owner
// whose claim token is not in keep.
func (s *scheduledTaskStore) GiveBackIdle(ctx context.Context, owner uuid.UUID, keep []uuid.UUID) (int, error) {
	kept := make(map[uuid.UUID]bool, len(keep))
	for _, k := range keep {
		kept[k] = true
	}
	_ = s.tm.acquireCommitGate(context.Background())
	defer s.tm.releaseCommitGate()

	running, err := readTasks(ctx, s.db, selectTaskSQL+` WHERE t.status = 'RUNNING' AND t.claim_owner = ?`, owner.String())
	if err != nil {
		return 0, fmt.Errorf("failed to read an owner's running tasks: %w", err)
	}
	var ops []scheduledTaskOp
	for _, t := range running {
		if kept[t.Claim.Token] {
			continue
		}
		back := copyScheduledTask(t)
		back.Status = spi.ScheduledTaskWaiting
		back.Claim = nil
		ops = append(ops, scheduledTaskOp{key: taskKey{tenant: t.TenantID, id: t.ID}, after: &back})
	}
	if err := s.tm.commitTaskWrites(ctx, ops, nil); err != nil {
		return 0, err
	}
	return len(ops), nil
}

// MarkUnsafe writes the mark of ref's life, naming ref's claim. It is
// idempotent for the same claim.
func (s *scheduledTaskStore) MarkUnsafe(ctx context.Context, ref spi.TaskRef) error {
	_ = s.tm.acquireCommitGate(context.Background())
	defer s.tm.releaseCommitGate()

	if _, err := fenced(taskView{ctx: ctx, db: s.db}, ref); err != nil {
		return err
	}
	var holder string
	err := s.db.QueryRowContext(ctx,
		`SELECT claim_token FROM scheduled_task_marks WHERE tenant_id = ? AND task_id = ? AND arm_token = ?`,
		string(ref.TenantID), ref.ID, ref.ArmToken.String()).Scan(&holder)
	switch {
	case errors.Is(err, sql.ErrNoRows):
	case err != nil:
		return fmt.Errorf("failed to read the mark of scheduled task %s: %w", ref.ID, err)
	case holder == ref.ClaimToken.String():
		return nil
	default:
		return fmt.Errorf("scheduled task %s: %w", ref.ID, spi.ErrMarkedByAnotherClaim)
	}
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO scheduled_task_marks (tenant_id, task_id, arm_token, claim_token) VALUES (?, ?, ?, ?)`,
		string(ref.TenantID), ref.ID, ref.ArmToken.String(), ref.ClaimToken.String()); err != nil {
		return fmt.Errorf("failed to mark scheduled task %s: %w", ref.ID, err)
	}
	return nil
}

// RecordAttempt ends ref's claim: the task goes back to WAITING with the
// attempt recorded. With ClearOwnMark it also removes the mark this claim
// wrote, in the same sqlTx.
func (s *scheduledTaskStore) RecordAttempt(ctx context.Context, ref spi.TaskRef, a spi.Attempt) error {
	_ = s.tm.acquireCommitGate(context.Background())
	defer s.tm.releaseCommitGate()

	t, err := fenced(taskView{ctx: ctx, db: s.db}, ref)
	if err != nil {
		return err
	}
	if !a.NotCounted {
		t.Attempts++
	}
	at := a.AtMs
	t.Status = spi.ScheduledTaskWaiting
	t.Claim = nil
	t.LastAttemptTime = &at
	t.LastError = a.Error
	t.NextAttemptTime = a.NextAttemptTime
	op := scheduledTaskOp{key: taskKey{tenant: ref.TenantID, id: ref.ID}, after: &t}
	return s.tm.commitTaskWrites(ctx, []scheduledTaskOp{op}, func(sqlTx *sql.Tx) error {
		if !a.ClearOwnMark {
			return nil
		}
		if _, err := sqlTx.ExecContext(ctx,
			`DELETE FROM scheduled_task_marks
			 WHERE tenant_id = ? AND task_id = ? AND arm_token = ? AND claim_token = ?`,
			string(ref.TenantID), ref.ID, ref.ArmToken.String(), ref.ClaimToken.String()); err != nil {
			return fmt.Errorf("failed to clear the mark of scheduled task %s: %w", ref.ID, err)
		}
		return nil
	})
}

// SweepMarks removes the marks of ended lives: those whose task is gone or
// has been re-armed.
func (s *scheduledTaskStore) SweepMarks(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx,
		`DELETE FROM scheduled_task_marks
		 WHERE NOT EXISTS (SELECT 1 FROM scheduled_tasks t
		                   WHERE t.tenant_id = scheduled_task_marks.tenant_id
		                     AND t.id = scheduled_task_marks.task_id
		                     AND t.arm_token = scheduled_task_marks.arm_token)`); err != nil {
		return fmt.Errorf("failed to sweep marks: %w", err)
	}
	return nil
}
```

`Heartbeat`, `RetireOwner`, `SweepOwners` and `SweepMarks` do not take the
commit gate: none of them writes a task row, and each is one statement.
`t.Claim` is not nil-checked for a RUNNING row: the table's `CHECK` makes a
RUNNING row without a claim unstorable.

The candidate scan reads every due task. SQLite serves one pnode, and the
memory plugin scans the same way; a `LIMIT` there would drop tenants the
turn-taking must see.

- [ ] **Step 4: Run the tests and see them pass**

```
cd /Users/paul/go-projects/cyoda-light/cyoda-go/.worktrees/598-scheduler-ownership/plugins/sqlite && go test ./... -skip 'TestConformance'
```

Expected: `ok`. Exit check, must print nothing:

```
cd /Users/paul/go-projects/cyoda-light/cyoda-go/.worktrees/598-scheduler-ownership && grep -n 'ErrUnsupported' plugins/sqlite/*.go
```

- [ ] **Step 5: Commit**

```
cd /Users/paul/go-projects/cyoda-light/cyoda-go/.worktrees/598-scheduler-ownership && git add plugins/sqlite/scheduled_task_claims.go plugins/sqlite/scheduled_task_claims_test.go && git commit -m "feat(sqlite): claims, owner liveness, marks and attempts

ClaimDue scans, chooses and writes its claims in one sqlTx under the
commit gate: one task per entity, per-tenant limits with turns, lost
owners by the store clock. Marks and heartbeats are durable, so a
restart with a mark set is never re-run.

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task BQ-3: First-committer-wins ordered by a sequence number

**Spec:** §10.3 C1 bullet "The check orders by a sequence number … SQLite's
submit-time comparison (`plugins/sqlite/txmanager.go:427-431, 525`) is
replaced"; §16 V1; §13 row "SQLite: a run never conflicts with its own claim
under a frozen clock" (the entity half; BQ-4 adds the task half).

**Files:**
- Modify: `plugins/sqlite/txmanager.go` (`committedTx` `:18-23`; struct
  `:66-141`; constructor `:146-160`; Begin `:420-433`; Commit doc `:63-65`,
  step 3 `:513-543`, flush-failure cleanup `:559-570`, step 6 `:576-622`;
  Rollback `:920-931`)
- Create: `plugins/sqlite/txmanager_seq_test.go`

**Interfaces:**
- Consumes: nothing new.
- Produces: `committedTx.seq`, `transactionManager.commitSeq`,
  `txSnapshotSeq`, `(*transactionManager).forgetLocked(txID)`,
  `pruneCommittedLogLocked()`.

- [ ] **Step 1: Write the failing tests**

`plugins/sqlite/txmanager_seq_test.go`:

```go
package sqlite_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/plugins/sqlite"
)

func seqEntity(id string) *spi.Entity {
	return &spi.Entity{
		Meta: spi.EntityMeta{ID: id, ModelRef: spi.ModelRef{EntityName: "SeqModel", ModelVersion: "1"}},
		Data: []byte(`{"n":1}`),
	}
}

func newFrozenTM(t *testing.T) (*sqlite.StoreFactory, spi.TransactionManager, context.Context) {
	t.Helper()
	clock := sqlite.NewTestClock() // never advanced
	f, err := sqlite.NewStoreFactoryForTest(context.Background(), filepath.Join(t.TempDir(), "seq.db"), sqlite.WithClock(clock))
	if err != nil {
		t.Fatalf("NewStoreFactoryForTest: %v", err)
	}
	t.Cleanup(func() { _ = f.Close() })
	ctx := testCtx("tenant-A")
	tm, err := f.TransactionManager(ctx)
	if err != nil {
		t.Fatalf("TransactionManager: %v", err)
	}
	return f, tm, ctx
}

func beginSave(t *testing.T, f *sqlite.StoreFactory, tm spi.TransactionManager, ctx context.Context, id string) string {
	t.Helper()
	txID, txCtx, err := tm.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	es, err := f.EntityStore(txCtx)
	if err != nil {
		t.Fatalf("EntityStore: %v", err)
	}
	if _, err := es.Save(txCtx, seqEntity(id)); err != nil {
		t.Fatalf("Save: %v", err)
	}
	return txID
}

// Under a frozen clock a commit before Begin carries the same instant as the
// snapshot. It precedes the transaction and must not conflict with it.
func TestFCW_ACommitBeforeBeginDoesNotConflictUnderAFrozenClock(t *testing.T) {
	f, tm, ctx := newFrozenTM(t)
	holdID, _, err := tm.Begin(ctx) // keeps the committed log from being pruned
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	t.Cleanup(func() { _ = tm.Rollback(ctx, holdID) })

	if err := tm.Commit(ctx, beginSave(t, f, tm, ctx, "e1")); err != nil {
		t.Fatalf("first Commit: %v", err)
	}
	if err := tm.Commit(ctx, beginSave(t, f, tm, ctx, "e1")); err != nil {
		t.Fatalf("a transaction begun after the first commit conflicted with it: %v", err)
	}
}

// Two overlapping transactions on one entity still conflict.
func TestFCW_OverlappingWritersStillConflictUnderAFrozenClock(t *testing.T) {
	f, tm, ctx := newFrozenTM(t)
	a := beginSave(t, f, tm, ctx, "e1")
	b := beginSave(t, f, tm, ctx, "e1")
	if err := tm.Commit(ctx, a); err != nil {
		t.Fatalf("first Commit: %v", err)
	}
	if err := tm.Commit(ctx, b); !errors.Is(err, spi.ErrConflict) {
		t.Fatalf("second Commit = %v, want ErrConflict", err)
	}
}
```

- [ ] **Step 2: Run the tests and see them fail**

```
cd /Users/paul/go-projects/cyoda-light/cyoda-go/.worktrees/598-scheduler-ownership/plugins/sqlite && go test ./... -run 'TestFCW_'
```

Expected: `TestFCW_ACommitBeforeBeginDoesNotConflictUnderAFrozenClock` fails
with `a transaction begun after the first commit conflicted with it: conflict`
(the text of `spi.ErrConflict`). The overlapping case passes.

- [ ] **Step 3: Write the implementation**

In `plugins/sqlite/txmanager.go`:

Replace `committedTx` (`:18-23`) with:

```go
// committedTx records a committed write in the in-memory log. seq orders the
// log: the conflict check compares it with the sequence number a transaction
// saw at Begin (txSnapshotSeq), never with a clock reading. Begin reserves its
// snapshot as the submit-time floor, so under a frozen clock a commit that
// preceded Begin can carry the same instant as the snapshot; the sequence
// number still orders them.
type committedTx struct {
	id       string
	seq      int64
	writeSet map[string]bool
}
```

Add to `transactionManager` (after `lastSubmitTime`, `:80`):

```go
	// commitSeq counts committed writes; txSnapshotSeq holds its value at
	// each open transaction's Begin. Both are read and written under mu by
	// callers holding the commit gate, so every Begin is ordered wholly
	// before or wholly after every commit.
	commitSeq     int64
	txSnapshotSeq map[string]int64 // txID → commitSeq at Begin; removed by forgetLocked
```

and `txSnapshotSeq: make(map[string]int64),` to `newTransactionManager`
(`:146-160`).

In Begin's gated closure (`:425-432`), after `m.active[txID] = tx`, add
`m.txSnapshotSeq[txID] = m.commitSeq`.

Add:

```go
// forgetLocked drops every piece of per-transaction state the manager holds
// for txID. Caller holds mu.
func (m *transactionManager) forgetLocked(txID string) {
	delete(m.active, txID)
	delete(m.committing, txID)
	delete(m.savepoints, txID)
	delete(m.txUniqueKeys, txID)
	delete(m.txSnapshotSeq, txID)
	delete(m.scheduledTaskOps, txID)
	delete(m.supersededSaves, txID)
	delete(m.deletedBufferedEntities, txID)
}

// pruneCommittedLogLocked drops the log entries no open transaction can
// conflict with: those at or below the oldest open snapshot's sequence
// number, or all of them when no transaction is open. Caller holds mu.
func (m *transactionManager) pruneCommittedLogLocked() {
	if len(m.active) == 0 {
		m.committedLog = m.committedLog[:0]
		return
	}
	oldest := int64(-1)
	for txID := range m.active {
		if s := m.txSnapshotSeq[txID]; oldest < 0 || s < oldest {
			oldest = s
		}
	}
	pruned := m.committedLog[:0]
	for _, c := range m.committedLog {
		if c.seq > oldest {
			pruned = append(pruned, c)
		}
	}
	m.committedLog = pruned
}
```

Replace step 3 (`:513-543`) with:

```go
	// 3. Conflict detection. A transaction conflicts with every commit whose
	// sequence number is above the one it saw at Begin and whose write set
	// meets its read or write set.
	if err := func() error {
		m.mu.Lock()
		defer m.mu.Unlock()
		snapshotSeq := m.txSnapshotSeq[txID]
		for _, committed := range m.committedLog {
			if committed.seq > snapshotSeq {
				for entityID := range committed.writeSet {
					if tx.ReadSet[entityID] || tx.WriteSet[entityID] {
						m.forgetLocked(txID)
						return spi.ErrConflict
					}
				}
			}
		}
		return nil
	}(); err != nil {
		return err
	}
```

In the flush-failure cleanup (`:559-570`), keep `tx.RolledBack = true` and
replace the seven `delete` lines with `m.forgetLocked(txID)`. Do the same in
Rollback (`:920-931`).

Replace the body of step 6 (`:577-621`) with:

```go
		m.mu.Lock()
		defer m.mu.Unlock()

		m.commitSeq++
		m.committedLog = append(m.committedLog, committedTx{
			id:       txID,
			seq:      m.commitSeq,
			writeSet: tx.WriteSet,
		})
		m.submitTimes[txID] = submitTimeEntry{submitTime: submitTime, tenantID: tx.TenantID}

		// Evict old submit times beyond TTL.
		evictBefore := m.factory.clock.Now().Add(-submitTimeTTL)
		for id, e := range m.submitTimes {
			if e.submitTime.Before(evictBefore) {
				delete(m.submitTimes, id)
			}
		}

		m.forgetLocked(txID)
		m.pruneCommittedLogLocked()
```

Replace the commit-ordering line of the type doc (`:63-65`) with: "Commit
ordering: acquire the commit gate -> validate SI+FCW by sequence number ->
capture submitTime -> BEGIN IMMEDIATE -> flush -> COMMIT -> append
committedLog with the next sequence number -> prune -> release the commit
gate."

- [ ] **Step 4: Run the tests and see them pass**

```
cd /Users/paul/go-projects/cyoda-light/cyoda-go/.worktrees/598-scheduler-ownership/plugins/sqlite && go test ./... -skip 'TestConformance'
```

Expected: `ok`. The existing transaction, CAS-gate, savepoint and stress tests
(`txmanager_begin_gate_internal_test.go`, `entity_store_cas_gate_internal_test.go`,
`concurrency_savepoint_test.go`, `stress_test.go`) prove the entity check
still refuses every real conflict.

- [ ] **Step 5: Commit**

```
cd /Users/paul/go-projects/cyoda-light/cyoda-go/.worktrees/598-scheduler-ownership && git add plugins/sqlite/txmanager.go plugins/sqlite/txmanager_seq_test.go && git commit -m "fix(sqlite): first-committer-wins orders by a sequence number

Begin reserves its snapshot as the submit-time floor, so under a
frozen clock a commit that preceded Begin carried the snapshot's own
instant and the >= comparison made the later transaction conflict
with it. Commits now take a sequence number under the commit gate and
the check compares it with the one each transaction saw at Begin, as
the memory plugin does.

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task BQ-4: C1 — task rows under first-committer-wins

**Spec:** §10.1 C1, C5; §10.3 C1 bullets; §5.2; §13 rows "a reclaimed or
re-armed task makes the old run's commit fail (C1)", "SQLite: a run never
conflicts with its own claim under a frozen clock".

**Files:**
- Modify: `plugins/sqlite/txmanager.go` (`committedTx`; step 3; step 4.5 from
  BQ-1; step 6; `commitTaskWrites`)
- Modify: `plugins/sqlite/export_test.go` (`CommittedLogLenForTest`)
- Create: `plugins/sqlite/scheduled_task_c1_test.go`

**Interfaces:**
- Consumes: BQ-1 ops, BQ-3 sequence number.
- Produces: `committedTx.taskWrites`, `taskWriteSet(ops) map[taskKey]bool`,
  `(*transactionManager).logTaskWritesLocked(map[taskKey]bool)`.

- [ ] **Step 1: Write the failing tests**

`plugins/sqlite/scheduled_task_c1_test.go` holds the BM-3 tests
(`plugins/memory/scheduled_task_c1_test.go` as BM-3 writes it) with
`package sqlite_test`, the import of `plugins/memory` replaced by
`plugins/sqlite`, and in `TestTasks_C1_WritesWithNoOpenTransactionLeaveNoLogEntries`, the
`tm := fx.f.GetTransactionManager().(*memory.TransactionManager)` line and the
`if` block after it replaced by:

```go
	if n := sqlite.CommittedLogLenForTest(fx.f); n != 0 {
		t.Fatalf("committed log holds %d entries with no transaction open, want 0", n)
	}
```

Add to `plugins/sqlite/export_test.go`:

```go
// CommittedLogLenForTest reports the length of the factory's committed log.
func CommittedLogLenForTest(f *StoreFactory) int {
	return f.tm.CommittedLogLen()
}
```

- [ ] **Step 2: Run the tests and see them fail**

```
cd /Users/paul/go-projects/cyoda-light/cyoda-go/.worktrees/598-scheduler-ownership/plugins/sqlite && go test ./... -run 'TestTasks_C1_'
```

Expected: `AClaimAfterBeginFailsTheCommit`,
`TwoTransactionsWritingOneRow_TheSecondCommitConflicts` and
`RemoveLifeOfALifeReplacedAfterBeginConflicts` fail with `Commit = <nil>, want
ErrConflict`. The other three pass.

- [ ] **Step 3: Write the implementation**

In `plugins/sqlite/txmanager.go`, add `taskWrites` to `committedTx`:

```go
	// taskWrites holds the task rows the write changed. It is apart from
	// writeSet, whose keys are entity ids, so the two checks never mix. A
	// write that committed on its own (commitTaskWrites) has only taskWrites.
	taskWrites map[taskKey]bool
```

Add:

```go
// taskWriteSet returns the task rows ops write, touches included.
func taskWriteSet(ops []scheduledTaskOp) map[taskKey]bool {
	if len(ops) == 0 {
		return nil
	}
	set := make(map[taskKey]bool, len(ops))
	for _, op := range ops {
		set[op.key] = true
	}
	return set
}

// logTaskWritesLocked records a task-row write that committed on its own, so
// that a transaction that began before it and writes one of the same rows
// fails at commit (C1). Caller holds the commit gate — which Begin also
// takes, so the write and its entry are ordered wholly before or wholly
// after any Begin — and mu.
func (m *transactionManager) logTaskWritesLocked(keys map[taskKey]bool) {
	m.commitSeq++
	m.committedLog = append(m.committedLog, committedTx{seq: m.commitSeq, taskWrites: keys})
	m.pruneCommittedLogLocked()
}
```

In `commitTaskWrites` (BQ-1), after the successful `sqlTx.Commit()`, before
`return nil`:

```go
	keys := taskWriteSet(ops)
	if len(keys) > 0 {
		m.mu.Lock()
		defer m.mu.Unlock()
		m.logTaskWritesLocked(keys)
	}
```

Replace step 3 (as BQ-3 left it) and delete step 4.5:

```go
	// 3. Conflict detection. A transaction conflicts with every commit whose
	// sequence number is above the one it saw at Begin and whose write set
	// meets its read or write set, or whose task writes meet its own. The
	// staged task-row ops are captured here: tx.OpMu.Lock (step 1b) blocks
	// every stageTaskOps, so they are stable for the rest of the commit.
	var scheduledOps []scheduledTaskOp
	if err := func() error {
		m.mu.Lock()
		defer m.mu.Unlock()
		scheduledOps = append([]scheduledTaskOp(nil), m.scheduledTaskOps[txID]...)
		taskWrites := taskWriteSet(scheduledOps)
		snapshotSeq := m.txSnapshotSeq[txID]
		for _, committed := range m.committedLog {
			if committed.seq <= snapshotSeq {
				continue
			}
			conflict := false
			for entityID := range committed.writeSet {
				if tx.ReadSet[entityID] || tx.WriteSet[entityID] {
					conflict = true
					break
				}
			}
			for k := range committed.taskWrites {
				if taskWrites[k] {
					conflict = true
					break
				}
			}
			if conflict {
				m.forgetLocked(txID)
				return spi.ErrConflict
			}
		}
		return nil
	}(); err != nil {
		return err
	}
```

In step 6, add `taskWrites: taskWriteSet(scheduledOps),` to the appended
`committedTx`.

- [ ] **Step 4: Run the tests and see them pass**

```
cd /Users/paul/go-projects/cyoda-light/cyoda-go/.worktrees/598-scheduler-ownership/plugins/sqlite && go test ./... -skip 'TestConformance'
```

Expected: `ok`, including `TestTasks_C1_ARunDoesNotConflictWithItsOwnClaimUnderAFrozenClock`.

- [ ] **Step 5: Commit**

```
cd /Users/paul/go-projects/cyoda-light/cyoda-go/.worktrees/598-scheduler-ownership && git add plugins/sqlite/txmanager.go plugins/sqlite/export_test.go plugins/sqlite/scheduled_task_c1_test.go && git commit -m "feat(sqlite): first-committer-wins covers task rows

Task rows join the commit's conflict check in a set of their own, next
to the entity ids. A task-row write that commits on its own takes a
sequence number and a log entry before it releases the commit gate.

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task BQ-5: C6 — a row an open transaction wrote is busy

**Spec:** as BM-4.

**Files:**
- Modify: `plugins/sqlite/txmanager.go` (new `busyTaskKeys`)
- Modify: `plugins/sqlite/scheduled_task_claims.go` (`ClaimDue`,
  `GiveBackIdle`, `MarkUnsafe`, `RecordAttempt`)
- Create: `plugins/sqlite/scheduled_task_busy_test.go`

**Interfaces:**
- Consumes: BQ-1 staged ops; `spi.ErrTaskBusy`.
- Produces: `(*transactionManager).busyTaskKeys() map[taskKey]bool`.

- [ ] **Step 1: Write the failing tests**

`plugins/sqlite/scheduled_task_busy_test.go` holds the BM-4 tests
(`plugins/memory/scheduled_task_busy_test.go` as BM-4 writes it) with
`package sqlite_test` and no other change.

- [ ] **Step 2: Run the tests and see them fail**

```
cd /Users/paul/go-projects/cyoda-light/cyoda-go/.worktrees/598-scheduler-ownership/plugins/sqlite && go test ./... -run 'TestTasks_C6_'
```

Expected: as BM-4 Step 2.

- [ ] **Step 3: Write the implementation**

Add to `plugins/sqlite/txmanager.go`:

```go
// busyTaskKeys returns the task rows an open transaction has staged a change
// to. Such a row is not claimable, and MarkUnsafe and RecordAttempt answer
// spi.ErrTaskBusy for it, until the transaction ends (C6). A touch is not a
// change. Callers hold the commit gate, so no Commit is between reading its
// ops and writing them. A transaction that stages an op after this call
// began before the caller's write, so its commit fails (C1).
func (m *transactionManager) busyTaskKeys() map[taskKey]bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	busy := make(map[taskKey]bool)
	for _, ops := range m.scheduledTaskOps {
		for _, op := range ops {
			if !op.touch {
				busy[op.key] = true
			}
		}
	}
	return busy
}
```

In `plugins/sqlite/scheduled_task_claims.go`:

- `ClaimDue`: after the candidate scan, drop busy rows before choosing:
  ```go
  	busy := s.tm.busyTaskKeys()
  	free := cands[:0]
  	for _, c := range cands {
  		if !busy[taskKey{tenant: c.TenantID, id: c.ID}] {
  			free = append(free, c)
  		}
  	}
  	chosen := selectClaims(free, req)
  ```
- `GiveBackIdle`: add `busy := s.tm.busyTaskKeys()` after the scan and change
  the skip to `if kept[t.Claim.Token] || busy[taskKey{tenant: t.TenantID, id: t.ID}] {`.
- `MarkUnsafe` and `RecordAttempt`: right after the `fenced` check, add
  ```go
  	if s.tm.busyTaskKeys()[taskKey{tenant: ref.TenantID, id: ref.ID}] {
  		return fmt.Errorf("scheduled task %s: %w", ref.ID, spi.ErrTaskBusy)
  	}
  ```

Extend the file's top comment with: "A row an open transaction has staged a
change to is busy (C6): `ClaimDue` and `GiveBackIdle` skip it; `MarkUnsafe`
and `RecordAttempt` answer `spi.ErrTaskBusy`, which the caller retries."

- [ ] **Step 4: Run the tests and see them pass**

```
cd /Users/paul/go-projects/cyoda-light/cyoda-go/.worktrees/598-scheduler-ownership/plugins/sqlite && go test ./... -skip 'TestConformance'
```

Expected: `ok`.

- [ ] **Step 5: Commit**

```
cd /Users/paul/go-projects/cyoda-light/cyoda-go/.worktrees/598-scheduler-ownership && git add plugins/sqlite/txmanager.go plugins/sqlite/scheduled_task_claims.go plugins/sqlite/scheduled_task_busy_test.go && git commit -m "feat(sqlite): a task row an open transaction wrote is busy

A row with a staged change is not claimable and not given back;
MarkUnsafe and RecordAttempt answer ErrTaskBusy for it.

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task BQ-6: `ErrStoreRejected` — input no backend stores, and deterministic SQLite errors

**Spec:** §5.6 "Every store sets it"; §5.8; §13 row "`spi.ErrStoreRejected`
… (every backend sets the marker)".

**Files:**
- Create: `plugins/sqlite/scheduled_task_validate.go`
- Modify: `plugins/sqlite/errors.go` (new `classifyRejection`)
- Modify: `plugins/sqlite/scheduled_task_store.go` (`applyTaskOp`,
  `ReconcileForEntity`, `Fail`)
- Modify: `plugins/sqlite/scheduled_task_claims.go` (`RecordAttempt`,
  `MarkUnsafe`)
- Modify: `plugins/sqlite/export_test.go` (`ClassifyRejectionForTest`)
- Create: `plugins/sqlite/scheduled_task_rejected_test.go`

**Interfaces:**
- Consumes: `spi.ErrStoreRejected`; `sqlite3.CONSTRAINT`, `TOOBIG`,
  `MISMATCH`, `RANGE`.
- Produces: `rejectErrorText`, `rejectFailureReason`, `rejectArm`,
  `maxTaskErrorBytes` (same rules as BM-5); `classifyRejection(error) error`,
  which BQ-7 also uses for the audit insert.

- [ ] **Step 1: Write the failing tests**

`plugins/sqlite/scheduled_task_rejected_test.go` holds the BM-5 test
(`plugins/memory/scheduled_task_rejected_test.go` as BM-5 writes it) with
`package sqlite_test`, and this second test appended:

```go
// A write SQLite itself refuses — a CHECK, NOT NULL or type violation — is
// deterministic: retrying it cannot succeed.
func TestTasks_SQLiteConstraintErrorsAreStoreRejections(t *testing.T) {
	fx := newTaskFixture(t)
	_, err := sqlite.DBForTest(fx.f).Exec(`INSERT INTO scheduled_tasks
		(id, tenant_id, type, scheduled_time, entity_id, model_name, model_version,
		 transition, source_state, armed_at, arm_token, status, next_attempt_time)
		VALUES ('e1:S:T', 'tenant-A', 'FIRE_TRANSITION', 1000, 'e1', 'M', 1, 'T', 'S', 0, ?, 'BOGUS', 1000)`,
		uuid.NewString())
	if err == nil {
		t.Fatal("the CHECK on status accepted BOGUS")
	}
	if got := sqlite.ClassifyRejectionForTest(err); !errors.Is(got, spi.ErrStoreRejected) {
		t.Fatalf("classified %v, want ErrStoreRejected", got)
	}

	plain := errors.New("connection reset")
	if got := sqlite.ClassifyRejectionForTest(plain); got != plain {
		t.Fatalf("a non-deterministic error was changed: %v", got)
	}
	if got := sqlite.ClassifyRejectionForTest(nil); got != nil {
		t.Fatalf("nil classified as %v", got)
	}
}
```

Add to `plugins/sqlite/export_test.go`:

```go
// ClassifyRejectionForTest exposes classifyRejection for unit tests.
var ClassifyRejectionForTest = classifyRejection
```

- [ ] **Step 2: Run the tests and see them fail**

```
cd /Users/paul/go-projects/cyoda-light/cyoda-go/.worktrees/598-scheduler-ownership/plugins/sqlite && go test ./... -run 'TestTasks_TheStoreRejects|TestTasks_SQLiteConstraint'
```

Expected: `FAIL … [build failed]` with `undefined: classifyRejection` from
`export_test.go`. That is this step's RED; the build failure covers both
tests, because they share the package.

- [ ] **Step 3: Write the implementation**

`plugins/sqlite/scheduled_task_validate.go`: the file BM-5 writes as
`plugins/memory/scheduled_task_validate.go`, with `package sqlite` and no
other change.

Add to `plugins/sqlite/errors.go`:

```go
// classifyRejection marks a deterministic SQLite rejection of a write — a
// constraint, a value too big, a type mismatch, a bind out of range — with
// spi.ErrStoreRejected: retrying it cannot succeed, and the scheduler latches
// the node on it. Every other error (BUSY, I/O, a closed database, a
// cancelled context) passes through unchanged and is retried. The original
// error stays in the chain. Used on every scheduled-task write and on every
// audit-event insert.
func classifyRejection(err error) error {
	if err == nil {
		return nil
	}
	for _, code := range []sqlite3.ErrorCode{sqlite3.CONSTRAINT, sqlite3.TOOBIG, sqlite3.MISMATCH, sqlite3.RANGE} {
		if errors.Is(err, code) {
			return fmt.Errorf("%w: %w", spi.ErrStoreRejected, err)
		}
	}
	return err
}
```

In `plugins/sqlite/scheduled_task_store.go`, `applyTaskOp` returns
`classifyRejection(err)` in both writing branches:

```go
	case op.after == nil:
		_, err := exec.ExecContext(ctx, `DELETE FROM scheduled_tasks WHERE tenant_id = ? AND id = ?`,
			string(op.key.tenant), op.key.id)
		return classifyRejection(err)
	default:
		_, err := exec.ExecContext(ctx, upsertTaskSQL, taskArgs(*op.after)...)
		return classifyRejection(err)
```

In `MarkUnsafe`, wrap the INSERT's error:
`return fmt.Errorf("failed to mark scheduled task %s: %w", ref.ID, classifyRejection(err))`.

Add the input checks, each as the first statement, exactly as BM-5 Step 3
does: `rejectArm(req)` in `ReconcileForEntity`; `rejectFailureReason(f.Reason)`
then `rejectErrorText(f.Error)` in `Fail`; `rejectErrorText(a.Error)` in
`RecordAttempt`. The input checks refuse at the call on every backend; the
schema's CHECKs stay as the table's own guarantee, and `classifyRejection`
marks any violation of them that reaches SQLite.

- [ ] **Step 4: Run the tests and see them pass**

```
cd /Users/paul/go-projects/cyoda-light/cyoda-go/.worktrees/598-scheduler-ownership/plugins/sqlite && go test ./... -skip 'TestConformance'
```

Expected: `ok`.

- [ ] **Step 5: Commit**

```
cd /Users/paul/go-projects/cyoda-light/cyoda-go/.worktrees/598-scheduler-ownership && git add plugins/sqlite/scheduled_task_validate.go plugins/sqlite/errors.go plugins/sqlite/scheduled_task_store.go plugins/sqlite/scheduled_task_claims.go plugins/sqlite/export_test.go plugins/sqlite/scheduled_task_rejected_test.go && git commit -m "feat(sqlite): deterministic task-store rejections carry ErrStoreRejected

Input no backend stores is refused at the call, with the same rules as
memory. A constraint, too-big, mismatch or range error from SQLite on
a task write is marked as a store rejection; other errors pass through
to be retried.

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task BQ-7: Audit events join the transaction

**Spec:** as BM-6.

**The divergence this fixes.** SQLite's `smAuditStore.Record` inserts on the
writer pool at once, whatever is on `ctx` (`plugins/sqlite/sm_audit_store.go:22-42`);
the store has no link to the transaction manager (`store_factory.go:398`).
PostgreSQL records on the transaction's connection
(`plugins/postgres/store_factory.go:255`, `:162-164`) and rolls the event back
with it. So on SQLite a superseded or rolled-back run's audit rows stay
visible, and PostgreSQL's do not. `flushToSQLite`'s comment
(`txmanager.go:851-877`) describes today's shape: events are already in the
table when the flush stamps them.

The fix mirrors BM-6: `Record` with a transaction on `ctx` stages the event
(id assigned, JSON already marshalled) on it; `flushToSQLite` inserts the
staged events in its `sqlTx`, just before the commit-instant `UPDATE`; every
abort path and `RollbackToSavepoint` drop them. A read inside the transaction
sees them. `Record` without a transaction is unchanged, except that its
deterministic failures carry `ErrStoreRejected`.

**Files:**
- Modify: `plugins/sqlite/sm_audit_store.go` (`smAuditStore` `:14-18`,
  `Record` `:22-42`, `GetEvents` `:44-63`, `GetEventsByTransaction` `:65-81`)
- Modify: `plugins/sqlite/store_factory.go` (`:398`)
- Modify: `plugins/sqlite/txmanager.go` (`savepointSnapshot`; fields and
  constructor; `forgetLocked` from BQ-3; step 3 capture from BQ-4;
  `flushToSQLite` signature `:635` and the stamp block `:851-883`;
  `Savepoint` `:1076`; `RollbackToSavepoint` `:1137-1138`)
- Modify: `plugins/sqlite/sm_audit_no_generator_internal_test.go` (`:43-47`)
- Create: `plugins/sqlite/sm_audit_tx_join_test.go`

**Interfaces:**
- Consumes: BQ-3 `forgetLocked`, BQ-4's step 3, BQ-6 `classifyRejection`.
- Produces: `stagedAuditEvent{entityID, event, doc}`,
  `(*transactionManager).stageAuditEvent`, `stagedAuditEvents`,
  `insertAuditEventSQL`.

- [ ] **Step 1: Write the failing tests**

`plugins/sqlite/sm_audit_tx_join_test.go` holds the BM-6 tests
(`plugins/memory/sm_audit_tx_join_test.go` as BM-6 writes it) with
`package sqlite_test`, and every `ctxWithTenant(` replaced by `tenantCtx(`.

In `plugins/sqlite/sm_audit_no_generator_internal_test.go`, change `}); err == nil {`
(`:45`) to `}); !errors.Is(err, spi.ErrStoreRejected) {`, the message to
`t.Fatalf("Record with no generator configured: err = %v, want ErrStoreRejected", err)`,
and add `"errors"` to its imports.

- [ ] **Step 2: Run the tests and see them fail**

```
cd /Users/paul/go-projects/cyoda-light/cyoda-go/.worktrees/598-scheduler-ownership/plugins/sqlite && go test ./... -run 'TestAudit_|TestSMAudit'
```

Expected: `ARecordInARolledBackTransactionIsDiscarded` fails with `outside the
transaction before commit: 1 events, want 0`;
`ARecordAfterARolledBackSavepointIsDropped` with two events;
`AnEventThatCannotBeWrittenIsAStoreRejection` with an error that is not
`ErrStoreRejected` (the marshal error today is plain);
`TestSMAuditStore_Record_NoGenerator` likewise.

- [ ] **Step 3: Write the implementation**

In `plugins/sqlite/txmanager.go`:

```go
// stagedAuditEvent is one audit event recorded inside an open transaction:
// its id assigned and its JSON document built, so nothing about it can fail
// at flush but the insert itself.
type stagedAuditEvent struct {
	entityID string
	event    spi.StateMachineEvent
	doc      []byte
}
```

Add the field (after `scheduledTaskOps`):

```go
	// auditOps holds the audit events recorded while the transaction is open,
	// in order. flushToSQLite inserts them in the commit's sqlTx, before the
	// commit-instant stamp; Rollback and every abort path drop them;
	// RollbackToSavepoint truncates them. Protected by mu. PostgreSQL gets the
	// same behaviour from recording on the transaction's connection.
	auditOps map[string][]stagedAuditEvent // txID → staged events
```

with `auditOps: make(map[string][]stagedAuditEvent),` in
`newTransactionManager`; `delete(m.auditOps, txID)` in `forgetLocked`;
`auditOpsLen int` in `savepointSnapshot`, set in `Savepoint` with
`auditOpsLen: len(m.auditOps[txID]),`, and truncated in `RollbackToSavepoint`
next to the `scheduledTaskOps` truncation:

```go
	if n := snap.auditOpsLen; n < len(m.auditOps[txID]) {
		m.auditOps[txID] = m.auditOps[txID][:n]
	}
```

and:

```go
// stageAuditEvent appends ev to txID's staged audit events. Protected by mu.
func (m *transactionManager) stageAuditEvent(txID string, ev stagedAuditEvent) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.auditOps[txID] = append(m.auditOps[txID], ev)
}

// stagedAuditEvents returns a copy of txID's staged audit events. Protected by mu.
func (m *transactionManager) stagedAuditEvents(txID string) []stagedAuditEvent {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]stagedAuditEvent(nil), m.auditOps[txID]...)
}
```

In Commit's step 3 closure (BQ-4), capture them beside `scheduledOps`:
`auditEvents = append([]stagedAuditEvent(nil), m.auditOps[txID]...)`
(declared `var auditEvents []stagedAuditEvent` beside `scheduledOps`), and
pass them to the flush: `m.flushToSQLite(ctx, tx, submitTime, scheduledOps, auditEvents)`.
`flushToSQLite` gains the parameter `auditEvents []stagedAuditEvent`. Replace
its stamp comment and statement (`:851-883`) with:

```go
	// Audit events recorded inside this transaction are inserted here, in
	// sqlTx, so they commit or roll back with it. Then every event LABELLED
	// with this transaction — those, and any recorded outside a transaction
	// under its id (EmitTransitionAborted labels by a cascade entry's id) —
	// takes the commit instant, so the audit trail and the version history
	// cannot drift apart or invert. Served by idx_sm_events_tenant_tx
	// (migration 000008).
	for _, st := range auditEvents {
		if _, err := sqlTx.ExecContext(ctx, insertAuditEventSQL,
			tid, st.entityID, st.event.TimeUUID, st.event.TransactionID,
			st.event.Timestamp.UnixMicro(), st.doc); err != nil {
			return fmt.Errorf("record staged audit event %s: %w", st.event.TimeUUID, classifyRejection(err))
		}
	}
	_, err = sqlTx.ExecContext(ctx,
		"UPDATE sm_audit_events SET timestamp = ? WHERE tenant_id = ? AND transaction_id = ?",
		submitMicro, tid, tx.ID)
	if err != nil {
		return fmt.Errorf("stamp audit events: %w", err)
	}
```

In `plugins/sqlite/store_factory.go:398`, construct the store with the manager:
`return &smAuditStore{db: f.db, tenantID: tid, uuids: f.uuids, tm: f.tm}, nil`.

In `plugins/sqlite/sm_audit_store.go`, add `"sort"` to the imports, the field
`tm *transactionManager` to `smAuditStore`, and replace `Record` with:

```go
// insertAuditEventSQL writes one audit event. Record uses it outside a
// transaction; flushToSQLite uses it for the events a transaction staged.
const insertAuditEventSQL = `INSERT INTO sm_audit_events (tenant_id, entity_id, event_id, transaction_id, timestamp, doc)
	 VALUES (?, ?, ?, ?, ?, jsonb(?))`

// Record assigns the event its id (see spi.StateMachineAuditStore): a
// caller's TimeUUID is ignored. With a transaction on ctx the event is staged
// on it and written only if the transaction commits.
func (s *smAuditStore) Record(ctx context.Context, entityID string, event spi.StateMachineEvent) error {
	if s.uuids == nil {
		return fmt.Errorf("failed to record state machine event for entity %s: no id generator configured: %w", entityID, spi.ErrStoreRejected)
	}
	event.TimeUUID = uuid.UUID(s.uuids.NewTimeUUID()).String()

	doc, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("failed to marshal state machine event: %w: %w", spi.ErrStoreRejected, err)
	}

	if tx := spi.GetTransaction(ctx); tx != nil {
		tx.OpMu.RLock()
		defer tx.OpMu.RUnlock()
		if tx.RolledBack {
			return fmt.Errorf("Record: %w (txID=%s)", spi.ErrTxRolledBack, tx.ID)
		}
		if tx.Closed {
			return fmt.Errorf("Record: %w (txID=%s)", spi.ErrTxAlreadyCommitted, tx.ID)
		}
		if tx.TenantID != s.tenantID {
			return fmt.Errorf("Record: %w (txID=%s)", spi.ErrTxTenantMismatch, tx.ID)
		}
		s.tm.stageAuditEvent(tx.ID, stagedAuditEvent{entityID: entityID, event: event, doc: doc})
		return nil
	}

	if _, err := s.db.ExecContext(ctx, insertAuditEventSQL,
		string(s.tenantID), entityID, event.TimeUUID, event.TransactionID,
		event.Timestamp.UnixMicro(), doc); err != nil {
		return fmt.Errorf("failed to record state machine event %s for entity %s: %w", event.TimeUUID, entityID, classifyRejection(err))
	}
	return nil
}

// stagedFor returns the events the transaction on ctx has recorded for
// entityID and not yet committed, so a read inside the transaction sees them.
func (s *smAuditStore) stagedFor(ctx context.Context, entityID string, keep func(spi.StateMachineEvent) bool) []spi.StateMachineEvent {
	tx := spi.GetTransaction(ctx)
	if tx == nil || tx.TenantID != s.tenantID || s.tm == nil {
		return nil
	}
	var out []spi.StateMachineEvent
	for _, st := range s.tm.stagedAuditEvents(tx.ID) {
		if st.entityID == entityID && keep(st.event) {
			out = append(out, st.event)
		}
	}
	return out
}

// withStaged merges staged events into committed ones, in timestamp order,
// as the SQL ORDER BY timestamp would place them.
func withStaged(committed, staged []spi.StateMachineEvent) []spi.StateMachineEvent {
	if len(staged) == 0 {
		return committed
	}
	out := append(committed, staged...)
	sort.SliceStable(out, func(i, j int) bool { return out[i].Timestamp.Before(out[j].Timestamp) })
	return out
}
```

`s.tm == nil` is the factory built without a transaction manager
(`newStoreFactory` alone, as `TestSMAuditStore_Record_NoGenerator` builds it);
such a factory has no transaction to stage on. In `GetEvents`, return
`withStaged(events, s.stagedFor(ctx, entityID, func(spi.StateMachineEvent) bool { return true }))`
(keeping the non-nil empty slice). In `GetEventsByTransaction`, return
`withStaged(events, s.stagedFor(ctx, entityID, func(e spi.StateMachineEvent) bool { return e.TransactionID == transactionID }))`.

- [ ] **Step 4: Run the tests and see them pass**

```
cd /Users/paul/go-projects/cyoda-light/cyoda-go/.worktrees/598-scheduler-ownership/plugins/sqlite && go test ./... -skip 'TestConformance'
```

Expected: `ok`, including `TestSMAudit_*` and `TestAudit_*`.

- [ ] **Step 5: Commit**

```
cd /Users/paul/go-projects/cyoda-light/cyoda-go/.worktrees/598-scheduler-ownership && git add plugins/sqlite/sm_audit_store.go plugins/sqlite/store_factory.go plugins/sqlite/txmanager.go plugins/sqlite/sm_audit_no_generator_internal_test.go plugins/sqlite/sm_audit_tx_join_test.go && git commit -m "fix(sqlite): audit events recorded in a transaction roll back with it

PostgreSQL records an audit event on the transaction's connection, so a
rolled-back transaction leaves no event; SQLite inserted at once and
kept it. Record now stages the event on the transaction on ctx and the
commit's flush inserts it before the commit-instant stamp. A marshal
failure, a missing id generator and a deterministic SQLite error on the
insert are store rejections.

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task BQ-8: Close-out — the whole ScheduledTasks suite, exit checks

**Spec:** §10.1 "Conformance"; §15 exit checks (the SQLite part).

**Files:**
- Verify only; modify whatever the runs below show.

**Interfaces:**
- Consumes: stream S's `runScheduledTasks`, through the existing
  `TestConformance` (`plugins/sqlite/conformance_test.go:12-26`).
- Produces: nothing new.

- [ ] **Step 1: Run the whole plugin, conformance included**

```
cd /Users/paul/go-projects/cyoda-light/cyoda-go/.worktrees/598-scheduler-ownership/plugins/sqlite && go test ./... && go vet ./...
```

Expected: `ok` and no vet output. If a `TestConformance/ScheduledTasks/…`
subtest fails, write the smallest plugin-local test that reproduces it, see
it fail, fix the store, and put the test in the file of the task whose area
it is; then rerun.

- [ ] **Step 2: Exit checks**

Each must print nothing:

```
cd /Users/paul/go-projects/cyoda-light/cyoda-go/.worktrees/598-scheduler-ownership && git grep -n -e RedispatchAfter -e AttemptCount -e ScanDue -e MarkRedispatch -e 'Upsert(' -e ErrUnsupported -e RunScheduledTaskStoreConformance -e redispatch_after -e attempt_count -- plugins/sqlite ':!plugins/sqlite/migrations/*' ':!plugins/sqlite/migration_000009_internal_test.go'
cd /Users/paul/go-projects/cyoda-light/cyoda-go/.worktrees/598-scheduler-ownership && git grep -n -e 'stageScheduledTaskOp' -e 'scheduledTaskOpsFor' -e 'scheduledTaskUpsert' -e 'scheduledTaskDelete' -e 'committed.submitTime' -- plugins/sqlite
```

The migration test is excluded because it writes a version-8 row on purpose.

- [ ] **Step 3: Commit (only if Step 1 or 2 changed a file)**

```
cd /Users/paul/go-projects/cyoda-light/cyoda-go/.worktrees/598-scheduler-ownership && git add <each changed file by name> && git commit -m "test(sqlite): <what the conformance run showed>

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

## Coverage carried forward (§13 rows with an S tick, on SQLite)

Every row of BM's table holds here too, through the same test names in
`plugins/sqlite`. Rows proper to SQLite:

| §13 row | Where it is proven |
|---|---|
| SQLite: a run never conflicts with its own claim under a frozen clock | `TestFCW_ACommitBeforeBeginDoesNotConflictUnderAFrozenClock` (BQ-3), `TestTasks_C1_ARunDoesNotConflictWithItsOwnClaimUnderAFrozenClock` (BQ-4); S suite |
| concurrent `ClaimDue` calls get disjoint sets | `TestTasks_ConcurrentClaimsAreDisjoint`; the unique partial index; S suite |
| a mark survives a process restart (§10.3 durability; the base of the "1" rows) | `TestTasks_MarksAndOwnersSurviveARestart` |
| `spi.ErrStoreRejected` (every backend sets the marker) | `TestTasks_TheStoreRejectsWhatNoBackendStores`, `TestTasks_SQLiteConstraintErrorsAreStoreRejections`; S suite |
| a pending task survives the schema change | `TestMigration9_KeepsPendingTasksAsNewLives`, `TestMigration9_Down` |
| a rolled-back run leaves no audit event (parity with PostgreSQL) | `TestAudit_*` (BQ-7) |

## Stream interface summary

Consumed from stream S: as BM.

Produced for other streams: nothing exported beyond the test exports
`CommittedLogLenForTest` and `ClassifyRejectionForTest` (`export_test.go`,
test builds only). `(*StoreFactory).ScheduledTaskStore`, `NewStoreFactoryForTest`,
`WithClock` and `NewTestClockAt` keep their signatures.

Behaviour other streams rely on, in addition to BM's list (every item of
which holds on SQLite too — the lead's rulings on `ClaimDue` limits, nil
`keep`, `NotCounted`, `Fail`'s `LastError`, tenant and entity from the
request, byte-wise id order, and audit events that roll back):
- The SQLite errors that count as a deterministic rejection are the result
  codes `CONSTRAINT` (every extended constraint code: CHECK, NOT NULL, UNIQUE,
  PRIMARY KEY, FOREIGN KEY), `TOOBIG`, `MISMATCH` and `RANGE`, on a
  scheduled-task write or an audit insert (`classifyRejection`). `BUSY`, I/O
  errors, a closed database and a cancelled context are not: they are
  retried. A JSON marshal failure of an audit event and a store without an id
  generator are rejections too. On the §5.7 path the audit insert runs in the
  commit's flush, so its rejection arrives from `Commit`, wrapped, and
  `errors.Is(err, spi.ErrStoreRejected)` holds there.
- Migration 9 keeps every pending task as a new WAITING life. Stream T's
  single-node SQLite restart rows ("1") build on `scheduled_task_marks` and
  `scheduler_owners` being durable.
- `commitTaskWrites`, `ClaimDue`, `GiveBackIdle`, `MarkUnsafe` and
  `RecordAttempt` take the commit gate; `Heartbeat` does not, so an entity
  commit delays a heartbeat only by its own flush on the single writer
  connection (C4 on SQLite).
- The entity conflict check no longer produces a false conflict under a
  frozen or coarse clock. A test anywhere that relied on that false conflict
  would now see `nil`; none in `plugins/sqlite` does (BQ-3 Step 4 runs them
  all).

## Open points

1. **All of BM's open points 1–10 apply here**, with `conformance_test.go:13-25`
   for point 2.
2. **Rejection set against the schema's CHECKs.** The Go checks (BQ-6) and the
   table's CHECKs cover the same rules for length, reason and status; NUL and
   invalid UTF-8 are Go-only, because SQLite does not validate either in
   `TEXT`. If `interfaces.md` widens the rejection set, both change.
3. **Legacy rows keep their pending time but lose their history.**
   Migration 9 keeps a pending task as a new life with `attempts` 0; the old
   `attempt_count` is dropped, as spec §10.1 removes `AttemptCount`. There are
   no production instances, so no row carries history that matters.
4. **`ClaimDue` scans every due task.** Memory does the same. A single-node
   backlog of very many due tasks makes each tick read them all; bounded by
   one pnode's data, it is not a correctness point, but the lead may want a
   bound once T's scenarios show the cost.
5. **BQ-6 Step 2's RED is a build failure** (`export_test.go` names
   `classifyRejection` before it exists). An executor who wants to see the
   BM-5 subtests fail on `nil` as well can add the classifier first and rerun
   before adding the input checks.
6. **The audit fix is BM's open point 9 on SQLite.** No SQLite test pinned the
   old behaviour. `flushToSQLite`'s comment about events being "visible here
   for a structural reason" (`txmanager.go:853-862`) is replaced in BQ-7,
   because in-transaction events are now inserted by the flush itself.
