package memory_test

import (
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	spitest "github.com/cyoda-platform/cyoda-go-spi/spitest"

	"github.com/cyoda-platform/cyoda-go/plugins/memory"
)

// TestConformance runs the SPI conformance harness against the memory plugin
// with a deterministic TestClock for temporal subtests.
func TestConformance(t *testing.T) {
	clock := memory.NewTestClock()
	factory := memory.NewStoreFactory(memory.WithClock(clock))
	t.Cleanup(func() { _ = factory.Close() })
	spitest.StoreFactoryConformance(t, spitest.Harness{
		Factory:      factory,
		AdvanceClock: clock.Advance,
		Now:          clock.Now,
		// The business clock is c; the physical clock is the harness's own
		// TestClock, so AdvanceClock ages liveness stamps without touching c.
		ClockFactory: func(c spi.Clock) (spi.StoreFactory, func()) {
			f := memory.NewStoreFactory(memory.WithClock(clock), memory.WithBusinessClock(c))
			return f, func() { _ = f.Close() }
		},
	})
}
