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
// top-level Integer leaf `age`, so the criterion evaluator's model-read
// resolves a declared numeric type for `$.age` (the read is eager once the
// criterion carries `$.age` as a data path — see evaluateCriterion's
// search.ConditionFieldPaths gate). That is what makes a comparison operator
// (GREATER_THAN) type-directed rather than degrading to non-match on an
// untyped leaf.
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
// workflow engine are type-directed: the evaluator eagerly loads the model's
// FieldsMap (once the criterion carries at least one data-field path) so a
// comparison/equality leaf resolves against the declared type. Tests that
// exercise data-field criteria must register the model the same way
// production does — otherwise a referenced leaf fails closed either with a
// model-load error, an undeclared-path error (search.ValidateKnownPaths —
// see TestEvaluateCriterion_UndeclaredPathAbortsTheSave), or a declared-but-
// type-mismatched error (search.ValidateConditionValueTypes — see
// TestEvaluateCriterion_DataFieldPathUntypedFailsClosed).
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
// GREATER_THAN data-field criterion evaluates type-directed (numeric) —
// proving the declared types loaded from the model store are what drive the
// match. The same criterion against a path the model does NOT declare fails
// closed instead of degrading to non-match — see
// TestEvaluateCriterion_UndeclaredPathAbortsTheSave (criterion_model_boundary_test.go).
// A path the model DOES declare, but with a type the operand cannot parse
// into, also fails closed — see TestEvaluateCriterion_DataFieldPathUntypedFailsClosed
// below.
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

// TestEvaluateCriterion_DataFieldPathUntypedFailsClosed is the companion that
// isolates the model's contribution: a model IS registered (schema loads
// successfully, no error) and DOES declare `$.age` — as Boolean — so
// search.ValidateKnownPaths' undeclared-path check has nothing to say about
// this criterion (that gap is covered separately by
// TestEvaluateCriterion_UndeclaredPathAbortsTheSave). The GREATER_THAN
// operand (5) parses into none of `$.age`'s declared types, so
// search.ValidateConditionValueTypes fails closed — distinct from BOTH a
// genuine model-load error (TestEvaluateCriterion_FailsClosedOnGenuineModelLoadError)
// and a successful, type-directed match/non-match
// (TestEvaluateCriterion_DataFieldTypeDirectedWithRegisteredModel). A
// declared-but-type-mismatched comparison leaf is a structural fault in the
// criterion, not a row-dependent non-match, so it must not be silently read
// as "doesn't match" (correctness-over-availability).
//
// Before Task 7 wired in search.ValidateConditionValueTypes, this same
// fixture (with `$.age` left UNDECLARED entirely) isolated
// internal/match/prepared.go's own leafNode expansion-failure branch
// instead — that branch is still reachable (any criterion whose only
// data leaf uses a non-parse-constrained operator skips
// ValidateConditionValueTypes' type check entirely), but no longer through
// THIS fixture: an undeclared `$.age` is now caught earlier, by
// search.ValidateKnownPaths, which would make this test a duplicate of
// TestEvaluateCriterion_UndeclaredPathAbortsTheSave instead of exercising
// the type-mismatch branch its name and doc promise. Declaring `$.age` (with
// an incompatible type) restores that isolation.
func TestEvaluateCriterion_DataFieldPathUntypedFailsClosed(t *testing.T) {
	engine, factory := setupEngine(t)
	ctx := ctxWithTenant(testTenant)
	ref := spi.ModelRef{EntityName: "person", ModelVersion: "1.0"}

	// Register a model that declares `name` (String) and `age` (Boolean) —
	// `age` IS declared, so the undeclared-path check accepts it, but a
	// GREATER_THAN 5 operand parses into none of Boolean's forms.
	registerModelFields(t, ctx, factory, ref, map[string]schema.DataType{
		"name": schema.String,
		"age":  schema.Boolean,
	})

	entity := makeEntity("e1", ref, map[string]any{"age": 30})

	_, _, err := engine.evaluateCriterion(simpleCriterion("$.age", "GREATER_THAN", 5), entity, &criterionContext{ctx: ctx})
	if err == nil {
		t.Fatal("expected an error: $.age is declared Boolean, so operand 5 parses into none of its declared types")
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

// TestEvaluateCriterion_LifecycleCriterionNeedsNoModel proves the model-read
// gate: a pure lifecycle/state criterion carries no data-field path (see
// search.ConditionFieldPaths), so evaluateCriterion never touches the model
// store for it — it evaluates correctly even when the model store is
// unavailable. The unavailability only fails criteria that actually
// reference data leaves. The companion,
// TestEvaluateCriterion_TemporalMetaUnderTextOperatorIsRefused
// (criterion_model_boundary_test.go), proves the other half of the same
// gate: the model READ is skipped here, but the VALIDATION CALL
// (search.ValidateConditionValueTypes) is never gated — it always runs with
// a nil model — because that is the one call that still refuses a
// text/pattern operator on a temporal meta field for a lifecycle-only
// criterion.
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
