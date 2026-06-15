package entity_test

import (
	"encoding/json"
	"testing"

	"github.com/cyoda-platform/cyoda-go/internal/domain/entity"
)

func TestGroupedStatsRequest_DecodeBasic(t *testing.T) {
	raw := `{
        "groupBy":      ["$.variantId", "state"],
        "limit":        100
    }`
	var r entity.GroupedStatsRequest
	if err := json.Unmarshal([]byte(raw), &r); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(r.GroupBy) != 2 {
		t.Fatalf("groupBy len=%d, want 2", len(r.GroupBy))
	}
	if r.Limit == nil || *r.Limit != 100 {
		t.Fatalf("limit not parsed")
	}
}

func TestGroupedStatsBucket_EncodeOmitsAggregationsWhenEmpty(t *testing.T) {
	b := entity.GroupedStatsBucket{
		GroupKey: []entity.GroupKeyEntryWire{
			{Path: "$.variantId", Value: "v1"},
		},
		Count: 42,
	}
	raw, err := json.Marshal(b)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if got := string(raw); containsSub(got, "aggregations") {
		t.Fatalf("aggregations should be omitted: %s", got)
	}
}

func containsSub(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
