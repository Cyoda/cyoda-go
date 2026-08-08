package sqlite

import (
	"encoding/json"
	"fmt"
	"strings"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// sqlPlan holds the result of translating a spi.Filter into SQL.
// where + args represent the pushable portion as a SQL WHERE fragment.
// postFilter is the residual filter that must be evaluated in Go.
//
// SQL-pushdown soundness contract: the pushed SQL WHERE is a best-effort
// NARROWING — the kernel (spi.Prepare/PreparedFilter.Match, re-run over the
// candidates the SQL returns) is authoritative. The invariant is that the
// pushed SQL returns a SUPERSET of the kernel's matches (it never misses
// one). A leaf is EXACT when its SQL matches the kernel bit-for-bit (only
// IsNull/NotNull — see leafExact); every other pushed leaf is at best a
// SOUND SUPERSET (SQLite storage-class / text comparison can over-select
// relative to the precise bignum/temporal kernel, so the kernel must
// re-check). The SQL LIMIT/OFFSET/GROUP-BY fast path (gated on postFilter ==
// nil) is allowed ONLY when the whole plan is exact.
type sqlPlan struct {
	where      string
	args       []any
	postFilter *spi.Filter
	// preparedPostFilter is postFilter compiled for per-row evaluation. It is
	// non-nil EXACTLY when postFilter is non-nil.
	//
	// postFilter itself stays a *spi.Filter and stays the field the planner's
	// own predicates read, because its NIL-NESS is what gates LIMIT pushdown,
	// native GROUP BY and the scan budget. A zero spi.PreparedFilter means
	// match-all, not absent, so replacing the field outright — or pairing a
	// value with a bool — would put that invariant back in play at every
	// consumer. Row loops read this field; planner decisions read postFilter.
	preparedPostFilter *spi.PreparedFilter
}

// leafExact reports whether a pushed leaf's SQL matches the kernel bit-for-bit.
// Only presence checks (IsNull/NotNull) are exact: SQL IS NULL / IS NOT NULL is
// identical to the kernel's absent/null semantics. Every other pushable op is at
// best a SOUND SUPERSET (see leafToSQL), so its candidates must be re-checked by
// the kernel. Mirrors postgres's leafExact exactly.
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
// Dissection strategy:
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
		plan.where, plan.args = toSQL(*pushed)
	}
	// If the plan is not provably exact (there is a residual, or any pushed leaf
	// is only a SOUND SUPERSET), the narrowing SQL may over-select, so the kernel
	// must re-check every candidate against the FULL original filter.
	if residual != nil || (pushed != nil && !allPushedExact(*pushed)) {
		full := filter
		plan.postFilter = &full
	}
	// Single population point, so the nil-ness invariant cannot drift between
	// the branches above.
	if plan.postFilter != nil {
		p := spi.Prepare(*plan.postFilter)
		plan.preparedPostFilter = &p
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
		if isLeafPushable(f) {
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
		return isLeafPushable(f)
	}
}

// isPushable returns true if a leaf operation can be translated to SQL as a
// sound superset of the kernel (see the sqlPlan soundness contract).
//
// Ne is deliberately NOT pushable: SQL "!=" UNDER-selects under storage-class /
// text collision (a value != operand precisely but SQLite-equal is a kernel
// match the SQL "!=" wrongly excludes), and "!=" rarely narrows anyway — so Ne
// is residual-only (kernel-evaluated). BetweenInclusive IS pushable: SQL BETWEEN
// is inclusive [lo,hi], a sound superset of the inclusive kernel between.
//
// Like is deliberately NOT pushable (as of this commit): SQL LIKE's '%'/'_'
// wildcards do not line up with Cloud's LIKE grammar (the kernel's
// likeToRegex, cyoda-go-spi eval_leaf.go), so a naive pushdown either escapes
// the wildcards into a literal match (under-selecting real wildcard patterns)
// or pushes them through unescaped (over-selecting/misinterpreting
// SQL-LIKE-specific escaping). A sound SQL-LIKE translation that aligns SQL
// LIKE to Cloud's grammar is deferred to a dedicated follow-up; until then
// Like is residual-only so the kernel evaluates it correctly. leafToSQL's LIKE
// branch is kept below (unreachable via isPushable, like Ne) for mirror
// totality with postgres.
//
// IMPORTANT: this OP-LEVEL set MUST match postgres's isPushable exactly.
// Adding or removing an op here without doing the same in postgres breaks the
// op-level parity invariant relied on by the cross-backend tests in
// e2e/parity/. The LEAF-LEVEL pushability decision (isLeafPushable) may
// diverge from postgres deliberately — see its doc comment.
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

