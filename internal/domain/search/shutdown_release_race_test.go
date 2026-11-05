package search_test

// What the shutdown release reports for a job whose executor decides its
// outcome while the release loop is still busy with other jobs.

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go-spi/predicate"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/domain/search"
	"github.com/cyoda-platform/cyoda-go/internal/platformclock"
	"github.com/cyoda-platform/cyoda-go/plugins/memory"
)

// firstReleaseBlocks holds the first Release until hold is closed and tells
// which job it was for.
type firstReleaseBlocks struct {
	spi.AsyncSearchStore
	once     sync.Once
	first    chan string
	hold     chan struct{}
	mu       sync.Mutex
	released map[string]error
}

func (s *firstReleaseBlocks) Release(ctx context.Context, jobID string, epoch int64) error {
	isFirst := false
	s.once.Do(func() { isFirst = true })
	if isFirst {
		s.first <- jobID
		<-s.hold
	}
	err := s.AsyncSearchStore.Release(ctx, jobID, epoch)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.released[jobID] = err
	return err
}

// perJobGateIterator blocks each job's scan on its own gate. The job is told
// apart by the entity store's iteration order: the first scan gets gates[0].
type gatedScans struct {
	mu    sync.Mutex
	gates []chan struct{}
	next  int
}

func (g *gatedScans) take() chan struct{} {
	g.mu.Lock()
	defer g.mu.Unlock()
	c := g.gates[g.next]
	g.next++
	return c
}

type onceGateIterator struct {
	spi.Iterator
	gate   chan struct{}
	opened bool
}

func (o *onceGateIterator) Next() bool {
	if !o.opened {
		<-o.gate
		o.opened = true
	}
	return o.Iterator.Next()
}

// Two jobs run when the node shuts down. The release loop is held on the
// first one; meanwhile the other job's executor decides its own outcome. That
// job's end is reported by its executor alone: one lost-claim line when it was
// cancelled elsewhere, none when it succeeded here. The released job reports
// nothing, and the count is of the jobs actually released.
func TestReleaseRegisteredJobs_ExecutorDecidingDuringTheLoop(t *testing.T) {
	for _, tc := range []struct {
		name          string
		cancelOther   bool
		wantLostLines int
		wantStatus    string
	}{
		{"cancelled elsewhere", true, 1, "CANCELLED"},
		{"finished here", false, 0, "SUCCESSFUL"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base := memory.NewStoreFactory()
			defer base.Close()
			ctx := tenantCtx("tenant-release-race")
			ref := spi.ModelRef{EntityName: "releaserace", ModelVersion: "1"}
			saveMinimalModel(t, ctx, base, ref)
			for i := 0; i < 3; i++ {
				saveEntity(t, ctx, base, ref, fmt.Sprintf("e%03d", i), []byte(`{}`))
			}
			scans := &gatedScans{gates: []chan struct{}{make(chan struct{}), make(chan struct{})}}
			started := make(chan struct{}, 2)
			ies := wrapIterate(t, base, ctx, func(it spi.Iterator) spi.Iterator {
				started <- struct{}{}
				return &onceGateIterator{Iterator: it, gate: scans.take()}
			})
			factory := &iterableFactory{StoreFactory: base, entityStore: ies}
			inner, _ := base.AsyncSearchStore(context.Background())
			store := &firstReleaseBlocks{AsyncSearchStore: inner, first: make(chan string, 1), hold: make(chan struct{}), released: map[string]error{}}
			pool := search.NewWorkerPool(2, 4)
			svc := search.NewSearchService(factory, common.NewTestUUIDGenerator(), store, newTestConsistency(t, factory), platformclock.Real()).
				WithAsyncPool(pool).
				WithHeartbeat(time.Hour)

			cond := &predicate.LifecycleCondition{Field: "state", OperatorType: "EQUALS", Value: "NEW"}
			var ids []string
			for i := 0; i < 2; i++ {
				id, err := svc.SubmitAsync(ctx, ref, cond, search.SearchOptions{Limit: 10})
				if err != nil {
					t.Fatalf("SubmitAsync: %v", err)
				}
				ids = append(ids, id)
				select {
				case <-started:
				case <-time.After(5 * time.Second):
					t.Fatal("a job never reached its scan")
				}
			}
			// The scan gates follow submission order: ids[i] holds gates[i].
			gateOf := map[string]chan struct{}{ids[0]: scans.gates[0], ids[1]: scans.gates[1]}

			logs := captureJobLogs(t)
			counted := make(chan int, 1)
			go func() { counted <- svc.ReleaseRegisteredJobs(context.Background()) }()
			held := <-store.first
			other := ids[0]
			if other == held {
				other = ids[1]
			}
			if tc.cancelOther {
				if err := inner.Cancel(ctx, other, time.Now()); err != nil {
					t.Fatalf("Cancel: %v", err)
				}
			}
			close(gateOf[other])
			deadline := time.Now().Add(5 * time.Second)
			for svc.RegisteredJobCountForTest() != 1 {
				if time.Now().After(deadline) {
					t.Fatal("the other job's executor never decided")
				}
				time.Sleep(10 * time.Millisecond)
			}
			close(store.hold)
			n := <-counted
			close(gateOf[held])
			pool.Drain(context.Background())

			if n != 1 {
				t.Errorf("ReleaseRegisteredJobs = %d, want 1: only the held job was released", n)
			}
			job, err := inner.GetJob(ctx, other)
			if err != nil || job.Status != tc.wantStatus {
				t.Fatalf("other job = (%v, %v), want status %s", job, err, tc.wantStatus)
			}
			var otherLines, heldLines []string
			for _, line := range strings.Split(logs.String(), "\n") {
				switch {
				case strings.Contains(line, "jobID="+other):
					otherLines = append(otherLines, line)
				case strings.Contains(line, "jobID="+held):
					heldLines = append(heldLines, line)
				}
			}
			if len(otherLines) != tc.wantLostLines {
				t.Errorf("lines for the job that decided = %q, want %d", otherLines, tc.wantLostLines)
			}
			for _, l := range otherLines {
				if !strings.Contains(l, lostClaimLine) || !strings.Contains(l, "level=INFO") {
					t.Errorf("line for the job that decided = %q, want the lost-claim line at INFO", l)
				}
			}
			if len(heldLines) != 0 {
				t.Errorf("lines for the released job = %q, want none", heldLines)
			}
		})
	}
}

