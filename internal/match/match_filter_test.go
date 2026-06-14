package match_test

import (
	"encoding/json"
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/match"
)

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal failed: %v", err)
	}
	return b
}

func TestMatchFilter_EqString(t *testing.T) {
	data := mustJSON(t, map[string]any{"variantId": "v1"})
	f := spi.Filter{
		Op:     spi.FilterEq,
		Path:   "variantId",
		Source: spi.SourceData,
		Value:  "v1",
	}
	if !match.MatchFilter(f, data, spi.EntityMeta{}) {
		t.Fatalf("expected MatchFilter to be true for matching data")
	}
	f.Value = "v2"
	if match.MatchFilter(f, data, spi.EntityMeta{}) {
		t.Fatalf("expected MatchFilter to be false for non-matching data")
	}
}

func TestMatchFilter_EmptyFilterMatchesAll(t *testing.T) {
	data := mustJSON(t, map[string]any{"x": 1})
	if !match.MatchFilter(spi.Filter{}, data, spi.EntityMeta{}) {
		t.Fatalf("zero-value Filter should match all")
	}
}

func TestMatchFilter_StateEq(t *testing.T) {
	f := spi.Filter{
		Op:     spi.FilterEq,
		Path:   "state",
		Source: spi.SourceMeta,
		Value:  "available",
	}
	if !match.MatchFilter(f, nil, spi.EntityMeta{State: "available"}) {
		t.Fatalf("expected state match")
	}
	if match.MatchFilter(f, nil, spi.EntityMeta{State: "shipped"}) {
		t.Fatalf("expected state non-match")
	}
}

func TestMatchFilter_Ne(t *testing.T) {
	data := mustJSON(t, map[string]any{"variantId": "v1"})
	f := spi.Filter{
		Op:     spi.FilterNe,
		Path:   "variantId",
		Source: spi.SourceData,
		Value:  "v2",
	}
	if !match.MatchFilter(f, data, spi.EntityMeta{}) {
		t.Fatalf("expected Ne to be true for different value")
	}
	f.Value = "v1"
	if match.MatchFilter(f, data, spi.EntityMeta{}) {
		t.Fatalf("expected Ne to be false for same value")
	}
}

func TestMatchFilter_NumericOrdering(t *testing.T) {
	data := mustJSON(t, map[string]any{"qty": 42})
	cases := []struct {
		name string
		op   spi.FilterOp
		val  any
		want bool
	}{
		{"gt true", spi.FilterGt, 10, true},
		{"gt false", spi.FilterGt, 100, false},
		{"gte equal", spi.FilterGte, 42, true},
		{"lt true", spi.FilterLt, 100, true},
		{"lt false", spi.FilterLt, 10, false},
		{"lte equal", spi.FilterLte, 42, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := spi.Filter{Op: tc.op, Path: "qty", Source: spi.SourceData, Value: tc.val}
			if got := match.MatchFilter(f, data, spi.EntityMeta{}); got != tc.want {
				t.Fatalf("op=%s val=%v: got %v want %v", tc.op, tc.val, got, tc.want)
			}
		})
	}
}

func TestMatchFilter_IsNullAndNotNull(t *testing.T) {
	data := mustJSON(t, map[string]any{"a": "x"})

	missing := spi.Filter{Op: spi.FilterIsNull, Path: "b", Source: spi.SourceData}
	if !match.MatchFilter(missing, data, spi.EntityMeta{}) {
		t.Fatalf("expected IsNull true for missing field")
	}

	present := spi.Filter{Op: spi.FilterIsNull, Path: "a", Source: spi.SourceData}
	if match.MatchFilter(present, data, spi.EntityMeta{}) {
		t.Fatalf("expected IsNull false for present field")
	}

	notNull := spi.Filter{Op: spi.FilterNotNull, Path: "a", Source: spi.SourceData}
	if !match.MatchFilter(notNull, data, spi.EntityMeta{}) {
		t.Fatalf("expected NotNull true for present field")
	}
	missingNotNull := spi.Filter{Op: spi.FilterNotNull, Path: "b", Source: spi.SourceData}
	if match.MatchFilter(missingNotNull, data, spi.EntityMeta{}) {
		t.Fatalf("expected NotNull false for missing field")
	}
}

func TestMatchFilter_AndGroup(t *testing.T) {
	data := mustJSON(t, map[string]any{"variantId": "v1", "qty": 5})
	f := spi.Filter{
		Op: spi.FilterAnd,
		Children: []spi.Filter{
			{Op: spi.FilterEq, Path: "variantId", Source: spi.SourceData, Value: "v1"},
			{Op: spi.FilterGt, Path: "qty", Source: spi.SourceData, Value: 1},
		},
	}
	if !match.MatchFilter(f, data, spi.EntityMeta{}) {
		t.Fatalf("expected AND to be true when all children match")
	}

	f.Children[1].Value = 100
	if match.MatchFilter(f, data, spi.EntityMeta{}) {
		t.Fatalf("expected AND to be false when one child fails")
	}
}

