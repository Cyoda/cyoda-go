package memory_test

import (
	"testing"

	spitest "github.com/cyoda-platform/cyoda-go-spi/spitest"

	"github.com/cyoda-platform/cyoda-go/plugins/memory"
)

// TestConformance runs the SPI conformance harness against the memory plugin
// with a deterministic TestClock for temporal subtests.
func TestConformance(t *testing.T) {
	clock := memory.NewTestClock()
	factory := memory.NewStoreFactory(memory.WithClock(clock))
	spitest.StoreFactoryConformance(t, spitest.Harness{
		Factory:      factory,
		AdvanceClock: clock.Advance,
		Now:          clock.Now,
	})
}
