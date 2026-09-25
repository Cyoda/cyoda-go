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

	// A claim token and a claim owner are set together or not at all, in any
	// status.
	_, err = pool.Exec(ctx, `INSERT INTO scheduled_tasks (id, tenant_id, type, scheduled_time, entity_id,
		model_name, model_version, transition, source_state, armed_at, arm_token, status, next_attempt_time,
		claim_token)
		VALUES ('d', 't', 'fire-transition', 1, 'k', 'M', 1, 'T', 'S', 0, gen_random_uuid(), 'WAITING', 1,
		        gen_random_uuid())`)
	if !errors.As(err, &pgErr) || pgErr.Code != pgerrcode.CheckViolation ||
		pgErr.ConstraintName != "scheduled_tasks_claim_pair_chk" {
		t.Errorf("a claim token without an owner: err = %v, want scheduled_tasks_claim_pair_chk", err)
	}

	// A task is keyed by (tenant, id), as on memory and SQLite: the same id in
	// another tenant is another task.
	if _, err := pool.Exec(ctx, `INSERT INTO scheduled_tasks (id, tenant_id, type, scheduled_time, entity_id,
		model_name, model_version, transition, source_state, armed_at, arm_token, status, next_attempt_time)
		VALUES ('a', 't2', 'fire-transition', 1, 'e', 'M', 1, 'T1', 'S', 0, gen_random_uuid(), 'WAITING', 1)`); err != nil {
		t.Errorf("the same id in another tenant: %v", err)
	}
	markCols := stringSet(t, pool, `SELECT column_name FROM information_schema.columns
		WHERE table_schema = 'public' AND table_name = 'scheduled_task_marks'`)
	if !markCols["tenant_id"] {
		t.Error("scheduled_task_marks lacks column tenant_id")
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
