package sqlite

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

// TestValidateJSONPath_AcceptsEmpty documents a deliberate behaviour change
// from the pre-SPI-grammar validator, which used to reject "" outright.
// docs/cloud-parity/path-grammar.md section 9 states the empty filter path
// is legal — the tree operators (AND/OR) carry one instead of a leaf
// condition — and spi.ValidateFilterPath (the one grammar this validator now
// delegates to) accepts it accordingly. This is not a new injection surface:
// validateFilterPaths, the only caller that reaches SQL interpolation, skips
// f.Path == "" before ever calling validateJSONPath.
func TestValidateJSONPath_AcceptsEmpty(t *testing.T) {
	if err := validateJSONPath(""); err != nil {
		t.Errorf("validateJSONPath(\"\") = %v, want nil (empty filter path is legal)", err)
	}
}

// TestValidateJSONPath_AcceptsHyphenatedSegments ensures field names that
// contain hyphens (e.g. "some-array", "some-object") are accepted.
// Hyphens are safe inside single-quoted SQLite JSON-path literals — they
// cannot break out of the surrounding quote and are valid JSON key characters.
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
// payloads are rejected before they can reach json_extract(...,'$.<path>').
//
// NOTE: single-hyphen and double-hyphen paths (e.g. "a-b", "a--b") are
// NOT injection vectors — hyphens are inert inside single-quoted SQLite
// string literals and are valid JSON key characters. Those paths are
// accepted (see TestValidateJSONPath_AcceptsHyphenatedSegments). Only
// characters that can break out of a single-quoted SQL literal, or that
// are structurally invalid (whitespace, empty segments, etc.) are rejected.
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
	// A well-formed non-empty path/field must still pass.
	if err := validateGroupAndAggregatePaths(
		[]spi.GroupExpr{{Kind: spi.GroupExprDataPath, Path: "tags[0]"}},
		[]spi.AggregateExpr{{Op: spi.AggSum, Field: "amount", Alias: "s"}},
	); err != nil {
		t.Errorf("well-formed group/aggregate paths: err = %v, want nil", err)
	}
}
