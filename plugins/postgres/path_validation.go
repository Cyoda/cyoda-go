package postgres

import (
	"errors"
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
var ErrInvalidFilterPath = errors.New("invalid filter path")

// validateJSONPath enforces an extended dotted-identifier grammar on paths
// that are interpolated into doc->'a'->>'b' expressions.
//
// Allowed: segments of ASCII letters, digits, underscore, and hyphen ('-'),
// separated by single '.' characters. At least one segment, no empty
// segments, no leading/trailing dots. This rejects every character that
// could terminate the surrounding single-quoted SQL literal or otherwise
// inject SQL — notably ', ", \, ;, /, *, whitespace, brackets, and control
// bytes.
//
// Hyphens are safe inside single-quoted postgres JSONB key literals: SQL
// comments ('--') only have special meaning OUTSIDE of string literals, so
// they cannot inject SQL through this path.
//
// The grammar is intentionally narrower than the full postgres JSONB path
// grammar (which accepts bracketed indices and Unicode identifiers). If a
// genuine use case ever needs those forms, extend this validator rather
// than bypassing it.
func validateJSONPath(path string) error {
	if path == "" {
		return fmt.Errorf("%w: empty", ErrInvalidFilterPath)
	}
	segmentStart := 0
	for i := 0; i < len(path); i++ {
		c := path[i]
		if c == '.' {
			if i == segmentStart {
				return fmt.Errorf("%w: empty segment", ErrInvalidFilterPath)
			}
			segmentStart = i + 1
			continue
		}
		if !isIdentByte(c) {
			return fmt.Errorf("%w: disallowed character %q at offset %d", ErrInvalidFilterPath, c, i)
		}
	}
	if segmentStart == len(path) {
		return fmt.Errorf("%w: trailing dot", ErrInvalidFilterPath)
	}
	return nil
}

func isIdentByte(c byte) bool {
	switch {
	case c >= 'a' && c <= 'z':
		return true
	case c >= 'A' && c <= 'Z':
		return true
	case c >= '0' && c <= '9':
		return true
	case c == '_':
		return true
	case c == '-':
		// Hyphens are valid JSON key characters and safe inside single-quoted
		// postgres JSONB path literals — they cannot break out of the
		// surrounding quote. SQL comments ('--') only have special meaning
		// outside of string literals.
		return true
	}
	return false
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
	}
	return nil
}
