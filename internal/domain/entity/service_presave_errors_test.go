package entity

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	spi "github.com/cyoda/cyoda-go-spi"
	"github.com/cyoda/cyoda-go/internal/common"
	wfengine "github.com/cyoda/cyoda-go/internal/domain/workflow"
)

// presaveCreateModel's workflow runs a SYNC processor on its automated transition
// out of the initial state, so a create dispatches one processor.
var presaveCreateModel = spi.ModelRef{EntityName: "PresaveCreate", ModelVersion: "1"}

// presaveDriverText is what the failing store says. It is never answered.
const presaveDriverText = "pq: relation entities is on fire"

// presaveHarness is the rollback harness with two workflows: rollbackModel has
// a MANUAL transition "go" carrying a SYNC processor, presaveCreateModel an
// automated one. The engine's Save — the save before the callout — fails with
// the error a case arms; the handler's own store stays healthy, so the
// failure is the engine's pre-callout save and nothing else.
func newPresaveHarness(t *testing.T) (*rollbackHarness, *atomic.Int32) {
	t.Helper()
	hn := newTrackingHandler(t)
	sync := spi.ProcessorDefinition{Type: wfengine.ProcessorTypeExternalized, Name: "reader", ExecutionMode: wfengine.ExecutionModeSync}
	hn.saveWorkflow(t, rollbackModel, spi.WorkflowDefinition{
		Version: "1.1", Name: "PresaveUpdateWF", InitialState: "A", Active: true,
		States: map[string]spi.StateDefinition{
			"A": {Transitions: []spi.TransitionDefinition{{Name: "go", Next: "B", Manual: true, Processors: []spi.ProcessorDefinition{sync}}}},
			"B": {},
		},
	})
	hn.registerModel(t, presaveCreateModel)
	hn.saveWorkflow(t, presaveCreateModel, spi.WorkflowDefinition{
		Version: "1.1", Name: "PresaveCreateWF", InitialState: "A", Active: true,
		States: map[string]spi.StateDefinition{
			"A": {Transitions: []spi.TransitionDefinition{{Name: "auto", Next: "B", Processors: []spi.ProcessorDefinition{sync}}}},
			"B": {},
		},
	})
	var dispatched atomic.Int32
	hn.proc.dispatchProcessor = func(context.Context, *spi.Entity, spi.ProcessorDefinition, string, string, string) (*spi.Entity, error) {
		dispatched.Add(1)
		return nil, nil
	}
	return hn, &dispatched
}

// presaveFailures are the three classes of failure the pre-callout save can
// meet, each with the answer it must get.
var presaveFailures = []struct {
	name  string
	err   error
	check func(t *testing.T, err error)
}{
	{
		name: "conflict",
		err:  fmt.Errorf("%s: %w", presaveDriverText, spi.ErrConflict),
		check: func(t *testing.T, err error) {
			t.Helper()
			requirePresaveAppError(t, err)
			requireConflict409(t, err)
		},
	},
	{
		name: "storage outage",
		err:  &markedStorageOutage{detail: presaveDriverText},
		check: func(t *testing.T, err error) {
			t.Helper()
			appErr := requirePresaveAppError(t, err)
			if appErr.Status != http.StatusServiceUnavailable || appErr.Code != common.ErrCodeStorageUnavailable || !appErr.Retryable {
				t.Fatalf("got %d %s retryable=%v, want a retryable 503 %s", appErr.Status, appErr.Code, appErr.Retryable, common.ErrCodeStorageUnavailable)
			}
		},
	},
	{
		name: "other failure",
		err:  errors.New(presaveDriverText),
		check: func(t *testing.T, err error) {
			t.Helper()
			appErr := requirePresaveAppError(t, err)
			if appErr.Status != http.StatusInternalServerError || appErr.Code != common.ErrCodeServerError || appErr.Level != common.LevelInternal {
				t.Fatalf("got %d %s level=%v, want an internal 500 %s (ticketed)", appErr.Status, appErr.Code, appErr.Level, common.ErrCodeServerError)
			}
		},
	},
}

func requirePresaveAppError(t *testing.T, err error) *common.AppError {
	t.Helper()
	var appErr *common.AppError
	if !errors.As(err, &appErr) {
		t.Fatalf("err = %v (%T), want *common.AppError", err, err)
	}
	if appErr.Code == common.ErrCodeWorkflowFailed || appErr.Code == common.ErrCodeEntityModified {
		t.Fatalf("got %d %s: a failed save before a callout is not the workflow's or the caller's", appErr.Status, appErr.Code)
	}
	if strings.Contains(appErr.Message, presaveDriverText) {
		t.Fatalf("the answer carries the store's text: %q", appErr.Message)
	}
	return appErr
}

