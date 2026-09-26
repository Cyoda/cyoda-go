package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cyoda-platform/cyoda-go/e2e/parity"
	"github.com/cyoda-platform/cyoda-go/e2e/parity/client"
	"github.com/cyoda-platform/cyoda-go/e2e/parity/fixtureutil"
)

// scheduler_mn_helpers_test.go — shared by the scheduler multi-node tests.
// Each test boots a cluster of its own (they kill, stop, signal and pause
// pnodes, so none belongs in the shared multinode registry) and reads the
// task rows straight from PostgreSQL: which pnode holds a claim is invisible
// at the API (spec §8 never returns node ids or tokens).

// schedMN is one scenario's cluster and a reader on its database.
type schedMN struct {
	pg     *pgMultiNode
	db     *pgxpool.Pool
	bootAt time.Time
}

// newSchedMN boots n pnodes with opts and extraEnv on top of the tuned
// cluster env, and opens a reader on the shared database.
func newSchedMN(t *testing.T, n int, opts fixtureutil.LaunchOpts, extraEnv ...string) *schedMN {
	t.Helper()
	fix, cleanup := MustSetupMultiNodeWithOpts(t, n, extraEnv, opts)
	t.Cleanup(cleanup)
	pg, ok := fix.(*pgMultiNode)
	if !ok {
		t.Fatalf("fixture is %T; want *pgMultiNode", fix)
	}
	db, err := pgxpool.New(context.Background(), pg.ConnString()) // never logged: it carries credentials
	if err != nil {
		t.Fatalf("reader pool: %v", err)
	}
	t.Cleanup(db.Close)
	return &schedMN{pg: pg, db: db, bootAt: time.Now()}
}

// hostOnly turns the scheduler off on pnode host.
func hostOnly(host int) func(int) []string {
	return func(i int) []string {
		if i == host {
			return []string{"CYODA_SCHEDULER_ENABLED=false"}
		}
		return nil
	}
}

// mnShort is a short random suffix for tags and model names.
func mnShort() string { return uuid.NewString()[:6] }

// mnTask is a scheduled_tasks row with whether a mark exists for its life.
type mnTask struct {
	Status, LastError, FailureReason, ClaimToken, ClaimOwner string
	Attempts, LostOwners                                     int
	PartialCommit, Marked                                    bool
}

// task reads the entity's task for transition.
func (s *schedMN) task(t *testing.T, entityID uuid.UUID, transition string) (mnTask, bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var r mnTask
	err := s.db.QueryRow(ctx, `
		SELECT st.status, COALESCE(st.last_error, ''), COALESCE(st.failure_reason, ''),
		       COALESCE(st.claim_token::text, ''), COALESCE(st.claim_owner::text, ''),
		       st.attempts, st.lost_owners, st.partial_commit,
		       EXISTS (SELECT 1 FROM scheduled_task_marks m WHERE m.task_id = st.id AND m.arm_token = st.arm_token)
		  FROM scheduled_tasks st WHERE st.entity_id = $1 AND st.transition = $2`,
		entityID.String(), transition,
	).Scan(&r.Status, &r.LastError, &r.FailureReason, &r.ClaimToken, &r.ClaimOwner,
		&r.Attempts, &r.LostOwners, &r.PartialCommit, &r.Marked)
	if errors.Is(err, pgx.ErrNoRows) {
		return mnTask{}, false
	}
	if err != nil {
		t.Fatalf("read task %s/%s: %v", entityID, transition, err)
	}
	return r, true
}

