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
		Skip: map[string]string{
			// spitest's Pattern/MalformedLike pins the pre-error Prepare
			// contract ("a leaf whose operand cannot be expanded becomes a
			// leaf that never matches" — Search returns no error, no rows).
			// spi.Prepare now returns an error wrapping ErrUnevaluableLeaf
			// for exactly this case (a pattern operand that will not
			// compile), and Search propagates it rather than degrading to
			// an empty page (correctness-over-availability). This memory
			// plugin's own TestMemorySearch_RejectsUnevaluableFilter pins
			// the new contract directly. The spitest case itself asserts
			// the superseded contract and needs an SPI-side update.
			"Searcher/Pattern/MalformedLike": "asserts the pre-ErrUnevaluableLeaf Prepare contract; superseded, needs an SPI-side fix",
		},
	})
}
