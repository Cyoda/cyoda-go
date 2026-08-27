package search

import (
	"errors"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/domain/model/schema"
)

func fields() map[string]schema.FieldDescriptor {
	return map[string]schema.FieldDescriptor{
		"$.surname":  {Path: "$.surname", Types: []schema.DataType{schema.String}},
		"$.age":      {Path: "$.age", Types: []schema.DataType{schema.Integer}},
		"$.tags[*]":  {Path: "$.tags[*]", Types: []schema.DataType{schema.String}, IsArray: true},
		"$.bornOn":   {Path: "$.bornOn", Types: []schema.DataType{schema.LocalDate}},
		"$.polyDate": {Path: "$.polyDate", Types: []schema.DataType{schema.String, schema.LocalDate}},
		// A scalar leaf INSIDE an array of objects. FieldsMap records it under
		// the wildcard key with IsArray:false — collectFields recurses into an
		// array's object element with inArray=false — so the IsArray guard below
		// does not fire for it.
		"$.items[*].name": {Path: "$.items[*].name", Types: []schema.DataType{schema.String}},
	}
}

// TestResolveOrderBy_DataTemporalSortsAsText locks in the sort/filter decoupling:
// a SourceData temporal field resolves to OrderText for ORDER BY (lexical
// ISO-8601 = chronological, byte-identical across memory/sqlite/postgres), NOT
// OrderTemporal (which would tie on memory's Num=0, NULL on postgres, and coerce
// leading digits on sqlite — three divergent orders + a wrong pushed page).
func TestResolveOrderBy_DataTemporalSortsAsText(t *testing.T) {
	got, err := resolveOrderBy([]OrderKey{
		{Path: "bornOn", Source: spi.SourceData},
	}, fields())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := spi.OrderSpec{Path: "bornOn", Source: spi.SourceData, Kind: spi.OrderText}
	if got[0] != want {
		t.Fatalf("data-temporal sort spec = %+v, want %+v", got[0], want)
	}
}

// TestResolveOrderBy_PolymorphicTemporalSortsAsText covers the Minor: a
// polymorphic [String, LocalDate] data field must sort lexically (OrderText)
// rather than failing the whole search on a mixed-class error.
func TestResolveOrderBy_PolymorphicTemporalSortsAsText(t *testing.T) {
	got, err := resolveOrderBy([]OrderKey{
		{Path: "polyDate", Source: spi.SourceData},
	}, fields())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := spi.OrderSpec{Path: "polyDate", Source: spi.SourceData, Kind: spi.OrderText}
	if got[0] != want {
		t.Fatalf("polymorphic-temporal sort spec = %+v, want %+v", got[0], want)
	}
}

func TestResolveOrderBy_DataAndMeta(t *testing.T) {
	got, err := resolveOrderBy([]OrderKey{
		{Path: "surname", Source: spi.SourceData, Desc: true},
		{Path: "creationDate", Source: spi.SourceMeta},
	}, fields())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []spi.OrderSpec{
		{Path: "surname", Source: spi.SourceData, Desc: true, Kind: spi.OrderText},
		{Path: "creationDate", Source: spi.SourceMeta, Desc: false, Kind: spi.OrderTemporal},
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("spec %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestResolveOrderBy_Rejections(t *testing.T) {
	f := fields()
	bad := [][]OrderKey{
		{{Path: "missing", Source: spi.SourceData}}, // not in schema (missing-key branch)
		{{Path: "tags", Source: spi.SourceData}},    // $.tags not a key; only $.tags[*] is (missing-key branch)
		{{Path: "nope", Source: spi.SourceMeta}},    // unknown meta
		// tags[*] IS a key in the fields map (IsArray=true) so the lookup
		// succeeds and the fd.IsArray branch fires — not the missing-key branch.
		// This case is unreachable via the HTTP grammar (isValidSortPath rejects
		// '[') but tests real defense-in-depth at the domain boundary.
		{{Path: "tags[*]", Source: spi.SourceData}},
	}
	for _, keys := range bad {
		if _, err := resolveOrderBy(keys, f); err == nil {
			t.Fatalf("expected error for %+v", keys)
		}
	}
}

// TestResolveOrderBy_RejectsSubscriptedDataPath pins that a sort path is held
// to the SCALAR path grammar, not merely to schema membership.
//
// "$.items[*].name" is a recorded field with IsArray:false, so membership and
// the array guard both pass it. The HTTP parser cannot spell it — isValidSortPath
// refuses "[" — but gRPC builds an OrderKey from the client's path verbatim
// (internal/grpc/search.go), so the two transports disagreed on the same
// request. Worse than the disagreement is where the accepted path ends up: on
// the in-memory branch, sortEntities resolves it with gjson, which has no
// bracket syntax, so every entity misses, all compare equal, and the caller
// gets 200 with results that are simply not sorted — a wrong-but-available
// answer to a request the engine should have refused.
//
// A projection has no single scalar to sort by in the first place, which is
// exactly why ValidateScalarJSONPath exists for groupBy and aggregation
// fields. Sort is the same case.
func TestResolveOrderBy_RejectsSubscriptedDataPath(t *testing.T) {
	for _, path := range []string{"items[*].name", "$.items[*].name"} {
		t.Run(path, func(t *testing.T) {
			_, err := resolveOrderBy([]OrderKey{{Path: path, Source: spi.SourceData}}, fields())
			if err == nil {
				t.Fatalf("resolveOrderBy(%q) was accepted; an array projection cannot denote the single scalar a sort key needs, and gjson resolves none of them — the caller would get an unsorted 200", path)
			}
			if !errors.Is(err, ErrInvalidFieldPath) {
				t.Errorf("resolveOrderBy(%q) error = %v; must wrap ErrInvalidFieldPath so every transport answers 400 INVALID_FIELD_PATH", path, err)
			}
		})
	}
}

// TestResolveOrderBy_RejectsDisallowedCharacterDataPath is the same boundary
// seen from the other side: a path carrying a character the segment charset
// refuses. Membership alone would report it as an unknown field, which is a
// true statement about a request that is malformed rather than unsatisfiable —
// and, on a model whose schema failed to load, membership is not checked at
// all.
func TestResolveOrderBy_RejectsDisallowedCharacterDataPath(t *testing.T) {
	for _, path := range []string{"sur name", "surname';DROP", "sur|name"} {
		t.Run(path, func(t *testing.T) {
			_, err := resolveOrderBy([]OrderKey{{Path: path, Source: spi.SourceData}}, fields())
			if err == nil {
				t.Fatalf("resolveOrderBy(%q) was accepted", path)
			}
			if !errors.Is(err, ErrInvalidFieldPath) {
				t.Errorf("resolveOrderBy(%q) error = %v; must wrap ErrInvalidFieldPath", path, err)
			}
		})
	}
}
