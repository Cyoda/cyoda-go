package postgres_test

import (
	"errors"
	"sync"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// A non-transactional CompareAndSave must read the current transaction ID and
// write the new version as one indivisible step. Outside a transaction every
// statement the store issues is separately auto-committed, so a check taken
// on its own leaves a check-then-write window: several callers naming the same
// expected transaction ID can all read it, all pass the check, and all write —
// each silently clobbering the last instead of getting ErrConflict.
func TestNonTxCompareAndSave_ExactlyOneWinner(t *testing.T) {
	factory := setupEntityTest(t)
	ctx := ctxWithTenant("tenant-cas")
	ref := spi.ModelRef{EntityName: "m-cas", ModelVersion: "1"}

	store, err := factory.EntityStore(ctx)
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
