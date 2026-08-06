package e2e_test

// tx_lifecycle_e2e_test.go — running-Postgres coverage for the deferred
// transaction scope that owns an entity write's transaction lifecycle.
//
// These scenarios need their own app with a deliberately tiny pool. Run against
// the package's shared TestMain stack (CYODA_POSTGRES_MAX_CONNS=5) they cannot
// isolate, and a saturation scenario would stall the rest of the suite for a
// full acquire timeout.
//
// Panic injection runs IN THE SERVER, via cfg.ExternalProcessing. A panic raised
// on the gRPC compute member would unwind on the member's goroutine and reach
// the engine as a dispatch failure, not as a panic through the handler's
// Begin→Commit window — which is the thing under test. localproc.DispatchCriteria
// is the one dispatch entry point with no recover, so a panicking FUNCTION
// criterion is what reaches the handler intact.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/app"
	"github.com/cyoda-platform/cyoda-go/internal/testing/localproc"
)

// --- harness ---------------------------------------------------------------

// newTinyPoolHarness builds a postgres-backed app with a pool small enough that
// a handful of leaked connections exhausts it, so "did the connection come back"
// is observable rather than inferred.
func newTinyPoolHarness(t *testing.T, maxConns int32) *callbackHarness {
	t.Helper()
	return newTinyPoolHarnessConfigured(t, maxConns, nil)
}

// newTinyPoolHarnessConfigured is newTinyPoolHarness with an extra cfg mutator
// applied after the pool sizing. It also stamps a per-test application_name on
// the harness pool's DSN so the pg_stat_activity probe can be scoped to this
// harness's own backends — see harnessAppName.
func newTinyPoolHarnessConfigured(t *testing.T, maxConns int32, configure func(*app.Config)) *callbackHarness {
	t.Helper()
	baseURL := pgURLFromEnv(t)
	return newCallbackHarnessConfigured(t, func(cfg *app.Config) {
		// Read by the postgres plugin's own getenv at factory-open time, which
		// happens inside app.New — i.e. after this mutator runs. pgxpool.ParseConfig
		// forwards application_name as a PostgreSQL runtime parameter.
		t.Setenv("CYODA_POSTGRES_URL", withAppName(t, baseURL, harnessAppName(t)))
		t.Setenv("CYODA_POSTGRES_MAX_CONNS", strconv.Itoa(int(maxConns)))
		t.Setenv("CYODA_POSTGRES_MIN_CONNS", "0")
		if configure != nil {
			configure(cfg)
		}
	})
}

// harnessAppName is the application_name this test's harness pool reports. It is
// derived from the test name so the probe below and the harness agree without
// threading a value through newTinyPoolHarness's signature (Tasks 10 and 11
// consume that signature as published).
func harnessAppName(t *testing.T) string {
	t.Helper()
	name := "txlife-" + strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			return r
		}
		return '-'
	}, t.Name())
	if len(name) > 63 { // PostgreSQL truncates application_name at NAMEDATALEN-1
		name = name[:63]
	}
	return name
}

// withAppName returns dsn with application_name set to name, replacing any value
// already present.
func withAppName(t *testing.T, dsn, name string) string {
	t.Helper()
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse postgres URL: %v", err)
	}
	q := u.Query()
	q.Set("application_name", name)
	u.RawQuery = q.Encode()
	return u.String()
}

// newTinyPoolProcHarness is newTinyPoolHarness with an in-process dispatcher, so
// a registered callout runs on the SERVER's request goroutine. The gRPC compute
// member the callback harness connects is unused by these scenarios.
func newTinyPoolProcHarness(t *testing.T, maxConns int32) (*callbackHarness, *localproc.LocalProcessingService) {
	t.Helper()
	svc := localproc.New()
	h := newTinyPoolHarnessConfigured(t, maxConns, func(cfg *app.Config) {
		cfg.ExternalProcessing = svc
	})
	return h, svc
}

