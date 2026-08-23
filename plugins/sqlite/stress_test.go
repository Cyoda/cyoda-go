package sqlite_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/plugins/sqlite"
)

// TestConcurrencyStress exercises concurrent transactional writes against
// the SQLite plugin with both conflicting (same entity) and non-conflicting
// (unique entity per goroutine) access patterns.
//
// 20 goroutines x 50 iterations each. Half target a shared entity (expected
// conflicts), half target unique entities (no conflicts expected).
//
// After all goroutines complete:
//   - Every successful commit's data must be present
//   - Entity versions must be consistent (no gaps, no duplicates)
//   - Run with -race to check for data races
//
// Guarded by testing.Short so `go test -short` skips this.
func TestConcurrencyStress(t *testing.T) {
	if testing.Short() {
		t.Skip("stress test")
	}

	const (
		numGoroutines = 20
		iterations    = 50
		sharedEntity  = "shared-entity"
	)

	dir := t.TempDir()
	dbPath := filepath.Join(dir, "stress.db")

	clock := sqlite.NewTestClock()
	factory, err := sqlite.NewStoreFactoryForTest(context.Background(), dbPath, sqlite.WithClock(clock))
	if err != nil {
		t.Fatalf("create factory: %v", err)
	}
	defer factory.Close()

	ctx := testCtx("tenant-stress")
	ref := spi.ModelRef{EntityName: "counter", ModelVersion: "1"}

	// Seed the shared entity so update transactions find it.
	store, err := factory.EntityStore(ctx)
	if err != nil {
		t.Fatalf("EntityStore: %v", err)
	}
	_, err = store.Save(ctx, &spi.Entity{
		Meta: spi.EntityMeta{ID: sharedEntity, ModelRef: ref},
		Data: []byte(`{"count":0}`),
	})
	if err != nil {
		t.Fatalf("seed shared entity: %v", err)
	}

	// Advance clock past the seed write's snapshot.
	clock.Advance(10 * time.Millisecond)

	var (
		wg             sync.WaitGroup
		sharedCommits  atomic.Int64
		sharedConflict atomic.Int64
		uniqueCommits  atomic.Int64
		uniqueErrors   atomic.Int64

		// Track successful unique entity IDs for verification.
		committedUniqueMu sync.Mutex
		committedUnique   []string
	)

	for g := 0; g < numGoroutines; g++ {
		wg.Add(1)
		go func(goroutineID int) {
			defer wg.Done()

			isConflicting := goroutineID < numGoroutines/2

			for i := 0; i < iterations; i++ {
				// Advance clock slightly so each iteration gets a unique snapshot.
				clock.Advance(1 * time.Millisecond)

				tm, tmErr := factory.TransactionManager(ctx)
				if tmErr != nil {
					t.Errorf("goroutine %d: TransactionManager: %v", goroutineID, tmErr)
					return
				}

				txID, txCtx, beginErr := tm.Begin(ctx)
				if beginErr != nil {
					t.Errorf("goroutine %d: Begin: %v", goroutineID, beginErr)
					return
				}

				txStore, storeErr := factory.EntityStore(txCtx)
				if storeErr != nil {
					_ = tm.Rollback(ctx, txID)
					t.Errorf("goroutine %d: EntityStore: %v", goroutineID, storeErr)
					return
				}

				var entityID string
				if isConflicting {
					entityID = sharedEntity
				} else {
					entityID = fmt.Sprintf("unique-%d-%d", goroutineID, i)
				}

				// For conflicting: read then update. For unique: create.
				if isConflicting {
					_, readErr := txStore.Get(txCtx, entityID)
					if readErr != nil {
						_ = tm.Rollback(ctx, txID)
						continue
					}
				}

				data := fmt.Sprintf(`{"goroutine":%d,"iteration":%d}`, goroutineID, i)
				_, saveErr := txStore.Save(txCtx, &spi.Entity{
					Meta: spi.EntityMeta{ID: entityID, ModelRef: ref},
					Data: []byte(data),
				})
				if saveErr != nil {
					_ = tm.Rollback(ctx, txID)
					continue
				}

				// Advance clock before commit so submit time advances.
				clock.Advance(1 * time.Millisecond)

				commitErr := tm.Commit(ctx, txID)
				if commitErr != nil {
					if errors.Is(commitErr, spi.ErrConflict) {
						if isConflicting {
							sharedConflict.Add(1)
						}
					} else {
						if !isConflicting {
							uniqueErrors.Add(1)
						}
					}
					continue
				}

				if isConflicting {
					sharedCommits.Add(1)
				} else {
					uniqueCommits.Add(1)
					func() {
						committedUniqueMu.Lock()
						defer committedUniqueMu.Unlock()
						committedUnique = append(committedUnique, entityID)
					}()
				}
			}
		}(g)
	}

	wg.Wait()

	t.Logf("shared: %d commits, %d conflicts", sharedCommits.Load(), sharedConflict.Load())
	t.Logf("unique: %d commits, %d errors", uniqueCommits.Load(), uniqueErrors.Load())

	// Assertion 1: at least some shared commits succeeded.
	if sharedCommits.Load() == 0 {
		t.Error("expected at least one successful shared entity commit")
	}

	// Assertion 2: some conflicts should have occurred on the shared entity.
	if sharedConflict.Load() == 0 {
		t.Error("expected at least one conflict on the shared entity")
	}

	// Assertion 3: no errors on unique entities (they never conflict).
	if uniqueErrors.Load() != 0 {
		t.Errorf("expected 0 unique entity errors, got %d", uniqueErrors.Load())
	}

	// Assertion 4: all committed unique entities are readable.
	store2, err := factory.EntityStore(ctx)
	if err != nil {
		t.Fatalf("EntityStore for verification: %v", err)
	}

	for _, id := range committedUnique {
		e, getErr := store2.Get(ctx, id)
		if getErr != nil {
			t.Errorf("committed unique entity %s not found: %v", id, getErr)
			continue
		}
		// Verify the data is valid JSON.
		var parsed map[string]any
		if jsonErr := json.Unmarshal(e.Data, &parsed); jsonErr != nil {
			t.Errorf("entity %s has invalid JSON: %v", id, jsonErr)
		}
	}

	// Assertion 5: shared entity has consistent version history (no gaps).
	metas, err := store2.GetVersionMetadata(ctx, sharedEntity, spi.VersionMetadataOptions{})
	if err != nil {
		t.Fatalf("GetVersionMetadata for shared entity: %v", err)
	}

	if len(metas) == 0 {
		t.Fatal("shared entity has no version history")
	}

	// Extract versions and sort them.
	versions := make([]int64, len(metas))
	for i, m := range metas {
		versions[i] = m.Version
	}
	sort.Slice(versions, func(i, j int) bool { return versions[i] < versions[j] })

	// Verify: starts at 1, no gaps, no duplicates.
	if versions[0] != 1 {
		t.Errorf("version history should start at 1, starts at %d", versions[0])
	}
	for i := 1; i < len(versions); i++ {
		if versions[i] != versions[i-1]+1 {
			t.Errorf("version gap: v%d -> v%d", versions[i-1], versions[i])
		}
	}

	// The number of version entries (excluding the initial seed) should match
	// sharedCommits + 1 (the initial non-transactional save).
	expectedVersions := sharedCommits.Load() + 1 // +1 for the seed write
	if int64(len(versions)) != expectedVersions {
		t.Errorf("expected %d versions for shared entity, got %d", expectedVersions, len(versions))
	}

	t.Logf("shared entity final version: %d, total versions: %d", versions[len(versions)-1], len(versions))
	t.Logf("unique entities committed: %d/%d", uniqueCommits.Load(), int64(numGoroutines/2)*int64(iterations))
}
