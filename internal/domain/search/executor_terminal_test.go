package search_test

// Two defects in runAsyncJob's tail, both about what the terminal write
// says and which context it says it on.

import (
	"context"
	"fmt"
	"iter"
	"sync"
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/domain/search"
	"github.com/cyoda-platform/cyoda-go/plugins/memory"
)

// terminalWriteObserverStore records the SUCCESSFUL terminal write — the
// context it arrives on and the result count it carries — and can REFUSE the
// streamed id sequence part-way, which is what a real store does when it
// hits its own limit or its chunk write fails: it takes consumeLimit ids and
// then declines the next one, so that last id is pulled off the iterator but
// never accepted.
type terminalWriteObserverStore struct {
	spi.AsyncSearchStore
	consumeLimit int // 0 means consume the whole sequence

	mu           sync.Mutex
	successCtx   context.Context
	successCount int
	sawSuccess   bool
}

func (s *terminalWriteObserverStore) SaveResults(ctx context.Context, jobID string, epoch int64, entityIDs iter.Seq[string]) error {
	if s.consumeLimit <= 0 {
		return s.AsyncSearchStore.SaveResults(ctx, jobID, epoch, entityIDs)
	}
	accepted := 0
	for range entityIDs {
		if accepted >= s.consumeLimit {
			// Refuse this id — the store is not taking it.
			break
		}
		accepted++
	}
	return nil
}

func (s *terminalWriteObserverStore) UpdateJobStatus(ctx context.Context, jobID string, epoch int64, status string, resultCount int, errMsg string, finishTime time.Time, calcTimeMs int64) error {
	if status == "SUCCESSFUL" {
		s.mu.Lock()
		s.successCtx = ctx
		s.successCount = resultCount
		s.sawSuccess = true
		s.mu.Unlock()
	}
	return s.AsyncSearchStore.UpdateJobStatus(ctx, jobID, epoch, status, resultCount, errMsg, finishTime, calcTimeMs)
}

func (s *terminalWriteObserverStore) snapshot() (context.Context, int, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.successCtx, s.successCount, s.sawSuccess
}

func newObserverService(t *testing.T, base *memory.StoreFactory, store *terminalWriteObserverStore) *search.SearchService {
	t.Helper()
	pool := search.NewWorkerPool(2, 8)
	t.Cleanup(func() { pool.Drain(context.Background()) })
	return search.NewSearchService(base, common.NewTestUUIDGenerator(), store).
		WithAsyncPool(pool).
		WithHeartbeat(50 * time.Millisecond)
}

// TestExecutor_ResultCount_CountsOnlyAcceptedIDs pins that the count written
// into the job record is the number of ids the consumer accepted, not the
// number the producer pulled off the iterator. The producer used to
// increment before yielding, so the one id the store declined was still
// counted and the job advertised a result the store never holds — a status
// that over-promises what GetAsyncResults can serve.
func TestExecutor_ResultCount_CountsOnlyAcceptedIDs(t *testing.T) {
	const total, consume = 10, 3
	base := memory.NewStoreFactory()
	defer base.Close()
	ctx := tenantCtx("tenant-count")
	ref := spi.ModelRef{EntityName: "countitem", ModelVersion: "1"}
	saveMinimalModel(t, ctx, base, ref)
	for i := 0; i < total; i++ {
		saveEntity(t, ctx, base, ref, fmt.Sprintf("e%05d", i), []byte(`{}`))
	}

	realAsync, err := base.AsyncSearchStore(context.Background())
	if err != nil {
		t.Fatalf("AsyncSearchStore: %v", err)
	}
	store := &terminalWriteObserverStore{AsyncSearchStore: realAsync, consumeLimit: consume}
	svc := newObserverService(t, base, store)

	jobID, err := svc.SubmitAsync(ctx, ref, matchEverything, search.SearchOptions{Limit: 10})
	if err != nil {
		t.Fatalf("SubmitAsync: %v", err)
	}
	status := pollUntilTerminal(t, svc, ctx, jobID, 5*time.Second)
	if status.Status != "SUCCESSFUL" {
		t.Fatalf("status = %q, want SUCCESSFUL", status.Status)
	}
	if status.Total != consume {
		t.Errorf("recorded result count = %d, want %d (the store accepted %d ids and declined the next; %d means the declined id was counted as a result)",
			status.Total, consume, consume, consume+1)
	}
}

// TestExecutor_TerminalWrite_IsNotCancellable pins that the SUCCESSFUL
// terminal write runs on a context stripped of cancellation, exactly as the
// panic path already does.
//
// Reading jobCtx.Err() and then writing on jobCtx is a TOCTOU: the heartbeat
// goroutine cancels jobCtx whenever a Heartbeat call is fenced out or a poll
// observes a terminal status, and if that lands in the window between the
// two, the write is aborted by its own context and the job stays RUNNING
// until the stale-job reaper eventually fails it. A ctx with no Done channel
// cannot be caught by that window at all — which is why this asserts the
// property rather than trying to hit the race.
func TestExecutor_TerminalWrite_IsNotCancellable(t *testing.T) {
	base := memory.NewStoreFactory()
	defer base.Close()
	ctx := tenantCtx("tenant-terminal")
	ref := spi.ModelRef{EntityName: "terminalitem", ModelVersion: "1"}
	saveMinimalModel(t, ctx, base, ref)
	saveEntity(t, ctx, base, ref, "e00001", []byte(`{}`))

	realAsync, err := base.AsyncSearchStore(context.Background())
	if err != nil {
		t.Fatalf("AsyncSearchStore: %v", err)
	}
	store := &terminalWriteObserverStore{AsyncSearchStore: realAsync}
	svc := newObserverService(t, base, store)

	jobID, err := svc.SubmitAsync(ctx, ref, matchEverything, search.SearchOptions{Limit: 10})
	if err != nil {
		t.Fatalf("SubmitAsync: %v", err)
	}
	if status := pollUntilTerminal(t, svc, ctx, jobID, 5*time.Second); status.Status != "SUCCESSFUL" {
		t.Fatalf("status = %q, want SUCCESSFUL", status.Status)
	}

	writeCtx, _, saw := store.snapshot()
	if !saw {
		t.Fatal("no SUCCESSFUL terminal write was observed")
	}
	if writeCtx.Done() != nil {
		t.Error("the terminal write ran on a cancellable context; a heartbeat cancel landing after the jobCtx.Err() check aborts it and leaves the job RUNNING")
	}
	if uc := spi.GetUserContext(writeCtx); uc == nil {
		t.Error("the terminal write lost its UserContext; stripping cancellation must keep the tenant scope")
	}
}
