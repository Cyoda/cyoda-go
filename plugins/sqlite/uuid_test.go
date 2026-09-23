package sqlite

import (
	"testing"

	"github.com/google/uuid"
)

// The SPI asks a UUIDGenerator for time-ordered (version 1) ids; the audit
// event id and the transaction id both rely on it.
func TestDefaultUUIDGenerator_IsVersion1(t *testing.T) {
	id := uuid.UUID((&defaultUUIDGenerator{}).NewTimeUUID())
	if id.Version() != 1 {
		t.Fatalf("NewTimeUUID returned version %d, want 1", id.Version())
	}
}