// failingTerminalWrite holds the first terminal status write until hold is
// closed, then fails it with an error that is not a store refusal, as a
// dropped connection does. Once closed is set (the store is closing, as
// App.Shutdown closes it after the release), Release fails too. It counts
// the Release calls that land.
type failingTerminalWrite struct {
	spi.AsyncSearchStore
	entered  chan struct{}
	hold     chan struct{}
	mu       sync.Mutex
	closed   bool
	releases int
}

func (s *failingTerminalWrite) UpdateJobStatus(ctx context.Context, jobID string, epoch int64, status string, count int, errMsg string, finishTime time.Time, calcTimeMs int64) error {
	s.entered <- struct{}{}
	<-s.hold
	return fmt.Errorf("update job %s: connection reset", jobID)
}

func (s *failingTerminalWrite) Release(ctx context.Context, jobID string, epoch int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return fmt.Errorf("release job %s: closed pool", jobID)
	}
	err := s.AsyncSearchStore.Release(ctx, jobID, epoch)
	if err == nil {
		s.releases++
	}
	return err
}

// A shutdown release that meets a job whose executor is recording its outcome
// still releases it, before the store closes: if the recording then fails for
// any reason but a store refusal, the job is already handed back, not left
// RUNNING with nobody to release it.
func TestReleaseRegisteredJobs_FailedRecordingDuringShutdownIsHandedBack(t *testing.T) {
	base := memory.NewStoreFactory()
	defer base.Close()
	ctx := tenantCtx("tenant-release-failed-write")
	ref := spi.ModelRef{EntityName: "releasefailedwrite", ModelVersion: "1"}
	saveMinimalModel(t, ctx, base, ref)
	saveEntity(t, ctx, base, ref, "e000", []byte(`{}`))
	inner, _ := base.AsyncSearchStore(context.Background())
	store := &failingTerminalWrite{AsyncSearchStore: inner, entered: make(chan struct{}, 1), hold: make(chan struct{})}
	letWrite := sync.OnceFunc(func() { close(store.hold) })
	t.Cleanup(letWrite) // a failed wait must not leave the worker blocked
	pool := search.NewWorkerPool(1, 4)
	svc := search.NewSearchService(base, common.NewTestUUIDGenerator(), store, newTestConsistency(t, base), platformclock.Real()).
		WithAsyncPool(pool).
		WithHeartbeat(time.Hour)

	jobID, err := svc.SubmitAsync(ctx, ref, &predicate.LifecycleCondition{Field: "state", OperatorType: "EQUALS", Value: "NEW"}, search.SearchOptions{Limit: 10})
	if err != nil {
		t.Fatalf("SubmitAsync: %v", err)
	}
	select {
	case <-store.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the job never reached its terminal write")
	}
	logs := captureJobLogs(t)
	if n := svc.ReleaseRegisteredJobs(context.Background()); n != 1 {
		t.Errorf("ReleaseRegisteredJobs = %d, want 1", n)
	}
	store.mu.Lock()
	store.closed = true // the store closes after the release, as at shutdown
	store.mu.Unlock()
	letWrite()
	pool.Drain(context.Background())

	store.mu.Lock()
	releases := store.releases
	store.mu.Unlock()
	if releases != 1 {
		t.Errorf("landed Release calls = %d, want 1: the job is handed back before the store closes", releases)
	}
	if strings.Contains(logs.String(), lostClaimLine) {
		t.Errorf("a lost claim was reported for a job still this node's:\n%s", logs.String())
	}
	if job, err := inner.GetJob(ctx, jobID); err != nil || job.Status != "RUNNING" {
		t.Errorf("job = (%v, %v), want RUNNING and released for the next claim", job, err)
	}
}

