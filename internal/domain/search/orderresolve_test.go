package search

import (
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/domain/model/schema"
)

func fields() map[string]schema.FieldDescriptor {
	return map[string]schema.FieldDescriptor{
		"$.surname": {Path: "$.surname", Types: []schema.DataType{schema.String}},
		"$.age":     {Path: "$.age", Types: []schema.DataType{schema.Integer}},
		"$.tags[*]": {Path: "$.tags[*]", Types: []schema.DataType{schema.String}, IsArray: true},
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
		{{Path: "missing", Source: spi.SourceData}},  // not in schema (missing-key branch)
		{{Path: "tags", Source: spi.SourceData}},     // $.tags not a key; only $.tags[*] is (missing-key branch)
		{{Path: "nope", Source: spi.SourceMeta}},     // unknown meta
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
