package entity

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/common"
)

// service_txaborted_test.go — an If-Match compare that meets a transaction an
// earlier conflict already aborted (spi.ErrTxAborted) never evaluated the
// precondition. Each of the three compares answers the retryable 409 CONFLICT
// with the cause attached, never 412 ENTITY_MODIFIED.

// txAbortedErr is what a store returns for a statement in such a transaction.
var txAbortedErr = fmt.Errorf("%w: a joined write lost first-committer-wins", spi.ErrTxAborted)

func requireTxAbortedConflict(t *testing.T, err error) {
	t.Helper()
	var appErr *common.AppError
	if !errors.As(err, &appErr) {
		t.Fatalf("returned %v, want an AppError", err)
	}
	if appErr.Status != http.StatusConflict || appErr.Code != common.ErrCodeConflict || !appErr.Retryable {
		t.Fatalf("returned %d %s retryable=%v, want a retryable 409 %s", appErr.Status, appErr.Code, appErr.Retryable, common.ErrCodeConflict)
	}
	if !errors.Is(appErr, spi.ErrTxAborted) {
		t.Fatalf("the cause is not attached: %v", appErr)
	}
}

// The handler's own If-Match compare, on a non-segmenting update.
func TestUpdateEntity_HandlerIfMatchInAbortedTx_Is409(t *testing.T) {
	hn := newTrackingHandler(t)
	hn.registerCountedTouchWorkflow(t, rollbackModel)
	id := hn.createEntityIn(t, rollbackModel, `{"name":"before"}`)
	current := hn.committedEntity(t, id).Meta.TransactionID
	hn.armed.casErr = txAbortedErr

	_, err := hn.h.UpdateEntity(hn.ctx, UpdateEntityInput{
		EntityID: id, Format: "JSON", Data: json.RawMessage(`{"name":"after"}`),
		Transition: "touch", IfMatch: current,
	})
	requireTxAbortedConflict(t, err)
}

// The collection update's handler compare: the item is not isolated as
// ENTITY_MODIFIED — the transaction later items would run in is gone — and the
// whole request answers 409.
func TestUpdateCollection_HandlerIfMatchInAbortedTx_AbortsBatch(t *testing.T) {
	hn := newTrackingHandler(t)
	touched := hn.registerCountedTouchWorkflow(t, rollbackModel)
	first := hn.createEntityIn(t, rollbackModel, `{"name":"first"}`)
	second := hn.createEntityIn(t, rollbackModel, `{"name":"before"}`)
	current := hn.committedEntity(t, first).Meta.TransactionID
	hn.armed.casErr = txAbortedErr

	res, err := hn.h.UpdateEntityCollection(hn.ctx, []UpdateCollectionItem{
		{EntityID: first, Transition: "touch", IfMatch: current, Payload: json.RawMessage(`{"name":"first-updated"}`)},
		{EntityID: second, Transition: "touch", Payload: json.RawMessage(`{"name":"after"}`)},
	})
	if err == nil {
		t.Fatalf("batch succeeded (%+v); an aborted transaction cannot carry later items", res)
	}
	requireTxAbortedConflict(t, err)
	if n := touched.Load(); n != 1 {
		t.Fatalf("the batch ran %d item(s), want 1; it went on past the aborted transaction", n)
	}
	if got := hn.committedName(t, second); got != "before" {
		t.Fatalf("item 1 committed as %q; an aborted batch must leave it untouched", got)
	}
}

// The engine's first-segment If-Match compare, before any commit or dispatch.
func TestUpdateEntity_FirstFlushIfMatchInAbortedTx_Is409(t *testing.T) {
	hn := newTrackingHandler(t)
	dispatched := hn.registerManualSegmentingWorkflow(t, rollbackSegmentModel)
	id := hn.createEntityIn(t, rollbackSegmentModel, `{"name":"seg"}`)
	current := hn.committedEntity(t, id).Meta.TransactionID
	hn.engineCAS.casErr = txAbortedErr
	hn.engineCAS.armed.Store(true)

	_, err := hn.h.UpdateEntity(hn.ctx, UpdateEntityInput{
		EntityID: id, Format: "JSON", Data: json.RawMessage(`{"name":"seg-updated"}`),
		Transition: "segment", IfMatch: current,
	})
	requireTxAbortedConflict(t, err)
	if n := dispatched.Load(); n != 0 {
		t.Fatalf("callout fired %d time(s); the first-segment compare was supposed to stop it", n)
	}
}
