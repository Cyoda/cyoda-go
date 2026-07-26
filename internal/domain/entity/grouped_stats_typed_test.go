package entity_test

import (
	"context"
	"encoding/json"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/domain/entity"
	"github.com/cyoda-platform/cyoda-go/internal/domain/model/schema"
)

// TestQueryGroupedStats_ResidualTypeDirectedWithFields proves the Task-8 wiring
// on the grouped-stats streaming residual (tallyStreaming): the residual match
// path is reached only for a non-pushable condition, so we wrap a plain-leaf
// numeric comparison in an OR group alongside a wildcard leaf (the wildcard
// makes ConditionToFilter reject the whole group, forcing the per-entity
// match.Match residual). A non-nil fields map declaring `$.age` Integer makes the
// GREATER_THAN comparison type-directed — only the entity whose age exceeds the
// operand is tallied. The nil-fields companion (untyped leaf → comparison
// degrades to non-match) yields no buckets, proving the loaded fields — not
// lexical comparison — drove the residual closure.
func TestQueryGroupedStats_ResidualTypeDirectedWithFields(t *testing.T) {
	// The wildcard child makes the group non-pushdownable, forcing the residual;
	// the plain-leaf `$.age` child is what type-directs inside the residual.
	cond := json.RawMessage(`{
		"type": "group",
		"operator": "OR",
		"conditions": [
			{"type": "simple", "jsonPath": "$.age", "operatorType": "GREATER_THAN", "value": 5},
			{"type": "simple", "jsonPath": "$.tags[*]", "operatorType": "EQUALS", "value": "zzz"}
		]
	}`)
	rows := []*spi.Entity{
		{Meta: spi.EntityMeta{State: "x"}, Data: []byte(`{"age":30,"tags":["a"]}`)},
		{Meta: spi.EntityMeta{State: "x"}, Data: []byte(`{"age":3,"tags":["b"]}`)},
	}
	req := &entity.ValidatedGroupedStatsRequest{
		GroupBy:   []entity.GroupExprValidated{{IsState: true}},
		Condition: []byte(cond),
	}
	fields := map[string]schema.FieldDescriptor{
		"$.age": {Path: "$.age", Types: []spi.DataType{spi.Integer}},
	}

	svc := entity.NewGroupedStatsService(10000)

	// Typed: age 30 > 5 matches (e1), age 3 > 5 does not and no tag == "zzz"
	// (e2) → one bucket, count 1.
	buckets, err := svc.QueryGroupedStats(context.Background(), &fakeIterable{entities: rows}, spi.ModelRef{}, fields, req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(buckets) != 1 || buckets[0].Count != 1 {
		t.Fatalf("typed residual: buckets = %+v, want one bucket count=1", buckets)
	}

	// Untyped control: identical request with nil fields → comparison degrades
	// to non-match for both entities → no buckets.
	buckets, err = svc.QueryGroupedStats(context.Background(), &fakeIterable{entities: rows}, spi.ModelRef{}, nil, req)
	if err != nil {
		t.Fatalf("unexpected error (nil fields): %v", err)
	}
	if len(buckets) != 0 {
		t.Fatalf("untyped residual must degrade to non-match: buckets = %+v, want none", buckets)
	}
}

// TestQueryGroupedStats_FieldsFeedConditionToFilter proves the other half of the
// plumbing: on the pushable branch the fields map is threaded into
// ConditionToFilter, which stamps the leaf's declared model types onto the
// pushed spi.Filter. The streaming fixture records the filter it received, so a
// stamped Declared == [Integer] is the evidence the fields reached
// ConditionToFilter (rather than a hardcoded nil).
func TestQueryGroupedStats_FieldsFeedConditionToFilter(t *testing.T) {
	cond := json.RawMessage(`{
		"type": "simple",
		"jsonPath": "$.age",
		"operatorType": "EQUALS",
		"value": 30
	}`)
	rows := []*spi.Entity{
		{Meta: spi.EntityMeta{State: "x"}, Data: []byte(`{"age":30}`)},
	}
	req := &entity.ValidatedGroupedStatsRequest{
		GroupBy:   []entity.GroupExprValidated{{IsState: true}},
		Condition: []byte(cond),
	}
	fields := map[string]schema.FieldDescriptor{
		"$.age": {Path: "$.age", Types: []spi.DataType{spi.Integer}},
	}

	iter := &fakeIterable{entities: rows}
	svc := entity.NewGroupedStatsService(10000)
	if _, err := svc.QueryGroupedStats(context.Background(), iter, spi.ModelRef{}, fields, req); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// The condition is pushable, so the stamped Filter must have been passed to
	// Iterate; its Declared field carries the model type from `fields`.
	if iter.lastFlt.Op == "" {
		t.Fatal("expected a pushable filter passed to Iterate, got zero-value")
	}
	if len(iter.lastFlt.Declared) != 1 || iter.lastFlt.Declared[0] != spi.Integer {
		t.Fatalf("pushed filter Declared = %v, want [Integer] (fields threaded into ConditionToFilter)", iter.lastFlt.Declared)
	}
}
