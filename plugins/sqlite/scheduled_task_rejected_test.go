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

// A rejection is checked before anything is staged: a rejected write on an
// open transaction leaves nothing on it, so the transaction's own view of
// the task, and the committed state after it commits, are both unchanged.
func TestTasks_RejectedWriteInTransactionStagesNothing(t *testing.T) {
	fx := newTaskFixture(t)
	bg := context.Background()
	arm(t, bg, fx.sts, taskTenantA, "e1", "T")
	before, _ := getTask(t, bg, fx.sts, taskTenantA, "e1:S:T")

	txID, txCtx := fx.begin(t, taskTenantA)
	noID := armTask(taskTenantA, "e1", "T2")
	noID.ID = ""
	_, err := fx.sts.ReconcileForEntity(txCtx, spi.ReconcileRequest{
		TenantID: taskTenantA, EntityID: "e1", CurrentState: "S", Arm: []spi.ScheduledTask{noID},
	})
	if !errors.Is(err, spi.ErrStoreRejected) {
		t.Fatalf("err = %v, want ErrStoreRejected", err)
	}

	// The transaction's own view (its snapshot plus its staged ops) still
	// shows e1's existing task untouched, and never gained the refused arm.
	staged, ok := getTask(t, txCtx, fx.sts, taskTenantA, "e1:S:T")
	if !ok || staged.ArmToken != before.ArmToken {
		t.Fatalf("the transaction's view changed from a rejected write: %+v", staged)
	}
	if _, ok := getTask(t, txCtx, fx.sts, taskTenantA, "e1:S:T2"); ok {
		t.Fatalf("the rejected arm was staged despite being refused")
	}

	if err := fx.commit(taskTenantA, txID); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	after, _ := getTask(t, bg, fx.sts, taskTenantA, "e1:S:T")
	if after.ArmToken != before.ArmToken {
		t.Fatalf("the committed task changed from a rejected write: %+v", after)
	}
	if _, ok := getTask(t, bg, fx.sts, taskTenantA, "e1:S:T2"); ok {
		t.Fatalf("the rejected arm was committed despite being refused")
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
