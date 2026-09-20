package grpc

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	cepb "github.com/cyoda-platform/cyoda-go/api/grpc/cloudevents"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/contract"
)

// kindOf runs one try and returns what it produced.
func kindOf(d *ProcessorDispatcher, ctx context.Context, member *Member, call Callout) (*contract.CalloutFailure, error) {
	_, failure, ctxErr := d.dispatchCalloutToMember(ctx, member, call, "")
	return failure, ctxErr
}

func rawCall(limit time.Duration) Callout {
	return Callout{
		Kind: ProcessorCallout, Name: "p", TenantID: testTenantID, RequestID: "r1", AnswerLimit: limit, OwnerNodeID: "node-test",
		eventType:    EntityProcessorCalculationRequest,
		buildRequest: func(id string) any { return map[string]any{"requestId": id} },
		mapResponse:  func(*ProcessingResponse) (CalloutResult, error) { return CalloutResult{}, nil },
	}
}

func assertKindAndCode(t *testing.T, failure *contract.CalloutFailure, ctxErr error, wantKind contract.CalloutFailureKind, wantCode string) {
	t.Helper()
	if ctxErr != nil {
		t.Fatalf("unexpected ctx error: %v", ctxErr)
	}
	if failure == nil {
		t.Fatal("expected a failure")
	}
	if failure.Kind != wantKind {
		t.Errorf("Kind = %v, want %v", failure.Kind, wantKind)
	}
	if failure.Code != wantCode {
		t.Errorf("Code = %q, want %q", failure.Code, wantCode)
	}
	var appErr *common.AppError
	if wantCode == "" {
		return
	}
	if !errors.As(failure, &appErr) || appErr.Code != wantCode || appErr.Status != 503 || !appErr.Retryable {
		t.Errorf("carried AppError = %+v, want retryable 503 %s", appErr, wantCode)
	}
	if failure.Message != appErr.Message {
		t.Errorf("Message = %q, want the AppError's %q", failure.Message, appErr.Message)
	}
}

func TestTryKind_TrackRequestOnAnEvictedMember_IsNoHandOff(t *testing.T) {
	d, registry, memberID, _ := setupTestDispatcher(t)
	member := registry.Get(memberID)
	registry.Unregister(member)
	failure, ctxErr := kindOf(d, testContext(), member, rawCall(30*time.Second))
	assertKindAndCode(t, failure, ctxErr, contract.NoHandOff, common.ErrCodeComputeMemberDisconnected)
}

func TestTryKind_EvictedDuringEnqueue_IsNoHandOff(t *testing.T) {
	d, member := newWedgedDispatcher(t)
	go func() {
		time.Sleep(30 * time.Millisecond)
		member.Evict(errors.New("member disconnected"))
	}()
	failure, ctxErr := kindOf(d, testContext(), member, rawCall(30*time.Second))
	assertKindAndCode(t, failure, ctxErr, contract.NoHandOff, common.ErrCodeComputeMemberDisconnected)
}

func TestTryKind_EnqueueDeadline_IsNoHandOff(t *testing.T) {
	d, member := newWedgedDispatcher(t)
	failure, ctxErr := kindOf(d, testContext(), member, rawCall(100*time.Millisecond))
	assertKindAndCode(t, failure, ctxErr, contract.NoHandOff, common.ErrCodeDispatchTimeout)
	if !strings.Contains(failure.Message, "processor dispatch timed out after 100ms: member not draining") {
		t.Errorf("message = %q", failure.Message)
	}
}

func TestTryKind_CallerCancelledDuringEnqueue_IsCtxErrUnchanged(t *testing.T) {
	d, member := newWedgedDispatcher(t)
	ctx, cancel := context.WithCancel(testContext())
	go func() { time.Sleep(30 * time.Millisecond); cancel() }()
	failure, ctxErr := kindOf(d, ctx, member, rawCall(30*time.Second))
	if failure != nil || ctxErr != context.Canceled {
		t.Fatalf("got (%v, %v), want (nil, context.Canceled)", failure, ctxErr)
	}
}

