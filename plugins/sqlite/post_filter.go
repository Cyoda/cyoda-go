package sqlite

import (
	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// EvaluateFilter is a public wrapper around evaluateFilter exposed so that
// cross-module parity tests (e.g. against internal/match.MatchFilter) can
// pin the contract that grouped-stats / streaming-tally must produce the
// same boolean as the sqlite post-filter step for any (filter, entity)
// tuple. NOT intended for hot-path use by other code — call sites within
// this plugin should keep using evaluateFilter directly.
func EvaluateFilter(f spi.Filter, entity *spi.Entity) (bool, error) {
	return evaluateFilter(f, entity)
}

// evaluateFilter evaluates a spi.Filter against an entity's data in Go.
// This is used for post-filtering residual (non-pushable) predicates.
// Delegates to spi.MatchFilter, the canonical cross-backend evaluator — see
// that function's doc for why the two must never diverge.
func evaluateFilter(f spi.Filter, entity *spi.Entity) (bool, error) {
	return spi.MatchFilter(f, entity.Data, entity.Meta), nil
}
