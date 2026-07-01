package sqlite

import (
	"errors"
	"fmt"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// ErrInvalidFilterPath is returned when a Filter.Path or OrderSpec.Path
// contains characters that could break out of a JSON-path literal in a
// json_extract expression. Sentinel for callers that want to distinguish
// input validation errors from storage errors.
var ErrInvalidFilterPath = errors.New("invalid filter path")

// validateJSONPath enforces an extended dotted-identifier grammar on paths
// that are interpolated into json_extract(..., '$.<path>') expressions.
//
// Allowed: segments of ASCII letters, digits, underscore, and hyphen ('-'),
// separated by single '.' characters. At least one segment, no empty
// segments, no leading/trailing dots. This rejects every character that
// could terminate the surrounding single-quoted SQL literal or otherwise
// inject SQL — notably ', ", \, ;, /, *, whitespace, and control bytes.
//
// Hyphens are safe inside single-quoted SQLite JSON-path literals: SQL
// comments ('--') and other hyphen sequences only have special meaning
// outside of string literals, so they cannot inject SQL through this path.
//
// The grammar is intentionally narrower than the full SQLite JSON path
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
		// SQLite json_extract path literals — they cannot break out of the
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
