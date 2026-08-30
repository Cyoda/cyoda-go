package memory_test

import (
	"context"
	"errors"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/plugins/memory"
)

// filterPathStore spins up a fresh factory with one seeded entity and returns
// the store, the model ref, and the context — the fixture every filter-path
// test drives Search / Iterate / GroupedAggregate against.
func filterPathStore(t *testing.T, tenant, model string) (spi.EntityStore, spi.ModelRef, context.Context) {
	t.Helper()
	f := memory.NewStoreFactory()
	t.Cleanup(func() { _ = f.Close() })

	ctx := txIndexCtx(tenant)
	store, err := f.EntityStore(ctx)
	if err != nil {
		t.Fatalf("EntityStore: %v", err)
	}
	ref := spi.ModelRef{EntityName: model, ModelVersion: "1"}
	if _, err := store.Save(ctx, &spi.Entity{
		Meta: spi.EntityMeta{ID: "e1", ModelRef: ref, State: "NEW"},
		Data: []byte(`{"v":1,"a":{"b":2},"a-b":3,"a_b":4}`),
	}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	return store, ref, ctx
}

// runAllFilterEntryPoints calls every memory entry point that accepts a
// spi.Filter with the same filter and reports each one's error, keyed by
// entry-point name. Search / Iterate / GroupedAggregate are the complete set
// (`grep 'filter spi.Filter' plugins/memory/*.go`).
func runAllFilterEntryPoints(t *testing.T, store spi.EntityStore, ref spi.ModelRef, ctx context.Context, filter spi.Filter) map[string]error {
	t.Helper()
	out := map[string]error{}

	_, err := store.(spi.Searcher).Search(ctx, filter, spi.SearchOptions{
		ModelName:    ref.EntityName,
		ModelVersion: ref.ModelVersion,
		Limit:        10,
	})
	out["Search"] = err

	it, err := store.(spi.Iterable).Iterate(ctx, ref, filter, spi.IterateOptions{})
	if it != nil {
		// Drain so a lazily-surfaced error is not mistaken for acceptance.
		for it.Next() {
		}
		if err == nil {
			err = it.Err()
		}
		_ = it.Close()
	}
	out["Iterate"] = err

	_, err = store.(spi.GroupedAggregator).GroupedAggregate(ctx, ref,
		[]spi.GroupExpr{{Kind: spi.GroupExprState}},
		filter,
		spi.GroupedAggregationsOptions{MaxBuckets: 10},
	)
	out["GroupedAggregate"] = err

	return out
}

// TestFilterPathValidation_RejectsMalformedPath: sqlite and postgres both run
// validateFilterPaths at the Search / Iterate / GroupedAggregate boundary, so
// a malformed or injection-shaped Filter.Path is ErrInvalidFilterPath there.
// Memory had no filter-path validator at all: the path was handed straight to
// the spi.Prepare kernel, which resolves it with gjson and simply finds
// nothing — the query returned an empty page instead of an error. That is a
// wrong-but-available answer, which the correctness-over-availability rule
// forbids, and it is the same input answered differently per backend.
//
// The shapes below are exactly the ones the one filter-path grammar
// (spi.ValidateFilterPath) rejects. "$.v" is included deliberately: the SPI
// contract for Filter.Path is a BARE dotted identifier — spi.ConditionToFilter
// strips the "$." prefix via stripDollarDot before a filter ever reaches a
// plugin, and the match kernel
// (prepared_filter.go: gjson.GetBytes(data, n.path)) does no stripping of its
// own, so "$.v" never matched anything on memory either. Memory's gjsonPath
// "$." strip applies to GroupExpr.Path / AggregateExpr.Field only, never to
// filter paths.
func TestFilterPathValidation_RejectsMalformedPath(t *testing.T) {
	store, ref, ctx := filterPathStore(t, "tenant-filterpath-bad", "m-filterpath-bad")

	for _, path := range []string{
		"foo';x",   // injection-shaped: quote + statement separator
		"a..b",     // empty segment
		"a.",       // trailing dot
		".a",       // leading dot
		"a b",      // whitespace
		"a\"b",     // double quote
		"a/b",      // slash
		"a*b",      // star
		"a[-1]",    // negative index — outside the one grammar
		"a[",       // unclosed bracket
		"a[0]b",    // trailing character after a well-formed subscript
		"$.v",      // "$."-prefixed: not the SPI's bare-path contract
		"a\\b",     // backslash
		"a\x00b",   // control byte
		"a;DROP--", // semicolon
	} {
		for _, src := range []spi.FieldSource{spi.SourceData, spi.SourceMeta} {
			filter := spi.Filter{Op: spi.FilterEq, Path: path, Source: src, Value: "x"}
			for name, err := range runAllFilterEntryPoints(t, store, ref, ctx, filter) {
				if !errors.Is(err, memory.ErrInvalidFilterPath) {
					t.Errorf("%s with %s filter path %q = %v, want ErrInvalidFilterPath", name, src, path, err)
				}
			}
		}
	}
}

// TestFilterPathValidation_RejectsMalformedPathInTree: validateFilterPaths on
// sqlite and postgres recurses through FilterAnd / FilterOr children, so a bad
// path nested under a group is rejected just the same. Memory must too — the
// tree walk is where a hand-built filter is most likely to hide one.
func TestFilterPathValidation_RejectsMalformedPathInTree(t *testing.T) {
	store, ref, ctx := filterPathStore(t, "tenant-filterpath-tree", "m-filterpath-tree")

	nested := spi.Filter{Op: spi.FilterAnd, Children: []spi.Filter{
		{Op: spi.FilterEq, Path: "v", Source: spi.SourceData, Value: 1},
		{Op: spi.FilterOr, Children: []spi.Filter{
			{Op: spi.FilterEq, Path: "a.b", Source: spi.SourceData, Value: 2},
			{Op: spi.FilterEq, Path: "bad';x", Source: spi.SourceData, Value: 3},
		}},
	}}

	for name, err := range runAllFilterEntryPoints(t, store, ref, ctx, nested) {
		if !errors.Is(err, memory.ErrInvalidFilterPath) {
			t.Errorf("%s with malformed path nested under and/or = %v, want ErrInvalidFilterPath", name, err)
		}
	}
}

// TestFilterPathValidation_AcceptsContractShapes is the regression half, and
// it matters more than the rejection half: every path shape that is legitimate
// under the SPI contract must keep working at every entry point. That covers
// the bare dotted-identifier grammar (letters, digits, underscore, hyphen,
// dots), the full canonical meta filter vocabulary spi.extractFilterMetaValue
// resolves (both the storage-key and the client-name spellings), the
// zero-value match-all filter, and a path-less leaf such as a tree operator.
func TestFilterPathValidation_AcceptsContractShapes(t *testing.T) {
	store, ref, ctx := filterPathStore(t, "tenant-filterpath-ok", "m-filterpath-ok")

	dataPaths := []string{
		"v", "a.b", "a-b", "a_b", "A1", "a.b.c", "x-y_z9.q",
		// Array subscripts: a positional index, a wildcard, and a field
		// literally named "0" are three different addresses the one grammar
		// admits (docs/cloud-parity/path-grammar.md section 9); none of them
		// is malformed. The filter matches nothing on the seeded entity
		// (which has no such array/field), which is fine — this test only
		// asserts the validator does not reject the shape.
		"tags[0]", "tags[*]", "items[*].sku", "obj.0", "m[0][1]",
	}
	for _, path := range dataPaths {
		filter := spi.Filter{Op: spi.FilterEq, Path: path, Source: spi.SourceData, Value: "x"}
		for name, err := range runAllFilterEntryPoints(t, store, ref, ctx, filter) {
			if err != nil {
				t.Errorf("%s with valid data filter path %q: unexpected error %v", name, path, err)
			}
		}
	}

	// The complete meta vocabulary the shared kernel resolves — storage keys
	// and canonical client names alike. sqlite/postgres apply the SAME
	// dotted-identifier grammar to meta filter paths (validateFilterPaths does
	// not branch on Source, unlike validateOrderSpecs), so none of these may
	// be rejected and none needs an allowlist.
	metaPaths := []string{
		"entity_id", "state", "version", "created_at", "updated_at",
		"model_name", "model_version", "change_type", "transaction_id",
		"id", "creationDate", "lastUpdateTime", "transitionForLatestSave", "transactionId",
	}
	for _, path := range metaPaths {
		filter := spi.Filter{Op: spi.FilterEq, Path: path, Source: spi.SourceMeta, Value: "x"}
		for name, err := range runAllFilterEntryPoints(t, store, ref, ctx, filter) {
			if err != nil {
				t.Errorf("%s with valid meta filter path %q: unexpected error %v", name, path, err)
			}
		}
	}

	// Zero-value filter: the "no filter" match-all convention.
	for name, err := range runAllFilterEntryPoints(t, store, ref, ctx, spi.Filter{}) {
		if err != nil {
			t.Errorf("%s with zero-value (match-all) filter: unexpected error %v", name, err)
		}
	}

	// A leaf with no path at all is skipped by the validator, exactly as on
	// sqlite/postgres (`if f.Path == "" { return nil }`).
	for name, err := range runAllFilterEntryPoints(t, store, ref, ctx, spi.Filter{Op: spi.FilterIsNull}) {
		if err != nil {
			t.Errorf("%s with path-less leaf: unexpected error %v", name, err)
		}
	}

	// An empty and/or group must stay a tautology, not become a validation error.
	for name, err := range runAllFilterEntryPoints(t, store, ref, ctx, spi.Filter{Op: spi.FilterAnd}) {
		if err != nil {
			t.Errorf("%s with empty AND group: unexpected error %v", name, err)
		}
	}
}

// TestFilterPathValidation_ValidFilterStillMatches: rejecting bad paths must
// not disturb what a good filter actually returns. Guards against a validator
// wired in so aggressively that it swallows the query.
func TestFilterPathValidation_ValidFilterStillMatches(t *testing.T) {
	store, ref, ctx := filterPathStore(t, "tenant-filterpath-match", "m-filterpath-match")

	filter := spi.Filter{
		Op:       spi.FilterEq,
		Path:     "a.b",
		Source:   spi.SourceData,
		Value:    2,
		Declared: []spi.DataType{spi.Integer},
	}

	got, err := store.(spi.Searcher).Search(ctx, filter, spi.SearchOptions{
		ModelName:    ref.EntityName,
		ModelVersion: ref.ModelVersion,
		Limit:        10,
	})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(got) != 1 || got[0].Meta.ID != "e1" {
		t.Fatalf("Search returned %d rows, want the seeded entity", len(got))
	}

	it, err := store.(spi.Iterable).Iterate(ctx, ref, filter, spi.IterateOptions{})
	if err != nil {
		t.Fatalf("Iterate: %v", err)
	}
	n := 0
	for it.Next() {
		n++
	}
	if err := it.Err(); err != nil {
		t.Fatalf("Iterate drain: %v", err)
	}
	_ = it.Close()
	if n != 1 {
		t.Fatalf("Iterate yielded %d rows, want 1", n)
	}

	buckets, err := store.(spi.GroupedAggregator).GroupedAggregate(ctx, ref,
		[]spi.GroupExpr{{Kind: spi.GroupExprState}},
		filter,
		spi.GroupedAggregationsOptions{MaxBuckets: 10},
	)
	if err != nil {
		t.Fatalf("GroupedAggregate: %v", err)
	}
	if len(buckets) != 1 || buckets[0].Count != 1 {
		t.Fatalf("GroupedAggregate = %+v, want one bucket with count 1", buckets)
	}
}

// TestOrderSpecValidation_RejectsUnknownMetaPath: postgres and sqlite reject
// an OrderSpec whose SourceMeta path is outside the canonical meta vocabulary
// with ErrInvalidFilterPath. Memory passed it straight to spi.LessByOrder,
// where an unknown meta path resolves to "missing" for every entity and
// silently degrades to the entity-id tiebreaker — the same input answered
// differently depending on the backend.
func TestOrderSpecValidation_RejectsUnknownMetaPath(t *testing.T) {
	f := memory.NewStoreFactory()
	t.Cleanup(func() { _ = f.Close() })

	ctx := txIndexCtx("tenant-orderpath")
	store, err := f.EntityStore(ctx)
	if err != nil {
		t.Fatalf("EntityStore: %v", err)
	}
	ref := spi.ModelRef{EntityName: "m-orderpath", ModelVersion: "1"}
	if _, err := store.Save(ctx, &spi.Entity{
		Meta: spi.EntityMeta{ID: "e1", ModelRef: ref, State: "NEW"},
		Data: []byte(`{"v":1}`),
	}); err != nil {
		t.Fatalf("Save: %v", err)
	}

	bad := []spi.OrderSpec{{Source: spi.SourceMeta, Path: "notAMetaField"}}

	searcher, ok := store.(spi.Searcher)
	if !ok {
		t.Fatal("EntityStore does not implement spi.Searcher")
	}
	_, err = searcher.Search(ctx, spi.Filter{}, spi.SearchOptions{
		ModelName:    ref.EntityName,
		ModelVersion: ref.ModelVersion,
		Limit:        10,
		OrderBy:      bad,
	})
	if !errors.Is(err, memory.ErrInvalidFilterPath) {
		t.Errorf("Search with unknown meta sort path = %v, want ErrInvalidFilterPath", err)
	}

	iterable, ok := store.(spi.Iterable)
	if !ok {
		t.Fatal("EntityStore does not implement spi.Iterable")
	}
	it, err := iterable.Iterate(ctx, ref, spi.Filter{}, spi.IterateOptions{OrderBy: bad})
	if it != nil {
		_ = it.Close()
	}
	if !errors.Is(err, memory.ErrInvalidFilterPath) {
		t.Errorf("Iterate with unknown meta sort path = %v, want ErrInvalidFilterPath", err)
	}
}

// TestOrderSpecValidation_AcceptsCanonicalMetaPaths: the allowlist must cover
// exactly the vocabulary spi.LessByOrder resolves, so a valid sort is never
// rejected.
func TestOrderSpecValidation_AcceptsCanonicalMetaPaths(t *testing.T) {
	f := memory.NewStoreFactory()
	t.Cleanup(func() { _ = f.Close() })

	ctx := txIndexCtx("tenant-orderpath-ok")
	store, err := f.EntityStore(ctx)
	if err != nil {
		t.Fatalf("EntityStore: %v", err)
	}
	ref := spi.ModelRef{EntityName: "m-orderpath-ok", ModelVersion: "1"}
	if _, err := store.Save(ctx, &spi.Entity{
		Meta: spi.EntityMeta{ID: "e1", ModelRef: ref, State: "NEW"},
		Data: []byte(`{"v":1}`),
	}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	searcher := store.(spi.Searcher)

	for _, path := range []string{"id", "state", "creationDate", "lastUpdateTime", "transitionForLatestSave", "transactionId"} {
		_, err := searcher.Search(ctx, spi.Filter{}, spi.SearchOptions{
			ModelName:    ref.EntityName,
			ModelVersion: ref.ModelVersion,
			Limit:        10,
			OrderBy:      []spi.OrderSpec{{Source: spi.SourceMeta, Path: path}},
		})
		if err != nil {
			t.Errorf("Search ordered by meta %q: unexpected error %v", path, err)
		}
	}
	// A plain data path is fine too.
	if _, err := searcher.Search(ctx, spi.Filter{}, spi.SearchOptions{
		ModelName:    ref.EntityName,
		ModelVersion: ref.ModelVersion,
		Limit:        10,
		OrderBy:      []spi.OrderSpec{{Source: spi.SourceData, Path: "v"}},
	}); err != nil {
		t.Errorf("Search ordered by data path: unexpected error %v", err)
	}
}

// TestOrderSpecValidation_RejectsMalformedDataPath: sqlite and postgres apply
// their dotted-identifier grammar to SourceData order paths as well, so a
// malformed path is ErrInvalidFilterPath there. Memory must classify it the
// same way rather than sorting everything as "missing".
func TestOrderSpecValidation_RejectsMalformedDataPath(t *testing.T) {
	f := memory.NewStoreFactory()
	t.Cleanup(func() { _ = f.Close() })

	ctx := txIndexCtx("tenant-orderpath-bad")
	store, err := f.EntityStore(ctx)
	if err != nil {
		t.Fatalf("EntityStore: %v", err)
	}
	ref := spi.ModelRef{EntityName: "m-orderpath-bad", ModelVersion: "1"}
	searcher := store.(spi.Searcher)

	for _, path := range []string{"a..b", "a.", "a b", "a'b"} {
		_, err := searcher.Search(ctx, spi.Filter{}, spi.SearchOptions{
			ModelName:    ref.EntityName,
			ModelVersion: ref.ModelVersion,
			Limit:        10,
			OrderBy:      []spi.OrderSpec{{Source: spi.SourceData, Path: path}},
		})
		if !errors.Is(err, memory.ErrInvalidFilterPath) {
			t.Errorf("Search ordered by malformed data path %q = %v, want ErrInvalidFilterPath", path, err)
		}
	}
}

// groupPathRejects are the shapes GroupExpr.Path / AggregateExpr.Field
// reject. Reused by the GroupExpr.Path and AggregateExpr.Field tests below —
// sqlite and postgres run the SAME checks over both fields
// (validateGroupAndAggregatePaths / groupExprToSQL / aggregateExprToSQL), so
// the two must reject identically.
//
// "a[0]" is NOT here: it is a legal positional array subscript under the one
// filter-path grammar (spi.ValidateFilterPath) — see groupPathAcceptsSubscript
// in TestGroupExprAndAggregateFieldValidation_AcceptsWellFormed. "" IS still
// here even though validateJSONPath alone now admits it: a group-by path or
// aggregate field always names a real field, so grouped_stats.go rejects ""
// explicitly before delegating, the same reasoning
// validateGroupAndAggregatePaths documents on sqlite/postgres.
var groupPathRejects = []string{
	"foo';x",   // injection-shaped: quote + statement separator
	"a..b",     // empty segment
	"a.",       // trailing dot
	".a",       // leading dot
	"a b",      // whitespace
	"a\"b",     // double quote
	"a/b",      // slash
	"a*b",      // star
	"a[-1]",    // negative index — outside the one grammar
	"a[",       // unclosed bracket
	"$.v",      // "$."-prefixed: not the SPI's bare-path contract
	"a\\b",     // backslash
	"a\x00b",   // control byte
	"a;DROP--", // semicolon
	"",         // empty — always rejected: a group/aggregate path names a real field
}

// TestGroupExprPathValidation_RejectsMalformedPath: sqlite (grouped_stats.go
// groupExprToSQL) and postgres (same function) both run validateJSONPath on
// GroupExpr.Path and surface ErrInvalidFilterPath from GroupedAggregate.
// Memory handed the path to gjson via gjsonPath instead, where a malformed
// path simply does not resolve — every entity fell into the nil bucket and the
// caller got a plausible-looking answer to a question it never asked. That is
// a wrong-but-available result (correctness-over-availability) and the same
// input answered differently per backend.
//
// "$.v" is in the reject set on purpose: the service layer strips the "$."
// (grouped_stats_service.go translateGroupBy) before any plugin is called, and
// re-decorates the wire path from the request afterwards
// (restoreJSONPathPrefix), so a "$."-prefixed path is a shape production never
// sends. Memory's gjsonPath tolerance for it was dead weight that only served
// to hide this divergence.
func TestGroupExprPathValidation_RejectsMalformedPath(t *testing.T) {
	store, ref, ctx := filterPathStore(t, "tenant-grouppath-bad", "m-grouppath-bad")
	ga := store.(spi.GroupedAggregator)

	for _, path := range groupPathRejects {
		_, err := ga.GroupedAggregate(ctx, ref,
			[]spi.GroupExpr{{Kind: spi.GroupExprDataPath, Path: path}},
			spi.Filter{},
			spi.GroupedAggregationsOptions{MaxBuckets: 10},
		)
		if !errors.Is(err, memory.ErrInvalidFilterPath) {
			t.Errorf("GroupedAggregate grouped by malformed path %q = %v, want ErrInvalidFilterPath", path, err)
		}
	}
}

// TestAggregateExprFieldValidation_RejectsMalformedField: the aggregation
// field goes through the same validator on sqlite and postgres
// (aggregateExprToSQL). Memory read it with gjson in memBucket.observe, where
// a malformed field never resolves — the aggregation silently reported the
// zero/nil result for every bucket.
func TestAggregateExprFieldValidation_RejectsMalformedField(t *testing.T) {
	store, ref, ctx := filterPathStore(t, "tenant-aggfield-bad", "m-aggfield-bad")
	ga := store.(spi.GroupedAggregator)

	for _, field := range groupPathRejects {
		_, err := ga.GroupedAggregate(ctx, ref,
			[]spi.GroupExpr{{Kind: spi.GroupExprState}},
			spi.Filter{},
			spi.GroupedAggregationsOptions{
				MaxBuckets:   10,
				Aggregations: []spi.AggregateExpr{{Op: spi.AggSum, Field: field, Alias: "s"}},
			},
		)
		if !errors.Is(err, memory.ErrInvalidFilterPath) {
			t.Errorf("GroupedAggregate aggregating malformed field %q = %v, want ErrInvalidFilterPath", field, err)
		}
	}
}

// TestGroupExprAndAggregateFieldValidation_AcceptsWellFormed is the positive
// control for the two tests above: a validator that rejected everything would
// satisfy them. Every path here is grammar-valid and present in
// filterPathStore's seeded entity, so the group key and the sum must both come
// back populated. GroupExprState carries no path and must stay exempt.
func TestGroupExprAndAggregateFieldValidation_AcceptsWellFormed(t *testing.T) {
	store, ref, ctx := filterPathStore(t, "tenant-grouppath-ok", "m-grouppath-ok")
	ga := store.(spi.GroupedAggregator)

	for _, path := range []string{"v", "a.b", "a-b", "a_b"} {
		buckets, err := ga.GroupedAggregate(ctx, ref,
			[]spi.GroupExpr{{Kind: spi.GroupExprDataPath, Path: path}},
			spi.Filter{},
			spi.GroupedAggregationsOptions{
				MaxBuckets:   10,
				Aggregations: []spi.AggregateExpr{{Op: spi.AggSum, Field: path, Alias: "s"}},
			},
		)
		if err != nil {
			t.Errorf("GroupedAggregate with well-formed path %q = %v, want success", path, err)
			continue
		}
		if len(buckets) != 1 {
			t.Errorf("path %q: got %d buckets, want 1", path, len(buckets))
			continue
		}
		if got := buckets[0].GroupKey[0].Path; got != path {
			t.Errorf("path %q: group key path = %q, want the path verbatim", path, got)
		}
		if buckets[0].GroupKey[0].Value == nil {
			t.Errorf("path %q: group key value is nil; a well-formed path over the seeded entity must resolve", path)
		}
		if buckets[0].Aggregations["s"] == nil {
			t.Errorf("path %q: sum is nil; a well-formed field over the seeded entity must resolve", path)
		}
	}

	// GroupExprState carries no Path and must not be held to the grammar.
	if _, err := ga.GroupedAggregate(ctx, ref,
		[]spi.GroupExpr{{Kind: spi.GroupExprState}},
		spi.Filter{},
		spi.GroupedAggregationsOptions{MaxBuckets: 10},
	); err != nil {
		t.Errorf("GroupedAggregate grouped by state = %v, want success", err)
	}
}

// TestGroupExprAndAggregateFieldValidation_AcceptsSubscripts checks that a
// "[N]" or "[*]" array subscript in a GroupExpr.Path / AggregateExpr.Field is
// grammar-valid and does not trip ErrInvalidFilterPath, mirroring the filter
// path coverage in TestFilterPathValidation_AcceptsContractShapes. The seeded
// entity (filterPathStore) has no array fields, so — unlike
// TestGroupExprAndAggregateFieldValidation_AcceptsWellFormed — this only
// asserts the validator does not reject the shape, not that a value resolves.
func TestGroupExprAndAggregateFieldValidation_AcceptsSubscripts(t *testing.T) {
	store, ref, ctx := filterPathStore(t, "tenant-grouppath-subscript", "m-grouppath-subscript")
	ga := store.(spi.GroupedAggregator)

	for _, path := range []string{"tags[0]", "tags[*]", "items[*].sku", "obj.0"} {
		if _, err := ga.GroupedAggregate(ctx, ref,
			[]spi.GroupExpr{{Kind: spi.GroupExprDataPath, Path: path}},
			spi.Filter{},
			spi.GroupedAggregationsOptions{
				MaxBuckets:   10,
				Aggregations: []spi.AggregateExpr{{Op: spi.AggSum, Field: path, Alias: "s"}},
			},
		); err != nil {
			t.Errorf("GroupedAggregate with subscript path %q = %v, want success", path, err)
		}
	}
}
