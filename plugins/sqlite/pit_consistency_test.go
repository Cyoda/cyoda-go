package sqlite_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/plugins/sqlite"
)

// At a same-millisecond boundary, the version selected by Search (raw <=)
// must be the same version GetAsAt re-fetches. Pre-fix, Search matched v1
// (city=Berlin) at pitBase while rounded GetAsAt resolved v2 (city=Munich).
func TestSqlitePIT_SearchSelectMatchesGetAsAtRefetch(t *testing.T) {
	dir := t.TempDir()
	clock := sqlite.NewTestClockAt(pitBase) // pitBase from pit_boundary_test.go
	factory, err := sqlite.NewStoreFactoryForTest(
		context.Background(), filepath.Join(dir, "c.db"), sqlite.WithClock(clock))
	if err != nil {
		t.Fatalf("factory: %v", err)
	}
	t.Cleanup(func() { factory.Close() })

	ctx := testCtx("tenant-1")
	store, _ := factory.EntityStore(ctx)
	ref := spi.ModelRef{EntityName: "person", ModelVersion: "1"}

	e := &spi.Entity{
		Meta: spi.EntityMeta{ID: "e-c", ModelRef: ref, State: "NEW"},
		Data: []byte(`{"city":"Berlin"}`),
	}
	if _, err := store.Save(ctx, e); err != nil { // v1 @ pitBase
		t.Fatalf("save v1: %v", err)
	}
	clock.Advance(300 * time.Microsecond)
	e.Data = []byte(`{"city":"Munich"}`)
	if _, err := store.Save(ctx, e); err != nil { // v2 @ pitBase+300µs
		t.Fatalf("save v2: %v", err)
	}

	pit := pitBase
	searcher := store.(spi.Searcher)
	results, err := searcher.Search(ctx,
		spi.Filter{Op: spi.FilterEq, Path: "city", Source: spi.SourceData, Value: "Berlin"},
		spi.SearchOptions{ModelName: "person", ModelVersion: "1", PointInTime: &pit})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("Search(city=Berlin, as-at pitBase) returned %d, want 1", len(results))
	}

	// Re-fetch the selected entity at the same instant: must agree (Berlin).
	got, err := store.GetAsAt(ctx, "e-c", pit)
	if err != nil {
		t.Fatalf("GetAsAt re-fetch: %v", err)
	}
	if string(got.Data) != `{"city":"Berlin"}` {
		t.Errorf("re-fetch = %s, want {\"city\":\"Berlin\"} (select/re-fetch disagree at boundary)", got.Data)
	}
}
