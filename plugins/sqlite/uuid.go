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
	// uuid.NewUUID reads the clock and the node id; with the node id cached
	// after the first call it cannot fail, and the interface has no error.
	id, _ := uuid.NewUUID()
	return [16]byte(id)
}

var _ spi.UUIDGenerator = (*defaultUUIDGenerator)(nil)
