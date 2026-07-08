package search

import (
	"bytes"
	"sort"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/tidwall/gjson"
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
func sortEntities(entities []*spi.Entity, specs []spi.OrderSpec) {
	sort.SliceStable(entities, func(i, j int) bool {
		for _, s := range specs {
			if decided, less := lessByKey(entities[i], entities[j], s); decided {
				return less
			}
		}
		return entities[i].Meta.ID < entities[j].Meta.ID
	})
}

// lessByKey decides ordering for a single key, fully applying direction and
// nulls-last. decided=false means the two are equal under this key (advance to
// the next key). A present value always precedes a missing/null one regardless
// of Desc.
func lessByKey(a, b *spi.Entity, s spi.OrderSpec) (decided, less bool) {
	av, aok := leafValue(a, s)
	bv, bok := leafValue(b, s)
	if !aok || !bok {
		if aok == bok {
			return false, false // both missing ⇒ equal
		}
		return true, aok // present (aok) sorts first, irrespective of Desc
	}
	c := compareValues(av, bv, s.Kind)
	if c == 0 {
		return false, false
	}
	if s.Desc {
		return true, c > 0
	}
	return true, c < 0
}

// compareValues returns -1/0/1 for two present values under the ordering class.
func compareValues(av, bv gjson.Result, kind spi.OrderKind) int {
	switch kind {
	case spi.OrderNumeric:
		return cmpFloat(av.Float(), bv.Float())
	case spi.OrderBool:
		return cmpBool(av.Bool(), bv.Bool())
	case spi.OrderTemporal:
		return cmpFloat(av.Num, bv.Num) // unix-millis carried in Num (see timeResult)
	default: // OrderText
		return bytes.Compare([]byte(av.String()), []byte(bv.String()))
	}
}

func leafValue(e *spi.Entity, s spi.OrderSpec) (gjson.Result, bool) {
	if s.Source == spi.SourceMeta {
		return metaLeaf(e, s.Path)
	}
	r := gjson.GetBytes(e.Data, s.Path)
	if !r.Exists() || r.Type == gjson.Null {
		return gjson.Result{}, false
	}
	return r, true
}

func metaLeaf(e *spi.Entity, path string) (gjson.Result, bool) {
	switch path {
	case "state":
		return gjson.Result{Type: gjson.String, Str: e.Meta.State}, e.Meta.State != ""
	case "transitionForLatestSave":
		return gjson.Result{Type: gjson.String, Str: e.Meta.TransitionForLatestSave}, e.Meta.TransitionForLatestSave != ""
	case "transactionId":
		return gjson.Result{Type: gjson.String, Str: e.Meta.TransactionID}, e.Meta.TransactionID != ""
	case "id":
		return gjson.Result{Type: gjson.String, Str: e.Meta.ID}, e.Meta.ID != ""
	case "creationDate":
		return timeResult(e.Meta.CreationDate)
	case "lastUpdateTime":
		return timeResult(e.Meta.LastModifiedDate)
	}
	return gjson.Result{}, false
}

func timeResult(t time.Time) (gjson.Result, bool) {
	if t.IsZero() {
		return gjson.Result{}, false
	}
	// Canonical temporal resolution is MILLISECONDS (the coarsest floor common
	// to every parity backend, incl. commercial Cassandra/HLC). Carry epoch-ms
	// in Num; UnixMilli (~1.75e12) is exact in float64 (< 2^53). Never carry
	// UnixNano — it exceeds 2^53 and loses precision. The SQL backends floor to
	// ms too (Tasks 10/11), so all paths tie within the same millisecond.
	// (Go UnixMilli truncates toward zero while postgres floor() rounds toward
	// -inf; they differ only for pre-1970 instants, which engine meta dates
	// never are — no fix needed.)
	return gjson.Result{Type: gjson.Number, Num: float64(t.UnixMilli())}, true
}

func cmpFloat(a, b float64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	default:
		return 0
	}
}

func cmpBool(a, b bool) int {
	switch {
	case !a && b:
		return -1
	case a && !b:
		return 1
	default:
		return 0
	}
}
