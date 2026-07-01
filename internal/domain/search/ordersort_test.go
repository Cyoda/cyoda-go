package search

import (
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

func ent(id, data string, created time.Time) *spi.Entity {
	return &spi.Entity{Meta: spi.EntityMeta{ID: id, CreationDate: created}, Data: []byte(data)}
}

func ids(es []*spi.Entity) []string {
	out := make([]string, len(es))
	for i, e := range es {
		out[i] = e.Meta.ID
	}
	return out
}

func TestSortEntities_NumericNotLexical(t *testing.T) {
	es := []*spi.Entity{
		ent("a", `{"n":9}`, time.Time{}),
		ent("b", `{"n":10}`, time.Time{}),
		ent("c", `{"n":100}`, time.Time{}),
	}
	sortEntities(es, []spi.OrderSpec{{Path: "n", Source: spi.SourceData, Kind: spi.OrderNumeric}})
	if got := ids(es); got[0] != "a" || got[1] != "b" || got[2] != "c" {
		t.Fatalf("numeric order = %v, want [a b c]", got)
	}
}

func TestSortEntities_NullsLast(t *testing.T) {
	es := []*spi.Entity{
		ent("a", `{"x":"m"}`, time.Time{}),
		ent("b", `{}`, time.Time{}), // missing x
		ent("c", `{"x":"z"}`, time.Time{}),
	}
	sortEntities(es, []spi.OrderSpec{{Path: "x", Source: spi.SourceData, Kind: spi.OrderText}})
	if got := ids(es); got[2] != "b" {
		t.Fatalf("nulls-last order = %v, want b last", got)
	}
}

func TestSortEntities_MetaCreationDateAndTiebreaker(t *testing.T) {
	t0 := time.Unix(0, 0).UTC()
	t1 := time.Unix(1, 0).UTC()
	es := []*spi.Entity{
		ent("b", `{}`, t1),
		ent("a", `{}`, t1),
		ent("c", `{}`, t0),
	}
	sortEntities(es, []spi.OrderSpec{{Path: "creationDate", Source: spi.SourceMeta, Kind: spi.OrderTemporal}})
	// c (t0) first; a,b (t1) ordered by entity_id tiebreaker
	if got := ids(es); got[0] != "c" || got[1] != "a" || got[2] != "b" {
		t.Fatalf("temporal+tiebreaker order = %v, want [c a b]", got)
	}
}

func TestSortEntities_NoKeysOrdersByID(t *testing.T) {
	es := []*spi.Entity{ent("c", `{}`, time.Time{}), ent("a", `{}`, time.Time{}), ent("b", `{}`, time.Time{})}
	sortEntities(es, nil) // no sort keys ⇒ entity_id asc, matching the SQL backends
	if got := ids(es); got[0] != "a" || got[1] != "b" || got[2] != "c" {
		t.Fatalf("no-keys order = %v, want [a b c]", got)
	}
}
