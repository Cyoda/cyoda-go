package sqlite_test

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
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
		// The business clock is c; the physical clock is the harness's own
		// TestClock, so AdvanceClock ages liveness stamps without touching c.
		ClockFactory: func(c spi.Clock) (spi.StoreFactory, func()) {
			p := filepath.Join(t.TempDir(), "clock.db")
			f, err := sqlite.NewStoreFactoryForTest(context.Background(), p,
				sqlite.WithClock(clock), sqlite.WithBusinessClock(c))
			if err != nil {
				// Called from a subtest's goroutine: panic, as the postgres harness does.
				panic(fmt.Sprintf("Harness.ClockFactory: %v", err))
			}
			return f, func() { _ = f.Close() }
		},
	})
}