// idleInTransactionConns counts THIS harness's backends parked mid-transaction.
// That state is precisely the signature of a transaction that was neither
// committed nor rolled back and whose pooled connection never came back.
//
// It is read from PostgreSQL (a pgxpool Stat only ever sees its own connections)
// but scoped by application_name to the harness under test. Counting every
// backend on the database would fold in the shared TestMain stack's scheduler and
// reaper transactions, and any of those opening inside the poll window would make
// a `<= before` assertion unreachable — a flake, not a finding. The observer pool
// carries a distinct application_name so it can never count itself.
func idleInTransactionConns(t *testing.T) int {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), withAppName(t, pgURLFromEnv(t), "txlife-observer"))
	if err != nil {
		t.Fatalf("open observer pool: %v", err)
	}
	defer pool.Close()
	var n int
	err = pool.QueryRow(context.Background(),
		`SELECT count(*) FROM pg_stat_activity
		 WHERE datname = current_database()
		   AND application_name = $1
		   AND state LIKE 'idle in transaction%'`, harnessAppName(t)).Scan(&n)
	if err != nil {
		t.Fatalf("read activity: %v", err)
	}
	return n
}

// pgURLFromEnv returns the base DSN TestMain published. Callers that need a
// scoped one run it through withAppName.
func pgURLFromEnv(t *testing.T) string {
	t.Helper()
	// TestMain sets CYODA_POSTGRES_URL for the whole run; App.StoreFactory() is
	// not a route to the pool (it returns the modelcache wrapper, which holds
	// the real factory in an unexported field and forwards only the
	// spi.StoreFactory methods).
	url := os.Getenv("CYODA_POSTGRES_URL")
	if url == "" {
		t.Fatal("CYODA_POSTGRES_URL is unset; TestMain did not run")
	}
	return url
}

// waitFor polls cond until it holds or the deadline passes. Fault tests assert
// consistency (the connection came back), not a precise interleave.
func waitFor(t *testing.T, within time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("%s: condition not met within %v", what, within)
}

// txLifeGateWF is a workflow whose single automated transition is guarded by a
// FUNCTION criterion — the callout shape that reaches the handler intact.
func txLifeGateWF(name, criterion string) string {
	return fmt.Sprintf(`{
		"importMode": "REPLACE",
		"workflows": [{
			"version": "1.1", "name": %q, "initialState": "NONE", "active": true,
			"states": {
				"NONE":   {"transitions": [{"name": "gate", "next": "ACTIVE", "manual": false,
					"criterion": {"type":"function","function":{"name":%q}}}]},
				"ACTIVE": {}
			}
		}]
	}`, name, criterion)
}

// txLifeSegmentWF segments before the gate: a COMMIT_BEFORE_DISPATCH processor
// on the first transition commits TX_pre and opens TX_post, and the criterion on
// the NEXT automated transition then runs with TX_post live.
func txLifeSegmentWF(name, processor, criterion string) string {
	return fmt.Sprintf(`{
		"importMode": "REPLACE",
		"workflows": [{
			"version": "1.1", "name": %q, "initialState": "NONE", "active": true,
			"states": {
				"NONE":   {"transitions": [{"name": "init", "next": "STAGED", "manual": false,
					"processors": [{"type": "calculator", "name": %q,
						"executionMode": "COMMIT_BEFORE_DISPATCH",
						"config": {"attachEntity": true, "calculationNodesTags": "", "startNewTxOnDispatch": true}}]}]},
				"STAGED": {"transitions": [{"name": "gate", "next": "DONE", "manual": false,
					"criterion": {"type":"function","function":{"name":%q}}}]},
				"DONE":   {}
			}
		}]
	}`, name, processor, criterion)
}

const txLifeSample = `{"name":"tx-life","amount":1,"status":"new"}`

// --- coverage row 1: a panic in an owned write returns the connection --------

func TestE2E_PanicInOwnedWrite_ReturnsConnection(t *testing.T) {
	h, svc := newTinyPoolProcHarness(t, 3)
	svc.RegisterCriteria("txlife-boom", func(context.Context, *spi.Entity, json.RawMessage) (bool, error) {
		panic("injected panic in criterion callout")
	})
	h.setupModelSampleWithWorkflow(t, "txlife-panicky", txLifeSample, txLifeGateWF("txlife-panicky-wf", "txlife-boom"))
	h.setupModelSampleWithWorkflow(t, "txlife-healthy", txLifeSample, workflowV1)

	before := idleInTransactionConns(t)

	_, status, body := h.CreateEntity(t, "txlife-panicky", 1, txLifeSample)
	if status != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 with a ticket; body: %s", status, body)
	}

	waitFor(t, 15*time.Second, "connection returned to the pool",
		func() bool { return idleInTransactionConns(t) <= before })

	// The decisive check: the pool is still usable. A leaked connection makes
	// this fail with an acquire timeout rather than a subtle counter drift.
	if _, s, b := h.CreateEntity(t, "txlife-healthy", 1, txLifeSample); s != http.StatusOK {
		t.Fatalf("node could not serve after a recovered panic: %d %s", s, b)
	}
}

