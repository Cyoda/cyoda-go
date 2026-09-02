package memory_test

import (
	"errors"
	"fmt"
	"sync"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/plugins/memory"
)

// Concurrent non-transactional compare-and-saves that CREATE the same id —
// each naming the empty expected transaction ID, "expect no entity" — must
// yield exactly one winner. memory holds entityMu across the check and the
// write whether or not the entity exists, so it already answers this way;
// postgres needed an advisory lock to get there (FOR UPDATE locks no absent
// row), and this pins the parity.
func TestNonTxCompareAndSave_ConcurrentCreates_ExactlyOneWinner(t *testing.T) {
	factory := memory.NewStoreFactory()
	defer factory.Close()
	ctx := ctxWithTenant("tenant-cas")
	ref := spi.ModelRef{EntityName: "m-cas-create", ModelVersion: "1"}
	store, err := factory.EntityStore(ctx)
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
