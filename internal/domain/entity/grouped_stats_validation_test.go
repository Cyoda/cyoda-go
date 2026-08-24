package entity_test

import (
	"errors"
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
		{"stray bracket quote", entity.GroupedStatsRequest{GroupBy: []string{"']"}}, 10000, "INVALID_GROUP_BY_PATH"},
		{"array projection", entity.GroupedStatsRequest{GroupBy: []string{"$.items[*]"}}, 10000, "INVALID_GROUP_BY_PATH"},
		{"positional index", entity.GroupedStatsRequest{GroupBy: []string{"$.items[0]"}}, 10000, "INVALID_GROUP_BY_PATH"},
		// Bracket-quoted property access is not the model's syntax. It used to
		// be folded into dotted form here; it is now rejected, matching the
		// condition surface so one request cannot be accepted in groupBy and
		// 400'd in condition for the same spelling.
		{"bracket quoted rejected",
			entity.GroupedStatsRequest{GroupBy: []string{"$.['variantId']"}}, 10000, "INVALID_GROUP_BY_PATH"},
		// A bare identifier is not a JSON Path. "variantId" names nothing the
		// grammar can address, so it errors rather than being read as "$.variantId".
		{"bare identifier rejected",
			entity.GroupedStatsRequest{GroupBy: []string{"variantId"}}, 10000, "INVALID_GROUP_BY_PATH"},
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

// TestValidateGroupedStatsRequest_PathGrammar_Rejects pins the boundary
// grammar for groupBy paths and aggregation fields.
//
// A malformed path is otherwise caught only when pushdown is actually used:
// every backend refuses it there (all three plugins validate group/aggregate
// paths against the SPI dotted-identifier grammar), but the service falls
// through to the in-process streaming tally whenever pushdown is declined (a
// residual filter, a point-in-time query, sqlite declining stdev). There the
// path is resolved with gjson, misses, and every entity buckets as null — a
// 200 with plausible-looking-but-wrong groups instead of an error.
//
// The grammar is the wire jsonPath grammar, shared with the condition surface
// via search.ValidateScalarJSONPath:
//
//	jsonPath = "$." segment ( "." segment )*
//	segment  = 1*( ALPHA / DIGIT / "_" / "-" )   ; ASCII only
//
// The "$." leader is REQUIRED (a bare identifier is not a JSON Path),
// bracket-quoted property access is not the model's syntax, and an array
// subscript cannot denote the single scalar a group key or aggregand needs.
func TestValidateGroupedStatsRequest_PathGrammar_Rejects(t *testing.T) {
	bad := []struct {
		name string
		path string
	}{
		// Not JSON Path at all — no "$." leader.
		{"bare identifier", "foo"},
		{"bare dotted", "foo.bar"},
		{"bare numeric segment", "items.0"},
		{"single quote", "foo';x"},
		{"drop table", "; DROP TABLE entities;"},
		{"leading dot", ".foo"},
		{"trailing dot", "foo."},
		{"empty segment", "foo..bar"},
		// Leader present, remainder outside the grammar.
		{"sql comment tail", "$.x'; --"},
		{"double quote", `$.foo"bar`},
		{"backslash", `$.foo\bar`},
		{"space", "$.foo bar"},
		{"tab", "$.foo\tbar"},
		{"newline", "$.foo\nbar"},
		{"nul byte", "$.foo\x00"},
		{"slash", "$.foo/bar"},
		{"asterisk", "$.foo*"},
		{"non-ascii", "$.café"},
		{"bare dollar", "$"},
		{"dollar dot only", "$."},
		{"leader only trailing dot", "$.foo."},
		{"recursive descent", "$..foo"},
		{"embedded dollar", "$.foo$bar"},
		{"filter expression", "$.foo?(@.x)"},
		{"colon", "$.foo:bar"},
		{"at sign", "$.@"},
		// Bracket-quoted property access — rejected, not folded to dotted form.
		{"bracket quoted", "$['x']"},
		{"bracket quoted after leader", "$.['variantId']"},
		{"bracket chain", "$['x']['y']"},
		{"bracket empty name", "$.['']"},
		{"stray bracket quote", "']"},
		// Array subscripts: valid JSON Path, but no single scalar to group on.
		{"array projection", "$.items[*]"},
		{"positional index", "$.items[0]"},
	}
	for _, tc := range bad {
		t.Run("groupBy/"+tc.name, func(t *testing.T) {
			_, err := entity.ValidateGroupedStatsRequest(
				entity.GroupedStatsRequest{GroupBy: []string{tc.path}}, 10000)
			assertValidationCode(t, err, "INVALID_GROUP_BY_PATH")
		})
		t.Run("aggField/"+tc.name, func(t *testing.T) {
			_, err := entity.ValidateGroupedStatsRequest(entity.GroupedStatsRequest{
				GroupBy:      []string{"state"},
				Aggregations: []entity.AggregationExprWire{{Op: "sum", Field: tc.path}},
			}, 10000)
			assertValidationCode(t, err, "INVALID_AGGREGATION_FIELD")
		})
	}
}

// TestValidateGroupedStatsRequest_PathGrammar_Accepts is the positive control
// for the tightening above: every shape the public surface documents must
// still be accepted and must still normalize to the same canonical form.
// A tightening that breaks valid callers is worse than the bug it fixes.
func TestValidateGroupedStatsRequest_PathGrammar_Accepts(t *testing.T) {
	good := []struct {
		name string
		path string
	}{
		{"single segment", "$.foo"},
		{"dotted", "$.foo.bar.baz"},
		{"underscore", "$.foo_bar"},
		{"hyphen", "$.foo-bar"},
		{"digits", "$.a1.2b"},
		{"numeric segment", "$.items.0"},
		{"uppercase", "$.FooBar"},
		{"state as data path", "$.state"},
		{"meta-looking data path", "$._meta.state"},
	}
	for _, tc := range good {
		t.Run("groupBy/"+tc.name, func(t *testing.T) {
			out, err := entity.ValidateGroupedStatsRequest(
				entity.GroupedStatsRequest{GroupBy: []string{tc.path}}, 10000)
			if err != nil {
				t.Fatalf("path %q rejected: %v", tc.path, err)
			}
			g := out.GroupBy[0]
			if g.IsState {
				t.Fatalf("path %q: unexpectedly treated as the state token", tc.path)
			}
			// A validated path is returned verbatim — the boundary validates,
			// it does not rewrite, so the response group-key echoes what the
			// request sent.
			if g.Path != tc.path {
				t.Fatalf("path %q came back as %q; the validator must not rewrite", tc.path, g.Path)
			}
		})
		t.Run("aggField/"+tc.name, func(t *testing.T) {
			out, err := entity.ValidateGroupedStatsRequest(entity.GroupedStatsRequest{
				GroupBy:      []string{"state"},
				Aggregations: []entity.AggregationExprWire{{Op: "sum", Field: tc.path}},
			}, 10000)
			if err != nil {
				t.Fatalf("field %q rejected: %v", tc.path, err)
			}
			if out.Aggregations[0].Field != tc.path {
				t.Fatalf("field %q came back as %q; the validator must not rewrite",
					tc.path, out.Aggregations[0].Field)
			}
		})
	}
}

// TestValidateGroupedStatsRequest_StateIsATokenNotAPath pins the one
// exemption from the leader rule and its exact scope. "state" buckets by the
// entity's lifecycle state; it is a token in the groupBy list, so it needs no
// "$." leader there. It is NOT a token anywhere else: as an aggregation field
// it is just an identifier missing its leader, and there is no defined
// aggregate over a lifecycle state — so it is rejected. "$.state" remains an
// ordinary data path on both surfaces.
func TestValidateGroupedStatsRequest_StateIsATokenNotAPath(t *testing.T) {
	out, err := entity.ValidateGroupedStatsRequest(
		entity.GroupedStatsRequest{GroupBy: []string{"state"}}, 10000)
	if err != nil {
		t.Fatalf("groupBy state token rejected: %v", err)
	}
	if !out.GroupBy[0].IsState {
		t.Fatalf("groupBy \"state\" produced Path=%q, want the reserved state token", out.GroupBy[0].Path)
	}

	_, err = entity.ValidateGroupedStatsRequest(entity.GroupedStatsRequest{
		GroupBy:      []string{"state"},
		Aggregations: []entity.AggregationExprWire{{Op: "sum", Field: "state"}},
	}, 10000)
	assertValidationCode(t, err, "INVALID_AGGREGATION_FIELD")
}

func assertValidationCode(t *testing.T, err error, want string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected error %s, got nil", want)
	}
	var ve *entity.GroupedStatsValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("expected GroupedStatsValidationError, got %T: %v", err, err)
	}
	if ve.Code != want {
		t.Fatalf("got code %s, want %s", ve.Code, want)
	}
}

