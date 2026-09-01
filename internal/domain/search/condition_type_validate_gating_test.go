package search

import (
	"context"
	"net/http"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go-spi/predicate"
)

// condition_type_validate_gating_test.go pins the fix to validateConditionTypes:
// the model READ is gated on whether cond addresses a data path, but the
// VALIDATION CALL (ValidateConditionValueTypes) is never gated on whether
// that read was attempted, succeeded, or found a schema.
//
// Before the fix, validateConditionTypes returned nil — "validation
// passed" — WITHOUT ever calling ValidateConditionValueTypes in two cases:
// an unreadable schema paired with a lifecycle-only condition, and a model
// that loaded cleanly but carries no schema yet (loadModelNode's ordinary
// (nil, nil) "no schema bound" return). Both silently skipped
// validateLifecycleType, the one check that refuses a text or pattern
// operator on a temporal meta field (creationDate/lastUpdateTime). Left
// unrejected, that predicate reaches internal/match's deliberate
// temporal-meta never-match guard unvalidated, and a NOT wrapping it
// inverts that guard into matching every entity.
//
// These tests construct the dangerous GroupCondition{Operator:"NOT"} shape
// directly rather than going through Search/ValidateCondition: ValidateCondition
// (operators.go) already rejects "NOT" unconditionally today (Task 12 of this
// plan — accepting NOT structurally — has not landed), so a real end-to-end
// call through Search would be refused there first, for an unrelated reason,
// and would pass whether or not this file's fix exists. Calling
// validateConditionTypes directly is what actually exercises — and would
// have caught — this specific gating regression.

// gatingModelStore is a minimal spi.ModelStore double for these tests. Get
// returns a canned descriptor (or error) and records how many times it was
// called, so a test can assert "no model read happened at all" — not merely
// "the read that happened was harmless".
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

// TestValidateConditionTypes_NotWrappedTemporalTextOperator_NoSchema_Refused
// is the entity-of-this-fix test: a model with NO schema registered yet
// (loadModelNode's ordinary (nil, nil) case) must still refuse
// NOT(creationDate CONTAINS "2024") — the exact shape a NOT arm inverts a
// permanent never-match guard into matching everything.
func TestValidateConditionTypes_NotWrappedTemporalTextOperator_NoSchema_Refused(t *testing.T) {
	store := &gatingModelStore{desc: &spi.ModelDescriptor{Ref: spi.ModelRef{EntityName: "x", ModelVersion: "1"}}}
	svc := &SearchService{}

	appErr := svc.validateConditionTypes(context.Background(), store, spi.ModelRef{EntityName: "x", ModelVersion: "1"}, notCreationDateContains2024())
	if appErr == nil {
		t.Fatal("validateConditionTypes returned nil (accepted) for NOT(creationDate CONTAINS \"2024\") " +
			"against a schema-less model; want a refusal — this predicate must never reach " +
			"internal/match's temporal-meta guard unvalidated")
	}
	if appErr.Status != http.StatusBadRequest {
		t.Errorf("appErr.Status = %d, want %d", appErr.Status, http.StatusBadRequest)
	}
}

// TestValidateConditionTypes_NotWrappedTemporalTextOperator_UnreadableSchema_Refused
// is the sibling case: the schema READ itself fails (infra outage), but the
// condition is lifecycle-only so the read is gated off entirely — the
// validation call must still run and still refuse this predicate, not treat
// "we didn't even try to read" as "nothing to validate".
func TestValidateConditionTypes_NotWrappedTemporalTextOperator_UnreadableSchema_Refused(t *testing.T) {
	store := &gatingModelStore{failGet: true, getErr: context.DeadlineExceeded}
	svc := &SearchService{}

	appErr := svc.validateConditionTypes(context.Background(), store, spi.ModelRef{EntityName: "x", ModelVersion: "1"}, notCreationDateContains2024())
	if appErr == nil {
		t.Fatal("validateConditionTypes returned nil (accepted) for a lifecycle-only NOT(...) " +
			"condition when the schema read was never attempted; want a refusal")
	}
	if store.gets != 0 {
		t.Errorf("modelStore.Get was called %d time(s) for a lifecycle-only condition, want 0: "+
			"the model read must be gated on data-path need, exactly like the validation call "+
			"must never be gated on the read's outcome", store.gets)
	}
}

// TestValidateConditionTypes_LifecycleOnly_NoModelRead confirms the
// legitimate case survives the fix: a VALID lifecycle-only condition is
// accepted, and — because it addresses no data path — the model is never
// read at all, matching workflow/engine.go's evaluateCriterion (Task 7) and
// grouped_stats_service.go's ValidateConditionValueTypes(nil, ...) shape.
func TestValidateConditionTypes_LifecycleOnly_NoModelRead(t *testing.T) {
	store := &gatingModelStore{failGet: true, getErr: context.DeadlineExceeded} // would fail the request if ever called
	svc := &SearchService{}

	cond := &predicate.LifecycleCondition{Field: "state", OperatorType: "EQUALS", Value: "active"}
	appErr := svc.validateConditionTypes(context.Background(), store, spi.ModelRef{EntityName: "x", ModelVersion: "1"}, cond)
	if appErr != nil {
		t.Fatalf("validateConditionTypes rejected a valid lifecycle-only condition: %v", appErr)
	}
	if store.gets != 0 {
		t.Errorf("modelStore.Get was called %d time(s) for a lifecycle-only condition, want 0", store.gets)
	}
}

// TestValidateConditionTypes_DataPath_UnreadableSchema_StaysAnInfraFailure
// pins that the fix does not touch the existing, separately-tested
// dependency-failure precedence for a condition that DOES need the schema
// (TestSearch_SchemaLoadFails_FailsClosedInsteadOfAnswering, service_test.go
// package): an unreadable schema still fails a data-path condition as a
// dependency failure, not a 400.
func TestValidateConditionTypes_DataPath_UnreadableSchema_StaysAnInfraFailure(t *testing.T) {
	store := &gatingModelStore{failGet: true, getErr: context.DeadlineExceeded}
	svc := &SearchService{}

	cond := &predicate.SimpleCondition{JsonPath: "$.price", OperatorType: "EQUALS", Value: float64(10)}
	appErr := svc.validateConditionTypes(context.Background(), store, spi.ModelRef{EntityName: "x", ModelVersion: "1"}, cond)
	if appErr == nil {
		t.Fatal("validateConditionTypes accepted a data-path condition despite an unreadable schema")
	}
	if appErr.Status < 500 {
		t.Errorf("appErr.Status = %d, want 5xx: an unreadable schema is a dependency failure", appErr.Status)
	}
	if store.gets != 1 {
		t.Errorf("modelStore.Get called %d time(s), want exactly 1 (the read this condition genuinely needs)", store.gets)
	}
}
