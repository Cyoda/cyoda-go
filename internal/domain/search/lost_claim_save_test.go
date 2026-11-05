package search_test

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go-spi/predicate"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/domain/search"
	"github.com/cyoda-platform/cyoda-go/internal/platformclock"
	"github.com/cyoda-platform/cyoda-go/plugins/memory"
)

// A cross-node cancel or a takeover is usually first seen by the scan's
// result writes, before any heartbeat tick. The refused write is an ordinary
// end on a cluster: the executor records nothing and logs no ERROR.
func TestAsyncSearch_LostClaimSeenBySaveResultsLogsNoError(t *testing.T) {
	for name, settle := range map[string]func(t *testing.T, store spi.AsyncSearchStore, ctx context.Context, jobID string){
		"cancelled by another node": func(t *testing.T, store spi.AsyncSearchStore, ctx context.Context, jobID string) {
			if err := store.Cancel(ctx, jobID, time.Now()); err != nil {
				t.Fatalf("Cancel: %v", err)
			}
		},
		"claimed by another executor": func(t *testing.T, store spi.AsyncSearchStore, _ context.Context, jobID string) {
			if _, err := store.ClaimStale(context.Background(), 0, 10); err != nil {
				t.Fatalf("ClaimStale: %v", err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			logs := &syncBuffer{}
			prev := slog.Default()
			slog.SetDefault(slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
			t.Cleanup(func() { slog.SetDefault(prev) })

			base := memory.NewStoreFactory()
			defer base.Close()
			ctx := tenantCtx("tenant-lost-claim")
			ref := spi.ModelRef{EntityName: "lostclaim", ModelVersion: "1"}
			saveMinimalModel(t, ctx, base, ref)
			for i := 0; i < 400; i++ {
				saveEntity(t, ctx, base, ref, fmt.Sprintf("e%03d", i), []byte(`{}`))
			}
			ies := wrapIterate(t, base, ctx, func(it spi.Iterator) spi.Iterator {
				return &delayIterator{Iterator: it, delay: 2 * time.Millisecond}
			})
			factory := &iterableFactory{StoreFactory: base, entityStore: ies}
			store, _ := base.AsyncSearchStore(context.Background())
			// A heartbeat that never ticks during the test: the result writes
			// are the first to meet the settled record.
			svc := search.NewSearchService(factory, common.NewTestUUIDGenerator(), store, newTestConsistency(t, factory), platformclock.Real()).
				WithAsyncPool(newTinyPool(t)).
				WithHeartbeat(time.Hour)

			jobID, err := svc.SubmitAsync(ctx, ref, &predicate.LifecycleCondition{Field: "state", OperatorType: "EQUALS", Value: "NEW"}, search.SearchOptions{Limit: 1000})
			if err != nil {
				t.Fatalf("SubmitAsync: %v", err)
			}
			time.Sleep(100 * time.Millisecond)
			settle(t, store, ctx, jobID)
			time.Sleep(1500 * time.Millisecond)

			if strings.Contains(logs.String(), "level=ERROR") {
				t.Fatalf("a lost claim logged an ERROR:\n%s", logs.String())
			}
		})
	}
}
