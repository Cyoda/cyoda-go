package search_test

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
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
	// The memory store reaps only a terminal job, so a removal cancels
	// first; the executor may meet either refusal, which is why every lost
	// claim is reported alike.
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

// blockingModelFactory hands out a model store whose reads, once armed, wait
// for their context to end and then answer its error, as a store's read does
// when the job it serves is cancelled mid-read.
type blockingModelFactory struct {
	spi.StoreFactory
	armed   atomic.Bool
	entered chan struct{}
}

func (f *blockingModelFactory) ModelStore(ctx context.Context) (spi.ModelStore, error) {
	ms, err := f.StoreFactory.ModelStore(ctx)
	if err != nil {
		return nil, err
	}
	return &blockingModelStore{ModelStore: ms, f: f}, nil
}

type blockingModelStore struct {
	spi.ModelStore
	f *blockingModelFactory
}

func (m *blockingModelStore) Get(ctx context.Context, ref spi.ModelRef) (*spi.ModelDescriptor, error) {
	if !m.f.armed.Load() {
		return m.ModelStore.Get(ctx, ref)
	}
	select {
	case m.f.entered <- struct{}{}:
	default:
	}
	<-ctx.Done()
	return nil, ctx.Err()
}

// A cancel that lands while the job is still loading what it needs before
// the scan — not only during the scan — ends it as a lost claim: one INFO
// line, no ERROR, no ticket.
func TestAsyncSearch_CancelDuringEarlyStepsEndsAsALostClaim(t *testing.T) {
	base := memory.NewStoreFactory()
	defer base.Close()
	ctx := tenantCtx("tenant-early-cancel")
	ref := spi.ModelRef{EntityName: "earlycancel", ModelVersion: "1"}
	saveMinimalModel(t, ctx, base, ref)
	store, _ := base.AsyncSearchStore(context.Background())
	factory := &blockingModelFactory{StoreFactory: base, entered: make(chan struct{}, 1)}
	pool := search.NewWorkerPool(1, 4)
	t.Cleanup(func() { pool.Drain(context.Background()) })
	hold := make(chan struct{})
	if err := pool.Submit(func() { <-hold }); err != nil {
		t.Fatalf("Submit the holder: %v", err)
	}
	svc := search.NewSearchService(factory, common.NewTestUUIDGenerator(), store, newTestConsistency(t, base), platformclock.Real()).
		WithAsyncPool(pool).
		WithHeartbeat(time.Hour)

	// Submit while the read is unarmed (submit-time validation reads the
	// model too), then arm it and let the job run into it.
	jobID, err := svc.SubmitAsync(ctx, ref, &predicate.LifecycleCondition{Field: "state", OperatorType: "EQUALS", Value: "NEW"}, search.SearchOptions{Limit: 10})
	if err != nil {
		t.Fatalf("SubmitAsync: %v", err)
	}
	factory.armed.Store(true)
	close(hold)
	select {
	case <-factory.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the job never reached its model read")
	}

	logs := captureJobLogs(t)
	if _, err := svc.CancelAsync(ctx, jobID); err != nil {
		t.Fatalf("CancelAsync: %v", err)
	}
	pool.Drain(context.Background()) // the executor has ended
	assertOneLostClaimLine(t, logs.String(), jobID)
}

// unreadableJobStore fails every job read once armed, as a store does when its
// main pool is exhausted.
type unreadableJobStore struct {
	spi.AsyncSearchStore
	armed atomic.Bool
}

func (s *unreadableJobStore) GetJob(ctx context.Context, jobID string) (*spi.SearchJob, error) {
	if s.armed.Load() {
		return nil, fmt.Errorf("get job %s: pool exhausted", jobID)
	}
	return s.AsyncSearchStore.GetJob(ctx, jobID)
}

