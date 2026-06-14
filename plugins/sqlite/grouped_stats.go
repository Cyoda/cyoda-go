package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// This file implements spi.Iterable and spi.GroupedAggregator on
// *entityStore for issue #299 (grouped entity statistics query).
//
// Design (spec §6.2):
//   - Iterate reuses the existing planQuery() WHERE-pushdown machinery and
//     evaluateFilter() residual evaluator from searcher.go, exactly mirroring
//     Search's pre-filter + post-filter pattern. The only structural
//     difference is that results are streamed through an Iterator wrapper
//     around sql.Rows instead of being collected into a slice.
//   - GroupedAggregate pushes COUNT/SUM/AVG/MIN/MAX as a native SQL GROUP BY.
//     It opts out (ErrAggregationNotPushdownable) whenever the request shape
//     can't be safely pushed:
//       * any AggStdev requested (D9 — sqlite has no native STDDEV and the
//         single-pass numerically-unsafe formula isn't acceptable; service
//         layer falls through to Iterate-streaming-tally with Welford).
//       * the filter has a residual (post-aggregation residual application
//         can't reconstruct per-bucket counts safely).
//       * opts.PointInTime is set (PIT joins are out of scope for v1; service
//         layer streaming-tally over Iterate handles it).
//
// Cardinality detection follows D17: we LIMIT MaxBuckets+1 and surface
// ErrGroupCardinalityExceeded the moment we observe MaxBuckets+1 rows.
//
// GROUP BY uses the full extractor expression (not the column alias) for
// portability across SQL dialects — even where sqlite would accept alias
// references, we don't rely on it.

// Compile-time interface checks.
var (
	_ spi.Iterable          = (*entityStore)(nil)
	_ spi.GroupedAggregator = (*entityStore)(nil)
)

// Iterate implements spi.Iterable.
//
// Returns an iterator over entities in the model matching filter. Pushable
// filter parts go into SQL WHERE; the residual is applied inside Next() via
// the same evaluateFilter() used by Search.
//
// PointInTime (when non-nil) walks entity_versions at the requested snapshot
// time, matching the convention used by searchPointInTimeBase.
func (s *entityStore) Iterate(
	ctx context.Context,
	model spi.ModelRef,
	filter spi.Filter,
	opts spi.IterateOptions,
) (spi.Iterator, error) {
	if err := validateFilterPaths(filter); err != nil {
		return nil, err
	}
	// Zero-value Filter means "match all" per the spi.Iterable contract.
	// Skip planQuery — it would treat the empty Op as non-pushable and
	// install the zero filter as a residual, breaking evaluateFilter.
	var plan sqlPlan
	if filter.Op != "" {
		plan = planQuery(filter)
	}

	searchOpts := spi.SearchOptions{
		ModelName:    model.EntityName,
		ModelVersion: model.ModelVersion,
		PointInTime:  opts.PointInTime,
	}

	var baseQuery string
	var baseArgs []any
	pointInTime := opts.PointInTime != nil
	if pointInTime {
		baseQuery, baseArgs = s.searchPointInTimeBase(searchOpts)
	} else {
		baseQuery, baseArgs = s.searchCurrentStateBase(searchOpts)
	}

	if plan.where != "" {
		baseQuery += " AND (" + plan.where + ")"
		baseArgs = append(baseArgs, plan.args...)
	}

	rows, err := s.db.QueryContext(ctx, baseQuery, baseArgs...)
	if err != nil {
		return nil, fmt.Errorf("iterate query: %w", err)
	}
	return &sqliteIter{
		ctx:         ctx,
		rows:        rows,
		postFilter:  plan.postFilter,
		pointInTime: pointInTime,
	}, nil
}

// sqliteIter wraps sql.Rows, applying any residual filter inside Next()
// before yielding each row. Err() is sticky; Close() is idempotent;
// ctx cancellation is observed.
type sqliteIter struct {
	ctx         context.Context
	rows        *sql.Rows
	postFilter  *spi.Filter
	pointInTime bool

	cur    *spi.Entity
	err    error
	closed bool
}

func (it *sqliteIter) Next() bool {
	if it.err != nil || it.closed {
		return false
	}
	if err := it.ctx.Err(); err != nil {
		it.err = err
		return false
	}
	for it.rows.Next() {
		if err := it.ctx.Err(); err != nil {
			it.err = err
			return false
		}
		var (
			e    *spi.Entity
			serr error
		)
		if it.pointInTime {
			e, serr = scanVersionEntity(it.rows)
		} else {
			e, serr = scanEntityFromRow(it.rows)
		}
		if serr != nil {
			it.err = serr
			return false
		}
		if it.postFilter != nil {
			ok, ferr := evaluateFilter(*it.postFilter, e)
			if ferr != nil {
				it.err = fmt.Errorf("post-filter evaluation: %w", ferr)
				return false
			}
			if !ok {
				continue
			}
		}
		it.cur = e
		return true
	}
	if err := it.rows.Err(); err != nil {
		it.err = fmt.Errorf("row iteration: %w", err)
	}
	return false
}

