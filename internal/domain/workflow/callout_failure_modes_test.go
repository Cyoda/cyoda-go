package workflow

import (
	"context"
	"errors"
	"net/http"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/contract"
	"github.com/cyoda-platform/cyoda-go/internal/testing/localproc"
	"github.com/cyoda-platform/cyoda-go/plugins/memory"
)

func memberFailed(message string, verdict bool) *contract.CalloutFailure {
	return &contract.CalloutFailure{Kind: contract.MemberFailed, Message: message, Retryable: &verdict}
}

func noAnswerFailure() *contract.CalloutFailure {
	appErr := common.Operational(http.StatusServiceUnavailable, common.ErrCodeDispatchTimeout,
		"processor dispatch timed out after 100ms: no response").AsRetryable()
	return &contract.CalloutFailure{Kind: contract.NoAnswer, Code: appErr.Code, Message: appErr.Message, Err: appErr}
}

// calloutModeEngine runs one transition with one externalized processor in the
// given mode, whose callout fails with failure.
func calloutModeEngine(t *testing.T, mode string, startNewTx *bool, failure error) (result *EngineResult, execErr error, cnt *segCounter, entity *spi.Entity) {
	t.Helper()
	factory := memory.NewStoreFactory()
	t.Cleanup(func() { factory.Close() })
	uuids := common.NewTestUUIDGenerator()
	cnt = &segCounter{}
	txMgr := &countingTxManager{inner: factory.NewTransactionManager(uuids), c: cnt}
	mock := &mockExternalProcessing{
		dispatchFunc: func(context.Context, *spi.Entity, spi.ProcessorDefinition, string, string, string) (*spi.Entity, error) {
			return nil, failure
		},
	}
	engine := NewEngine(factory, uuids, txMgr, WithExternalProcessing(mock))

	ctx := ctxWithTenant(testTenant)
	modelRef := spi.ModelRef{EntityName: "callout-mode-" + mode, ModelVersion: "1.0"}
	wf := spi.WorkflowDefinition{
		Version: "1.1", Name: "CalloutModeWF", InitialState: "OPEN", Active: true,
		States: map[string]spi.StateDefinition{
			"OPEN": {Transitions: []spi.TransitionDefinition{
				{Name: "GO", Next: "DONE", Manual: false,
					Processors: []spi.ProcessorDefinition{
						{Type: ProcessorTypeExternalized, Name: "charge", ExecutionMode: mode,
							Config: spi.ProcessorConfig{StartNewTxOnDispatch: startNewTx}},
					}},
			}},
			"DONE": {},
		},
	}
	saveWorkflow(t, factory, ctx, modelRef, []spi.WorkflowDefinition{wf})

	entryTxID, txCtx, err := txMgr.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	t.Cleanup(func() { _ = txMgr.Rollback(txCtx, entryTxID) })
	*cnt = segCounter{}

	entity = &spi.Entity{
		Meta: spi.EntityMeta{ID: "callout-mode-1", TenantID: testTenant, ModelRef: modelRef, TransactionID: entryTxID},
		Data: []byte(`{"x":1}`),
	}
	result, execErr = engine.Execute(txCtx, entity, "")
	return result, execErr, cnt, entity
}

// SYNC and ASYNC_SAME_TX: the operation fails, the engine's wrap names the
// processor, and the failure — kind, the cnode's text, its verdict — is still
// there for the classifier behind the wrap.
func TestCalloutFailure_SyncModes_FailTheOperationAndKeepTheFailure(t *testing.T) {
	for _, mode := range []string{ExecutionModeSync, ExecutionModeAsyncSameTx} {
		t.Run(mode, func(t *testing.T) {
			_, err, cnt, _ := calloutModeEngine(t, mode, nil, memberFailed("card declined", true))
			if err == nil {
				t.Fatal("expected the operation to fail")
			}
			if got, want := err.Error(), "processor charge failed: card declined"; got != want {
				t.Errorf("error = %q, want %q", got, want)
			}
			var failure *contract.CalloutFailure
			if !errors.As(err, &failure) || failure.Kind != contract.MemberFailed || failure.Retryable == nil || !*failure.Retryable {
				t.Errorf("failure = %+v, want MemberFailed with verdict true behind the wrap", failure)
			}
			if cnt.commits != 0 {
				t.Errorf("engine committed %d times, want 0: nothing of a failed operation is committed", cnt.commits)
			}
		})
	}
}

