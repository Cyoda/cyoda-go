package postgres

import (
	"github.com/google/uuid"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// defaultUUIDGenerator produces real UUID v1 values. Used by NewFactory
// to initialize the plugin's TransactionManager.
type defaultUUIDGenerator struct{}

func (g *defaultUUIDGenerator) NewTimeUUID() [16]byte {
	// google/uuid v1.6.0's NewUUID can only fail via GetTime, which always
	// returns a nil error (the return exists for API compatibility, not
	// because it can happen); the node id falls back to a random value when
	// no hardware interface is available. The discarded error is safe, and
	// the UUIDGenerator interface has no error to return regardless.
	id, _ := uuid.NewUUID()
	return [16]byte(id)
}

var _ spi.UUIDGenerator = (*defaultUUIDGenerator)(nil)
