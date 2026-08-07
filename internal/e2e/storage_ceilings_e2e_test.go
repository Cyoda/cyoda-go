package e2e_test

// storage_ceilings_e2e_test.go — a saturated connection pool must fail an entity
// write FAST, with a retryable 503 STORAGE_UNAVAILABLE, on both entry points.
//
// These are fault tests: they assert the outcome (one 503, quickly, carrying the
// right code) rather than a precise interleave. They run on their own
// one-connection app — against the package's shared stack they could not
// saturate anything in isolation, and a saturation scenario there would stall
// every other test for a full acquire timeout.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	cepb "github.com/cyoda-platform/cyoda-go/api/grpc/cloudevents"
	cyodapb "github.com/cyoda-platform/cyoda-go/api/grpc/cyoda"
	"github.com/cyoda-platform/cyoda-go/app"
	internalgrpc "github.com/cyoda-platform/cyoda-go/internal/grpc"
	"github.com/cyoda-platform/cyoda-go/internal/testing/localproc"
)

const (
	storageCeilingSample = `{"name":"pool-hold","amount":1,"status":"new"}`
	// Short enough that a saturated write reports in well under the test's
	// fail-fast budget, long enough not to fire on ordinary scheduling jitter.
	storageCeilingAcquireTimeout = "500ms"
)

// storageCeilingModel derives a per-test model name. Each test here stands up
// its own app, but they all share the package's Postgres container, so a fixed
// name would collide on the second import (MODEL_ALREADY_LOCKED).
func storageCeilingModel(t *testing.T, prefix string) string {
	t.Helper()
	return prefix + "-" + strings.ToLower(strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			return r
		}
		return '-'
	}, t.Name()))
}

// holdHarness is a one-connection stack whose gate criterion parks the server's
// request goroutine with the transaction — and therefore the pool's only
// connection — in hand, until the test releases it.
type holdHarness struct {
	*callbackHarness
	model   string
	entered chan struct{} // closed by the criterion once it is holding
	release chan struct{}
	done    chan createEntityResult
}

// newSaturatedPoolHarness builds the stack and imports the model whose single
// automated transition is gated by the blocking criterion.
func newSaturatedPoolHarness(t *testing.T) *holdHarness {
	t.Helper()
	svc := localproc.New()
	h := newTinyPoolHarnessConfigured(t, 1, func(cfg *app.Config) {
		// Read by the postgres plugin's own getenv at factory-open time, which
		// happens inside app.New — i.e. after this mutator runs.
		t.Setenv("CYODA_POSTGRES_ACQUIRE_TIMEOUT", storageCeilingAcquireTimeout)
		// In-process dispatch, so the criterion below runs on the SERVER's
		// request goroutine with the transaction open.
		cfg.ExternalProcessing = svc
		// The scan loop would queue behind the held connection for the whole
		// test and log acquire failures unrelated to what is under test.
		cfg.Scheduler.Enabled = false
	})

	hh := &holdHarness{
		callbackHarness: h,
		model:           storageCeilingModel(t, "pool-hold"),
		entered:         make(chan struct{}),
		release:         make(chan struct{}),
		done:            make(chan createEntityResult, 1),
	}

	var once sync.Once
	criterion := hh.model + "-crit"
	svc.RegisterCriteria(criterion, func(context.Context, *spi.Entity, json.RawMessage) (bool, error) {
		once.Do(func() { close(hh.entered) })
		<-hh.release
		return false, nil // no match: the holder's own write is not what is asserted
	})
	h.setupModelSampleWithWorkflow(t, hh.model, storageCeilingSample,
		txLifeGateWF(hh.model+"-wf", criterion))
	return hh
}

// hold drives one create into the gate criterion and returns once that criterion
// is parked holding the only connection. The returned func releases it and waits
// for the holding request to finish.
func (hh *holdHarness) hold(t *testing.T) func() {
	t.Helper()
	// Seed the cached bearer on the test goroutine, and warm the model cache, so
	// neither the holder nor the saturated request below needs a connection
	// before it reaches Begin.
	hh.token(t)

	go func() { hh.done <- hh.CreateEntityRaw(hh.model, 1, storageCeilingSample) }()

	select {
	case <-hh.entered:
	case <-time.After(30 * time.Second):
		t.Fatal("the gate criterion never ran; the pool was never saturated")
	}

	var once sync.Once
	return func() {
		once.Do(func() {
			close(hh.release)
			select {
			case <-hh.done:
			case <-time.After(30 * time.Second):
				t.Error("the holding request never returned")
			}
		})
	}
}

