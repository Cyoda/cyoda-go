package sqlite

import (
	"github.com/google/uuid"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// defaultUUIDGenerator produces time-based (version 1) UUIDs, as the SPI's
// UUIDGenerator contract asks. Used by NewFactory for the transaction
// manager and the state machine audit store.
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
