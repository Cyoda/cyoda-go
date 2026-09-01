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

// TestQueryGroupedStats_BareLeafField_UnevaluableLeaf_MapsTo400 is the Gate-6
// sibling fix pinned alongside Task 5's main classifier work: grouped-stats'
// streaming-tally fallback (queryGroupedStatsInner -> tallyStreaming) drives a
// store's spi.Iterable.Iterate with a translated pushdown Filter exactly like
// search.Service.Search's own pushdown branch does, and a real backend's
// Iterate can refuse that Filter with spi.ErrUnevaluableLeaf for the same
// reason (a comparison operand against a field with zero declared types — a
// real, spec-anticipated bare {"kind":"LEAF"} schema shape; see
// search_test's classify_store_query_error_test.go for the full rationale).
// classifyGroupedStatsError had no case for it before this task, so it fell
// through to the generic 500 SERVER_ERROR — the identical defect
// SearchService.Search's ClassifyStoreQueryError closes for the search
// surface, just unaddressed on the grouped-stats surface. fakeIterable
// genuinely applies the Filter via spi.Prepare(flt).Match (see its own doc
// comment above), so this is a real (non-stubbed) propagation through the
// same kernel a production backend uses — no aggregator capability is given,
// so queryGroupedStatsInner has no choice but the streaming path.
func TestQueryGroupedStats_BareLeafField_UnevaluableLeaf_MapsTo400(t *testing.T) {
	svc := entity.NewGroupedStatsService(10000)
	iter := &fakeIterable{entities: []*spi.Entity{
		{Meta: spi.EntityMeta{State: "available"}, Data: []byte(`{"score":5}`)},
	}}
	fields := map[string]schema.FieldDescriptor{"$.score": {}}
	cond, err := json.Marshal(map[string]any{
		"type":         "simple",
		"jsonPath":     "$.score",
		"operatorType": "EQUALS",
		"value":        5,
	})
	if err != nil {
		t.Fatalf("marshal condition: %v", err)
	}
	req := &entity.ValidatedGroupedStatsRequest{
		GroupBy:   []entity.GroupExprValidated{{IsState: true}},
		Condition: cond,
	}

	_, gsErr := svc.QueryGroupedStats(context.Background(), iter, spi.ModelRef{}, fields, req)

	var appErr *common.AppError
	if !errors.As(gsErr, &appErr) {
		t.Fatalf("want *common.AppError, got %T: %v", gsErr, gsErr)
	}
	if appErr.Status != http.StatusBadRequest || appErr.Code != common.ErrCodeInvalidCondition {
		t.Errorf("got %d/%q, want 400/%s", appErr.Status, appErr.Code, common.ErrCodeInvalidCondition)
	}
	if !errors.Is(gsErr, spi.ErrUnevaluableLeaf) {
		t.Errorf("errors.Is(err, spi.ErrUnevaluableLeaf) = false, err = %v", gsErr)
	}
}
