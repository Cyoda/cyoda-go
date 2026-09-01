package memory

import (
	"errors"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// TestValidateJSONPath_MatchesSPIGrammar checks the unexported validator
// directly against the one SPI filter-path grammar
// (spi.ValidateFilterPath, docs/cloud-parity/path-grammar.md section 9): a
// bracketed wildcard or non-negative index is a legitimate array subscript,
// and every rejection the grammar states stays rejected here too.
func TestValidateJSONPath_MatchesSPIGrammar(t *testing.T) {
	for _, p := range []string{"tags[0]", "tags[*]", "items[*].sku", "obj.0"} {
		if err := validateJSONPath(p); err != nil {
			t.Errorf("validateJSONPath(%q): unexpected error %v", p, err)
		}
	}
	for _, p := range []string{"a'b", "a;DROP", "a[-1]", "a["} {
		if err := validateJSONPath(p); err == nil {
			t.Errorf("validateJSONPath(%q): want rejection", p)
		}
	}
}

// TestRejectSubscript_ParseFailureRejects pins rejectSubscript's default on
// a path spi.ParseFilterPath cannot parse. Every call site runs
// validateJSONPath first, so this is unreachable in practice with a
// well-formed caller — but the DEFAULT direction still matters: it was
// nil (accept), the permissive choice, which .claude/rules/correctness-over-
// availability.md forbids for a dependency (here, a successful parse) a
// correct answer requires. Flipped to reject, matching a defensively-reached
// caller that skips validateJSONPath the same way every other path fault in
// this function is handled.
func TestRejectSubscript_ParseFailureRejects(t *testing.T) {
	for _, p := range []string{"a[", "a]", "a[-1]", "a[?(@.x)]"} {
		if err := rejectSubscript(p, "sort path"); err == nil {
			t.Errorf("rejectSubscript(%q): want rejection on a path that fails to parse, got nil", p)
		}
	}
}

// TestValidateFilterPaths_RecursesOnAnyNodeWithChildren pins the defect this
// package's validateFilterPaths used to have: it recursed on a case list of
// named branch operators (FilterAnd, FilterOr) rather than on the PRESENCE
// of a subtree. A node with an unrecognised Op and populated Children fell
// through to the "f.Path == """ check and returned nil without ever
// inspecting Children — its subtree went unvalidated.
//
// FilterNot does not exist on this SPI version yet — a later task adds it —
// so spi.FilterOp("not") stands in for exactly the shape a NOT node will
// present to this validator until it is rebuilt against the SPI version
// that defines FilterNot: an unrecognised Op carrying Children. A validator
// that still switched on named operators would treat this node as a
// pathless leaf and never look inside it, letting a malformed path reach
// spi.Prepare unchecked; there it becomes a never-match leaf, and a NOT
// wrapping it would invert that into matching every entity instead of being
// rejected outright.
func TestValidateFilterPaths_RecursesOnAnyNodeWithChildren(t *testing.T) {
	malformed := spi.Filter{Op: spi.FilterEq, Path: "a[", Source: spi.SourceData, Value: "x"}
	wellFormed := spi.Filter{Op: spi.FilterEq, Path: "a.b", Source: spi.SourceData, Value: "x"}

	// The bug: a malformed path nested under an operator this validator
	// does not recognise must still be caught.
	unknownOpMalformed := spi.Filter{Op: spi.FilterOp("not"), Children: []spi.Filter{malformed}}
	if err := validateFilterPaths(unknownOpMalformed); !errors.Is(err, spi.ErrInvalidFilterPath) {
		t.Errorf("malformed path nested under unrecognised op %q: err = %v, want wraps spi.ErrInvalidFilterPath", unknownOpMalformed.Op, err)
	}

	// Regression guard: the existing recognised-operator behaviour must not
	// have broken. A malformed path nested under a genuine FilterAnd is
	// still rejected.
	andMalformed := spi.Filter{Op: spi.FilterAnd, Children: []spi.Filter{wellFormed, malformed}}
	if err := validateFilterPaths(andMalformed); !errors.Is(err, spi.ErrInvalidFilterPath) {
		t.Errorf("malformed path nested under FilterAnd: err = %v, want wraps spi.ErrInvalidFilterPath", err)
	}

	// Regression guard: a well-formed path nested under either kind of
	// branch node is still accepted — the fix must not start rejecting
	// valid filters.
	andWellFormed := spi.Filter{Op: spi.FilterAnd, Children: []spi.Filter{wellFormed}}
	if err := validateFilterPaths(andWellFormed); err != nil {
		t.Errorf("well-formed path nested under FilterAnd: err = %v, want nil", err)
	}
	unknownOpWellFormed := spi.Filter{Op: spi.FilterOp("not"), Children: []spi.Filter{wellFormed}}
	if err := validateFilterPaths(unknownOpWellFormed); err != nil {
		t.Errorf("well-formed path nested under unrecognised op %q: err = %v, want nil", unknownOpWellFormed.Op, err)
	}
}