// --- coverage row 2: repeated panics do not take the node out ---------------

// TestE2E_RepeatedPanics_NodeKeepsServing asserts request handling continues —
// NOT that health is green. The recovery middleware stores false on the health
// flag and nothing resets it, so the health endpoint reports DOWN after the
// first recovered panic. That is the existing contract and is deliberate: a node
// that has panicked has unknown state.
func TestE2E_RepeatedPanics_NodeKeepsServing(t *testing.T) {
	const poolSize = 3
	h, svc := newTinyPoolProcHarness(t, poolSize)
	svc.RegisterCriteria("txlife-boom-repeat", func(context.Context, *spi.Entity, json.RawMessage) (bool, error) {
		panic("injected panic in criterion callout")
	})
	h.setupModelSampleWithWorkflow(t, "txlife-repeat", txLifeSample, txLifeGateWF("txlife-repeat-wf", "txlife-boom-repeat"))
	h.setupModelSampleWithWorkflow(t, "txlife-repeat-ok", txLifeSample, workflowV1)

	// Well beyond pool size: if a panic held its connection, the pool would be
	// exhausted several iterations before this loop ends.
	for i := 0; i < 4*poolSize; i++ {
		if _, status, body := h.CreateEntity(t, "txlife-repeat", 1, txLifeSample); status != http.StatusInternalServerError {
			t.Fatalf("panic %d: status = %d, want 500; body: %s", i, status, body)
		}
	}

	if _, status, body := h.CreateEntity(t, "txlife-repeat-ok", 1, txLifeSample); status != http.StatusOK {
		t.Fatalf("node stopped serving after %d panics: %d %s", 4*poolSize, status, body)
	}
}

// --- coverage rows 4 / 4a: a failure after segmentation releases TX_post -----

// TestE2E_PanicAfterSegmentation_RollsBackTXPost is coverage row 4's E2E half.
// The unit half lives in the workflow package; this proves it end-to-end on a
// real pool, where a leaked TX_post is observable as a connection that never
// comes back.
func TestE2E_PanicAfterSegmentation_RollsBackTXPost(t *testing.T) {
	assertSegmentReleased(t, "txlife-seg-panic",
		func(svc *localproc.LocalProcessingService, criterion string) {
			svc.RegisterCriteria(criterion, func(context.Context, *spi.Entity, json.RawMessage) (bool, error) {
				panic("injected panic after the segment boundary")
			})
		},
		http.StatusInternalServerError)
}

// TestE2E_CriterionCalloutFailsMidCascade_RollsBackTXPost is coverage row 4a's
// E2E half — the non-panic case, which is the one reachable in ordinary
// operation by a compute node being unavailable.
func TestE2E_CriterionCalloutFailsMidCascade_RollsBackTXPost(t *testing.T) {
	assertSegmentReleased(t, "txlife-seg-fail",
		func(svc *localproc.LocalProcessingService, criterion string) {
			svc.RegisterCriteria(criterion, func(context.Context, *spi.Entity, json.RawMessage) (bool, error) {
				return false, errors.New("compute node unavailable")
			})
		},
		-1) // any non-2xx
}

