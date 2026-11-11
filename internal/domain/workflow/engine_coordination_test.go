package workflow

import (
	"testing"

	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/platformclock"
	"github.com/cyoda-platform/cyoda-go/plugins/memory"
)

// An engine that makes callouts must be given the coordination its callouts
// are registered with: one of its own would place its processor scopes where
// the doors and the owner's loop never look. It is refused at construction.
func TestNewEngine_ExternalProcessingWithoutCoordinationPanics(t *testing.T) {
	factory := memory.NewStoreFactory()
	t.Cleanup(func() { factory.Close() })
	uuids := common.NewTestUUIDGenerator()
	defer func() {
		if recover() == nil {
			t.Fatal("NewEngine with external processing and no coordination did not panic")
		}
	}()
	NewEngine(factory, uuids, factory.NewTransactionManager(uuids), platformclock.Real(), WithExternalProcessing(&mockExternalProcessing{}))
}

// An engine that makes no callouts has no callbacks and no processor scopes
// to share: it builds its own coordination.
func TestNewEngine_NoExternalProcessingNeedsNoCoordination(t *testing.T) {
	factory := memory.NewStoreFactory()
	t.Cleanup(func() { factory.Close() })
	uuids := common.NewTestUUIDGenerator()
	if e := NewEngine(factory, uuids, factory.NewTransactionManager(uuids), platformclock.Real()); e.coord == nil {
		t.Fatal("no coordination")
	}
}
