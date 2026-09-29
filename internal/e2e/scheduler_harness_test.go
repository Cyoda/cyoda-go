package e2e_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cyoda-platform/cyoda-go/app"
	"github.com/cyoda-platform/cyoda-go/e2e/parity/fixtureutil"
)

// scheduler_harness_test.go gives a scheduler test a stack of its own on a
// PostgreSQL database of its own. The scheduler claims across tenants
// (ClaimDue is cross-tenant), so a scheduler on the shared test database
// would claim tasks other tests' stacks armed and run them through the wrong
// engine. A database per test makes "the only scheduler that can see this
// task" true by construction.

// harnessTenant is the tenant every harness stack's default admin token uses.
const harnessTenant = "test-tenant"

// schedDB is one test's database: its URL for the stack, a pool for reads.
type schedDB struct {
	name string
	url  string
	pool *pgxpool.Pool
	// keyClient is the M2M client the keyStacks on this database share (see
	// newKeyStackWith); nil until the first keyStack creates it. It is
	// written without synchronisation, which is safe because the keyStacks
	// on one database are built sequentially, on the test goroutine.
	keyClient *m2mCredential
}

// m2mCredential is an M2M client id and its secret (never logged).
type m2mCredential struct{ id, secret string }

// newSchedDB creates an empty database in the shared container. The stack
// that opens it migrates it (CYODA_POSTGRES_AUTO_MIGRATE is set by TestMain).
func newSchedDB(t *testing.T) *schedDB {
	t.Helper()
	ctx := context.Background()
	name := "sched_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:20]
	if _, err := dbPool.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatalf("create database %s: %v", name, err)
	}
	u, err := url.Parse(pgURLFromEnv(t))
	if err != nil {
		t.Fatalf("parse postgres URL: %v", err)
	}
	u.Path = "/" + name
	pool, err := pgxpool.New(ctx, u.String())
	if err != nil {
		t.Fatalf("pool for %s: %v", name, err)
	}
	s := &schedDB{name: name, url: u.String(), pool: pool}
	// Registered first, so it runs after the stack's own cleanups.
	t.Cleanup(func() {
		pool.Close()
		if _, err := dbPool.Exec(context.Background(), "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)"); err != nil {
			t.Logf("drop database %s: %v", name, err)
		}
	})
	return s
}

// schedulerTuning is the scheduler timing every scheduler stack runs with:
// the parity values (fixtureutil.Tuned*), a 1s shutdown drain, one try per
// callout, and the single-node patience for a compute node.
func schedulerTuning(cfg *app.Config) {
	cfg.Scheduler.Enabled = true
	cfg.Scheduler.ScanInterval = fixtureutil.TunedScanInterval
	cfg.Scheduler.HeartbeatInterval = fixtureutil.TunedHeartbeatInterval
	cfg.Scheduler.StaleAfter = fixtureutil.TunedStaleAfter
	cfg.Scheduler.RetryDelay = fixtureutil.TunedRetryDelay
	cfg.Scheduler.RetryDelayMax = fixtureutil.TunedRetryDelayMax
	cfg.Scheduler.ShutdownDrain = time.Second
	cfg.Callout.FixedNumRetries = 0
	cfg.Cluster.DispatchWaitTimeout = fixtureutil.TunedDispatchWaitTimeout
}

// newSchedulerHarness is a stack with a live scheduler on a database of its
// own and no cnode. configure runs after schedulerTuning and may override it.
func newSchedulerHarness(t *testing.T, configure func(*app.Config)) (*callbackHarness, *schedDB) {
	t.Helper()
	s := newSchedDB(t)
	h := newCalloutHarness(t, func(cfg *app.Config) {
		t.Setenv("CYODA_POSTGRES_URL", s.url)
		schedulerTuning(cfg)
		if configure != nil {
			configure(cfg)
		}
	})
	return h, s
}

