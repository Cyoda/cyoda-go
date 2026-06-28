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
		                SELECT DISTINCT ON (entity_id) doc
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
