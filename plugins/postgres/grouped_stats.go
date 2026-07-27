package postgres

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// This file implements spi.Iterable and spi.GroupedAggregator on
// *entityStore for the grouped entity statistics query endpoint
// (POST /api/entity/stats/{name}/{version}/query).
//
// Design (spec §6.2, postgres variant):
//   - Iterate reuses the planQuery() WHERE-pushdown machinery from
//     query_planner.go (Task 14). The residual filter is applied inside
//     Next() via an in-plugin evaluator (evalPostFilter below) — the same
//     pre-filter + post-filter pattern sqlite uses, kept self-contained
//     because plugins are sibling modules and cannot import each other.
//   - GroupedAggregate pushes COUNT/SUM/AVG/MIN/MAX *and* STDDEV_SAMP as a
//     native SQL GROUP BY. Unlike sqlite (D9 opts out of stdev because the
//     one-pass formula is numerically unsafe), postgres's STDDEV_SAMP is
//     numerically stable, so the request shape is always pushable as long
//     as no residual and no PIT are involved.
//
// Decline cases (return spi.ErrAggregationNotPushdownable):
//   - Filter has a residual (post-aggregation residual application can't
//     reconstruct per-bucket counts safely).
//   - opts.PointInTime is set (PIT GROUP BY would need a DISTINCT ON wrapper;
//     out of scope for v1 — service layer falls through to streaming tally
//     over Iterate, which DOES support PIT).
//
// Cardinality detection (D17): LIMIT MaxBuckets+1 and surface
// ErrGroupCardinalityExceeded the moment we observe MaxBuckets+1 rows.
//
// GROUP BY uses the full extractor expression (not the column alias) for
// portability across SQL dialects.

// Compile-time interface checks.
var (
	_ spi.Iterable          = (*entityStore)(nil)
	_ spi.GroupedAggregator = (*entityStore)(nil)
)

// Iterate implements spi.Iterable.
//
// Returns an iterator over entities in the model matching filter. Pushable
// filter parts go into SQL WHERE; the residual is applied inside Next() via
// evalPostFilter.
//
// PointInTime (when non-nil) walks entity_versions at the requested snapshot
// using DISTINCT ON to surface only the latest visible version per entity,
// then excludes deletion-marker versions.
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
	// install the zero filter as a residual.
	var plan sqlPlan
	if filter.Op != "" {
		plan = planQuery(filter)
	}

	baseQuery, baseArgs := s.searchBaseQuery(model.EntityName, model.ModelVersion, opts.PointInTime)

	if plan.where != "" {
		// planQuery starts its placeholder counter at 1, but we've already
		// consumed $1..$N for the base args. Renumber.
		shifted := shiftPlaceholders(plan.where, len(baseArgs))
		baseQuery += " AND (" + shifted + ")"
		baseArgs = append(baseArgs, plan.args...)
	}

	rows, err := s.q.Query(ctx, baseQuery, baseArgs...)
	if err != nil {
		return nil, fmt.Errorf("iterate query: %w", err)
	}
	return &postgresIter{
		ctx:        ctx,
		rows:       rows,
		postFilter: plan.postFilter,
	}, nil
}

// shiftPlaceholders renumbers $N tokens in s, adding offset to each. Used to
// merge planQuery's WHERE fragment (which starts at $1) into a query that
// has already used $1..$offset for base args (tenant/model/PIT).
//
// The transformation is purely lexical on $N where N is a decimal integer
// directly following '$'. planQuery only emits this form (see toSQL +
// nextPlaceholder), so a byte-level rewrite is sufficient and avoids
// pulling in a full SQL parser.
func shiftPlaceholders(s string, offset int) string {
	if offset == 0 {
		return s
	}
	var b strings.Builder
	b.Grow(len(s) + 4)
	for i := 0; i < len(s); i++ {
		if s[i] != '$' {
			b.WriteByte(s[i])
			continue
		}
		// Collect the run of digits after '$'.
		j := i + 1
		for j < len(s) && s[j] >= '0' && s[j] <= '9' {
			j++
		}
		if j == i+1 {
			// Lone '$' with no digits — preserve verbatim.
			b.WriteByte('$')
			continue
		}
		var n int
		for k := i + 1; k < j; k++ {
			n = n*10 + int(s[k]-'0')
		}
		fmt.Fprintf(&b, "$%d", n+offset)
		i = j - 1
	}
	return b.String()
}

// postgresIter wraps pgx.Rows, applying any residual filter inside Next()
// before yielding each row. Err() is sticky; Close() is idempotent;
// ctx cancellation is observed.
type postgresIter struct {
	ctx        context.Context
	rows       pgx.Rows
	postFilter *spi.Filter

	cur    *spi.Entity
	err    error
	closed bool
}

