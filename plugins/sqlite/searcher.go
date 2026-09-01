package sqlite

import (
	"context"
	"fmt"
	"sort"
	"strings"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// Compile-time check that entityStore implements spi.Searcher.
var _ spi.Searcher = (*entityStore)(nil)

// Search implements spi.Searcher for the SQLite entity store.
//
// Search is bounded-or-fail: opts.Limit >= 1 is REQUIRED, a cap on the matched
// set, not a page size. A matched set larger than Limit is
// spi.ErrSearchResultLimitExceeded, never a truncated prefix; exactly-at-limit
// succeeds. opts.Limit <= 0 is a contract violation — Search returns an error
// rather than treating it as "unbounded" or substituting a default of its own
// (see spi.Searcher's doc comment; the engine resolves the direct-search
// default before calling, so Search itself never needs to guess a bound).
//
// Three branches, all producing the same result set that
// GetAll + spi.Prepare(filter).Match would for the same transaction state:
//   - non-tx (or in-tx point-in-time): committed pushdown via searchCommitted —
//     the query planner pushes pushable predicates to SQL and post-filters the
//     residual in Go; the bound is enforced in SQL (LIMIT limit+1, so the extra
//     row is the proof of overflow) when no residual exists, or by counting
//     matches as they stream in Go when a residual post-filter is active. In-tx
//     PIT is committed-only (no buffer overlay, no read-set) — the overlay for
//     that dimension is a later task.
//   - in-tx, PointInTime==nil: read-your-own-writes overlay via searchTxOverlay —
//     a bounded streaming merge (spi.MergeBounded) of the committed snapshot (at
//     tx.SnapshotTime, suppressing tx.Deletes and buffered ids) with the matching
//     buffer entries. Returned committed ids enter tx.ReadSet only when
//     opts.TrackingRead is set — under bounded-or-fail that is exactly the
//     matched set, since there is no page smaller than it.
func (s *entityStore) Search(ctx context.Context, filter spi.Filter, opts spi.SearchOptions) ([]*spi.Entity, error) {
	if opts.Limit <= 0 {
		return nil, fmt.Errorf("search: limit must be >= 1, got %d", opts.Limit)
	}
	if err := validateFilterPaths(filter); err != nil {
		return nil, err
	}
	if err := validateOrderSpecs(opts.OrderBy); err != nil {
		return nil, err
	}

	tx := spi.GetTransaction(ctx)
	if tx != nil && opts.PointInTime == nil {
		return s.searchTxOverlay(ctx, tx, filter, opts)
	}
	// Non-tx (committed pushdown) or in-tx point-in-time (committed-only, unchanged).
	return s.searchCommitted(ctx, filter, opts)
}

// searchCommitted runs the committed pushdown: plan the filter, push the
// pushable portion to SQL, post-filter the residual in Go, and enforce the
// bounded-or-fail cap. Used by the non-tx path and the in-tx point-in-time
// path (committed-only).
//
// The result bound (opts.Limit) is the only bound over the streamed rows. The
// residual scan itself is unmetered: time-unbounded work is the caller's to
// bound, via the direct-search timeout or async job cancellation, never the
// backend's.
func (s *entityStore) searchCommitted(ctx context.Context, filter spi.Filter, opts spi.SearchOptions) ([]*spi.Entity, error) {
	plan, err := planFor(filter)
	if err != nil {
		return nil, fmt.Errorf("Search: %w", err)
	}

	var baseQuery string
	var baseArgs []any

	if opts.PointInTime != nil {
		baseQuery, baseArgs = s.searchPointInTimeBase(opts)
	} else {
		baseQuery, baseArgs = s.searchCurrentStateBase(opts)
	}

	if plan.where != "" {
		baseQuery += " AND (" + plan.where + ")"
		baseArgs = append(baseArgs, plan.args...)
	}

	if opts.PointInTime != nil {
		baseQuery += orderByClause(opts.OrderBy, "ev")
	} else {
		baseQuery += orderByClause(opts.OrderBy, "")
	}

	// When there is no residual, push the bound into SQL. Ask for limit+1: the
	// extra row is the proof that the matched set does not fit, which is what
	// bounded-or-fail must report instead of truncating to limit. opts.Limit is
	// guaranteed >= 1 here — Search rejects Limit <= 0 before this is reached.
	if plan.postFilter == nil {
		baseQuery += " LIMIT ?"
		baseArgs = append(baseArgs, opts.Limit+1)
	}

	rows, err := s.db.QueryContext(ctx, baseQuery, baseArgs...)
	if err != nil {
		return nil, fmt.Errorf("search query: %w", err)
	}
	defer rows.Close()

	var results []*spi.Entity
	scanned := 0

	for rows.Next() {
		// Amortized cancellation check (spec D5): checked every 1024 rows
		// (scanned&1023==0, true at scanned==0 too) so an already-expired or
		// since-expired ctx aborts the scan deterministically instead of
		// depending on database/sql's/the driver's own cancellation timing.
		if scanned&1023 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, fmt.Errorf("Search: %w", err)
			}
		}
		scanned++

		var e *spi.Entity
		var scanErr error
		if opts.PointInTime != nil {
			e, scanErr = scanVersionEntity(rows)
		} else {
			e, scanErr = scanEntityFromRow(rows)
		}
		if scanErr != nil {
			return nil, scanErr
		}

		if plan.preparedPostFilter != nil && !evaluateFilter(*plan.preparedPostFilter, e) {
			continue
		}

		results = append(results, e)
		if len(results) > opts.Limit {
			return nil, fmt.Errorf("search: more than %d matches: %w", opts.Limit, spi.ErrSearchResultLimitExceeded)
		}
	}

	if err := rows.Err(); err != nil {
		// Prefer ctx.Err() over the raw driver error when the row stream
		// ended because the deadline fired: the sqlite driver's own
		// interrupt mechanism can surface a driver-specific error (e.g.
		// "sqlite3: interrupted") that does not chain to
		// context.DeadlineExceeded/Canceled on its own. Checking ctx here
		// guarantees the caller always gets a deterministic, chainable
		// error when cancellation is the actual cause.
		if cErr := ctx.Err(); cErr != nil {
			return nil, fmt.Errorf("Search: %w", cErr)
		}
		return nil, fmt.Errorf("row iteration: %w", err)
	}

	return results, nil
}

