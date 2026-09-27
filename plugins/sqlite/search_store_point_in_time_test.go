package sqlite_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/plugins/sqlite"
)

// CreateJob always persists PointInTime, the zero instant included: the
// column is NOT NULL, and a zero PointInTime is a value, not an absence. This
// is how PostgreSQL has always stored it.
func TestSQLiteSearchStore_CreateJob_PersistsEveryPointInTime(t *testing.T) {
	factory, err := sqlite.NewStoreFactoryForTest(context.Background(), filepath.Join(t.TempDir(), "pit.db"))
	if err != nil {
		t.Fatalf("NewStoreFactoryForTest: %v", err)
	}
	t.Cleanup(func() { factory.Close() })
	ctx := testCtx("tenant-pit")
	store, err := factory.AsyncSearchStore(ctx)
	if err != nil {
		t.Fatalf("AsyncSearchStore: %v", err)
	}

	asAt := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		id  string
		pit time.Time
	}{
		{id: "job-zero", pit: time.Time{}},
		{id: "job-set", pit: asAt},
	} {
		job := &spi.SearchJob{
			ID:          tc.id,
			Status:      "RUNNING",
			ModelRef:    spi.ModelRef{EntityName: "item", ModelVersion: "1"},
			PointInTime: tc.pit,
			CreateTime:  asAt,
		}
		if err := store.CreateJob(ctx, job); err != nil {
			t.Fatalf("CreateJob(%s): %v", tc.id, err)
		}
		got, err := store.GetJob(ctx, tc.id)
		if err != nil {
			t.Fatalf("GetJob(%s): %v", tc.id, err)
		}
		if !got.PointInTime.Equal(tc.pit) {
			t.Errorf("GetJob(%s).PointInTime = %v, want %v", tc.id, got.PointInTime, tc.pit)
		}
	}
}
