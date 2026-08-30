package memory

import (
	"fmt"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// ErrInvalidFilterPath is returned when a Filter.Path or an OrderSpec.Path is
// outside the vocabulary the search contract defines. Named and behaved
// identically to the sqlite and postgres sentinels of the same name so the
// same input is classified the same way on every backend — memory has no SQL
// to inject into, but a backend that silently accepts what the others reject
// is a divergence, not a leniency, and answering a malformed path with an
// empty page is a wrong-but-available result.
//
// It wraps [spi.ErrInvalidFilterPath], the cross-backend sentinel, so
// errors.Is matches against either this one or the SPI one. The "%w"-only
// wrap adds no prefix, leaving the message text unchanged.
var ErrInvalidFilterPath = fmt.Errorf("%w", spi.ErrInvalidFilterPath)

// metaOrderPaths is the canonical SourceMeta sort vocabulary — exactly the
// paths spi.LessByOrder's orderMetaLeaf resolves, and the same set sqlite's
// metaBlobKey (plus "id") and postgres's metaJSONKey allow.
var metaOrderPaths = map[string]bool{
	"id":                      true,
	"state":                   true,
	"creationDate":            true,
	"lastUpdateTime":          true,
	"transitionForLatestSave": true,
	"transactionId":           true,
}

// validateFilterPaths walks a Filter tree and returns the first invalid path
// it encounters. Structurally identical to the sqlite and postgres validators
// of the same name — including that it does NOT branch on Source: the same
// dotted-identifier grammar applies to SourceMeta filter paths as to
// SourceData ones, and no meta allowlist is applied (unlike
// validateOrderSpecs). Every canonical meta filter name the shared kernel
// resolves is grammar-valid, so the allowlist would be redundant there and
// adding one here would be its own divergence.
//
// Leaf nodes without a Path (IsNull tree operators etc.) are skipped, matching
// the SQL backends.
//
// Memory interpolates nothing into SQL, so this is not an injection guard
// here; it exists so a malformed path is classified as ErrInvalidFilterPath on
// every backend instead of degrading to an empty page on this one. Filter
// paths are the bare dotted-identifier form — spi.ConditionToFilter strips the
// "$." prefix before a filter reaches any plugin, and the shared match kernel
// resolves Filter.Path with gjson verbatim, so a "$."-prefixed filter path
// never matched anything here either. GroupExpr.Path and AggregateExpr.Field
// go through validateJSONPath at the GroupedAggregate boundary and are held to
// the same bare-path grammar.
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

// validateOrderSpecs checks every OrderSpec path, mirroring the sqlite and
// postgres validators: SourceMeta paths must be in metaOrderPaths; SourceData
// paths must match the dotted-identifier grammar the SQL backends interpolate
// into json_extract. Empty paths are skipped.
func validateOrderSpecs(specs []spi.OrderSpec) error {
	for _, s := range specs {
		if s.Path == "" {
			continue
		}
		if s.Source == spi.SourceMeta {
			if !metaOrderPaths[s.Path] {
				return fmt.Errorf("%w: unknown meta sort path %q", ErrInvalidFilterPath, s.Path)
			}
			continue
		}
		if err := validateJSONPath(s.Path); err != nil {
			return err
		}
		if err := rejectSubscript(s.Path, "sort path"); err != nil {
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
// surface it names. Mirrors sqlite's and postgres's rejectSubscript.
//
// path is assumed already grammar-valid: every call site runs validateJSONPath
// first, so a parse error here is not expected in practice. But should this
// ever be reached defensively with an unvalidated path, the fail-closed
// answer is rejection, not silent acceptance: per
// .claude/rules/correctness-over-availability.md, a dependency (here, a
// successful parse) a correct "no subscript" answer requires must fail the
// check, not be treated as satisfying it.
func rejectSubscript(path, what string) error {
	hops, err := spi.ParseFilterPath(path)
	if err != nil {
		return fmt.Errorf("%w: %s %q: %s", ErrInvalidFilterPath, what, path, err)
	}
	for _, hop := range hops {
		if len(hop.Subs) > 0 {
			return fmt.Errorf("%w: %s %q carries an array subscript, which is not a grouping dimension, aggregation field, or sort key",
				ErrInvalidFilterPath, what, path)
		}
	}
	return nil
}

// validateJSONPath enforces the one SPI filter-path grammar
// (spi.ValidateFilterPath, docs/cloud-parity/path-grammar.md section 9):
// dotted name segments of ASCII letters/digits/underscore/hyphen, each
// optionally followed by one or more "[N]" or "[*]" array subscripts.
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