func TestMatchFilter_OrGroup(t *testing.T) {
	data := mustJSON(t, map[string]any{"variantId": "v1"})
	f := spi.Filter{
		Op: spi.FilterOr,
		Children: []spi.Filter{
			{Op: spi.FilterEq, Path: "variantId", Source: spi.SourceData, Value: "vX"},
			{Op: spi.FilterEq, Path: "variantId", Source: spi.SourceData, Value: "v1"},
		},
	}
	if !match.MatchFilter(f, data, spi.EntityMeta{}) {
		t.Fatalf("expected OR to be true when one child matches")
	}

	f.Children[1].Value = "vY"
	if match.MatchFilter(f, data, spi.EntityMeta{}) {
		t.Fatalf("expected OR to be false when no children match")
	}
}

func TestMatchFilter_StringOps(t *testing.T) {
	data := mustJSON(t, map[string]any{"name": "Cyoda-Go"})
	cases := []struct {
		name string
		op   spi.FilterOp
		val  string
		want bool
	}{
		{"contains hit", spi.FilterContains, "oda", true},
		{"contains miss", spi.FilterContains, "zzz", false},
		{"starts hit", spi.FilterStartsWith, "Cy", true},
		{"starts miss", spi.FilterStartsWith, "Go", false},
		{"ends hit", spi.FilterEndsWith, "Go", true},
		{"ends miss", spi.FilterEndsWith, "Cy", false},
		{"like hit", spi.FilterLike, "Cy%Go", true},
		{"like underscore", spi.FilterLike, "Cyoda_Go", true},
		{"like miss", spi.FilterLike, "Zzz%", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := spi.Filter{Op: tc.op, Path: "name", Source: spi.SourceData, Value: tc.val}
			if got := match.MatchFilter(f, data, spi.EntityMeta{}); got != tc.want {
				t.Fatalf("op=%s val=%q: got %v want %v", tc.op, tc.val, got, tc.want)
			}
		})
	}
}

func TestMatchFilter_NestedAndOr(t *testing.T) {
	data := mustJSON(t, map[string]any{"variantId": "v1", "qty": 5, "color": "red"})
	// (variantId == v1) AND (qty > 100 OR color == "red")
	f := spi.Filter{
		Op: spi.FilterAnd,
		Children: []spi.Filter{
			{Op: spi.FilterEq, Path: "variantId", Source: spi.SourceData, Value: "v1"},
			{
				Op: spi.FilterOr,
				Children: []spi.Filter{
					{Op: spi.FilterGt, Path: "qty", Source: spi.SourceData, Value: 100},
					{Op: spi.FilterEq, Path: "color", Source: spi.SourceData, Value: "red"},
				},
			},
		},
	}
	if !match.MatchFilter(f, data, spi.EntityMeta{}) {
		t.Fatalf("expected nested AND/OR to match")
	}
}

func TestMatchFilter_MetaOtherFields(t *testing.T) {
	meta := spi.EntityMeta{
		ID:               "ent-1",
		State:            "available",
		Version:          7,
		CreationDate:     time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC),
		LastModifiedDate: time.Date(2024, 6, 1, 0, 0, 0, 0, time.UTC),
		ChangeType:       "UPDATED",
	}

	cases := []struct {
		name string
		path string
		val  any
		want bool
	}{
		{"entity_id match", "entity_id", "ent-1", true},
		{"entity_id miss", "entity_id", "ent-2", false},
		{"change_type match", "change_type", "UPDATED", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := spi.Filter{Op: spi.FilterEq, Path: tc.path, Source: spi.SourceMeta, Value: tc.val}
			if got := match.MatchFilter(f, nil, meta); got != tc.want {
				t.Fatalf("path=%s: got %v want %v", tc.path, got, tc.want)
			}
		})
	}
}

func TestMatchFilter_EmptyAndGroupIsTrue(t *testing.T) {
	// An empty AND is the identity element — tautology. This mirrors the
	// arrayToFilter "all positions are nil" branch in filter_translate.go.
	f := spi.Filter{Op: spi.FilterAnd}
	if !match.MatchFilter(f, nil, spi.EntityMeta{}) {
		t.Fatalf("expected empty AND to be true (tautology)")
	}
}

func TestMatchFilter_EmptyOrGroupIsFalse(t *testing.T) {
	// An empty OR is the identity element for OR — false.
	f := spi.Filter{Op: spi.FilterOr, Children: []spi.Filter{}}
	// Children explicit slice (len 0) avoids zero-value-Filter early-out.
	if match.MatchFilter(f, nil, spi.EntityMeta{}) {
		t.Fatalf("expected empty OR to be false")
	}
}