// searchCurrentStateBase returns the base SQL for current-state search.
func (s *entityStore) searchCurrentStateBase(opts spi.SearchOptions) (string, []any) {
	query := `SELECT entity_id, model_name, model_version, version,
	                 json(data), json(meta), created_at, updated_at
	          FROM entities
	          WHERE tenant_id = ? AND model_name = ? AND model_version = ? AND NOT deleted`
	args := []any{string(s.tenantID), opts.ModelName, opts.ModelVersion}
	return query, args
}

// searchPointInTimeBase returns the base SQL for point-in-time search.
func (s *entityStore) searchPointInTimeBase(opts spi.SearchOptions) (string, []any) {
	return s.searchSnapshotBase(opts, timeToMicro(*opts.PointInTime))
}

// searchSnapshotBase returns the base SQL selecting the latest non-deleted
// version of each entity for the model as of snapshotMicro. Shared by the
// point-in-time path (snapshotMicro = opts.PointInTime) and the in-tx overlay
// (snapshotMicro = tx.SnapshotTime) so both agree with getSnapshot/getAllTx.
//
// Uses submit_time <= ? (non-strict) matching the memory plugin's convention
// (!v.submitTime.After(snapshotTime)) and all other snapshot queries in this
// package (getSnapshot, getAllTx, DeleteAll tx). Rows scan via scanVersionEntity.
func (s *entityStore) searchSnapshotBase(opts spi.SearchOptions, snapshotMicro int64) (string, []any) {
	query := `SELECT ev.entity_id, ev.model_name, ev.model_version, ev.version,
	                 json(ev.data), json(ev.meta), ev.submit_time
	          FROM entity_versions ev
	          INNER JOIN (
	              SELECT entity_id, MAX(version) AS max_ver
	              FROM entity_versions
	              WHERE tenant_id = ? AND model_name = ? AND model_version = ? AND submit_time <= ?
	              GROUP BY entity_id
	          ) latest ON ev.entity_id = latest.entity_id AND ev.version = latest.max_ver
	          WHERE ev.tenant_id = ? AND ev.change_type != 'DELETED'`
	args := []any{string(s.tenantID), opts.ModelName, opts.ModelVersion, snapshotMicro, string(s.tenantID)}
	return query, args
}

