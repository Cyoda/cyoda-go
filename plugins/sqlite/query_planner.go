package sqlite

import (
	"fmt"
	"strings"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// sqlPlan holds the result of translating a spi.Filter into SQL.
// where + args represent the pushable portion as a SQL WHERE fragment.
// postFilter is the residual filter that must be evaluated in Go.
type sqlPlan struct {
	where      string
	args       []any
	postFilter *spi.Filter
}

// planQuery translates a spi.Filter tree into a SQL WHERE clause and an
// optional residual filter for post-processing in Go.
//
// Dissection strategy:
//   - Greedy AND: extract pushable children into SQL, collect non-pushable as residual
//   - Conservative OR: only push down if ALL children are pushable, otherwise entire OR is residual
//   - Leaf nodes: pushable ops produce SQL fragments; non-pushable become residual
func planQuery(filter spi.Filter) sqlPlan {
	pushed, residual := dissect(filter)
	plan := sqlPlan{postFilter: residual}
	if pushed != nil {
		plan.where, plan.args = toSQL(*pushed)
	}
	return plan
}

// dissect splits a filter tree into a pushable portion and a residual portion.
func dissect(f spi.Filter) (pushed *spi.Filter, residual *spi.Filter) {
	switch f.Op {
	case spi.FilterAnd:
		return dissectAnd(f)
	case spi.FilterOr:
		return dissectOr(f)
	default:
		if isPushable(f.Op) {
			return &f, nil
		}
		return nil, &f
	}
}

// dissectAnd implements greedy AND dissection: push pushable children to SQL,
// collect non-pushable children as residual.
func dissectAnd(f spi.Filter) (*spi.Filter, *spi.Filter) {
	var pushedChildren, residualChildren []spi.Filter
	for _, child := range f.Children {
		p, r := dissect(child)
		if p != nil {
			pushedChildren = append(pushedChildren, *p)
		}
		if r != nil {
			residualChildren = append(residualChildren, *r)
		}
	}

	var pushed, residual *spi.Filter

	switch len(pushedChildren) {
	case 0:
		// nothing to push
	case 1:
		pushed = &pushedChildren[0]
	default:
		pushed = &spi.Filter{Op: spi.FilterAnd, Children: pushedChildren}
	}

	switch len(residualChildren) {
	case 0:
		// no residual
	case 1:
		residual = &residualChildren[0]
	default:
		residual = &spi.Filter{Op: spi.FilterAnd, Children: residualChildren}
	}

	return pushed, residual
}

// dissectOr implements conservative OR dissection: only push if ALL children
// are fully pushable, otherwise the entire OR is residual.
func dissectOr(f spi.Filter) (*spi.Filter, *spi.Filter) {
	for _, child := range f.Children {
		if !isFullyPushable(child) {
			return nil, &f
		}
	}
	return &f, nil
}

// isFullyPushable returns true if the entire filter subtree can be pushed to SQL.
func isFullyPushable(f spi.Filter) bool {
	switch f.Op {
	case spi.FilterAnd, spi.FilterOr:
		for _, c := range f.Children {
			if !isFullyPushable(c) {
				return false
			}
		}
		return true
	default:
		return isPushable(f.Op)
	}
}

// isPushable returns true if a leaf operation can be translated to SQL.
func isPushable(op spi.FilterOp) bool {
	switch op {
	case spi.FilterEq, spi.FilterNe, spi.FilterGt, spi.FilterLt,
		spi.FilterGte, spi.FilterLte, spi.FilterContains,
		spi.FilterStartsWith, spi.FilterEndsWith, spi.FilterLike,
		spi.FilterIsNull, spi.FilterNotNull, spi.FilterBetween:
		return true
	}
	return false
}

// toSQL recursively converts a (fully pushable) filter tree to a SQL WHERE
// fragment and bound arguments.
func toSQL(f spi.Filter) (string, []any) {
	switch f.Op {
	case spi.FilterAnd:
		return joinChildren(f.Children, " AND ")
	case spi.FilterOr:
		return joinChildren(f.Children, " OR ")
	default:
		return leafToSQL(f)
	}
}

// joinChildren produces a parenthesized, joined SQL fragment for each child.
func joinChildren(children []spi.Filter, sep string) (string, []any) {
	parts := make([]string, 0, len(children))
	var allArgs []any
	for _, c := range children {
		sql, args := toSQL(c)
		parts = append(parts, "("+sql+")")
		allArgs = append(allArgs, args...)
	}
	return strings.Join(parts, sep), allArgs
}

// directMetaColumns lists SourceMeta paths that map to direct columns in
// the entities table. Paths not in this set live inside the meta JSONB blob
// and require json_extract(json(meta), '$.path').
var directMetaColumns = map[string]bool{
	"entity_id":     true,
	"tenant_id":     true,
	"model_name":    true,
	"model_version": true,
	"version":       true,
	"deleted":       true,
	"created_at":    true,
	"updated_at":    true,
}

// fieldExpr returns the SQL expression for accessing a field.
// SourceMeta "id" resolves to the entity_id column (direct, no json_extract).
// SourceMeta fields matching a canonical lifecycle-filter name (as used by
// post-#423 temporal/lifecycle filters, e.g. "creationDate") are mapped
// through metaBlobKey to their meta-blob storage key — mirroring
// orderByFieldExpr's resolution so filter and ORDER BY agree on where a
// canonical path lives.
// SourceMeta fields with direct columns (storage-key vocabulary, e.g.
// "entity_id") use the column name directly.
// Remaining SourceMeta fields (storage-key vocabulary not backed by a direct
// column, e.g. "state") use json_extract on the meta JSONB column.
// SourceData fields use json_extract on the data BLOB column.
//
// Safety invariant: f.Path is interpolated into a JSON-path literal and
// therefore MUST have been validated by validateFilterPaths at the
// Search() boundary (see path_validation.go). Adding a new caller that
// bypasses Search() re-introduces SQL injection — call validateJSONPath
// or validateFilterPaths before invoking this function.
func fieldExpr(f spi.Filter) string {
	if f.Source == spi.SourceMeta {
		if f.Path == "id" {
			return "entity_id"
		}
		if key, ok := metaBlobKey[f.Path]; ok {
			return jsonExtract("meta", key)
		}
		if directMetaColumns[f.Path] {
			return f.Path
		}
		return fmt.Sprintf("json_extract(json(meta), '$.%s')", f.Path)
	}
	return fmt.Sprintf("json_extract(data, '$.%s')", f.Path)
}

// leafToSQL translates a single leaf filter node to SQL with NULL/3VL handling.
//
// NULL/3VL rules:
//   - is_null/not_null: presence checks, handled first — see below.
//   - eq/gt/lt/gte/lte/between: wrap with IS NOT NULL guard so missing fields
//     don't silently evaluate to NULL (which WHERE would filter out, diverging
//     from Go semantics where missing != value is true)
//   - ne: wrap with IS NULL OR so missing fields match (Go treats missing != value as true)
//   - String ops (contains, starts_with, ends_with): use instr/substr, not LIKE
//   - like: uses LIKE with ESCAPE '\' and value preprocessing
func leafToSQL(f spi.Filter) (string, []any) {
	// Presence checks (IS_NULL/NOT_NULL) are coercion-independent: a raw
	// null-check on fieldExpr(f) is correct regardless of f.Coercion, so
	// they are handled here BEFORE the CoerceTemporal routing below —
	// mirroring spi.evalLeafFilter's ordering. Routing these into
	// temporalLeafToSQL would divide a NULL by 1000 (still NULL, harmless)
	// but then fall through sqlOpForTemporal's op switch, which has no
	// case for IsNull/NotNull and previously silently dropped the
	// predicate (sqlite: emitted "1=1"; postgres: emitted "col = 0").
	switch f.Op {
	case spi.FilterIsNull:
		return fmt.Sprintf("%s IS NULL", fieldExpr(f)), nil
	case spi.FilterNotNull:
		return fmt.Sprintf("%s IS NOT NULL", fieldExpr(f)), nil
	}
	if f.Coercion == spi.CoerceTemporal {
		return temporalLeafToSQL(f)
	}
	col := fieldExpr(f)
	switch f.Op {
	case spi.FilterEq:
		return fmt.Sprintf("(%s IS NOT NULL AND %s = ?)", col, col), []any{f.Value}
	case spi.FilterNe:
		return fmt.Sprintf("(%s IS NULL OR %s != ?)", col, col), []any{f.Value}
	case spi.FilterGt:
		return fmt.Sprintf("(%s IS NOT NULL AND %s > ?)", col, col), []any{f.Value}
	case spi.FilterLt:
		return fmt.Sprintf("(%s IS NOT NULL AND %s < ?)", col, col), []any{f.Value}
	case spi.FilterGte:
		return fmt.Sprintf("(%s IS NOT NULL AND %s >= ?)", col, col), []any{f.Value}
	case spi.FilterLte:
		return fmt.Sprintf("(%s IS NOT NULL AND %s <= ?)", col, col), []any{f.Value}
	case spi.FilterContains:
		return fmt.Sprintf("instr(%s, ?) > 0", col), []any{f.Value}
	case spi.FilterStartsWith:
		return fmt.Sprintf("substr(%s, 1, length(?)) = ?", col), []any{f.Value, f.Value}
	case spi.FilterEndsWith:
		return fmt.Sprintf("substr(%s, -length(?)) = ?", col), []any{f.Value, f.Value}
	case spi.FilterLike:
		return fmt.Sprintf("%s LIKE ? ESCAPE '\\'", col), []any{escapeLike(fmt.Sprint(f.Value))}
	case spi.FilterBetween:
		if len(f.Values) >= 2 {
			return fmt.Sprintf("(%s IS NOT NULL AND %s BETWEEN ? AND ?)", col, col),
				[]any{f.Values[0], f.Values[1]}
		}
		return "1=1", nil
	}
	return "1=1", nil
}

// temporalLeafToSQL translates a CoerceTemporal leaf (spi.Filter.Coercion ==
// spi.CoerceTemporal) into SQL. Presence checks (IsNull/NotNull) never reach
// this function — leafToSQL handles them first, coercion-independent. The
// meta/data blob stores timestamps as microsecond integers, so the field
// expression is divided by 1000 (µs->ms floor) — mirroring
// orderByFieldExpr's OrderTemporal handling exactly, so filter and ORDER BY
// compare the same representation.
//
// Operands are parsed to int64 epoch-ms Go-side via spi.ParseTemporalMillis
// and bound as ordinary ? args (never string-formatted into the SQL text);
// upstream validation guarantees operands here are valid offset-bearing
// RFC3339 instants — a parse failure degrades to ms=0, defensive only.
//
// NULL/3VL rules mirror the non-temporal leaf shapes: BETWEEN/Eq/Gt/Lt/Gte/Lte
// require the column IS NOT NULL (a NULL/unparseable stored value never
// matches a positive comparison); NE uses IS NULL OR != so a NULL/unparseable
// stored value vacuously satisfies "not equal" (matching CompareTemporal's
// vacuous-true-for-NE rule).
func temporalLeafToSQL(f spi.Filter) (string, []any) {
	// SQLite integer division truncates toward zero; postgres' floor()
	// (used by cyoda_epoch_millis in the postgres plugin) truncates toward
	// -inf. The two floors coincide for non-negative operands, which holds
	// here because the engine only ever stores post-1970 (non-negative µs)
	// timestamps — so the cross-backend µs->ms floor is consistent in
	// practice despite the differing primitive semantics.
	col := "(" + fieldExpr(f) + " / 1000)"
	switch f.Op {
	case spi.FilterBetween:
		if len(f.Values) < 2 {
			return "1=1", nil
		}
		lo, _ := spi.ParseTemporalMillis(fmt.Sprint(f.Values[0]))
		hi, _ := spi.ParseTemporalMillis(fmt.Sprint(f.Values[1]))
		return fmt.Sprintf("(%s IS NOT NULL AND %s BETWEEN ? AND ?)", col, col), []any{lo, hi}
	case spi.FilterNe:
		ms, _ := spi.ParseTemporalMillis(fmt.Sprint(f.Value))
		return fmt.Sprintf("(%s IS NULL OR %s != ?)", col, col), []any{ms}
	default:
		sqlOp := sqlOpForTemporal(f.Op)
		if sqlOp == "" {
			return "1=1", nil
		}
		ms, _ := spi.ParseTemporalMillis(fmt.Sprint(f.Value))
		return fmt.Sprintf("(%s IS NOT NULL AND %s %s ?)", col, col, sqlOp), []any{ms}
	}
}

// sqlOpForTemporal maps a comparison FilterOp to its SQL operator for
// temporal leaves. Returns "" for ops that don't reach here: Between/Ne are
// handled separately by temporalLeafToSQL, and IsNull/NotNull are handled
// earlier still, in leafToSQL, before CoerceTemporal routing even applies
// (a presence check is coercion-independent — see leafToSQL's doc comment).
func sqlOpForTemporal(op spi.FilterOp) string {
	switch op {
	case spi.FilterEq:
		return "="
	case spi.FilterGt:
		return ">"
	case spi.FilterLt:
		return "<"
	case spi.FilterGte:
		return ">="
	case spi.FilterLte:
		return "<="
	}
	return ""
}

// escapeLike escapes LIKE wildcards (%, _, \) in a user-provided value
// so they are treated as literal characters with ESCAPE '\'.
func escapeLike(s string) string {
	s = strings.ReplaceAll(s, "\\", "\\\\")
	s = strings.ReplaceAll(s, "%", "\\%")
	s = strings.ReplaceAll(s, "_", "\\_")
	return s
}