// TestE2E_SaturatedPool_WriteReturns503 is the HTTP half: the write fails fast
// with a retryable 503 STORAGE_UNAVAILABLE instead of queueing behind the pool.
func TestE2E_SaturatedPool_WriteReturns503(t *testing.T) {
	h := newSaturatedPoolHarness(t)
	release := h.hold(t)
	defer release()

	start := time.Now()
	_, status, body := h.CreateEntity(t, h.model, 1, storageCeilingSample)
	elapsed := time.Since(start)

	if status != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body: %s", status, body)
	}
	assertStorageUnavailableProblem(t, body)
	if elapsed > 3*time.Second {
		t.Fatalf("queued for %v instead of failing fast", elapsed)
	}
	t.Logf("saturated-pool write returned 503 after %v; body: %s", elapsed, body)
}

// assertStorageUnavailableProblem checks the RFC 9457 body carries the error
// code and the retryable flag, and that the client-facing detail stays generic —
// the cause is infrastructure and belongs in the log, not the response.
func assertStorageUnavailableProblem(t *testing.T, body string) {
	t.Helper()
	var pd struct {
		Status int            `json:"status"`
		Detail string         `json:"detail"`
		Props  map[string]any `json:"properties"`
	}
	if err := json.Unmarshal([]byte(body), &pd); err != nil {
		t.Fatalf("problem detail is not JSON: %v; body: %s", err, body)
	}
	if code, _ := pd.Props["errorCode"].(string); code != "STORAGE_UNAVAILABLE" {
		t.Errorf("errorCode = %q, want STORAGE_UNAVAILABLE; body: %s", code, body)
	}
	if retryable, _ := pd.Props["retryable"].(bool); !retryable {
		t.Errorf("retryable is not set; body: %s", body)
	}
	for _, leak := range []string{"postgres://", "password", "dbname=", "127.0.0.1"} {
		if strings.Contains(strings.ToLower(pd.Detail), leak) {
			t.Errorf("client-facing detail leaks infrastructure (%q): %s", leak, pd.Detail)
		}
	}
}

// TestE2E_SaturatedPool_GRPCEnvelope is the gRPC half. HTTP and gRPC are
// separate entry points and both must be covered. Over gRPC the envelope's code
// field is the generic class CLIENT_ERROR; the domain code is carried in the
// message, which is the established convention for this surface.
func TestE2E_SaturatedPool_GRPCEnvelope(t *testing.T) {
	h := newSaturatedPoolHarness(t)
	release := h.hold(t)
	defer release()

	start := time.Now()
	env, err := h.createEntityGRPC(h.model, 1, storageCeilingSample)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("gRPC create: %v", err)
	}

	if env.Success {
		t.Fatal("gRPC create succeeded on a saturated one-connection pool")
	}
	if env.Error == nil {
		t.Fatal("failed gRPC create carried no error envelope")
	}
	if env.Error.Code != "CLIENT_ERROR" {
		t.Errorf("Error.Code = %q, want CLIENT_ERROR (the envelope class)", env.Error.Code)
	}
	if !strings.HasPrefix(env.Error.Message, "STORAGE_UNAVAILABLE:") {
		t.Errorf("Error.Message = %q, want the STORAGE_UNAVAILABLE domain code", env.Error.Message)
	}
	if env.Error.Retryable == nil || !*env.Error.Retryable {
		t.Errorf("Error.Retryable = %v, want true", env.Error.Retryable)
	}
	if elapsed > 3*time.Second {
		t.Fatalf("queued for %v instead of failing fast", elapsed)
	}
	t.Logf("saturated-pool gRPC create failed after %v: %s", elapsed, env.Error.Message)
}

// --- the server-side ceilings ----------------------------------------------
//
// Both are transactions the DATABASE ends, and they are reported differently on
// purpose. A transaction reclaimed by the idle ceiling is transient contention —
// a retry on a fresh one may well succeed — so it is a retryable 503. A
// statement cancelled by statement_timeout is not: re-running a statement that
// just exceeded the ceiling will exceed it again, so it is a 500 with a ticket,
// and the cause is named in the log rather than advertised to the caller.

const (
	// Small enough to fire inside a test, comfortably larger than every
	// statement the harness's own boot and model import issue.
	idleCeilingLimit = 1500 * time.Millisecond
	stmtCeilingLimit = time.Second
)

// newReclaimedTxHarness builds a stack whose idle-in-transaction ceiling fires
// during a single SYNC callout, and imports a model whose only transition runs
// that callout. In-process dispatch is what makes the sleep happen on the
// SERVER's request goroutine, with the transaction open and nothing written in
// between — the one shape that lets the idle clock run out.
func newReclaimedTxHarness(t *testing.T) (*callbackHarness, string) {
	t.Helper()
	svc := localproc.New()
	h := newCallbackHarnessConfigured(t, func(cfg *app.Config) {
		// Read by the postgres plugin's own getenv at factory-open time, which
		// happens inside app.New — i.e. after this mutator runs.
		t.Setenv("CYODA_POSTGRES_IDLE_IN_TX_TIMEOUT", idleCeilingLimit.String())
		cfg.ExternalProcessing = svc
		// The scan loop would keep tripping the same ceiling in the background
		// and log failures unrelated to what is under test.
		cfg.Scheduler.Enabled = false
	})

	model := storageCeilingModel(t, "idle-ceiling")
	proc := model + "-sleeper"
	registerSleeper(svc, proc, idleCeilingLimit+2*time.Second)
	h.setupModelSampleWithWorkflow(t, model, storageCeilingSample, pgCeilingPipelineWF(model+"-wf", proc))
	return h, model
}

