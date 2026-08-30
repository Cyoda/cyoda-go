package search

import (
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

func TestParseSortParam(t *testing.T) {
	got, err := ParseSortParam([]string{"surname:desc", "@creationDate:asc", "address.home-address.country"}, 16)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []OrderKey{
		{Path: "surname", Source: spi.SourceData, Desc: true},
		{Path: "creationDate", Source: spi.SourceMeta, Desc: false},
		{Path: "address.home-address.country", Source: spi.SourceData, Desc: false},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d keys, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("key %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestParseSortParam_DollarTolerated(t *testing.T) {
	got, err := ParseSortParam([]string{"$.surname:desc"}, 16)
	if err != nil || got[0].Path != "surname" || got[0].Source != spi.SourceData || !got[0].Desc {
		t.Fatalf("got %+v err %v", got, err)
	}
}

func TestParseSortParam_DataFieldNamedMeta(t *testing.T) {
	got, err := ParseSortParam([]string{"meta.label.position.x:desc"}, 16)
	if err != nil || got[0].Source != spi.SourceData || got[0].Path != "meta.label.position.x" {
		t.Fatalf("data field 'meta' mis-parsed: %+v err %v", got, err)
	}
}

func TestParseSortParam_Errors(t *testing.T) {
	bad := [][]string{
		{""}, {":desc"}, {"@"}, {"name:"}, {"name:up"},
		{"@a.b.c"},                      // nested meta
		{"surname", "surname"},          // duplicate
		{"surname:asc", "surname:desc"}, // duplicate (conflicting dir)

		// A sort path names fields by the same segment charset the jsonPath
		// grammar admits, so anything outside it is refused here too rather
		// than reaching a backend that would sort by a field it cannot
		// resolve. Pinned explicitly because the charset now has ONE
		// definition ([schema.IsSegmentName]) that this validator shares.
		{"$."},          // leader only
		{"a..b"},        // empty segment
		{"a."},          // trailing dot
		{".a"},          // leading dot
		{"first name"},  // space
		{"café"},        // non-ascii
		{"日本"},          // non-ascii multibyte
		{"a[0]"},        // subscript
		{"a['x']"},      // bracket-quoted
		{"a/b"},         // slash
		{"a*"},          // asterisk
		{"a'; --"},      // sql tail
		{"a\x00"},       // nul byte
		{"@first name"}, // meta path, same charset
	}
	for _, in := range bad {
		if _, err := ParseSortParam(in, 16); err == nil {
			t.Fatalf("expected error for %v", in)
		}
	}
	// cap exceeded
	many := make([]string, 17)
	for i := range many {
		many[i] = "f" + string(rune('a'+i))
	}
	if _, err := ParseSortParam(many, 16); err == nil {
		t.Fatal("expected cap error")
	}
}

// TestSortParam_UsesTheSharedScanner pins that isValidSortPath's accept set
// coincides with ValidateScalarJSONPath's — both hold a bare data path to
// the same segment charset ([schema.IsSegmentName]) and reject a subscript.
// It runs on the LEADER-STRIPPED token (isValidSortPath never sees "$."),
// so the comparison prepends "$." to each candidate before calling
// ValidateScalarJSONPath, putting both scanners on the same string.
//
// This table spans path-grammar.md §2's full accept/reject list — every
// well-formed subscript chain, every rejected shape (empty or trailing
// segment, bracket-quoted access, unclosed/unmatched bracket, negative/
// signed/exponent index, slice, union, filter expression, whitespace in a
// subscript, a non-index chained subscript, and any character outside the
// name set) — plus the SQL-injection and non-ASCII probes ParseSortParam's
// own error table already exercises. If no case below finds a real
// divergence, isValidSortPath is a second scanner that happens to agree,
// not a defect; this test is the proof.
func TestSortParam_UsesTheSharedScanner(t *testing.T) {
	cases := []string{
		// accepted forms
		"amount",
		"address.city",
		"obj.0",
		"address.home-address.country",
		"a_b-c9",
		"tags",

		// rejected: no "$." leader is out of scope here (both calls always
		// prepend "$."), so this table only spans what differs AFTER the
		// leader — every other row of §2's table.
		"",             // leader only after prepending
		".",            // empty + trailing segment
		"a..b",         // empty segment
		"a.",           // trailing segment
		"[0]",          // no name before a subscript
		"[*]",          // no name before a subscript
		"a[]",          // empty subscript
		"a[0]",         // subscript (scalar surface disallows)
		"a[*]",         // subscript
		"a[12]",        // subscript
		"a[-1]",        // negative index
		"a[+1]",        // signed index
		"a[1e2]",       // exponent index
		"a[0:2]",       // slice
		"a[0,1]",       // union
		"a[?(@.x)]",    // filter expression
		"a[ 0]",        // whitespace in subscript
		"a[0 ]",        // whitespace in subscript
		"a[",           // unclosed bracket
		"a[0",          // unclosed bracket
		"a]",           // unmatched bracket
		"a].b",         // unmatched bracket
		"a[0]b",        // char after well-formed subscript
		"a[0];DROP",    // char after well-formed subscript
		"tags[*][x]",   // non-index chained subscript
		"a[0][-1]",     // non-index chained subscript
		"a[*].",        // trailing segment after subscript
		"a[*]..b",      // empty segment after subscript
		"tags[*].name", // multi-segment with subscript
		"items[0].sku", // multi-segment with subscript
		"matrix[*][*]", // chained wildcard subscripts
		"matrix[0][1]", // chained index subscripts
		"a[0][*].b",    // mixed chained subscripts
		"['x']",        // bracket-quoted property access
		".['x']",       // bracket-quoted property access
		`a["b"]`,       // bracket-quoted property access
		`a[0]['x']`,    // bracket-quoted property access after subscript
		"a b",          // disallowed character (space)
		"a;DROP",       // disallowed character (SQL tail)
		"a/etc",        // disallowed character (slash)
		"a*",           // disallowed character (asterisk outside subscript)
		"a'; --",       // disallowed character (SQL tail)
		"a\x00",        // disallowed character (NUL)
		"café",         // non-ASCII
		"日本",           // non-ASCII multibyte
	}
	for _, p := range cases {
		t.Run(p, func(t *testing.T) {
			httpErr := parseSortErrForTest("$." + p)
			svcErr := ValidateScalarJSONPath("$." + p)
			if (httpErr == nil) != (svcErr == nil) {
				t.Errorf("%q: http=%v service=%v — the two scanners disagree", p, httpErr, svcErr)
			}
		})
	}
}

// parseSortErrForTest runs the HTTP sort-parameter scanner (parseSortToken)
// and returns only its error, discarding the parsed OrderKey — the table
// above only cares whether the two scanners agree on accept vs reject.
func parseSortErrForTest(raw string) error {
	_, err := parseSortToken(raw)
	return err
}
