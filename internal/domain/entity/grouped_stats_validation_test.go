package entity_test

import (
	"testing"
	"time"

	"github.com/cyoda-platform/cyoda-go/internal/domain/entity"
)

func TestValidateGroupedStatsRequest(t *testing.T) {
	intPtr := func(i int) *int { return &i }
	timePtr := func(s string) *time.Time {
		t, _ := time.Parse(time.RFC3339, s)
		return &t
	}
	cases := []struct {
		name       string
		in         entity.GroupedStatsRequest
		maxBuckets int
		wantCode   string // "" = no error
	}{
		{"missing groupBy", entity.GroupedStatsRequest{}, 10000, "MISSING_GROUP_BY"},
		{"empty entry", entity.GroupedStatsRequest{GroupBy: []string{""}}, 10000, "INVALID_GROUP_BY_PATH"},
		{"array projection", entity.GroupedStatsRequest{GroupBy: []string{"$.items[*]"}}, 10000, "INVALID_GROUP_BY_PATH"},
		{"positional index", entity.GroupedStatsRequest{GroupBy: []string{"$.items[0]"}}, 10000, "INVALID_GROUP_BY_PATH"},
		{"bracket scalar accepted",
			entity.GroupedStatsRequest{GroupBy: []string{"$.['variantId']"}}, 10000, ""},
		{"duplicate groupBy",
			entity.GroupedStatsRequest{GroupBy: []string{"state", "state"}}, 10000, "DUPLICATE_GROUP_BY"},
		{"unknown agg op",
			entity.GroupedStatsRequest{
				GroupBy: []string{"state"},
				Aggregations: []entity.AggregationExprWire{
					{Op: "median", Field: "$.x"},
				}}, 10000, "INVALID_AGGREGATION_OP"},
		{"agg field array projection",
			entity.GroupedStatsRequest{
				GroupBy: []string{"state"},
				Aggregations: []entity.AggregationExprWire{
					{Op: "sum", Field: "$.x[*]"},
				}}, 10000, "INVALID_AGGREGATION_FIELD"},
		{"distinct pair colliding alias",
			entity.GroupedStatsRequest{
				GroupBy: []string{"state"},
				Aggregations: []entity.AggregationExprWire{
					{Op: "sum", Field: "$.x", As: "v"},
					{Op: "avg", Field: "$.y", As: "v"},
				}}, 10000, "DUPLICATE_AGGREGATION_ALIAS"},
		{"identical pair silently deduped",
			entity.GroupedStatsRequest{
				GroupBy: []string{"state"},
				Aggregations: []entity.AggregationExprWire{
					{Op: "sum", Field: "$.x"},
					{Op: "sum", Field: "$.x"},
				}}, 10000, ""},
		{"limit > ceiling",
			entity.GroupedStatsRequest{
				GroupBy: []string{"state"},
				Limit:   intPtr(20000),
			}, 10000, "INVALID_LIMIT"},
		{"limit non-positive",
			entity.GroupedStatsRequest{
				GroupBy: []string{"state"},
				Limit:   intPtr(0),
			}, 10000, "INVALID_LIMIT"},
		{"happy path", entity.GroupedStatsRequest{
			GroupBy:     []string{"state", "$.variantId"},
			PointInTime: timePtr("2026-06-14T12:00:00Z"),
			Limit:       intPtr(50),
		}, 10000, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := entity.ValidateGroupedStatsRequest(tc.in, tc.maxBuckets)
			if tc.wantCode == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected error %s, got nil", tc.wantCode)
			}
			ve, ok := err.(*entity.GroupedStatsValidationError)
			if !ok {
				t.Fatalf("expected GroupedStatsValidationError, got %T: %v", err, err)
			}
			if ve.Code != tc.wantCode {
				t.Fatalf("got code %s, want %s", ve.Code, tc.wantCode)
			}
		})
	}
}