// assertSegmentReleased drives one create through a COMMIT_BEFORE_DISPATCH
// segment into a failing gate criterion and asserts the three things that must
// hold afterwards: the request failed, TX_pre's work is durable (the engine
// committed it before the callout), and TX_post is gone.
func assertSegmentReleased(t *testing.T, model string, register func(*localproc.LocalProcessingService, string), wantStatus int) {
	t.Helper()
	h, svc := newTinyPoolProcHarness(t, 3)

	segmenter := model + "-proc"
	criterion := model + "-crit"
	svc.RegisterProcessor(segmenter, func(_ context.Context, e *spi.Entity, _ spi.ProcessorDefinition) (*spi.Entity, error) {
		return e, nil
	})
	register(svc, criterion)
	h.setupModelSampleWithWorkflow(t, model, txLifeSample, txLifeSegmentWF(model+"-wf", segmenter, criterion))

	before := idleInTransactionConns(t)

	entityID, status, body := h.CreateEntity(t, model, 1, txLifeSample)
	switch {
	case wantStatus > 0 && status != wantStatus:
		t.Fatalf("status = %d, want %d; body: %s", status, wantStatus, body)
	case wantStatus < 0 && status == http.StatusOK:
		t.Fatalf("cascade reported success despite the gate criterion failing; body: %s", body)
	}

	waitFor(t, 15*time.Second, "TX_post released",
		func() bool { return idleInTransactionConns(t) <= before })

	// The create response carries no id on a failed cascade; find the entity the
	// engine committed into TX_pre by listing the model.
	if entityID == "" {
		entityID = h.firstEntityID(t, model)
	}
	if entityID == "" {
		t.Fatal("TX_pre's committed segment is not visible; the test did not segment and proves nothing")
	}
	state, s := h.GetEntityState(t, entityID)
	if s != http.StatusOK {
		t.Fatalf("TX_pre's committed segment is not readable: GET returned %d", s)
	}
	if state == "DONE" {
		t.Fatal("TX_post's transition landed durably; the failed segment was committed, not rolled back")
	}

	// And the node still serves — a leaked TX_post on a 3-connection pool would
	// eventually make this fail with an acquire timeout.
	if _, s, b := h.CreateEntity(t, model, 1, txLifeSample); s == http.StatusServiceUnavailable {
		t.Fatalf("pool exhausted after the failed cascade: %d %s", s, b)
	}
}

// firstEntityID returns the id of the first entity of a model, or "".
func (h *callbackHarness) firstEntityID(t *testing.T, model string) string {
	t.Helper()
	resp := h.DoAuth(t, http.MethodGet, fmt.Sprintf("/api/entity/%s/1", model), "", "")
	body := h.readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		return ""
	}
	var arr []map[string]any
	if err := json.Unmarshal([]byte(body), &arr); err != nil || len(arr) == 0 {
		return ""
	}
	meta, _ := arr[0]["meta"].(map[string]any)
	id, _ := meta["id"].(string)
	return id
}

// --- coverage row 8a: a cancelled client still gets its transaction back ----

// TestE2E_ClientCancelledRequest_StillRollsBack is coverage row 8a's E2E half.
// The rollback context is derived WithoutCancel, so it keeps the UserContext
// verifyTenant reads while dropping the cancellation — the rollback runs even
// though the request that opened the transaction is gone.
func TestE2E_ClientCancelledRequest_StillRollsBack(t *testing.T) {
	h, svc := newTinyPoolProcHarness(t, 3)
	svc.RegisterCriteria("txlife-slow", func(context.Context, *spi.Entity, json.RawMessage) (bool, error) {
		time.Sleep(2 * time.Second) // outlive the client below
		return false, errors.New("callout gave up")
	})
	h.setupModelSampleWithWorkflow(t, "txlife-slow", txLifeSample, txLifeGateWF("txlife-slow-wf", "txlife-slow"))

	before := idleInTransactionConns(t)

	reqCtx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost,
		h.baseURL+"/api/entity/JSON/txlife-slow/1", strings.NewReader(txLifeSample))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+h.token(t))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err == nil {
		resp.Body.Close()
		t.Fatalf("expected the client to give up first, got %d", resp.StatusCode)
	}

	waitFor(t, 20*time.Second, "abandoned request's transaction rolled back",
		func() bool { return idleInTransactionConns(t) <= before })
}

// --- coverage row 3: a failing joined callback leaves its owner's tx alone ---

// txLifeAbsentEntityID is a well-formed UUID that names no entity, so the route's
// UUID binding passes and the failure comes from the flow, not the edge.
const txLifeAbsentEntityID = "00000000-0000-4000-8000-0000000004a3"

// joinedCallbackOutcome is what the compute member records from inside the
// processor, asserted on the test goroutine afterwards.
type joinedCallbackOutcome struct {
	deleteStatus int
	createStatus int
	createdID    string
}

