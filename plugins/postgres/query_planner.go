package postgres

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// sqlPlan holds the result of translating a spi.Filter into SQL.
// where + args represent the pushable portion as a SQL WHERE fragment.
// postFilter is the residual filter that must be evaluated in Go.
//
// This mirrors the sqlite plugin's sqlPlan in shape so that cross-backend
// parity tests in e2e/parity/ can assert identical pushable/residual splits.
//
// SQL-pushdown soundness contract: the pushed SQL WHERE is a best-effort
// NARROWING — the kernel (spi.MatchFilter, re-run over the candidates the SQL
// returns) is authoritative. The invariant is that the pushed SQL returns a
// SUPERSET of the kernel's matches (it never misses one). A leaf is EXACT when
// its SQL matches the kernel bit-for-bit (only IsNull/NotNull — see leafExact);
// every other pushed leaf is at best a SOUND SUPERSET (float8/text SQL can
// over-select relative to the precise bignum/temporal kernel, so the kernel
// must re-check). The SQL LIMIT/OFFSET/GROUP-BY fast path (gated on
// postFilter == nil) is allowed ONLY when the whole plan is exact.
type sqlPlan struct {
	where      string
	args       []any
	postFilter *spi.Filter
}

// leafExact reports whether a pushed leaf's SQL matches the kernel bit-for-bit.
// Only presence checks (IsNull/NotNull) are exact: SQL IS NULL / IS NOT NULL is
// identical to the kernel's absent/null semantics. Every other pushable op is at
// best a SOUND SUPERSET (see leafToSQL), so its candidates must be re-checked by
// the kernel. Mirrors sqlite's leafExact exactly.
func leafExact(op spi.FilterOp) bool {
	return op == spi.FilterIsNull || op == spi.FilterNotNull
}

// allPushedExact walks a pushed filter subtree (as produced by dissect) and
// reports whether every leaf in it is leafExact. AND/OR branches recurse.
// Used by planQuery to decide whether the SQL is provably exact (fast path) or
// must trigger a full kernel re-check.
func allPushedExact(f spi.Filter) bool {
	switch f.Op {
	case spi.FilterAnd, spi.FilterOr:
		for _, c := range f.Children {
			if !allPushedExact(c) {
				return false
			}
		}
		return true
	default:
		return leafExact(f.Op)
	}
}