func (it *sqliteIter) Entity() *spi.Entity { return it.cur }
func (it *sqliteIter) Err() error          { return it.err }

func (it *sqliteIter) Close() error {
	if it.closed {
		return nil
	}
	it.closed = true
	it.cur = nil
	if it.rows != nil {
		return it.rows.Close()
	}
	return nil
}

// GroupedAggregate implements spi.GroupedAggregator. Returns
// ErrAggregationNotPushdownable for request shapes the SQL path cannot
// safely cover (stdev / residual filter / point-in-time); the caller is
// expected to fall back to Iterate-driven streaming tally.
func (s *entityStore) GroupedAggregate(
	ctx context.Context,
	model spi.ModelRef,
	groupBy []spi.GroupExpr,
	filter spi.Filter,
	opts spi.GroupedAggregationsOptions,
) ([]spi.GroupedAggregateBucket, error) {
	// D9: sqlite has no native STDDEV and the single-pass formula is
	// numerically unsafe. Decline so service layer uses Welford in-memory.
	for _, a := range opts.Aggregations {
		if a.Op == spi.AggStdev {
			return nil, spi.ErrAggregationNotPushdownable
		}
	}

	// PIT pushdown is out of scope for v1 — streaming tally over Iterate
	// (which does support PIT) handles it without per-query SQL plumbing.
	if opts.PointInTime != nil {
		return nil, spi.ErrAggregationNotPushdownable
	}

	if err := validateFilterPaths(filter); err != nil {
		return nil, err
	}
	// Zero-value Filter means "match all" (same convention as Iterable).
	var plan sqlPlan
	if filter.Op != "" {
		plan = planQuery(filter)
	}
	if plan.postFilter != nil {
		// A SQL GROUP BY can't safely apply a residual filter after the
		// fact — it would corrupt per-bucket counts/aggregates. Defer to
		// streaming tally.
		return nil, spi.ErrAggregationNotPushdownable
	}

	groupExprs := make([]string, len(groupBy))
	groupPaths := make([]string, len(groupBy))
	for i, g := range groupBy {
		expr, path, err := groupExprToSQL(g)
		if err != nil {
			return nil, err
		}
		groupExprs[i] = expr
		groupPaths[i] = path
	}

	// SELECT list: group-key columns first, then COUNT(*), then each agg.
	selectParts := make([]string, 0, len(groupExprs)+1+len(opts.Aggregations))
	for i, expr := range groupExprs {
		selectParts = append(selectParts, fmt.Sprintf("%s AS gk_%d", expr, i))
	}
	selectParts = append(selectParts, "COUNT(*) AS gs_count")
	for i, a := range opts.Aggregations {
		aggSQL, err := aggregateExprToSQL(a)
		if err != nil {
			return nil, err
		}
		selectParts = append(selectParts, fmt.Sprintf("%s AS agg_%d", aggSQL, i))
	}

	q := "SELECT " + strings.Join(selectParts, ", ")
	q += " FROM entities WHERE tenant_id = ? AND model_name = ? AND model_version = ? AND NOT deleted"
	args := []any{string(s.tenantID), model.EntityName, model.ModelVersion}
	if plan.where != "" {
		q += " AND (" + plan.where + ")"
		args = append(args, plan.args...)
	}
	if len(groupExprs) > 0 {
		// GROUP BY uses full expressions (not aliases) for portability — D17.
		q += " GROUP BY " + strings.Join(groupExprs, ", ")
	}
	// D17 cardinality detection: select one over the cap so we can detect
	// overflow without an extra COUNT(DISTINCT) round-trip.
	limit := opts.MaxBuckets + 1
	if opts.MaxBuckets <= 0 {
		limit = 0 // 0 disables LIMIT, treat as unbounded
	}
	if limit > 0 {
		q += " LIMIT ?"
		args = append(args, limit)
	}

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("grouped aggregate query: %w", err)
	}
	defer rows.Close()

	// Build scan destinations once. Each group-key column is sql.NullString
	// (a JSON-extracted scalar text or NULL), the count is int64, each
	// aggregate is sql.NullFloat64.
	nGroups := len(groupExprs)
	nAggs := len(opts.Aggregations)
	groupVals := make([]sql.NullString, nGroups)
	var countVal int64
	aggVals := make([]sql.NullFloat64, nAggs)

	scanDest := make([]any, 0, nGroups+1+nAggs)
	for i := range groupVals {
		scanDest = append(scanDest, &groupVals[i])
	}
	scanDest = append(scanDest, &countVal)
	for i := range aggVals {
		scanDest = append(scanDest, &aggVals[i])
	}

	out := make([]spi.GroupedAggregateBucket, 0, 16)
	for rows.Next() {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if err := rows.Scan(scanDest...); err != nil {
			return nil, fmt.Errorf("scan grouped row: %w", err)
		}
		// D17: if we ever see MaxBuckets+1 rows, surface the cap-exceeded
		// signal. The driver may not let us short-circuit the rows.Next
		// loop cleanly, but we exit immediately on the overflow row.
		if opts.MaxBuckets > 0 && len(out) >= opts.MaxBuckets {
			return nil, spi.ErrGroupCardinalityExceeded
		}

		bucket := spi.GroupedAggregateBucket{
			GroupKey: make([]spi.GroupKeyEntry, nGroups),
			Count:    countVal,
		}
		for i, gv := range groupVals {
			var v any
			if gv.Valid {
				v = gv.String
			}
			bucket.GroupKey[i] = spi.GroupKeyEntry{
				Path:  groupPaths[i],
				Value: v,
			}
		}
		if nAggs > 0 {
			bucket.Aggregations = make(map[string]any, nAggs)
			for i, a := range opts.Aggregations {
				if aggVals[i].Valid {
					bucket.Aggregations[a.Alias] = aggVals[i].Float64
				} else {
					bucket.Aggregations[a.Alias] = nil
				}
			}
		}
		out = append(out, bucket)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate grouped rows: %w", err)
	}

	return out, nil
}

