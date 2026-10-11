package search

import (
	"net/http"
	"testing"

	"github.com/cyoda-platform/cyoda-go-spi/predicate"
	"github.com/cyoda-platform/cyoda-go/internal/common"
)

// condition_type_validate_gating_test.go pins that validateConditionTypes
// always runs its check. A lifecycle-only condition carries no data path, so
// validateConditionPaths returns no fields map for it — and the
// model-independent half of the check must still refuse a string or pattern
// operator on a temporal meta field (creationDate/lastUpdateTime). A NOT
// wrapping such a leaf selects every entity the leaf does not match.

func notCreationDateContains2024() *predicate.GroupCondition {
	return &predicate.GroupCondition{Operator: "NOT", Conditions: []predicate.Condition{
		&predicate.LifecycleCondition{Field: "creationDate", OperatorType: "CONTAINS", Value: "2024"},
	}}
}

func TestValidateConditionTypes_NotWrappedTemporalTextOperator_NoFields_Refused(t *testing.T) {
	svc := &SearchService{}
	appErr := svc.validateConditionTypes(nil, notCreationDateContains2024())
	if appErr == nil {
		t.Fatal("validateConditionTypes accepted NOT(creationDate CONTAINS \"2024\") with no fields map; want a refusal")
	}
	if appErr.Status != http.StatusBadRequest || appErr.Code != common.ErrCodeConditionTypeMismatch {
		t.Errorf("got %d %s, want 400 %s", appErr.Status, appErr.Code, common.ErrCodeConditionTypeMismatch)
	}
}

func TestValidateConditionTypes_LifecycleOnly_Accepted(t *testing.T) {
	svc := &SearchService{}
	cond := &predicate.LifecycleCondition{Field: "state", OperatorType: "EQUALS", Value: "active"}
	if appErr := svc.validateConditionTypes(nil, cond); appErr != nil {
		t.Fatalf("validateConditionTypes rejected a valid lifecycle-only condition: %v", appErr)
	}
}
