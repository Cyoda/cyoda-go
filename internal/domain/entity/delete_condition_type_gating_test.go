package entity

import (
	"context"
	"errors"
	"net/http"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go-spi/predicate"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/match"
)

// delete_condition_type_gating_test.go pins the fix to deleteConditionTypeCheck
// (extracted from planDeleteSelection): the model READ is gated on whether
// cond addresses a data path, but the VALIDATION CALL
// (search.ValidateConditionValueTypes) is never gated on whether that read
// was attempted, succeeded, or found a schema.
//
// Before the fix, planDeleteSelection's inline "if node != nil { ... }"
// skipped ValidateConditionValueTypes entirely whenever deleteModelSchemaNode
// returned a nil node — the ordinary "model has no schema yet" state, not a
// failure. That silently accepted a text or pattern operator on a temporal
// meta field (creationDate/lastUpdateTime), which internal/match's
// prepareLifecycle answers with a deliberate, permanent never-match. A NOT
// wrapping that leaf inverts the guard into matching every entity — on
// conditional delete, the highest blast-radius surface this predicate
// reaches, that is a delete-everything.
//
// These tests construct the dangerous GroupCondition{Operator:"NOT"} shape
// directly and call deleteConditionTypeCheck, bypassing
// planDeleteSelection's own search.ValidateCondition call (a few lines
// above the type-check in the real flow): ValidateCondition already rejects
// "NOT" unconditionally today (Task 12 of this plan — accepting NOT
// structurally — has not landed), so a real end-to-end
// DeleteEntitiesConditional call with this shape is refused there first,
// for an unrelated reason, and would pass whether or not this fix exists.
// Calling deleteConditionTypeCheck directly is what actually exercises —
// and would have caught — this specific gating regression, independent of
// when NOT becomes reachable end-to-end.

// gatingModelStore is a minimal spi.ModelStore double, local to this test
// file: it records how many times Get was called, so a test can assert "no
// model read happened at all" — not merely "the read that happened was
// harmless".
type gatingModelStore struct {
	desc    *spi.ModelDescriptor
	getErr  error
	gets    int
	failGet bool
}

func (m *gatingModelStore) Get(context.Context, spi.ModelRef) (*spi.ModelDescriptor, error) {
	m.gets++
	if m.failGet {
		return nil, m.getErr
	}
	return m.desc, nil
}
func (m *gatingModelStore) Save(context.Context, *spi.ModelDescriptor) error { return nil }
func (m *gatingModelStore) GetAll(context.Context) ([]spi.ModelRef, error)   { return nil, nil }
func (m *gatingModelStore) Delete(context.Context, spi.ModelRef) error       { return nil }
func (m *gatingModelStore) Lock(context.Context, spi.ModelRef) error         { return nil }
func (m *gatingModelStore) Unlock(context.Context, spi.ModelRef) error       { return nil }
func (m *gatingModelStore) IsLocked(context.Context, spi.ModelRef) (bool, error) {
	return true, nil
}
func (m *gatingModelStore) SetChangeLevel(context.Context, spi.ModelRef, spi.ChangeLevel) error {
	return nil
}
func (m *gatingModelStore) ExtendSchema(context.Context, spi.ModelRef, spi.SchemaDelta) error {
	return nil
}

var _ spi.ModelStore = (*gatingModelStore)(nil)

func notCreationDateContains2024() *predicate.GroupCondition {
	return &predicate.GroupCondition{Operator: "NOT", Conditions: []predicate.Condition{
		&predicate.LifecycleCondition{Field: "creationDate", OperatorType: "CONTAINS", Value: "2024"},
	}}
}

// TestDeleteConditionTypeCheck_NotWrappedTemporalTextOperator_NoSchema_Refused
// is the crux of this fix: a model with NO schema registered yet
// (deleteModelSchemaNode's ordinary (nil, nil) case) must still refuse
// NOT(creationDate CONTAINS "2024") — the exact shape a NOT arm inverts a
// permanent never-match guard into matching every entity, on the delete
// surface where that means every entity is removed.
func TestDeleteConditionTypeCheck_NotWrappedTemporalTextOperator_NoSchema_Refused(t *testing.T) {
	store := &gatingModelStore{desc: &spi.ModelDescriptor{Ref: spi.ModelRef{EntityName: "x", ModelVersion: "1"}}}
	ref := spi.ModelRef{EntityName: "x", ModelVersion: "1"}

	err := deleteConditionTypeCheck(context.Background(), store, ref, notCreationDateContains2024())
	if err == nil {
		t.Fatal("deleteConditionTypeCheck returned nil (accepted) for NOT(creationDate CONTAINS \"2024\") " +
			"against a schema-less model; want a refusal — this predicate must never reach " +
			"internal/match's temporal-meta guard unvalidated, or every entity gets deleted")
	}
	var appErr *common.AppError
	if !errors.As(err, &appErr) {
		t.Fatalf("got err %T (%v), want *common.AppError", err, err)
	}
	if appErr.Status != http.StatusBadRequest {
		t.Errorf("appErr.Status = %d, want %d", appErr.Status, http.StatusBadRequest)
	}
}

