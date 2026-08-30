package postgres

import (
	"errors"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// TestValidateJSONPath_Accepts ensures well-formed dotted-identifier paths pass.
func TestValidateJSONPath_Accepts(t *testing.T) {
	valid := []string{
		"state",
		"city",
		"name",
		"nested.field",
		"a.b.c",
		"field_1",
		"UserID",
		"order42",
		"_private",
	}
	for _, p := range valid {
		if err := validateJSONPath(p); err != nil {
			t.Errorf("validateJSONPath(%q) returned unexpected error: %v", p, err)
		}
	}
}

// TestValidateJSONPath_AcceptsHyphenatedSegments ensures field names that
// contain hyphens (e.g. "some-array", "some-object") are accepted. Hyphens
// are safe inside single-quoted postgres JSONB key literals — they cannot
// break out of the surrounding quote.
func TestValidateJSONPath_AcceptsHyphenatedSegments(t *testing.T) {
	valid := []string{
		"some-array",
		"some-array.some-object",
		"some-array.some-object.some-key",
		"field-name",
		"a-b-c",
	}
	for _, p := range valid {
		if err := validateJSONPath(p); err != nil {
			t.Errorf("validateJSONPath(%q) returned unexpected error: %v", p, err)
		}
	}
}

// TestValidateJSONPath_RejectsInjection ensures classic SQL-injection
// payloads are rejected before they can reach doc->'a'->>'b' interpolation.
func TestValidateJSONPath_RejectsInjection(t *testing.T) {
	malicious := []string{
		// Single-quote escape — the core injection vector.
		"state')--",
		"state') UNION SELECT 1 --",
		"a'b",
		// Block-comment sequences (/* breaks the string context).
		"a/*b*/c",
		// SQL statement terminators.
		"a;b",
		";DROP TABLE entities",
		// Whitespace and control characters.
		"a b",
		"a\nb",
		"a\tb",
		// Empty segments / malformed dotting. The empty path itself is NOT
		// here: see TestValidateJSONPath_AcceptsEmpty for why.
		".",
		".foo",
		"foo.",
		"a..b",
		// Backslash / quote characters outright.
		`a"b`,
		`a\b`,
	}
	for _, p := range malicious {
		err := validateJSONPath(p)
		if err == nil {
			t.Errorf("validateJSONPath(%q) = nil, want non-nil (injection payload accepted)", p)
			continue
		}
		if !errors.Is(err, ErrInvalidFilterPath) {
			t.Errorf("validateJSONPath(%q) = %v, want wraps ErrInvalidFilterPath", p, err)
		}
	}
}

// TestValidateJSONPath_AcceptsEmpty documents a deliberate behaviour: the
// empty filter path is legal per the one grammar
// (docs/cloud-parity/path-grammar.md section 9) — the tree operators (AND/OR)
// carry one instead of a leaf condition. This is not a new injection
// surface: validateFilterPaths, the only caller that reaches SQL
// interpolation, skips f.Path == "" before ever calling validateJSONPath.
func TestValidateJSONPath_AcceptsEmpty(t *testing.T) {
	if err := validateJSONPath(""); err != nil {
		t.Errorf("validateJSONPath(\"\") = %v, want nil (empty filter path is legal)", err)
	}
}

// TestValidateJSONPath_AcceptsSubscripts checks the validator against the
// one SPI grammar (spi.ValidateFilterPath): a bracketed wildcard or
// non-negative index is a legitimate array subscript, and every rejection
// the grammar states stays rejected here too.
func TestValidateJSONPath_AcceptsSubscripts(t *testing.T) {
	for _, p := range []string{"tags[0]", "tags[*]", "items[*].sku", "obj.0", "m[0][1]"} {
		if err := validateJSONPath(p); err != nil {
			t.Errorf("validateJSONPath(%q): unexpected error %v", p, err)
		}
	}
	for _, p := range []string{"a'b", "a;DROP", "a[-1]", "a[0:2]", "a[", "a[0]b"} {
		if err := validateJSONPath(p); err == nil {
			t.Errorf("validateJSONPath(%q): want rejection", p)
		}
	}
}

// TestValidateGroupAndAggregatePaths_RejectsEmpty: unlike a filter leaf's
// Path (where "" is the legitimate "no field" shape the AND/OR tree
// operators carry), a GroupExpr.Path or AggregateExpr.Field always names a
// real field. validateJSONPath alone now admits "" (spi.ValidateFilterPath
// is right to, for a filter leaf), so validateGroupAndAggregatePaths must
// catch the empty case itself rather than silently accepting a meaningless
// "group by nothing" / "aggregate nothing" request.
func TestValidateGroupAndAggregatePaths_RejectsEmpty(t *testing.T) {
	if err := validateGroupAndAggregatePaths(
		[]spi.GroupExpr{{Kind: spi.GroupExprDataPath, Path: ""}}, nil,
	); !errors.Is(err, ErrInvalidFilterPath) {
		t.Errorf("empty GroupExpr.Path: err = %v, want ErrInvalidFilterPath", err)
	}
	if err := validateGroupAndAggregatePaths(
		nil, []spi.AggregateExpr{{Op: spi.AggSum, Field: "", Alias: "s"}},
	); !errors.Is(err, ErrInvalidFilterPath) {
		t.Errorf("empty AggregateExpr.Field: err = %v, want ErrInvalidFilterPath", err)
	}
	// GroupExprState carries no path and must stay exempt.
	if err := validateGroupAndAggregatePaths(
		[]spi.GroupExpr{{Kind: spi.GroupExprState}}, nil,
	); err != nil {
		t.Errorf("GroupExprState: err = %v, want nil", err)
	}
	// A well-formed, subscript-free non-empty path/field must still pass.
	if err := validateGroupAndAggregatePaths(
		[]spi.GroupExpr{{Kind: spi.GroupExprDataPath, Path: "amount"}},
		[]spi.AggregateExpr{{Op: spi.AggSum, Field: "amount", Alias: "s"}},
	); err != nil {
		t.Errorf("well-formed group/aggregate paths: err = %v, want nil", err)
	}
}

// TestValidateGroupAndAggregatePaths_RejectsSubscript pins
// docs/cloud-parity/path-grammar.md section 7: "An array position is
// therefore not a grouping dimension, an aggregation field or a sort key.
// Those three surfaces admit no subscript... The three surfaces that reject
// subscripts use the grammar of section 2 with the subscript production
// removed." "tags[0]" and "tags[*]" are legal FILTER paths (see
// TestValidateJSONPath_AcceptsSubscripts) but must be REJECTED here — the
// same string is legal in one position and illegal in another.
func TestValidateGroupAndAggregatePaths_RejectsSubscript(t *testing.T) {
	for _, p := range []string{"tags[0]", "tags[*]", "items[0].sku", "m[0][1]"} {
		if err := validateGroupAndAggregatePaths(
			[]spi.GroupExpr{{Kind: spi.GroupExprDataPath, Path: p}}, nil,
		); !errors.Is(err, ErrInvalidFilterPath) {
			t.Errorf("group-by path %q: err = %v, want ErrInvalidFilterPath", p, err)
		}
		if err := validateGroupAndAggregatePaths(
			nil, []spi.AggregateExpr{{Op: spi.AggSum, Field: p, Alias: "s"}},
		); !errors.Is(err, ErrInvalidFilterPath) {
			t.Errorf("aggregate field %q: err = %v, want ErrInvalidFilterPath", p, err)
		}
	}
}

// TestValidateOrderSpecs_RejectsSubscript: a SourceData sort key is a scalar
// surface too (docs/cloud-parity/path-grammar.md section 7) and must reject
// the same subscripted paths a filter accepts.
func TestValidateOrderSpecs_RejectsSubscript(t *testing.T) {
	for _, p := range []string{"tags[0]", "tags[*]"} {
		err := validateOrderSpecs([]spi.OrderSpec{{Path: p, Source: spi.SourceData}})
		if !errors.Is(err, ErrInvalidFilterPath) {
			t.Errorf("order-by path %q: err = %v, want ErrInvalidFilterPath", p, err)
		}
	}
	// Subscript-free order-by paths are unaffected.
	if err := validateOrderSpecs([]spi.OrderSpec{{Path: "amount", Source: spi.SourceData}}); err != nil {
		t.Errorf("order-by path %q: err = %v, want nil", "amount", err)
	}
}

// TestRejectSubscript_ParseFailureRejects pins rejectSubscript's default on
// a path spi.ParseFilterPath cannot parse. Every call site runs
// validateJSONPath first, so this is unreachable in practice with a
// well-formed caller — but the DEFAULT direction still matters: it was
// nil (accept), the permissive choice, which .claude/rules/correctness-over-
// availability.md forbids for a dependency (here, a successful parse) a
// correct answer requires. Flipped to reject.
func TestRejectSubscript_ParseFailureRejects(t *testing.T) {
	for _, p := range []string{"a[", "a]", "a[-1]", "a[?(@.x)]"} {
		if err := rejectSubscript(p, "sort path"); err == nil {
			t.Errorf("rejectSubscript(%q): want rejection on a path that fails to parse, got nil", p)
		}
	}
}
