package sqlite

import (
	"context"
	"database/sql"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// ClassifyErrorForTest exposes classifyError for unit tests.
var ClassifyErrorForTest = classifyError

// ClassifyClaimErrorForTest exposes classifyClaimError for unit tests
// verifying the ErrUniqueViolation vs ErrConflict discrimination.
var ClassifyClaimErrorForTest = classifyClaimError

// DBForTest returns the underlying *sql.DB for diagnostic queries in tests
// (e.g. counting rows in unique_claims).
func DBForTest(f *StoreFactory) *sql.DB {
	return f.db
}

// SearchCandidateIDsForTest returns the entity IDs the SQL WHERE fragment
// planQuery(filter) produces BEFORE any Go-side postFilter re-check — i.e.
// the raw pushdown candidate set exactly as searchCommitted would scan it,
// minus the residual application step. Used by the pushdown-soundness
// property test (soundness_property_test.go) to assert the SQL pre-filter
// is a SUPERSET of the kernel's true matches (never under-selects) —
// independent of, and in addition to, the "backend result == kernel result"
// equality proxy that store.Search() (which DOES apply the residual)
// provides.
func SearchCandidateIDsForTest(f *StoreFactory, ctx context.Context, tenantID spi.TenantID, modelName, modelVersion string, filter spi.Filter) ([]string, error) {
	s := &entityStore{db: f.db, tenantID: tenantID, tm: f.tm, clock: f.clock, cfg: f.cfg}
	var plan sqlPlan
	if filter.Op != "" {
		plan = planQuery(filter)
	}
	baseQuery, baseArgs := s.searchCurrentStateBase(spi.SearchOptions{ModelName: modelName, ModelVersion: modelVersion})
	if plan.where != "" {
		baseQuery += " AND (" + plan.where + ")"
		baseArgs = append(baseArgs, plan.args...)
	}
	rows, err := s.db.QueryContext(ctx, baseQuery, baseArgs...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		e, err := scanEntityFromRow(rows)
		if err != nil {
			return nil, err
		}
		ids = append(ids, e.Meta.ID)
	}
	return ids, rows.Err()
}
