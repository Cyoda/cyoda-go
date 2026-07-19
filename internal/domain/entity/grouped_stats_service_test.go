package entity_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/domain/entity"
)

// fakeIterable satisfies only spi.Iterable.
type fakeIterable struct {
	entities []*spi.Entity
	lastFlt  spi.Filter
}

func (f *fakeIterable) Iterate(_ context.Context, _ spi.ModelRef, flt spi.Filter, _ spi.IterateOptions) (spi.Iterator, error) {
	f.lastFlt = flt
	return &fakeIter{rows: f.entities}, nil
}

type fakeIter struct {
	rows []*spi.Entity
	idx  int
	err  error
}

func (i *fakeIter) Next() bool {
	if i.err != nil || i.idx >= len(i.rows) {
		return false
	}
	i.idx++
	return true
}
func (i *fakeIter) Entity() *spi.Entity { return i.rows[i.idx-1] }
func (i *fakeIter) Err() error          { return i.err }
func (i *fakeIter) Close() error        { return nil }

// fakeAggregator satisfies only spi.GroupedAggregator (and embeds an Iterable
// when tests want both capabilities).
type fakeAggregator struct {
	called  bool
	buckets []spi.GroupedAggregateBucket
	err     error
}

func (f *fakeAggregator) GroupedAggregate(
	_ context.Context,
	_ spi.ModelRef,
	_ []spi.GroupExpr,
	_ spi.Filter,
	_ spi.GroupedAggregationsOptions,
) ([]spi.GroupedAggregateBucket, error) {
	f.called = true
	if f.err != nil {
		return nil, f.err
	}
	return f.buckets, nil
}

// dualBackend satisfies BOTH Iterable and GroupedAggregator.
type dualBackend struct {
	*fakeIterable
	*fakeAggregator
}