// TestE2E_IdleInTxCeiling_Returns503 is coverage row 9 on the HTTP door. Without
// the classification the same failure surfaces as an opaque 500, which tells the
// caller nothing and — worse — tells them not to retry something that would
// have worked.
func TestE2E_IdleInTxCeiling_Returns503(t *testing.T) {
	h, model := newReclaimedTxHarness(t)

	entityID, status, body := h.CreateEntity(t, model, 1, storageCeilingSample)

	if status != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body: %s", status, body)
	}
	assertStorageUnavailableProblem(t, body)

	// Nothing partially durable: the reclaimed transaction took its writes with
	// it, so a retry starts from a clean slate rather than a half-written entity.
	if entityID != "" {
		if _, s := h.GetEntityState(t, entityID); s == http.StatusOK {
			t.Fatalf("entity %s is readable after its transaction was reclaimed", entityID)
		}
	}
	t.Logf("reclaimed transaction reported as 503: %s", body)
}

// TestE2E_IdleInTxCeiling_GRPCEnvelope is the same failure on the second entry
// point. Over gRPC the envelope's code field is the generic class CLIENT_ERROR
// and the domain code rides in the message — the established convention for this
// surface, matching the saturated-pool case above.
func TestE2E_IdleInTxCeiling_GRPCEnvelope(t *testing.T) {
	h, model := newReclaimedTxHarness(t)

	env, err := h.createEntityGRPC(model, 1, storageCeilingSample)
	if err != nil {
		t.Fatalf("gRPC create: %v", err)
	}
	if env.Success {
		t.Fatal("create succeeded despite the transaction being reclaimed")
	}
	if env.Error == nil {
		t.Fatal("failed gRPC create carried no error envelope")
	}
	if env.Error.Code != "CLIENT_ERROR" {
		t.Errorf("Error.Code = %q, want CLIENT_ERROR (the envelope class)", env.Error.Code)
	}
	if !strings.HasPrefix(env.Error.Message, "STORAGE_UNAVAILABLE:") {
		t.Errorf("Error.Message = %q, want the STORAGE_UNAVAILABLE domain code", env.Error.Message)
	}
	if env.Error.Retryable == nil || !*env.Error.Retryable {
		t.Errorf("Error.Retryable = %v, want true", env.Error.Retryable)
	}
	t.Logf("reclaimed transaction reported over gRPC as: %s", env.Error.Message)
}

// newStatementCeilingHarness builds a stack whose statement_timeout is short,
// and a model with no callouts at all — the statement this ceiling cancels is
// the entity write itself. It returns the model plus one committed entity for
// the scenarios below to contend on.
func newStatementCeilingHarness(t *testing.T) (*callbackHarness, string, string) {
	t.Helper()
	h := newCallbackHarnessConfigured(t, func(cfg *app.Config) {
		t.Setenv("CYODA_POSTGRES_STATEMENT_TIMEOUT", stmtCeilingLimit.String())
		cfg.Scheduler.Enabled = false
	})
	model := storageCeilingModel(t, "stmt-ceiling")
	h.setupModelSampleWithWorkflow(t, model, storageCeilingSample, workflowV1)

	entityID, status, body := h.CreateEntity(t, model, 1, storageCeilingSample)
	if status != http.StatusOK {
		t.Fatalf("seed entity: %d %s", status, body)
	}
	return h, model, entityID
}

// holdRowLock takes and holds a row lock on ONE entity, from a connection of the
// test's own, so a write to that entity waits on it rather than proceeding.
//
// A lock wait is the deterministic way to build a statement that exceeds
// statement_timeout: the ceiling counts the whole statement, waiting included,
// so the cancellation lands at a known moment instead of depending on how long
// some scan happens to take on the machine running the test. It is also a
// realistic cause — contention on a hot row is exactly what puts an ordinary
// write over the ceiling in production.
//
// It is a ROW lock, not a table lock, on purpose. Every test in this package
// shares one PostgreSQL container, so `LOCK TABLE entities` would stall the
// shared stack's own background loops — and any neighbour writing an entity —
// for as long as it is held. This blocks exactly one row, which only the test
// that seeded it ever touches.
func holdRowLock(t *testing.T, entityID string) func() {
	t.Helper()
	return holdRowLockOn(t, `SELECT entity_id FROM entities WHERE entity_id = $1 FOR UPDATE`, entityID)
}

