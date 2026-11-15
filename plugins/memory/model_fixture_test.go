package memory_test

import (
	"context"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	spitest "github.com/cyoda-platform/cyoda-go-spi/spitest"
)

// lockModels stores each ref LOCKED, with no unique keys, in ctx's tenant.
// Storage saves an entity only into a model that exists and is LOCKED, so a
// test that saves entities declares their models first.
func lockModels(t testing.TB, f spi.StoreFactory, ctx context.Context, refs ...spi.ModelRef) {
	t.Helper()
	for _, ref := range refs {
		spitest.EnsureLockedModel(t, f, ctx, ref)
	}
}
