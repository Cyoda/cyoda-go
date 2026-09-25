package workflow

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/common"
)

// TestEngine_JoinedChain_RefusesCommitBeforeDispatch: a chain that joined its
// transaction — a compute node's callback — never commits it. A
// COMMIT_BEFORE_DISPATCH processor such a chain reaches is refused before the
// segment is flushed, before anything commits and before the processor is
// dispatched, with a domain error that names the workflow and the processor.
// Both variants commit the segment they were handed, so both are refused.
func TestEngine_JoinedChain_RefusesCommitBeforeDispatch(t *testing.T) {
	for _, startNewTx := range []bool{false, true} {
		name := "startNewTxOnDispatch=false"
		if startNewTx {
			name = "startNewTxOnDispatch=true"
		}
		t.Run(name, func(t *testing.T) {
			h := newSegmentGuardHarness(t, "memory")
			h.registerCBDProcessor("segmenter")
			h.wf.States["A"].Transitions[0].Processors[0].Config.StartNewTxOnDispatch = &startNewTx

			var committed []string
			h.txMgr.commit = func(ctx context.Context, txID string) error {
				committed = append(committed, txID)
				return h.txMgr.TransactionManager.Commit(ctx, txID)
			}
			h.dispatchProcessor = func(context.Context, *spi.Entity, spi.ProcessorDefinition, string, string, string) (*spi.Entity, error) {
				h.dispatched = true
				return nil, nil
			}

			entryTxID, ownerCtx := h.begin(t)
			joinedCtx := WithJoinedTransaction(ownerCtx, entryTxID)

			_, err := h.engine.Execute(joinedCtx, h.entity, "")

			var appErr *common.AppError
			if !errors.As(err, &appErr) {
				t.Fatalf("err = %v; want an *common.AppError", err)
			}
			if appErr.Status != http.StatusConflict || appErr.Code != common.ErrCodeCommitInJoinedTransaction {
				t.Fatalf("got %d %s; want %d %s", appErr.Status, appErr.Code, http.StatusConflict, common.ErrCodeCommitInJoinedTransaction)
			}
			if !strings.Contains(appErr.Message, `"SegmentGuardWF"`) || !strings.Contains(appErr.Message, `"segmenter"`) {
				t.Errorf("message %q does not name the workflow and the processor", appErr.Message)
			}
			if len(committed) != 0 {
				t.Fatalf("a joined chain committed %v", committed)
			}
			if h.dispatched {
				t.Fatal("the refused processor was dispatched")
			}
			if h.saves != 0 || len(h.casSites) != 0 {
				t.Fatalf("the segment was flushed (%d saves, CAS sites %v) before the refusal", h.saves, h.casSites)
			}
			// The owner's transaction is still open and still its own to decide.
			if h.txMgr.sawRollbackOf(entryTxID) {
				t.Fatal("the joined chain rolled back its owner's transaction")
			}
			if err := h.txMgr.TransactionManager.Commit(ownerCtx, entryTxID); err != nil {
				t.Fatalf("owner could not commit its own transaction after the refusal: %v", err)
			}
		})
	}
}

// TestEngine_OwnerChain_CommitBeforeDispatchStillSegments is the control: the
// marker is keyed to the joined transaction, so the owner's chain — which never
// carries it — segments exactly as before.
func TestEngine_OwnerChain_CommitBeforeDispatchStillSegments(t *testing.T) {
	h := newSegmentGuardHarness(t, "memory")
	h.registerCBDProcessor("segmenter")

	entryTxID, ownerCtx := h.begin(t)
	res, err := h.engine.Execute(ownerCtx, h.entity, "")
	if err != nil {
		t.Fatalf("owner chain: %v", err)
	}
	if res.FinalTxID == entryTxID {
		t.Fatal("owner chain did not segment")
	}
}

// TestEngine_JoinedMarker_OtherTransaction_DoesNotRefuse pins the key: a chain
// marked as having joined a DIFFERENT transaction owns the one it runs in.
func TestEngine_JoinedMarker_OtherTransaction_DoesNotRefuse(t *testing.T) {
	h := newSegmentGuardHarness(t, "memory")
	h.registerCBDProcessor("segmenter")

	entryTxID, ownerCtx := h.begin(t)
	ctx := WithJoinedTransaction(ownerCtx, "some-other-transaction")
	res, err := h.engine.Execute(ctx, h.entity, "")
	if err != nil {
		t.Fatalf("chain owning its transaction: %v", err)
	}
	if res.FinalTxID == entryTxID {
		t.Fatal("chain owning its transaction did not segment")
	}
}
