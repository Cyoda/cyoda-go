package sqlite

import (
	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// EvaluateFilter is a public wrapper around evaluateFilter exposed so that
// cross-module parity tests (against internal/match.MatchFilter) can pin the
// contract that grouped-stats / streaming-tally must produce the same boolean
// as the sqlite post-filter step for any (filter, entity) tuple. NOT intended
// for hot-path use by other code — call sites within this plugin should keep
// using evaluateFilter directly.
func EvaluateFilter(p spi.PreparedFilter, entity *spi.Entity) bool {
	return evaluateFilter(p, entity)
}

// evaluateFilter evaluates an already-prepared filter against an entity's data
// in Go, for residual (non-pushable) predicates. It takes a prepared filter
// rather than a spi.Filter so the operand parse, type bucketing and regex
// compilation happen once per query at the plan site, not once per row here.
//
// Delegates to the canonical cross-backend kernel — see spi.Prepare for why
// this plugin must never grow an evaluator of its own.
func evaluateFilter(p spi.PreparedFilter, entity *spi.Entity) bool {
	return p.Match(entity.Data, entity.Meta)
}