func TestTryKind_DisconnectedWhileWaiting_IsNoAnswer(t *testing.T) {
	d, registry, memberID, sentCh := setupTestDispatcher(t)
	member := registry.Get(memberID)
	go func() {
		<-sentCh
		member.Evict(errors.New("stream dropped"))
	}()
	failure, ctxErr := kindOf(d, testContext(), member, rawCall(5*time.Second))
	assertKindAndCode(t, failure, ctxErr, contract.NoAnswer, common.ErrCodeComputeMemberDisconnected)
}

func TestTryKind_AnswerLimit_IsNoAnswer(t *testing.T) {
	d, registry, memberID, _ := setupTestDispatcher(t)
	failure, ctxErr := kindOf(d, testContext(), registry.Get(memberID), rawCall(50*time.Millisecond))
	assertKindAndCode(t, failure, ctxErr, contract.NoAnswer, common.ErrCodeDispatchTimeout)
	if !strings.Contains(failure.Message, "processor dispatch timed out after 50ms: no response") {
		t.Errorf("message = %q", failure.Message)
	}
}

func TestTryKind_CallerCancelledWhileWaiting_IsCtxErrUnchanged(t *testing.T) {
	d, registry, memberID, sentCh := setupTestDispatcher(t)
	ctx, cancel := context.WithCancel(testContext())
	go func() { <-sentCh; cancel() }()
	failure, ctxErr := kindOf(d, ctx, registry.Get(memberID), rawCall(5*time.Second))
	if failure != nil || ctxErr != context.Canceled {
		t.Fatalf("got (%v, %v), want (nil, context.Canceled)", failure, ctxErr)
	}
}

func TestTryKind_MemberAnsweredFailure_IsMemberFailedWithItsVerdict(t *testing.T) {
	yes, no := true, false
	tests := []struct {
		name      string
		memberErr string
		verdict   *bool
		wantMsg   string
	}{
		{"verdict true", "downstream busy", &yes, "downstream busy"},
		{"verdict false", "card declined", &no, "card declined"},
		{"verdict absent", "card declined", nil, "card declined"},
		{"no message", "", nil, "processor returned failure"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d, registry, memberID, sentCh := setupTestDispatcher(t)
			member := registry.Get(memberID)
			go func() {
				<-sentCh
				member.CompleteRequest("r1", &ProcessingResponse{Success: false, Error: tt.memberErr, Retryable: tt.verdict})
			}()
			failure, ctxErr := kindOf(d, testContext(), member, rawCall(5*time.Second))
			assertKindAndCode(t, failure, ctxErr, contract.MemberFailed, "")
			if failure.Message != tt.wantMsg || failure.Error() != tt.wantMsg {
				t.Errorf("Message/Error = %q/%q, want %q", failure.Message, failure.Error(), tt.wantMsg)
			}
			if (failure.Retryable == nil) != (tt.verdict == nil) || (tt.verdict != nil && *failure.Retryable != *tt.verdict) {
				t.Errorf("Retryable = %v, want %v", failure.Retryable, tt.verdict)
			}
			var appErr *common.AppError
			if errors.As(failure, &appErr) {
				t.Errorf("MemberFailed must carry no AppError, got %v", appErr)
			}
		})
	}
}

