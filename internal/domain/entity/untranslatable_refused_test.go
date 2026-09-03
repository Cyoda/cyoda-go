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

// TestQueryGroupedStats_FunctionCondition_Is400 pins the BOUNDARY that stands
// in front of grouped stats' translation step, not the refusal arm behind it.
//
// Be precise about what this can and cannot show. Grouped statistics used to
// answer an untranslatable condition by passing the store a zero-value filter
// and re-matching every yielded entity in this process — a whole-model scan,
// the shape direct search deleted. That branch is gone and the entry point
// refuses instead (grouped_stats_service.go). But the refusal arm is NOT
// reachable through this entry point: QueryGroupedStats takes raw JSON, so
// every condition it sees is one of predicate.ParseCondition's five clause
// types, and search.ValidateCondition rejects the only one of those that
// fails translation — a function clause — before the translator runs. So this
// test passes with the refusal arm reverted, and would be dishonest if it
// claimed otherwise.
//
// What it does pin is worth having: a function clause is 400
// INVALID_CONDITION and the store is never reached, so the refusal happens
// before any scan rather than after one. The unreachability of the arm itself
// is established by enumeration in
// TestDeleteAndGroupedStats_ClearingImpliesTranslates, which is the right
// shape of proof for a property no input can exercise. Search's equivalent
// arm HAS a direct test only because its service method takes a
// predicate.Condition and a test can hand it a caller-built type; grouped
// stats and conditional delete take bytes, and cannot.
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