// isComparisonOp reports whether op is a scalar comparison whose SQL bind is
// sensitive to the stored value's storage class (Eq/Ne/Gt/Lt/Gte/Lte/Between).
// String ops (Contains/StartsWith/EndsWith) and presence checks
// (IsNull/NotNull) are NOT comparisons in this sense and are unaffected by the
// type-family routing.
func isComparisonOp(op spi.FilterOp) bool {
	switch op {
	case spi.FilterEq, spi.FilterNe, spi.FilterGt, spi.FilterLt,
		spi.FilterGte, spi.FilterLte, spi.FilterBetween, spi.FilterBetweenInclusive:
		return true
	}
	return false
}

// isLeafPushable is the LEAF-LEVEL pushability decision: it layers a
// type-family check on top of the op-level isPushable. A comparison leaf
// (isComparisonOp) whose DECLARED type set is polymorphic (len > 1, i.e. its
// stored values may span different type families / SQLite storage classes) is
// NOT pushable on sqlite: json_extract preserves each stored scalar's native
// storage class and SQLite never equates different classes (30 = '30' is
// false), so no single-storage-class-bound SQL predicate can be a SUPERSET of
// every kernel branch. Such leaves are routed to the residual, where the
// kernel (spi.Prepare/PreparedFilter.Match) evaluates all branches correctly.
//
// DELIBERATE MIRROR DIVERGENCE from postgres: postgres's `->>` extraction
// stringifies every stored scalar to text, so a single text bind IS already a
// sound superset across storage classes there — postgres keeps pushing these
// leaves. The mirror contract requires identical RESULTS (the kernel is
// authoritative on both backends), NOT identical pushed WHERE clauses; the
// spec explicitly permits sqlite to push FEWER leaves. Presence checks
// (IsNull/NotNull) are type-family-independent and stay pushable. Temporal
// leaves (Coercion == CoerceTemporal) compare as a single normalized epoch-ms
// integer regardless of declared subtype, so the polymorphic rule does not
// apply to them either.
func isLeafPushable(f spi.Filter) bool {
	if !isPushable(f.Op) {
		return false
	}
	if f.Coercion == spi.CoerceTemporal {
		// Meta temporal fields store a single full instant (a µs-integer here /
		// an offset-bearing RFC3339 string on postgres) that the epoch-ms push
		// (temporalLeafToSQL) compares soundly. DATA temporal fields store bare
		// ISO-subtype strings whose comparison needs the kernel's temporal-
		// subtype resolution — an imprecise-floor op mutation (e.g. `>=
		// 2024-09-09` on a Year field becomes `> 2024`) that a flat epoch-ms
		// compare cannot reproduce as a sound superset. Route data temporal
		// COMPARISONS to the residual, where the kernel
		// (spi.Prepare/PreparedFilter.Match) is authoritative; presence checks
		// (IsNull/NotNull) are coercion-independent and stay pushable.
		if f.Source == spi.SourceData && isComparisonOp(f.Op) {
			return false
		}
		return true
	}
	if isComparisonOp(f.Op) && len(f.Declared) > 1 {
		return false
	}
	return true
}

// comparisonBind returns the operand-binding form for a NON-temporal
// comparison leaf, directed by the STORAGE CLASS json_extract yields for the
// leaf's DECLARED type family rather than by the operand's own Go/JSON kind,
// so the pushed SQL is a sound superset of the kernel on SQLite (where
// json_extract preserves each stored scalar's native storage class):
//   - monomorphic TEXT-storage-class declared (String/Character/UUID/…, see
//     isTextStorageClass) -> TEXT bind (the normalized text form of the
//     operand), so a json.Number operand text-compares against the text-stored
//     scalar a text field yields (json_extract('"30"') is TEXT '30').
//   - everything else (numeric, Boolean, empty/unstamped) -> bindArg (native):
//     an integral json.Number becomes int64 / a fractional one float64,
//     matching the INTEGER/REAL json_extract yields for a numeric field; a Go
//     bool binds to the INTEGER 1/0 that json_extract yields for a JSON
//     boolean (json_extract('true') is 1, NOT the text 'true' — so a Boolean
//     field must NOT text-bind). This is the existing large-int/float-precision
//     path, unchanged.
//
// Polymorphic leaves (len(Declared) > 1) never reach here — isLeafPushable
// routes them to the residual.
func comparisonBind(f spi.Filter, v any) any {
	if len(f.Declared) == 1 && isTextStorageClass(f.Declared[0]) {
		return operandText(v)
	}
	return bindArg(v)
}

// isTextStorageClass reports whether json_extract yields a SQLite TEXT storage
// class for a stored value of declared type dt. These are the JSON-string-
// serialized families: a text bind is needed so a numeric-looking operand
// (json.Number) does not miss the text-stored scalar. Numeric types yield
// INTEGER/REAL and Boolean yields INTEGER 1/0 — neither is text, so both keep
// the native bindArg path.
func isTextStorageClass(dt spi.DataType) bool {
	switch dt {
	case spi.String, spi.Character, spi.UUIDType, spi.TimeUUIDType, spi.ByteArray:
		return true
	}
	return false
}

