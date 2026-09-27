package memory_test

import (
	"context"
	"errors"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// A commit that aborts — on a first-committer-wins conflict or on a unique
// key violation — leaves the transaction rolled back, not committed: every
// later call with its ctx answers ErrTxRolledBack, and a Rollback of it
// finds nothing left to roll back.
func TestTx_AnAbortedCommitLeavesTheTransactionRolledBack(t *testing.T) {
	ref := spi.ModelRef{EntityName: "abort", ModelVersion: "1"}
	aborts := []struct {
		name string
		want error
		// abort makes txCtx's commit fail.
		abort func(t *testing.T, fx taskFixture, store spi.EntityStore, txCtx context.Context)
	}{
		{"conflict", spi.ErrConflict, func(t *testing.T, fx taskFixture, store spi.EntityStore, txCtx context.Context) {
			if _, err := store.Save(txCtx, &spi.Entity{Meta: spi.EntityMeta{ID: "e1", ModelRef: ref}, Data: []byte(`{}`)}); err != nil {
				t.Fatalf("Save: %v", err)
			}
			otherID, otherCtx := fx.begin(t, taskTenantA)
			if _, err := store.Save(otherCtx, &spi.Entity{Meta: spi.EntityMeta{ID: "e1", ModelRef: ref}, Data: []byte(`{}`)}); err != nil {
				t.Fatalf("other Save: %v", err)
			}
			if err := fx.commit(taskTenantA, otherID); err != nil {
				t.Fatalf("other Commit: %v", err)
			}
		}},
		{"unique violation", spi.ErrUniqueViolation, func(t *testing.T, fx taskFixture, store spi.EntityStore, txCtx context.Context) {
			keyCtx := spi.WithUniqueKeys(txCtx, []spi.UniqueKey{{ID: "email-key", Fields: []string{"$.email"}}})
			for _, id := range []string{"e1", "e2"} {
				if _, err := store.Save(keyCtx, &spi.Entity{Meta: spi.EntityMeta{ID: id, ModelRef: ref}, Data: []byte(`{"email":"a@x.com"}`)}); err != nil {
					t.Fatalf("Save %s: %v", id, err)
				}
			}
		}},
	}
	for _, a := range aborts {
		t.Run(a.name, func(t *testing.T) {
			fx := newTaskFixture(t)
			store, err := fx.f.EntityStore(ctxWithTenant(taskTenantA))
			if err != nil {
				t.Fatalf("EntityStore: %v", err)
			}
			arm(t, context.Background(), fx.sts, taskTenantA, "e1", "T")
			armed, _ := getTask(t, context.Background(), fx.sts, taskTenantA, "e1:S:T")
			txID, txCtx := fx.begin(t, taskTenantA)
			a.abort(t, fx, store, txCtx)
			if err := fx.commit(taskTenantA, txID); !errors.Is(err, a.want) {
				t.Fatalf("Commit = %v, want %v", err, a.want)
			}

			as := auditStore(t, fx, txCtx)
			if err := as.Record(txCtx, "e1", spi.StateMachineEvent{EventType: spi.SMEventTransitionMade, TransactionID: txID}); !errors.Is(err, spi.ErrTxRolledBack) {
				t.Fatalf("Record = %v, want ErrTxRolledBack", err)
			}
			if _, err := fx.sts.ReconcileForEntity(txCtx, spi.ReconcileRequest{TenantID: taskTenantA, EntityID: "e1", CurrentState: "S",
				Arm: []spi.ScheduledTask{armTask(taskTenantA, "e1", "T")}}); !errors.Is(err, spi.ErrTxRolledBack) {
				t.Fatalf("ReconcileForEntity = %v, want ErrTxRolledBack", err)
			}
			if _, err := store.Save(txCtx, &spi.Entity{Meta: spi.EntityMeta{ID: "e3", ModelRef: ref}, Data: []byte(`{}`)}); !errors.Is(err, spi.ErrTxRolledBack) {
				t.Fatalf("Save = %v, want ErrTxRolledBack", err)
			}
			if got, ok := getTask(t, txCtx, fx.sts, taskTenantA, "e1:S:T"); !ok || got.ArmToken != armed.ArmToken {
				t.Fatalf("Get = %+v, %v; want the committed life armed before the abort", got, ok)
			}
			if err := fx.tm.Rollback(ctxWithTenant(taskTenantA), txID); !errors.Is(err, spi.ErrTxNotFound) {
				t.Fatalf("Rollback = %v, want ErrTxNotFound: the abort already discarded the transaction", err)
			}
		})
	}
}
