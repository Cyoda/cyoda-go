package search

import (
	"fmt"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/domain/model/schema"
)

// classifyType returns the single canonical ordering class for a leaf's
// declared types, used by the FILTER path (filter_translate.go) to route
// coercion. Null members are ignored (nullable fields are fine). The remaining
// members must all map to the same class, else there is no deterministic order
// and the field is unsortable. Temporal data subtypes classify as
// OrderTemporal here so data-temporal comparisons route to the residual /
// temporal-aware pushdown — see sortKindForData for why the SORT path differs.
func classifyType(types []schema.DataType) (spi.OrderKind, error) {
	return classifyTypesFold(types, nil)
}

// sortKindForData returns the ORDER-BY sort class for a SourceData leaf. It
// mirrors classifyType but folds temporal subtypes onto OrderText, decoupling
// the SORT path from the FILTER path (which keeps OrderTemporal — do not change
// classifyType). A data-temporal field is stored as its bare ISO-8601 string,
// and ISO-8601 lexical order IS chronological order and is byte-identical across
// every backend: memory bytes.Compare (LessByOrder OrderText), postgres
// COLLATE "C", sqlite COLLATE BINARY. Sorting it as OrderTemporal would instead
// demand an epoch-ms normalization the bare stored subtype cannot supply
// (offset-less "2024-09-09"/"2024"), which each backend degrades differently —
// memory ties on Num=0, postgres yields NULL, sqlite coerces leading digits —
// producing three divergent orderings (and, with no residual, a wrong pushed
// LIMIT/OFFSET page). Folding to OrderText restores a single lexical order.
//
// Because temporal folds to OrderText, a polymorphic [String, LocalDate] field
// unifies to OrderText and sorts lexically rather than erroring on mixed class.
func sortKindForData(types []schema.DataType) (spi.OrderKind, error) {
	return classifyTypesFold(types, foldTemporalToText)
}

// foldTemporalToText maps OrderTemporal onto OrderText, leaving every other
// class untouched. It is the per-type fold that makes the SORT path treat data
// temporal fields lexically.
func foldTemporalToText(k spi.OrderKind) spi.OrderKind {
	if k == spi.OrderTemporal {
		return spi.OrderText
	}
	return k
}

// classifyTypesFold is the shared unification core for classifyType and
// sortKindForData. Each non-null member is classified via scalarClass, then
// (optionally) folded; all folded classes must agree, else the field has no
// deterministic order and is unsortable.
func classifyTypesFold(types []schema.DataType, fold func(spi.OrderKind) spi.OrderKind) (spi.OrderKind, error) {
	var (
		have bool
		kind spi.OrderKind
	)
	for _, t := range types {
		if t == schema.Null {
			continue
		}
		k, err := scalarClass(t)
		if err != nil {
			return 0, err
		}
		if fold != nil {
			k = fold(k)
		}
		if !have {
			kind, have = k, true
			continue
		}
		if k != kind {
			return 0, fmt.Errorf("field has mixed ordering classes and cannot be sorted")
		}
	}
	if !have {
		return 0, fmt.Errorf("field has no sortable scalar type")
	}
	return kind, nil
}

func scalarClass(t schema.DataType) (spi.OrderKind, error) {
	switch {
	case schema.IsNumeric(t):
		return spi.OrderNumeric, nil
	case t == schema.Boolean:
		return spi.OrderBool, nil
	case t == schema.String, t == schema.Character, t == schema.UUIDType,
		t == schema.TimeUUIDType:
		// Compared as their stored ISO/string form (Text/byte order).
		return spi.OrderText, nil
	case t == schema.LocalDate, t == schema.LocalDateTime,
		t == schema.LocalTime, t == schema.ZonedDateTime, t == schema.Year,
		t == schema.YearMonth:
		// Temporal subtypes compare chronologically (with cross-subtype
		// resolution), not lexically. Routing them to OrderTemporal makes
		// dataCoercion stamp CoerceTemporal for a temporal data field —
		// lighting up the pushdown temporal path and temporal-aware sort —
		// and matches how temporal meta fields (creationDate/…) are ordered.
		return spi.OrderTemporal, nil
	default: // ByteArray and anything non-scalar
		return 0, fmt.Errorf("type %s is not sortable", t)
	}
}

type metaField struct {
	Source spi.FieldSource
	Path   string
	Kind   spi.OrderKind
}

// sortableMetaFields is the closed set of meta sort keys (canonical client
// names from the result envelope). The plugins map these to physical storage.
var sortableMetaFields = map[string]metaField{
	"state":                   {Source: spi.SourceMeta, Path: "state", Kind: spi.OrderText},
	"creationDate":            {Source: spi.SourceMeta, Path: "creationDate", Kind: spi.OrderTemporal},
	"lastUpdateTime":          {Source: spi.SourceMeta, Path: "lastUpdateTime", Kind: spi.OrderTemporal},
	"transitionForLatestSave": {Source: spi.SourceMeta, Path: "transitionForLatestSave", Kind: spi.OrderText},
	"transactionId":           {Source: spi.SourceMeta, Path: "transactionId", Kind: spi.OrderText},
	"id":                      {Source: spi.SourceMeta, Path: "id", Kind: spi.OrderText},
}

// resolveMetaField looks up name in sortableMetaFields. The map-key lookup is
// what enforces "no nested meta paths": a dotted name (e.g. "a.b") is simply
// not a key in the map and returns ok=false.
func resolveMetaField(name string) (metaField, bool) {
	mf, ok := sortableMetaFields[name]
	return mf, ok
}

// isTemporalMetaField reports whether the given (already-canonicalized) meta
// field name is classified as temporal in sortableMetaFields — the single
// source of truth for the meta vocabulary. Callers translating or validating
// lifecycle conditions derive temporal routing from this lookup rather than
// maintaining a separate hardcoded field set.
func isTemporalMetaField(field string) bool {
	mf, ok := resolveMetaField(field)
	return ok && mf.Kind == spi.OrderTemporal
}

// isKnownMetaFilterField reports whether name is a valid meta filter field:
// either a sortableMetaFields key, or the "previousTransition" alias that
// canonicalizes to "transitionForLatestSave".
func isKnownMetaFilterField(field string) bool {
	if field == "previousTransition" {
		return true
	}
	_, ok := resolveMetaField(field)
	return ok
}
