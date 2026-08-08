package workflow

import (
	"errors"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/plugins/memory"
)

// newEngineWithFailingModelStore builds an engine on the standard
// setupEngine fixture, but wraps the factory so ModelStore(ctx) fails — the
// branch at engine.go that sets loadErr. It returns an entity whose model
// declares $.age as an Integer (irrelevant here since the store never comes
// up, but keeps the fixture parallel to the healthy variant).
func newEngineWithFailingModelStore(t *testing.T) (*Engine, *spi.Entity, *criterionContext) {
	t.Helper()
	baseFactory := memory.NewStoreFactory()
	t.Cleanup(func() { baseFactory.Close() })
	uuids := common.NewTestUUIDGenerator()
	txMgr := baseFactory.NewTransactionManager(uuids)
	factory := &errModelStoreFactory{StoreFactory: baseFactory, err: errors.New("model store down")}
	engine := NewEngine(factory, uuids, txMgr)

	ctx := ctxWithTenant(testTenant)
	ref := spi.ModelRef{EntityName: "person", ModelVersion: "1.0"}
	entity := makeEntity("e1", ref, map[string]any{"age": 30})

	return engine, entity, &criterionContext{ctx: ctx}
}

// newEngineWithHealthyModelStore builds an engine on the standard
// setupEngine fixture with a model registered that declares $.age as an
// Integer, so the criterion's first leaf genuinely resolves a declared type.
func newEngineWithHealthyModelStore(t *testing.T) (*Engine, *spi.Entity, *criterionContext) {
	t.Helper()
	engine, factory := setupEngine(t)
	ctx := ctxWithTenant(testTenant)
	ref := spi.ModelRef{EntityName: "person", ModelVersion: "1.0"}
	registerTypedModel(t, ctx, factory, ref)

	entity := makeEntity("e1", ref, map[string]any{"age": 30})

	return engine, entity, &criterionContext{ctx: ctx}
}

// TestEvaluateCriterion_InfraFailureBeatsStructuralError pins spec §5's
// precedence: with the model store down, a criterion that ALSO carries a
// structural fault must report the infrastructure failure, not the client
// error. Reporting a 400 for a server-side outage is the wrong way round, and
// the design philosophy says the operation fails closed on the unavailable
// dependency.
//
// This INVERTS the pre-split order, where the match error was checked first
// and loadErr was discarded without even being logged.
//
// The condition is chosen so its FIRST leaf resolves a declared type: without
// that, loadErr never latches and the test would assert nothing. A leaf like
// `state == "X"` would not do — a purely lifecycle criterion loads no schema.
func TestEvaluateCriterion_InfraFailureBeatsStructuralError(t *testing.T) {
	criterion := []byte(`{
		"type": "group",
		"operator": "OR",
		"conditions": [
			{"type":"simple","jsonPath":"$.age","operatorType":"GREATER_THAN","value":5},
			{"type":"simple","jsonPath":"$.x","operatorType":"IS_CHANGED"}
		]
	}`)

	e, entity, cc := newEngineWithFailingModelStore(t)

	_, _, err := e.evaluateCriterion(criterion, entity, cc)
	if err == nil {
		t.Fatal("evaluateCriterion() = nil error, want the infra failure")
	}
	if !errors.Is(err, ErrCriterionTypingInfra) {
		t.Fatalf("evaluateCriterion() error = %v, want one wrapping ErrCriterionTypingInfra "+
			"(the structural fault on the sibling leaf must not mask a model-store outage)", err)
	}
}

// TestEvaluateCriterion_StructuralErrorReportedWhenStoreIsHealthy is the
// control: with the store up, the same criterion reports the structural fault.
// Without this row the test above would pass on an engine that always returned
// the infra error.
func TestEvaluateCriterion_StructuralErrorReportedWhenStoreIsHealthy(t *testing.T) {
	criterion := []byte(`{
		"type": "group",
		"operator": "OR",
		"conditions": [
			{"type":"simple","jsonPath":"$.age","operatorType":"GREATER_THAN","value":5},
			{"type":"simple","jsonPath":"$.x","operatorType":"IS_CHANGED"}
		]
	}`)

	e, entity, cc := newEngineWithHealthyModelStore(t)

	_, _, err := e.evaluateCriterion(criterion, entity, cc)
	if err == nil {
		t.Fatal("evaluateCriterion() = nil error, want the structural fault")
	}
	if errors.Is(err, ErrCriterionTypingInfra) {
		t.Fatalf("evaluateCriterion() error = %v, want the structural fault, not an infra error", err)
	}
	if err.Error() != "unsupported operator: IS_CHANGED" {
		t.Errorf("evaluateCriterion() error = %q, want %q", err.Error(), "unsupported operator: IS_CHANGED")
	}
}