// ErrInvalidGroupExpr signals a malformed spi.GroupExpr reaching the plugin.
// Inputs are normally validated upstream (Task 6); this is defence-in-depth.
var ErrInvalidGroupExpr = errors.New("invalid group expression")

// groupExprToSQL translates a spi.GroupExpr into a SQL expression and the
// canonical path string that ends up in the response GroupKeyEntry.Path.
//
// Safety invariant: g.Path (for GroupExprDataPath) MUST have been validated
// via validateJSONPath; the validator's grammar permits only characters that
// cannot break out of a single-quoted JSON path literal.
func groupExprToSQL(g spi.GroupExpr) (string, string, error) {
	switch g.Kind {
	case spi.GroupExprState:
		// json_extract on meta JSONB blob. Missing/null state surfaces as
		// SQL NULL → sql.NullString.Valid==false → response value nil,
		// matching the streaming-tally path's coercion (no collapse to "").
		return "json_extract(json(meta), '$.state')", "state", nil
	case spi.GroupExprDataPath:
		if err := validateJSONPath(g.Path); err != nil {
			return "", "", err
		}
		// D4: a runtime object/array at a scalar JSONPath MUST coerce to
		// null so it merges with explicit-null/missing buckets, matching
		// the streaming-tally path (gjson.JSON → nil in
		// buildGroupKeyFromEntity). Without this guard, sqlite's
		// json_extract returns the JSON text '{"k":"v"}' / '[1,2,3]'
		// verbatim and produces distinct buckets per object content.
		//
		// The same wrapped expression goes into both the SELECT (with
		// `AS gk_N`) and the GROUP BY clause — caller relies on that
		// identity, see groupExprs reuse in GroupedAggregate.
		//
		// Use the two-arg form `json_type(data, '$.path')` rather than
		// `json_type(json_extract(...))` — json_type on a scalar string
		// raises "malformed JSON" because the extracted scalar is no
		// longer JSON-formatted, but the path form returns the type of
		// the value at the path directly.
		extract := fmt.Sprintf("json_extract(data, '$.%s')", g.Path)
		typeCheck := fmt.Sprintf("json_type(data, '$.%s')", g.Path)
		expr := fmt.Sprintf("CASE WHEN %s IN ('object','array') THEN NULL ELSE %s END", typeCheck, extract)
		return expr, g.Path, nil
	default:
		return "", "", fmt.Errorf("%w: unknown kind %d", ErrInvalidGroupExpr, g.Kind)
	}
}

// ErrInvalidAggregate signals a malformed AggregateExpr reaching the plugin.
var ErrInvalidAggregate = errors.New("invalid aggregate expression")

// aggregateExprToSQL translates AggSum/AggAvg/AggMin/AggMax into the matching
// SQL aggregate over json_extract(data, '$.<field>') cast to REAL. Returns an
// error for AggStdev (caller filters it out earlier) or unknown ops.
//
// Safety invariant: a.Field MUST have been validated upstream — see
// validateJSONPath grammar.
func aggregateExprToSQL(a spi.AggregateExpr) (string, error) {
	if err := validateJSONPath(a.Field); err != nil {
		return "", err
	}
	col := fmt.Sprintf("CAST(json_extract(data, '$.%s') AS REAL)", a.Field)
	switch a.Op {
	case spi.AggSum:
		return "SUM(" + col + ")", nil
	case spi.AggAvg:
		return "AVG(" + col + ")", nil
	case spi.AggMin:
		return "MIN(" + col + ")", nil
	case spi.AggMax:
		return "MAX(" + col + ")", nil
	case spi.AggStdev:
		// Caller filters stdev out before reaching us. Defence-in-depth.
		return "", fmt.Errorf("%w: stdev not pushable", ErrInvalidAggregate)
	default:
		return "", fmt.Errorf("%w: unknown op %q", ErrInvalidAggregate, a.Op)
	}
}

