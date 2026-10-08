package search_test

import (
	"context"
	"testing"
	"time"

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

// fixedInstantTM is a transaction manager whose only working method is
// ConsistencyTime, which answers a fixed instant chosen here: for tests whose
// subject never reads the consistency time but must supply the service.
type fixedInstantTM struct {
	spi.TransactionManager
	at time.Time
}

func (f fixedInstantTM) ConsistencyTime(context.Context) (time.Time, error) { return f.at, nil }

// newFixedConsistency builds the consistency service over a fixed instant.
func newFixedConsistency() *consistency.Service {
	return consistency.New(fixedInstantTM{at: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)})
}
