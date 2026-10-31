package memory_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// armAt arms one task of entity due at scheduledMs.
func armAt(t *testing.T, sts spi.ScheduledTaskStore, tenant spi.TenantID, entity string, scheduledMs int64) {
	t.Helper()
	task := armTask(tenant, entity, "T")
	task.ScheduledTime = scheduledMs
	_, err := sts.ReconcileForEntity(context.Background(), spi.ReconcileRequest{
		TenantID: tenant, EntityID: entity, CurrentState: "S", Arm: []spi.ScheduledTask{task},
	})
	if err != nil {
		t.Fatalf("ReconcileForEntity: %v", err)
	}
}

func TestTasks_SchedulerActivity(t *testing.T) {
	bg := context.Background()

	t.Run("empty store", func(t *testing.T) {
		fx := newTaskFixture(t)
		got, err := fx.sts.SchedulerActivity(bg)
		if err != nil {
			t.Fatalf("SchedulerActivity: %v", err)
		}
		if got.HasWaiting || got.Running {
			t.Fatalf("empty store = %+v, want no waiting and not running", got)
		}
	})

	t.Run("earliest waiting across tenants, failed ignored, then running", func(t *testing.T) {
		fx := newTaskFixture(t)
		// A FAILED task at 1000.
		armAt(t, fx.sts, taskTenantA, "failed", 1_000)
		c := claimDue(t, fx.sts, uuid.New(), false)
		if len(c) != 1 {
			t.Fatalf("claimed %d, want 1", len(c))
		}
		if err := fx.sts.Fail(bg, refOf(c[0]), spi.Failure{Reason: spi.FailureOwnerLostRepeatedly, AtMs: 1_500}); err != nil {
			t.Fatalf("Fail: %v", err)
		}
		armAt(t, fx.sts, taskTenantA, "a", 3_000)
		armAt(t, fx.sts, taskTenantB, "b", 2_000)

		got, err := fx.sts.SchedulerActivity(bg)
		if err != nil {
			t.Fatalf("SchedulerActivity: %v", err)
		}
		if !got.HasWaiting || got.EarliestWaitingMs != 2_000 || got.Running {
			t.Fatalf("got %+v, want Earliest 2000, HasWaiting, not Running", got)
		}

		// ClaimDue at 2500 takes only the 2000 task.
		claimed, err := fx.sts.ClaimDue(bg, spi.ClaimRequest{
			Owner: uuid.New(), NowMs: 2_500, StaleAfter: time.Minute, Limit: 100, PerTenantLimit: 100,
		})
		if err != nil || len(claimed) != 1 {
			t.Fatalf("ClaimDue = %v, %v; want one task", claimed, err)
		}
		got, err = fx.sts.SchedulerActivity(bg)
		if err != nil {
			t.Fatalf("SchedulerActivity: %v", err)
		}
		if !got.Running || !got.HasWaiting || got.EarliestWaitingMs != 3_000 {
			t.Fatalf("got %+v, want Running, Earliest 3000", got)
		}
	})
}
