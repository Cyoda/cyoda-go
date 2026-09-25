package grpc

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/contract"
)

// --- one try: handedOff is exactly "Member.Send returned nil" ---

// calloutDeadlineIn is a context whose own deadline, d from now, is the
// callout's: it ends with contract.ErrCalloutDeadline as its cause.
func calloutDeadlineIn(t *testing.T, d time.Duration) context.Context {
	t.Helper()
	ctx, cancel := context.WithDeadlineCause(testContext(), time.Now().Add(d), contract.ErrCalloutDeadline)
	t.Cleanup(cancel)
	return ctx
}

// cancelledAfter is the caller's context, cancelled d from now.
func cancelledAfter(t *testing.T, d time.Duration) context.Context {
	t.Helper()
	ctx, cancel := context.WithCancel(testContext())
	t.Cleanup(cancel)
	time.AfterFunc(d, cancel)
	return ctx
}

// leavesOnReceipt is the caller's context and a cnode script that cancels it
// when the cnode is given the work: Member.Send has returned nil by then.
func leavesOnReceipt(t *testing.T) (context.Context, script) {
	t.Helper()
	ctx, cancel := context.WithCancel(testContext())
	t.Cleanup(cancel)
	return ctx, func(*Member, string) { cancel() }
}

func alreadyCancelled() context.Context {
	ctx, cancel := context.WithCancel(testContext())
	cancel()
	return ctx
}

func TestTry_HandedOff_FalseOnEveryReturnBeforeTheSend(t *testing.T) {
	tests := []struct {
		name  string
		setup func(t *testing.T) (context.Context, *ProcessorDispatcher, *Member, Callout)
	}{
		{"the request cannot be built (Terminal)", func(t *testing.T) (context.Context, *ProcessorDispatcher, *Member, Callout) {
			reg := NewMemberRegistry()
			m, _ := attach(t, reg, "m-1", testTenantID, "x", answersAs("m-1"))
			call := rawCall(5 * time.Second)
			call.buildRequest = func(string) any { return make(chan int) } // json.Marshal refuses a channel
			return testContext(), newTestDispatcher(t, reg), m, call
		}},
		{"no auth context (Terminal)", func(t *testing.T) (context.Context, *ProcessorDispatcher, *Member, Callout) {
			reg := NewMemberRegistry()
			m, _ := attach(t, reg, "m-1", testTenantID, "x", answersAs("m-1"))
			return context.Background(), newTestDispatcher(t, reg), m, rawCall(5 * time.Second)
		}},
		{"the member is gone before the request is tracked", func(t *testing.T) (context.Context, *ProcessorDispatcher, *Member, Callout) {
			reg := NewMemberRegistry()
			m, _ := attach(t, reg, "m-1", testTenantID, "x", answersAs("m-1"))
			m.Evict(errors.New("gone"))
			return testContext(), newTestDispatcher(t, reg), m, rawCall(5 * time.Second)
		}},
		{"the member does not drain within the answer limit", func(t *testing.T) (context.Context, *ProcessorDispatcher, *Member, Callout) {
			d, m := newWedgedDispatcher(t)
			return testContext(), d, m, rawCall(50 * time.Millisecond)
		}},
		{"the callout deadline passes before the send", func(t *testing.T) (context.Context, *ProcessorDispatcher, *Member, Callout) {
			d, m := newWedgedDispatcher(t)
			return calloutDeadlineIn(t, 30*time.Millisecond), d, m, rawCall(5 * time.Second)
		}},
		{"the caller went away before the send", func(t *testing.T) (context.Context, *ProcessorDispatcher, *Member, Callout) {
			reg := NewMemberRegistry()
			m, _ := attach(t, reg, "m-1", testTenantID, "x", answersAs("m-1"))
			return alreadyCancelled(), newTestDispatcher(t, reg), m, rawCall(5 * time.Second)
		}},
		{"the caller goes away while the send waits", func(t *testing.T) (context.Context, *ProcessorDispatcher, *Member, Callout) {
			d, m := newWedgedDispatcher(t)
			return cancelledAfter(t, 30*time.Millisecond), d, m, rawCall(5 * time.Second)
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, d, m, call := tt.setup(t)
			_, failure, handedOff, ctxErr := d.dispatchCalloutToMember(ctx, m, call, "")
			if failure == nil && ctxErr == nil {
				t.Fatal("the try succeeded; want it to end before the send")
			}
			if handedOff {
				t.Errorf("handedOff = true, want false: Member.Send never returned nil (failure %v, ctxErr %v)", failure, ctxErr)
			}
		})
	}
}

