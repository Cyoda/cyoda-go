package postgres

import (
	"context"
	"fmt"
	"strings"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// Compile-time check that *entityStore implements spi.Searcher.
var _ spi.Searcher = (*entityStore)(nil)

// Search implements spi.Searcher for the PostgreSQL entity store. Pushable
// predicates go into the SQL WHERE via planQuery; the residual (regex /
// case-insensitive ops) is evaluated in Go by postgresIter/evalPostFilter.
//
// Pagination: when there is no residual, LIMIT/OFFSET are pushed into SQL.
// With a residual, rows are streamed, post-filtered, and offset/limit applied
// in Go — early-stopping once offset+limit matches are gathered, but ONLY when
// Limit > 0 (an unbounded Limit<=0 request must drain all matches before
// applying the offset, else a naive offset+limit==offset stop returns empty).
//
// No scan budget (unlike sqlite): the production engine streams in SQL order
// and bounds memory via the early-stop when a limit is set. An unbounded
// request with a residual is O(n) memory — the same profile as the in-memory
// fallback it replaces.
func (s *entityStore) Search(ctx context.Context, filter spi.Filter, opts spi.SearchOptions) ([]*spi.Entity, error) {
	if err := validateFilterPaths(filter); err != nil {
		return nil, err
	}
	if err := validateOrderSpecs(opts.OrderBy); err != nil {
		return nil, err
	}

	// Zero-value Filter means "match all" — skip planQuery (it would treat the
	// empty Op as non-pushable and install the zero filter as a residual).
	var plan sqlPlan
	if filter.Op != "" {
		plan = planQuery(filter)
	}

	baseQuery, baseArgs := s.searchBaseQuery(opts.ModelName, opts.ModelVersion, opts.PointInTime)

	if plan.where != "" {
		shifted := shiftPlaceholders(plan.where, len(baseArgs))
		baseQuery += " AND (" + shifted + ")"
		baseArgs = append(baseArgs, plan.args...)
	}

	baseQuery += orderByClause(opts)

	// No residual → LIMIT/OFFSET in SQL.
	if plan.postFilter == nil {
		if opts.Limit > 0 {
			baseQuery += fmt.Sprintf(" LIMIT $%d", len(baseArgs)+1)
			baseArgs = append(baseArgs, opts.Limit)
		}
		if opts.Offset > 0 {
			baseQuery += fmt.Sprintf(" OFFSET $%d", len(baseArgs)+1)
			baseArgs = append(baseArgs, opts.Offset)
		}
	}

	rows, err := s.q.Query(ctx, baseQuery, baseArgs...)
	if err != nil {
		return nil, fmt.Errorf("search query: %w", err)
	}
	it := &postgresIter{ctx: ctx, rows: rows, postFilter: plan.postFilter}
	defer it.Close()

	var results []*spi.Entity

	// No residual: SQL already applied LIMIT/OFFSET; collect everything.
	if plan.postFilter == nil {
		for it.Next() {
			results = append(results, it.Entity())
		}
		if err := it.Err(); err != nil {
			return nil, err
		}
		return results, nil
	}

	// Residual present: postgresIter yields only post-filter matches. Apply
	// offset/limit in Go. Early-stop only when a limit is set (S1 guard).
	for it.Next() {
		results = append(results, it.Entity())
		if opts.Limit > 0 && len(results) >= opts.Offset+opts.Limit {
			break
		}
	}
	if err := it.Err(); err != nil {
		return nil, err
	}
	if opts.Offset > 0 {
		if opts.Offset >= len(results) {
			return nil, nil
		}
		results = results[opts.Offset:]
	}
	if opts.Limit > 0 && len(results) > opts.Limit {
		results = results[:opts.Limit]
	}
	return results, nil
}

// metaJSONKey maps canonical meta sort-path names (as used in spi.OrderSpec)
// to the corresponding key in the _meta JSONB block stored on disk. The "id"
// path is special-cased to the entity_id column — it is not in this map.
// Note: postgres stores the transition as "transition" (not
// "transition_for_latest_save" as sqlite does — postgres diverges here).
var metaJSONKey = map[string]string{
	"state":                   "state",
	"creationDate":            "creation_date",
	"lastUpdateTime":          "last_modified_date",
	"transitionForLatestSave": "transition",
	"transactionId":           "transaction_id",
}

// orderByClause builds the SQL ORDER BY from opts.OrderBy.
//
//   - Empty → default "ORDER BY entity_id COLLATE "C"" (unique, deterministic).
//   - Every key gets NULLS LAST so absent/null values sort after real values
//     regardless of ASC/DESC.
//   - An entity_id tiebreaker is appended unless the terminal key already
//     resolves to entity_id (Path="id", Source=SourceMeta), avoiding duplicates.
//
// Both the default ORDER BY and the appended tiebreaker use COLLATE "C" for
// byte-order semantics, consistent with the explicit @id sort key and the
// sqlite/memory paths. This guards against a nondeterministic ICU database
// collation if the cluster is provisioned with a non-C default collation.
//
// entity_id and other bare column names resolve against the entities table
// (current-state) or the `latest` derived table (point-in-time), both of
// which expose entity_id in their outer SELECT.
func orderByClause(opts spi.SearchOptions) string {
	if len(opts.OrderBy) == 0 {
		// COLLATE "C": byte-order semantics, consistent with @id sort key and
		// sqlite/memory paths; guards against nondeterministic ICU DB collation.
		return ` ORDER BY entity_id COLLATE "C"`
	}
	clauses := make([]string, 0, len(opts.OrderBy)+1)
	for _, spec := range opts.OrderBy {
		expr := orderByFieldExpr(spec)
		if spec.Desc {
			expr += " DESC"
		}
		clauses = append(clauses, expr+" NULLS LAST")
	}
	// Append entity_id tiebreaker unless the last spec already IS entity_id.
	// COLLATE "C": byte-order semantics consistent with @id sort key and
	// sqlite/memory paths; guards against nondeterministic ICU DB collation.
	if last := opts.OrderBy[len(opts.OrderBy)-1]; !(last.Source == spi.SourceMeta && last.Path == "id") {
		clauses = append(clauses, `entity_id COLLATE "C"`)
	}
	return " ORDER BY " + strings.Join(clauses, ", ")
}

// orderByFieldExpr returns the SQL ordering expression for an OrderSpec.
//
// SourceMeta "id" → entity_id column (direct, no JSONB extraction).
// SourceMeta other → JSONB extraction on doc->'_meta' with the canonical key
// from metaJSONKey (guaranteed valid by validateOrderSpecs).
// SourceData → JSONB extraction on doc.
//
// Kind wraps the base expression:
//   - OrderNumeric  → cyoda_try_float8(base): NULL-safe coercion (not a raw
//     ::double precision cast which would error on non-numeric text); the
//     helper is already used elsewhere in this plugin.
//   - OrderTemporal → floor(extract(epoch from (base)::timestamptz)*1000):
//     converts RFC3339 text to epoch-milliseconds (canonical resolution for
//     cross-backend parity).
//   - OrderBool     → (base)::boolean
//   - OrderText     → (base) COLLATE "C" (byte-order comparison)
//
// Safety invariant: spec.Path is interpolated into a JSON-key literal and
// MUST have been validated by validateOrderSpecs at the Search() boundary.
func orderByFieldExpr(spec spi.OrderSpec) string {
	var base string
	switch {
	case spec.Source == spi.SourceMeta && spec.Path == "id":
		base = "entity_id"
	case spec.Source == spi.SourceMeta:
		key, ok := metaJSONKey[spec.Path]
		if !ok {
			// Unreachable: validateOrderSpecs rejects any meta path outside the
			// canonical set before Search() builds SQL. Panic surfaces a bypass
			// (e.g. a future refactor) instead of silently interpolating input.
			panic(fmt.Sprintf("orderByFieldExpr: unmapped meta sort path %q", spec.Path))
		}
		base = jsonbExtractText("doc->'_meta'", key)
	default:
		base = jsonbExtractText("doc", spec.Path)
	}
	switch spec.Kind {
	case spi.OrderNumeric:
		// cyoda_try_float8 returns NULL on non-numeric text (→ NULLS LAST),
		// matching sqlite's lenient CAST; a raw ::double precision cast would
		// error the whole query on non-numeric stored values.
		return "cyoda_try_float8(" + base + ")"
	case spi.OrderTemporal:
		// _meta value is RFC3339 text; floor the instant to epoch-milliseconds
		// (the canonical cross-backend resolution) so all backends agree.
		return "floor(extract(epoch from (" + base + ")::timestamptz)*1000)"
	case spi.OrderBool:
		return "(" + base + ")::boolean"
	default: // OrderText (zero value)
		// For non-id meta text fields, postgres stores state/transition/transaction_id
		// without omitempty, so an empty value lands as "" (a present, non-null empty
		// string) rather than an absent key. Under COLLATE "C", "" sorts FIRST
		// ascending, diverging from sqlite (absent key → NULL → NULLS LAST) and the
		// in-memory comparator (metaLeaf treats empty string as MISSING → LAST).
		// NULLIF converts "" to NULL so NULLS LAST takes effect, restoring parity.
		// Data paths and the entity_id column never store empty-means-missing values
		// this way, so NULLIF is not applied to them.
		if spec.Source == spi.SourceMeta && spec.Path != "id" {
			return `NULLIF(` + base + `, '') COLLATE "C"`
		}
		return "(" + base + `) COLLATE "C"`
	}
}
