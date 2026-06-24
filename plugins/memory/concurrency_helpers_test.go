package memory_test

import (
	"errors"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// isToleratedClosedTxErr reports whether err is a benign "tx already
// closed/rolled back/committed/not found" outcome — the kind that is
// expected when a concurrent op races against Commit/Rollback/Savepoint/
// RollbackToSavepoint/Join. Anything else is a defect and should fail the
// test.
//
// Matches the three-way disjunction of transaction-state sentinels defined
// in cyoda-go-spi, plus bare ErrNotFound because in-read ops
// (Get/GetAll/Exists/Delete) can legitimately surface entity-level not-found
// when a concurrent Rollback or Commit changes visibility mid-op — that is
// a tolerated race outcome, not a defect.
func isToleratedClosedTxErr(err error) bool {
	return errors.Is(err, spi.ErrTxNotFound) ||
		errors.Is(err, spi.ErrTxTerminated) ||
		errors.Is(err, spi.ErrTxCommitInProgress) ||
		errors.Is(err, spi.ErrNotFound)
}
