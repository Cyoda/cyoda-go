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

const lostClaimLine = "async search job is no longer this executor's to record"

// A job that stops being this executor's while it runs — cancelled on
// another node, claimed by another executor, or removed — ends the executor
// with exactly one log line, whichever path sees it first (the heartbeat, a
// result write, the terminal write), and no ERROR: an ordinary end on a
// cluster. A claim taken from a live executor is logged at WARN (the claimant
// judged its heartbeats stale); a cancel or a removal at DEBUG.
func TestAsyncSearch_LostClaimEndsWithOneLine(t *testing.T) {
	cancelOnOtherNode := func(t *testing.T, store spi.AsyncSearchStore, ctx context.Context, jobID string) {
		if err := store.Cancel(ctx, jobID, time.Now()); err != nil {
			t.Fatalf("Cancel: %v", err)
		}
	}
	claimElsewhere := func(t *testing.T, store spi.AsyncSearchStore, _ context.Context, _ string) {
		if _, err := store.ClaimStale(context.Background(), 0, 10); err != nil {
			t.Fatalf("ClaimStale: %v", err)
		}
	}
	remove := func(t *testing.T, store spi.AsyncSearchStore, ctx context.Context, jobID string) {
		cancelOnOtherNode(t, store, ctx, jobID)
		if _, err := store.ReapExpired(context.Background(), 0); err != nil {
			t.Fatalf("ReapExpired: %v", err)
		}
	}
	for _, c := range []struct {
		name      string
		heartbeat time.Duration
		settle    func(*testing.T, spi.AsyncSearchStore, context.Context, string)
		wantLevel string
	}{
		{"cancelled, seen by a result write", time.Hour, cancelOnOtherNode, "DEBUG"},
		{"claimed elsewhere, seen by a result write", time.Hour, claimElsewhere, "WARN"},
		{"removed, seen by a result write", time.Hour, remove, "DEBUG"},
		{"cancelled, seen by the heartbeat", 10 * time.Millisecond, cancelOnOtherNode, "DEBUG"},
		{"claimed elsewhere, seen by the heartbeat", 10 * time.Millisecond, claimElsewhere, "WARN"},
	} {
		t.Run(c.name, func(t *testing.T) {
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
			svc := search.NewSearchService(factory, common.NewTestUUIDGenerator(), store, newTestConsistency(t, factory), platformclock.Real()).
				WithAsyncPool(newTinyPool(t)).
				WithHeartbeat(c.heartbeat)

			jobID, err := svc.SubmitAsync(ctx, ref, &predicate.LifecycleCondition{Field: "state", OperatorType: "EQUALS", Value: "NEW"}, search.SearchOptions{Limit: 1000})
			if err != nil {
				t.Fatalf("SubmitAsync: %v", err)
			}
			time.Sleep(100 * time.Millisecond)
			c.settle(t, store, ctx, jobID)

			deadline := time.Now().Add(5 * time.Second)
			for !strings.Contains(logs.String(), lostClaimLine) {
				if time.Now().After(deadline) {
					t.Fatalf("the executor never reported the lost claim:\n%s", logs.String())
				}
				time.Sleep(10 * time.Millisecond)
			}
			time.Sleep(100 * time.Millisecond) // let anything that would follow it land

			out := logs.String()
			if n := strings.Count(out, lostClaimLine); n != 1 {
				t.Fatalf("the lost claim was reported %d times, want once:\n%s", n, out)
			}
			for _, line := range strings.Split(out, "\n") {
				if strings.Contains(line, lostClaimLine) && !strings.Contains(line, "level="+c.wantLevel) {
					t.Fatalf("lost claim logged as %q, want level %s", line, c.wantLevel)
				}
			}
			if strings.Contains(out, "level=ERROR") {
				t.Fatalf("a lost claim logged an ERROR:\n%s", out)
			}
			if c.wantLevel == "DEBUG" && strings.Contains(out, "level=WARN") {
				t.Fatalf("an ordinary end logged a WARN:\n%s", out)
			}
		})
	}
}
