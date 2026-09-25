package entity

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"sync/atomic"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/common"
)

// TestJoinedFlows_CommitBeforeDispatch_RefusedOwnerUntouched: a compute node's
// callback runs in the transaction it joined, and only the operation that
// began that transaction commits it. Every entity flow that runs the workflow
// engine refuses a COMMIT_BEFORE_DISPATCH processor on a joined call with a 409
// COMMIT_IN_JOINED_TRANSACTION, and leaves the owner's transaction exactly as it
// was: open, not rolled back, and the only transaction open. The processor is
// never dispatched.
//
// The batch update carries an If-Match on purpose: its per-item isolation must
// not swallow the refusal as a precondition failure.
func TestJoinedFlows_CommitBeforeDispatch_RefusedOwnerUntouched(t *testing.T) {
	segmentRef := spi.ModelRef{EntityName: "JoinedSegmentWidget", ModelVersion: "1"}

	type flow struct {
		name string
		// setup prepares the workflow and any committed entity the flow needs,
		// and returns that entity's id ("" when none).
		setup func(t *testing.T, hn *rollbackHarness, dispatched *atomic.Int32) string
		run   func(ctx context.Context, hn *rollbackHarness, id string) error
	}
	createSetup := func(t *testing.T, hn *rollbackHarness, dispatched *atomic.Int32) string {
		hn.registerSegmentingWorkflow(t)
		hn.proc.dispatchProcessor = func(context.Context, *spi.Entity, spi.ProcessorDefinition, string, string, string) (*spi.Entity, error) {
			dispatched.Add(1)
			return nil, nil
		}
		return ""
	}
	updateSetup := func(t *testing.T, hn *rollbackHarness, dispatched *atomic.Int32) string {
		counter := hn.registerManualSegmentingWorkflow(t, segmentRef)
		id := hn.createEntityIn(t, segmentRef, `{"name":"w"}`)
		hn.proc.dispatchProcessor = func(context.Context, *spi.Entity, spi.ProcessorDefinition, string, string, string) (*spi.Entity, error) {
			counter.Add(1)
			dispatched.Add(1)
			return nil, nil
		}
		return id
	}
	flows := []flow{
		{"CreateEntity", createSetup, func(ctx context.Context, hn *rollbackHarness, _ string) error {
			_, err := hn.h.CreateEntity(ctx, rollbackWidgetInput())
			return err
		}},
		{"CreateEntityCollection", createSetup, func(ctx context.Context, hn *rollbackHarness, _ string) error {
			_, err := hn.h.CreateEntityCollection(ctx, []CollectionItem{{
				ModelName: rollbackModel.EntityName, ModelVersion: 1, Payload: json.RawMessage(`{"name":"w"}`),
			}})
			return err
		}},
		{"UpdateEntity", updateSetup, func(ctx context.Context, hn *rollbackHarness, id string) error {
			_, err := hn.h.UpdateEntity(ctx, UpdateEntityInput{
				EntityID: id, Format: "JSON", Data: json.RawMessage(`{"name":"x"}`), Transition: "segment",
			})
			return err
		}},
		{"UpdateEntityCollection", updateSetup, func(ctx context.Context, hn *rollbackHarness, id string) error {
			current := hn.committedEntity(t, id)
			_, err := hn.h.UpdateEntityCollection(ctx, []UpdateCollectionItem{{
				EntityID: id, Payload: json.RawMessage(`{"name":"x"}`), Transition: "segment",
				IfMatch: current.Meta.TransactionID,
			}})
			return err
		}},
	}

	for _, f := range flows {
		t.Run(f.name, func(t *testing.T) {
			hn := newTrackingHandler(t)
			var dispatched atomic.Int32
			id := f.setup(t, hn, &dispatched)

			ownerTxID, _, err := hn.tracker.Begin(hn.ctx)
			if err != nil {
				t.Fatalf("owner Begin: %v", err)
			}
			joinedCtx, err := hn.tracker.Join(hn.ctx, ownerTxID)
			if err != nil {
				t.Fatalf("Join: %v", err)
			}

			runErr := f.run(joinedCtx, hn, id)

			var appErr *common.AppError
			if !errors.As(runErr, &appErr) {
				t.Fatalf("err = %v; want an *common.AppError", runErr)
			}
			if appErr.Status != http.StatusConflict || appErr.Code != common.ErrCodeCommitInJoinedTransaction {
				t.Fatalf("got %d %s (%s); want %d %s", appErr.Status, appErr.Code, appErr.Message,
					http.StatusConflict, common.ErrCodeCommitInJoinedTransaction)
			}
			if n := dispatched.Load(); n != 0 {
				t.Fatalf("the refused processor was dispatched %d time(s)", n)
			}
			if hn.tracker.wasRolledBack(ownerTxID) {
				t.Fatal("a joined call rolled back its owner's transaction")
			}
			if open := hn.tracker.openTxIDs(); !slices.Equal(open, []string{ownerTxID}) {
				t.Fatalf("open transactions = %v; want only the owner's %s (not committed, nothing leaked)", open, ownerTxID)
			}
		})
	}
}
