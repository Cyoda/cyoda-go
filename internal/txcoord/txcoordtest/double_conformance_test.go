package txcoordtest_test

import (
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	spitest "github.com/cyoda-platform/cyoda-go-spi/spitest"

	"github.com/cyoda-platform/cyoda-go/internal/txcoord/txcoordtest"
	"github.com/cyoda-platform/cyoda-go/plugins/memory"
)

// TestDouble_Conformance runs the SPI conformance suite, its Coordination
// cases included, on the test double: the memory plugin with a transaction
// coordinator. Outside a scope the double is the memory plugin itself, so the
// other cases pin that the wrapper changes nothing there.
func TestDouble_Conformance(t *testing.T) {
	clock := memory.NewTestClock()
	factory := txcoordtest.NewFactory(memory.NewStoreFactory(memory.WithClock(clock)))
	if _, ok := factory.(spi.TransactionCoordinationCapable); !ok {
		t.Fatal("the double must offer the transaction-coordination capability")
	}
	spitest.StoreFactoryConformance(t, spitest.Harness{
		Factory:      factory,
		AdvanceClock: clock.Advance,
		Now:          clock.Now,
		Skip: map[string]string{
			// Two of these cases commit the entity they conflict over after
			// the case's transaction began. A transaction reads at the snapshot
			// Begin took (spi.TransactionState.SnapshotTime), so the memory
			// plugin cannot see it. CompleteRefusalOrder's cases are not
			// separately skippable. double_test.go runs all three with the
			// entity committed first.
			"Coordination/ScopeConflict":        seededAfterBegin,
			"Coordination/CompleteRefusalOrder": seededAfterBegin,
		},
	})
}

const seededAfterBegin = "the case commits its entity after its transaction began, which a snapshot taken at Begin cannot see"