// newSchedulerCallbackHarness is newSchedulerHarness plus the default cnode of
// newCallbackHarness (tag "sched-fn", serving registered closures).
func newSchedulerCallbackHarness(t *testing.T, configure func(*app.Config)) (*callbackHarness, *schedDB) {
	t.Helper()
	h, s := newSchedulerHarness(t, configure)
	h.member = h.AttachCnode(t, cnodeSpec{name: "default", tags: []string{scheduledFnTag}, script: h.registeredScript}).m
	return h, s
}

// newStackOn opens a further stack on an existing test database — the
// restart of a pnode on the same storage.
func newStackOn(t *testing.T, s *schedDB, configure func(*app.Config)) *callbackHarness {
	t.Helper()
	return newCalloutHarness(t, func(cfg *app.Config) {
		t.Setenv("CYODA_POSTGRES_URL", s.url)
		schedulerTuning(cfg)
		if configure != nil {
			configure(cfg)
		}
	})
}

// taskRow is a scheduled_tasks row as a test sees it, with whether a mark
// exists for its current life.
type taskRow struct {
	ID, Status, LastError, FailureReason string
	ArmToken, ClaimToken, ClaimOwner     string
	Attempts, LostOwners                 int
	PartialCommit, Marked                bool
}

// task reads the entity's task for transition. ok is false when there is none.
func (s *schedDB) task(t *testing.T, entityID, transition string) (taskRow, bool) {
	t.Helper()
	var r taskRow
	err := s.pool.QueryRow(context.Background(), `
		SELECT st.id, st.status, COALESCE(st.last_error, ''), COALESCE(st.failure_reason, ''),
		       st.arm_token::text, COALESCE(st.claim_token::text, ''), COALESCE(st.claim_owner::text, ''),
		       st.attempts, st.lost_owners, st.partial_commit,
		       EXISTS (SELECT 1 FROM scheduled_task_marks m
		                WHERE m.tenant_id = st.tenant_id AND m.task_id = st.id AND m.arm_token = st.arm_token)
		  FROM scheduled_tasks st
		 WHERE st.tenant_id = $1 AND st.entity_id = $2 AND st.transition = $3`,
		harnessTenant, entityID, transition,
	).Scan(&r.ID, &r.Status, &r.LastError, &r.FailureReason, &r.ArmToken, &r.ClaimToken, &r.ClaimOwner,
		&r.Attempts, &r.LostOwners, &r.PartialCommit, &r.Marked)
	if errors.Is(err, pgx.ErrNoRows) {
		return taskRow{}, false
	}
	if err != nil {
		t.Fatalf("read task of %s/%s: %v", entityID, transition, err)
	}
	return r, true
}

