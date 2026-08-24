package entity

import (
	"strings"
	"testing"
)

// FuzzNormalizeScalarPath ensures the path validator doesn't panic on
// adversarial inputs and that its contract holds:
//
//   - any accepted path has no [ or ] in it (no array projection or
//     subscript is accepted on a surface that needs a single scalar)
//   - accepting is idempotent: an accepted path is itself valid input and
//     comes back unchanged. The validator does not rewrite — that is what
//     lets the response's group-key path echo the request verbatim.
//   - error path returns "" (no half-state leak)
//
// Seeds cover happy paths, edge cases, and known SQL-injection /
// JSONPath-shape adversarial inputs.
func FuzzNormalizeScalarPath(f *testing.F) {
	seeds := []string{
		"state",
		"$.foo",
		"$.foo.bar",
		"$.['variantId']",
		"$.items[*]",
		"$.items[0]",
		"$['x']['y']",
		"",
		"; DROP TABLE entities;",
		"$.x'; --",
		"$.\n\x00",
		strings.Repeat("a.", 1000),
		"$..foo",
		"$.foo..bar",
		"foo",
		"$",
		"$.",
		"[",
		"]",
		"$.['']",
	}
	for _, s := range seeds {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, raw string) {
		// Must never panic.
		norm, err := normalizeScalarPath(raw)
		if err != nil {
			// Error path: norm must be "" (no half-state leak).
			if norm != "" {
				t.Fatalf("error returned but norm=%q for input %q", norm, raw)
			}
			return
		}
		// Success path: no array projection brackets remain.
		if strings.ContainsAny(norm, "[]") {
			t.Fatalf("normalized output contains [ or ]: %q from %q", norm, raw)
		}
		// Idempotence: renormalizing produces the same string.
		norm2, err2 := normalizeScalarPath(norm)
		if err2 != nil {
			t.Fatalf("re-normalize failed: %q → %q → err %v", raw, norm, err2)
		}
		if norm2 != norm {
			t.Fatalf("normalize not idempotent: %q → %q → %q", raw, norm, norm2)
		}
	})
}