// sortEntitiesByOrder sorts entities in place by the canonical spi.LessByOrder
// comparator (a strict total order with an entity_id ascending tiebreaker), so
// the buffer `adds` slice is ordered identically to the SQL ORDER BY stream
// before the merge.
//
// A single ctx check gates the O(n log n) sort itself (spec D5's pre-sort
// check): the buffer-match loop that built rows already pays for its own
// amortized checks over the scan, but the sort is a separate unit of Go-only
// work worth gating on its own before it runs.
func sortEntitiesByOrder(ctx context.Context, rows []*spi.Entity, order []spi.OrderSpec) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("Search: %w", err)
	}
	sort.Slice(rows, func(i, j int) bool {
		return spi.LessByOrder(rows[i], rows[j], order)
	})
	return nil
}

// searchTxOverlay implements the in-transaction read-your-own-writes overlay for
// PointInTime==nil: a bounded streaming merge (spi.MergeBounded) of the committed
// snapshot at tx.SnapshotTime with the tx's matching buffered writes.
//
// Committed candidates are streamed in ORDER BY order WITHOUT SQL LIMIT (the
// bound is enforced by MergeBounded over the merged committed+buffered
// sequence, not by SQL alone, since a buffered own-write can itself be what
// pushes the total over the cap). The residual post-filter still applies to the
// committed stream, so a filter whose pushable part narrows the candidate set
// does not full-scan. The scan itself is unmetered and opts.Limit is the only
// bound, exactly as in searchCommitted.
//
// The whole operation runs under tx.OpMu.RLock (fail fast on tx.RolledBack) so
// Commit/Rollback (which take tx.OpMu.Lock) cannot race our reads of
// tx.Buffer/tx.Deletes or our write to tx.ReadSet. Lock order: tx.OpMu before
// the sql.DB query — identical to Save/GetAll/getAllTx in this package.
func (s *entityStore) searchTxOverlay(ctx context.Context, tx *spi.TransactionState, filter spi.Filter, opts spi.SearchOptions) ([]*spi.Entity, error) {
	modelRef := spi.ModelRef{EntityName: opts.ModelName, ModelVersion: opts.ModelVersion}
	plan, err := planFor(filter)
	if err != nil {
		return nil, fmt.Errorf("Search: %w", err)
	}
	// The buffered own-writes are matched against the FULL original filter (not
	// the residual), so they need their own prepared value. Prepared once,
	// above the loop.
	pf, err := spi.Prepare(filter)
	if err != nil {
		return nil, fmt.Errorf("Search: %w", err)
	}

	// Committed candidate SQL: snapshot at tx.SnapshotTime, ORDER BY, no LIMIT.
	baseQuery, baseArgs := s.searchSnapshotBase(opts, timeToMicro(tx.SnapshotTime))
	if plan.where != "" {
		baseQuery += " AND (" + plan.where + ")"
		baseArgs = append(baseArgs, plan.args...)
	}
	baseQuery += orderByClause(opts.OrderBy, "ev")

	var results []*spi.Entity
	err = func() error {
		tx.OpMu.RLock()
		defer tx.OpMu.RUnlock()
		if tx.RolledBack {
			return fmt.Errorf("Search: %w (txID=%s)", spi.ErrTxRolledBack, tx.ID)
		}

		rows, err := s.db.QueryContext(ctx, baseQuery, baseArgs...)
		if err != nil {
			return fmt.Errorf("search query: %w", err)
		}
		defer rows.Close()

		// Lazy committed source: scan one row per call, apply the residual
		// post-filter. Never drains into a slice.
		scanned := 0
		next := func() (*spi.Entity, bool, error) {
			for rows.Next() {
				// Amortized cancellation check (spec D5): same shape as
				// searchCommitted's row loop — checked every 1024 rows so an
				// expired ctx aborts the streamed merge deterministically.
				if scanned&1023 == 0 {
					if err := ctx.Err(); err != nil {
						return nil, false, fmt.Errorf("Search: %w", err)
					}
				}
				scanned++
				e, scanErr := scanVersionEntity(rows)
				if scanErr != nil {
					return nil, false, scanErr
				}
				if plan.preparedPostFilter != nil && !evaluateFilter(*plan.preparedPostFilter, e) {
					continue
				}
				return e, true, nil
			}
			if err := rows.Err(); err != nil {
				// Prefer ctx.Err() over the raw driver error — see the
				// matching comment in searchCommitted.
				if cErr := ctx.Err(); cErr != nil {
					return nil, false, fmt.Errorf("Search: %w", cErr)
				}
				return nil, false, fmt.Errorf("row iteration: %w", err)
			}
			return nil, false, nil
		}

		// adds = matching buffered own-writes for this model, excluding staged
		// deletes. copyEntity so no store-internal pointer escapes the lock.
		adds := make([]*spi.Entity, 0, len(tx.Buffer))
		addI := 0
		for id, e := range tx.Buffer {
			// Amortized cancellation check (spec D5): this loop is pure Go
			// (no SQL), so it has no other cancellation signal — checked
			// every 1024 entries so a large buffer under an expiring ctx
			// aborts promptly instead of scanning to completion regardless
			// of the deadline.
			if addI&1023 == 0 {
				if err := ctx.Err(); err != nil {
					return fmt.Errorf("Search: %w", err)
				}
			}
			addI++
			if tx.Deletes[id] {
				continue
			}
			if e.Meta.ModelRef != modelRef {
				continue
			}
			if pf.Match(e.Data, e.Meta) {
				adds = append(adds, copyEntity(e))
			}
		}
		if err := sortEntitiesByOrder(ctx, adds, opts.OrderBy); err != nil {
			return err
		}

		// A committed row is suppressed if staged for delete OR shadowed by a
		// buffered own-write (the buffered version, if matching, arrives via adds).
		deleted := func(id string) bool {
			if tx.Deletes[id] {
				return true
			}
			_, buffered := tx.Buffer[id]
			return buffered
		}

		page, mErr := spi.MergeBounded(next, adds, deleted, opts.OrderBy, opts.Limit)
		if mErr != nil {
			return mErr
		}

		// Read-set recording is CONDITIONAL on TrackingRead (unlike GetAll, which
		// records unconditionally). Only returned committed rows (not buffered —
		// those are own-writes already in the write-set) enter the read-set.
		// Under bounded-or-fail, page IS the whole matched committed+buffered
		// set (MergeBounded fails rather than truncating it), so this records
		// every matching committed row, not an arbitrary window of them — a
		// transaction that reads a predicate now has the entire matched set
		// validated at commit under first-committer-wins.
		if opts.TrackingRead {
			for _, e := range page {
				if _, buffered := tx.Buffer[e.Meta.ID]; !buffered {
					tx.ReadSet[e.Meta.ID] = true
				}
			}
		}
		results = page
		return nil
	}()
	if err != nil {
		return nil, err
	}
	return results, nil
}