// holdScheduledTaskRowLock is holdRowLock aimed at the OTHER table an entity
// write touches. The settle-time arm/cancel pass re-arms via an upsert keyed on
// the task's deterministic id, so locking the row the seed create armed makes
// the next save's re-arm wait — the same deterministic route past
// statement_timeout, on a statement the engine issues rather than the handler.
func holdScheduledTaskRowLock(t *testing.T, entityID string) func() {
	t.Helper()
	return holdRowLockOn(t, `SELECT id FROM scheduled_tasks WHERE entity_id = $1 FOR UPDATE`, entityID)
}

// holdRowLockOn is the shared body: open a connection of the test's own, run
// query (which must select exactly one row FOR UPDATE) and hold the transaction
// open until the returned func releases it.
func holdRowLockOn(t *testing.T, query, entityID string) func() {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)

	fail := func(format string, args ...any) {
		cancel()
		t.Fatalf(format, args...)
	}

	pool, err := pgxpool.New(ctx, withAppName(t, pgURLFromEnv(t), "stmt-ceiling-locker"))
	if err != nil {
		fail("open locker pool: %v", err)
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		pool.Close()
		fail("begin locker transaction: %v", err)
	}

	var locked string
	if err := tx.QueryRow(ctx, query, entityID).Scan(&locked); err != nil {
		_ = tx.Rollback(ctx)
		pool.Close()
		fail("lock row for entity %s (%s): %v", entityID, query, err)
	}

	var once sync.Once
	return func() {
		once.Do(func() {
			_ = tx.Rollback(ctx)
			pool.Close()
			cancel()
		})
	}
}

// TestE2E_StatementTimeout_Returns500WithTicket is coverage row 12 on the HTTP
// door. 500, not 503 and not 409: the statement that was cancelled would be
// cancelled again, so nothing here may advertise a retry. What the change buys
// is the log line naming statement_timeout — the response itself stays the
// deliberately opaque ticket.
func TestE2E_StatementTimeout_Returns500WithTicket(t *testing.T) {
	h, _, entityID := newStatementCeilingHarness(t)
	release := holdRowLock(t, entityID)
	defer release()

	start := time.Now()
	resp := h.DoAuth(t, http.MethodDelete, "/api/entity/"+entityID, "", "")
	body := h.readBody(t, resp)
	elapsed := time.Since(start)

	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d after %v, want 500 — re-running the statement would exceed the ceiling again; body: %s", resp.StatusCode, elapsed, body)
	}
	assertCancelledStatementProblem(t, body)
	t.Logf("cancelled statement reported as 500 after %v: %s", elapsed, body)
}

// --- The same ceiling, reached from inside the workflow engine --------------
//
// The two scenarios above cancel a statement the entity handler issues, and the
// handler classifies its own store errors. The engine's are different: an
// unclassified engine error reaches the entity service's catch-all, which mints
// a 400 WORKFLOW_FAILED whose detail is the raw error text. Below is the door
// that reaches the engine's own store calls.

// storageCeilingUpdate is a second payload, so the update under test is a real
// write rather than a no-op.
const storageCeilingUpdate = `{"name":"pool-hold","amount":2,"status":"held"}`

// scheduledCeilingWF carries a Schedule far enough out that nothing ever fires
// it. The Schedule is the whole point: the engine's settle-time arm/cancel pass
// writes to the scheduled-task store on EVERY save of an entity on this
// workflow, which puts that store — and its failures — on the ordinary write
// path rather than in a corner of it.
func scheduledCeilingWF(name string) string {
	return fmt.Sprintf(`{
		"importMode": "REPLACE",
		"workflows": [{
			"version": "1.1", "name": %q, "initialState": "Open", "active": true,
			"states": {
				"Open":   {"transitions": [{"name": "AutoClose", "next": "Closed", "manual": false,
					"schedule": {"delayMs": 3600000}}]},
				"Closed": {}
			}
		}]
	}`, name)
}

// newScheduledReArmCeilingHarness is newStatementCeilingHarness with a scheduled
// workflow, plus one committed entity whose ScheduledTask row the scenarios
// below contend on.
func newScheduledReArmCeilingHarness(t *testing.T) (*callbackHarness, string, string) {
	t.Helper()
	h := newCallbackHarnessConfigured(t, func(cfg *app.Config) {
		t.Setenv("CYODA_POSTGRES_STATEMENT_TIMEOUT", stmtCeilingLimit.String())
		// Otherwise the scan loop fires (or contends on) the very task these
		// scenarios lock.
		cfg.Scheduler.Enabled = false
	})
	model := storageCeilingModel(t, "sched-rearm-ceiling")
	h.setupModelSampleWithWorkflow(t, model, storageCeilingSample, scheduledCeilingWF(model+"-wf"))

	entityID, status, body := h.CreateEntity(t, model, 1, storageCeilingSample)
	if status != http.StatusOK {
		t.Fatalf("seed entity: %d %s", status, body)
	}
	return h, model, entityID
}

