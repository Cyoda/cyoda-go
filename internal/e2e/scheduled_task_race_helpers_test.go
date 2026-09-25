package e2e_test

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// schedWritesWorkflow arms AutoClose one hour after an entity enters OPEN.
// No test runs for an hour, so no scheduler on the suite's database finds
// one of these tasks due. Every change to their rows is one the test makes.
const schedWritesWorkflow = `{
	"importMode": "REPLACE",
	"workflows": [{
		"version": "1.1", "name": "sched-writes-wf", "initialState": "OPEN", "active": true,
		"states": {
			"OPEN": {"transitions": [
				{"name": "AutoClose", "next": "CLOSED", "manual": false, "schedule": {"delayMs": 3600000}},
				{"name": "Leave", "next": "LEFT", "manual": true}
			]},
			"CLOSED": {},
			"LEFT": {}
		}
	}]
}`

const schedWritesPayload = `{"name":"Order","amount":100,"status":"draft"}`

func setupScheduledModel(t *testing.T, model string) {
	t.Helper()
	setupModelWithWorkflow(t, model, schedWritesWorkflow)
}

// doAuthOnceRaw sends one authenticated request and never retries. The
// tests here must see the server's own answer: doAuthRaw retries a
// retryable 409 on the client side, which would hide both a 409 and a
// missing server-side retry.
func doAuthOnceRaw(ctx context.Context, method, path, body string) (*http.Response, error) {
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	req, err := authRequestRaw(ctx, method, path, r)
	if err != nil {
		return nil, err
	}
	return http.DefaultClient.Do(req)
}

// taskRows counts scheduled-task rows matching where, read from Postgres.
// where is a constant of the calling test; values go in args.
func taskRows(t *testing.T, where string, args ...any) int {
	t.Helper()
	return queryDB(t, "test-tenant", "SELECT count(*) FROM scheduled_tasks WHERE "+where, args...)
}

// taskRowHold is a transaction on the verification pool that holds the row
// locks of some scheduled-task rows.
type taskRowHold struct {
	tx    pgx.Tx
	pid   int32
	where string
	args  []any
}

// holdTaskRows locks the rows matching where (FOR UPDATE) and keeps them
// locked until claimAndCommit, or until the test ends.
func holdTaskRows(t *testing.T, where string, args ...any) *taskRowHold {
	t.Helper()
	ctx := context.Background()
	tx, err := dbPool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	t.Cleanup(func() { _ = tx.Rollback(context.Background()) })
	var pid int32
	if err := tx.QueryRow(ctx, "SELECT pg_backend_pid()").Scan(&pid); err != nil {
		t.Fatalf("pg_backend_pid: %v", err)
	}
	var n int
	if err := tx.QueryRow(ctx,
		"SELECT count(*) FROM (SELECT id FROM scheduled_tasks WHERE "+where+" FOR UPDATE) held", args...).Scan(&n); err != nil {
		t.Fatalf("lock task rows: %v", err)
	}
	if n == 0 {
		t.Fatalf("no task rows match %q: the test would prove nothing", where)
	}
	return &taskRowHold{tx: tx, pid: pid, where: where, args: args}
}

// awaitBlocked returns once another backend waits on this hold's locks.
func (h *taskRowHold) awaitBlocked(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		var n int
		if err := dbPool.QueryRow(context.Background(),
			"SELECT count(*) FROM pg_stat_activity WHERE $1 = ANY(pg_blocking_pids(pid))", h.pid).Scan(&n); err != nil {
			t.Fatalf("pg_stat_activity: %v", err)
		}
		if n > 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("no statement ever waited on the held task rows")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// claimAndCommit changes the held rows as a claim does and commits. The
// statement waiting on the lock began its snapshot before this commit, so
// PostgreSQL fails it with 40001.
func (h *taskRowHold) claimAndCommit(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	if _, err := h.tx.Exec(ctx,
		"UPDATE scheduled_tasks SET status = 'RUNNING', claim_token = gen_random_uuid(), claim_owner = gen_random_uuid() WHERE "+h.where,
		h.args...); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if err := h.tx.Commit(ctx); err != nil {
		t.Fatalf("commit claim: %v", err)
	}
}

var inlineSafe = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)

// conflictTrigger fails every delete of a matching scheduled-task row with
// SQLSTATE 40001, the refusal first-committer-wins gives. It counts the
// refusals in a sequence, which a rollback does not undo.
type conflictTrigger struct{ name string }

// installConflictTrigger scopes the trigger to rows whose column equals one
// of values. column is entity_id or model_name.
func installConflictTrigger(t *testing.T, column string, values ...string) *conflictTrigger {
	t.Helper()
	if column != "entity_id" && column != "model_name" {
		t.Fatalf("column %q is not entity_id or model_name", column)
	}
	quoted := make([]string, 0, len(values))
	for _, v := range values {
		if !inlineSafe.MatchString(v) {
			t.Fatalf("value %q is not safe to inline in DDL", v)
		}
		quoted = append(quoted, "'"+v+"'")
	}
	c := &conflictTrigger{name: fmt.Sprintf("w_conflict_%d", time.Now().UnixNano())}
	ddl := fmt.Sprintf(`
CREATE SEQUENCE %[1]s;
CREATE FUNCTION %[1]s() RETURNS trigger LANGUAGE plpgsql AS $fn$
BEGIN
  IF OLD.%[2]s IN (%[3]s) THEN
    PERFORM nextval('%[1]s');
    RAISE EXCEPTION 'test: task row changed after this transaction began' USING ERRCODE = '40001';
  END IF;
  RETURN OLD;
END
$fn$;
CREATE TRIGGER %[1]s BEFORE DELETE ON scheduled_tasks FOR EACH ROW EXECUTE FUNCTION %[1]s();`,
		c.name, column, strings.Join(quoted, ", "))
	if _, err := dbPool.Exec(context.Background(), ddl); err != nil {
		t.Fatalf("install conflict trigger: %v", err)
	}
	t.Cleanup(func() { c.remove(t) })
	return c
}

// refusals reports how many deletes the trigger refused.
func (c *conflictTrigger) refusals(t *testing.T) int64 {
	t.Helper()
	var n int64
	if err := dbPool.QueryRow(context.Background(),
		fmt.Sprintf("SELECT CASE WHEN is_called THEN last_value ELSE 0 END FROM %s", c.name)).Scan(&n); err != nil {
		t.Fatalf("read refusals: %v", err)
	}
	return n
}

// remove drops the trigger. Safe to call twice.
func (c *conflictTrigger) remove(t *testing.T) {
	t.Helper()
	if _, err := dbPool.Exec(context.Background(), fmt.Sprintf(
		"DROP TRIGGER IF EXISTS %[1]s ON scheduled_tasks; DROP FUNCTION IF EXISTS %[1]s(); DROP SEQUENCE IF EXISTS %[1]s;", c.name)); err != nil {
		t.Errorf("remove conflict trigger: %v", err)
	}
}
