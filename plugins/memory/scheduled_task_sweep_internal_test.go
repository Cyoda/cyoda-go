package memory

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

func TestSweeps_RemoveEndedLivesAndDeadUnusedOwners(t *testing.T) {
	clock := NewTestClockAt(time.UnixMilli(1_000_000))
	f := NewStoreFactory(WithClock(clock))
	t.Cleanup(func() { _ = f.Close() })
	sts := &scheduledTaskStore{f: f}
	bg := context.Background()
	task := spi.ScheduledTask{
		ID: "e1:S:T", TenantID: "tenant-A", Type: spi.ScheduledTaskFireTransition,
		ScheduledTime: 1_000, EntityID: "e1", ModelName: "M", ModelVersion: 1,
		Transition: "T", SourceState: "S",
	}
	armReq := spi.ReconcileRequest{TenantID: "tenant-A", EntityID: "e1", CurrentState: "S", Arm: []spi.ScheduledTask{task}}
	if _, err := sts.ReconcileForEntity(bg, armReq); err != nil {
		t.Fatalf("arm: %v", err)
	}

	busyOwner, idleOwner := uuid.New(), uuid.New()
	for _, o := range []uuid.UUID{busyOwner, idleOwner} {
		if err := sts.Heartbeat(bg, o); err != nil {
			t.Fatalf("Heartbeat: %v", err)
		}
	}
	claimed, err := sts.ClaimDue(bg, spi.ClaimRequest{Owner: busyOwner, NowMs: 2_000, StaleAfter: time.Minute, Limit: 1, PerTenantLimit: 1})
	if err != nil || len(claimed) != 1 {
		t.Fatalf("ClaimDue: %v, %d", err, len(claimed))
	}
	c := claimed[0]
	if err := sts.MarkUnsafe(bg, spi.TaskRef{TenantID: c.TenantID, ID: c.ID, ArmToken: c.ArmToken, ClaimToken: c.Claim.Token}); err != nil {
		t.Fatalf("MarkUnsafe: %v", err)
	}

	if err := sts.SweepMarks(bg); err != nil {
		t.Fatalf("SweepMarks: %v", err)
	}
	if len(f.taskMarks) != 1 {
		t.Fatalf("SweepMarks removed the mark of a live life")
	}
	if _, err := sts.ReconcileForEntity(bg, armReq); err != nil { // a new life ends the marked one
		t.Fatalf("re-arm: %v", err)
	}
	if err := sts.SweepMarks(bg); err != nil {
		t.Fatalf("SweepMarks: %v", err)
	}
	if len(f.taskMarks) != 0 {
		t.Fatalf("SweepMarks kept %d marks of ended lives", len(f.taskMarks))
	}

	// The re-arm left no RUNNING task; claim again so busyOwner is referenced.
	if _, err := sts.ClaimDue(bg, spi.ClaimRequest{Owner: busyOwner, NowMs: 2_000, StaleAfter: time.Minute, Limit: 1, PerTenantLimit: 1}); err != nil {
		t.Fatalf("ClaimDue: %v", err)
	}
	clock.Advance(time.Hour)
	if err := sts.SweepOwners(bg, time.Minute); err != nil {
		t.Fatalf("SweepOwners: %v", err)
	}
	if _, ok := f.schedulerOwners[idleOwner]; ok {
		t.Fatal("SweepOwners kept a dead owner no task references")
	}
	if _, ok := f.schedulerOwners[busyOwner]; !ok {
		t.Fatal("SweepOwners removed an owner a RUNNING task references")
	}
}
