package entity

import (
	"context"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go-spi/predicate"
	"github.com/cyoda-platform/cyoda-go/internal/domain/model/schema"
)

// residualProbeIterable is an Iterate-only store that records the Filter
// tallyStreaming hands to Iterate and otherwise ignores it, returning every
// row regardless — the same shape entity_test's fakeIterable used to have,
// used here deliberately: this test's whole point is that the RESIDUAL, not
// the store, must exclude the non-matching row when pushable is false.
type residualProbeIterable struct {
	// Embedded nil: only Iterate is ever called on this double.
	spi.EntityStore
	entities []*spi.Entity
	lastFlt  spi.Filter
}

func (f *residualProbeIterable) Iterate(_ context.Context, _ spi.ModelRef, flt spi.Filter, _ spi.IterateOptions) (spi.Iterator, error) {
	f.lastFlt = flt
	return &residualProbeIter{rows: f.entities}, nil
}

type residualProbeIter struct {
	rows []*spi.Entity
	idx  int
}

func (i *residualProbeIter) Next() bool {
	if i.idx >= len(i.rows) {
		return false
	}
	i.idx++
	return true
}
func (i *residualProbeIter) Entity() *spi.Entity { return i.rows[i.idx-1] }
func (i *residualProbeIter) Err() error          { return nil }
func (i *residualProbeIter) Close() error        { return nil }

// TestTallyStreaming_UnpushableConditionAppliesResidual is a white-box unit
// test of tallyStreaming's own contract: when the caller determined the
// condition is not pushable (pushable == false), tallyStreaming must pass a
// zero-value Filter to Iterate and re-apply the condition itself, per entity,
// via the prepared residual (match.Prepare/(Prepared).Match) — never trust
// the store to have filtered when it was told not to.
//
// This used to be an entity_test (black-box) test driving the scenario
// through QueryGroupedStats with a JSON condition on a wildcard array path
// ($.items[*].name), on the premise that spi.ConditionToFilter refused that
// path and so pushable came back false. That premise is dead:
// spi.ConditionToFilter now pushes a wildcard array path down like any other
// (path-grammar.md §2/§8), so the condition translates and pushable comes
// back true for every condition shape reachable through
// ValidatedGroupedStatsRequest.Condition — queryGroupedStatsInner runs
// search.ValidateCondition on parsedCond before ever calling
// spi.ConditionToFilter, and every shape that clears that check now also
// clears translation (same closed five-clause set, same grammar, same
// operator set — see search.ValidateCondition and spi.ConditionToFilter's
// switches). A parsedCond of nil doesn't reach ConditionToFilter at all and
// leaves pushable at its true zero-value. So there is no longer any
// req.Condition value — well-formed or otherwise — that makes
// queryGroupedStatsInner compute pushable == false; the residual guard
// inside tallyStreaming is live code but currently unreachable from that
// entry point.
//
// The residual behavior itself is still a real contract tallyStreaming must
// honor — a future condition shape, or a plugin whose GroupedAggregate
// legitimately declines a shape mid-query, can still reach it — so this
// calls tallyStreaming directly with pushable forced to false, the same way
// production code would if that path were reachable, rather than deleting
// the coverage because the path it exercises has (for now) no way in from
// the outside.
func TestTallyStreaming_UnpushableConditionAppliesResidual(t *testing.T) {
	cond := &predicate.SimpleCondition{
		JsonPath:     "$.items[*].name",
		OperatorType: "EQUALS",
		Value:        "keep",
	}
	rows := []*spi.Entity{
		{Meta: spi.EntityMeta{State: "available"}, Data: []byte(`{"items":[{"name":"keep"}]}`)},
		{Meta: spi.EntityMeta{State: "available"}, Data: []byte(`{"items":[{"name":"drop"}]}`)},
	}
	it := &residualProbeIterable{entities: rows}
	svc := NewGroupedStatsService(10000)
	req := &ValidatedGroupedStatsRequest{
		GroupBy: []GroupExprValidated{{IsState: true}},
	}
	// The residual runs the type-directed kernel, which needs the declared
	// type of the leaf: an undeclared comparison leaf degrades to non-match.
	fields := map[string]schema.FieldDescriptor{
		"$.items[*].name": {Path: "$.items[*].name", Types: []spi.DataType{spi.String}, IsArray: true},
	}

	buckets, err := svc.tallyStreaming(context.Background(), it, spi.ModelRef{}, fields, req, spi.Filter{}, false, cond)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Critically, Iterate must have been called with a zero-value Filter —
	// tallyStreaming must not trust a pushFilter it was told is unpushable.
	if it.lastFlt.Op != "" {
		t.Fatalf("expected zero-value Filter, got %+v", it.lastFlt)
	}
	if len(buckets) != 1 || buckets[0].Count != 1 {
		t.Fatalf("buckets = %+v, want one bucket count=1 (residual excluded the second row)", buckets)
	}
}
