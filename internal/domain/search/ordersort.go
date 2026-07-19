package search

import (
	"sort"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// sortEntities sorts in place per the canonical ordering semantic: each spec
// in precedence order, comparison fixed by Kind, missing/null last (both
// directions), with a final entity_id ascending tiebreaker. Stable so the
// tiebreaker resolves equal rows deterministically.
//
// It runs UNCONDITIONALLY, even with no specs: the SQL backends default to
// "ORDER BY entity_id", but the memory backend's GetAll returns Go
// map-iteration order, so without this the no-sort case would diverge
// (non-deterministic memory order vs entity_id on SQL). With no specs the loop
// body is skipped and rows order by the entity_id tiebreaker — matching SQL.
//
// The comparison itself delegates to spi.LessByOrder, the canonical
// cross-backend comparator shared with the SQL backends' ORDER BY — see that
// function's doc for why the two must never diverge.
func sortEntities(entities []*spi.Entity, specs []spi.OrderSpec) {
	sort.SliceStable(entities, func(i, j int) bool {
		return spi.LessByOrder(entities[i], entities[j], specs)
	})
}