func TestTryKind_CloudEventBuild_IsTerminal(t *testing.T) {
	d, registry, memberID, sentCh := setupTestDispatcher(t)
	call := rawCall(5 * time.Second)
	call.buildRequest = func(string) any { return make(chan int) } // json cannot marshal a channel
	failure, ctxErr := kindOf(d, testContext(), registry.Get(memberID), call)
	if ctxErr != nil {
		t.Fatalf("unexpected ctx error: %v", ctxErr)
	}
	if failure == nil || failure.Kind != contract.Terminal {
		t.Fatalf("failure = %+v, want Terminal", failure)
	}
	// This pnode failed to build its own request: internal, sanitized. The
	// real error (which could otherwise name a Go type from the callout's own
	// construction) must never reach Message/Error() — only a ticketed 500
	// carries it, behind the AppError's cause.
	const wantMsg = "SERVER_ERROR: internal error"
	if failure.Code != common.ErrCodeServerError || failure.Message != wantMsg || failure.Error() != wantMsg {
		t.Errorf("Code/Message/Error = %q/%q/%q, want %q/%q/%q",
			failure.Code, failure.Message, failure.Error(), common.ErrCodeServerError, wantMsg, wantMsg)
	}
	var appErr *common.AppError
	if !errors.As(failure, &appErr) || appErr.Level != common.LevelInternal {
		t.Errorf("carried AppError = %+v, want a LevelInternal error", appErr)
	}
	select {
	case <-sentCh:
		t.Fatal("nothing may be sent")
	default:
	}
}

func TestTryKind_AuthContext_IsTerminalAndNamesNoPrincipal(t *testing.T) {
	d, registry, memberID, sentCh := setupTestDispatcher(t)
	ctx := spi.WithUserContext(context.Background(), &spi.UserContext{
		UserID: "user-secret-id", Tenant: spi.Tenant{ID: testTenantID}, // Kind unset
	})
	failure, ctxErr := kindOf(d, ctx, registry.Get(memberID), rawCall(5*time.Second))
	assertKindAndCode(t, failure, ctxErr, contract.Terminal, "")
	if !errors.Is(failure, contract.ErrAuthContextUnavailable) {
		t.Error("the sentinel the classifier maps to a ticketed 500 must survive")
	}
	if strings.Contains(failure.Message, "user-secret-id") {
		t.Errorf("Message is client-visible (it is copied into attempts) and must not name the principal: %q", failure.Message)
	}
	select {
	case <-sentCh:
		t.Fatal("nothing may be sent")
	default:
	}
}

func TestTryKind_ResponsePayloadUnmarshal_IsMemberFailed(t *testing.T) {
	registry := NewMemberRegistry()
	m := registry.Register("m-1", testTenantID, []string{"python"}, func(ce *cepb.CloudEvent) error {
		reqID, err := extractRequestID(ce)
		if err != nil {
			t.Errorf("extractRequestID: %v", err)
			return nil
		}
		registry.Get("m-1").CompleteRequest(reqID, &ProcessingResponse{Success: true, Payload: json.RawMessage(`{"data":`)})
		return nil
	}, nil)
	t.Cleanup(func() { registry.Unregister(m) })
	d := newTestDispatcher(t, registry)

	call := NewProcessorCallout(testTenantID, testEntity(), testProcessor("python", 0), "wf1", "t1", "tx-1")
	call.RequestID, call.AnswerLimit, call.OwnerNodeID = "r1", 5*time.Second, "node-test"
	failure, ctxErr := kindOf(d, testContext(), m, call)
	if ctxErr != nil {
		t.Fatalf("unexpected ctx error: %v", ctxErr)
	}
	// The member answered success, but its own payload does not decode: that
	// is its fault, not this node's, so it is MemberFailed, not Terminal. The
	// real decode error (which could otherwise quote a byte of the member's
	// response) must never reach Message/Error().
	if failure == nil || failure.Kind != contract.MemberFailed {
		t.Fatalf("failure = %+v, want MemberFailed", failure)
	}
	const wantMsg = "the compute member's response could not be read"
	if failure.Message != wantMsg || failure.Error() != wantMsg {
		t.Errorf("Message/Error = %q/%q, want %q", failure.Message, failure.Error(), wantMsg)
	}
	if failure.Code != "" {
		t.Errorf("Code = %q, want empty: MemberFailed carries none", failure.Code)
	}
	var appErr *common.AppError
	if errors.As(failure, &appErr) {
		t.Errorf("MemberFailed must carry no AppError, got %v", appErr)
	}
}