// planQuery translates a spi.Filter tree into a SQL WHERE clause and an
// optional residual filter for post-processing in Go.
//
// Dissection strategy (must match sqlite's planQuery exactly so parity tests
// see the same pushable/residual split given the same input):
//   - Greedy AND: extract pushable children into SQL, collect non-pushable as residual
//   - Conservative OR: only push down if ALL children are pushable, otherwise entire OR is residual
//   - Leaf nodes: pushable ops produce SQL fragments; non-pushable become residual
//
// Soundness gate: unless the plan is fully EXACT — no residual AND every pushed
// leaf satisfies leafExact — the FULL original filter is installed as postFilter
// so the kernel re-checks every candidate the narrowing SQL returns. This also
// disables the SQL LIMIT/OFFSET/GROUP-BY fast path (gated on postFilter == nil).
func planQuery(filter spi.Filter) sqlPlan {
	pushed, residual := dissect(filter)
	plan := sqlPlan{postFilter: residual}
	if pushed != nil {
		argCounter := 0
		plan.where, plan.args = toSQL(*pushed, &argCounter)
	}
	// If the plan is not provably exact (there is a residual, or any pushed leaf
	// is only a SOUND SUPERSET), the narrowing SQL may over-select, so the kernel
	// must re-check every candidate against the FULL original filter.
	if residual != nil || (pushed != nil && !allPushedExact(*pushed)) {
		full := filter
		plan.postFilter = &full
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

// isPushable returns true if a leaf operation can be translated to SQL as a
// sound superset of the kernel (see the sqlPlan soundness contract).
//
// Ne is deliberately NOT pushable: SQL "!=" UNDER-selects under float8/text
// collision (a value != operand precisely but float8/text-equal is a kernel
// match the SQL "!=" wrongly excludes), and "!=" rarely narrows anyway — so Ne
// is residual-only (kernel-evaluated). BetweenInclusive IS pushable: SQL BETWEEN
// is inclusive [lo,hi], a sound superset of the inclusive kernel between (and,
// by float8 monotonicity, of any value the kernel matches).
//
// Like is deliberately NOT pushable (as of this commit): SQL LIKE's '%'/'_'
// wildcards do not line up with Cloud's LIKE grammar (spi.MatchFilter's
// likeToRegex), so a naive pushdown either escapes the wildcards into a
// literal match (under-selecting real wildcard patterns) or pushes them
// through unescaped (over-selecting/misinterpreting SQL-LIKE-specific
// escaping). A sound SQL-LIKE translation that aligns SQL LIKE to Cloud's
// grammar is deferred to a dedicated follow-up; until then Like is
// residual-only so the kernel evaluates it correctly. leafToSQL's LIKE
// branch is kept below (unreachable via isPushable, like Ne) for mirror
// totality with sqlite.
//
// IMPORTANT: this set MUST match sqlite's isPushable exactly. Adding or
// removing an op here without doing the same in sqlite breaks the parity
// invariant relied on by the cross-backend tests in e2e/parity/.
func isPushable(op spi.FilterOp) bool {
	switch op {
	case spi.FilterEq, spi.FilterGt, spi.FilterLt,
		spi.FilterGte, spi.FilterLte, spi.FilterContains,
		spi.FilterStartsWith, spi.FilterEndsWith,
		spi.FilterIsNull, spi.FilterNotNull,
		spi.FilterBetween, spi.FilterBetweenInclusive:
		return true
	}
	return false
}

// toSQL recursively converts a (fully pushable) filter tree to a SQL WHERE
// fragment and bound arguments. argCounter is a monotonically increasing
// placeholder index used to generate $1, $2, ... — it MUST be shared across
// the whole tree so each leaf gets a unique placeholder number.
func toSQL(f spi.Filter, argCounter *int) (string, []any) {
	switch f.Op {
	case spi.FilterAnd:
		return joinChildren(f.Children, " AND ", argCounter)
	case spi.FilterOr:
		return joinChildren(f.Children, " OR ", argCounter)
	default:
		return leafToSQL(f, argCounter)
	}
}

// joinChildren produces a parenthesized, joined SQL fragment for each child.
func joinChildren(children []spi.Filter, sep string, argCounter *int) (string, []any) {
	parts := make([]string, 0, len(children))
	var allArgs []any
	for _, c := range children {
		sql, args := toSQL(c, argCounter)
		parts = append(parts, "("+sql+")")
		allArgs = append(allArgs, args...)
	}
	return strings.Join(parts, sep), allArgs
}

// directMetaColumns lists SourceMeta paths that map to direct columns on the
// entities table. Paths not in this set live inside the doc JSONB blob under
// the _meta key and require doc->'_meta'->>'path' extraction.
//
// Note: this set is smaller than sqlite's because postgres stores fewer fields
// as direct columns (state/created_at/updated_at live in the _meta JSONB
// block here, not as separate columns).
var directMetaColumns = map[string]bool{
	"entity_id":     true,
	"tenant_id":     true,
	"model_name":    true,
	"model_version": true,
	"version":       true,
	"deleted":       true,
}

// fieldExpr returns the SQL expression for accessing a field's text value.
//
// SourceMeta resolution order (mirrors orderByFieldExpr in searcher.go,
// which resolves the same canonical meta paths for ORDER BY — do not
// maintain a second mapping):
//  1. Path "id" → the entity_id column directly (not in metaJSONKey).
//  2. Path in metaJSONKey (canonical lifecycle-filter name, e.g.
//     "creationDate") → doc->'_meta'->>'<storage-key>' using the mapped
//     storage key (e.g. "creation_date"). Filter paths reaching this
//     function are always canonical post-validation (ConditionToFilter /
//     lifecycleToFilter build them, and unknown meta fields are rejected
//     upstream), so this is the common case for meta filters.
//  3. Path in directMetaColumns (internal/direct-column paths such as
//     "entity_id", "tenant_id", "version" used by non-search internal
//     filters) → the column name directly. Kept as a defensive fallback so
//     existing internal callers using storage-column names don't regress.
//  4. Otherwise → raw doc->'_meta'->>'<path>' (unreachable for validated
//     search filters; kept total for defensiveness).
//
// SourceData paths are extracted as doc->>'<path>' (or doc->'a'->>'b' for
// nested dotted paths).
//
// Safety invariant: f.Path is interpolated into single-quoted JSON-key
// literals and therefore MUST have been validated by validateFilterPaths at
// the Search() boundary (see path_validation.go, when added). The validator
// rejects any character that could terminate a quoted literal.
func fieldExpr(f spi.Filter) string {
	if f.Source == spi.SourceMeta {
		if f.Path == "id" {
			return "entity_id"
		}
		if key, ok := metaJSONKey[f.Path]; ok {
			return jsonbExtractText("doc->'_meta'", key)
		}
		if directMetaColumns[f.Path] {
			return f.Path
		}
		// Meta path inside the JSONB _meta block.
		return jsonbExtractText("doc->'_meta'", f.Path)
	}
	return jsonbExtractText("doc", f.Path)
}

// jsonbExtractText returns a SQL expression that extracts the dotted path as
// text from a JSONB root expression. For a single segment, uses ->>; for
// multiple segments, uses -> for all but the last and ->> for the last.
func jsonbExtractText(root, path string) string {
	segments := strings.Split(path, ".")
	if len(segments) == 1 {
		return fmt.Sprintf("%s->>'%s'", root, segments[0])
	}
	var b strings.Builder
	b.WriteString(root)
	for i, seg := range segments {
		if i == len(segments)-1 {
			fmt.Fprintf(&b, "->>'%s'", seg)
		} else {
			fmt.Fprintf(&b, "->'%s'", seg)
		}
	}
	return b.String()
}

// jsonbExtractJSONB returns a SQL expression that extracts the dotted path
// as JSONB (NOT text) from a JSONB root expression — every segment uses ->.
// Used to feed jsonb_typeof for D4 non-scalar coercion in grouped-stats
// group-key expressions; jsonb_typeof needs a jsonb input, not text.
func jsonbExtractJSONB(root, path string) string {
	segments := strings.Split(path, ".")
	var b strings.Builder
	b.WriteString(root)
	for _, seg := range segments {
		fmt.Fprintf(&b, "->'%s'", seg)
	}
	return b.String()
}

// isNumericValue reports whether v is a Go numeric type (int*/uint*/float*)
// or a json.Number — the SPI's predicate parser decodes numeric search
// operands as json.Number (a string-kind type, via json.Decoder.UseNumber)
// to preserve precision losslessly, so it must route through the numeric
// branch exactly like a native Go numeric would, not the lexical branch a
// string-kind type would otherwise fall to.
// Numeric values use cyoda_try_float8 + ::float8 for safe overflow-free
// comparisons; non-numeric values use lexicographic text comparison.
func isNumericValue(v any) bool {
	switch v.(type) {
	case int, int8, int16, int32, int64,
		uint, uint8, uint16, uint32, uint64,
		float32, float64,
		json.Number:
		return true
	}
	return false
}

// numericArg converts an operand already routed through the numeric branch
// (isNumericValue(v) == true) into a value pgx can encode against a
// ::float8 cast. A json.Number is a string-kind type with no bool/float8
// encode plan of its own — pgx needs an actual Go numeric — so it is
// parsed to float64 here (cyoda_try_float8's own arithmetic is float8, so
// no precision is lost by this conversion beyond what float8 already
// bounds). Native Go numeric types pass through unchanged.
func numericArg(v any) any {
	if n, ok := v.(json.Number); ok {
		f, err := n.Float64()
		if err != nil {
			// Unreachable in practice: the SPI parser only ever produces a
			// json.Number from a syntactically valid JSON number literal,
			// so Float64() cannot fail. Fail closed rather than bind a
			// malformed value: this leaf never matches.
			return nil
		}
		return f
	}
	return v
}

// textArg normalizes an operand for binding against a text-typed doc->>'path'
// extraction. A Go bool cannot be encoded into text (OID 25) by pgx — it has no
// bool->text encode plan — so a boolean operand is rendered as its text form
// ("true"/"false"), which is exactly how doc->>'path' renders a stored JSON
// boolean. This mirrors the lexicographic text comparison used for strings and
// the memory/sqlite backends. Non-bool values pass through unchanged.
func textArg(v any) any {
	if b, ok := v.(bool); ok {
		return strconv.FormatBool(b)
	}
	return v
}

// orderExpr returns the SQL expression used as the LHS of an ordering
// comparison (Gt/Lt/Gte/Lte/Between). For numeric values it wraps the field
// in cyoda_try_float8(...) so overflow/non-numeric content returns NULL
// rather than raising. For string values it uses the plain text expression.
// The bool result indicates whether numeric mode was used (callers append
// ::float8 to the placeholder accordingly).
func orderExpr(f spi.Filter, numeric bool) string {
	col := fieldExpr(f)
	if numeric {
		// Only the JSONB-extracted forms need the wrapper; for direct columns
		// (entity_id, version, etc.) we still wrap for consistency — the
		// function is total over text input.
		return fmt.Sprintf("cyoda_try_float8(%s)", col)
	}
	return col
}

// nextPlaceholder bumps the counter and returns the corresponding $N token.
func nextPlaceholder(counter *int) string {
	*counter++
	return fmt.Sprintf("$%d", *counter)
}

// leafToSQL translates a single leaf filter node to SQL with NULL/3VL handling.
//
// NULL/3VL rules (mirrored from sqlite):
//   - is_null/not_null: presence checks, handled first — see below.
//   - eq/gt/lt/gte/lte/between: wrap with IS NOT NULL guard so missing fields
//     don't silently evaluate to NULL (which WHERE would filter out, diverging
//     from Go semantics where missing != value is true)
//   - ne: wrap with IS NULL OR so missing fields match (Go treats missing != value as true)
//   - String ops (contains, starts_with, ends_with): use strpos/substr/right, not LIKE
//   - like: uses LIKE with ESCAPE '\' and value preprocessing
//
// Numeric eq/ne and ordering ops route the field expression through
// cyoda_try_float8 and cast the placeholder to float8 so overflow/non-numeric
// content returns NULL rather than raising 22003, and so a numeric operand is
// compared numerically against the text-typed doc->>'path' extraction (a raw
// numeric bind against a text column fails to encode) — the regex+EXCEPTION
// helper is defined in migration 000002. String values keep text comparison;
// boolean operands are rendered to their text form ("true"/"false") via textArg
// so they encode against the text-typed extraction (see textArg).
func leafToSQL(f spi.Filter, counter *int) (string, []any) {
	// Presence checks (IS_NULL/NOT_NULL) are coercion-independent: a raw
	// null-check on fieldExpr(f) is correct regardless of f.Coercion, so
	// they are handled here BEFORE the CoerceTemporal routing below —
	// mirroring spi.evalLeafFilter's ordering. Routing these into
	// temporalLeafToSQL would fall into its default branch, whose
	// sqlOpForTemporal has no case for IsNull/NotNull and falls back to
	// "=" — previously silently corrupting the predicate into
	// "cyoda_epoch_millis(col) = $1" bound to ms=0 (1970-01-01), matching
	// every non-null row instead of performing a null check.
	switch f.Op {
	case spi.FilterIsNull:
		return fmt.Sprintf("%s IS NULL", fieldExpr(f)), nil
	case spi.FilterNotNull:
		return fmt.Sprintf("%s IS NOT NULL", fieldExpr(f)), nil
	}
	if f.Coercion == spi.CoerceTemporal {
		return temporalLeafToSQL(f, counter)
	}
	switch f.Op {
	case spi.FilterEq:
		// Numeric operand: cyoda_try_float8 coerces the field to float8, so a field stored
		// as a numeric-looking string (e.g. "30") coerces and matches — intentional, matching
		// sqlite's type-coercing comparison and the S4 numeric-equality intent; string operands
		// use plain text comparison.
		if isNumericValue(f.Value) {
			col := orderExpr(f, true)
			p := nextPlaceholder(counter)
			return fmt.Sprintf("(%s IS NOT NULL AND %s = %s::float8)", col, col, p), []any{numericArg(f.Value)}
		}
		col := fieldExpr(f)
		p := nextPlaceholder(counter)
		return fmt.Sprintf("(%s IS NOT NULL AND %s = %s)", col, col, p), []any{textArg(f.Value)}
	case spi.FilterNe:
		if isNumericValue(f.Value) {
			col := orderExpr(f, true)
			p := nextPlaceholder(counter)
			return fmt.Sprintf("(%s IS NULL OR %s != %s::float8)", col, col, p), []any{numericArg(f.Value)}
		}
		col := fieldExpr(f)
		p := nextPlaceholder(counter)
		return fmt.Sprintf("(%s IS NULL OR %s != %s)", col, col, p), []any{textArg(f.Value)}
	case spi.FilterGt:
		// SOUND SUPERSET: strict ">" would UNDER-select — a stored value a
		// sub-ULP beyond the operand collides to the same float8, so ">" wrongly
		// excludes a kernel match. Relax to ">=" (float8 rounding is monotonic,
		// so x > operand ⟹ float8(x) >= float8(operand)); the kernel re-check
		// removes the boundary false-positives.
		return orderingOp(f, ">=", counter)
	case spi.FilterLt:
		// SOUND SUPERSET: strict "<" relaxed to "<=" for the same reason.
		return orderingOp(f, "<=", counter)
	case spi.FilterGte:
		return orderingOp(f, ">=", counter)
	case spi.FilterLte:
		return orderingOp(f, "<=", counter)
	case spi.FilterContains:
		col := fieldExpr(f)
		p := nextPlaceholder(counter)
		return fmt.Sprintf("strpos(%s, %s) > 0", col, p), []any{fmt.Sprint(f.Value)}
	case spi.FilterStartsWith:
		col := fieldExpr(f)
		p1 := nextPlaceholder(counter)
		p2 := nextPlaceholder(counter)
		sv := fmt.Sprint(f.Value)
		return fmt.Sprintf("substr(%s, 1, length(%s)) = %s", col, p1, p2), []any{sv, sv}
	case spi.FilterEndsWith:
		// postgres substr(col, -N) does NOT mean "last N characters" the way
		// sqlite's does — postgres substr's start position is 1-indexed and a
		// non-positive start just shifts the (clamped) window forward from
		// position 1, returning the whole remaining string. right(col, N) is
		// postgres's actual "last N characters" primitive, so use that
		// instead (sqlite keeps its substr(col, -N) form — see this file's
		// sibling in plugins/sqlite, which is correct there).
		col := fieldExpr(f)
		p1 := nextPlaceholder(counter)
		p2 := nextPlaceholder(counter)
		sv := fmt.Sprint(f.Value)
		return fmt.Sprintf("right(%s, char_length(%s)) = %s", col, p1, p2), []any{sv, sv}
	case spi.FilterLike:
		col := fieldExpr(f)
		p := nextPlaceholder(counter)
		return fmt.Sprintf("%s LIKE %s ESCAPE '\\'", col, p), []any{escapeLike(fmt.Sprint(f.Value))}
	case spi.FilterBetween, spi.FilterBetweenInclusive:
		// SOUND SUPERSET: SQL BETWEEN is inclusive [lo,hi]. For the inclusive
		// kernel between that is exact-on-value; for the exclusive kernel between
		// (FilterBetween) the inclusive SQL is a strict superset (kernel re-check
		// enforces the open bounds). float8 rounding is monotonic, so a value the
		// kernel matches always falls within the float8 [lo,hi] the SQL tests.
		if len(f.Values) >= 2 {
			numeric := isNumericValue(f.Values[0]) && isNumericValue(f.Values[1])
			col := orderExpr(f, numeric)
			p1 := nextPlaceholder(counter)
			p2 := nextPlaceholder(counter)
			if numeric {
				return fmt.Sprintf("(%s IS NOT NULL AND %s BETWEEN %s::float8 AND %s::float8)",
					col, col, p1, p2), []any{numericArg(f.Values[0]), numericArg(f.Values[1])}
			}
			return fmt.Sprintf("(%s IS NOT NULL AND %s BETWEEN %s AND %s)",
				col, col, p1, p2), []any{textArg(f.Values[0]), textArg(f.Values[1])}
		}
		// Malformed BETWEEN (not exactly 2 operands) fails closed — exclude
		// every row, matching memory's spi.MatchFilter semantics. Validation
		// upstream (search.validateBetweenArity) rejects this shape before it
		// ever reaches a plugin; this is defense-in-depth only.
		return "false", nil
	}
	return "1=1", nil
}

// temporalLeafToSQL translates a CoerceTemporal leaf (spi.Filter.Coercion ==
// spi.CoerceTemporal) into SQL. Presence checks (IsNull/NotNull) never reach
// this function — leafToSQL handles them first, coercion-independent. The
// field expression is wrapped in cyoda_epoch_millis(...) (migration 000005)
// so chronological comparisons
// compare as bigint epoch-ms rather than lexicographic text — matching the
// canonical temporal-scalar rule shared by spi.ParseTemporalMillis /
// spi.CompareTemporal and the sqlite/memory Go evaluators.
//
// Operands are parsed to int64 epoch-ms Go-side via spi.ParseTemporalMillis
// and bound as ordinary $N args (never string-formatted into the SQL text) —
// cyoda_epoch_millis is NULL-safe/total, so a malformed operand simply binds
// as 0 with ok=false discarded; upstream validation guarantees operands here
// are valid offset-bearing RFC3339 instants, so this is defensive only.
//
// NULL/3VL rules mirror the non-temporal leaf shapes: BETWEEN/Eq/Gt/Lt/Gte/Lte
// require the column IS NOT NULL (a NULL/unparseable stored value never
// matches a positive comparison); NE uses IS NULL OR != so a NULL/unparseable
// stored value vacuously satisfies "not equal" (matching CompareTemporal's
// vacuous-true-for-NE rule).
func temporalLeafToSQL(f spi.Filter, counter *int) (string, []any) {
	col := "cyoda_epoch_millis(" + fieldExpr(f) + ")"
	switch f.Op {
	case spi.FilterBetween, spi.FilterBetweenInclusive:
		if len(f.Values) < 2 {
			// Malformed BETWEEN (not exactly 2 operands) fails closed —
			// exclude every row, matching memory's spi.MatchFilter
			// semantics, and never index f.Values out of range. Validation
			// upstream (search.validateBetweenArity) rejects this shape
			// before it ever reaches a plugin; this is defense-in-depth only.
			return "false", nil
		}
		lo, _ := spi.ParseTemporalMillis(fmt.Sprint(f.Values[0]))
		hi, _ := spi.ParseTemporalMillis(fmt.Sprint(f.Values[1]))
		p1 := nextPlaceholder(counter)
		p2 := nextPlaceholder(counter)
		return fmt.Sprintf("(%s IS NOT NULL AND %s BETWEEN %s AND %s)", col, col, p1, p2), []any{lo, hi}
	case spi.FilterNe:
		ms, _ := spi.ParseTemporalMillis(fmt.Sprint(f.Value))
		p := nextPlaceholder(counter)
		return fmt.Sprintf("(%s IS NULL OR %s != %s)", col, col, p), []any{ms}
	default:
		ms, _ := spi.ParseTemporalMillis(fmt.Sprint(f.Value))
		p := nextPlaceholder(counter)
		return fmt.Sprintf("(%s IS NOT NULL AND %s %s %s)", col, col, sqlOpForTemporal(f.Op), p), []any{ms}
	}
}

// sqlOpForTemporal maps a comparison FilterOp to its SQL operator for the
// default (non-BETWEEN, non-NE) branch of temporalLeafToSQL. IsNull/NotNull
// never reach here — leafToSQL intercepts them before CoerceTemporal routing
// even applies (see leafToSQL's doc comment), so this function's domain is
// Eq/Gt/Lt/Gte/Lte only.
func sqlOpForTemporal(op spi.FilterOp) string {
	switch op {
	case spi.FilterGt:
		// SOUND SUPERSET: relaxed from strict ">" to ">=" (the kernel does
		// temporal-subtype resolution the SQL doesn't, so the strict form can
		// UNDER-select; the full-filter kernel re-check enforces strictness).
		return ">="
	case spi.FilterLt:
		return "<="
	case spi.FilterGte:
		return ">="
	case spi.FilterLte:
		return "<="
	default: // spi.FilterEq
		return "="
	}
}

// orderingOp emits a comparison clause for Gt/Lt/Gte/Lte. Numeric values
// route through cyoda_try_float8 with a ::float8 cast on the placeholder;
// string values use plain text comparison.
func orderingOp(f spi.Filter, sqlOp string, counter *int) (string, []any) {
	numeric := isNumericValue(f.Value)
	col := orderExpr(f, numeric)
	p := nextPlaceholder(counter)
	if numeric {
		return fmt.Sprintf("(%s IS NOT NULL AND %s %s %s::float8)", col, col, sqlOp, p), []any{numericArg(f.Value)}
	}
	return fmt.Sprintf("(%s IS NOT NULL AND %s %s %s)", col, col, sqlOp, p), []any{textArg(f.Value)}
}

// escapeLike escapes LIKE wildcards (%, _, \) in a user-provided value
// so they are treated as literal characters with ESCAPE '\'.
func escapeLike(s string) string {
	s = strings.ReplaceAll(s, "\\", "\\\\")
	s = strings.ReplaceAll(s, "%", "\\%")
	s = strings.ReplaceAll(s, "_", "\\_")
	return s
}