// ASYNC_NEW_TX: the operation continues and nothing is reported — not even a
// retryable verdict.
func TestCalloutFailure_AsyncNewTx_OperationContinuesNothingReported(t *testing.T) {
	for name, failure := range map[string]error{
		"member failed, verdict true": memberFailed("card declined", true),
		"no answer":                   noAnswerFailure(),
	} {
		t.Run(name, func(t *testing.T) {
			result, err, _, entity := calloutModeEngine(t, ExecutionModeAsyncNewTx, nil, failure)
			if err != nil {
				t.Fatalf("Execute = %v, want success: an ASYNC_NEW_TX callout failure does not fail the operation", err)
			}
			if !result.Success || entity.Meta.State != "DONE" {
				t.Errorf("success = %v state = %q, want true and DONE", result.Success, entity.Meta.State)
			}
		})
	}
}

// COMMIT_BEFORE_DISPATCH, both variants: the operation fails with the try's own
// error, and TX_pre stays committed.
func TestCalloutFailure_CommitBeforeDispatch_FailsAndLeavesTxPreCommitted(t *testing.T) {
	yes, no := true, false
	for name, startNewTx := range map[string]*bool{"new tx on dispatch": &yes, "no tx on dispatch": &no} {
		t.Run(name, func(t *testing.T) {
			_, err, cnt, _ := calloutModeEngine(t, ExecutionModeCommitBeforeDispatch, startNewTx, noAnswerFailure())
			var appErr *common.AppError
			if !errors.As(err, &appErr) || appErr.Code != common.ErrCodeDispatchTimeout || !appErr.Retryable {
				t.Fatalf("error = %v, want the try's own retryable DISPATCH_TIMEOUT", err)
			}
			if cnt.commits != 1 {
				t.Errorf("engine commits = %d, want 1: TX_pre is committed before the callout and stays committed", cnt.commits)
			}
		})
	}
}

// The arming function's failure names the function, as a processor's names the
// processor, and keeps the cnode's verdict.
func TestReconcile_FunctionMemberFailed_NamesTheFunctionAndKeepsTheVerdict(t *testing.T) {
	const nowMs = int64(1_700_000_000_000)

	lp := localproc.New()
	lp.RegisterFunction("calcFire", func(context.Context, *spi.Entity, spi.ScheduleFunction) (contract.FunctionResult, error) {
		return contract.FunctionResult{}, memberFailed("rates service is down", true)
	})
	engine, factory := setupEngineWithClockAndExtProc(t, nowMs, lp)
	ctx := ctxWithTenant(testTenant)
	modelRef := spi.ModelRef{EntityName: "fn-member-failed-order", ModelVersion: "1.0"}
	saveWorkflow(t, factory, ctx, modelRef, []spi.WorkflowDefinition{scheduleFunctionWorkflow("FnMemberFailedWF", "calcFire")})

	_, err := engine.Execute(ctx, makeEntity("fn-member-failed-e1", modelRef, map[string]any{}), "")
	if err == nil {
		t.Fatal("expected the write to fail")
	}
	if got, want := err.Error(), "schedule function calcFire failed: rates service is down"; got != want {
		t.Errorf("error = %q, want %q", got, want)
	}
	var failure *contract.CalloutFailure
	if !errors.As(err, &failure) || failure.Retryable == nil || !*failure.Retryable {
		t.Errorf("failure = %+v, want the verdict kept behind the wrap", failure)
	}
}