// TestDeleteConditionTypeCheck_RefusalMeansNothingIsSelected demonstrates
// the actual consequence in terms the coordinator asked for directly: a
// refused type check means planDeleteSelection never proceeds to
// spi.ConditionToFilter or match.Prepare at all, so no deleteSelectionPlan —
// and therefore no filter and no residual matcher that could select any
// row — is ever built for this condition. Contrast this with what
// match.Prepare alone would answer for the SAME condition if it were ever
// reached unvalidated (the pre-fix consequence, reproduced directly against
// match.Prepare rather than by reverting production code): every entity
// matches, because NOT inverts prepareLifecycle's permanent never-match
// guard.
func TestDeleteConditionTypeCheck_RefusalMeansNothingIsSelected(t *testing.T) {
	store := &gatingModelStore{desc: &spi.ModelDescriptor{Ref: spi.ModelRef{EntityName: "x", ModelVersion: "1"}}}
	ref := spi.ModelRef{EntityName: "x", ModelVersion: "1"}
	cond := notCreationDateContains2024()

	// With the fix: refused before any selection plan is built.
	if err := deleteConditionTypeCheck(context.Background(), store, ref, cond); err == nil {
		t.Fatal("expected deleteConditionTypeCheck to refuse this condition")
	}

	// The consequence this refusal exists to prevent, made concrete: if
	// this SAME unvalidated condition ever reached match.Prepare (as it
	// would have, pre-fix, once planDeleteSelection's translate-then-residual
	// fallback ran), it matches EVERY entity — arbitrary data, arbitrary
	// meta, all of them — not merely the ones the caller meant to select.
	prepared, err := match.Prepare(cond, nil)
	if err != nil {
		t.Fatalf("match.Prepare: %v", err)
	}
	entities := []struct {
		data []byte
		meta spi.EntityMeta
	}{
		{[]byte(`{}`), spi.EntityMeta{}},
		{[]byte(`{"anything":"at all"}`), spi.EntityMeta{State: "active"}},
		{[]byte(`{"nested":{"x":1}}`), spi.EntityMeta{State: "closed"}},
	}
	for i, e := range entities {
		if !prepared.Match(e.data, e.meta) {
			t.Errorf("entity %d: match.Prepare(NOT(creationDate CONTAINS \"2024\")).Match() = false, "+
				"want true — this is the delete-everything consequence deleteConditionTypeCheck's "+
				"refusal above prevents from ever being reached", i)
		}
	}
}

// TestDeleteConditionTypeCheck_NotWrappedTemporalTextOperator_UnreadableSchema_Refused
// is the sibling case: the schema READ itself fails (infra outage), but the
// condition is lifecycle-only so the read is gated off entirely — the
// validation call must still run and still refuse this predicate.
func TestDeleteConditionTypeCheck_NotWrappedTemporalTextOperator_UnreadableSchema_Refused(t *testing.T) {
	store := &gatingModelStore{failGet: true, getErr: context.DeadlineExceeded}
	ref := spi.ModelRef{EntityName: "x", ModelVersion: "1"}

	err := deleteConditionTypeCheck(context.Background(), store, ref, notCreationDateContains2024())
	if err == nil {
		t.Fatal("deleteConditionTypeCheck returned nil (accepted) for a lifecycle-only NOT(...) " +
			"condition when the schema read was never attempted; want a refusal")
	}
	if store.gets != 0 {
		t.Errorf("modelStore.Get was called %d time(s) for a lifecycle-only condition, want 0: "+
			"the model read must be gated on data-path need, and the validation call must never "+
			"be gated on the read's outcome", store.gets)
	}
}

// TestDeleteConditionTypeCheck_LifecycleOnly_NoModelRead confirms the
// legitimate case survives the fix: a VALID lifecycle-only condition is
// accepted, and — because it addresses no data path — the model is never
// read at all, matching workflow/engine.go's evaluateCriterion (Task 7),
// grouped_stats_service.go's ValidateConditionValueTypes(nil, ...) shape,
// and this task's identical fix to search.validateConditionTypes.
func TestDeleteConditionTypeCheck_LifecycleOnly_NoModelRead(t *testing.T) {
	store := &gatingModelStore{failGet: true, getErr: context.DeadlineExceeded} // would fail the request if ever called
	ref := spi.ModelRef{EntityName: "x", ModelVersion: "1"}

	cond := &predicate.LifecycleCondition{Field: "state", OperatorType: "EQUALS", Value: "active"}
	if err := deleteConditionTypeCheck(context.Background(), store, ref, cond); err != nil {
		t.Fatalf("deleteConditionTypeCheck rejected a valid lifecycle-only condition: %v", err)
	}
	if store.gets != 0 {
		t.Errorf("modelStore.Get was called %d time(s) for a lifecycle-only condition, want 0", store.gets)
	}
}

// TestDeleteConditionTypeCheck_DataPath_UnreadableSchema_StaysAnInfraFailure
// pins that the fix does not touch the existing dependency-failure
// precedence for a condition that DOES need the schema: an unreadable
// schema still fails a data-path condition as an infra error, not silently
// or as a 400.
func TestDeleteConditionTypeCheck_DataPath_UnreadableSchema_StaysAnInfraFailure(t *testing.T) {
	store := &gatingModelStore{failGet: true, getErr: context.DeadlineExceeded}
	ref := spi.ModelRef{EntityName: "x", ModelVersion: "1"}

	cond := &predicate.SimpleCondition{JsonPath: "$.price", OperatorType: "EQUALS", Value: float64(10)}
	err := deleteConditionTypeCheck(context.Background(), store, ref, cond)
	if err == nil {
		t.Fatal("deleteConditionTypeCheck accepted a data-path condition despite an unreadable schema")
	}
	var appErr *common.AppError
	if errors.As(err, &appErr) {
		t.Errorf("got *common.AppError %v, want a plain wrapped error (an infra failure, not a client 4xx)", appErr)
	}
	if store.gets != 1 {
		t.Errorf("modelStore.Get called %d time(s), want exactly 1 (the read this condition genuinely needs)", store.gets)
	}
}
