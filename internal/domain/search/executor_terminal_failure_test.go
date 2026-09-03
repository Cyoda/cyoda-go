package search_test

// The FAILURE-path twin of executor_terminal_test.go's
// TestExecutor_TerminalWrite_IsNotCancellable.

import (
	"context"
	"errors"
	"iter"
	"sync"
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/domain/search"
	"github.com/cyoda-platform/cyoda-go/plugins/memory"
)

// failingSaveObserverStore refuses the streamed id sequence outright — what a
// real store does when its chunk write fails — and records the context the
// resulting FAILED terminal write arrives on.
type failingSaveObserverStore struct {
	spi.AsyncSearchStore

	mu         sync.Mutex
	failedCtx  context.Context
	failedMsg  string
	sawFailure bool
}

func (s *failingSaveObserverStore) SaveResults(context.Context, string, int64, iter.Seq[string]) error {
	return errors.New("chunk write refused by the store")
}

func (s *failingSaveObserverStore) UpdateJobStatus(ctx context.Context, jobID string, epoch int64, status string, resultCount int, errMsg string, finishTime time.Time, calcTimeMs int64) error {
	if status == "FAILED" {
		s.mu.Lock()
		s.failedCtx = ctx
		s.failedMsg = errMsg
		s.sawFailure = true
		s.mu.Unlock()
	}
	return s.AsyncSearchStore.UpdateJobStatus(ctx, jobID, epoch, status, resultCount, errMsg, finishTime, calcTimeMs)
}

func (s *failingSaveObserverStore) snapshot() (context.Context, string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.failedCtx, s.failedMsg, s.sawFailure
}

// TestExecutor_FailureTerminalWrite_IsNotCancellable pins that a FAILED
// terminal write, like the SUCCESSFUL one, runs on a context stripped of
// cancellation.
//
// The success branch was made non-cancellable; its five siblings — the
// producer-error and save-error arms of the same switch, and the three
// early-return arms above them — still passed the cancellable jobCtx. The
// producer/save arms are reached only after `jobCtx.Err() != nil` evaluated
// FALSE, so the identical TOCTOU is open on them: the heartbeat goroutine can
// cancel between that check and the write, which aborts it and leaves a
// finished job RUNNING until the stale-job reaper fails it — with the reaper's
// generic message in place of the real failure reason the executor knows.
//
// The fix belongs inside writeAsyncFailure rather than at each call site: it
// is a terminal write by definition, so no caller wants it cancellable. That
// makes this one assertion cover all five.
func TestExecutor_FailureTerminalWrite_IsNotCancellable(t *testing.T) {
	base := memory.NewStoreFactory()
	defer base.Close()
	ctx := tenantCtx("tenant-terminal-fail")
	ref := spi.ModelRef{EntityName: "failitem", ModelVersion: "1"}
	saveMinimalModel(t, ctx, base, ref)
	saveEntity(t, ctx, base, ref, "e00001", []byte(`{}`))

	realAsync, err := base.AsyncSearchStore(context.Background())
	if err != nil {
		t.Fatalf("AsyncSearchStore: %v", err)
	}
	store := &failingSaveObserverStore{AsyncSearchStore: realAsync}
	pool := search.NewWorkerPool(2, 8)
	t.Cleanup(func() { pool.Drain(context.Background()) })
	svc := search.NewSearchService(base, common.NewTestUUIDGenerator(), store).
		WithAsyncPool(pool).
		WithHeartbeat(50 * time.Millisecond)

	jobID, err := svc.SubmitAsync(ctx, ref, matchEverything, search.SearchOptions{Limit: 10})
	if err != nil {
		t.Fatalf("SubmitAsync: %v", err)
	}
	if status := pollUntilTerminal(t, svc, ctx, jobID, 5*time.Second); status.Status != "FAILED" {
		t.Fatalf("status = %q, want FAILED", status.Status)
	}

	writeCtx, msg, saw := store.snapshot()
	if !saw {
		t.Fatal("no FAILED terminal write was observed")
	}
	if writeCtx.Done() != nil {
		t.Error("the FAILED terminal write ran on a cancellable context; a heartbeat cancel landing after the jobCtx.Err() check aborts it and leaves the job RUNNING until the reaper fails it with a generic message")
	}
	if uc := spi.GetUserContext(writeCtx); uc == nil {
		t.Error("the FAILED terminal write lost its UserContext; stripping cancellation must keep the tenant scope")
	}
	if msg == "" {
		t.Error("the FAILED terminal write recorded no message; the executor's own failure reason is what the reaper cannot reconstruct")
	}
}