func (it *postgresIter) Next() bool {
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
		var doc []byte
		if err := it.rows.Scan(&doc); err != nil {
			it.err = fmt.Errorf("scan iterate row: %w", err)
			return false
		}
		e, err := unmarshalEntityDoc(doc)
		if err != nil {
			it.err = fmt.Errorf("unmarshal iterate row: %w", err)
			return false
		}
		if it.postFilter != nil {
			ok, ferr := evalPostFilter(*it.postFilter, e, doc)
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

func (it *postgresIter) Entity() *spi.Entity { return it.cur }
func (it *postgresIter) Err() error          { return it.err }

func (it *postgresIter) Close() error {
	if it.closed {
		return nil
	}
	it.closed = true
	it.cur = nil
	if it.rows != nil {
		it.rows.Close()
	}
	return nil
}

// GroupedAggregate implements spi.GroupedAggregator. Returns
// ErrAggregationNotPushdownable for request shapes the SQL path cannot
// safely cover (residual filter / point-in-time); the caller is expected
// to fall back to Iterate-driven streaming tally.
func (s *entityStore) GroupedAggregate(
	ctx context.Context,
	model spi.ModelRef,
	groupBy []spi.GroupExpr,
	filter spi.Filter,
	opts spi.GroupedAggregationsOptions,
) ([]spi.GroupedAggregateBucket, error) {
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
	q += " FROM entities WHERE tenant_id = $1 AND model_name = $2 AND model_version = $3 AND NOT deleted"
	args := []any{string(s.tenantID), model.EntityName, model.ModelVersion}
	if plan.where != "" {
		shifted := shiftPlaceholders(plan.where, len(args))
		q += " AND (" + shifted + ")"
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
		placeholder := fmt.Sprintf("$%d", len(args)+1)
		q += " LIMIT " + placeholder
		args = append(args, limit)
	}

	rows, err := s.q.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("grouped aggregate query: %w", err)
	}
	defer rows.Close()

	// Build scan destinations once. Each group-key column is a nullable
	// string (a JSONB-extracted scalar text or NULL), the count is int64,
	// each aggregate is a nullable float64.
	nGroups := len(groupExprs)
	nAggs := len(opts.Aggregations)
	groupVals := make([]*string, nGroups)
	var countVal int64
	aggVals := make([]*float64, nAggs)

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
		// D17: if we ever see MaxBuckets+1 rows, surface the cap-exceeded signal.
		if opts.MaxBuckets > 0 && len(out) >= opts.MaxBuckets {
			return nil, spi.ErrGroupCardinalityExceeded
		}

		bucket := spi.GroupedAggregateBucket{
			GroupKey: make([]spi.GroupKeyEntry, nGroups),
			Count:    countVal,
		}
		for i, gv := range groupVals {
			var v any
			if gv != nil {
				v = *gv
			}
			bucket.GroupKey[i] = spi.GroupKeyEntry{
				Path:  groupPaths[i],
				Value: v,
			}
		}
		if nAggs > 0 {
			bucket.Aggregations = make(map[string]any, nAggs)
			for i, a := range opts.Aggregations {
				if aggVals[i] != nil {
					bucket.Aggregations[a.Alias] = *aggVals[i]
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
// Safety invariant: g.Path (for GroupExprDataPath) MUST be validated by
// validateJSONPath before interpolation. We re-validate at the SQL boundary
// as defence-in-depth — a malformed path surfaces as a validation error
// rather than malformed SQL.
func groupExprToSQL(g spi.GroupExpr) (string, string, error) {
	switch g.Kind {
	case spi.GroupExprState:
		// State lives in the JSONB doc at $._meta.state. Missing state surfaces
		// as SQL NULL → nil-pointer scan → response value nil, matching the
		// streaming-tally path's coercion.
		return "doc->'_meta'->>'state'", "state", nil
	case spi.GroupExprDataPath:
		if err := validateJSONPath(g.Path); err != nil {
			return "", "", err
		}
		// D4: a runtime object/array at a scalar JSONPath MUST coerce to
		// null so it merges with explicit-null/missing buckets, matching
		// the streaming-tally path (gjson.JSON → nil in
		// buildGroupKeyFromEntity). Without this guard, postgres's
		// doc->>'tag' returns the JSON text of an object/array verbatim
		// and produces distinct buckets per object content.
		//
		// The same wrapped expression goes into both the SELECT (with
		// `AS gk_N`) and the GROUP BY clause — caller relies on that
		// identity, see groupExprs reuse in GroupedAggregate.
		textExpr := jsonbExtractText("doc", g.Path)
		typeExpr := jsonbExtractJSONB("doc", g.Path)
		expr := fmt.Sprintf("CASE WHEN jsonb_typeof(%s) IN ('object','array') THEN NULL ELSE %s END", typeExpr, textExpr)
		return expr, g.Path, nil
	default:
		return "", "", fmt.Errorf("%w: unknown kind %d", ErrInvalidGroupExpr, g.Kind)
	}
}

// ErrInvalidAggregate signals a malformed AggregateExpr reaching the plugin.
var ErrInvalidAggregate = errors.New("invalid aggregate expression")

// aggregateExprToSQL translates AggSum/AggAvg/AggMin/AggMax/AggStdev into the
// matching SQL aggregate over cyoda_try_float8(doc->>'<field>'). Unlike
// sqlite, postgres pushes stdev natively via STDDEV_SAMP — the function is
// numerically stable on postgres.
//
// Safety invariant: a.Field is re-validated at the SQL boundary even though
// Task 6 should have validated upstream.
func aggregateExprToSQL(a spi.AggregateExpr) (string, error) {
	if err := validateJSONPath(a.Field); err != nil {
		return "", err
	}
	col := fmt.Sprintf("cyoda_try_float8(%s)", jsonbExtractText("doc", a.Field))
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
		// postgres STDDEV_SAMP is numerically stable; safe to push.
		return "STDDEV_SAMP(" + col + ")", nil
	default:
		return "", fmt.Errorf("%w: unknown op %q", ErrInvalidAggregate, a.Op)
	}
}

// ---------- Post-filter evaluator (residual application) ----------

// evalPostFilter evaluates a residual Filter subtree against a decoded
// entity in Go. Delegates to spi.MatchFilter, the canonical cross-backend
// evaluator that sqlite's post-filter and the memory plugin also use — see
// that function's doc for why the two must never diverge.
func evalPostFilter(f spi.Filter, entity *spi.Entity, doc []byte) (bool, error) {
	return spi.MatchFilter(f, doc, entity.Meta), nil
}
