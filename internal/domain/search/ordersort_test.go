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

func TestSortEntities_NullsLastUnderDesc(t *testing.T) {
	es := []*spi.Entity{
		ent("a", `{"x":"m"}`, time.Time{}),
		ent("b", `{}`, time.Time{}), // missing x — must be last regardless of Desc
		ent("c", `{"x":"z"}`, time.Time{}),
	}
	// Desc=true: present values sort z before m, but missing must still come last.
	sortEntities(es, []spi.OrderSpec{{Path: "x", Source: spi.SourceData, Kind: spi.OrderText, Desc: true}})
	got := ids(es)
	if got[2] != "b" {
		t.Fatalf("nulls-last-desc order = %v, want b last", got)
	}
	// Additionally verify that with Desc=true, z (c) sorts before m (a).
	if got[0] != "c" || got[1] != "a" {
		t.Fatalf("nulls-last-desc order = %v, want [c a b]", got)
	}
}

func TestSortEntities_SubMillisecondTemporalTie(t *testing.T) {
	// Two entities whose creationDate differ by only 500µs (same UnixMilli = 1000).
	// They must tie on creationDate and resolve by entity_id ascending tiebreaker.
	//
	// If the implementation used UnixNano instead of UnixMilli the nanos would
	// differ (1_000_000_000 vs 1_000_500_000) and "b" (smaller nano) would sort
	// before "a" by time — the opposite of what we assert. Only UnixMilli causes
	// both to be equal and lets the tiebreaker decide.
	t1a := time.Unix(1, 0).UTC()         // UnixMilli = 1000, UnixNano = 1_000_000_000
	t1b := time.Unix(1, 500_000).UTC()   // UnixMilli = 1000, UnixNano = 1_000_500_000
	es := []*spi.Entity{
		ent("b", `{}`, t1a), // smaller nano, but same milli — tiebreaker should put "a" first
		ent("a", `{}`, t1b), // larger nano, but same milli — tiebreaker should put "a" first
	}
	sortEntities(es, []spi.OrderSpec{{Path: "creationDate", Source: spi.SourceMeta, Kind: spi.OrderTemporal}})
	if got := ids(es); got[0] != "a" || got[1] != "b" {
		t.Fatalf("sub-millisecond temporal tie = %v, want [a b] (id tiebreaker)", got)
	}
}
