package search

import (
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

func TestSearchOptions_CarriesOrderBy(t *testing.T) {
	o := SearchOptions{OrderBy: []OrderKey{{Path: "surname", Source: spi.SourceData, Desc: true}}}
	if len(o.OrderBy) != 1 || o.OrderBy[0].Path != "surname" {
		t.Fatalf("OrderBy not carried: %+v", o.OrderBy)
	}
}
