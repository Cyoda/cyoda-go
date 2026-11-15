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
	})
}
