package postgres

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

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
// fallback it replaces. Time is bounded server-side by statement_timeout, and
// on the async-search scan by that workload's own ceiling — see searchCommitted.
//
// Transaction awareness (read-your-own-writes). Unlike the memory and sqlite
// backends — which stage a transaction's writes in an in-process buffer
// (spi.TransactionState.Buffer/.Deletes) and must overlay that buffer onto a
// committed snapshot to serve RYW — the postgres backend runs every transaction
// as a real pgx.Tx under REPEATABLE READ. The query below executes through the
// context-resolving Querier (s.q), which resolves the active pgx.Tx from ctx, so
// it already observes the transaction's own uncommitted creates/updates/deletes:
// RYW is provided by the database, and the committed pushdown IS the RYW result.
// No buffer overlay, no spi.MergeBounded, and no tx.OpMu are involved (postgres
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

// searchCommitted routes the committed pushdown to the querier it should run
// through, which is the context-resolving one for every search except the
// async-search scan.
//
// That scan gets a transaction of its own so it can raise its statement ceiling
// (searchUnderOwnCeiling). The two conditions are both required: the context
// must be one the AsyncSearchStore marked, AND there must be no transaction
// already active — opening a second transaction under a caller who is already in
// one would run the scan outside the transaction whose writes it is supposed to
// see, losing read-your-own-writes and holding a second pooled connection. An
// async job never runs in a transaction, so this is a guard, not a branch the
// production path takes.
func (s *entityStore) searchCommitted(ctx context.Context, filter spi.Filter, opts spi.SearchOptions) ([]*spi.Entity, error) {
	if ceiling, ok := searchScanCeiling(ctx); ok && s.pool != nil && spi.GetTransaction(ctx) == nil {
		return s.searchUnderOwnCeiling(ctx, ceiling, filter, opts)
	}
	return s.runSearch(ctx, s.q, filter, opts)
}

// searchUnderOwnCeiling runs the scan in a transaction whose first statement
// replaces the interactive statement ceiling with the async-search one.
//
// SET LOCAL, never SET: the ceiling has to die with the transaction. A session
// SET would ride the pooled connection back into the pool and cap — or uncap —
// every interactive statement that borrowed it next.
//
// The transaction is read-only in effect and always rolled back: there is
// nothing to commit, and a rollback returns the connection just as cleanly.
func (s *entityStore) searchUnderOwnCeiling(ctx context.Context, ceiling time.Duration, filter spi.Filter, opts spi.SearchOptions) ([]*spi.Entity, error) {
	// Acquire-only deadline, cancelled the moment Begin has returned — see
	// newAcquireContext. A deadline that reached the transaction handle would
	// cancel the scan when the acquire window closed, which is the opposite of
	// giving it room to run.
	acquireCtx, cancelAcquire := newAcquireContext(ctx, s.acquireTimeout)
	tx, err := s.pool.Begin(acquireCtx)
	cancelAcquire()
	if err != nil {
		// classifyAcquireErr, not classifyError: an acquire that timed out never
		// reached the server, so there is no SQLSTATE and no torn socket for
		// classifyError to recognise — it would fall through unmarked and the job
		// record would say only "context deadline exceeded". This is the same
		// saturated pool the write doors report as storage-unavailable.
		return nil, classifyAcquireErr(ctx, acquireCtx, "begin async search scan", err)
	}
	// Rollback on a context derived WithoutCancel: on the cancellation path the
	// caller's context may itself be the thing that expired, and a rollback on an
	// expired context destroys the pooled connection instead of returning it.
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	// The tenant RLS policies read, matching what TransactionManager.Begin does
	// for every other transaction this plugin opens. set_config rather than SET
	// LOCAL because PostgreSQL's SET takes no bound parameters.
	if _, err := tx.Exec(ctx, "SELECT set_config('app.current_tenant', $1, true)", string(s.tenantID)); err != nil {
		return nil, fmt.Errorf("set tenant for async search scan: %w", classifyError(err))
	}

	// pgDurationMillis, never a Go duration string: PostgreSQL's time units are
	// us/ms/s/min/h/d — "m" is not among them — and Go renders 30 minutes as
	// "30m0s", which is invalid twice over.
	if _, err := tx.Exec(ctx, "SET LOCAL statement_timeout = "+pgDurationMillis(ceiling)); err != nil {
		return nil, fmt.Errorf("set search statement ceiling: %w", classifyError(err))
	}

	results, err := s.runSearch(ctx, tx, filter, opts)
	if err != nil {
		return nil, s.classifyScanError(err)
	}
	return results, nil
}

// classifyScanError names the async-search ceiling when it is what fired, and
// otherwise classifies the error the way every other statement in this plugin is
// classified.
//
// The ceiling is checked first and does NOT fall through to classifySQLState:
// that branch logs statement_timeout by name, which would be the wrong setting
// here and would send an operator to the wrong knob.
func (s *entityStore) classifyScanError(err error) error {
	if isStatementTimeout(err) {
		// NOT retryable, and deliberately not marked so: a scan re-run after
		// exceeding its ceiling exceeds it again. Naming the setting is the whole
		// operational benefit — the caller's own job record says only that a
		// ceiling was hit.
		slog.Warn("async search scan cancelled after exceeding the configured ceiling",
			"pkg", "postgres", "setting", "CYODA_POSTGRES_SEARCH_STATEMENT_TIMEOUT", "err", err)
		return &searchCeilingError{cause: err}
	}
	return classifyError(err)
}

// runSearch runs the committed pushdown through q: plan the filter, push the
// pushable portion to SQL, and — when there is no residual — push the
// limit+1 probe described on Search above. When there is a residual, rows
// are streamed and post-filtered in Go with no paging: Search raises the
// moment the running count exceeds the bound. With the context-resolving
// Querier, inside a transaction it observes the tx's own writes
// (read-your-own-writes) natively; outside a transaction it reads committed data.
func (s *entityStore) runSearch(ctx context.Context, q Querier, filter spi.Filter, opts spi.SearchOptions) ([]*spi.Entity, error) {
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

	rows, err := q.Query(ctx, baseQuery, baseArgs...)
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