// awaitTask polls every 100ms until cond holds.
func (s *schedMN) awaitTask(t *testing.T, id uuid.UUID, transition string, within time.Duration, what string, cond func(mnTask, bool) bool) mnTask {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		r, ok := s.task(t, id, transition)
		if cond(r, ok) {
			return r
		}
		if time.Now().After(deadline) {
			t.Fatalf("task %s/%s: %s not seen within %s; last: present=%t %+v", id, transition, what, within, ok, r)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// ownerNode maps a claim_owner to the pnode whose scheduler announced that
// incarnation, or -1. A pnode with its scheduler off announces none.
func (s *schedMN) ownerNode(t *testing.T, owner string) int {
	t.Helper()
	for i := 0; i < s.pg.NodeCount(); i++ {
		id, err := fixtureutil.IncarnationFromLog(s.pg.NodeLogs(i))
		if err == nil && id.String() == owner {
			return i
		}
	}
	return -1
}

// client is a parity client for tenant on pnode i.
func (s *schedMN) client(i int, tenant parity.Tenant) *client.Client {
	return client.NewClient(s.pg.BaseURLs()[i], tenant.Token)
}

// startClient starts a compute client for tenant on pnode node.
func (s *schedMN) startClient(t *testing.T, node int, tenant parity.Tenant, tag, behaviour string) parity.ComputeClient {
	t.Helper()
	cc := s.pg.StartComputeClient(t, node, parity.ComputeClientSpec{TenantID: tenant.ID, Tags: []string{tag}, Behaviour: behaviour})
	t.Cleanup(cc.Stop)
	return cc
}

// mnWorkflow wraps states in a schema 1.5 import document, initial state Open.
func mnWorkflow(wfName string, states map[string]any) string {
	b, _ := json.Marshal(map[string]any{"importMode": "REPLACE", "workflows": []any{map[string]any{
		"version": "1.5", "name": wfName, "initialState": "Open", "active": true, "states": states}}})
	return string(b)
}

// mnProc is one catalog processor routed to tag with one try and the given
// answer limit.
func mnProc(name, mode, tag string, idempotent bool, answerLimitMs int) map[string]any {
	return map[string]any{"type": "calculator", "name": name, "executionMode": mode,
		"config": map[string]any{"attachEntity": true, "calculationNodesTags": tag, "idempotent": idempotent,
			"retryPolicy": "NONE", "responseTimeoutMs": answerLimitMs}}
}

// mnFire is Open -[Fire, scheduled]-> Done carrying procs.
func mnFire(delayMs, timeoutMs int64, procs ...map[string]any) map[string]any {
	sched := map[string]any{"delayMs": delayMs}
	if timeoutMs > 0 {
		sched["timeoutMs"] = timeoutMs
	}
	list := make([]any, 0, len(procs))
	for _, p := range procs {
		list = append(list, p)
	}
	return map[string]any{
		"Open": map[string]any{"transitions": []any{map[string]any{
			"name": "Fire", "next": "Done", "manual": false, "schedule": sched, "processors": list}}},
		"Done": map[string]any{},
	}
}

// mnSample declares every field the scenarios' entities carry.
const mnSample = `{"k":1,"amount":1}`

// mnSetup imports, locks and gives the model its workflow, then creates one
// entity.
func mnSetup(t *testing.T, c *client.Client, model, wf string) uuid.UUID {
	t.Helper()
	if err := c.ImportModel(t, model, 1, mnSample); err != nil {
		t.Fatalf("ImportModel: %v", err)
	}
	if err := c.LockModel(t, model, 1); err != nil {
		t.Fatalf("LockModel: %v", err)
	}
	if err := c.ImportWorkflow(t, model, 1, wf); err != nil {
		t.Fatalf("ImportWorkflow: %v", err)
	}
	id, err := c.CreateEntity(t, model, 1, mnSample)
	if err != nil {
		t.Fatalf("CreateEntity: %v", err)
	}
	return id
}

// mnReceived counts the requests cc received for id.
func mnReceived(t *testing.T, cc parity.ComputeClient, id uuid.UUID) int {
	t.Helper()
	n := 0
	for _, r := range cc.Received(t) {
		if r.EntityID == id.String() {
			n++
		}
	}
	return n
}

// mnCountEvents counts the entity's StateMachine events of eventType.
func mnCountEvents(t *testing.T, c *client.Client, id uuid.UUID, eventType string) int {
	t.Helper()
	resp, err := c.GetAuditEvents(t, id)
	if err != nil {
		t.Fatalf("GetAuditEvents: %v", err)
	}
	n := 0
	for i := range resp.Items {
		if resp.Items[i].AuditEventType != "StateMachine" {
			continue
		}
		sm, err := resp.Items[i].AsStateMachine()
		if err != nil {
			t.Fatalf("AsStateMachine: %v", err)
		}
		if sm.EventType == eventType {
			n++
		}
	}
	return n
}

// mnAwait polls cond every 100ms.
func mnAwait(t *testing.T, within time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(within)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("%s not seen within %s", what, within)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// mnLongAnswer is the answer limit of a processor a scenario keeps in flight
// across a stale period; the cluster allows it through
// CYODA_CALLOUT_RESPONSE_TIMEOUT_MAX_MS (mnLongAnswerEnv).
const (
	mnLongAnswer    = 240000
	mnLongAnswerEnv = "CYODA_CALLOUT_RESPONSE_TIMEOUT_MAX_MS=300000"
)
