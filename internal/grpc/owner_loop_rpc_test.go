package grpc

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	cepb "github.com/cyoda-platform/cyoda-go/api/grpc/cloudevents"
	events "github.com/cyoda-platform/cyoda-go/api/grpc/events"
	"github.com/cyoda-platform/cyoda-go/internal/common"
)

// createWithTimeout is createScheduledEntity with a transactionTimeoutMs.
func createWithTimeout(t *testing.T, svc *CloudEventsServiceImpl, ctx context.Context, modelName string, timeoutMs int) events.EntityTransactionResponseJson {
	t.Helper()
	createCE := makeCE(EntityCreateRequest, map[string]any{
		"id":                   "create-1",
		"dataFormat":           "JSON",
		"transactionTimeoutMs": timeoutMs,
		"payload": map[string]any{
			"model": map[string]any{"name": modelName, "version": 1},
			"data":  map[string]any{"name": "Test Order", "amount": 100, "status": "draft"},
		},
	})
	resp, err := svc.EntityManage(ctx, createCE)
	if err != nil {
		t.Fatalf("unexpected gRPC transport error: %v", err)
	}
	var typed events.EntityTransactionResponseJson
	validateResponse(t, resp, &typed)
	return typed
}

// Every try used: a function is repeat-safe, so both silent cnodes are tried,
// and the envelope carries CALLOUT_FAILED with the tries listed.
func TestRPC_Function_EveryTryUsed_CalloutFailedEnvelope(t *testing.T) {
	const modelName = "grpc-owner-every-try-used"
	const tag = "every-try-used-tag"
	svc, wfHandler, ctx := newTestEnvWithOwner(t, 30*time.Second, 60*time.Second, OwnerTestConfig{FixedNumRetries: 1})
	setupScheduledWorkflowRPCEnv(t, svc, wfHandler, ctx, modelName,
		scheduleFunctionRPCWorkflowJSON("every-try-used-wf", schedFnConfigJSON("calcFire", tag, 50)))

	var asked atomic.Int32
	for _, id := range []string{"m-1", "m-2"} {
		svc.registry.Register(id, testTenant, []string{tag}, func(*cepb.CloudEvent) error {
			asked.Add(1)
			return nil // takes the work, never answers
		}, nil)
	}

	typed := createScheduledEntity(t, svc, ctx, modelName)

	assertClientErrorEnvelope(t, typed, "CALLOUT_FAILED")
	if !strings.HasPrefix(typed.Error.Message, "CALLOUT_FAILED: the callout could not be completed, got 2 failures: [member<") {
		t.Errorf("message = %s", typed.Error.Message)
	}
	for _, want := range []string{"member<m-1>: DISPATCH_TIMEOUT:", "member<m-2>: DISPATCH_TIMEOUT:"} {
		if !strings.Contains(typed.Error.Message, want) {
			t.Errorf("message = %s, want it to contain %q", typed.Error.Message, want)
		}
	}
	if typed.Error.Retryable == nil || !*typed.Error.Retryable {
		t.Error("expected retryable=true")
	}
	if got := asked.Load(); got != 2 {
		t.Errorf("%d tries were made, want 2", got)
	}
}

func TestRPC_NoCnodeWithinThePatience_NoComputeMemberEnvelope(t *testing.T) {
	tests := []struct {
		name     string
		patience time.Duration
		atLeast  time.Duration
		atMost   time.Duration
	}{
		{"patience 0: at once", 0, 0, 2 * time.Second},
		{"patience 200ms: after the patience", 200 * time.Millisecond, 200 * time.Millisecond, 5 * time.Second},
	}
	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			modelName := "grpc-owner-no-cnode-" + string(rune('a'+i))
			svc, wfHandler, ctx := newTestEnvWithOwner(t, 30*time.Second, 60*time.Second, OwnerTestConfig{FixedNumRetries: 3, Patience: tt.patience})
			setupScheduledWorkflowRPCEnv(t, svc, wfHandler, ctx, modelName,
				scheduleFunctionRPCWorkflowJSON("no-cnode-wf", schedFnConfigJSON("calcFire", "nobody-has-this-tag", 0)))
			start := time.Now()

			typed := createScheduledEntity(t, svc, ctx, modelName)

			elapsed := time.Since(start)
			assertClientErrorEnvelope(t, typed, "NO_COMPUTE_MEMBER_FOR_TAG")
			if typed.Error.Retryable == nil || !*typed.Error.Retryable {
				t.Error("expected retryable=true")
			}
			if elapsed < tt.atLeast || elapsed > tt.atMost {
				t.Errorf("answered after %v, want between %v and %v", elapsed, tt.atLeast, tt.atMost)
			}
		})
	}
}