// TestE2E_StatementTimeout_ScheduledReArm_IsNotAClientError — a cancelled
// statement inside the engine's re-arm is a server-side condition. Reported as
// 400 WORKFLOW_FAILED it is wrong twice over: it blames the caller for an
// outage they did not cause, and — because a 4xx detail is full domain detail
// by contract — it puts the driver's own text, table names and SQLSTATE in the
// response body.
func TestE2E_StatementTimeout_ScheduledReArm_IsNotAClientError(t *testing.T) {
	h, _, entityID := newScheduledReArmCeilingHarness(t)
	release := holdScheduledTaskRowLock(t, entityID)
	defer release()

	start := time.Now()
	resp := h.DoAuth(t, http.MethodPut, "/api/entity/JSON/"+entityID, storageCeilingUpdate, "")
	body := h.readBody(t, resp)
	elapsed := time.Since(start)

	if resp.StatusCode == http.StatusBadRequest {
		t.Fatalf("a cancelled statement in the engine was blamed on the caller after %v; body: %s", elapsed, body)
	}
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d after %v, want 500 — re-running the statement would exceed the ceiling again; body: %s",
			resp.StatusCode, elapsed, body)
	}
	assertCancelledStatementProblem(t, body)
	// The engine names the store it was writing to; a client has no business
	// knowing which tables back a scheduled transition.
	for _, leak := range []string{"scheduled_tasks", "reconcile"} {
		if strings.Contains(strings.ToLower(body), leak) {
			t.Errorf("client-facing body leaks engine/storage internals (%q): %s", leak, body)
		}
	}
	t.Logf("cancelled re-arm reported as 500 after %v: %s", elapsed, body)
}

// TestE2E_StatementTimeout_ScheduledReArm_GRPCEnvelope is the second entry
// point. Over gRPC a 4xx would put the same raw text in Error.Message, which is
// the only place the failure is described at all.
func TestE2E_StatementTimeout_ScheduledReArm_GRPCEnvelope(t *testing.T) {
	h, _, entityID := newScheduledReArmCeilingHarness(t)
	release := holdScheduledTaskRowLock(t, entityID)
	defer release()

	env, err := h.updateEntityGRPC(entityID, storageCeilingUpdate)
	if err != nil {
		t.Fatalf("gRPC update: %v", err)
	}
	if env.Success {
		t.Fatal("update succeeded despite the re-arm statement being cancelled")
	}
	if env.Error == nil {
		t.Fatal("failed gRPC update carried no error envelope")
	}
	if env.Error.Retryable != nil && *env.Error.Retryable {
		t.Errorf("a cancelled statement was advertised as retryable: %+v", env.Error)
	}
	if !strings.Contains(env.Error.Message, "ticket") {
		t.Errorf("Error.Message = %q, want the generic ticket message", env.Error.Message)
	}
	for _, leak := range []string{"sqlstate", "57014", "canceling statement", "statement_timeout", "scheduled_tasks", "pgx"} {
		if strings.Contains(strings.ToLower(env.Error.Message), leak) {
			t.Errorf("gRPC envelope leaks internal detail (%q): %s", leak, env.Error.Message)
		}
	}
	t.Logf("cancelled re-arm reported over gRPC as: %s", env.Error.Message)
}

// TestE2E_StatementTimeout_ConditionalDeleteIsNotAConflict is the endpoint that
// makes the split matter most.
//
// DeleteEntitiesConditional records a per-id failure and carries on to commit.
// The cancelled statement has already aborted the PostgreSQL transaction, so
// that commit's own probe comes back 25P02 in_failed_sql_transaction — which,
// read as a serialization conflict, hands the caller a RETRYABLE 409 for a
// statement the ceiling would cancel on every attempt. It must not.
//
// The same response is where the per-id error text lands, so this is also where
// driver text would reach the wire.
func TestE2E_StatementTimeout_ConditionalDeleteIsNotAConflict(t *testing.T) {
	h, model, entityID := newStatementCeilingHarness(t)
	release := holdRowLock(t, entityID)
	defer release()

	cond := `{"type":"simple","jsonPath":"$.name","operatorType":"EQUALS","value":"pool-hold"}`
	resp := h.DoAuth(t, http.MethodDelete, fmt.Sprintf("/api/entity/%s/1", model), cond, "")
	body := h.readBody(t, resp)

	if resp.StatusCode == http.StatusConflict {
		t.Fatalf("a cancelled statement was reported as a retryable conflict; the client would retry something the ceiling cancels again. body: %s", body)
	}
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body: %s", resp.StatusCode, body)
	}
	assertCancelledStatementProblem(t, body)
	t.Logf("conditional delete over a cancelled statement reported as 500: %s", body)
}