// TestPreCalloutSaveFailure_Classification: a failure of the save the engine
// makes before a callout is the transaction's, not the workflow's and not the
// caller's precondition. A conflict is a retryable 409 CONFLICT, a storage
// outage a 503, anything else a ticketed 500 — on every write door, and
// never a 400 WORKFLOW_FAILED carrying the store's text. Nothing is dispatched.
func TestPreCalloutSaveFailure_Classification(t *testing.T) {
	doors := []struct {
		name  string
		drive func(t *testing.T, hn *rollbackHarness, id string) error
	}{
		{"create", func(t *testing.T, hn *rollbackHarness, _ string) error {
			_, err := hn.h.CreateEntity(hn.ctx, CreateEntityInput{
				EntityName: presaveCreateModel.EntityName, ModelVersion: presaveCreateModel.ModelVersion,
				Format: "JSON", Data: json.RawMessage(`{"name":"new"}`),
			})
			return err
		}},
		{"update", func(t *testing.T, hn *rollbackHarness, id string) error {
			_, err := hn.h.UpdateEntity(hn.ctx, UpdateEntityInput{
				EntityID: id, Format: "JSON", Data: json.RawMessage(`{"name":"after"}`), Transition: "go",
			})
			return err
		}},
		{"update collection", func(t *testing.T, hn *rollbackHarness, id string) error {
			_, err := hn.h.UpdateEntityCollection(hn.ctx, []UpdateCollectionItem{{
				EntityID: id, Payload: json.RawMessage(`{"name":"after"}`), Transition: "go",
			}})
			return err
		}},
	}
	for _, door := range doors {
		for _, f := range presaveFailures {
			t.Run(door.name+"/"+f.name, func(t *testing.T) {
				hn, dispatched := newPresaveHarness(t)
				id := hn.createEntityIn(t, rollbackModel, `{"name":"before"}`)
				hn.engineCAS.saveErr = f.err

				f.check(t, door.drive(t, hn, id))
				if n := dispatched.Load(); n != 0 {
					t.Fatalf("%d processor(s) dispatched after the save before the callout failed", n)
				}
				if got := hn.committedName(t, id); got != "before" {
					t.Fatalf("committed name = %q, want %q: the failed request committed", got, "before")
				}
			})
		}
	}
}

// TestPreCalloutSaveConflict_CollectionIfMatch_NotIsolated: in a collection
// update whose items carry If-Match, an ENTITY_MODIFIED item is isolated and
// the loop carries on. A conflict from the save before a callout is not that:
// the transaction cannot commit, so the whole request fails with a retryable
// 409 and the loop stops. Only the first item's save fails, so a loop that
// carried on would dispatch the second item's processor.
func TestPreCalloutSaveConflict_CollectionIfMatch_NotIsolated(t *testing.T) {
	hn, dispatched := newPresaveHarness(t)
	first := hn.createEntityIn(t, rollbackModel, `{"name":"before"}`)
	second := hn.createEntityIn(t, rollbackModel, `{"name":"before"}`)
	hn.engineCAS.saveErr = fmt.Errorf("%s: %w", presaveDriverText, spi.ErrConflict)
	// One-shot: the engine's first Save fails, every later one succeeds.
	// onSave runs before saveErr is read, so clearing it on the second call
	// lets that call through.
	saves := 0
	hn.engineCAS.onSave = func(string) {
		saves++
		if saves > 1 {
			hn.engineCAS.saveErr = nil
		}
	}

	res, err := hn.h.UpdateEntityCollection(hn.ctx, []UpdateCollectionItem{
		{EntityID: first, Payload: json.RawMessage(`{"name":"after"}`), Transition: "go", IfMatch: hn.committedEntity(t, first).Meta.TransactionID},
		{EntityID: second, Payload: json.RawMessage(`{"name":"after"}`), Transition: "go", IfMatch: hn.committedEntity(t, second).Meta.TransactionID},
	})
	if err == nil {
		t.Fatalf("err = nil, result %+v; want the request to fail with 409", res)
	}
	requirePresaveAppError(t, err)
	requireConflict409(t, err)
	if n := dispatched.Load(); n != 0 {
		t.Fatalf("%d processor(s) dispatched: the loop carried on past the failed save", n)
	}
	if saves != 1 {
		t.Fatalf("the engine saved %d time(s); want only the first item's failed save", saves)
	}
	for _, id := range []string{first, second} {
		if got := hn.committedName(t, id); got != "before" {
			t.Fatalf("entity %s committed name = %q, want %q", id, got, "before")
		}
	}
}
