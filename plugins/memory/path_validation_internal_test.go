package memory

import "testing"

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
