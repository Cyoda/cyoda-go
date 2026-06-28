package sqlite_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/plugins/sqlite"
)

var pitBase = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// pitTwoVersions saves v1 at pitBase and v2 300µs later (same millisecond),
// driven by a deterministic TestClock.
func pitTwoVersions(t *testing.T) (*sqlite.StoreFactory, context.Context) {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "pit.db")
	clock := sqlite.NewTestClockAt(pitBase)
	factory, err := sqlite.NewStoreFactoryForTest(context.Background(), dbPath, sqlite.WithClock(clock))
	if err != nil {
		t.Fatalf("create factory: %v", err)
	}
	t.Cleanup(func() { factory.Close() })

	ctx := testCtx("tenant-1")
	store, _ := factory.EntityStore(ctx)
	ref := spi.ModelRef{EntityName: "person", ModelVersion: "1"}

	e := &spi.Entity{
		Meta: spi.EntityMeta{ID: "e-pit", ModelRef: ref, State: "NEW"},
		Data: []byte(`{"v":1}`),
	}
	if _, err := store.Save(ctx, e); err != nil {
		t.Fatalf("Save v1: %v", err)
	}
	clock.Advance(300 * time.Microsecond)
	e.Data = []byte(`{"v":2}`)
	if _, err := store.Save(ctx, e); err != nil {
		t.Fatalf("Save v2: %v", err)
	}
	return factory, ctx
}

func TestSqlitePIT_GetAsAt_InclusiveExactT(t *testing.T) {
	factory, ctx := pitTwoVersions(t)
	store, _ := factory.EntityStore(ctx)

	got, err := store.GetAsAt(ctx, "e-pit", pitBase)
	if err != nil {
		t.Fatalf("GetAsAt(pitBase): %v", err)
	}
	if string(got.Data) != `{"v":1}` {
		t.Errorf("GetAsAt(pitBase) = %s, want {\"v\":1} (inclusive of exactly T, no round-up)", got.Data)
	}
}

func TestSqlitePIT_GetAllAsAt_InclusiveExactT(t *testing.T) {
	factory, ctx := pitTwoVersions(t)
	store, _ := factory.EntityStore(ctx)
	ref := spi.ModelRef{EntityName: "person", ModelVersion: "1"}

	got, err := store.GetAllAsAt(ctx, ref, pitBase)
	if err != nil {
		t.Fatalf("GetAllAsAt(pitBase): %v", err)
	}
	if len(got) != 1 || string(got[0].Data) != `{"v":1}` {
		t.Errorf("GetAllAsAt(pitBase) = %v entities, want 1 with {\"v\":1}", len(got))
	}
}
