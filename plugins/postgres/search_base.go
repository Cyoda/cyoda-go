package postgres

import "time"

// searchBaseQuery builds the base SELECT over a model for current-state
// (pit == nil) or point-in-time (pit != nil) reads. The outer projection is
// always `SELECT doc` (one column) — the S-1 invariant the row scanner
// (postgresIter, grouped_stats.go) depends on.
//
// Positional args: $1 tenant, $2 entityName, $3 modelVersion, and for PIT
// $4 the snapshot time. Callers append a pushdown WHERE fragment with
// shiftPlaceholders(frag, len(args)) and (for Search) ORDER BY / LIMIT / OFFSET.
//
// PIT uses the canonical inclusive bound valid_time <= $4 (no rounding).
// Shared by Iterate and Search so both stay in lock-step.
func (s *entityStore) searchBaseQuery(entityName, modelVersion string, pit *time.Time) (string, []any) {
	tid := string(s.tenantID)
	if pit != nil {
		// Bi-temporal snapshot: inner DISTINCT ON picks the latest version per
		// entity visible at the snapshot; outer drops deletion-marker versions
		// AFTER the DISTINCT ON (so a delete shadows an older live version).
		baseQuery := `SELECT doc FROM (
		                SELECT DISTINCT ON (entity_id)
		                       entity_id, model_name, model_version, version, doc
		                FROM entity_versions
		                WHERE tenant_id = $1 AND model_name = $2 AND model_version = $3
		                  AND valid_time <= $4
		                  AND transaction_time <= CURRENT_TIMESTAMP
		                ORDER BY entity_id, valid_time DESC, transaction_time DESC
		             ) latest
		             WHERE (doc->'_meta'->>'deleted')::boolean IS NOT TRUE`
		return baseQuery, []any{tid, entityName, modelVersion, *pit}
	}
	baseQuery := `SELECT doc
		             FROM entities
		             WHERE tenant_id = $1 AND model_name = $2 AND model_version = $3 AND NOT deleted`
	return baseQuery, []any{tid, entityName, modelVersion}
}

// committedQuerier is the querier EVERY point-in-time read runs through:
// GetAsAt, GetAllAsAt, GetPage(asAt), and Search/Iterate with a PointInTime.
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