// TestValidateGroupedStatsRequest_SynthesizedAliasStripsJSONPathPrefix pins
// the contract that synthesized response-object aliases do NOT embed the
// `$.` JSONPath leader. Pre-fix, a `sum` over `$.amount` with no explicit
// `as` produced `sum_$.amount` — ugly and breaks dotted-property access in
// clients. The fix strips `$.` for the alias only; the validated Field
// keeps it because downstream gjson extraction relies on the prefix.
func TestValidateGroupedStatsRequest_SynthesizedAliasStripsJSONPathPrefix(t *testing.T) {
	in := entity.GroupedStatsRequest{
		GroupBy: []string{"state"},
		Aggregations: []entity.AggregationExprWire{
			{Op: "sum", Field: "$.amount"},                   // synthesized
			{Op: "avg", Field: "$.nested.price"},             // synthesized, multi-segment
			{Op: "min", Field: "$.amount", As: "min_amount"}, // explicit alias unchanged
			{Op: "max", Field: "$.qty"},                      // synthesized, single segment
		},
	}
	out, err := entity.ValidateGroupedStatsRequest(in, 10000)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(out.Aggregations) != 4 {
		t.Fatalf("got %d aggregations, want 4", len(out.Aggregations))
	}
	// Aliases in order, with $. stripped from synthesized ones only.
	want := []string{"sum_amount", "avg_nested.price", "min_amount", "max_qty"}
	for i, w := range want {
		if out.Aggregations[i].Alias != w {
			t.Errorf("aggregations[%d].Alias = %q, want %q", i, out.Aggregations[i].Alias, w)
		}
	}
	// Fields keep $. — gjson extraction depends on the canonical form.
	wantField := []string{"$.amount", "$.nested.price", "$.amount", "$.qty"}
	for i, w := range wantField {
		if out.Aggregations[i].Field != w {
			t.Errorf("aggregations[%d].Field = %q, want %q", i, out.Aggregations[i].Field, w)
		}
	}
}
