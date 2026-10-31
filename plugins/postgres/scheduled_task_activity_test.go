package postgres_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

func TestPostgres_SchedulerActivity(t *testing.T) {
	bg := context.Background()

	t.Run("empty store", func(t *testing.T) {
		_, sts := newTaskStore(t, 5)
		got, err := sts.SchedulerActivity(bg)
		if err != nil {
			t.Fatalf("SchedulerActivity: %v", err)
		}
		if got.HasWaiting || got.Running {
			t.Fatalf("empty store = %+v, want no waiting and not running", got)
		}
	})

	t.Run("earliest waiting across tenants, failed ignored, then running", func(t *testing.T) {
		_, sts := newTaskStore(t, 5)
		// A FAILED task at 1000.
		arm(t, sts, "tenant-A", "failed", "S", taskSpec("tenant-A", "failed", "S", "T", 1_000))
		claimed, err := sts.ClaimDue(bg, spi.ClaimRequest{
			Owner: uuid.New(), NowMs: 1_500, StaleAfter: time.Minute, Limit: 10, PerTenantLimit: 10,
		})
		if err != nil || len(claimed) != 1 {
			t.Fatalf("ClaimDue = %v, %v; want one task", claimed, err)
		}
		if err := sts.Fail(bg, refOf(claimed[0]), spi.Failure{Reason: spi.FailureOwnerLostRepeatedly, AtMs: 1_500}); err != nil {
			t.Fatalf("Fail: %v", err)
		}
		arm(t, sts, "tenant-A", "a", "S", taskSpec("tenant-A", "a", "S", "T", 3_000))
		arm(t, sts, "tenant-B", "b", "S", taskSpec("tenant-B", "b", "S", "T", 2_000))

		got, err := sts.SchedulerActivity(bg)
		if err != nil {
			t.Fatalf("SchedulerActivity: %v", err)
		}
		if !got.HasWaiting || got.EarliestWaitingMs != 2_000 || got.Running {
			t.Fatalf("got %+v, want Earliest 2000, HasWaiting, not Running", got)
		}

		// ClaimDue at 2500 takes only the 2000 task.
		claimed, err = sts.ClaimDue(bg, spi.ClaimRequest{
			Owner: uuid.New(), NowMs: 2_500, StaleAfter: time.Minute, Limit: 10, PerTenantLimit: 10,
		})
		if err != nil || len(claimed) != 1 {
			t.Fatalf("ClaimDue = %v, %v; want one task", claimed, err)
		}
		got, err = sts.SchedulerActivity(bg)
		if err != nil {
			t.Fatalf("SchedulerActivity: %v", err)
		}
		if !got.Running || !got.HasWaiting || got.EarliestWaitingMs != 3_000 {
			t.Fatalf("got %+v, want Running, Earliest 3000", got)
		}
	})
}
