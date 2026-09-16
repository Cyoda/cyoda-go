package postgres

import "time"

// pitBaseQueryTemplate is the point-in-time base SELECT. It is a named
// constant for the same reason getPageCurrentQuery is: a later EXPLAIN
// assertion needs to plan the query that ACTUALLY runs, not a copy that
// drifts from it.
//
// Shape: one index probe per entity via idx_ev_bitemporal, instead of
// DISTINCT ON walking every revision of every entity up to the instant.
//
// The inner subquery projects exactly what the former `latest` derived table
// projected. The caller's pushdown condition and ORDER BY are generated with
// BARE column names (query_planner.go fieldExpr, searcher.go
// orderByFieldExpr) — `doc`, `entity_id`, `version`, `deleted`. Exposing both
// `entities` and the lateral to those expressions would make `doc` ambiguous
// and would silently resolve `version` against the entity's CURRENT row
// rather than its row at the instant.
//
// The model predicate is repeated inside the lateral: model membership is a
// property of the version row, and filtering only the entities row would move
// that axis onto the entity's current model.
//
// version DESC is the final tiebreak. Every row a transaction writes shares
// one valid_time and one transaction_time, so a delete-then-recreate in one
// transaction ties on both keys; without the tiebreak the winner is arbitrary
// and a plan change can flip it.
//
// $1 tenant, $2 entity name, $3 model version, $4 instant.
const pitBaseQueryTemplate = `SELECT doc FROM (
                SELECT v.doc, v.entity_id, v.version, v.model_name, v.model_version
                FROM entities e
                CROSS JOIN LATERAL (
                  SELECT ev.doc, ev.entity_id, ev.version, ev.model_name, ev.model_version
                  FROM entity_versions ev
                  WHERE ev.tenant_id = e.tenant_id AND ev.entity_id = e.entity_id
                    AND ev.model_name = $2 AND ev.model_version = $3
                    AND ev.valid_time <= $4
                    AND ev.transaction_time <= CURRENT_TIMESTAMP
                  ORDER BY ev.valid_time DESC, ev.transaction_time DESC, ev.version DESC
                  LIMIT 1
                ) v
                WHERE e.tenant_id = $1 AND e.model_name = $2 AND e.model_version = $3
             ) latest
             WHERE (doc->'_meta'->>'deleted')::boolean IS NOT TRUE`

// searchBaseQuery builds the base SELECT over a model for current-state
// (pit == nil) or point-in-time (pit != nil) reads. The outer projection is
// always `SELECT doc` (one column) — the S-1 invariant the row scanner
// (postgresIter, grouped_stats.go) depends on.
//
// Positional args: $1 tenant, $2 entityName, $3 modelVersion, and for PIT
// $4 the snapshot time. Callers append a pushdown WHERE fragment with
// shiftPlaceholders(frag, len(args)) and (for Search) ORDER BY / LIMIT / OFFSET.
//
// PIT uses the canonical inclusive bound valid_time <= $4 (no rounding), and
// follows entities: one lateral probe per row in `entities` rather than a
// DISTINCT ON over all of entity_versions. The equivalence to the old,
// revision-walking form rests on entities holding a row for every entity
// that ever existed (Save/Delete never remove it) and a tombstone version
// being filtered by the same deleted check either way — plus one property
// this file does not itself provide: an entity's model reference never
// changing after it is first set. That invariant, if enforced, belongs to
// Save's model-reference handling, not to this query — and as of this
// commit nothing enforces it: Save's entities upsert unconditionally
// rewrites model_name/model_version to the incoming value on every write.
// Without that enforcement, a Save that changes an entity's ModelRef mid-
// lifetime strands its earlier-model version history: this lateral repeats
// the model predicate against every version it probes (see below), so a
// version written under the old model no longer matches a PIT read issued
// under that old model, even though the same version was reachable there
// before the change.
//
// Shared by Iterate and Search so both stay in lock-step.
func (s *entityStore) searchBaseQuery(entityName, modelVersion string, pit *time.Time) (string, []any) {
	tid := string(s.tenantID)
	if pit != nil {
		return pitBaseQueryTemplate, []any{tid, entityName, modelVersion, *pit}
	}
	baseQuery := `SELECT doc
		             FROM entities
		             WHERE tenant_id = $1 AND model_name = $2 AND model_version = $3 AND NOT deleted`
	return baseQuery, []any{tid, entityName, modelVersion}
}

// committedQuerier is the querier EVERY point-in-time read runs through:
// GetAsAt, GetPage(asAt), and Search/Iterate with a PointInTime.
//
// A point-in-time read is committed-only — it ignores any ambient transaction
// and answers from committed state as of the requested instant. s.q would
// resolve the caller's pgx.Tx and hand back that transaction's own uncommitted
// writes, and the `transaction_time <= CURRENT_TIMESTAMP` guard in the PIT
// queries cannot filter them out: Save stamps valid_time/transaction_time from
// CURRENT_TIMESTAMP, which PostgreSQL fixes at transaction START, so inside the
// writing transaction the comparison reduces to T_start <= T_start. Pinning the
// pool is the only thing that actually reads committed state.
//
// Classification is the plain funnel rather than ctxQuerier's transaction-scoped
// one, for the same reason: the statement does not belong to the caller's
// transaction, so an error it raises must not reclaim that transaction's
// bookkeeping.
//
// No app.current_tenant GUC is set here, and none is needed: set_config's
// is_local flag scopes that setting to a transaction, so NO non-transactional
// statement this plugin issues has ever carried it — every pool-routed read
// (which is what a read outside a transaction already is) is in exactly this
// position. Tenant isolation on this path is the `WHERE tenant_id = $1`
// predicate the PIT queries all carry, which migration 000001 names as the
// primary mechanism; RLS is enabled-not-forced defence-in-depth on top.
//
// Not joining the caller's transaction makes an in-transaction read hold two
// connections at once, so the acquire is bounded: see unjoinedQuerier, which is
// the shared mechanism this and the async-search job store both take.
func (s *entityStore) committedQuerier() Querier {
	return unjoinedQuerier{pool: s.pool, acquireTimeout: s.acquireTimeout, what: "committed-only"}
}
