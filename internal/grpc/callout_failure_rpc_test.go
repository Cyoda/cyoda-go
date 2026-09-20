package grpc

import (
	"fmt"
	"strings"
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
