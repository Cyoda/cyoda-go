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
// Two nodes exercise this, and neither subsumes the other.
// "__unknown_branch_op__" is a deliberately fictional Filter.Op — it can
// never collide with a real one, present or future — standing in for any
// operator this validator has not been taught to recognise. spi.FilterNot is
// the REAL branch operator the SPI now defines. A validator rewritten as a
// fixed case list — "case spi.FilterAnd, spi.FilterOr, spi.FilterNot", the
// natural rewrite once FilterNot exists — would pass the FilterNot case by
// name while still leaving the synthetic-op case (and any future real
// operator nobody has added a case for yet) unvalidated: the malformed path
// would reach spi.Prepare unchecked, become a never-match leaf there, and a
// NOT wrapping it would invert that into matching every entity instead of
// being rejected outright. Both assertions must stay green independently for
// the fix to be proven.
func TestValidateFilterPaths_RecursesOnAnyNodeWithChildren(t *testing.T) {
	malformed := spi.Filter{Op: spi.FilterEq, Path: "a[", Source: spi.SourceData, Value: "x"}
	wellFormed := spi.Filter{Op: spi.FilterEq, Path: "a.b", Source: spi.SourceData, Value: "x"}

	// The bug: a malformed path nested under an operator this validator has
	// never seen must still be caught.
	unknownOpMalformed := spi.Filter{Op: spi.FilterOp("__unknown_branch_op__"), Children: []spi.Filter{malformed}}
	if err := validateFilterPaths(unknownOpMalformed); !errors.Is(err, spi.ErrInvalidFilterPath) {
		t.Errorf("malformed path nested under unrecognised op %q: err = %v, want wraps spi.ErrInvalidFilterPath", unknownOpMalformed.Op, err)
	}

	// The same bug, pinned separately against the REAL spi.FilterNot rather
	// than a synthetic stand-in — see the doc comment above for why this
	// case does not subsume, and is not subsumed by, the one above.
	notMalformed := spi.Filter{Op: spi.FilterNot, Children: []spi.Filter{malformed}}
	if err := validateFilterPaths(notMalformed); !errors.Is(err, spi.ErrInvalidFilterPath) {
		t.Errorf("malformed path nested under FilterNot: err = %v, want wraps spi.ErrInvalidFilterPath", err)
	}

	// Regression guard: the existing recognised-operator behaviour must not
	// have broken. A malformed path nested under a genuine FilterAnd is
	// still rejected.
	andMalformed := spi.Filter{Op: spi.FilterAnd, Children: []spi.Filter{wellFormed, malformed}}
	if err := validateFilterPaths(andMalformed); !errors.Is(err, spi.ErrInvalidFilterPath) {
		t.Errorf("malformed path nested under FilterAnd: err = %v, want wraps spi.ErrInvalidFilterPath", err)
	}

	// Regression guard: a well-formed path nested under any of these kinds
	// of branch node is still accepted — the fix must not start rejecting
	// valid filters.
	andWellFormed := spi.Filter{Op: spi.FilterAnd, Children: []spi.Filter{wellFormed}}
	if err := validateFilterPaths(andWellFormed); err != nil {
		t.Errorf("well-formed path nested under FilterAnd: err = %v, want nil", err)
	}
	unknownOpWellFormed := spi.Filter{Op: spi.FilterOp("__unknown_branch_op__"), Children: []spi.Filter{wellFormed}}
	if err := validateFilterPaths(unknownOpWellFormed); err != nil {
		t.Errorf("well-formed path nested under unrecognised op %q: err = %v, want nil", unknownOpWellFormed.Op, err)
	}
	notWellFormed := spi.Filter{Op: spi.FilterNot, Children: []spi.Filter{wellFormed}}
	if err := validateFilterPaths(notWellFormed); err != nil {
		t.Errorf("well-formed path nested under FilterNot: err = %v, want nil", err)
	}
}