// TestE2E_JoinedCallbackFailure_DoesNotRollBackOwner is coverage row 3's E2E
// half. It runs over the REAL gRPC dispatch path so the callback genuinely joins
// the owner's transaction via the signed tx-token, and the joined write's own
// deferred scope must decline to touch a transaction it does not own.
//
// The joined write is a DELETE of an absent id, chosen deliberately.
// DeleteEntity's very first act is beginScope, so the 404 surfaces from
// entityStore.Get with the joined scope live and `defer scope.Release()` armed —
// the code path under test. A joined CREATE against an unregistered model would
// NOT do: CreateEntity resolves and validates the model ~50 lines before
// beginScope, so it returns 404 having constructed no scope at all, and the test
// would pass against a Release that rolls back its owner unconditionally.
//
// Two consequences are asserted, and the second is the load-bearing one:
// a SECOND joined write, issued after the failure, must still succeed and must
// still be durable after the owner commits. That can only hold if the owner's
// transaction survived the first callback intact.
func TestE2E_JoinedCallbackFailure_DoesNotRollBackOwner(t *testing.T) {
	h := newCallbackHarness(t)

	const primary = "txlife-owner"
	const secondary = "txlife-owner-child"
	outcome := make(chan joinedCallbackOutcome, 1)

	h.RegisterProc("txlife-owner-proc", func(rc *reqCtx) (map[string]any, error) {
		var out joinedCallbackOutcome

		del, err := rc.DeleteEntity(txLifeAbsentEntityID)
		if err != nil {
			return nil, fmt.Errorf("callback delete: %w", err)
		}
		out.deleteStatus = del.StatusCode

		// The owner's transaction must still be open and still committable. This
		// write lands in the same buffer and is read back below after the commit.
		created, err := rc.CreateEntity(secondary, 1, txLifeSample)
		if err != nil {
			return nil, fmt.Errorf("callback create: %w", err)
		}
		out.createStatus, out.createdID = created.StatusCode, created.EntityID

		outcome <- out
		return nil, nil // the owner's cascade carries on regardless
	})

	primaryWF := `{
		"importMode": "REPLACE",
		"workflows": [{
			"version": "1.1", "name": "txlife-owner-wf", "initialState": "NONE", "active": true,
			"states": {
				"NONE":   {"transitions": [{"name": "init", "next": "ACTIVE", "manual": false,
					"processors": [{"type": "calculator", "name": "txlife-owner-proc",
						"executionMode": "SYNC",
						"config": {"attachEntity": true, "calculationNodesTags": ""}}]}]},
				"ACTIVE": {}
			}
		}]
	}`
	h.setupModelSampleWithWorkflow(t, primary, txLifeSample, primaryWF)
	h.setupModelSampleWithWorkflow(t, secondary, txLifeSample, workflowV1)

	primaryID, status, body := h.CreateEntity(t, primary, 1, txLifeSample)
	if status != http.StatusOK {
		t.Fatalf("owner create failed; a joined callback rolled back its owner's transaction: %d %s", status, body)
	}

	var out joinedCallbackOutcome
	select {
	case out = <-outcome:
	case <-time.After(30 * time.Second):
		t.Fatal("the processor's joined callback never completed")
	}

	if out.deleteStatus != http.StatusNotFound {
		t.Fatalf("joined delete status = %d, want 404 from inside the flow; the test must fail AFTER beginScope", out.deleteStatus)
	}
	if out.createStatus != http.StatusOK {
		t.Fatalf("the joined write after the failure returned %d; the failed callback took its owner's transaction down with it", out.createStatus)
	}

	// The owner committed: its entity is durable and reached its settled state.
	state, s := h.GetEntityState(t, primaryID)
	if s != http.StatusOK {
		t.Fatalf("owner's entity is not readable after commit: %d", s)
	}
	if state != "ACTIVE" {
		t.Fatalf("owner's entity state = %q, want ACTIVE", state)
	}

	// ...and so is the write the second joined callback buffered into it. A
	// rollback by the first callback would have discarded this.
	if out.createdID == "" {
		t.Fatal("joined create returned no entity id")
	}
	if _, s := h.GetEntityState(t, out.createdID); s != http.StatusOK {
		t.Fatalf("the joined callback's write is not durable after the owner committed: GET returned %d", s)
	}
}