// assertCancelledStatementProblem checks the RFC 9457 body carries a ticket, is
// NOT advertised as retryable, and leaks none of the driver's own text.
func assertCancelledStatementProblem(t *testing.T, body string) {
	t.Helper()
	var pd struct {
		Status int            `json:"status"`
		Detail string         `json:"detail"`
		Ticket string         `json:"ticket"`
		Props  map[string]any `json:"properties"`
	}
	if err := json.Unmarshal([]byte(body), &pd); err != nil {
		t.Fatalf("problem detail is not JSON: %v; body: %s", err, body)
	}
	if pd.Ticket == "" {
		t.Errorf("500 carried no ticket, so the log line naming the ceiling cannot be correlated: %s", body)
	}
	if retryable, ok := pd.Props["retryable"].(bool); ok && retryable {
		t.Errorf("a cancelled statement was advertised as retryable: %s", body)
	}
	assertNoInternalDetail(t, body)
}

// assertNoInternalDetail fails if a client-facing body carries driver text, SQL,
// SQLSTATEs or connection detail.
func assertNoInternalDetail(t *testing.T, body string) {
	t.Helper()
	for _, leak := range []string{
		"sqlstate", "canceling statement", "statement_timeout", "pgx", "pgconn",
		"insert into", "postgres://", "password", "dbname=",
	} {
		if strings.Contains(strings.ToLower(body), leak) {
			t.Errorf("client-facing body leaks internal detail (%q): %s", leak, body)
		}
	}
}

// TestE2E_StatementTimeout_GRPCEnvelope is row 12's second entry point.
func TestE2E_StatementTimeout_GRPCEnvelope(t *testing.T) {
	h, _, entityID := newStatementCeilingHarness(t)
	release := holdRowLock(t, entityID)
	defer release()

	env, err := h.deleteEntityGRPC(entityID)
	if err != nil {
		t.Fatalf("gRPC delete: %v", err)
	}
	if env.Success {
		t.Fatal("delete succeeded despite the statement being cancelled")
	}
	if env.Error == nil {
		t.Fatal("failed gRPC delete carried no error envelope")
	}
	if env.Error.Code != "SERVER_ERROR" {
		t.Errorf("Error.Code = %q, want SERVER_ERROR", env.Error.Code)
	}
	if env.Error.Retryable != nil && *env.Error.Retryable {
		t.Error("a cancelled statement was advertised as retryable over gRPC")
	}
	assertNoInternalDetail(t, env.Error.Message)
	t.Logf("cancelled statement reported over gRPC as: %s", env.Error.Message)
}

// txEnvelope is the error-bearing subset of EntityTransactionResponse.
type txEnvelope struct {
	Success bool `json:"success"`
	Error   *struct {
		Code      string `json:"code"`
		Message   string `json:"message"`
		Retryable *bool  `json:"retryable"`
	} `json:"error"`
}

// createEntityGRPC issues an EntityCreateRequest over the real gRPC entity API
// (the member's connection), unjoined.
func (h *callbackHarness) createEntityGRPC(model string, version int, payload string) (txEnvelope, error) {
	var data map[string]any
	if err := json.Unmarshal([]byte(payload), &data); err != nil {
		return txEnvelope{}, err
	}
	reqCE, err := internalgrpc.NewCloudEvent(internalgrpc.EntityCreateRequest, map[string]any{
		"id":         "storage-ceiling-create",
		"dataFormat": "JSON",
		"payload": map[string]any{
			"model": map[string]any{"name": model, "version": version},
			"data":  data,
		},
	})
	if err != nil {
		return txEnvelope{}, err
	}
	client := cyodapb.NewCloudEventsServiceClient(h.member.conn)
	respCE, err := client.EntityManage(h.grpcCtx(""), reqCE)
	if err != nil {
		return txEnvelope{}, err
	}
	return parseTxEnvelope(respCE)
}

// updateEntityGRPC issues an EntityUpdateRequest over the real gRPC entity API
// (the member's connection), unjoined, with no transition — a loopback update,
// the gRPC twin of PUT /api/entity/{format}/{entityId}.
func (h *callbackHarness) updateEntityGRPC(entityID, payload string) (txEnvelope, error) {
	var data map[string]any
	if err := json.Unmarshal([]byte(payload), &data); err != nil {
		return txEnvelope{}, err
	}
	reqCE, err := internalgrpc.NewCloudEvent(internalgrpc.EntityUpdateRequest, map[string]any{
		"id":         "storage-ceiling-update",
		"dataFormat": "JSON",
		"payload": map[string]any{
			"entityId": entityID,
			"data":     data,
		},
	})
	if err != nil {
		return txEnvelope{}, err
	}
	client := cyodapb.NewCloudEventsServiceClient(h.member.conn)
	respCE, err := client.EntityManage(h.grpcCtx(""), reqCE)
	if err != nil {
		return txEnvelope{}, err
	}
	return parseTxEnvelope(respCE)
}