// metaBlobKey maps canonical meta sort-path names (as used in spi.OrderSpec)
// to the corresponding key in the meta JSON blob stored on-disk. The "id"
// case is special: it resolves to the entity_id column, not the blob.
var metaBlobKey = map[string]string{
	"state":                   "state",
	"creationDate":            "creation_date",
	"lastUpdateTime":          "last_modified_date",
	"transitionForLatestSave": "transition_for_latest_save",
	"transactionId":           "transaction_id",
}

// jsonExtract wraps col in json() before extracting key, handling text-affinity
// blobs that may not be stored in JSON canonical form.
func jsonExtract(col, key string) string {
	return fmt.Sprintf("json_extract(json(%s), '$.%s')", col, key)
}

// orderByClause builds a SQL ORDER BY clause from order. Shared by Search
// (SearchOptions.OrderBy) and Iterate (IterateOptions.OrderBy) — both fields
// are the same []spi.OrderSpec type.
//
//   - When order is empty, defaults to "ORDER BY entity_id". For Search this
//     is the documented canonical default; for Iterate an empty OrderBy means
//     "unspecified" per the Iterable doc, and a deterministic order is a
//     conformant (if stronger-than-required) choice within "unspecified".
//   - Each clause gets NULLS LAST so absent/null values sort after real values
//     regardless of ASC/DESC.
//   - A entity_id tiebreaker is appended unless the last OrderSpec already
//     resolves to entity_id (Source=SourceMeta, Path="id"), avoiding duplicates.
//   - tablePrefix is prepended to column references (e.g., "ev" for PIT queries).
func orderByClause(order []spi.OrderSpec, tablePrefix string) string {
	idCol := "entity_id"
	if tablePrefix != "" {
		idCol = tablePrefix + ".entity_id"
	}
	if len(order) == 0 {
		return " ORDER BY " + idCol
	}
	clauses := make([]string, 0, len(order)+1)
	for _, spec := range order {
		expr := orderByFieldExpr(spec, tablePrefix)
		if spec.Desc {
			expr += " DESC"
		}
		clauses = append(clauses, expr+" NULLS LAST")
	}
	// Append entity_id tiebreaker unless the last spec already IS entity_id.
	if last := order[len(order)-1]; !(last.Source == spi.SourceMeta && last.Path == "id") {
		clauses = append(clauses, idCol)
	}
	return " ORDER BY " + strings.Join(clauses, ", ")
}

