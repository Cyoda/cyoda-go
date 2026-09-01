package entity_test

import (
	"context"
	"encoding/json"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/domain/entity"
	"github.com/cyoda-platform/cyoda-go/internal/domain/model/schema"
)

// TestQueryGroupedStats_PushdownTypeDirectedWithFields proves the fields map
// threaded into the grouped-stats pushdown (spi.ConditionToFilter) makes a
// comparison type-directed the same way it does on the residual path: we
// wrap a plain-leaf numeric comparison in an OR group alongside a wildcard
// leaf. A non-nil fields map declaring `$.age` Integer makes the
// GREATER_THAN comparison type-directed — only the entity whose age exceeds
// the operand is tallied.
//
// The second child ($.tags[*] CONTAINS "zzz") is deliberately a STRING op,
// declaration-independent regardless of what fields declares (see
// TestApplyOperator_StringOpsAreDeclarationIndependent in internal/match) —
// so whether the whole OR filter can be prepared at all depends only on
// $.age's typing, isolating exactly the property this test is about.
//
// The nil-fields companion is no longer a graceful "comparison degrades to
// non-match" — the hardened kernel (spi.Prepare) now REJECTS a Filter
// carrying an unevaluable leaf outright, rather than partially evaluating an
// OR/AND with a sibling it cannot type: an OR that silently dropped an
// unevaluable leaf could answer true off a sibling and mask what a full,
// fail-closed evaluation would have said. So with nil fields, $.age's
// GREATER_THAN leaf has no declared type and the WHOLE query now fails
// closed with an error, proving the loaded fields — not lexical comparison —
// are what make the query evaluable at all, not merely what make it typed.
//
// This used to be named …ResidualTypeDirectedWithFields and exercise the
// per-entity match.Prepare residual specifically: the wildcard child used to
// make ConditionToFilter reject the whole group (non-pushdownable),
// forcing tallyStreaming's residual guard. That premise is dead —
// spi.ConditionToFilter now pushes a wildcard array path down like any
// other (path-grammar.md §2/§8), so this whole OR group is pushable and the
// SAME type-directed comparison now runs inside the pushed-down Filter
// instead — see fakeIterable's doc comment for why that distinction is
// visible to this test at all (a store that doesn't enforce the filter it's
// handed can't tell the two mechanisms apart, and used to mask exactly this
// kind of premise rot).
func TestQueryGroupedStats_PushdownTypeDirectedWithFields(t *testing.T) {
	// The wildcard child no longer forces a residual — spi.ConditionToFilter
	// pushes it down like any other path — but $.age is still what
	// type-directs the match, now inside the pushed-down Filter.
	cond := json.RawMessage(`{
		"type": "group",
		"operator": "OR",
		"conditions": [
			{"type": "simple", "jsonPath": "$.age", "operatorType": "GREATER_THAN", "value": 5},
			{"type": "simple", "jsonPath": "$.tags[*]", "operatorType": "CONTAINS", "value": "zzz"}
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

	// Typed: age 30 > 5 matches (e1), age 3 > 5 does not and no tag contains
	// "zzz" (e2) → one bucket, count 1.
	buckets, err := svc.QueryGroupedStats(context.Background(), &fakeIterable{entities: rows}, spi.ModelRef{}, fields, req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(buckets) != 1 || buckets[0].Count != 1 {
		t.Fatalf("typed pushdown: buckets = %+v, want one bucket count=1", buckets)
	}

	// Untyped control: identical request with nil fields → $.age's
	// comparison leaf has no declared type and cannot be evaluated, so the
	// whole query fails closed rather than silently returning an
	// under-matched result.
	_, err = svc.QueryGroupedStats(context.Background(), &fakeIterable{entities: rows}, spi.ModelRef{}, nil, req)
	if err == nil {
		t.Fatal("untyped pushdown must fail closed (unevaluable $.age leaf), got nil error")
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
