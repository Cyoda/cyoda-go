package workflow

import (
	"context"
	"errors"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/domain/model/schema"
	"github.com/cyoda-platform/cyoda-go/plugins/memory"
)

// registerTypedModel saves a model descriptor whose schema declares a single
// top-level Integer leaf `age`, so the criterion evaluator's lazily-loaded
// FieldsMap resolver returns a declared numeric type for `$.age`. That is what
// makes a comparison operator (GREATER_THAN) type-directed rather than degrading
// to non-match on an untyped leaf.
func registerTypedModel(t *testing.T, ctx context.Context, factory spi.StoreFactory, ref spi.ModelRef) {
	t.Helper()
	node := schema.NewObjectNode()
	node.SetChild("age", schema.NewLeafNode(schema.Integer))
	raw, err := schema.Marshal(node)
	if err != nil {
		t.Fatalf("schema.Marshal: %v", err)
	}
	ms, err := factory.ModelStore(ctx)
	if err != nil {
		t.Fatalf("ModelStore: %v", err)
	}
	if err := ms.Save(ctx, &spi.ModelDescriptor{Ref: ref, Schema: raw}); err != nil {
		t.Fatalf("Save model: %v", err)
	}
}

// registerModelFields saves a model descriptor whose schema declares the given
// top-level leaf fields with their declared types. Data-field criteria in the
// workflow engine are type-directed: the evaluator lazily loads the model's
// FieldsMap so a comparison/equality leaf resolves against the declared type.
// Tests that exercise data-field criteria must register the model the same way
// production does — otherwise a referenced leaf either fails closed (model-load
// error) or degrades to non-match (declared type absent).
func registerModelFields(t *testing.T, ctx context.Context, factory spi.StoreFactory, ref spi.ModelRef, fields map[string]schema.DataType) {
	t.Helper()
	node := schema.NewObjectNode()
	for name, dt := range fields {
		node.SetChild(name, schema.NewLeafNode(dt))
	}
	raw, err := schema.Marshal(node)
	if err != nil {
		t.Fatalf("schema.Marshal: %v", err)
	}
	ms, err := factory.ModelStore(ctx)
	if err != nil {
		t.Fatalf("ModelStore: %v", err)
	}
	if err := ms.Save(ctx, &spi.ModelDescriptor{Ref: ref, Schema: raw}); err != nil {
		t.Fatalf("Save model: %v", err)
	}
}

// errModelStoreFactory wraps a StoreFactory but makes ModelStore fail, modelling
// a genuine model-store outage. Every other capability delegates to the inner
// factory so the rest of the engine still works.
type errModelStoreFactory struct {
	spi.StoreFactory
	err error
}

func (f *errModelStoreFactory) ModelStore(context.Context) (spi.ModelStore, error) {
	return nil, f.err
}

// TestEvaluateCriterion_DataFieldTypeDirectedWithRegisteredModel proves the
// Task-8 wiring: with a registered model declaring `$.age` Integer, a
// GREATER_THAN data-field criterion evaluates type-directed (numeric), and the
// same criterion with NO model registered degrades to non-match — proving the
// declared types loaded from the model store are what drive the match.
func TestEvaluateCriterion_DataFieldTypeDirectedWithRegisteredModel(t *testing.T) {
	engine, factory := setupEngine(t)
	ctx := ctxWithTenant(testTenant)
	ref := spi.ModelRef{EntityName: "person", ModelVersion: "1.0"}
	registerTypedModel(t, ctx, factory, ref)

	entity := makeEntity("e1", ref, map[string]any{"age": 30})

	// age(30) > 5 → typed numeric comparison matches.
	got, _, err := engine.evaluateCriterion(simpleCriterion("$.age", "GREATER_THAN", 5), entity, &criterionContext{ctx: ctx})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !got {
		t.Fatal("age(30) GREATER_THAN 5 must match under a declared-Integer field")
	}

	// age(30) > 50 → typed numeric comparison does NOT match (real evaluation,
	// not an always-true wiring stub).
	got, _, err = engine.evaluateCriterion(simpleCriterion("$.age", "GREATER_THAN", 50), entity, &criterionContext{ctx: ctx})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got {
		t.Fatal("age(30) GREATER_THAN 50 must NOT match")
	}
}

