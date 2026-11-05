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
// with exactly one log line for the job, at INFO, whichever path sees it
// first (the heartbeat, a result write, the terminal write): an ordinary end
// on a cluster, reported the same way however it is met.
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
	}{
		{"cancelled, seen by a result write", time.Hour, cancelOnOtherNode},
		{"claimed elsewhere, seen by a result write", time.Hour, claimElsewhere},
		{"removed, seen by a result write", time.Hour, remove},
		{"cancelled, seen by the heartbeat", 10 * time.Millisecond, cancelOnOtherNode},
		{"claimed elsewhere, seen by the heartbeat", 10 * time.Millisecond, claimElsewhere},
		{"removed, seen by the heartbeat", 10 * time.Millisecond, remove},
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
			settledAt := len(logs.String())
			c.settle(t, store, ctx, jobID)

			deadline := time.Now().Add(5 * time.Second)
			for !strings.Contains(logs.String(), lostClaimLine) {
				if time.Now().After(deadline) {
					t.Fatalf("the executor never reported the lost claim:\n%s", logs.String())
				}
				time.Sleep(10 * time.Millisecond)
			}
			time.Sleep(100 * time.Millisecond) // let anything that would follow it land

			// Every line about this job after the event, whatever its text.
			var jobLines []string
			for _, line := range strings.Split(logs.String()[settledAt:], "\n") {
				if strings.Contains(line, "jobID="+jobID) {
					jobLines = append(jobLines, line)
				}
			}
			if len(jobLines) != 1 {
				t.Fatalf("the lost claim produced %d lines for the job, want one:\n%s", len(jobLines), strings.Join(jobLines, "\n"))
			}
			if !strings.Contains(jobLines[0], lostClaimLine) || !strings.Contains(jobLines[0], "level=INFO") {
				t.Fatalf("lost claim logged as %q, want the lost-claim line at INFO", jobLines[0])
			}
		})
	}
}