// deleteEntityGRPC issues an EntityDeleteRequest over the real gRPC entity API
// (the member's connection), unjoined. EntityDeleteResponse carries the same
// error envelope shape as EntityTransactionResponse, so txEnvelope reads both.
func (h *callbackHarness) deleteEntityGRPC(entityID string) (txEnvelope, error) {
	reqCE, err := internalgrpc.NewCloudEvent(internalgrpc.EntityDeleteRequest, map[string]any{
		"id":       "storage-ceiling-delete",
		"entityId": entityID,
	})
	if err != nil {
		return txEnvelope{}, err
	}
	client := cyodapb.NewCloudEventsServiceClient(h.member.conn)
	respCE, err := client.EntityManage(h.grpcCtx(""), reqCE)
	if err != nil {
		return txEnvelope{}, err
	}
	return parseTxEnvelope(respCE)
}

func parseTxEnvelope(ce *cepb.CloudEvent) (txEnvelope, error) {
	_, payload, err := internalgrpc.ParseCloudEvent(ce)
	if err != nil {
		return txEnvelope{}, err
	}
	var env txEnvelope
	if err := json.Unmarshal(payload, &env); err != nil {
		return txEnvelope{}, errors.New("unmarshal EntityTransactionResponse: " + err.Error())
	}
	return env, nil
}

// --- the async-search scan's own ceiling ------------------------------------
//
// Async search is the one workload whose purpose is to run long, so on postgres
// it carries its own statement ceiling rather than sharing the interactive one:
// a single knob would force operators to choose between fast-failing interactive
// writes and long analytical scans. It is applied with SET LOCAL inside the
// scan's own transaction, which is what keeps the two apart.

const (
	// Small enough that any real scan exceeds it, and the smallest value the
	// plugin accepts at all (below 1ms a ceiling truncates to "disabled").
	searchCeilingLimit = "1ms"

	// Every async search is a point-in-time read — SubmitAsync stamps one — so
	// the scan reads every version behind the model through the bi-temporal
	// DISTINCT ON. These make that scan take milliseconds rather than
	// microseconds, so "it exceeded 1ms" is not a claim about how fast the
	// machine running the test is.
	searchCeilingSeedVersions = 5000
)

// newSearchCeilingHarness builds a ONE-connection stack whose async-search
// ceiling is 1ms, imports a model with no callouts, and seeds enough history
// behind it that the scan is not instantaneous.
//
// One connection on purpose: the scenarios below assert that the ceiling the
// scan applies does not survive on the connection it borrowed. With a larger
// pool a later request could be handed a different connection and pass without
// ever re-using the poisoned one.
func newSearchCeilingHarness(t *testing.T) (*callbackHarness, string) {
	t.Helper()
	h := newTinyPoolHarnessConfigured(t, 1, func(cfg *app.Config) {
		// Read by the postgres plugin's own getenv at factory-open time, which
		// happens inside app.New — i.e. after this mutator runs.
		t.Setenv("CYODA_POSTGRES_SEARCH_STATEMENT_TIMEOUT", searchCeilingLimit)
		// The scan loop would queue behind the single connection for the whole
		// test and log acquire failures unrelated to what is under test.
		cfg.Scheduler.Enabled = false
	})
	model := storageCeilingModel(t, "search-ceiling")
	h.setupModelSampleWithWorkflow(t, model, storageCeilingSample, workflowV1)

	if _, status, body := h.CreateEntity(t, model, 1, storageCeilingSample); status != http.StatusOK {
		t.Fatalf("seed entity: %d %s", status, body)
	}
	seedEntityVersions(t, model, searchCeilingSeedVersions)
	return h, model
}

// seedEntityVersions copies the model's one committed version n times under
// fresh entity ids, from a connection of the test's own.
//
// Rows, not locks. Every test in this package shares one PostgreSQL container,
// so anything that blocked writes — a table lock, an ALTER — would stall the
// shared stack and its neighbours for as long as it was held. These rows are
// addressed only by this test's model name, and are removed again on cleanup.
func seedEntityVersions(t *testing.T, model string, n int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	pool, err := pgxpool.New(ctx, withAppName(t, pgURLFromEnv(t), "search-ceiling-seeder"))
	if err != nil {
		t.Fatalf("open seeding pool: %v", err)
	}
	defer pool.Close()

	if _, err := pool.Exec(ctx, `
		INSERT INTO entity_versions (tenant_id, entity_id, model_name, model_version, version,
		                             valid_time, transaction_time, wall_clock_time, doc)
		SELECT tenant_id, entity_id || '-' || g, model_name, model_version, version,
		       valid_time, transaction_time, wall_clock_time,
		       jsonb_set(doc, '{_meta,id}', to_jsonb(entity_id || '-' || g))
		FROM entity_versions, generate_series(1, $1) AS g
		WHERE model_name = $2`, n, model); err != nil {
		t.Fatalf("seed %d entity versions for %s: %v", n, model, err)
	}
	t.Cleanup(func() {
		cctx, ccancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer ccancel()
		cpool, err := pgxpool.New(cctx, withAppName(t, pgURLFromEnv(t), "search-ceiling-cleaner"))
		if err != nil {
			return
		}
		defer cpool.Close()
		_, _ = cpool.Exec(cctx, `DELETE FROM entity_versions WHERE model_name = $1`, model)
	})
}

