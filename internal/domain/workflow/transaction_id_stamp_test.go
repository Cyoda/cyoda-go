package workflow

import (
	"encoding/json"
	"fmt"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/common/commontest"
)

// A scheduled fire's entity carries the fire's own transaction id from the
// start: a lifecycle criterion on transactionId evaluated before any callout
// sees the running transaction, as on the client doors, not the transaction
// that committed the entity before.
func TestFireScheduled_EntityCarriesTheFiresTransactionID(t *testing.T) {
	const armMs = int64(1_700_000_000_000)
	const delayMs = int64(1000)
	engine, factory, _, advance := setupEngineForCBDFire(t, armMs, &mockExternalProcessing{}, 0)
	ctx := ctxWithTenant(testTenant)
	modelRef := spi.ModelRef{EntityName: "fire-txid", ModelVersion: "1.0"}
	seeded := seedFireEntity(t, factory, ctx, "fire-txid-e1", modelRef, "OPEN", true, map[string]any{})

	criterion := json.RawMessage(fmt.Sprintf(
		`{"type":"lifecycle","field":"transactionId","operatorType":"EQUALS","value":%q}`, seeded.Meta.TransactionID))
	wf := spi.WorkflowDefinition{
		Version: "1.1", Name: "FireTxIDWF", InitialState: "OPEN", Active: true,
		States: map[string]spi.StateDefinition{
			"OPEN": {Transitions: []spi.TransitionDefinition{
				{Name: "AutoClose", Next: "CLOSED", Schedule: &spi.TransitionSchedule{DelayMs: delayMs}, Criterion: criterion},
			}},
			"CLOSED": {},
		},
	}
	saveWorkflow(t, factory, ctx, modelRef, []spi.WorkflowDefinition{wf})
	id := taskID(testTenant, "fire-txid-e1", "OPEN", "AutoClose")
	armTask(t, factory, ctx, spi.ScheduledTask{
		ID: id, TenantID: testTenant, Type: spi.ScheduledTaskFireTransition,
		ScheduledTime: armMs + delayMs, EntityID: "fire-txid-e1", ModelName: modelRef.EntityName,
		Transition: "AutoClose", SourceState: "OPEN", ArmedAt: armMs,
		ArmedBy: spi.Principal{ID: "arm-user", Kind: spi.PrincipalUser},
	})
	advance(delayMs)

	r := fireDue(t, engine, commontest.SystemUserContext(testTenant), id)
	if r.Err != nil {
		t.Fatalf("FireScheduledTransition: %v", r.Err)
	}
	if r.Outcome != OutcomeDeclined {
		t.Fatalf("outcome = %v, want declined: the criterion saw the previous transaction's id", r.Outcome)
	}
}
