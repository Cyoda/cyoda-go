package search

import (
	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/domain/model/schema"
)

// sortKindForData returns the ORDER-BY sort class for a SourceData leaf. It
// mirrors spi.ClassifyType but folds temporal subtypes onto OrderText,
// decoupling the SORT path from the FILTER path (which keeps OrderTemporal via
// spi.ClassifyType in the condition translator). A data-temporal field is stored as its bare ISO-8601 string,
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
	return spi.ClassifyTypesFold(types, foldTemporalToText)
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

// metaField is an alias for [spi.MetaField]. The vocabulary itself lives in the
// SPI — see spi.ResolveMetaField — so this package, the condition translator
// and the storage plugins cannot disagree about which meta names exist or how
// they order.
type metaField = spi.MetaField

func resolveMetaField(name string) (metaField, bool) { return spi.ResolveMetaField(name) }

func isTemporalMetaField(field string) bool { return spi.IsTemporalMetaField(field) }

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