// submitAsyncSearchOn submits a match-all async search and returns the job id.
func (h *callbackHarness) submitAsyncSearchOn(t *testing.T, model string) string {
	t.Helper()
	resp := h.DoAuth(t, http.MethodPost, fmt.Sprintf("/api/search/async/%s/1", model),
		`{"type":"group","operator":"AND","conditions":[]}`, "")
	body := h.readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("submit async search: %d %s", resp.StatusCode, body)
	}
	jobID := strings.Trim(strings.TrimSpace(body), `"`)
	if jobID == "" {
		t.Fatal("submit async search returned an empty job id")
	}
	return jobID
}

// waitForAsyncTerminal polls the status endpoint until the job leaves RUNNING.
func (h *callbackHarness) waitForAsyncTerminal(t *testing.T, jobID string, timeout time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		resp := h.DoAuth(t, http.MethodGet, "/api/search/async/"+jobID+"/status", "", "")
		body := h.readBody(t, resp)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("async search status: %d %s", resp.StatusCode, body)
		}
		var st map[string]any
		if err := json.Unmarshal([]byte(body), &st); err != nil {
			t.Fatalf("async search status is not JSON: %v; body: %s", err, body)
		}
		if s, _ := st["searchJobStatus"].(string); s != "RUNNING" {
			return s
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("async search job %s never reached a terminal status within %v", jobID, timeout)
	return ""
}

// persistedJobError reads the message recorded against the job — the string
// GetJob serves back, and the only place an async failure is reported.
func persistedJobError(t *testing.T, jobID string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, withAppName(t, pgURLFromEnv(t), "search-ceiling-reader"))
	if err != nil {
		t.Fatalf("open reader pool: %v", err)
	}
	defer pool.Close()
	var msg string
	if err := pool.QueryRow(ctx, `SELECT error FROM search_jobs WHERE id = $1`, jobID).Scan(&msg); err != nil {
		t.Fatalf("read search job %s: %v", jobID, err)
	}
	return msg
}

// TestE2E_AsyncSearch_CeilingExceeded_RecordsSanitizedFailure — the persisted
// job message is the one deliberate exception to output sanitization: the job is
// the caller's own work, so it is told which ceiling it hit. That message is
// fixed and names nothing else — a raw driver string here would put SQL, a
// SQLSTATE and connection detail in a record GetJob serves straight back.
func TestE2E_AsyncSearch_CeilingExceeded_RecordsSanitizedFailure(t *testing.T) {
	h, model := newSearchCeilingHarness(t)

	jobID := h.submitAsyncSearchOn(t, model)
	if status := h.waitForAsyncTerminal(t, jobID, 60*time.Second); status != "FAILED" {
		t.Fatalf("async search status = %q, want FAILED — nothing bounded the scan", status)
	}

	msg := persistedJobError(t, jobID)
	if !strings.Contains(msg, "search statement ceiling") {
		t.Fatalf("job error %q does not name the ceiling the caller hit", msg)
	}
	for _, leak := range []string{"pgx", "SELECT", "SQLSTATE", "57014", "statement_timeout", "host=", "password"} {
		if strings.Contains(msg, leak) {
			t.Fatalf("job error leaked internals (%q): %s", leak, msg)
		}
	}
	t.Logf("async search failed with the sanitized message: %s", msg)
}

// TestE2E_SearchCeiling_DoesNotLeakOntoInteractiveStatements guards the split.
// SET LOCAL scopes the search ceiling to the scan's own transaction, so the
// interactive ceiling the pool carries is untouched — the two knobs must not
// collapse into one, or operators would have to choose between fast-failing
// writes and long analytical scans.
//
// The pool here holds ONE connection, so the write and the interactive search
// below run on the very connection the scan borrowed: had the ceiling been set
// on the session rather than the transaction, they would die on it too.
func TestE2E_SearchCeiling_DoesNotLeakOntoInteractiveStatements(t *testing.T) {
	h, model := newSearchCeilingHarness(t)

	jobID := h.submitAsyncSearchOn(t, model)
	if status := h.waitForAsyncTerminal(t, jobID, 60*time.Second); status != "FAILED" {
		t.Fatalf("async search status = %q, want FAILED — the 1ms search ceiling never reached the scan, so this scenario proves nothing", status)
	}

	if _, status, body := h.CreateEntity(t, model, 1, storageCeilingSample); status != http.StatusOK {
		t.Fatalf("an ordinary write failed with %d after the scan ran; the search ceiling leaked onto the interactive path. body: %s", status, body)
	}

	resp := h.DoAuth(t, http.MethodPost, fmt.Sprintf("/api/search/direct/%s/1?pageSize=10", model),
		`{"type":"simple","jsonPath":"$.name","operatorType":"EQUALS","value":"pool-hold"}`, "")
	body := h.readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("an interactive search failed with %d after the scan ran; the search ceiling leaked onto the interactive path. body: %s", resp.StatusCode, body)
	}
}
