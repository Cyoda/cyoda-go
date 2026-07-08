package sqlite

import "database/sql"

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
