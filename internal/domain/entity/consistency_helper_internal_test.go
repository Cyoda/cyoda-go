package entity

import (
	"context"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/domain/consistency"
)

// newTestConsistencyFor builds the consistency service over the transaction
// manager a test already hands to New.
func newTestConsistencyFor(tm spi.TransactionManager) *consistency.Service {
	return consistency.New(tm)
}

// fixedInstantTM is a transaction manager whose only working method is
// ConsistencyTime, which answers a fixed instant chosen here: for tests that
// have no transaction manager but must supply the service.
type fixedInstantTM struct {
	spi.TransactionManager
	at time.Time
}

func (f fixedInstantTM) ConsistencyTime(context.Context) (time.Time, error) { return f.at, nil }

// newFixedConsistency builds the consistency service over a fixed instant.
func newFixedConsistency() *consistency.Service {
	return consistency.New(fixedInstantTM{at: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)})
}