// TestEvaluateCriterion_DataFieldNonMatchWhenPathUntyped is the companion that
// isolates the model's contribution: a model IS registered (schema loads
// successfully, no error), but it declares only `name` — `$.age` carries no
// declared type — so the GREATER_THAN comparison leaf degrades to non-match.
// This is the intended graceful "path not typed" behaviour, distinct from a
// genuine load error which fails closed.
func TestEvaluateCriterion_DataFieldNonMatchWhenPathUntyped(t *testing.T) {
	engine, factory := setupEngine(t)
	ctx := ctxWithTenant(testTenant)
	ref := spi.ModelRef{EntityName: "person", ModelVersion: "1.0"}

	// Register a model that declares `name` (String) but NOT `age`.
	node := schema.NewObjectNode()
	node.SetChild("name", schema.NewLeafNode(schema.String))
	raw, err := schema.Marshal(node)
	if err != nil {
		t.Fatalf("schema.Marshal: %v", err)
	}
	ms, err := factory.ModelStore(ctx)
	if err != nil {
		t.Fatalf("ModelStore: %v", err)
	}
	if err := ms.Save(ctx, &spi.ModelDescriptor{Ref: ref, Schema: raw}); err != nil {
		t.Fatalf("Save model: %v", err)
	}

	entity := makeEntity("e1", ref, map[string]any{"age": 30})

	got, _, err := engine.evaluateCriterion(simpleCriterion("$.age", "GREATER_THAN", 5), entity, &criterionContext{ctx: ctx})
	if err != nil {
		t.Fatalf("successful schema load must not error, got: %v", err)
	}
	if got {
		t.Fatal("with no declared type for $.age the comparison leaf must degrade to non-match")
	}
}

// TestEvaluateCriterion_FailsClosedOnGenuineModelLoadError proves the
// correctness-over-availability policy: when a data-field criterion needs the
// model and the model store is unavailable (genuine load error, NOT the
// (nil,nil) no-schema case), evaluateCriterion surfaces the error rather than
// silently under-matching.
func TestEvaluateCriterion_FailsClosedOnGenuineModelLoadError(t *testing.T) {
	baseFactory := memory.NewStoreFactory()
	t.Cleanup(func() { baseFactory.Close() })
	uuids := common.NewTestUUIDGenerator()
	txMgr := baseFactory.NewTransactionManager(uuids)
	wantErr := errors.New("model store down")
	factory := &errModelStoreFactory{StoreFactory: baseFactory, err: wantErr}
	engine := NewEngine(factory, uuids, txMgr)

	ctx := ctxWithTenant(testTenant)
	ref := spi.ModelRef{EntityName: "person", ModelVersion: "1.0"}
	entity := makeEntity("e1", ref, map[string]any{"age": 30})

	_, _, err := engine.evaluateCriterion(simpleCriterion("$.age", "GREATER_THAN", 5), entity, &criterionContext{ctx: ctx})
	if err == nil {
		t.Fatal("a genuine model-load error on a data-field criterion must fail closed (surface the error)")
	}
	if !errors.Is(err, wantErr) {
		t.Errorf("surfaced error must wrap the store error; got: %v", err)
	}
}

// TestEvaluateCriterion_LifecycleCriterionNeedsNoModel proves the lazy-load
// contract: a pure lifecycle/state criterion never touches the model store, so
// it evaluates correctly even when the model store is unavailable — the
// unavailability only fails criteria that actually reference data leaves.
func TestEvaluateCriterion_LifecycleCriterionNeedsNoModel(t *testing.T) {
	baseFactory := memory.NewStoreFactory()
	t.Cleanup(func() { baseFactory.Close() })
	uuids := common.NewTestUUIDGenerator()
	txMgr := baseFactory.NewTransactionManager(uuids)
	factory := &errModelStoreFactory{StoreFactory: baseFactory, err: errors.New("model store down")}
	engine := NewEngine(factory, uuids, txMgr)

	ctx := ctxWithTenant(testTenant)
	ref := spi.ModelRef{EntityName: "person", ModelVersion: "1.0"}
	entity := makeEntity("e1", ref, map[string]any{"age": 30})
	entity.Meta.State = "CREATED"

	got, _, err := engine.evaluateCriterion(lifecycleCriterion("state", "EQUALS", "CREATED"), entity, &criterionContext{ctx: ctx})
	if err != nil {
		t.Fatalf("lifecycle criterion must not touch the model store (no error expected), got: %v", err)
	}
	if !got {
		t.Fatal("state EQUALS CREATED must match")
	}
}
