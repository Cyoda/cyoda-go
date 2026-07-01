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
		{"@a.b.c"}, // nested meta
		{"surname", "surname"},          // duplicate
		{"surname:asc", "surname:desc"}, // duplicate (conflicting dir)
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
