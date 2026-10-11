package entity

import (
	"context"
	"errors"
	"net/http"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go-spi/predicate"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/domain/model/schema"
	"github.com/cyoda-platform/cyoda-go/internal/domain/search"
	"github.com/cyoda-platform/cyoda-go/internal/match"
)

// delete_condition_type_gating_test.go pins conditional delete's
// type-soundness check: it runs against the fields map the condition's paths
// were validated against, and it always runs — with no fields at all, its
// model-independent half still refuses a string or pattern operator on a
// temporal meta field (creationDate/lastUpdateTime). A NOT wrapping such a
// leaf selects every entity the leaf does not match — on conditional delete,
// a mass delete.

func notCreationDateContains2024() *predicate.GroupCondition {
	return &predicate.GroupCondition{Operator: "NOT", Conditions: []predicate.Condition{
		&predicate.LifecycleCondition{Field: "creationDate", OperatorType: "CONTAINS", Value: "2024"},
	}}
}

// TestDeleteConditionTypeCheck_NotWrappedTemporalTextOperator_NoFields_Refused:
// a lifecycle-only condition carries no data path, so no fields map exists
// for it — and NOT(creationDate CONTAINS "2024") must still be refused.
func TestDeleteConditionTypeCheck_NotWrappedTemporalTextOperator_NoFields_Refused(t *testing.T) {
	err := deleteConditionTypeCheck(nil, notCreationDateContains2024())
	var appErr *common.AppError
	if !errors.As(err, &appErr) {
		t.Fatalf("got err %T (%v), want a *common.AppError refusal", err, err)
	}
	if appErr.Status != http.StatusBadRequest || appErr.Code != common.ErrCodeConditionTypeMismatch {
		t.Errorf("got %d %s, want 400 %s", appErr.Status, appErr.Code, common.ErrCodeConditionTypeMismatch)
	}
}

// TestDeleteConditionTypeCheck_RefusalMeansNothingIsSelected demonstrates
// the consequence the refusal prevents: match.Prepare, reached unvalidated
// with the same condition, matches every entity — none of them holds a
// creationDate the inner leaf could match, and NOT selects them all.
func TestDeleteConditionTypeCheck_RefusalMeansNothingIsSelected(t *testing.T) {
	cond := notCreationDateContains2024()
	if err := deleteConditionTypeCheck(nil, cond); err == nil {
		t.Fatal("expected deleteConditionTypeCheck to refuse this condition")
	}

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
				"want true — this is the mass delete the refusal above prevents", i)
		}
	}
}

// TestDeleteConditionTypeCheck_LifecycleOnly_Accepted: a valid lifecycle-only
// condition passes with no fields map.
func TestDeleteConditionTypeCheck_LifecycleOnly_Accepted(t *testing.T) {
	cond := &predicate.LifecycleCondition{Field: "state", OperatorType: "EQUALS", Value: "active"}
	if err := deleteConditionTypeCheck(nil, cond); err != nil {
		t.Fatalf("deleteConditionTypeCheck rejected a valid lifecycle-only condition: %v", err)
	}
}

// TestPlanDeleteSelection_DataPath_UnreadableSchema_IsAnInfraFailure: a
// condition with a data path needs the schema; when it cannot be read, the
// delete fails as an infrastructure error, not silently and not as a 4xx.
func TestPlanDeleteSelection_DataPath_UnreadableSchema_IsAnInfraFailure(t *testing.T) {
	h := &Handler{}
	ref := spi.ModelRef{EntityName: "x", ModelVersion: "1"}
	cond := &predicate.SimpleCondition{JsonPath: "$.price", OperatorType: "EQUALS", Value: float64(10)}

	_, err := h.planDeleteSelection(context.Background(), &failingModelStore{refreshingStore: &refreshingStore{}, getErr: context.DeadlineExceeded}, ref, cond)
	if err == nil {
		t.Fatal("planDeleteSelection accepted a data-path condition despite an unreadable schema")
	}
	var appErr *common.AppError
	if errors.As(err, &appErr) {
		t.Errorf("got *common.AppError %v, want a plain wrapped error (an infra failure, not a client 4xx)", appErr)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("got %v, want the store's error to stay reachable", err)
	}
}

// TestPlanDeleteSelection_PathAddedByAPeer_OperatorTypeChecked: after the
// bounded refresh reveals a numeric field a peer just added, the type check
// sees it, so NOT_CONTAINS on it is refused rather than deleting every entity
// holding a number there.
func TestPlanDeleteSelection_PathAddedByAPeer_OperatorTypeChecked(t *testing.T) {
	h := &Handler{}
	ref := spi.ModelRef{EntityName: "E", ModelVersion: "1"}
	stale := buildDescriptorWithFields(t, ref, "a")
	freshNode := schema.NewObjectNode()
	freshNode.SetChild("a", schema.NewLeafNode(schema.String))
	freshNode.SetChild("n", schema.NewLeafNode(schema.Integer))
	raw, err := schema.Marshal(freshNode)
	if err != nil {
		t.Fatalf("schema.Marshal: %v", err)
	}
	fresh := &spi.ModelDescriptor{Ref: ref, State: spi.ModelLocked, Schema: raw}
	ms := &refreshingStore{
		getQueue:     []*spi.ModelDescriptor{stale},
		refreshQueue: []*spi.ModelDescriptor{fresh},
	}

	cond := &predicate.SimpleCondition{JsonPath: "$.n", OperatorType: "NOT_CONTAINS", Value: "7"}
	_, planErr := h.planDeleteSelection(context.Background(), ms, ref, cond)
	if !errors.Is(planErr, search.ErrConditionTypeMismatch) {
		var appErr *common.AppError
		if !errors.As(planErr, &appErr) || appErr.Code != common.ErrCodeConditionTypeMismatch {
			t.Fatalf("got %v, want 400 %s", planErr, common.ErrCodeConditionTypeMismatch)
		}
	}
}
