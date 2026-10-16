package workflow

import (
	"context"
	"errors"
	"fmt"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/contract"
)

// processorRecords drives one manual transition whose pipeline is procs, each
// dispatch answered by answer, and returns the STATE_PROCESS_RESULT events in
// the order they were recorded.
func processorRecords(t *testing.T, procs []spi.ProcessorDefinition, answer func(spi.ProcessorDefinition) error) []spi.StateMachineEvent {
	t.Helper()
	engine, factory := setupEngine(t)
	engine.extProc = &mockExternalProcessing{
		dispatchFunc: func(_ context.Context, _ *spi.Entity, proc spi.ProcessorDefinition, _, _, _ string) (*spi.Entity, error) {
			return nil, answer(proc)
		},
	}
	ctx := ctxWithTenant(testTenant)
	modelRef := spi.ModelRef{EntityName: "proc-record", ModelVersion: "1.0"}
	wf := spi.WorkflowDefinition{
		Version: "1.1", Name: "ProcRecordWF", InitialState: "INITIAL", Active: true,
		States: map[string]spi.StateDefinition{
			"INITIAL": {Transitions: []spi.TransitionDefinition{
				{Name: "FIRE", Next: "DONE", Manual: true, Processors: procs},
			}},
			"DONE": {},
		},
	}
	saveWorkflow(t, factory, ctx, modelRef, []spi.WorkflowDefinition{wf})

	_, _ = engine.Execute(ctx, makeEntity("e1", modelRef, map[string]any{}), "FIRE")

	auditStore, err := factory.StateMachineAuditStore(ctx)
	if err != nil {
		t.Fatalf("StateMachineAuditStore: %v", err)
	}
	events, err := auditStore.GetEvents(ctx, "e1")
	if err != nil {
		t.Fatalf("GetEvents: %v", err)
	}
	var out []spi.StateMachineEvent
	for _, ev := range events {
		if ev.EventType == spi.SMEventStateProcessResult {
			out = append(out, ev)
		}
	}
	return out
}

func TestProcessorRecord_Fields(t *testing.T) {
	procs := []spi.ProcessorDefinition{
		{Type: ProcessorTypeExternalized, Name: "first", ExecutionMode: ExecutionModeAsyncSameTx},
		{Type: ProcessorTypeExternalized, Name: "second"},
		{Type: ProcessorTypeExternalized, Name: "third", ExecutionMode: ExecutionModeSync},
	}
	records := processorRecords(t, procs, func(p spi.ProcessorDefinition) error {
		if p.Name == "third" {
			return errors.New("platform error")
		}
		return nil
	})
	want := []map[string]any{
		{"processorName": "first", "executionMode": ExecutionModeAsyncSameTx, "success": true},
		{"processorName": "second", "executionMode": ExecutionModeSync, "success": true},
		{"processorName": "third", "executionMode": ExecutionModeSync, "success": false},
	}
	if len(records) != len(want) {
		t.Fatalf("STATE_PROCESS_RESULT records = %d, want %d: %+v", len(records), len(want), records)
	}
	for i, w := range want {
		if got := records[i].Data; fmt.Sprint(got) != fmt.Sprint(w) {
			t.Errorf("record %d data = %v, want %v", i, got, w)
		}
	}
}

func TestProcessorRecord_FailReason(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string // "" = no failReason
	}{
		{"member text", &contract.CalloutFailure{Kind: contract.MemberFailed, Message: "card declined", MemberText: "card declined"}, "card declined"},
		{"member text behind wrapping", fmt.Errorf("dispatch: %w", &contract.CalloutFailure{Kind: contract.MemberFailed, Message: "card declined", MemberText: "card declined"}), "card declined"},
		{"member text after a no-answer try", &contract.CalloutFailure{Kind: contract.MemberFailed, Message: "card declined", MemberText: "card declined",
			Attempts: []contract.CalloutAttempt{{MemberID: "m1", Kind: contract.NoAnswer, Cause: "no answer"}, {MemberID: "m2", Kind: contract.MemberFailed, Cause: "card declined"}}}, "card declined"},
		{"member sent no text", &contract.CalloutFailure{Kind: contract.MemberFailed, Message: "processor returned failure"}, ""},
		{"callout timeout", &contract.CalloutFailure{Kind: contract.NoAnswer, Message: "processor dispatch timed out", Err: errors.New("processor dispatch timed out")}, ""},
		{"no member", &contract.CalloutFailure{Kind: contract.NoHandOff, Message: "no member", Err: contract.ErrNoMatchingMember}, ""},
		{"unreadable answer", &contract.CalloutFailure{Kind: contract.Terminal, Message: "unreadable answer", Err: errors.New("unreadable answer")}, ""},
		{"platform error", errors.New("pgx: connection reset"), ""},
		{"platform error holding member-like text", fmt.Errorf("member said: card declined"), ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			records := processorRecords(t,
				[]spi.ProcessorDefinition{{Type: ProcessorTypeExternalized, Name: "p", ExecutionMode: ExecutionModeSync}},
				func(spi.ProcessorDefinition) error { return tc.err })
			if len(records) != 1 {
				t.Fatalf("STATE_PROCESS_RESULT records = %d, want 1: %+v", len(records), records)
			}
			got, present := records[0].Data["failReason"]
			if tc.want == "" {
				if present {
					t.Errorf("failReason = %v, want absent", got)
				}
				return
			}
			if got != tc.want {
				t.Errorf("failReason = %v (present=%t), want %q", got, present, tc.want)
			}
			if success, _ := records[0].Data["success"].(bool); success {
				t.Error("success = true, want false")
			}
		})
	}
}

func TestProcessorFailure_OneRecord(t *testing.T) {
	records := processorRecords(t,
		[]spi.ProcessorDefinition{{Type: ProcessorTypeExternalized, Name: "p", ExecutionMode: ExecutionModeSync}},
		func(spi.ProcessorDefinition) error { return errProcessorRefused })
	if len(records) != 1 {
		t.Fatalf("STATE_PROCESS_RESULT records = %d, want 1: %+v", len(records), records)
	}
	if records[0].Data["processorName"] != "p" {
		t.Errorf("record = %v, want processor p's own record", records[0].Data)
	}
}

// The tracing decorator outside this package applies the same rule through
// contract.ProcessorExecutionMode; it must give this package's SYNC.
func TestProcessorExecutionMode_EmptyIsThisPackagesSync(t *testing.T) {
	if got := contract.ProcessorExecutionMode(""); got != ExecutionModeSync {
		t.Errorf("ProcessorExecutionMode(\"\") = %q, want %q", got, ExecutionModeSync)
	}
	for _, m := range []string{ExecutionModeSync, ExecutionModeAsyncSameTx, ExecutionModeAsyncNewTx, ExecutionModeCommitBeforeDispatch} {
		if got := contract.ProcessorExecutionMode(m); got != m {
			t.Errorf("ProcessorExecutionMode(%q) = %q, want it unchanged", m, got)
		}
	}
}
