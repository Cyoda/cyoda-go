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

// orderByClause builds the SQL ORDER BY from opts.OrderBy. Empty → default
// "ORDER BY entity_id" (a unique, never-null key, so pages are deterministic).
// Bare column names resolve against the entities table (current-state) or the
// `latest` derived table (point-in-time), both of which expose entity_id.
func orderByClause(opts spi.SearchOptions) string {
	if len(opts.OrderBy) == 0 {
		return " ORDER BY entity_id"
	}
	clauses := make([]string, 0, len(opts.OrderBy))
	for _, spec := range opts.OrderBy {
		expr := orderByFieldExpr(spec)
		if spec.Desc {
			expr += " DESC"
		}
		clauses = append(clauses, expr)
	}
	return " ORDER BY " + strings.Join(clauses, ", ")
}

// orderByFieldExpr returns the SQL ordering expression for an OrderSpec,
// reusing the filter field rules (directMetaColumns / doc->'_meta'->>'p' /
// doc->>'p').
//
// Safety invariant: spec.Path is interpolated into a JSON-key literal and
// MUST have been validated by validateOrderSpecs at the Search() boundary.
func orderByFieldExpr(spec spi.OrderSpec) string {
	if spec.Source == spi.SourceMeta {
		if directMetaColumns[spec.Path] {
			return spec.Path
		}
		return jsonbExtractText("doc->'_meta'", spec.Path)
	}
	return jsonbExtractText("doc", spec.Path)
}