// orderByFieldExpr returns the SQL expression for an OrderSpec field.
//
// SourceMeta "id" → entity_id column (direct, no json_extract).
// SourceMeta other → json_extract on meta blob with canonical key from metaBlobKey.
// SourceData → json_extract on data blob.
// Kind wraps the expression: Numeric → CAST AS REAL, Temporal → /1000 (µs→ms
// floor), Bool → raw (json_extract returns 0/1), Text → COLLATE BINARY.
//
// Safety invariant: spec.Path is interpolated into a JSON-path literal and
// MUST have been validated by validateOrderSpecs at the Search() boundary
// (see path_validation.go). Adding a new caller that bypasses Search() re-
// introduces SQL injection.
func orderByFieldExpr(spec spi.OrderSpec, tablePrefix string) string {
	qualify := func(col string) string {
		if tablePrefix != "" {
			return tablePrefix + "." + col
		}
		return col
	}
	var base string
	switch {
	case spec.Source == spi.SourceMeta && spec.Path == "id":
		base = qualify("entity_id")
	case spec.Source == spi.SourceMeta:
		key, ok := metaBlobKey[spec.Path]
		if !ok {
			// Unreachable: validateOrderSpecs rejects any meta path outside the
			// canonical set before Search() builds SQL. Panic surfaces a bypass
			// (e.g. a future refactor) instead of silently interpolating input.
			panic(fmt.Sprintf("orderByFieldExpr: unmapped meta sort path %q", spec.Path))
		}
		base = jsonExtract(qualify("meta"), key)
	default:
		base = fmt.Sprintf("json_extract(%s, '$.%s')", qualify("data"), spec.Path)
	}
	switch spec.Kind {
	case spi.OrderNumeric:
		return "CAST(" + base + " AS REAL)"
	case spi.OrderTemporal:
		// Meta blob stores timestamps as microseconds; floor to ms via integer
		// division for cross-backend parity (Cassandra HLC precision floor).
		return "(" + base + ") / 1000"
	case spi.OrderBool:
		return base // json_extract yields 0/1 natively
	default: // OrderText (zero value)
		return base + " COLLATE BINARY"
	}
}
