package search_test

// What CancelAsync answers when the job ends on its own just before the
// cancel lands.

import (
	"context"
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go-spi/predicate"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/domain/search"
	"github.com/cyoda-platform/cyoda-go/internal/platformclock"
	"github.com/cyoda-platform/cyoda-go/plugins/memory"
)

// finishesBeforeCancel records the job SUCCESSFUL at its first claim's epoch
// just before each Cancel, as an executor finishing between CancelAsync's
// status read and its cancel does. Cancel is idempotent on a terminal job, so
// it then answers nil.
type finishesBeforeCancel struct {
	spi.AsyncSearchStore
}

func (s *finishesBeforeCancel) Cancel(ctx context.Context, jobID string, finishTime time.Time) error {
	if err := s.AsyncSearchStore.UpdateJobStatus(ctx, jobID, 1, "SUCCESSFUL", 0, "", finishTime, 0); err != nil {
		return err
	}
	return s.AsyncSearchStore.Cancel(ctx, jobID, finishTime)
}

// A job that finished before the cancel landed was not cancelled: CancelAsync
// says so and reports the status the job ended with.
func TestCancelAsync_JobThatFinishedFirstIsNotReportedCancelled(t *testing.T) {
	base := memory.NewStoreFactory()
	defer base.Close()
	ctx := tenantCtx("tenant-cancel-outcome")
	ref := spi.ModelRef{EntityName: "canceloutcome", ModelVersion: "1"}
	saveMinimalModel(t, ctx, base, ref)
	inner, _ := base.AsyncSearchStore(context.Background())

	// One worker, held busy, so the job stays RUNNING in the store and no
	// executor touches it.
	pool := search.NewWorkerPool(1, 4)
	hold := make(chan struct{})
	if err := pool.Submit(func() { <-hold }); err != nil {
		t.Fatalf("Submit the holder: %v", err)
	}
	svc := search.NewSearchService(base, common.NewTestUUIDGenerator(), &finishesBeforeCancel{AsyncSearchStore: inner}, newTestConsistency(t, base), platformclock.Real()).
		WithAsyncPool(pool).
		WithHeartbeat(time.Hour)
	jobID, err := svc.SubmitAsync(ctx, ref, &predicate.LifecycleCondition{Field: "state", OperatorType: "EQUALS", Value: "NEW"}, search.SearchOptions{Limit: 10})
	if err != nil {
		t.Fatalf("SubmitAsync: %v", err)
	}

	result, err := svc.CancelAsync(ctx, jobID)
	close(hold)
	pool.Drain(context.Background())
	if err != nil {
		t.Fatalf("CancelAsync: %v", err)
	}
	if result.Cancelled || result.CurrentStatus != "SUCCESSFUL" {
		t.Fatalf("CancelAsync = %+v, want not cancelled, current status SUCCESSFUL", result)
	}
}
