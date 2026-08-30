package entity

import (
	"math"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// TestBuildGroupKeyFromEntity_NumericSegmentIsNotAnIndex and
// TestExtractNumerics_NumericSegmentIsNotAnIndex pin path-grammar.md §3/§10's
// addressing rule on the grouping and aggregation surfaces: a bare hop named
// "0" is a field-name lookup, never an array-index shortcut, regardless of
// what shape the stored value turns out to be. gjson.GetBytes's own path
// syntax disagrees — it resolves an all-digit segment against an ARRAY
// receiver as a positional index — so a groupBy/aggregation field that went
// through gjson.GetBytes directly (bypassing spi.ParseFilterPath /
// spi.ResolvePath) saw "$.obj.0" over {"obj":["X","Y"]} as "X", diverging
// from spi.ResolvePath and both SQL backends (which return NULL/non-existent
// for the same shape). Both must resolve to absent/NaN.
func TestBuildGroupKeyFromEntity_NumericSegmentIsNotAnIndex(t *testing.T) {
	groups := []GroupExprValidated{{Path: "$.obj.0"}}
	e := &spi.Entity{Data: []byte(`{"obj":["X","Y"]}`)}
	rawVals, keys := buildGroupKeyFromEntity(groups, e)
	if rawVals[0] != nil {
		t.Fatalf("buildGroupKeyFromEntity(%q) rawVal = %v, want nil (absent, not array element 0)", groups[0].Path, rawVals[0])
	}
	if keys[0].Value != nil {
		t.Fatalf("buildGroupKeyFromEntity(%q) key.Value = %v, want nil", groups[0].Path, keys[0].Value)
	}
}

func TestExtractNumerics_NumericSegmentIsNotAnIndex(t *testing.T) {
	aggs := []AggregationExprValidated{{Op: AggSum, Field: "$.obj.0", Alias: "sum_obj_0"}}
	data := []byte(`{"obj":[1,2]}`)
	got := extractNumerics(aggs, data)
	if len(got) != 1 || !math.IsNaN(got[0]) {
		t.Fatalf("extractNumerics(%q) = %v, want [NaN] (absent, not array element 0)", aggs[0].Field, got)
	}
}
