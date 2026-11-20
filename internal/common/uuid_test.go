package common_test

import (
	"testing"

	"github.com/google/uuid"

	"github.com/cyoda/cyoda-go/internal/common"
)

func TestDefaultUUIDGeneratorProducesV1(t *testing.T) {
	gen := common.NewDefaultUUIDGenerator()
	id := uuid.UUID(gen.NewTimeUUID())
	if id.Version() != 1 {
		t.Fatalf("expected UUID version 1, got %d", id.Version())
	}
}

func TestDefaultUUIDGeneratorUnique(t *testing.T) {
	gen := common.NewDefaultUUIDGenerator()
	a := gen.NewTimeUUID()
	b := gen.NewTimeUUID()
	if a == b {
		t.Fatalf("expected two different UUIDs, got %x twice", a)
	}
}

func TestTestUUIDGeneratorDeterministic(t *testing.T) {
	gen1 := common.NewTestUUIDGenerator()
	gen2 := common.NewTestUUIDGenerator()

	for i := 0; i < 5; i++ {
		a := gen1.NewTimeUUID()
		b := gen2.NewTimeUUID()
		if a != b {
			t.Fatalf("iteration %d: expected same UUID %x, got %x", i, a, b)
		}
		// Also verify version bits are set to 1.
		if uuid.UUID(a).Version() != 1 {
			t.Fatalf("iteration %d: expected UUID version 1, got %d", i, uuid.UUID(a).Version())
		}
	}
}