func TestQueryGroupedStats_FallsBackToStreaming(t *testing.T) {
	rows := []*spi.Entity{
		{Meta: spi.EntityMeta{State: "available"}, Data: []byte(`{}`)},
		{Meta: spi.EntityMeta{State: "available"}, Data: []byte(`{}`)},
		{Meta: spi.EntityMeta{State: "allocated"}, Data: []byte(`{}`)},
	}
	svc := entity.NewGroupedStatsService(10000)
	req := &entity.ValidatedGroupedStatsRequest{
		GroupBy: []entity.GroupExprValidated{{IsState: true}},
	}
	buckets, err := svc.QueryGroupedStats(context.Background(), &fakeIterable{entities: rows}, spi.ModelRef{}, req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(buckets) != 2 {
		t.Fatalf("got %d buckets, want 2", len(buckets))
	}
	// total count check: 2 + 1 = 3
	var total int64
	for _, b := range buckets {
		total += b.Count
	}
	if total != 3 {
		t.Fatalf("total count %d, want 3", total)
	}
}

func TestQueryGroupedStats_501WhenNoCapability(t *testing.T) {
	type noop struct{}
	svc := entity.NewGroupedStatsService(10000)
	_, err := svc.QueryGroupedStats(context.Background(), noop{}, spi.ModelRef{}, &entity.ValidatedGroupedStatsRequest{GroupBy: []entity.GroupExprValidated{{IsState: true}}})
	if !errors.Is(err, entity.ErrBackendNotSupported) {
		t.Fatalf("want ErrBackendNotSupported, got %v", err)
	}
}

func TestQueryGroupedStats_PrefersPushdownWhenAvailable(t *testing.T) {
	agg := &fakeAggregator{
		buckets: []spi.GroupedAggregateBucket{
			{GroupKey: []spi.GroupKeyEntry{{Path: "state", Value: "available"}}, Count: 42},
			{GroupKey: []spi.GroupKeyEntry{{Path: "state", Value: "allocated"}}, Count: 7},
		},
	}
	iter := &fakeIterable{entities: []*spi.Entity{
		{Meta: spi.EntityMeta{State: "should-not-be-seen"}, Data: []byte(`{}`)},
	}}
	dual := dualBackend{fakeIterable: iter, fakeAggregator: agg}
	svc := entity.NewGroupedStatsService(10000)
	req := &entity.ValidatedGroupedStatsRequest{
		GroupBy: []entity.GroupExprValidated{{IsState: true}},
	}
	buckets, err := svc.QueryGroupedStats(context.Background(), dual, spi.ModelRef{}, req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !agg.called {
		t.Fatal("GroupedAggregate was not called even though backend supports pushdown")
	}
	if len(buckets) != 2 {
		t.Fatalf("got %d buckets, want 2", len(buckets))
	}
	// Sorted by count desc.
	if buckets[0].Count != 42 || buckets[1].Count != 7 {
		t.Fatalf("buckets not sorted by count desc: %+v", buckets)
	}
}

func TestQueryGroupedStats_PushdownNotPushdownableFallsBackToStreaming(t *testing.T) {
	agg := &fakeAggregator{err: spi.ErrAggregationNotPushdownable}
	iter := &fakeIterable{entities: []*spi.Entity{
		{Meta: spi.EntityMeta{State: "available"}, Data: []byte(`{}`)},
		{Meta: spi.EntityMeta{State: "allocated"}, Data: []byte(`{}`)},
	}}
	dual := dualBackend{fakeIterable: iter, fakeAggregator: agg}
	svc := entity.NewGroupedStatsService(10000)
	req := &entity.ValidatedGroupedStatsRequest{
		GroupBy: []entity.GroupExprValidated{{IsState: true}},
	}
	buckets, err := svc.QueryGroupedStats(context.Background(), dual, spi.ModelRef{}, req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !agg.called {
		t.Fatal("GroupedAggregate should have been attempted")
	}
	if len(buckets) != 2 {
		t.Fatalf("streaming fallback should have produced 2 buckets, got %d", len(buckets))
	}
}

func TestQueryGroupedStats_PushdownArbitraryErrorPropagates(t *testing.T) {
	wantErr := errors.New("boom")
	agg := &fakeAggregator{err: wantErr}
	iter := &fakeIterable{entities: []*spi.Entity{
		{Meta: spi.EntityMeta{State: "available"}, Data: []byte(`{}`)},
	}}
	dual := dualBackend{fakeIterable: iter, fakeAggregator: agg}
	svc := entity.NewGroupedStatsService(10000)
	req := &entity.ValidatedGroupedStatsRequest{
		GroupBy: []entity.GroupExprValidated{{IsState: true}},
	}
	_, err := svc.QueryGroupedStats(context.Background(), dual, spi.ModelRef{}, req)
	if !errors.Is(err, wantErr) {
		t.Fatalf("want %v, got %v", wantErr, err)
	}
}

func TestQueryGroupedStats_InTransactionSkipsPushdown(t *testing.T) {
	agg := &fakeAggregator{} // would succeed; we assert it's NOT called
	iter := &fakeIterable{entities: []*spi.Entity{
		{Meta: spi.EntityMeta{State: "available"}, Data: []byte(`{}`)},
	}}
	dual := dualBackend{fakeIterable: iter, fakeAggregator: agg}
	svc := entity.NewGroupedStatsService(10000)
	req := &entity.ValidatedGroupedStatsRequest{
		GroupBy: []entity.GroupExprValidated{{IsState: true}},
	}
	tx := &spi.TransactionState{ID: "tx-test"}
	ctx := spi.WithTransaction(context.Background(), tx)
	buckets, err := svc.QueryGroupedStats(ctx, dual, spi.ModelRef{}, req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if agg.called {
		t.Fatal("GroupedAggregate must NOT be called inside an active transaction (D11)")
	}
	if len(buckets) != 1 {
		t.Fatalf("streaming path should have yielded 1 bucket, got %d", len(buckets))
	}
}

func TestQueryGroupedStats_CardinalityExceeded(t *testing.T) {
	rows := make([]*spi.Entity, 0, 5)
	for i := 0; i < 5; i++ {
		rows = append(rows, &spi.Entity{
			Meta: spi.EntityMeta{State: fmt.Sprintf("state-%d", i)},
			Data: []byte(`{}`),
		})
	}
	svc := entity.NewGroupedStatsService(3) // ceiling = 3
	req := &entity.ValidatedGroupedStatsRequest{
		GroupBy: []entity.GroupExprValidated{{IsState: true}},
	}
	_, err := svc.QueryGroupedStats(context.Background(), &fakeIterable{entities: rows}, spi.ModelRef{}, req)
	if !errors.Is(err, spi.ErrGroupCardinalityExceeded) {
		t.Fatalf("want ErrGroupCardinalityExceeded, got %v", err)
	}
}

func TestQueryGroupedStats_StreamingWithFilterPushdown(t *testing.T) {
	// Valid condition that translates to a Filter cleanly.
	cond := json.RawMessage(`{
		"type": "simple",
		"jsonPath": "$.color",
		"operatorType": "EQUALS",
		"value": "red"
	}`)
	rows := []*spi.Entity{
		{Meta: spi.EntityMeta{State: "available"}, Data: []byte(`{"color":"red"}`)},
		{Meta: spi.EntityMeta{State: "available"}, Data: []byte(`{"color":"red"}`)},
	}
	iter := &fakeIterable{entities: rows}
	svc := entity.NewGroupedStatsService(10000)
	req := &entity.ValidatedGroupedStatsRequest{
		GroupBy:   []entity.GroupExprValidated{{IsState: true}},
		Condition: []byte(cond),
	}
	buckets, err := svc.QueryGroupedStats(context.Background(), iter, spi.ModelRef{}, req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Filter was pushable: should have been passed to the iterator.
	if iter.lastFlt.Op == "" {
		t.Fatal("expected pushable filter passed to Iterate, got zero-value")
	}
	if len(buckets) != 1 || buckets[0].Count != 2 {
		t.Fatalf("buckets = %+v, want one bucket count=2", buckets)
	}
}

func TestQueryGroupedStats_StreamingWithUnpushableConditionAppliesResidual(t *testing.T) {
	// FunctionCondition is parseable but ConditionToFilter rejects it,
	// so the service must pass zero-value Filter and re-apply match.Match.
	cond := json.RawMessage(`{
		"type": "function",
		"function": {"name": "any-fn"}
	}`)
	rows := []*spi.Entity{
		{Meta: spi.EntityMeta{State: "available"}, Data: []byte(`{}`)},
	}
	iter := &fakeIterable{entities: rows}
	svc := entity.NewGroupedStatsService(10000)
	req := &entity.ValidatedGroupedStatsRequest{
		GroupBy:   []entity.GroupExprValidated{{IsState: true}},
		Condition: []byte(cond),
	}
	_, err := svc.QueryGroupedStats(context.Background(), iter, spi.ModelRef{}, req)
	// match.Match returns an error for FunctionCondition — surface it.
	if err == nil {
		t.Fatal("expected match.Match error for function condition, got nil")
	}
	// And critically, Iterate must have been called with a zero-value Filter.
	if iter.lastFlt.Op != "" {
		t.Fatalf("expected zero-value Filter, got %+v", iter.lastFlt)
	}
}

func TestQueryGroupedStats_AggregationsViaStreaming(t *testing.T) {
	rows := []*spi.Entity{
		{Meta: spi.EntityMeta{State: "x"}, Data: []byte(`{"v":10}`)},
		{Meta: spi.EntityMeta{State: "x"}, Data: []byte(`{"v":20}`)},
		{Meta: spi.EntityMeta{State: "x"}, Data: []byte(`{"v":30}`)},
	}
	svc := entity.NewGroupedStatsService(10000)
	req := &entity.ValidatedGroupedStatsRequest{
		GroupBy: []entity.GroupExprValidated{{IsState: true}},
		Aggregations: []entity.AggregationExprValidated{
			{Op: entity.AggSum, Field: "$.v", Alias: "sum_v"},
			{Op: entity.AggAvg, Field: "$.v", Alias: "avg_v"},
		},
	}
	buckets, err := svc.QueryGroupedStats(context.Background(), &fakeIterable{entities: rows}, spi.ModelRef{}, req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(buckets) != 1 {
		t.Fatalf("want 1 bucket, got %d", len(buckets))
	}
	if got := buckets[0].Aggregations["sum_v"]; got != 60.0 {
		t.Fatalf("sum_v = %v, want 60", got)
	}
	if got := buckets[0].Aggregations["avg_v"]; got != 20.0 {
		t.Fatalf("avg_v = %v, want 20", got)
	}
}

func TestQueryGroupedStats_GroupByNumberProducesMultipleBuckets(t *testing.T) {
	// D4 regression: numeric runtime values are scalar and must be grouped
	// by their text representation, not collapsed into a single null bucket.
	rows := []*spi.Entity{
		{Meta: spi.EntityMeta{State: "x"}, Data: []byte(`{"tier":1}`)},
		{Meta: spi.EntityMeta{State: "x"}, Data: []byte(`{"tier":1}`)},
		{Meta: spi.EntityMeta{State: "x"}, Data: []byte(`{"tier":2}`)},
	}
	svc := entity.NewGroupedStatsService(10000)
	req := &entity.ValidatedGroupedStatsRequest{
		GroupBy: []entity.GroupExprValidated{{Path: "$.tier"}},
	}
	buckets, err := svc.QueryGroupedStats(context.Background(), &fakeIterable{entities: rows}, spi.ModelRef{}, req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(buckets) != 2 {
		t.Fatalf("got %d buckets, want 2 (one per distinct numeric value)", len(buckets))
	}
	// Sorted count desc — first bucket should be tier=1 with count 2.
	if buckets[0].Count != 2 || buckets[1].Count != 1 {
		t.Fatalf("bucket counts = [%d, %d], want [2, 1]", buckets[0].Count, buckets[1].Count)
	}
	// The groupKey value should be the JSON text form "1" / "2", matching
	// postgres's `doc->>'tier'` behaviour.
	if got := buckets[0].GroupKey[0].Value; got != "1" {
		t.Fatalf("buckets[0] groupKey value = %v (%T), want string \"1\"", got, got)
	}
	if got := buckets[1].GroupKey[0].Value; got != "2" {
		t.Fatalf("buckets[1] groupKey value = %v (%T), want string \"2\"", got, got)
	}
}

func TestQueryGroupedStats_GroupByBoolProducesMultipleBuckets(t *testing.T) {
	// D4 regression: boolean runtime values are scalar and must be grouped
	// by "true"/"false", not collapsed into a single null bucket.
	rows := []*spi.Entity{
		{Meta: spi.EntityMeta{State: "x"}, Data: []byte(`{"premium":true}`)},
		{Meta: spi.EntityMeta{State: "x"}, Data: []byte(`{"premium":false}`)},
	}
	svc := entity.NewGroupedStatsService(10000)
	req := &entity.ValidatedGroupedStatsRequest{
		GroupBy: []entity.GroupExprValidated{{Path: "$.premium"}},
	}
	buckets, err := svc.QueryGroupedStats(context.Background(), &fakeIterable{entities: rows}, spi.ModelRef{}, req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(buckets) != 2 {
		t.Fatalf("got %d buckets, want 2 (one per distinct bool value)", len(buckets))
	}
	// With equal counts, D12 total order is groupKey lex; "false" < "true".
	if got := buckets[0].GroupKey[0].Value; got != "false" {
		t.Fatalf("buckets[0] groupKey value = %v (%T), want string \"false\"", got, got)
	}
	if got := buckets[1].GroupKey[0].Value; got != "true" {
		t.Fatalf("buckets[1] groupKey value = %v (%T), want string \"true\"", got, got)
	}
}

func TestQueryGroupedStats_NonScalarRuntimeValueCoercesToNull(t *testing.T) {
	// D4: object/array runtime values DO coerce to null (this protects
	// D4's actual intent — only non-scalar values collapse).
	rows := []*spi.Entity{
		{Meta: spi.EntityMeta{State: "x"}, Data: []byte(`{"variantId":{"x":1}}`)},
		{Meta: spi.EntityMeta{State: "x"}, Data: []byte(`{"variantId":[1,2,3]}`)},
	}
	svc := entity.NewGroupedStatsService(10000)
	req := &entity.ValidatedGroupedStatsRequest{
		GroupBy: []entity.GroupExprValidated{{Path: "$.variantId"}},
	}
	buckets, err := svc.QueryGroupedStats(context.Background(), &fakeIterable{entities: rows}, spi.ModelRef{}, req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(buckets) != 1 {
		t.Fatalf("got %d buckets, want 1 (object + array both coerce to null)", len(buckets))
	}
	if buckets[0].Count != 2 {
		t.Fatalf("bucket count = %d, want 2", buckets[0].Count)
	}
	if got := buckets[0].GroupKey[0].Value; got != nil {
		t.Fatalf("groupKey value = %v (%T), want nil (D4 null coercion)", got, got)
	}
}

func TestQueryGroupedStats_PushdownPropagatesCardinalityError(t *testing.T) {
	// If the pushdown plugin itself reports cardinality exceeded, surface
	// that — do NOT silently fall back to streaming (streaming would re-do
	// the same work and likely fail too, but more importantly the plugin's
	// signal is authoritative).
	agg := &fakeAggregator{err: spi.ErrGroupCardinalityExceeded}
	iter := &fakeIterable{}
	dual := dualBackend{fakeIterable: iter, fakeAggregator: agg}
	svc := entity.NewGroupedStatsService(10000)
	req := &entity.ValidatedGroupedStatsRequest{
		GroupBy: []entity.GroupExprValidated{{IsState: true}},
	}
	_, err := svc.QueryGroupedStats(context.Background(), dual, spi.ModelRef{}, req)
	if !errors.Is(err, spi.ErrGroupCardinalityExceeded) {
		t.Fatalf("want ErrGroupCardinalityExceeded, got %v", err)
	}
}

// TestQueryGroupedStats_MalformedRegexRejected is a regression test for a
// fail-open bug: the plugin residual filter evaluators
// (plugins/sqlite/post_filter.go evaluateFilter, plugins/postgres/grouped_stats.go
// evalPostFilter) delegate to the error-free spi.MatchFilter, which returns
// false (non-match) rather than erroring on a malformed MATCHES_PATTERN
// regex. Without upstream validation this silently under-includes buckets
// instead of rejecting the request. QueryGroupedStats must reject a
// malformed regex before dispatching to any backend, the same way the
// search path already does.
func TestQueryGroupedStats_MalformedRegexRejected(t *testing.T) {
	cond := json.RawMessage(`{
		"type": "simple",
		"jsonPath": "$.color",
		"operatorType": "MATCHES_PATTERN",
		"value": "["
	}`)
	rows := []*spi.Entity{
		{Meta: spi.EntityMeta{State: "available"}, Data: []byte(`{"color":"red"}`)},
	}
	iter := &fakeIterable{entities: rows}
	svc := entity.NewGroupedStatsService(10000)
	req := &entity.ValidatedGroupedStatsRequest{
		GroupBy:   []entity.GroupExprValidated{{IsState: true}},
		Condition: []byte(cond),
	}
	_, err := svc.QueryGroupedStats(context.Background(), iter, spi.ModelRef{}, req)
	if !errors.Is(err, entity.ErrInvalidCondition) {
		t.Fatalf("want ErrInvalidCondition for malformed regex, got %v", err)
	}
}

// TestQueryGroupedStats_ValidRegexStillSucceeds guards against an
// over-broad fix: a well-formed MATCHES_PATTERN must still succeed.
func TestQueryGroupedStats_ValidRegexStillSucceeds(t *testing.T) {
	cond := json.RawMessage(`{
		"type": "simple",
		"jsonPath": "$.color",
		"operatorType": "MATCHES_PATTERN",
		"value": "^r.*$"
	}`)
	rows := []*spi.Entity{
		{Meta: spi.EntityMeta{State: "available"}, Data: []byte(`{"color":"red"}`)},
	}
	iter := &fakeIterable{entities: rows}
	svc := entity.NewGroupedStatsService(10000)
	req := &entity.ValidatedGroupedStatsRequest{
		GroupBy:   []entity.GroupExprValidated{{IsState: true}},
		Condition: []byte(cond),
	}
	buckets, err := svc.QueryGroupedStats(context.Background(), iter, spi.ModelRef{}, req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(buckets) != 1 || buckets[0].Count != 1 {
		t.Fatalf("buckets = %+v, want one bucket count=1", buckets)
	}
}
