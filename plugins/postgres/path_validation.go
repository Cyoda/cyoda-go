package postgres

import (
	"fmt"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// ErrInvalidFilterPath is returned when a Filter.Path, GroupExpr.Path, or
// AggregateExpr.Field contains characters that could break out of a JSONB
// path literal in a doc->>'<path>' style expression. Sentinel for callers
// that want to distinguish input validation errors from storage errors.
//
// The same grammar is enforced in plugins/sqlite/path_validation.go; the
// two implementations are kept structurally identical so cross-backend
// parity tests can rely on the same rejection set.
//
// It wraps [spi.ErrInvalidFilterPath], the cross-backend sentinel, so
// errors.Is matches against either this one or the SPI one. The "%w"-only
// wrap adds no prefix, leaving the message text unchanged.
var ErrInvalidFilterPath = fmt.Errorf("%w", spi.ErrInvalidFilterPath)

// validateJSONPath enforces the one SPI filter-path grammar
// (spi.ValidateFilterPath, docs/cloud-parity/path-grammar.md section 9) on
// paths that are interpolated into doc->'a'->>'b' expressions: dotted name
// segments, ASCII letters/digits/underscore/hyphen, each optionally followed
// by one or more "[N]" or "[*]" array subscripts.
//
// This rejects every character that could terminate the surrounding
// single-quoted SQL literal or otherwise inject SQL — notably ', ", \, ;,
// /, whitespace, and control bytes — which is also why the grammar is the
// injection guard for jsonbExtractText/jsonbExtractJSONB's interpolation,
// not just a syntax check.
//
// Hyphens are safe inside single-quoted postgres JSONB key literals: SQL
// comments ('--') only have special meaning OUTSIDE of string literals, so
// they cannot inject SQL through this path.
//
// Deliberately delegates to spi.ValidateFilterPath rather than scanning its
// own copy of the grammar: two independent scanners drift (this repo has
// already spent one fix round collapsing exactly that drift), and the SPI
// definition is the single source of truth every plugin and the engine's
// own resolver share.
func validateJSONPath(path string) error {
	if err := spi.ValidateFilterPath(path); err != nil {
		return fmt.Errorf("%w: %s", ErrInvalidFilterPath, err)
	}
	return nil
}

// validateFilterPaths walks a Filter tree and returns the first invalid path
// it encounters. Leaf nodes without a Path (IsNull tree operators etc.) are
// skipped; only nodes whose Path will be interpolated into SQL are checked.
func validateFilterPaths(f spi.Filter) error {
	switch f.Op {
	case spi.FilterAnd, spi.FilterOr:
		for _, c := range f.Children {
			if err := validateFilterPaths(c); err != nil {
				return err
			}
		}
		return nil
	}
	if f.Path == "" {
		return nil
	}
	return validateJSONPath(f.Path)
}

// validateGroupAndAggregatePaths holds GroupExpr.Path and AggregateExpr.Field
// to the same grammar as filter paths. GroupExpr kinds that carry no path
// (GroupExprState) are exempt.
//
// Unlike a filter leaf's Path — where an empty string is the legitimate
// "no field" shape the AND/OR tree operators carry, and validateFilterPaths
// skips it before ever reaching validateJSONPath — a GroupExpr.Path or
// AggregateExpr.Field always names a real field to group or aggregate by;
// there is no operator-node reading for it. validateJSONPath alone now
// admits "" (it delegates to the one grammar, which is right for a filter
// leaf), so this function rejects the empty case itself before delegating,
// rather than silently letting a meaningless "group by nothing" request
// through.
//
// Called at the top of GroupedAggregate, next to validateFilterPaths, so a
// malformed path is classified as a client error on every backend regardless
// of which pushdown decline the request would otherwise have hit. The
// duplicate checks inside groupExprToSQL / aggregateExprToSQL remain as the
// injection guard at the point of interpolation.
func validateGroupAndAggregatePaths(groupBy []spi.GroupExpr, aggs []spi.AggregateExpr) error {
	for _, g := range groupBy {
		if g.Kind != spi.GroupExprDataPath {
			continue
		}
		if g.Path == "" {
			return fmt.Errorf("%w: empty group-by path", ErrInvalidFilterPath)
		}
		if err := validateJSONPath(g.Path); err != nil {
			return err
		}
		if err := rejectSubscript(g.Path, "group-by path"); err != nil {
			return err
		}
	}
	for _, a := range aggs {
		if a.Field == "" {
			return fmt.Errorf("%w: empty aggregate field", ErrInvalidFilterPath)
		}
		if err := validateJSONPath(a.Field); err != nil {
			return err
		}
		if err := rejectSubscript(a.Field, "aggregate field"); err != nil {
			return err
		}
	}
	return nil
}

// rejectSubscript rejects a path that carries an array subscript ("[N]" or
// "[*]") anywhere along its hops. docs/cloud-parity/path-grammar.md section
// 7: "An array position is therefore not a grouping dimension, an
// aggregation field or a sort key. Those three surfaces admit no
// subscript... The three surfaces that reject subscripts use the grammar of
// section 2 with the subscript production removed." A subscripted path is
// legal on a FILTER leaf (validateFilterPaths / validateJSONPath alone) but
// illegal here — the same string, two different verdicts depending on which
// surface it names. Mirrors sqlite's rejectSubscript.
//
// path is assumed already grammar-valid: every call site runs validateJSONPath
// first. A parse error here is therefore not expected in practice and is
// treated as "no subscript" (falls through) rather than duplicating the
// grammar error, keeping this helper total.
func rejectSubscript(path, what string) error {
	hops, err := spi.ParseFilterPath(path)
	if err != nil {
		return nil
	}
	for _, hop := range hops {
		if len(hop.Subs) > 0 {
			return fmt.Errorf("%w: %s %q carries an array subscript, which is not a grouping dimension, aggregation field, or sort key",
				ErrInvalidFilterPath, what, path)
		}
	}
	return nil
}

// validateOrderSpecs checks every OrderSpec before any path is interpolated
// into SQL. Two checks are applied, in order:
//
//  1. SourceMeta paths: only "id" (special) and the keys of metaJSONKey are
//     accepted; anything else is rejected with ErrInvalidFilterPath. This is
//     an additive check that runs BEFORE the injection guard below.
//
//  2. SourceData paths: validated against the dotted-identifier grammar by
//     validateJSONPath (injection guard). Empty paths are skipped.
//
// MUST be called at the Search() boundary before any OrderSpec.Path is
// interpolated into SQL (injection invariant).
func validateOrderSpecs(specs []spi.OrderSpec) error {
	for _, sp := range specs {
		if sp.Path == "" {
			continue
		}
		if sp.Source == spi.SourceMeta {
			// "id" is special-cased to the entity_id column; all other meta
			// paths must be in the canonical set.
			if sp.Path != "id" {
				if _, ok := metaJSONKey[sp.Path]; !ok {
					return fmt.Errorf("%w: unknown meta sort path %q", ErrInvalidFilterPath, sp.Path)
				}
			}
			continue
		}
		if err := validateJSONPath(sp.Path); err != nil {
			return err
		}
		if err := rejectSubscript(sp.Path, "sort path"); err != nil {
			return err
		}
	}
	return nil
}
