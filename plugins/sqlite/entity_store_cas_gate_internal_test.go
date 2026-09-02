package sqlite

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// A non-transactional CompareAndSave must read the current transaction ID and
// write the new version as one indivisible step. The writer connection
// serialises only the writes, so a check taken outside the commit gate leaves
// a check-then-write window: several callers naming the same expected
// transaction ID can all read it, all pass the check, and all write — each
// silently clobbering the last instead of getting ErrConflict.
func TestNonTxCompareAndSave_ExactlyOneWinner(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "cas-gate.db")
	f, err := NewStoreFactoryForTest(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("NewStoreFactoryForTest: %v", err)
	}
	defer f.Close()

	ctx := attrInternalCtx("tenant-cas", "alice", spi.PrincipalUser)
	ref := spi.ModelRef{EntityName: "m-cas", ModelVersion: "1"}
	store, err := f.EntityStore(ctx)
	if err != nil {
		t.Fatalf("EntityStore: %v", err)
	}

	const seedTxID = "tx-seed"
	if _, err := store.Save(ctx, &spi.Entity{
		Meta: spi.EntityMeta{
			ID: "e-cas", TenantID: "tenant-cas", ModelRef: ref,
			State: "open", TransactionID: seedTxID,
		},
		Data: []byte(`{"n":0}`),
	}); err != nil {
		t.Fatalf("seed Save: %v", err)
	}

	const racers = 8
	var wg sync.WaitGroup
	results := make([]error, racers)
	start := make(chan struct{})
	for i := range racers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, results[i] = store.CompareAndSave(ctx, &spi.Entity{
				Meta: spi.EntityMeta{
					ID: "e-cas", TenantID: "tenant-cas", ModelRef: ref,
					State: "open", TransactionID: "tx-writer",
				},
				Data: []byte(`{"n":1}`),
			}, seedTxID)
		}()
	}
	close(start)
	wg.Wait()

	var wins, conflicts int
	for i, err := range results {
		switch {
		case err == nil:
			wins++
		case errors.Is(err, spi.ErrConflict):
			conflicts++
		default:
			t.Fatalf("racer %d: unexpected error: %v", i, err)
		}
	}
	if wins != 1 || conflicts != racers-1 {
		t.Fatalf("got %d winners and %d conflicts, want exactly 1 winner and %d conflicts", wins, conflicts, racers-1)
	}
}

// The same indivisibility, for the CREATE case: several callers naming the
// empty expected transaction ID — "expect no entity" — must not all succeed.
// The commit gate already covers it here, because it is held across the check
// and the write whether or not a row exists. Postgres needed an advisory lock
// to get the same answer (FOR UPDATE locks no absent row); this pins the
// parity.
func TestNonTxCompareAndSave_ConcurrentCreates_ExactlyOneWinner(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "cas-create.db")
	f, err := NewStoreFactoryForTest(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("NewStoreFactoryForTest: %v", err)
	}
	defer f.Close()

	ctx := attrInternalCtx("tenant-cas", "alice", spi.PrincipalUser)
	ref := spi.ModelRef{EntityName: "m-cas-create", ModelVersion: "1"}
	store, err := f.EntityStore(ctx)
	if err != nil {
		t.Fatalf("EntityStore: %v", err)
	}

	const racers = 8
	var wg sync.WaitGroup
	results := make([]error, racers)
	start := make(chan struct{})
	for i := range racers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, results[i] = store.CompareAndSave(ctx, &spi.Entity{
				Meta: spi.EntityMeta{
					ID: "e-cas-create", TenantID: "tenant-cas", ModelRef: ref,
					State: "open", TransactionID: fmt.Sprintf("tx-writer-%d", i),
				},
				Data: []byte(`{"n":1}`),
			}, "")
		}()
	}
	close(start)
	wg.Wait()

	var wins, conflicts int
	for i, err := range results {
		switch {
		case err == nil:
			wins++
		case errors.Is(err, spi.ErrConflict):
			conflicts++
		default:
			t.Fatalf("racer %d: unexpected error: %v", i, err)
		}
	}
	if wins != 1 || conflicts != racers-1 {
		t.Fatalf("got %d winners and %d conflicts, want exactly 1 winner and %d conflicts", wins, conflicts, racers-1)
	}
}
