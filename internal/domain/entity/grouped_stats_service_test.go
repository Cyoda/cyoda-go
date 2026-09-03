package entity_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/domain/entity"
	"github.com/cyoda-platform/cyoda-go/internal/domain/model/schema"
	"github.com/cyoda-platform/cyoda-go/internal/domain/search"
)

// fakeIterable serves Iterate and nothing else, and genuinely applies the Filter
// it is handed — via spi.Prepare(flt).Match, the same kernel a real backend's
// Iterate uses — rather than recording it and returning every row regardless.
//
// It used to be a pure recorder, trusting every caller to either pass a
// zero-value Filter (which spi.Prepare/PreparedFilter.Match already treats
// as match-everything, so genuine filtering is a no-op for those callers) or
// not care about the returned rows' contents. That was safe only as long as
// grouped_stats_service.go's tallyStreaming had some way to reach its
// residual guard (pushable == false) with a real, non-trivial condition —
// once every condition shape reachable from ValidatedGroupedStatsRequest
// became pushable (spi.ConditionToFilter now translates a wildcard array
// path like any other), tallyStreaming started trusting THIS double to have
// filtered a real, non-zero pushdown Filter it was handed, and a recorder
// that doesn't enforce it silently returned unfiltered rows — the residual
// guard being correctly skipped is what makes not-filtering wrong, not a
// property of the guard itself. A genuinely-filtering double is what a real
// backend's Iterate is, so this is what fakeIterable must be to stand in for
// one.
type fakeIterable struct {
	// Embedded nil: only Iterate is ever called on this double, and a
	// panic on any other EntityStore method is the assertion that stays
	// true. Iterate below shadows the embedded interface's own.
	spi.EntityStore
	entities []*spi.Entity
	lastFlt  spi.Filter
	// iterErr, when set, is returned from the yielded fakeIter's Err()
	// after Next() runs dry — models a mid-stream driver failure (e.g.
	// a scan-budget sentinel) surfacing after iteration completes.
	iterErr error
}

