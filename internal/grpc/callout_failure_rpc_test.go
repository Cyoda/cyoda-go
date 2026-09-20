package grpc

import (
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	cepb "github.com/cyoda-platform/cyoda-go/api/grpc/cloudevents"
)

// processorRPCWorkflowJSON is one automated transition with one SYNC
// externalized processor.
func processorRPCWorkflowJSON(wfName, procName, tag string, responseTimeoutMs int64) string {
	return fmt.Sprintf(`{
		"importMode": "REPLACE",
		"workflows": [{
			"version": "1.1", "name": %q, "initialState": "Open", "active": true,
			"states": {
				"Open": {"transitions": [{"name": "init", "next": "Closed", "manual": false,
					"processors": [{"type": "calculator", "name": %q, "executionMode": "SYNC",
						"config": {"attachEntity": true, "calculationNodesTags": %q, "responseTimeoutMs": %d}}]
				}]},
				"Closed": {}
			}
		}]
	}`, wfName, procName, tag, responseTimeoutMs)
}

// A workflow imported under a higher bound, run on a server whose bound was
// lowered since: the callout fails, naming the setting, and no cnode is asked.
func TestRPC_Processor_StoredTimeoutOverLoweredBound_Envelope(t *testing.T) {
	const modelName = "grpc-proc-over-bound"
	const tag = "over-bound-tag"
	svc, wfHandler, ctx := newTestEnvWithDispatchLimits(t, 500*time.Millisecond, time.Second)
	setupScheduledWorkflowRPCEnv(t, svc, wfHandler, ctx, modelName,
		processorRPCWorkflowJSON("over-bound-wf", "slow-proc", tag, 5000))

	sent := make(chan struct{}, 1)
	svc.registry.Register("m-1", testTenant, []string{tag}, func(*cepb.CloudEvent) error {
		sent <- struct{}{}
		return nil
	}, nil)

	typed := createScheduledEntity(t, svc, ctx, modelName)
	assertClientErrorEnvelope(t, typed, "WORKFLOW_FAILED")
	if !strings.Contains(typed.Error.Message, "CYODA_CALLOUT_RESPONSE_TIMEOUT_MAX_MS") {
		t.Errorf("message must name the setting, got %s", typed.Error.Message)
	}
	if typed.Error.Retryable != nil && *typed.Error.Retryable {
		t.Error("a Terminal failure is not retryable")
	}
	select {
	case <-sent:
		t.Fatal("no cnode may be asked")
	default:
	}
}

// A cnode that answers "I failed": one try, 400 WORKFLOW_FAILED, the message
// names the processor and carries the cnode's own text — and no longer the
// inner "processor dispatch failed:" segment. (Whether the envelope is marked
// retryable for verdict=true is decided where workflow errors are classified,
// outside this package; this pins code and message for all three verdicts.)
func TestRPC_ProcessorMemberFailed_EnvelopeCarriesTheMemberMessage(t *testing.T) {
	yes, no := true, false
	for name, tc := range map[string]struct {
		verdict   *bool
		retryable bool
	}{
		"verdict true":   {&yes, true},
		"verdict false":  {&no, false},
		"verdict absent": {nil, false},
	} {
		verdict := tc.verdict
		t.Run(name, func(t *testing.T) {
			const modelName = "grpc-proc-member-failed"
			const tag = "member-failed-tag"
			svc, wfHandler, ctx := newTestEnvWithDispatch(t)
			setupScheduledWorkflowRPCEnv(t, svc, wfHandler, ctx, modelName,
				processorRPCWorkflowJSON("member-failed-wf", "charge", tag, 5000))

			var asked atomic.Int32
			for _, id := range []string{"m-1", "m-2"} {
				svc.registry.Register(id, testTenant, []string{tag}, func(ce *cepb.CloudEvent) error {
					asked.Add(1)
					reqID, err := extractRequestID(ce)
					if err != nil {
						t.Errorf("extractRequestID: %v", err)
						return nil
					}
					svc.registry.Get(id).CompleteRequest(reqID, &ProcessingResponse{Success: false, Error: "card declined", Retryable: verdict})
					return nil
				}, nil)
			}

			typed := createScheduledEntity(t, svc, ctx, modelName)
			assertClientErrorEnvelope(t, typed, "WORKFLOW_FAILED")
			if !strings.Contains(typed.Error.Message, "processor charge failed: card declined") {
				t.Errorf("message = %s", typed.Error.Message)
			}
			if strings.Contains(typed.Error.Message, "dispatch failed") {
				t.Errorf("the inner \"dispatch failed:\" segment must be gone: %s", typed.Error.Message)
			}
			if got := asked.Load(); got != 1 {
				t.Errorf("%d cnodes were asked, want exactly one try", got)
			}
			if got := typed.Error.Retryable != nil && *typed.Error.Retryable; got != tc.retryable {
				t.Errorf("envelope retryable = %v, want %v: the cnode's verdict decides it", got, tc.retryable)
			}
		})
	}
}

// NoHandOff with one try: the try's own code reaches the envelope, retryable.
func TestRPC_Processor_NoHandOff_OwnCodeEnvelope(t *testing.T) {
	const modelName = "grpc-proc-no-handoff"
	const tag = "no-handoff-tag"
	svc, wfHandler, ctx := newTestEnvWithDispatch(t)
	setupScheduledWorkflowRPCEnv(t, svc, wfHandler, ctx, modelName,
		processorRPCWorkflowJSON("no-handoff-wf", "charge", tag, 100))

	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	wedged := svc.registry.Register("m-1", testTenant, []string{tag}, func(*cepb.CloudEvent) error { <-release; return nil }, nil)
	_ = wedged.Send(ctx, mustCE(t)) // park the writer: the next enqueue has nowhere to go

	typed := createScheduledEntity(t, svc, ctx, modelName)
	assertClientErrorEnvelope(t, typed, "DISPATCH_TIMEOUT")
	if !strings.Contains(typed.Error.Message, "member not draining") {
		t.Errorf("message = %s", typed.Error.Message)
	}
	if typed.Error.Retryable == nil || !*typed.Error.Retryable {
		t.Error("expected retryable=true")
	}
}

// NoAnswer on a processor that is not idempotent: stop, the try's own code,
// and a second matching cnode is never asked.
func TestRPC_Processor_NoAnswer_NotIdempotent_OwnCodeEnvelope(t *testing.T) {
	const modelName = "grpc-proc-no-answer"
	const tag = "no-answer-tag"
	svc, wfHandler, ctx := newTestEnvWithDispatch(t)
	setupScheduledWorkflowRPCEnv(t, svc, wfHandler, ctx, modelName,
		processorRPCWorkflowJSON("no-answer-wf", "charge", tag, 50))

	var asked atomic.Int32
	for _, id := range []string{"m-1", "m-2"} {
		svc.registry.Register(id, testTenant, []string{tag}, func(*cepb.CloudEvent) error {
			asked.Add(1)
			return nil // takes the work, never answers
		}, nil)
	}

	typed := createScheduledEntity(t, svc, ctx, modelName)
	assertClientErrorEnvelope(t, typed, "DISPATCH_TIMEOUT")
	if !strings.Contains(typed.Error.Message, "no response") {
		t.Errorf("message = %s", typed.Error.Message)
	}
	if typed.Error.Retryable == nil || !*typed.Error.Retryable {
		t.Error("expected retryable=true")
	}
	if got := asked.Load(); got != 1 {
		t.Errorf("%d cnodes were asked, want 1", got)
	}
}
