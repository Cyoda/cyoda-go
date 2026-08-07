package e2e_test

// pg_ceilings_e2e_test.go — coverage row 11b, on a running PostgreSQL.
//
// The idle-in-transaction ceiling bounds one IDLE GAP, not a transaction's
// lifetime. A cascade can therefore spend far more than the ceiling in total
// across many compute-node callouts and still commit, because the engine writes
// to the database between them — for two processors in the same transition, the
// only thing between them is the audit INSERT the engine records per processor
// (internal/domain/workflow/engine_processors.go recordEvent →
// plugins/postgres/sm_audit_store.go Record), and on PostgreSQL that INSERT runs
// inside the live transaction and restarts the server's idle clock.
//
// That reset is load-bearing for the shipped 5m default and was previously
// undesigned, so both halves are asserted: a cascade whose TOTAL callout time
// exceeds the ceiling commits, and — the control that stops the first from being
// vacuous — a SINGLE callout longer than the ceiling does not.

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/app"
	"github.com/cyoda-platform/cyoda-go/internal/testing/localproc"
)

// pgCeilingIdleLimit is the idle-in-transaction ceiling these scenarios run
// under. Small enough to be crossed in seconds, comfortably larger than the
// per-gap sleeps below.
const pgCeilingIdleLimit = 3 * time.Second

// newIdleCeilingHarness builds a postgres-backed stack whose pool opens every
// connection with idle_in_transaction_session_timeout set to
// pgCeilingIdleLimit, and dispatches callouts in process so a processor's sleep
// happens on the server's request goroutine with the transaction open.
func newIdleCeilingHarness(t *testing.T) (*callbackHarness, *localproc.LocalProcessingService) {
	t.Helper()
	svc := localproc.New()
	h := newCallbackHarnessConfigured(t, func(cfg *app.Config) {
		// Read by the postgres plugin's own getenv inside app.New, i.e. after
		// this mutator runs.
		t.Setenv("CYODA_POSTGRES_IDLE_IN_TX_TIMEOUT", pgCeilingIdleLimit.String())
		cfg.ExternalProcessing = svc
	})
	return h, svc
}

// pgCeilingPipelineWF is one automated transition carrying procs as SYNC
// processors, so they all run inline in the same transaction with nothing
// between consecutive callouts but the engine's own per-processor audit write.
func pgCeilingPipelineWF(name string, procs ...string) string {
	defs := make([]string, 0, len(procs))
	for _, p := range procs {
		defs = append(defs, fmt.Sprintf(
			`{"type": "calculator", "name": %q, "executionMode": "SYNC",
			  "config": {"attachEntity": true, "calculationNodesTags": ""}}`, p))
	}
	return fmt.Sprintf(`{
		"importMode": "REPLACE",
		"workflows": [{
			"version": "1.1", "name": %q, "initialState": "NONE", "active": true,
			"states": {
				"NONE":   {"transitions": [{"name": "run", "next": "ACTIVE", "manual": false,
					"processors": [%s]}]},
				"ACTIVE": {}
			}
		}]
	}`, name, strings.Join(defs, ","))
}

// registerSleeper registers a processor that holds the transaction idle for d
// and returns the entity unchanged.
func registerSleeper(svc *localproc.LocalProcessingService, name string, d time.Duration) {
	svc.RegisterProcessor(name, func(_ context.Context, e *spi.Entity, _ spi.ProcessorDefinition) (*spi.Entity, error) {
		time.Sleep(d)
		return e, nil
	})
}

// TestE2E_DeepCascade_ExceedsIdleCeilingInTotal_StillCommits is coverage row
// 11b. Five callouts of one second each spend 5s inside a transaction whose idle
// ceiling is 3s, and the write commits: no single gap crosses the ceiling
// because the per-processor audit INSERT restarts the clock between them.
func TestE2E_DeepCascade_ExceedsIdleCeilingInTotal_StillCommits(t *testing.T) {
	h, svc := newIdleCeilingHarness(t)

	const model = "pgceil-cascade"
	const gap = time.Second
	const steps = 5

	procs := make([]string, 0, steps)
	for i := 0; i < steps; i++ {
		name := fmt.Sprintf("%s-p%d", model, i)
		registerSleeper(svc, name, gap)
		procs = append(procs, name)
	}
	if total := gap * steps; total <= pgCeilingIdleLimit {
		t.Fatalf("total callout time %v does not exceed the %v ceiling; the scenario proves nothing", total, pgCeilingIdleLimit)
	}

	h.setupModelSampleWithWorkflow(t, model, txLifeSample, pgCeilingPipelineWF(model+"-wf", procs...))

	start := time.Now()
	entityID, status, body := h.CreateEntity(t, model, 1, txLifeSample)
	elapsed := time.Since(start)

	if status != http.StatusOK {
		t.Fatalf("status = %d after %v of callouts under a %v idle ceiling; the per-processor audit INSERT should have reset the idle timer. body: %s",
			status, elapsed, pgCeilingIdleLimit, body)
	}
	if elapsed <= pgCeilingIdleLimit {
		t.Fatalf("the cascade took %v, which never crossed the %v ceiling in total; the scenario proves nothing", elapsed, pgCeilingIdleLimit)
	}
	t.Logf("%d callouts of %v committed in %v under a %v idle ceiling", steps, gap, elapsed, pgCeilingIdleLimit)

	// Durable and settled: a commit that silently lost the transition would
	// leave the entity in its initial state.
	state, s := h.GetEntityState(t, entityID)
	if s != http.StatusOK {
		t.Fatalf("entity not readable after commit: %d", s)
	}
	if state != "ACTIVE" {
		t.Fatalf("entity state = %q, want ACTIVE", state)
	}
}

// TestE2E_SingleCalloutExceedsIdleCeiling_Fails is the control for the scenario
// above. One callout longer than the ceiling, with no intervening write, must
// NOT commit — otherwise the ceiling is not reaching the cascade's transaction
// at all and the passing case above would be meaningless.
//
// It asserts only "not committed": which failure the client sees is the concern
// of the error-mapping tasks, not of this one.
func TestE2E_SingleCalloutExceedsIdleCeiling_Fails(t *testing.T) {
	h, svc := newIdleCeilingHarness(t)

	const model = "pgceil-single"
	const proc = model + "-p0"
	registerSleeper(svc, proc, pgCeilingIdleLimit+2*time.Second)

	h.setupModelSampleWithWorkflow(t, model, txLifeSample, pgCeilingPipelineWF(model+"-wf", proc))

	entityID, status, body := h.CreateEntity(t, model, 1, txLifeSample)
	if status == http.StatusOK {
		t.Fatalf("a callout of %v committed under a %v idle ceiling; the ceiling is not reaching the cascade's transaction, so the deep-cascade scenario proves nothing. body: %s",
			pgCeilingIdleLimit+2*time.Second, pgCeilingIdleLimit, body)
	}
	t.Logf("single over-ceiling callout rejected with %d: %s", status, body)

	// Nothing partially durable: the reclaimed transaction took its writes with it.
	if entityID != "" {
		if _, s := h.GetEntityState(t, entityID); s == http.StatusOK {
			t.Fatalf("entity %s is readable after the transaction was reclaimed; the write committed partially", entityID)
		}
	}
	if id := h.firstEntityID(t, model); id != "" {
		t.Fatalf("entity %s from the reclaimed transaction is durable; it should have been rolled back", id)
	}
}
