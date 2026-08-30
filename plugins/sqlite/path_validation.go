package sqlite

import (
	"fmt"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// ErrInvalidFilterPath is returned when a Filter.Path or OrderSpec.Path
// contains characters that could break out of a JSON-path literal in a
// json_extract expression. Sentinel for callers that want to distinguish
// input validation errors from storage errors.
//
// It wraps [spi.ErrInvalidFilterPath], the cross-backend sentinel, so
// errors.Is matches against either this one or the SPI one. The "%w"-only
// wrap adds no prefix, leaving the message text unchanged.
var ErrInvalidFilterPath = fmt.Errorf("%w", spi.ErrInvalidFilterPath)

// validateJSONPath enforces the one SPI filter-path grammar
// (spi.ValidateFilterPath, docs/cloud-parity/path-grammar.md section 9) on
// paths that are interpolated into json_extract(..., '$.<path>')
// expressions: dotted name segments, ASCII letters/digits/underscore/hyphen,
// each optionally followed by one or more "[N]" or "[*]" array subscripts.
//
// This rejects every character that could terminate the surrounding
// single-quoted SQL literal or otherwise inject SQL — notably ', ", \, ;,
// /, whitespace, and control bytes — which is also why the grammar is the
// injection guard for fieldExpr's interpolation, not just a syntax check.
//
// Hyphens are safe inside single-quoted SQLite JSON-path literals: SQL
// comments ('--') and other hyphen sequences only have special meaning
// outside of string literals, so they cannot inject SQL through this path.
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
		if err := validateJSONPath(g.Path); err != nil {
			return err
		}
	}
	for _, a := range aggs {
		if err := validateJSONPath(a.Field); err != nil {
			return err
		}
	}
	return nil
}

// validateOrderSpecs checks every OrderSpec path. For SourceMeta paths,
// the canonical name must be in the metaBlobKey allowlist (or "id");
// unknown meta names are rejected before any SQL is built. For SourceData
// paths, the existing validateJSONPath SQL-injection guard applies.
func validateOrderSpecs(specs []spi.OrderSpec) error {
	for _, s := range specs {
		if s.Path == "" {
			continue
		}
		if s.Source == spi.SourceMeta {
			if s.Path != "id" {
				if _, ok := metaBlobKey[s.Path]; !ok {
					return fmt.Errorf("%w: unknown meta sort path %q", ErrInvalidFilterPath, s.Path)
				}
			}
			continue
		}
		if err := validateJSONPath(s.Path); err != nil {
			return err
		}
	}
	return nil
}