// operandText normalizes an operand to its text form for a TEXT bind, mirroring
// the SPI's operandString for the kinds a search operand can take (json.Number
// keeps its exact lexical form; a bool becomes "true"/"false"; nil becomes the
// empty string). A json.Number is a string-kind type, so fmt.Sprint yields its
// literal digits.
func operandText(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case bool:
		if x {
			return "true"
		}
		return "false"
	default:
		return fmt.Sprint(x)
	}
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

// bindArg normalizes an operand for binding as a `?` placeholder. The SPI's
// predicate parser decodes numeric search operands as json.Number (a
// string-kind type, via json.Decoder.UseNumber) to preserve precision
// losslessly. database/sql has no driver.Valuer for json.Number, so it
// binds one raw as TEXT — flipping SQLite's storage-class comparison from
// numeric to lexicographic and diverging from the memory/SPI kernel, which
// compares the underlying numeric value regardless of Go kind. Converting
// to a native numeric Go value here restores INTEGER/REAL affinity. Integral
// values convert to int64 (matching how the engine stores whole-number JSON
// fields); fractional values convert to float64. Non-json.Number operands
// pass through unchanged — this only ever changes json.Number binding.
func bindArg(v any) any {
	n, ok := v.(json.Number)
	if !ok {
		return v
	}
	if i, err := n.Int64(); err == nil {
		return i
	}
	f, err := n.Float64()
	if err != nil {
		// Unreachable in practice: the SPI parser only ever produces a
		// json.Number from a syntactically valid JSON number literal, so
		// neither Int64() nor Float64() can fail on it. Fail closed rather
		// than bind a malformed value: this leaf never matches.
		return nil
	}
	return f
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
		return fmt.Sprintf("(%s IS NOT NULL AND %s = ?)", col, col), []any{comparisonBind(f, f.Value)}
	case spi.FilterNe:
		return fmt.Sprintf("(%s IS NULL OR %s != ?)", col, col), []any{comparisonBind(f, f.Value)}
	case spi.FilterGt:
		// SOUND SUPERSET: strict ">" would UNDER-select — a stored value a
		// sub-ULP beyond the operand collides to the same REAL, so ">" wrongly
		// excludes a kernel match. Relax to ">=" (float rounding is monotonic);
		// the kernel re-check removes the boundary false-positives.
		return fmt.Sprintf("(%s IS NOT NULL AND %s >= ?)", col, col), []any{comparisonBind(f, f.Value)}
	case spi.FilterLt:
		// SOUND SUPERSET: strict "<" relaxed to "<=" for the same reason.
		return fmt.Sprintf("(%s IS NOT NULL AND %s <= ?)", col, col), []any{comparisonBind(f, f.Value)}
	case spi.FilterGte:
		return fmt.Sprintf("(%s IS NOT NULL AND %s >= ?)", col, col), []any{comparisonBind(f, f.Value)}
	case spi.FilterLte:
		return fmt.Sprintf("(%s IS NOT NULL AND %s <= ?)", col, col), []any{comparisonBind(f, f.Value)}
	case spi.FilterContains:
		return fmt.Sprintf("instr(%s, ?) > 0", col), []any{f.Value}
	case spi.FilterStartsWith:
		return fmt.Sprintf("substr(%s, 1, length(?)) = ?", col), []any{f.Value, f.Value}
	case spi.FilterEndsWith:
		return fmt.Sprintf("substr(%s, -length(?)) = ?", col), []any{f.Value, f.Value}
	case spi.FilterLike:
		return fmt.Sprintf("%s LIKE ? ESCAPE '\\'", col), []any{escapeLike(fmt.Sprint(f.Value))}
	case spi.FilterBetween, spi.FilterBetweenInclusive:
		// SOUND SUPERSET: SQL BETWEEN is inclusive [lo,hi]. For the inclusive
		// kernel between that is exact-on-value; for the exclusive kernel between
		// (FilterBetween) the inclusive SQL is a strict superset (kernel re-check
		// enforces the open bounds).
		if len(f.Values) >= 2 {
			return fmt.Sprintf("(%s IS NOT NULL AND %s BETWEEN ? AND ?)", col, col),
				[]any{comparisonBind(f, f.Values[0]), comparisonBind(f, f.Values[1])}
		}
		// Malformed BETWEEN (not exactly 2 operands) fails closed — exclude
		// every row, matching memory's spi.Prepare/PreparedFilter.Match
		// semantics. Validation upstream (search.validateBetweenArity) rejects
		// this shape before it ever reaches a plugin; this is defense-in-depth
		// only.
		return "0", nil
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
	case spi.FilterBetween, spi.FilterBetweenInclusive:
		if len(f.Values) < 2 {
			// Malformed BETWEEN (not exactly 2 operands) fails closed —
			// exclude every row, matching memory's
			// spi.Prepare/PreparedFilter.Match semantics. Validation upstream
			// (search.validateBetweenArity) rejects this shape before it ever
			// reaches a plugin; this is defense-in-depth only.
			return "0", nil
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
