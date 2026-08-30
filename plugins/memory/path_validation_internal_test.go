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
