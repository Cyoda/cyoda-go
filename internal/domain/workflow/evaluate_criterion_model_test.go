package workflow

import (
	"context"
	"errors"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/domain/model/schema"
	"github.com/cyoda-platform/cyoda-go/internal/match"
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
// TestEvaluateCriterion_ContainerPathScalarComparisonAbortsTheSave,
// criterion_model_boundary_test.go).
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

// registerUnionTypedModel saves a model descriptor whose schema declares a
// single top-level leaf carrying MULTIPLE declared scalar types (the model
// has observed both, e.g. an early-lifecycle field that started as an
// Integer and was later also written as a Boolean). Used to isolate
// match.Prepare's own leaf-expansion-failure branch from
// search.ValidateConditionValueTypes: for a RANGE operator (BETWEEN),
// ValidateConditionValueTypes checks EACH bound independently against the
// declared set (operandParsesDeclared, an operator-independent FilterEq
// oracle) and accepts as long as every bound parses into SOME declared
// type — it does not require the SAME type for both bounds. The SPI
// kernel's own expandBetween is stricter: a numeric range requires BOTH
// bounds to parse as a number, a temporal range requires both to parse as a
// timestamp, and only a declared String type accepts a mixed pair
// lexicographically — so a boolean/numeric mixed pair against
// [Integer, Boolean] parses per-element but engages no bucket jointly,
// and match.Prepare (which calls the same expandBetween) fails with
// ErrUnevaluableLeaf while ValidateConditionValueTypes has already
// accepted it.
func registerUnionTypedModel(t *testing.T, ctx context.Context, factory spi.StoreFactory, ref spi.ModelRef, field string, types ...schema.DataType) {
	t.Helper()
	if len(types) == 0 {
		t.Fatal("registerUnionTypedModel: at least one type required")
	}
	leaf := schema.NewLeafNode(types[0])
	leaf.AddScalarTypes(types[1:]...)
	node := schema.NewObjectNode()
	node.SetChild(field, leaf)
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
// A path the model DOES declare, but whose bounds jointly engage no
// declared type, also fails closed — see
// TestEvaluateCriterion_DataFieldPathUntypedFailsClosed below.
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

// TestEvaluateCriterion_DataFieldPathUntypedFailsClosed isolates
// match.Prepare's OWN leaf-expansion-failure branch (internal/match's
// leafNode/ErrUnevaluableLeaf) — distinct from BOTH search.ValidateKnownPaths
// (TestEvaluateCriterion_UndeclaredPathAbortsTheSave — $.age is DECLARED
// here) and search.ValidateConditionValueTypes, which this exact criterion
// does NOT trip.
//
// `$.age` is declared with a UNION of scalar types, [Integer, Boolean] (the
// model has observed both). The criterion is BETWEEN [5, true].
// search.ValidateConditionValueTypes checks each bound independently
// (operandParsesDeclared, an operator-independent oracle) and accepts: "5"
// parses as Integer, "true" parses as Boolean, each individually a member of
// the declared set — so it raises no error. The SPI kernel's own
// expandBetween (which match.Prepare calls) is stricter: a numeric range
// requires BOTH bounds to parse as a number, a temporal range requires both
// to parse as a timestamp, and only a declared String type accepts a mixed
// pair lexicographically — [Integer, Boolean] offers none of those, so no
// bucket engages jointly and expandBetween fails with "range bounds parse
// into no declared type", which leafNode turns into match.ErrUnevaluableLeaf.
//
// Before Task 7 wired in search.ValidateConditionValueTypes, a simpler
// fixture (a single-type field an operand couldn't parse into at all) also
// isolated this same match.Prepare branch — but for the SINGLE-type case,
// ValidateConditionValueTypes' per-element check and match.Prepare's
// engagement check share the identical underlying oracle for every
// comparison and range operator and are structurally guaranteed to agree,
// so no single-type fixture can any longer tell the two apart: with
// ValidateConditionValueTypes now running before match.Prepare, a
// single-type mismatch is always caught by ValidateConditionValueTypes
// first, and match.Prepare's own check becomes unreachable through such a
// fixture — verified: deleting the search.ValidateConditionValueTypes call
// site left the OLD single-type version of this test passing unchanged.
// The union-type BETWEEN mismatch above is the one shape where the two
// checks genuinely disagree, and it is what makes this test's assertion
// (errors.Is(err, match.ErrUnevaluableLeaf)) meaningful: it fails if EITHER
// candidate call site — search.ValidateConditionValueTypes (which must NOT
// have already rejected this criterion) or match.Prepare (which must be the
// one that does) — is wrong.
func TestEvaluateCriterion_DataFieldPathUntypedFailsClosed(t *testing.T) {
	engine, factory := setupEngine(t)
	ctx := ctxWithTenant(testTenant)
	ref := spi.ModelRef{EntityName: "person", ModelVersion: "1.0"}

	registerUnionTypedModel(t, ctx, factory, ref, "age", schema.Integer, schema.Boolean)

	entity := makeEntity("e1", ref, map[string]any{"age": 30})

	_, _, err := engine.evaluateCriterion(simpleCriterion("$.age", "BETWEEN", []any{5, true}), entity, &criterionContext{ctx: ctx})
	if err == nil {
		t.Fatal("expected an error: BETWEEN [5, true] engages no declared type jointly, even though $.age is declared [Integer, Boolean]")
	}
	if !errors.Is(err, match.ErrUnevaluableLeaf) {
		t.Fatalf("expected match.ErrUnevaluableLeaf (match.Prepare's own leaf-expansion-failure branch); "+
			"got a different error, meaning search.ValidateConditionValueTypes rejected this criterion instead "+
			"of accepting it as expected: %v", err)
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