// The client's own limit ends a callout at once — from a wait as from a try —
// and the envelope is the 408 every other timed-out write gets.
func TestRPC_ClientTimeoutDuringAWaitAndDuringATry_TransactionTimeoutEnvelope(t *testing.T) {
	for _, during := range []string{"a wait", "a try"} {
		t.Run("during "+during, func(t *testing.T) {
			modelName := "grpc-owner-client-timeout-" + strings.ReplaceAll(during, " ", "-")
			const tag = "client-timeout-tag"
			svc, wfHandler, ctx := newTestEnvWithOwner(t, 30*time.Second, 60*time.Second, OwnerTestConfig{FixedNumRetries: 3, Patience: 30 * time.Second})
			setupScheduledWorkflowRPCEnv(t, svc, wfHandler, ctx, modelName,
				scheduleFunctionRPCWorkflowJSON("client-timeout-wf", schedFnConfigJSON("calcFire", tag, 20000)))
			if during == "a try" {
				svc.registry.Register("m-1", testTenant, []string{tag}, func(*cepb.CloudEvent) error { return nil }, nil)
			}
			start := time.Now()

			typed := createWithTimeout(t, svc, ctx, modelName, 100)

			if elapsed := time.Since(start); elapsed > 5*time.Second {
				t.Errorf("answered after %v: the client's limit must end the callout at once", elapsed)
			}
			if typed.Success || typed.Error == nil {
				t.Fatal("expected the operation to fail")
			}
			if typed.Error.Code != "CLIENT_ERROR" || !strings.HasPrefix(typed.Error.Message, common.ErrCodeTransactionTimeout+":") {
				t.Errorf("envelope = %s / %s, want CLIENT_ERROR with prefix %s", typed.Error.Code, typed.Error.Message, common.ErrCodeTransactionTimeout)
			}
			if typed.Error.Retryable == nil || !*typed.Error.Retryable {
				t.Error("expected retryable=true")
			}
		})
	}
}

// A client that went away is not a domain failure: a ticketed SERVER_ERROR, no
// callout detail, and no waiting out the patience.
func TestRPC_ClientGoesAwayDuringAWait_TicketedServerErrorEnvelope(t *testing.T) {
	const modelName = "grpc-owner-client-gone"
	svc, wfHandler, ctx := newTestEnvWithOwner(t, 30*time.Second, 60*time.Second, OwnerTestConfig{FixedNumRetries: 3, Patience: 30 * time.Second})
	setupScheduledWorkflowRPCEnv(t, svc, wfHandler, ctx, modelName,
		scheduleFunctionRPCWorkflowJSON("client-gone-wf", schedFnConfigJSON("calcFire", "nobody-has-this-tag", 0)))
	ctx, cancel := context.WithCancel(ctx)
	time.AfterFunc(100*time.Millisecond, cancel)
	start := time.Now()

	typed := createScheduledEntity(t, svc, ctx, modelName)

	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("answered after %v: a cancelled caller ends the wait at once", elapsed)
	}
	if typed.Success || typed.Error == nil || typed.Error.Code != "SERVER_ERROR" || !strings.Contains(typed.Error.Message, "ticket") {
		t.Errorf("envelope = %+v, want a ticketed SERVER_ERROR", typed.Error)
	}
	if typed.Error != nil && strings.Contains(typed.Error.Message, "NO_COMPUTE_MEMBER") {
		t.Errorf("message = %s: a cancelled request is not reported as a missing compute member", typed.Error.Message)
	}
}