func TestTry_HandedOff_TrueOnEveryReturnAfterTheSend(t *testing.T) {
	unreadable := func(t *testing.T) (context.Context, *ProcessorDispatcher, *Member, Callout) {
		reg := NewMemberRegistry()
		m, _ := attach(t, reg, "m-1", testTenantID, "x", answers(ProcessingResponse{Success: true}))
		call := rawCall(5 * time.Second)
		call.mapResponse = func(*ProcessingResponse) (CalloutResult, error) { return CalloutResult{}, errors.New("cannot read") }
		return testContext(), newTestDispatcher(t, reg), m, call
	}
	silent := func(ctx func(t *testing.T) context.Context, limit time.Duration) func(t *testing.T) (context.Context, *ProcessorDispatcher, *Member, Callout) {
		return func(t *testing.T) (context.Context, *ProcessorDispatcher, *Member, Callout) {
			reg := NewMemberRegistry()
			m, _ := attach(t, reg, "m-1", testTenantID, "x", nil) // takes the work, never answers
			return ctx(t), newTestDispatcher(t, reg), m, rawCall(limit)
		}
	}
	answering := func(s script) func(t *testing.T) (context.Context, *ProcessorDispatcher, *Member, Callout) {
		return func(t *testing.T) (context.Context, *ProcessorDispatcher, *Member, Callout) {
			reg := NewMemberRegistry()
			m, _ := attach(t, reg, "m-1", testTenantID, "x", s)
			return testContext(), newTestDispatcher(t, reg), m, rawCall(5 * time.Second)
		}
	}
	tests := []struct {
		name  string
		ok    bool
		setup func(t *testing.T) (context.Context, *ProcessorDispatcher, *Member, Callout)
	}{
		{"the cnode answers success", true, answering(answers(ProcessingResponse{Success: true}))},
		{"the cnode answers failure (MemberFailed)", false, answering(answers(ProcessingResponse{Success: false, Error: "declined"}))},
		{"the answer is unreadable at the stream (Terminal)", false, answering(answers(ProcessingResponse{Unreadable: "success was null"}))},
		{"the answer does not map (Terminal)", false, unreadable},
		{"the cnode drops after the send (NoAnswer)", false, answering(drops())},
		{"no answer within the answer limit (NoAnswer)", false, silent(func(*testing.T) context.Context { return testContext() }, 50*time.Millisecond)},
		{"the callout deadline passes while waiting (NoAnswer)", false, silent(func(t *testing.T) context.Context { return calloutDeadlineIn(t, 30*time.Millisecond) }, 5*time.Second)},
		{"the caller goes away while waiting", false, func(t *testing.T) (context.Context, *ProcessorDispatcher, *Member, Callout) {
			reg := NewMemberRegistry()
			ctx, leaves := leavesOnReceipt(t)
			m, _ := attach(t, reg, "m-1", testTenantID, "x", leaves)
			return ctx, newTestDispatcher(t, reg), m, rawCall(5 * time.Second)
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, d, m, call := tt.setup(t)
			_, failure, handedOff, ctxErr := d.dispatchCalloutToMember(ctx, m, call, "")
			if ok := failure == nil && ctxErr == nil; ok != tt.ok {
				t.Fatalf("succeeded = %v, want %v (failure %v, ctxErr %v)", ok, tt.ok, failure, ctxErr)
			}
			if !handedOff {
				t.Errorf("handedOff = false, want true: Member.Send returned nil (failure %v, ctxErr %v)", failure, ctxErr)
			}
		})
	}
}

// --- the local procedure: HandedOff is sticky across its tries ---

