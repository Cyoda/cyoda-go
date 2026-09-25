package memory_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"

	spi "github.com/cyoda-platform/cyoda-go-spi"
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
