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
// Bounding: Search is bounded-or-fail. opts.Limit > 0 is a cap on the matched
// set, not a page size — a matched set larger than Limit is
// spi.ErrSearchResultLimitExceeded, never a truncated prefix; exactly-at-limit
// succeeds. opts.Limit <= 0 is unbounded and must never raise; no default is
// substituted for it. When there is no residual, the bound is pushed into SQL
// as "LIMIT limit+1": the extra row, if returned, is the proof that the
// matched set does not fit, which Search reports instead of truncating to
// limit. With a residual, rows are streamed and post-filtered in Go, and
// Search raises the moment the running count exceeds Limit — there is no page
// to gather, so it stops as soon as the matched set is known not to fit.
//
// No scan budget (unlike sqlite): the production engine streams in SQL order
// and bounds memory via the limit+1 probe / early-raise above. An unbounded
// request with a residual is O(n) memory — the same profile as the in-memory
// fallback it replaces.
//
// Transaction awareness (read-your-own-writes). Unlike the memory and sqlite
// backends — which stage a transaction's writes in an in-process buffer
// (spi.TransactionState.Buffer/.Deletes) and must overlay that buffer onto a
// committed snapshot to serve RYW — the postgres backend runs every transaction
// as a real pgx.Tx under REPEATABLE READ. The query below executes through the
// context-resolving Querier (s.q), which resolves the active pgx.Tx from ctx, so
// it already observes the transaction's own uncommitted creates/updates/deletes:
// RYW is provided by the database, and the committed pushdown IS the RYW result.
// No buffer overlay, no spi.MergePage, and no tx.OpMu are involved (postgres
// never populates Buffer/Deletes/DeleteAttribution or any other
// TransactionState bookkeeping field; Get/GetAll don't take tx.OpMu either).
// The one tx-specific behaviour Search adds over the committed pushdown
// is read-set recording — see the TrackingRead block at the end of the function.
func (s *entityStore) Search(ctx context.Context, filter spi.Filter, opts spi.SearchOptions) ([]*spi.Entity, error) {
	if err := validateFilterPaths(filter); err != nil {
		return nil, err
	}
	if err := validateOrderSpecs(opts.OrderBy); err != nil {
		return nil, err
	}

	results, err := s.searchCommitted(ctx, filter, opts)
	if err != nil {
		return nil, err
	}

	// Read-set recording. Only in-transaction, current-state (PointInTime==nil),
	// and only when TrackingRead is requested. Each returned entity's observed
	// version enters the tx read-set so commit-time first-committer-wins
	// validates it (mirroring Get/GetAll, which record via recordReadIfInTx —
	// but they record unconditionally; Search records only when asked).
	//
	// Recording the matched set, which under bounded-or-fail is everything the
	// search returns, matches the sqlite overlay: a search "reads" exactly the
	// rows it returns. recordReadIfInTx → RecordRead no-ops for ids already in
	// the tx write-set, so an in-tx UPDATE's row never enters the read-set (it's
	// in the write-set).
	// A fresh in-tx INSERT is NOT tracked in the write-set (see Save's isNew
	// comment), so a TrackingRead search returning it DOES add it to the
	// read-set — harmless: ValidateReadSet runs inside the same pgx.Tx at
	// commit and sees the own write at the recorded version, so it matches and
	// never false-conflicts. In-tx point-in-time search is committed-only and
	// records nothing (consistent with GetAsAt / GetAllAsAt, which deliberately
	// skip read-set tracking for historical reads).
	if opts.TrackingRead && opts.PointInTime == nil && s.tm != nil {
		for _, e := range results {
			s.tm.recordReadIfInTx(ctx, e.Meta.ID, e.Meta.Version)
		}
	}
	return results, nil
}

// searchCommitted runs the committed pushdown: plan the filter, push the
// pushable portion (and, when there is no residual, LIMIT/OFFSET) to SQL, then
// post-filter the residual and page in Go. Executed through the context-
// resolving Querier, so inside a transaction it observes the tx's own writes
// (read-your-own-writes) natively; outside a transaction it reads committed data.
func (s *entityStore) searchCommitted(ctx context.Context, filter spi.Filter, opts spi.SearchOptions) ([]*spi.Entity, error) {
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

	// No residual → push the bound into SQL. Ask for limit+1: the extra row is
	// the proof that the matched set does not fit, which bounded-or-fail must
	// report instead of truncating to limit.
	if plan.postFilter == nil && opts.Limit > 0 {
		baseQuery += fmt.Sprintf(" LIMIT $%d", len(baseArgs)+1)
		baseArgs = append(baseArgs, opts.Limit+1)
	}

	rows, err := s.q.Query(ctx, baseQuery, baseArgs...)
	if err != nil {
		return nil, fmt.Errorf("search query: %w", err)
	}
	it := &postgresIter{ctx: ctx, rows: rows, postFilter: plan.postFilter}
	defer it.Close()

	var results []*spi.Entity

	// No residual: SQL already applied the limit+1 probe; collect everything.
	if plan.postFilter == nil {
		for it.Next() {
			results = append(results, it.Entity())
		}
		if err := it.Err(); err != nil {
			return nil, err
		}
		if opts.Limit > 0 && len(results) > opts.Limit {
			return nil, fmt.Errorf("search: more than %d matches: %w", opts.Limit, spi.ErrSearchResultLimitExceeded)
		}
		return results, nil
	}

	// Residual present: postgresIter yields only post-filter matches. Stop the
	// moment the matched set is known not to fit — there is no page to gather.
	for it.Next() {
		results = append(results, it.Entity())
		if opts.Limit > 0 && len(results) > opts.Limit {
			return nil, fmt.Errorf("search: more than %d matches: %w", opts.Limit, spi.ErrSearchResultLimitExceeded)
		}
	}
	if err := it.Err(); err != nil {
		return nil, err
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
//   - Empty → default `ORDER BY entity_id COLLATE "C"` (unique, deterministic).
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
//   - OrderTemporal → cyoda_epoch_millis(base) (migration 000005): converts
//     RFC3339 text to epoch-milliseconds (canonical resolution for
//     cross-backend parity), NULL-safe on offset-less/malformed stored text
//     rather than raising — the same IMMUTABLE function used by temporal
//     filter pushdown in query_planner.go, so ORDER BY and WHERE agree on a
//     single epoch-ms SQL form.
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
		// _meta value is RFC3339 text; cyoda_epoch_millis floors the instant to
		// epoch-milliseconds (the canonical cross-backend resolution) so all
		// backends agree, and returns NULL (→ NULLS LAST) rather than raising
		// on offset-less/malformed stored text.
		return "cyoda_epoch_millis(" + base + ")"
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
