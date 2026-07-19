package sqlite

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// Compile-time check that entityStore implements spi.Searcher.
var _ spi.Searcher = (*entityStore)(nil)

// ErrScanBudgetExhausted is returned when a search with a residual filter
// examines more rows than the configured SearchScanLimit without filling
// the requested result page. Callers should tighten their filter.
var ErrScanBudgetExhausted = errors.New("scan budget exhausted")

// Search implements spi.Searcher for the SQLite entity store.
//
// Three branches, all producing the same result set that GetAll + spi.MatchFilter
// would for the same transaction state:
//   - non-tx (or in-tx point-in-time): committed pushdown via searchCommitted —
//     the query planner pushes pushable predicates to SQL and post-filters the
//     residual in Go; pagination is applied in SQL when no residual exists, or in
//     Go after post-filtering. In-tx PIT is committed-only (no buffer overlay, no
//     read-set) — the overlay for that dimension is a later task.
//   - in-tx, PointInTime==nil: read-your-own-writes overlay via searchTxOverlay —
//     a bounded streaming merge of the committed snapshot (at tx.SnapshotTime,
//     suppressing tx.Deletes and buffered ids) with the matching buffer entries.
//     Returned committed ids enter tx.ReadSet only when opts.TrackingRead is set.
func (s *entityStore) Search(ctx context.Context, filter spi.Filter, opts spi.SearchOptions) ([]*spi.Entity, error) {
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
// pushable portion to SQL, post-filter the residual in Go, and page. Used by
// the non-tx path and the in-tx point-in-time path (committed-only).
func (s *entityStore) searchCommitted(ctx context.Context, filter spi.Filter, opts spi.SearchOptions) ([]*spi.Entity, error) {
	plan := planQuery(filter)

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
		baseQuery += orderByClause(opts, "ev")
	} else {
		baseQuery += orderByClause(opts, "")
	}

	// When there is no residual, apply LIMIT/OFFSET in SQL.
	if plan.postFilter == nil {
		if opts.Limit > 0 {
			baseQuery += " LIMIT ?"
			baseArgs = append(baseArgs, opts.Limit)
		}
		if opts.Offset > 0 {
			baseQuery += " OFFSET ?"
			baseArgs = append(baseArgs, opts.Offset)
		}
	}

	rows, err := s.db.QueryContext(ctx, baseQuery, baseArgs...)
	if err != nil {
		return nil, fmt.Errorf("search query: %w", err)
	}
	defer rows.Close()

	var results []*spi.Entity
	scanned := 0

	for rows.Next() {
		if plan.postFilter != nil && scanned >= s.cfg.SearchScanLimit {
			return nil, fmt.Errorf("%w: examined %d rows", ErrScanBudgetExhausted, s.cfg.SearchScanLimit)
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

		if plan.postFilter != nil {
			matches, evalErr := evaluateFilter(*plan.postFilter, e)
			if evalErr != nil {
				return nil, fmt.Errorf("post-filter evaluation: %w", evalErr)
			}
			if !matches {
				continue
			}
		}

		results = append(results, e)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("row iteration: %w", err)
	}

	// Apply offset and limit in Go when post-filtering was active.
	if plan.postFilter != nil {
		if opts.Offset > 0 {
			if opts.Offset >= len(results) {
				return nil, nil
			}
			results = results[opts.Offset:]
		}
		if opts.Limit > 0 && len(results) > opts.Limit {
			results = results[:opts.Limit]
		}
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
func sortEntitiesByOrder(rows []*spi.Entity, order []spi.OrderSpec) {
	sort.Slice(rows, func(i, j int) bool {
		return spi.LessByOrder(rows[i], rows[j], order)
	})
}

// searchTxOverlay implements the in-transaction read-your-own-writes overlay for
// PointInTime==nil: a bounded streaming merge (spi.MergePage) of the committed
// snapshot at tx.SnapshotTime with the tx's matching buffered writes.
//
// Committed candidates are streamed in ORDER BY order WITHOUT SQL LIMIT/OFFSET
// (paging is applied by MergePage over the merged sequence). The residual
// post-filter and SearchScanLimit still apply to the committed stream, so a
// filter whose pushable part narrows the candidate set does not full-scan; a
// broad residual can still exhaust the budget as in the non-tx path.
//
// The whole operation runs under tx.OpMu.RLock (fail fast on tx.RolledBack) so
// Commit/Rollback (which take tx.OpMu.Lock) cannot race our reads of
// tx.Buffer/tx.Deletes or our write to tx.ReadSet. Lock order: tx.OpMu before
// the sql.DB query — identical to Save/GetAll/getAllTx in this package.
func (s *entityStore) searchTxOverlay(ctx context.Context, tx *spi.TransactionState, filter spi.Filter, opts spi.SearchOptions) ([]*spi.Entity, error) {
	modelRef := spi.ModelRef{EntityName: opts.ModelName, ModelVersion: opts.ModelVersion}
	plan := planQuery(filter)

	// Committed candidate SQL: snapshot at tx.SnapshotTime, ORDER BY, no LIMIT/OFFSET.
	baseQuery, baseArgs := s.searchSnapshotBase(opts, timeToMicro(tx.SnapshotTime))
	if plan.where != "" {
		baseQuery += " AND (" + plan.where + ")"
		baseArgs = append(baseArgs, plan.args...)
	}
	baseQuery += orderByClause(opts, "ev")

	var results []*spi.Entity
	err := func() error {
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
		// post-filter, honour the scan budget. Never drains into a slice.
		scanned := 0
		next := func() (*spi.Entity, bool, error) {
			for rows.Next() {
				if plan.postFilter != nil && scanned >= s.cfg.SearchScanLimit {
					return nil, false, fmt.Errorf("%w: examined %d rows", ErrScanBudgetExhausted, s.cfg.SearchScanLimit)
				}
				scanned++
				e, scanErr := scanVersionEntity(rows)
				if scanErr != nil {
					return nil, false, scanErr
				}
				if plan.postFilter != nil {
					matches, evalErr := evaluateFilter(*plan.postFilter, e)
					if evalErr != nil {
						return nil, false, fmt.Errorf("post-filter evaluation: %w", evalErr)
					}
					if !matches {
						continue
					}
				}
				return e, true, nil
			}
			if err := rows.Err(); err != nil {
				return nil, false, fmt.Errorf("row iteration: %w", err)
			}
			return nil, false, nil
		}

		// adds = matching buffered own-writes for this model, excluding staged
		// deletes. copyEntity so no store-internal pointer escapes the lock.
		adds := make([]*spi.Entity, 0, len(tx.Buffer))
		for id, e := range tx.Buffer {
			if tx.Deletes[id] {
				continue
			}
			if e.Meta.ModelRef != modelRef {
				continue
			}
			if spi.MatchFilter(filter, e.Data, e.Meta) {
				adds = append(adds, copyEntity(e))
			}
		}
		sortEntitiesByOrder(adds, opts.OrderBy)

		// A committed row is suppressed if staged for delete OR shadowed by a
		// buffered own-write (the buffered version, if matching, arrives via adds).
		deleted := func(id string) bool {
			if tx.Deletes[id] {
				return true
			}
			_, buffered := tx.Buffer[id]
			return buffered
		}

		page, mErr := spi.MergePage(next, adds, deleted, opts.OrderBy, opts.Offset, opts.Limit)
		if mErr != nil {
			return mErr
		}

		// Read-set recording is CONDITIONAL on TrackingRead (unlike GetAll, which
		// records unconditionally). Only returned committed rows (not buffered —
		// those are own-writes already in the write-set) enter the read-set.
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

// orderByClause builds a SQL ORDER BY clause from opts.OrderBy.
//
//   - When OrderBy is empty, defaults to "ORDER BY entity_id".
//   - Each clause gets NULLS LAST so absent/null values sort after real values
//     regardless of ASC/DESC.
//   - A entity_id tiebreaker is appended unless the last OrderSpec already
//     resolves to entity_id (Source=SourceMeta, Path="id"), avoiding duplicates.
//   - tablePrefix is prepended to column references (e.g., "ev" for PIT queries).
func orderByClause(opts spi.SearchOptions, tablePrefix string) string {
	idCol := "entity_id"
	if tablePrefix != "" {
		idCol = tablePrefix + ".entity_id"
	}
	if len(opts.OrderBy) == 0 {
		return " ORDER BY " + idCol
	}
	clauses := make([]string, 0, len(opts.OrderBy)+1)
	for _, spec := range opts.OrderBy {
		expr := orderByFieldExpr(spec, tablePrefix)
		if spec.Desc {
			expr += " DESC"
		}
		clauses = append(clauses, expr+" NULLS LAST")
	}
	// Append entity_id tiebreaker unless the last spec already IS entity_id.
	if last := opts.OrderBy[len(opts.OrderBy)-1]; !(last.Source == spi.SourceMeta && last.Path == "id") {
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