// heldTerminalWrite holds the first terminal status write until hold is
// closed, then lets it through.
type heldTerminalWrite struct {
	spi.AsyncSearchStore
	entered chan struct{}
	hold    chan struct{}
}

func (s *heldTerminalWrite) UpdateJobStatus(ctx context.Context, jobID string, epoch int64, status string, count int, errMsg string, finishTime time.Time, calcTimeMs int64) error {
	s.entered <- struct{}{}
	<-s.hold
	return s.AsyncSearchStore.UpdateJobStatus(ctx, jobID, epoch, status, count, errMsg, finishTime, calcTimeMs)
}

// A recording that lands after the shutdown release still finishes the job:
// Release keeps the epoch, so the executor's terminal write is not refused,
// and nothing is reported as lost.
func TestReleaseRegisteredJobs_RecordingAfterTheReleaseFinishesTheJob(t *testing.T) {
	base := memory.NewStoreFactory()
	defer base.Close()
	ctx := tenantCtx("tenant-release-then-record")
	ref := spi.ModelRef{EntityName: "releasethenrecord", ModelVersion: "1"}
	saveMinimalModel(t, ctx, base, ref)
	for i := 0; i < 3; i++ {
		saveEntity(t, ctx, base, ref, fmt.Sprintf("e%03d", i), []byte(`{}`))
	}
	inner, _ := base.AsyncSearchStore(context.Background())
	store := &heldTerminalWrite{AsyncSearchStore: inner, entered: make(chan struct{}, 1), hold: make(chan struct{})}
	letWrite := sync.OnceFunc(func() { close(store.hold) })
	t.Cleanup(letWrite) // a failed wait must not leave the worker blocked
	pool := search.NewWorkerPool(1, 4)
	svc := search.NewSearchService(base, common.NewTestUUIDGenerator(), store, newTestConsistency(t, base), platformclock.Real()).
		WithAsyncPool(pool).
		WithHeartbeat(time.Hour)

	jobID, err := svc.SubmitAsync(ctx, ref, &predicate.LifecycleCondition{Field: "state", OperatorType: "EQUALS", Value: "NEW"}, search.SearchOptions{Limit: 10})
	if err != nil {
		t.Fatalf("SubmitAsync: %v", err)
	}
	select {
	case <-store.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the job never reached its terminal write")
	}
	logs := captureJobLogs(t)
	if n := svc.ReleaseRegisteredJobs(context.Background()); n != 1 {
		t.Errorf("ReleaseRegisteredJobs = %d, want 1", n)
	}
	letWrite()
	pool.Drain(context.Background())

	job, err := inner.GetJob(ctx, jobID)
	if err != nil || job.Status != "SUCCESSFUL" || job.ResultCount != 3 {
		t.Fatalf("job = (%+v, %v), want SUCCESSFUL with 3 results", job, err)
	}
	if strings.Contains(logs.String(), lostClaimLine) {
		t.Errorf("a lost claim was reported for a job that finished here:\n%s", logs.String())
	}
}
