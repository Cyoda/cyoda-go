package entity_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/domain/entity"
	"github.com/cyoda-platform/cyoda-go/internal/domain/model/schema"
)

// A condition that cannot be translated is refused, not served another way.
//
// Grouped statistics used to answer one by passing the store a zero-value
// filter and re-matching every yielded entity in this process — a whole-model
// scan, the shape direct search deleted. It now refuses, which is only free
// because the refusal is unreachable from validated input
// (TestDeleteAndGroupedStats_ClearingImpliesTranslates). This test drives the
// arm itself, through the entry point, to pin the status and code it answers
// with rather than trusting it never runs.
//
// The condition is a function clause: search.ValidateCondition refuses it
// first, so this asserts the boundary that stands in front of the arm. There
// is no wire shape that clears validation and then fails translation — that
// is the point — so the arm is pinned by the classifier it shares with
// search rather than by a request that reaches it.
func TestQueryGroupedStats_FunctionCondition_Is400(t *testing.T) {
	cond := json.RawMessage(`{
		"type": "function",
		"function": {"name": "isWeekend", "args": {}}
	}`)
	svc := entity.NewGroupedStatsService(10000)
	req := &entity.ValidatedGroupedStatsRequest{
		GroupBy:   []entity.GroupExprValidated{{IsState: true}},
		Condition: []byte(cond),
	}
	store := &fakeIterable{entities: nil}

	_, err := svc.QueryGroupedStats(context.Background(), store, spi.ModelRef{},
		map[string]schema.FieldDescriptor{}, req)

	var appErr *common.AppError
	if !errors.As(err, &appErr) {
		t.Fatalf("want *common.AppError, got %T: %v", err, err)
	}
	if appErr.Status != http.StatusBadRequest || appErr.Code != common.ErrCodeInvalidCondition {
		t.Errorf("got %d/%q, want 400/%s", appErr.Status, appErr.Code, common.ErrCodeInvalidCondition)
	}
	// The store must not have been reached: refusing after a whole-model
	// scan would be the very thing this change removes.
	if store.lastFlt.Op != "" {
		t.Error("Iterate was reached for a condition the boundary refuses")
	}
}