// awaitTask polls the task every 20ms until cond holds and returns the first
// row that satisfies it.
func (s *schedDB) awaitTask(t *testing.T, entityID, transition string, within time.Duration, what string, cond func(taskRow, bool) bool) taskRow {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		r, ok := s.task(t, entityID, transition)
		if cond(r, ok) {
			return r
		}
		if time.Now().After(deadline) {
			t.Fatalf("task %s/%s: %s not seen within %s; last: present=%t %+v", entityID, transition, what, within, ok, r)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// count runs a count(*) query on this test's database.
func (s *schedDB) count(t *testing.T, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := s.pool.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		t.Fatalf("query failed: %v", err)
	}
	return n
}

// awaitDBCondition polls cond every 20ms until it is true.
func awaitDBCondition(t *testing.T, within time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(within)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("%s not seen within %s", what, within)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// schedDoc wraps states in a schema 1.5 import document (the minor that
// accepts a processor's idempotent) whose initial state is Open.
func schedDoc(wfName string, states map[string]any) string {
	b, _ := json.Marshal(map[string]any{
		"importMode": "REPLACE",
		"workflows": []any{map[string]any{
			"version": "1.5", "name": wfName, "initialState": "Open", "active": true, "states": states,
		}},
	})
	return string(b)
}

// sProc is one processor of a scheduler test workflow. idempotent false is
// the unsafe processor of spec §3.
func sProc(name, mode, tag string, idempotent bool) map[string]any {
	return map[string]any{"type": "calculator", "name": name, "executionMode": mode,
		"config": map[string]any{"attachEntity": true, "calculationNodesTags": tag,
			"idempotent": idempotent, "responseTimeoutMs": 60000}}
}

// fireOpenToDone is Open -[Fire, scheduled]-> Done carrying procs. timeoutMs
// 0 leaves timeoutMs off.
func fireOpenToDone(wfName string, delayMs, timeoutMs int64, procs ...map[string]any) string {
	sched := map[string]any{"delayMs": delayMs}
	if timeoutMs > 0 {
		sched["timeoutMs"] = timeoutMs
	}
	fire := map[string]any{"name": "Fire", "next": "Done", "manual": false, "schedule": sched}
	if len(procs) > 0 {
		list := make([]any, 0, len(procs))
		for _, p := range procs {
			list = append(list, p)
		}
		fire["processors"] = list
	}
	return schedDoc(wfName, map[string]any{
		"Open": map[string]any{"transitions": []any{fire}},
		"Done": map[string]any{},
	})
}

// scriptHoldFirst holds this cnode's first callout until release is closed
// and answers every later one at once.
func scriptHoldFirst(release <-chan struct{}) cnodeScript {
	var calls atomic.Int32
	return func(ctx context.Context, _ receivedCallout, _ *reqCtx) cnodeReply {
		if calls.Add(1) == 1 {
			select {
			case <-release:
			case <-ctx.Done():
				return neverAnswer()
			}
		}
		return answerOK()
	}
}

// schedEvents returns up to 500 StateMachine events of the entity on h's
// stack, newest first — enough for a write-heavy history (the endpoint's
// default page is 20).
func schedEvents(t *testing.T, h *callbackHarness, entityID string) []map[string]any {
	t.Helper()
	resp := h.DoAuth(t, http.MethodGet, "/api/audit/entity/"+entityID+"?eventType=StateMachine&limit=500", "", "")
	body := h.readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("audit GET %s: %d %s", entityID, resp.StatusCode, body)
	}
	var r struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal([]byte(body), &r); err != nil {
		t.Fatalf("decode audit page: %v (body %s)", err, body)
	}
	return r.Items
}

// failEvent returns the data of the entity's SCHEDULED_TRANSITION_FAIL event.
func failEvent(t *testing.T, h *callbackHarness, entityID string) map[string]any {
	t.Helper()
	for _, ev := range smEventsOfType(h.GetSMAuditEvents(t, entityID), "SCHEDULED_TRANSITION_FAIL") {
		data, _ := ev["data"].(map[string]any)
		return data
	}
	t.Fatalf("entity %s has no SCHEDULED_TRANSITION_FAIL event", entityID)
	return nil
}

// uniq returns prefix plus a short random suffix: model names and tags.
func uniq(prefix string) string { return prefix + "-" + uuid.NewString()[:8] }

// TestSchedulerHarness_OwnDatabase: a task armed on a scheduler stack lives
// in that stack's database only.
func TestSchedulerHarness_OwnDatabase(t *testing.T) {
	h, s := newSchedulerHarness(t, func(cfg *app.Config) { cfg.Scheduler.Enabled = false })
	model := uniq("sh-own")
	h.SetupModelWithWorkflow(t, model, fireOpenToDone("sh-own-wf", 600_000, 0))
	id, status, body := h.CreateEntity(t, model, 1, workflowSampleModel)
	if status != http.StatusOK {
		t.Fatalf("create: %d %s", status, body)
	}
	if r, ok := s.task(t, id, "Fire"); !ok || r.Status != "WAITING" || r.Attempts != 0 {
		t.Fatalf("task in the harness database = %+v present=%t; want WAITING, attempts 0", r, ok)
	}
	if n := queryDB(t, harnessTenant, "SELECT count(*) FROM scheduled_tasks WHERE entity_id = $1", id); n != 0 {
		t.Fatalf("the shared database holds %d rows of this entity; want 0", n)
	}
}
