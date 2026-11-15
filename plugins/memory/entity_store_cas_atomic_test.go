package memory_test

import (
	"errors"
	"sync"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/plugins/memory"
)

// A non-transactional CompareAndSave must read the current transaction ID and
// write the new version as one indivisible step: several callers naming the
// same expected transaction ID must not all pass the check and all write, each
// silently clobbering the last instead of getting ErrConflict.
//
// The create form of this race is gone with the create path — compare-and-save
// rejects an empty expected ID now, so nothing races to bring an absent row
// into existence through it. The update form is what remains, and it is the
// form postgres and sqlite pin too
// (entity_store_cas_atomic_test.go, entity_store_cas_gate_internal_test.go):
// memory holds entityMu across the check and the write, so it answers this
// way, and the assertion is what keeps that true.
func TestNonTxCompareAndSave_ExactlyOneWinner(t *testing.T) {
	factory := memory.NewStoreFactory()
	lockModels(t, factory, ctxWithTenant("tenant-cas"), spi.ModelRef{EntityName: "m-cas", ModelVersion: "1"})
	defer factory.Close()
	ctx := ctxWithTenant("tenant-cas")
	ref := spi.ModelRef{EntityName: "m-cas", ModelVersion: "1"}
	store, err := factory.EntityStore(ctx)
	if err != nil {
		t.Fatalf("EntityStore: %v", err)
	}

	tm := factory.NewTransactionManager(newTestUUIDGenerator())
	seedTxID := saveCommitted(t, tm, store, ctx, &spi.Entity{
		Meta: spi.EntityMeta{
			ID: "e-cas", TenantID: "tenant-cas", ModelRef: ref,
			State: "open",
		},
		Data: []byte(`{"n":0}`),
	})

	const racers = 8
	var wg sync.WaitGroup
	results := make([]error, racers)
	start := make(chan struct{})
	for i := range racers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			// The winner's write is its own transaction and stores the empty
			// transaction ID, which no later check against the seed's matches.
			_, results[i] = store.CompareAndSave(ctx, &spi.Entity{
				Meta: spi.EntityMeta{
					ID: "e-cas", TenantID: "tenant-cas", ModelRef: ref,
					State: "open",
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
