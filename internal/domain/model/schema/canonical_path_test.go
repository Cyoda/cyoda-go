package schema

import "testing"

// FieldsMap records an array hop as "[*]" — the element type of "$.arr" lives
// under "$.arr[*]", never under "$.arr[0]". A wire path is free to address one
// element positionally, and every declared-type lookup in the stack indexes
// that same map, so the positional spelling must be canonicalised to the
// wildcard spelling before the lookup. Without it the lookup misses, the leaf
// carries no declared type, and a type-directed comparison expands into
// nothing and never matches — an empty page for a field that holds the value.
func TestCanonicalFieldPath(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"no subscript", "$.amount", "$.amount"},
		{"already wildcard", "$.tags[*]", "$.tags[*]"},
		{"already wildcard nested", "$.items[*].name", "$.items[*].name"},
		{"index leaf", "$.arr[0]", "$.arr[*]"},
		{"index multi-digit", "$.arr[10]", "$.arr[*]"},
		{"index mid", "$.items[1].name", "$.items[*].name"},
		{"index twice", "$.a[0].b[1]", "$.a[*].b[*]"},
		{"index chained", "$.a[0][1]", "$.a[*][*]"},
		{"mixed", "$.a[*].b[0]", "$.a[*].b[*]"},
		{"empty", "", ""},

		// Shapes with no FieldsMap counterpart are left verbatim — they name
		// no single element, so there is no key they could canonicalise to.
		{"negative index", "$.arr[-1]", "$.arr[-1]"},
		{"slice", "$.arr[0:2]", "$.arr[0:2]"},
		{"union", "$.arr[0,1]", "$.arr[0,1]"},
		{"unclosed", "$.arr[0", "$.arr[0"},

		// An overflowing digit run is not a well-formed index at all — it is
		// the same C1 axis path-grammar.md §9/§10 close everywhere else: the
		// digit class alone says nothing about whether the run fits int32
		// (the bound, narrower than Go's int/int64, that every in-tree
		// backend can address), and spi.ParseFilterPath (which this now
		// delegates to, rather than a byte-scan with its own copy of the
		// digit-class check) rejects it. Left verbatim, matching every other
		// shape this function cannot canonicalise to a FieldsMap key.
		{"overflowing index", "$.tags[99999999999999999999]", "$.tags[99999999999999999999]"},
		{"index overflowing int32", "$.tags[2147483648]", "$.tags[2147483648]"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := CanonicalFieldPath(tc.in); got != tc.want {
				t.Errorf("CanonicalFieldPath(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestCanonicalFieldPath_MatchesFieldsMapKeys pins the canonical form against
// the map the schema itself builds, so the two cannot drift.
func TestCanonicalFieldPath_MatchesFieldsMapKeys(t *testing.T) {
	root := NewObjectNode()
	root.SetChild("arr", NewArrayNode(NewLeafNode(Integer)))
	item := NewObjectNode()
	item.SetChild("name", NewLeafNode(String))
	root.SetChild("items", NewArrayNode(item))

	fields := root.FieldsMap()
	for _, wire := range []string{"$.arr[0]", "$.items[1].name"} {
		key := CanonicalFieldPath(wire)
		if _, ok := fields[key]; !ok {
			t.Errorf("CanonicalFieldPath(%q) = %q, absent from FieldsMap %v", wire, key, keysOf(fields))
		}
	}
}

func keysOf(m map[string]FieldDescriptor) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
