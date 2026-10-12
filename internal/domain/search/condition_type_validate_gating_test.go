package search

import (
	"errors"
	"net/http"
	"testing"

	"github.com/cyoda-platform/cyoda-go-spi/predicate"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/domain/model/schema"
)

// condition_type_validate_gating_test.go pins that the validation sequence
// always runs its type check. A lifecycle-only condition carries no data path, so
// validateConditionPaths returns no fields map for it — and the
// model-independent half of the check must still refuse a string or pattern
// operator on a temporal meta field (creationDate/lastUpdateTime). A NOT
// wrapping such a leaf selects every entity the leaf does not match.

// noModelPaths is the path step of a condition that names no data path: no
// model is consulted and no fields map comes back.
func noModelPaths() (map[string]schema.FieldDescriptor, error) { return nil, nil }

func notCreationDateContains2024() *predicate.GroupCondition {
	return &predicate.GroupCondition{Operator: "NOT", Conditions: []predicate.Condition{
		&predicate.LifecycleCondition{Field: "creationDate", OperatorType: "CONTAINS", Value: "2024"},
	}}
}

func TestValidateConditionSequence_NotWrappedTemporalTextOperator_NoFields_Refused(t *testing.T) {
	_, err := validateConditionSequence(notCreationDateContains2024(), noModelPaths)
	var appErr *common.AppError
	if !errors.As(err, &appErr) {
		t.Fatal("the sequence accepted NOT(creationDate CONTAINS \"2024\") with no fields map; want a refusal")
	}
	if appErr.Status != http.StatusBadRequest || appErr.Code != common.ErrCodeConditionTypeMismatch {
		t.Errorf("got %d %s, want 400 %s", appErr.Status, appErr.Code, common.ErrCodeConditionTypeMismatch)
	}
}

func TestValidateConditionSequence_LifecycleOnly_Accepted(t *testing.T) {
	cond := &predicate.LifecycleCondition{Field: "state", OperatorType: "EQUALS", Value: "active"}
	if _, err := validateConditionSequence(cond, noModelPaths); err != nil {
		t.Fatalf("the sequence rejected a valid lifecycle-only condition: %v", err)
	}
}