// A lost claim is decided from the store's own refusal, not from a read of the
// job afterwards: a cancel seen by the heartbeat while job reads fail is still
// one INFO lost-claim line, with no ERROR and no ticket.
func TestAsyncSearch_LostClaimNeedsNoJobRead(t *testing.T) {
	base := memory.NewStoreFactory()
	defer base.Close()
	ctx := tenantCtx("tenant-lost-claim-noread")
	ref := spi.ModelRef{EntityName: "lostclaimnoread", ModelVersion: "1"}
	saveMinimalModel(t, ctx, base, ref)
	for i := 0; i < 400; i++ {
		saveEntity(t, ctx, base, ref, fmt.Sprintf("e%03d", i), []byte(`{}`))
	}
	ies := wrapIterate(t, base, ctx, func(it spi.Iterator) spi.Iterator {
		return &delayIterator{Iterator: it, delay: 2 * time.Millisecond}
	})
	factory := &iterableFactory{StoreFactory: base, entityStore: ies}
	inner, _ := base.AsyncSearchStore(context.Background())
	store := &unreadableJobStore{AsyncSearchStore: inner}
	pool := search.NewWorkerPool(1, 4)
	svc := search.NewSearchService(factory, common.NewTestUUIDGenerator(), store, newTestConsistency(t, factory), platformclock.Real()).
		WithAsyncPool(pool).
		WithHeartbeat(10 * time.Millisecond)

	jobID, err := svc.SubmitAsync(ctx, ref, &predicate.LifecycleCondition{Field: "state", OperatorType: "EQUALS", Value: "NEW"}, search.SearchOptions{Limit: 1000})
	if err != nil {
		t.Fatalf("SubmitAsync: %v", err)
	}
	time.Sleep(100 * time.Millisecond)
	logs := captureJobLogs(t)
	store.armed.Store(true)
	if err := inner.Cancel(ctx, jobID, time.Now()); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	pool.Drain(context.Background()) // the executor has ended
	assertOneLostClaimLine(t, logs.String(), jobID)
}

// gateIterator blocks every Next until gate is closed, ignoring the job's
// context, as a store read that does not watch it does.
type gateIterator struct {
	spi.Iterator
	gate    chan struct{}
	entered chan struct{}
}

func (g *gateIterator) Next() bool {
	select {
	case g.entered <- struct{}{}:
	default:
	}
	<-g.gate
	return g.Iterator.Next()
}

// refusedHeartbeatSignal closes refused at the first Heartbeat the store
// refuses.
type refusedHeartbeatSignal struct {
	spi.AsyncSearchStore
	once    sync.Once
	refused chan struct{}
}

func (s *refusedHeartbeatSignal) Heartbeat(ctx context.Context, jobID string, epoch int64) error {
	err := s.AsyncSearchStore.Heartbeat(ctx, jobID, epoch)
	if err != nil {
		s.once.Do(func() { close(s.refused) })
	}
	return err
}

// A job settled elsewhere while this node still runs it is a lost claim when
// the node shuts down, too: the shutdown release reports it with the one INFO
// lost-claim line — and when the executor has already seen the refusal, the
// release adds nothing to its report.
func TestReleaseRegisteredJobs_LostClaimIsOneLine(t *testing.T) {
	for _, tc := range []struct {
		name      string
		heartbeat time.Duration
	}{
		{"executor has not seen it", time.Hour},
		{"heartbeat already saw it", 10 * time.Millisecond},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base := memory.NewStoreFactory()
			defer base.Close()
			ctx := tenantCtx("tenant-release-lost")
			ref := spi.ModelRef{EntityName: "releaselost", ModelVersion: "1"}
			saveMinimalModel(t, ctx, base, ref)
			for i := 0; i < 5; i++ {
				saveEntity(t, ctx, base, ref, fmt.Sprintf("e%03d", i), []byte(`{}`))
			}
			gate := make(chan struct{})
			entered := make(chan struct{}, 1)
			ies := wrapIterate(t, base, ctx, func(it spi.Iterator) spi.Iterator {
				return &gateIterator{Iterator: it, gate: gate, entered: entered}
			})
			factory := &iterableFactory{StoreFactory: base, entityStore: ies}
			inner, _ := base.AsyncSearchStore(context.Background())
			store := &refusedHeartbeatSignal{AsyncSearchStore: inner, refused: make(chan struct{})}
			pool := search.NewWorkerPool(1, 4)
			svc := search.NewSearchService(factory, common.NewTestUUIDGenerator(), store, newTestConsistency(t, factory), platformclock.Real()).
				WithAsyncPool(pool).
				WithHeartbeat(tc.heartbeat)

			jobID, err := svc.SubmitAsync(ctx, ref, &predicate.LifecycleCondition{Field: "state", OperatorType: "EQUALS", Value: "NEW"}, search.SearchOptions{Limit: 10})
			if err != nil {
				t.Fatalf("SubmitAsync: %v", err)
			}
			select {
			case <-entered:
			case <-time.After(5 * time.Second):
				t.Fatal("the job never reached its scan")
			}
			logs := captureJobLogs(t)
			if err := store.Cancel(ctx, jobID, time.Now()); err != nil {
				t.Fatalf("Cancel: %v", err)
			}
			if tc.heartbeat < time.Hour {
				select {
				case <-store.refused:
				case <-time.After(5 * time.Second):
					t.Fatal("the heartbeat was never refused")
				}
			}
			if n := svc.ReleaseRegisteredJobs(context.Background()); n != 0 {
				t.Errorf("ReleaseRegisteredJobs = %d, want 0: nothing was released", n)
			}
			close(gate)
			pool.Drain(context.Background())
			assertOneLostClaimLine(t, logs.String(), jobID)
		})
	}
}
