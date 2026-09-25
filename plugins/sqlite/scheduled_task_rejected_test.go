package sqlite_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/plugins/sqlite"
)

func TestTasks_TheStoreRejectsWhatNoBackendStores(t *testing.T) {
	fx := newTaskFixture(t)
	bg := context.Background()
	arm(t, bg, fx.sts, taskTenantA, "e1", "T")
	c := claimDue(t, fx.sts, uuid.New(), false)[0]
	ref := refOf(c)
	const secret = "do-not-echo"

	noID := armTask(taskTenantA, "e1", "T")
	noID.ID = ""
	cases := []struct {
		name string
		call func() error
	}{
		{"attempt error with NUL", func() error {
			return fx.sts.RecordAttempt(bg, ref, spi.Attempt{Error: secret + "\x00", AtMs: 2_000, NextAttemptTime: 3_000})
		}},
		{"attempt error not UTF-8", func() error {
			return fx.sts.RecordAttempt(bg, ref, spi.Attempt{Error: secret + "\xff", AtMs: 2_000, NextAttemptTime: 3_000})
		}},
		{"attempt error over 1024 bytes", func() error {
			return fx.sts.RecordAttempt(bg, ref, spi.Attempt{Error: secret + strings.Repeat("x", 1024), AtMs: 2_000, NextAttemptTime: 3_000})
		}},
		{"failure with an unknown reason", func() error {
			return fx.sts.Fail(bg, ref, spi.Failure{Reason: "NOT_A_REASON", Error: secret, AtMs: 2_000})
		}},
		{"failure error with NUL", func() error {
			return fx.sts.Fail(bg, ref, spi.Failure{Reason: spi.FailureRunPanicked, Error: secret + "\x00", AtMs: 2_000})
		}},
		{"arm without an id", func() error {
			_, err := fx.sts.ReconcileForEntity(bg, spi.ReconcileRequest{TenantID: taskTenantA, EntityID: "e1", CurrentState: "S",
				Arm: []spi.ScheduledTask{noID}})
			return err
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.call()
			if !errors.Is(err, spi.ErrStoreRejected) {
				t.Fatalf("err = %v, want ErrStoreRejected", err)
			}
			if strings.Contains(err.Error(), secret) {
				t.Fatalf("the rejection repeats the rejected text: %v", err)
			}
		})
	}

	got, _ := getTask(t, bg, fx.sts, taskTenantA, "e1:S:T")
	if got.Status != spi.ScheduledTaskRunning || got.Claim == nil || got.Claim.Token != c.Claim.Token || got.ArmToken != c.ArmToken {
		t.Fatalf("a rejected write changed the task: %+v", got)
	}
}

// A write refused inside an open transaction stages nothing: the row is not
// busy, and the transaction commits without a conflict even though the row
// changes after it began.
func TestTasks_ARefusedWriteInATransactionStagesNothing(t *testing.T) {
	fx := newTaskFixture(t)
	bg := context.Background()
	arm(t, bg, fx.sts, taskTenantA, "e1", "T")
	c := claimDue(t, fx.sts, uuid.New(), false)[0]
	stale := refOf(c)
	stale.ClaimToken = uuid.New()
	noID := armTask(taskTenantA, "e1", "T")
	noID.ID = ""

	txID, txCtx := fx.begin(t, taskTenantA)
	refusals := []struct {
		name string
		want error
		call func() error
	}{
		{"stale StampSegment", spi.ErrStaleClaim, func() error { return fx.sts.StampSegment(txCtx, stale, true) }},
		{"stale Fail", spi.ErrStaleClaim, func() error {
			return fx.sts.Fail(txCtx, stale, spi.Failure{Reason: spi.FailureRunPanicked, Error: "E", AtMs: 2_000})
		}},
		{"Fail with an unknown reason", spi.ErrStoreRejected, func() error {
			return fx.sts.Fail(txCtx, refOf(c), spi.Failure{Reason: "NOT_A_REASON", AtMs: 2_000})
		}},
		{"Fail with error text not UTF-8", spi.ErrStoreRejected, func() error {
			return fx.sts.Fail(txCtx, refOf(c), spi.Failure{Reason: spi.FailureRunPanicked, Error: "\xff", AtMs: 2_000})
		}},
		{"arm without an id", spi.ErrStoreRejected, func() error {
			_, err := fx.sts.ReconcileForEntity(txCtx, spi.ReconcileRequest{TenantID: taskTenantA, EntityID: "e1", CurrentState: "S",
				Arm: []spi.ScheduledTask{noID}})
			return err
		}},
	}
	for _, r := range refusals {
		if err := r.call(); !errors.Is(err, r.want) {
			t.Fatalf("%s = %v, want %v", r.name, err, r.want)
		}
	}

	if err := fx.sts.MarkUnsafe(bg, refOf(c)); err != nil {
		t.Fatalf("MarkUnsafe = %v, want nil: a refused write does not make the row busy", err)
	}
	if err := fx.sts.RecordAttempt(bg, refOf(c), spi.Attempt{AtMs: 2_000, NextAttemptTime: 3_000}); err != nil {
		t.Fatalf("RecordAttempt = %v, want nil", err)
	}
	if err := fx.commit(taskTenantA, txID); err != nil {
		t.Fatalf("Commit = %v, want nil: the refused writes staged nothing", err)
	}
	got, _ := getTask(t, bg, fx.sts, taskTenantA, "e1:S:T")
	if got.Status != spi.ScheduledTaskWaiting || got.PartialCommit || got.ArmToken != c.ArmToken {
		t.Fatalf("task = %+v, want the recorded attempt and nothing of the refused writes", got)
	}
}

// A write SQLite itself refuses — a CHECK, NOT NULL or type violation — is
// deterministic: retrying it cannot succeed.
func TestTasks_SQLiteConstraintErrorsAreStoreRejections(t *testing.T) {
	fx := newTaskFixture(t)
	_, err := sqlite.DBForTest(fx.f).Exec(`INSERT INTO scheduled_tasks
		(id, tenant_id, type, scheduled_time, entity_id, model_name, model_version,
		 transition, source_state, armed_at, arm_token, status, next_attempt_time)
		VALUES ('e1:S:T', 'tenant-A', 'FIRE_TRANSITION', 1000, 'e1', 'M', 1, 'T', 'S', 0, ?, 'BOGUS', 1000)`,
		uuid.NewString())
	if err == nil {
		t.Fatal("the CHECK on status accepted BOGUS")
	}
	if got := sqlite.ClassifyRejectionForTest(err); !errors.Is(got, spi.ErrStoreRejected) {
		t.Fatalf("classified %v, want ErrStoreRejected", got)
	}

	plain := errors.New("connection reset")
	if got := sqlite.ClassifyRejectionForTest(plain); got != plain {
		t.Fatalf("a non-deterministic error was changed: %v", got)
	}
	if got := sqlite.ClassifyRejectionForTest(nil); got != nil {
		t.Fatalf("nil classified as %v", got)
	}
}
