package app

import (
	"context"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/domain/consistency"
)

// newTestConsistency builds the consistency service over the transaction
// manager of the store factory a test already holds.
func newTestConsistency(tb testing.TB, f spi.StoreFactory) *consistency.Service {
	tb.Helper()
	tm, err := f.TransactionManager(context.Background())
	if err != nil {
		tb.Fatalf("transaction manager: %v", err)
	}
	return consistency.New(tm)
}