func (f *fakeIterable) Iterate(_ context.Context, _ spi.ModelRef, flt spi.Filter, _ spi.IterateOptions) (spi.Iterator, error) {
	f.lastFlt = flt
	pf, err := spi.Prepare(flt)
	if err != nil {
		return nil, err
	}
	rows := make([]*spi.Entity, 0, len(f.entities))
	for _, e := range f.entities {
		if pf.Match(e.Data, e.Meta) {
			rows = append(rows, e)
		}
	}
	return &fakeIter{rows: rows, err: f.iterErr}, nil
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

// closeStickyIter models the trap tallyStreaming's iterator drain used to
// have (M4, final review): some iterator implementations only surface a
// sticky scan error once Close() has run — Err() returns nil beforehand.
// Reading Err() before Close() (the pre-fix ordering: a bare
// `defer iter.Close()` registered ahead of a same-function `iter.Err()`
// call that ran first) would see the stale nil and miss the failure
// entirely; reading it after Close() (the fix) observes it.
type closeStickyIter struct {
	rows      []*spi.Entity
	idx       int
	stickyErr error
	err       error
	closed    bool
}

func (i *closeStickyIter) Next() bool {
	if i.idx >= len(i.rows) {
		return false
	}
	i.idx++
	return true
}
func (i *closeStickyIter) Entity() *spi.Entity { return i.rows[i.idx-1] }
func (i *closeStickyIter) Err() error          { return i.err }
func (i *closeStickyIter) Close() error {
	if !i.closed {
		i.closed = true
		i.err = i.stickyErr
	}
	return nil
}

// closeStickyIterable serves Iterate and nothing else, always yielding a
// closeStickyIter over rows.
type closeStickyIterable struct {
	// Embedded nil: see fakeIterable.
	spi.EntityStore
	rows      []*spi.Entity
	stickyErr error
}

func (f *closeStickyIterable) Iterate(_ context.Context, _ spi.ModelRef, _ spi.Filter, _ spi.IterateOptions) (spi.Iterator, error) {
	return &closeStickyIter{rows: f.rows, stickyErr: f.stickyErr}, nil
}

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

// newStreamingStatsFixture builds a service + streaming-only (Iterable)
// store whose iterator's Err() returns iterErr once Next() runs dry —
// used to exercise sentinel classification for errors surfaced from the
// streaming fallback path.
func newStreamingStatsFixture(t *testing.T, iterErr error) (
	svc *entity.GroupedStatsService,
	ctx context.Context,
	store *fakeIterable,
	model spi.ModelRef,
	req *entity.ValidatedGroupedStatsRequest,
) {
	t.Helper()
	svc = entity.NewGroupedStatsService(10000)
	ctx = context.Background()
	store = &fakeIterable{iterErr: iterErr}
	model = spi.ModelRef{}
	req = &entity.ValidatedGroupedStatsRequest{
		GroupBy: []entity.GroupExprValidated{{IsState: true}},
	}
	return svc, ctx, store, model, req
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
	buckets, err := svc.QueryGroupedStats(context.Background(), &fakeIterable{entities: rows}, spi.ModelRef{}, nil, req)
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
	buckets, err := svc.QueryGroupedStats(context.Background(), dual, spi.ModelRef{}, nil, req)
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
	buckets, err := svc.QueryGroupedStats(context.Background(), dual, spi.ModelRef{}, nil, req)
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
	_, err := svc.QueryGroupedStats(context.Background(), dual, spi.ModelRef{}, nil, req)
	if !errors.Is(err, wantErr) {
		t.Fatalf("want %v, got %v", wantErr, err)
	}
	// Arbitrary storage/driver errors must NOT be classified as an
	// AppError — otherwise the handler's default 500 fallback would be
	// bypassed and an unrecognized failure would surface as a 4xx.
	var appErr *common.AppError
	if errors.As(err, &appErr) {
		t.Errorf("arbitrary storage error must NOT be wrapped as AppError (would become 4xx): %v", err)
	}
}

// TestQueryGroupedStats_CloseOnlySurfacedErrorFailsQuery pins the fix for
// M4 (final review): tallyStreaming used to read iter.Err() before the
// deferred iter.Close() ran, so an error that only becomes visible via
// Err() after Close() (closeStickyIter's shape — see its doc comment) was
// silently missed and the query returned success. Reordering to read Err()
// after Close() runs surfaces it.
func TestQueryGroupedStats_CloseOnlySurfacedErrorFailsQuery(t *testing.T) {
	svc := entity.NewGroupedStatsService(10000)
	req := &entity.ValidatedGroupedStatsRequest{
		GroupBy: []entity.GroupExprValidated{{IsState: true}},
	}
	stickyErr := errors.New("driver error surfaced only at Close")
	store := &closeStickyIterable{
		rows:      []*spi.Entity{{Meta: spi.EntityMeta{State: "available"}, Data: []byte(`{}`)}},
		stickyErr: stickyErr,
	}

	_, err := svc.QueryGroupedStats(context.Background(), store, spi.ModelRef{}, nil, req)
	if err == nil {
		t.Fatal("expected the Close()-only-surfaced error to fail the query, got nil (success) — a silent scan truncation")
	}
	if !errors.Is(err, stickyErr) {
		t.Errorf("got %v, want an error wrapping/matching stickyErr", err)
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
	buckets, err := svc.QueryGroupedStats(ctx, dual, spi.ModelRef{}, nil, req)
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
	_, err := svc.QueryGroupedStats(context.Background(), &fakeIterable{entities: rows}, spi.ModelRef{}, nil, req)
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
	// fakeIterable genuinely applies the pushed Filter (spi.Prepare(flt).Match),
	// and the pushdown Filter's Declared type comes from fields: an EQUALS leaf
	// with no declared type degrades to non-match in the shared kernel. Declare
	// $.color so the two matching rows below are actually evaluated as matches,
	// not merely returned unfiltered by the store.
	fields := map[string]schema.FieldDescriptor{
		"$.color": {Path: "$.color", Types: []spi.DataType{spi.String}},
	}
	buckets, err := svc.QueryGroupedStats(context.Background(), iter, spi.ModelRef{}, fields, req)
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

// TestQueryGroupedStats_WildcardArrayPathConditionPushesDown proves a
// wildcard array path condition is now filtered correctly end to end via
// genuine pushdown, not a re-applied residual.
//
// This test used to be named …AppliesResidual and pin the opposite: a
// wildcard array path was parseable and evaluable in-memory but
// ConditionToFilter rejected the "[" as non-pushdownable, forcing the
// service down the residual path (a zero-value Filter to Iterate, then
// match.Prepare/(Prepared).Match re-applied per row). That premise is dead —
// spi.ConditionToFilter now pushes a wildcard array path down like any other
// (path-grammar.md §2/§8) — so `pushable` comes back true for this
// condition and the residual guard never fires for it. The genuinely-
// filtering fakeIterable (see its own doc comment) is what makes this a
// meaningful assertion rather than a coincidence: the pushed Filter it
// receives is what excludes the second row, and iter.lastFlt now being
// NON-zero (the opposite of what this test used to require) is the
// evidence of that.
//
// There is no residual mechanism left to cover separately: the tally applies
// the translated filter and nothing else, and a condition that would not
// translate is refused before it gets here.
func TestQueryGroupedStats_WildcardArrayPathConditionPushesDown(t *testing.T) {
	cond := json.RawMessage(`{
		"type": "simple",
		"jsonPath": "$.items[*].name",
		"operatorType": "EQUALS",
		"value": "keep"
	}`)
	rows := []*spi.Entity{
		{Meta: spi.EntityMeta{State: "available"}, Data: []byte(`{"items":[{"name":"keep"}]}`)},
		{Meta: spi.EntityMeta{State: "available"}, Data: []byte(`{"items":[{"name":"drop"}]}`)},
	}
	iter := &fakeIterable{entities: rows}
	svc := entity.NewGroupedStatsService(10000)
	req := &entity.ValidatedGroupedStatsRequest{
		GroupBy:   []entity.GroupExprValidated{{IsState: true}},
		Condition: []byte(cond),
	}
	// The pushed Filter's Declared type comes from fields: an EQUALS leaf
	// with no declared type degrades to non-match in the shared kernel.
	fields := map[string]schema.FieldDescriptor{
		"$.items[*].name": {Path: "$.items[*].name", Types: []spi.DataType{spi.String}, IsArray: true},
	}
	buckets, err := svc.QueryGroupedStats(context.Background(), iter, spi.ModelRef{}, fields, req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Critically, Iterate must have been called with the pushed-down Filter,
	// not a zero-value one — the condition is pushable now.
	if iter.lastFlt.Op == "" {
		t.Fatal("expected the pushable filter passed to Iterate, got zero-value")
	}
	if len(buckets) != 1 || buckets[0].Count != 1 {
		t.Fatalf("buckets = %+v, want one bucket count=1 (the pushed filter excluded the second row)", buckets)
	}
}

// TestQueryGroupedStats_TallyTrustsTheTranslatedFilter pins that the tally
// hands the store the filter the condition translated to and then counts what
// comes back, without re-deciding per row.
//
// It used to guard the symmetry between two "!pushable" guards in
// tallyStreaming — the one that built a residual match.Prepared and the one
// in the per-row loop that applied it — because a loop guard firing while
// the build guard had not left every row matched against a zero-value
// match.Prepared, silently emptying every bucket with no error. Both guards
// are gone: an untranslatable condition is refused now, so the tally has no
// second opinion to keep in sync. The scenario still earns its place —
// a real filter, a store that enforces it (fakeIterable), and mixed rows, so
// a tally that dropped or ignored the filter produces an observably wrong
// count rather than coincidentally passing.
func TestQueryGroupedStats_TallyTrustsTheTranslatedFilter(t *testing.T) {
	cond := json.RawMessage(`{
		"type": "simple",
		"jsonPath": "$.color",
		"operatorType": "EQUALS",
		"value": "red"
	}`)
	rows := []*spi.Entity{
		{Meta: spi.EntityMeta{State: "available"}, Data: []byte(`{"color":"red"}`)},
		{Meta: spi.EntityMeta{State: "available"}, Data: []byte(`{"color":"red"}`)},
		{Meta: spi.EntityMeta{State: "available"}, Data: []byte(`{"color":"blue"}`)},
	}
	iter := &fakeIterable{entities: rows}
	svc := entity.NewGroupedStatsService(10000)
	req := &entity.ValidatedGroupedStatsRequest{
		GroupBy:   []entity.GroupExprValidated{{IsState: true}},
		Condition: []byte(cond),
	}
	// The pushdown Filter's Declared type comes from fields: an EQUALS leaf
	// with no declared type degrades to non-match in the shared kernel, so
	// fakeIterable's own spi.Prepare(flt).Match needs it too or every row —
	// matching or not — would be excluded regardless of any residual guard.
	fields := map[string]schema.FieldDescriptor{
		"$.color": {Path: "$.color", Types: []spi.DataType{spi.String}},
	}
	buckets, err := svc.QueryGroupedStats(context.Background(), iter, spi.ModelRef{}, fields, req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(buckets) != 1 || buckets[0].Count != 2 {
		t.Fatalf("buckets = %+v, want one bucket count=2 (the two pushdown-filtered red rows, not silently emptied by a wrongly re-applied residual)", buckets)
	}
}

// A function clause reaching the grouped-stats service would be a boundary
// bug — search.ValidateCondition rejects it first. Defence in depth: if one
// ever does, it must fail closed rather than silently produce buckets over an
// unevaluated predicate.
func TestQueryGroupedStats_FunctionConditionFailsClosed(t *testing.T) {
	cond := json.RawMessage(`{"type":"function","function":{"name":"any-fn"}}`)
	rows := []*spi.Entity{
		{Meta: spi.EntityMeta{State: "available"}, Data: []byte(`{}`)},
	}
	svc := entity.NewGroupedStatsService(10000)
	req := &entity.ValidatedGroupedStatsRequest{
		GroupBy:   []entity.GroupExprValidated{{IsState: true}},
		Condition: []byte(cond),
	}
	if _, err := svc.QueryGroupedStats(context.Background(), &fakeIterable{entities: rows}, spi.ModelRef{}, nil, req); err == nil {
		t.Fatal("expected a function condition to fail closed, got nil")
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
	buckets, err := svc.QueryGroupedStats(context.Background(), &fakeIterable{entities: rows}, spi.ModelRef{}, nil, req)
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
	buckets, err := svc.QueryGroupedStats(context.Background(), &fakeIterable{entities: rows}, spi.ModelRef{}, nil, req)
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
	buckets, err := svc.QueryGroupedStats(context.Background(), &fakeIterable{entities: rows}, spi.ModelRef{}, nil, req)
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
	buckets, err := svc.QueryGroupedStats(context.Background(), &fakeIterable{entities: rows}, spi.ModelRef{}, nil, req)
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
	_, err := svc.QueryGroupedStats(context.Background(), dual, spi.ModelRef{}, nil, req)
	if !errors.Is(err, spi.ErrGroupCardinalityExceeded) {
		t.Fatalf("want ErrGroupCardinalityExceeded, got %v", err)
	}
}

// TestQueryGroupedStats_InvalidFilterPathMapsTo400 pins the missing arm of
// classifyGroupedStatsError. spi.ErrInvalidFilterPath is the cross-backend
// sentinel a plugin returns when a path it was handed is not the model's path
// syntax — malformed CLIENT input, reaching the plugin's backstop. Returned
// unclassified it became a 500 plus a support ticket, on the same input
// /search answers 400 INVALID_FIELD_PATH for.
func TestQueryGroupedStats_InvalidFilterPathMapsTo400(t *testing.T) {
	agg := &fakeAggregator{err: fmt.Errorf("plugin detail: %w", spi.ErrInvalidFilterPath)}
	iter := &fakeIterable{}
	dual := dualBackend{fakeIterable: iter, fakeAggregator: agg}
	svc := entity.NewGroupedStatsService(10000)
	req := &entity.ValidatedGroupedStatsRequest{
		GroupBy: []entity.GroupExprValidated{{IsState: true}},
	}
	_, err := svc.QueryGroupedStats(context.Background(), dual, spi.ModelRef{}, nil, req)

	var appErr *common.AppError
	if !errors.As(err, &appErr) {
		t.Fatalf("want *common.AppError, got %T: %v", err, err)
	}
	if appErr.Status != http.StatusBadRequest || appErr.Code != common.ErrCodeInvalidFieldPath {
		t.Errorf("got %d/%q, want 400/%s", appErr.Status, appErr.Code, common.ErrCodeInvalidFieldPath)
	}
	if !errors.Is(err, spi.ErrInvalidFilterPath) {
		t.Errorf("errors.Is(err, ErrInvalidFilterPath) = false; WithCause must preserve the sentinel")
	}
}

// TestQueryGroupedStats_MalformedRegexRejected is a regression test for a
// fail-open bug: the plugin residual filter evaluators
// (plugins/sqlite/post_filter.go evaluateFilter, plugins/postgres/grouped_stats.go
// evalPostFilter) delegate to the error-free spi.PreparedFilter.Match kernel,
// which returns false (non-match) rather than erroring on a malformed MATCHES_PATTERN
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
	_, err := svc.QueryGroupedStats(context.Background(), iter, spi.ModelRef{}, nil, req)
	if !errors.Is(err, entity.ErrInvalidCondition) {
		t.Fatalf("want ErrInvalidCondition for malformed regex, got %v", err)
	}
}

// TestQueryGroupedStats_LifecycleTemporalTypeMismatchRejected is a
// regression test for the validation-consistency gap: /search rejects a
// type-unsound temporal/lifecycle condition (400), but grouped-stats
// previously accepted the same malformed condition and silently degraded to
// an empty result (the condition doesn't translate to a pushdown filter and
// never matches anything via match.Prepare either). QueryGroupedStats must
// reject a temporal comparison operand that parses into no temporal type,
// the same way search.ValidateConditionValueTypes does.
func TestQueryGroupedStats_LifecycleTemporalTypeMismatchRejected(t *testing.T) {
	// Parse-based (spec §6): a comparison operand that parses into no temporal
	// type is rejected; a string operator like CONTAINS is now accepted, so a
	// genuinely-unparseable operand drives this rejection.
	cond := json.RawMessage(`{
		"type": "lifecycle",
		"field": "creationDate",
		"operatorType": "GREATER_THAN",
		"value": "not-a-date"
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
	_, err := svc.QueryGroupedStats(context.Background(), iter, spi.ModelRef{}, nil, req)
	if err == nil {
		t.Fatal("expected error for non-temporal operand against temporal field creationDate, got nil (previously silently degraded to empty result)")
	}
	if !errors.Is(err, search.ErrConditionTypeMismatch) {
		t.Fatalf("want search.ErrConditionTypeMismatch (parity with /search's CONDITION_TYPE_MISMATCH), got %v", err)
	}
}

// TestQueryGroupedStats_LifecycleTemporalStringOpRejected is the fifth
// defect this batch closes: a string or pattern operator on a temporal meta
// field (creationDate, lastUpdateTime) previously produced two different
// answers depending on the query plan — the SPI kernel's pushdown re-check
// bridges the field to RFC3339 text and matches CONTAINS lexically, while
// internal/match's residual route guarded the same case to a never-match.
// classifyGroupedStatsError must classify the shared boundary's rejection as
// search.ErrInvalidCondition (400 INVALID_CONDITION), the same code /search
// now answers for the identical condition.
func TestQueryGroupedStats_LifecycleTemporalStringOpRejected(t *testing.T) {
	cond := json.RawMessage(`{
		"type": "lifecycle",
		"field": "creationDate",
		"operatorType": "CONTAINS",
		"value": "2021"
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
	_, err := svc.QueryGroupedStats(context.Background(), iter, spi.ModelRef{}, nil, req)
	if err == nil {
		t.Fatal("expected error for CONTAINS against temporal field creationDate, got nil")
	}
	if !errors.Is(err, search.ErrInvalidCondition) {
		t.Fatalf("want search.ErrInvalidCondition (parity with /search's INVALID_CONDITION), got %v", err)
	}
	var appErr *common.AppError
	if !errors.As(err, &appErr) {
		t.Fatalf("want *common.AppError, got %T: %v", err, err)
	}
	if appErr.Code != common.ErrCodeInvalidCondition {
		t.Fatalf("got code %s, want %s", appErr.Code, common.ErrCodeInvalidCondition)
	}
}

// TestQueryGroupedStats_MalformedBetweenArityRejected is a regression test
// for the same validation-consistency gap: a BETWEEN condition with the
// wrong number of operands must be rejected up front (search.ValidateCondition),
// not silently mistranslated by the filter/match layers downstream.
func TestQueryGroupedStats_MalformedBetweenArityRejected(t *testing.T) {
	cond := json.RawMessage(`{
		"type": "simple",
		"jsonPath": "$.price",
		"operatorType": "BETWEEN",
		"value": [10]
	}`)
	rows := []*spi.Entity{
		{Meta: spi.EntityMeta{State: "available"}, Data: []byte(`{"price":10}`)},
	}
	iter := &fakeIterable{entities: rows}
	svc := entity.NewGroupedStatsService(10000)
	req := &entity.ValidatedGroupedStatsRequest{
		GroupBy:   []entity.GroupExprValidated{{IsState: true}},
		Condition: []byte(cond),
	}
	_, err := svc.QueryGroupedStats(context.Background(), iter, spi.ModelRef{}, nil, req)
	if !errors.Is(err, entity.ErrInvalidCondition) {
		t.Fatalf("want ErrInvalidCondition for malformed-arity BETWEEN, got %v", err)
	}
}

// TestQueryGroupedStats_UnknownMetaFieldRejected is a regression test for
// the same gap: an unrecognized meta filter field name must be rejected as
// INVALID_FIELD_PATH parity with /search, not silently treated as
// never-matching.
func TestQueryGroupedStats_UnknownMetaFieldRejected(t *testing.T) {
	cond := json.RawMessage(`{
		"type": "lifecycle",
		"field": "bogus",
		"operatorType": "EQUALS",
		"value": "x"
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
	_, err := svc.QueryGroupedStats(context.Background(), iter, spi.ModelRef{}, nil, req)
	if !errors.Is(err, search.ErrInvalidFieldPath) {
		t.Fatalf("want search.ErrInvalidFieldPath (parity with /search's INVALID_FIELD_PATH), got %v", err)
	}
}

// TestQueryGroupedStats_ValidTemporalConditionStillSucceeds guards against
// an over-broad fix: a well-formed temporal lifecycle condition (valid
// comparison operator + offset-bearing RFC3339 operand) must still succeed.
func TestQueryGroupedStats_ValidTemporalConditionStillSucceeds(t *testing.T) {
	cond := json.RawMessage(`{
		"type": "lifecycle",
		"field": "creationDate",
		"operatorType": "GREATER_THAN",
		"value": "2021-01-01T00:00:00Z"
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
	_, err := svc.QueryGroupedStats(context.Background(), iter, spi.ModelRef{}, nil, req)
	if err != nil {
		t.Fatalf("unexpected error for valid temporal condition: %v", err)
	}
}

// TestQueryGroupedStats_ValidDataConditionStillSucceeds guards against an
// over-broad fix: a well-formed data-field condition must still succeed
// even though grouped-stats now runs search.ValidateConditionValueTypes with
// a nil model (data-field checks are gracefully skipped without a schema).
func TestQueryGroupedStats_ValidDataConditionStillSucceeds(t *testing.T) {
	cond := json.RawMessage(`{
		"type": "simple",
		"jsonPath": "$.color",
		"operatorType": "EQUALS",
		"value": "red"
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
	// fakeIterable genuinely applies the pushed Filter, whose Declared type
	// comes from fields: an EQUALS leaf with no declared type degrades to
	// non-match in the shared kernel. Declare $.color so the bucket-count
	// assertion below exercises a real match, not an unfiltered pass-through.
	fields := map[string]schema.FieldDescriptor{
		"$.color": {Path: "$.color", Types: []spi.DataType{spi.String}},
	}
	buckets, err := svc.QueryGroupedStats(context.Background(), iter, spi.ModelRef{}, fields, req)
	if err != nil {
		t.Fatalf("unexpected error for valid data condition: %v", err)
	}
	if len(buckets) != 1 || buckets[0].Count != 1 {
		t.Fatalf("buckets = %+v, want one bucket count=1", buckets)
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
	buckets, err := svc.QueryGroupedStats(context.Background(), iter, spi.ModelRef{}, nil, req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(buckets) != 1 || buckets[0].Count != 1 {
		t.Fatalf("buckets = %+v, want one bucket count=1", buckets)
	}
}