func TestRunLocal_HandedOff(t *testing.T) {
	badEntity := &spi.Entity{Meta: spi.EntityMeta{ID: "entity-bad", TenantID: testTenantID}, Data: []byte(`not json`)}
	noHandOff := contract.NoHandOff
	tests := []struct {
		name     string
		want     bool
		lastKind *contract.CalloutFailureKind
		setup    func(t *testing.T) (context.Context, *ProcessorDispatcher, Callout)
	}{
		{"no cnode", false, nil, func(t *testing.T) (context.Context, *ProcessorDispatcher, Callout) {
			return testContext(), newTestDispatcher(t, NewMemberRegistry()), processorCall("x", false, 5*time.Second)
		}},
		{"every cnode gone before its send", false, nil, func(t *testing.T) (context.Context, *ProcessorDispatcher, Callout) {
			reg := NewMemberRegistry()
			attach(t, reg, "m-1", testTenantID, "x", answersAs("m-1"))
			attach(t, reg, "m-2", testTenantID, "x", answersAs("m-2"))
			return testContext(), dispatcherGoneOnPick(t, reg, "m-1", "m-2"), processorCall("x", false, 5*time.Second)
		}},
		{"the caller went away before the first try", false, nil, func(t *testing.T) (context.Context, *ProcessorDispatcher, Callout) {
			reg := NewMemberRegistry()
			attach(t, reg, "m-1", testTenantID, "x", answersAs("m-1"))
			return alreadyCancelled(), newTestDispatcher(t, reg), processorCall("x", false, 5*time.Second)
		}},
		{"the request cannot be built (Terminal before the send)", false, nil, func(t *testing.T) (context.Context, *ProcessorDispatcher, Callout) {
			reg := NewMemberRegistry()
			attach(t, reg, "m-1", testTenantID, "x", answersAs("m-1"))
			processor := spi.ProcessorDefinition{Name: "my-proc", Config: spi.ProcessorConfig{AttachEntity: true, CalculationNodesTags: "x"}}
			call := armed(NewProcessorCallout(testTenantID, badEntity, processor, "wf1", "t1", "tx-1"), false, 5*time.Second)
			return testContext(), newTestDispatcher(t, reg), call
		}},
		{"a cnode answers", true, nil, func(t *testing.T) (context.Context, *ProcessorDispatcher, Callout) {
			reg := NewMemberRegistry()
			attach(t, reg, "m-1", testTenantID, "x", answersAs("m-1"))
			return testContext(), newTestDispatcher(t, reg), processorCall("x", false, 5*time.Second)
		}},
		{"the answer is unreadable (Terminal after the send)", true, nil, func(t *testing.T) (context.Context, *ProcessorDispatcher, Callout) {
			reg := NewMemberRegistry()
			attach(t, reg, "m-1", testTenantID, "x", answers(ProcessingResponse{Success: true, Payload: json.RawMessage(`not json`)}))
			return testContext(), newTestDispatcher(t, reg), processorCall("x", false, 5*time.Second)
		}},
		{"the caller goes away after the send", true, nil, func(t *testing.T) (context.Context, *ProcessorDispatcher, Callout) {
			reg := NewMemberRegistry()
			ctx, leaves := leavesOnReceipt(t)
			attach(t, reg, "m-1", testTenantID, "x", leaves)
			return ctx, newTestDispatcher(t, reg), processorCall("x", false, 5*time.Second)
		}},
		{"a try handed off, then a later try failed before its send", true, &noHandOff, func(t *testing.T) (context.Context, *ProcessorDispatcher, Callout) {
			reg := NewMemberRegistry()
			attach(t, reg, "m-1", testTenantID, "x", nil) // takes the work, never answers: NoAnswer
			attach(t, reg, "m-2", testTenantID, "x", answersAs("m-2"))
			// repeat-safe, so the NoAnswer on m-1 permits m-2, which is gone on pick
			return testContext(), dispatcherGoneOnPick(t, reg, "m-2"), processorCall("x", true, 50*time.Millisecond)
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, d, call := tt.setup(t)
			res := d.RunLocal(ctx, call, 4)
			if tt.lastKind != nil && (res.Failure == nil || res.Failure.Kind != *tt.lastKind) {
				t.Fatalf("last failure = %+v, want kind %v", res.Failure, *tt.lastKind)
			}
			if res.HandedOff != tt.want {
				t.Errorf("HandedOff = %v, want %v (failure %v, ctxErr %v, tries %d)", res.HandedOff, tt.want, res.Failure, res.CtxErr, res.TriesUsed)
			}
		})
	}
}
