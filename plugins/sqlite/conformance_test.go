package sqlite_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/cyoda-platform/cyoda-go-spi/spitest"
	"github.com/cyoda-platform/cyoda-go/plugins/sqlite"
)

func TestConformance(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "conformance.db")

	clock := sqlite.NewTestClock()
	factory, err := sqlite.NewStoreFactoryForTest(context.Background(), dbPath, sqlite.WithClock(clock))
	if err != nil {
		t.Fatalf("create factory: %v", err)
	}

	spitest.StoreFactoryConformance(t, spitest.Harness{
		Factory:      factory,
		AdvanceClock: clock.Advance,
		Now:          clock.Now,
	})
}
