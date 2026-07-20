package grpc

import (
	"context"
	"encoding/json"
	"io"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	cepb "github.com/cyoda-platform/cyoda-go/api/grpc/cloudevents"
)

// mockBidiStream simulates a BidiStreamingServer for testing StartStreaming.
type mockBidiStream struct {
	ctx     context.Context
	recvCh  chan *cepb.CloudEvent
	sentMu  sync.Mutex
	sent    []*cepb.CloudEvent
	sentCh  chan *cepb.CloudEvent // optional: signals when a message is sent
	recvErr error                 // if set, Recv returns this after recvCh is drained
}

func newMockBidiStream(ctx context.Context) *mockBidiStream {
	return &mockBidiStream{
		ctx:    ctx,
		recvCh: make(chan *cepb.CloudEvent, 32),
		sentCh: make(chan *cepb.CloudEvent, 32),
	}
}

func (m *mockBidiStream) Send(ce *cepb.CloudEvent) error {
	m.sentMu.Lock()
	m.sent = append(m.sent, ce)
	m.sentMu.Unlock()
	// Non-blocking signal.
	select {
	case m.sentCh <- ce:
	default:
	}
	return nil
}

func (m *mockBidiStream) Recv() (*cepb.CloudEvent, error) {
	select {
	case ce, ok := <-m.recvCh:
		if !ok {
			if m.recvErr != nil {
				return nil, m.recvErr
			}
			return nil, io.EOF
		}
		return ce, nil
	case <-m.ctx.Done():
		return nil, m.ctx.Err()
	}
}

func (m *mockBidiStream) Context() context.Context       { return m.ctx }
func (m *mockBidiStream) SetHeader(_ metadata.MD) error  { return nil }
func (m *mockBidiStream) SendHeader(_ metadata.MD) error { return nil }
func (m *mockBidiStream) SetTrailer(_ metadata.MD)       {}
func (m *mockBidiStream) SendMsg(_ any) error            { return nil }
func (m *mockBidiStream) RecvMsg(_ any) error            { return nil }

// sentMessages returns a snapshot of all sent messages.
func (m *mockBidiStream) sentMessages() []*cepb.CloudEvent {
	m.sentMu.Lock()
	defer m.sentMu.Unlock()
	cp := make([]*cepb.CloudEvent, len(m.sent))
	copy(cp, m.sent)
	return cp
}

// waitForSent blocks until a message is sent or times out.
func (m *mockBidiStream) waitForSent(t *testing.T, timeout time.Duration) *cepb.CloudEvent {
	t.Helper()
	select {
	case ce := <-m.sentCh:
		return ce
	case <-time.After(timeout):
		t.Fatal("timed out waiting for sent message")
		return nil
	}
}

// enqueue adds a message to the receive channel (simulating client sending).
func (m *mockBidiStream) enqueue(ce *cepb.CloudEvent) {
	m.recvCh <- ce
}

// closeRecv closes the receive channel, causing Recv to return io.EOF.
func (m *mockBidiStream) closeRecv() {
	close(m.recvCh)
}

// --- helpers ---

func makeJoinEvent(t *testing.T, tenantID string, tags []string) *cepb.CloudEvent {
	t.Helper()
	payload := map[string]any{
		"id":                  "join-event-1",
		"tags":                tags,
		"joinedLegalEntityId": tenantID,
	}
	ce, err := NewCloudEvent(CalculationMemberJoinEvent, payload)
	if err != nil {
		t.Fatalf("failed to create join event: %v", err)
	}
	return ce
}

func makeKeepAliveEvent(t *testing.T) *cepb.CloudEvent {
	t.Helper()
	ce, err := NewCloudEvent(CalculationMemberKeepAliveEvent, map[string]any{"success": true})
	if err != nil {
		t.Fatalf("failed to create keep alive event: %v", err)
	}
	return ce
}

func newServiceForTest() *CloudEventsServiceImpl {
	return &CloudEventsServiceImpl{
		registry: NewMemberRegistry(),
	}
}

func m2mContext(tenantID spi.TenantID) context.Context {
	return spi.WithUserContext(context.Background(), &spi.UserContext{
		UserID:   "m2m-client",
		UserName: "m2m",
		Tenant:   spi.Tenant{ID: tenantID, Name: "Test Tenant"},
		Roles:    []string{"ROLE_M2M"},
	})
}

func nonM2MContext(tenantID spi.TenantID) context.Context {
	return spi.WithUserContext(context.Background(), &spi.UserContext{
		UserID:   "user-1",
		UserName: "alice",
		Tenant:   spi.Tenant{ID: tenantID, Name: "Test Tenant"},
		Roles:    []string{"ROLE_USER"},
	})
}

// --- tests ---

func TestStreaming_JoinAndGreet(t *testing.T) {
	svc := newServiceForTest()
	ctx, cancel := context.WithCancel(m2mContext("tenant-1"))
	defer cancel()

	stream := newMockBidiStream(ctx)

	// Send join event.
	stream.enqueue(makeJoinEvent(t, "tenant-1", []string{"python", "go"}))

	// Close recv after join so the loop exits with EOF.
	// But we need to wait for the greet to be sent first, so close after a delay.
	done := make(chan error, 1)
	go func() {
		done <- svc.StartStreaming(stream)
	}()

	// Wait for greet event.
	greetCE := stream.waitForSent(t, 2*time.Second)
	if greetCE.Type != CalculationMemberGreetEvent {
		t.Fatalf("expected greet event type %s, got %s", CalculationMemberGreetEvent, greetCE.Type)
	}

	// Parse greet payload.
	_, greetPayload, err := ParseCloudEvent(greetCE)
	if err != nil {
		t.Fatalf("failed to parse greet event: %v", err)
	}
	var greet struct {
		MemberID            string `json:"memberId"`
		JoinedLegalEntityID string `json:"joinedLegalEntityId"`
		Success             bool   `json:"success"`
	}
	if err := json.Unmarshal(greetPayload, &greet); err != nil {
		t.Fatalf("failed to unmarshal greet payload: %v", err)
	}
	if greet.MemberID == "" {
		t.Fatal("expected non-empty memberId in greet event")
	}
	if greet.JoinedLegalEntityID != "tenant-1" {
		t.Errorf("expected joinedLegalEntityId 'tenant-1', got %q", greet.JoinedLegalEntityID)
	}
	if !greet.Success {
		t.Error("expected success=true in greet event")
	}

	// Close the stream.
	stream.closeRecv()

	err = <-done
	if err != nil && err != io.EOF {
		t.Fatalf("unexpected error from StartStreaming: %v", err)
	}
}

func TestStreaming_MemberRegisteredAndUnregistered(t *testing.T) {
	svc := newServiceForTest()
	ctx, cancel := context.WithCancel(m2mContext("tenant-1"))
	defer cancel()

	stream := newMockBidiStream(ctx)
	stream.enqueue(makeJoinEvent(t, "tenant-1", []string{"python"}))

	done := make(chan error, 1)
	go func() {
		done <- svc.StartStreaming(stream)
	}()

	// Wait for greet.
	greetCE := stream.waitForSent(t, 2*time.Second)
	_, greetPayload, _ := ParseCloudEvent(greetCE)
	memberID := ExtractStringField(greetPayload, "memberId")

	// Verify member is registered.
	member := svc.registry.Get(memberID)
	if member == nil {
		t.Fatal("expected member to be registered after greet")
	}
	if member.TenantID != "tenant-1" {
		t.Errorf("expected tenant 'tenant-1', got %q", member.TenantID)
	}

	// Close stream to trigger unregister.
	stream.closeRecv()
	<-done

	// Verify member is unregistered.
	member = svc.registry.Get(memberID)
	if member != nil {
		t.Fatal("expected member to be unregistered after stream close")
	}
}

// TestStreaming_InboundKeepAliveUpdatesLivenessNoEcho asserts the keep-alive
// protocol invariant: an inbound member keep-alive is liveness-only. The server
// updates the member's LastSeen and does NOT echo a keep-alive back. Echoing
// turns a client that also echoes inbound keep-alives into a zero-delay,
// unbounded ping-pong storm that pins both processes at 100% CPU. Each side
// must ping only on its own ticker (see TestStreaming_ServerSendsKeepAliveOnTicker).
func TestStreaming_InboundKeepAliveUpdatesLivenessNoEcho(t *testing.T) {
	svc := newServiceForTest()
	// Long ticker interval so the server's own keep-alive ticker cannot fire
	// during the test window and be mistaken for an echo.
	svc.SetKeepAliveConfig(time.Hour, 2*time.Hour)

	ctx, cancel := context.WithCancel(m2mContext("tenant-1"))
	defer cancel()

	stream := newMockBidiStream(ctx)
	stream.enqueue(makeJoinEvent(t, "tenant-1", []string{}))

	done := make(chan error, 1)
	go func() {
		done <- svc.StartStreaming(stream)
	}()

	// Wait for greet, then grab the member so we can observe liveness.
	greetCE := stream.waitForSent(t, 2*time.Second)
	_, greetPayload, _ := ParseCloudEvent(greetCE)
	memberID := ExtractStringField(greetPayload, "memberId")
	member := svc.registry.Get(memberID)
	if member == nil {
		t.Fatal("member not found")
	}
	before := member.LastSeen()

	// Send an inbound keep-alive from the "client".
	stream.enqueue(makeKeepAliveEvent(t))

	// Liveness must be refreshed: poll until LastSeen advances past the
	// registration timestamp (proves the keep-alive was processed).
	deadline := time.Now().Add(2 * time.Second)
	for member.LastSeen().Equal(before) {
		if time.Now().After(deadline) {
			t.Fatal("inbound keep-alive did not refresh member liveness")
		}
		time.Sleep(2 * time.Millisecond)
	}

	// The server must NOT echo a keep-alive back. Nothing (other than the
	// already-consumed greet) should have been sent. Give any (buggy) echo a
	// generous window to arrive on the send channel.
	select {
	case ce := <-stream.sentCh:
		t.Fatalf("server echoed an event in response to an inbound keep-alive: type=%s", ce.Type)
	case <-time.After(200 * time.Millisecond):
		// No echo — correct.
	}

	// Close.
	stream.closeRecv()
	<-done
}

// TestStreaming_ServerSendsKeepAliveOnTicker guards the mechanism the server
// now relies on solely: it pings the member on its own interval ticker,
// independent of any inbound traffic. This is what keeps the member's liveness
// fresh (via the client echoing on its own schedule) now that the server no
// longer echoes inbound keep-alives.
func TestStreaming_ServerSendsKeepAliveOnTicker(t *testing.T) {
	svc := newServiceForTest()
	// Short interval so the ticker fires quickly; large timeout so the member
	// is never reaped during the test.
	svc.SetKeepAliveConfig(30*time.Millisecond, time.Hour)

	ctx, cancel := context.WithCancel(m2mContext("tenant-1"))
	defer cancel()

	stream := newMockBidiStream(ctx)
	stream.enqueue(makeJoinEvent(t, "tenant-1", []string{}))

	done := make(chan error, 1)
	go func() {
		done <- svc.StartStreaming(stream)
	}()

	// Consume the greet.
	greetCE := stream.waitForSent(t, 2*time.Second)
	if greetCE.Type != CalculationMemberGreetEvent {
		t.Fatalf("expected greet first, got %s", greetCE.Type)
	}

	// Without sending any inbound message, the server's ticker must emit a
	// keep-alive on its own.
	kaCE := stream.waitForSent(t, 2*time.Second)
	if kaCE.Type != CalculationMemberKeepAliveEvent {
		t.Fatalf("expected server ticker to send a keep-alive, got %s", kaCE.Type)
	}

	stream.closeRecv()
	<-done
}

// TestStreaming_EchoingClientDoesNotStorm reproduces the storm scenario from
// the wild: a client that echoes every inbound keep-alive back to the server.
// If the server also echoed inbound keep-alives, the two would ping-pong with
// zero delay and the server's keep-alive output would grow without bound. With
// the server treating inbound keep-alives as liveness-only, its output is
// capped at roughly one keep-alive per ticker interval regardless of how
// eagerly the client echoes.
func TestStreaming_EchoingClientDoesNotStorm(t *testing.T) {
	svc := newServiceForTest()
	const interval = 20 * time.Millisecond
	svc.SetKeepAliveConfig(interval, time.Hour)

	ctx, cancel := context.WithCancel(m2mContext("tenant-1"))
	defer cancel()

	stream := newMockBidiStream(ctx)
	stream.enqueue(makeJoinEvent(t, "tenant-1", []string{}))

	done := make(chan error, 1)
	go func() {
		done <- svc.StartStreaming(stream)
	}()

	// Echoing client: for every keep-alive the server sends, immediately send
	// one back — the exact behavior that ignited the storm in the field. The
	// echoed event is read-only to the server, so one instance is reused (and
	// built here, not in the goroutine, to keep t.* off a non-test goroutine).
	echo := makeKeepAliveEvent(t)
	stop := make(chan struct{})
	go func() {
		for {
			select {
			case <-stop:
				return
			case ce := <-stream.sentCh:
				if ce != nil && ce.Type == CalculationMemberKeepAliveEvent {
					select {
					case stream.recvCh <- echo:
					case <-stop:
						return
					}
				}
			}
		}
	}()

	// Let the loop run for a fixed number of ticker intervals.
	const window = 10 * interval
	start := time.Now()
	time.Sleep(window)
	elapsed := time.Since(start)

	// Count keep-alives the server emitted. A correct server emits ~one per
	// ticker interval of elapsed wall-time; a storming server emits orders of
	// magnitude more (one per client echo, compounding with zero delay — the
	// buggy code produced ~50k in this window). Scale the bound to actual
	// elapsed time (not the nominal sleep) so a scheduling overrun on a loaded
	// CI box can't false-fail, while still flagging any true storm.
	expected := int(elapsed / interval)
	limit := expected + 20
	sent := stream.sentMessages()
	kaCount := 0
	for _, ce := range sent {
		if ce.Type == CalculationMemberKeepAliveEvent {
			kaCount++
		}
	}
	if kaCount > limit {
		t.Fatalf("server emitted %d keep-alives in %v with an echoing client — storm not contained (expected ~%d ticker sends, limit %d)", kaCount, elapsed, expected, limit)
	}

	close(stop)
	cancel()
	<-done
}

func TestStreaming_NoRoleM2M_PermissionDenied(t *testing.T) {
	svc := newServiceForTest()
	ctx := nonM2MContext("tenant-1")

	stream := newMockBidiStream(ctx)

	err := svc.StartStreaming(stream)
	if err == nil {
		t.Fatal("expected error for non-M2M user")
	}

	st, ok := status.FromError(err)
	if !ok {
		t.Fatalf("expected gRPC status error, got %v", err)
	}
	if st.Code() != codes.PermissionDenied {
		t.Errorf("expected PermissionDenied, got %v", st.Code())
	}
}

func TestStreaming_NoUserContext_Unauthenticated(t *testing.T) {
	svc := newServiceForTest()
	ctx := context.Background() // no UserContext

	stream := newMockBidiStream(ctx)

	err := svc.StartStreaming(stream)
	if err == nil {
		t.Fatal("expected error for missing user context")
	}

	st, ok := status.FromError(err)
	if !ok {
		t.Fatalf("expected gRPC status error, got %v", err)
	}
	if st.Code() != codes.Unauthenticated {
		t.Errorf("expected Unauthenticated, got %v", st.Code())
	}
}

func TestStreaming_FirstMessageNotJoin_InvalidArgument(t *testing.T) {
	svc := newServiceForTest()
	ctx := m2mContext("tenant-1")

	stream := newMockBidiStream(ctx)

	// Send a keep-alive as the first message instead of a join.
	stream.enqueue(makeKeepAliveEvent(t))

	err := svc.StartStreaming(stream)
	if err == nil {
		t.Fatal("expected error for non-join first message")
	}

	st, ok := status.FromError(err)
	if !ok {
		t.Fatalf("expected gRPC status error, got %v", err)
	}
	if st.Code() != codes.InvalidArgument {
		t.Errorf("expected InvalidArgument, got %v", st.Code())
	}
}

func TestStreaming_TenantMismatch_PermissionDenied(t *testing.T) {
	svc := newServiceForTest()
	ctx := m2mContext("tenant-1")

	stream := newMockBidiStream(ctx)

	// Join with a different tenant.
	stream.enqueue(makeJoinEvent(t, "tenant-2", nil))

	err := svc.StartStreaming(stream)
	if err == nil {
		t.Fatal("expected error for tenant mismatch")
	}

	st, ok := status.FromError(err)
	if !ok {
		t.Fatalf("expected gRPC status error, got %v", err)
	}
	if st.Code() != codes.PermissionDenied {
		t.Errorf("expected PermissionDenied, got %v", st.Code())
	}
}

func TestStreaming_ProcessorResponse(t *testing.T) {
	svc := newServiceForTest()
	ctx, cancel := context.WithCancel(m2mContext("tenant-1"))
	defer cancel()

	stream := newMockBidiStream(ctx)
	stream.enqueue(makeJoinEvent(t, "tenant-1", []string{"python"}))

	done := make(chan error, 1)
	go func() {
		done <- svc.StartStreaming(stream)
	}()

	// Wait for greet to get memberID.
	greetCE := stream.waitForSent(t, 2*time.Second)
	_, greetPayload, _ := ParseCloudEvent(greetCE)
	memberID := ExtractStringField(greetPayload, "memberId")

	// Track a request on the member.
	member := svc.registry.Get(memberID)
	if member == nil {
		t.Fatal("member not found")
	}
	respCh := member.TrackRequest("req-123")

	// Send processor response from the "client".
	respPayload := map[string]any{
		"requestId": "req-123",
		"success":   true,
		"payload":   map[string]any{"data": map[string]any{"updated": true}},
	}
	respCE, err := NewCloudEvent(EntityProcessorCalculationResponse, respPayload)
	if err != nil {
		t.Fatalf("failed to create response event: %v", err)
	}
	stream.enqueue(respCE)

	// Wait for the response to be routed.
	select {
	case resp := <-respCh:
		if resp == nil {
			t.Fatal("expected non-nil response")
		}
		if !resp.Success {
			t.Error("expected success=true")
		}
		if resp.Payload == nil {
			t.Error("expected non-nil payload")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for processor response")
	}

	// Close.
	stream.closeRecv()
	<-done
}

func TestStreaming_CriteriaResponse(t *testing.T) {
	svc := newServiceForTest()
	ctx, cancel := context.WithCancel(m2mContext("tenant-1"))
	defer cancel()

	stream := newMockBidiStream(ctx)
	stream.enqueue(makeJoinEvent(t, "tenant-1", []string{"python"}))

	done := make(chan error, 1)
	go func() {
		done <- svc.StartStreaming(stream)
	}()

	// Wait for greet.
	greetCE := stream.waitForSent(t, 2*time.Second)
	_, greetPayload, _ := ParseCloudEvent(greetCE)
	memberID := ExtractStringField(greetPayload, "memberId")

	member := svc.registry.Get(memberID)
	if member == nil {
		t.Fatal("member not found")
	}
	respCh := member.TrackRequest("req-456")

	// Send criteria response.
	respPayload := map[string]any{
		"requestId": "req-456",
		"success":   true,
		"matches":   true,
	}
	respCE, err := NewCloudEvent(EntityCriteriaCalculationResponse, respPayload)
	if err != nil {
		t.Fatalf("failed to create criteria response event: %v", err)
	}
	stream.enqueue(respCE)

	select {
	case resp := <-respCh:
		if resp == nil {
			t.Fatal("expected non-nil response")
		}
		if !resp.Success {
			t.Error("expected success=true")
		}
		if resp.Matches == nil || !*resp.Matches {
			t.Error("expected matches=true")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for criteria response")
	}

	stream.closeRecv()
	<-done
}

func TestStreaming_CriteriaResponse_PropagatesReason(t *testing.T) {
	svc := newServiceForTest()
	ctx, cancel := context.WithCancel(m2mContext("tenant-1"))
	defer cancel()

	stream := newMockBidiStream(ctx)
	stream.enqueue(makeJoinEvent(t, "tenant-1", []string{"python"}))

	done := make(chan error, 1)
	go func() { done <- svc.StartStreaming(stream) }()

	greetCE := stream.waitForSent(t, 2*time.Second)
	_, greetPayload, _ := ParseCloudEvent(greetCE)
	memberID := ExtractStringField(greetPayload, "memberId")

	member := svc.registry.Get(memberID)
	if member == nil {
		t.Fatal("member not found")
	}
	respCh := member.TrackRequest("req-reason-1")

	respPayload := map[string]any{
		"requestId": "req-reason-1",
		"success":   true,
		"matches":   false,
		"reason":    "credit score 540 below threshold 600",
	}
	respCE, err := NewCloudEvent(EntityCriteriaCalculationResponse, respPayload)
	if err != nil {
		t.Fatalf("failed to create criteria response event: %v", err)
	}
	stream.enqueue(respCE)

	select {
	case resp := <-respCh:
		if resp == nil {
			t.Fatal("expected non-nil response")
		}
		if resp.Reason != "credit score 540 below threshold 600" {
			t.Errorf("expected reason propagated, got %q", resp.Reason)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for criteria response")
	}

	stream.closeRecv()
	<-done
}

// --- Retryable flag plumbing (audit §M1) -----------------------------------
//
// These tests cover the wire tri-state: retryable=true / retryable=false /
// retryable absent on a present error / no error at all. All four must
// round-trip into ProcessingResponse.Retryable as a *bool whose presence
// distinguishes "wire said so" from "wire didn't say". Plus one symmetric
// pair on the criteria response handler — both halves of the future retry
// loop need this data, so both are surfaced now.

// retryableHarness wraps the common setup for retryable-plumbing tests:
// a joined member, a tracked request, and the response channel
// the test asserts on. Caller MUST call closeAndWait() at end of test.
type retryableHarness struct {
	stream *mockBidiStream
	done   chan error
	respCh <-chan *ProcessingResponse
}

func newRetryableHarness(t *testing.T, requestID string) *retryableHarness {
	t.Helper()
	svc := newServiceForTest()
	ctx, cancel := context.WithCancel(m2mContext("tenant-1"))
	t.Cleanup(cancel)

	stream := newMockBidiStream(ctx)
	stream.enqueue(makeJoinEvent(t, "tenant-1", []string{"python"}))

	done := make(chan error, 1)
	go func() {
		done <- svc.StartStreaming(stream)
	}()

	greetCE := stream.waitForSent(t, 2*time.Second)
	_, greetPayload, _ := ParseCloudEvent(greetCE)
	memberID := ExtractStringField(greetPayload, "memberId")
	member := svc.registry.Get(memberID)
	if member == nil {
		t.Fatal("member not found")
	}
	respCh := member.TrackRequest(requestID)
	return &retryableHarness{stream, done, respCh}
}

func (h *retryableHarness) closeAndWait() {
	h.stream.closeRecv()
	<-h.done
}

// awaitResponse blocks until a response is routed (or fails the test on
// timeout). Returns the captured *ProcessingResponse for assertion.
func (h *retryableHarness) awaitResponse(t *testing.T) *ProcessingResponse {
	t.Helper()
	select {
	case resp := <-h.respCh:
		if resp == nil {
			t.Fatal("expected non-nil response")
		}
		return resp
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for response")
		return nil
	}
}

func TestStreaming_ProcessorResponse_PropagatesRetryableFalse(t *testing.T) {
	h := newRetryableHarness(t, "req-rt-false")
	defer h.closeAndWait()

	respPayload := map[string]any{
		"requestId": "req-rt-false",
		"success":   false,
		"error": map[string]any{
			"message":   "boom",
			"retryable": false,
		},
	}
	respCE, err := NewCloudEvent(EntityProcessorCalculationResponse, respPayload)
	if err != nil {
		t.Fatalf("failed to create response event: %v", err)
	}
	h.stream.enqueue(respCE)

	resp := h.awaitResponse(t)
	if resp.Retryable == nil {
		t.Fatal("expected Retryable to be non-nil (wire said retryable=false)")
	}
	if *resp.Retryable {
		t.Errorf("expected Retryable=false, got true")
	}
}

func TestStreaming_ProcessorResponse_PropagatesRetryableTrue(t *testing.T) {
	h := newRetryableHarness(t, "req-rt-true")
	defer h.closeAndWait()

	respPayload := map[string]any{
		"requestId": "req-rt-true",
		"success":   false,
		"error": map[string]any{
			"message":   "transient",
			"retryable": true,
		},
	}
	respCE, err := NewCloudEvent(EntityProcessorCalculationResponse, respPayload)
	if err != nil {
		t.Fatalf("failed to create response event: %v", err)
	}
	h.stream.enqueue(respCE)

	resp := h.awaitResponse(t)
	if resp.Retryable == nil {
		t.Fatal("expected Retryable to be non-nil (wire said retryable=true)")
	}
	if !*resp.Retryable {
		t.Errorf("expected Retryable=true, got false")
	}
}

func TestStreaming_ProcessorResponse_RetryableNilWhenAbsent(t *testing.T) {
	h := newRetryableHarness(t, "req-rt-absent")
	defer h.closeAndWait()

	respPayload := map[string]any{
		"requestId": "req-rt-absent",
		"success":   false,
		"error": map[string]any{
			"message": "no flag set",
		},
	}
	respCE, err := NewCloudEvent(EntityProcessorCalculationResponse, respPayload)
	if err != nil {
		t.Fatalf("failed to create response event: %v", err)
	}
	h.stream.enqueue(respCE)

	resp := h.awaitResponse(t)
	if resp.Retryable != nil {
		t.Errorf("expected Retryable to be nil when wire omitted the key; got *Retryable=%v", *resp.Retryable)
	}
}

func TestStreaming_ProcessorResponse_RetryableNilOnSuccess(t *testing.T) {
	h := newRetryableHarness(t, "req-rt-success")
	defer h.closeAndWait()

	respPayload := map[string]any{
		"requestId": "req-rt-success",
		"success":   true,
		"payload":   map[string]any{"data": map[string]any{"ok": true}},
	}
	respCE, err := NewCloudEvent(EntityProcessorCalculationResponse, respPayload)
	if err != nil {
		t.Fatalf("failed to create response event: %v", err)
	}
	h.stream.enqueue(respCE)

	resp := h.awaitResponse(t)
	if resp.Retryable != nil {
		t.Errorf("expected Retryable to be nil on success (no error block); got *Retryable=%v", *resp.Retryable)
	}
}

func TestStreaming_CriteriaResponse_PropagatesRetryableFlag(t *testing.T) {
	// Symmetric to the processor case: criteria responses can also carry a
	// member-vetoed retryable=false on the inbound error shape. The future
	// retry loop covers the criteria dispatch path, so symmetry now avoids
	// a second streaming-handler edit later.
	t.Run("retryable_false", func(t *testing.T) {
		h := newRetryableHarness(t, "req-cri-false")
		defer h.closeAndWait()

		respPayload := map[string]any{
			"requestId": "req-cri-false",
			"success":   false,
			"matches":   false,
			"error": map[string]any{
				"message":   "criteria boom",
				"retryable": false,
			},
		}
		respCE, err := NewCloudEvent(EntityCriteriaCalculationResponse, respPayload)
		if err != nil {
			t.Fatalf("failed to create criteria response event: %v", err)
		}
		h.stream.enqueue(respCE)

		resp := h.awaitResponse(t)
		if resp.Retryable == nil || *resp.Retryable {
			t.Errorf("expected Retryable=false on criteria response; got %v", resp.Retryable)
		}
	})

	t.Run("absent_on_success", func(t *testing.T) {
		h := newRetryableHarness(t, "req-cri-success")
		defer h.closeAndWait()

		respPayload := map[string]any{
			"requestId": "req-cri-success",
			"success":   true,
			"matches":   true,
		}
		respCE, err := NewCloudEvent(EntityCriteriaCalculationResponse, respPayload)
		if err != nil {
			t.Fatalf("failed to create criteria response event: %v", err)
		}
		h.stream.enqueue(respCE)

		resp := h.awaitResponse(t)
		if resp.Retryable != nil {
			t.Errorf("expected Retryable=nil on criteria success; got *Retryable=%v", *resp.Retryable)
		}
	})
}

// TestStreaming_FunctionResponse exercises the inbound handler for the
// Function callout (a third callout shape alongside processor/criteria):
// EntityFunctionCalculationResponse carries Result (raw JSON) + ResultKind
// (a discriminator string) instead of the processor's Payload or the
// criteria's Matches/Reason.
func TestStreaming_FunctionResponse(t *testing.T) {
	svc := newServiceForTest()
	ctx, cancel := context.WithCancel(m2mContext("tenant-1"))
	defer cancel()

	stream := newMockBidiStream(ctx)
	stream.enqueue(makeJoinEvent(t, "tenant-1", []string{"python"}))

	done := make(chan error, 1)
	go func() {
		done <- svc.StartStreaming(stream)
	}()

	greetCE := stream.waitForSent(t, 2*time.Second)
	_, greetPayload, _ := ParseCloudEvent(greetCE)
	memberID := ExtractStringField(greetPayload, "memberId")

	member := svc.registry.Get(memberID)
	if member == nil {
		t.Fatal("member not found")
	}
	respCh := member.TrackRequest("req-fn-1")

	respPayload := map[string]any{
		"requestId":  "req-fn-1",
		"entityId":   "e",
		"success":    true,
		"result":     map[string]any{"fireAfterMs": 1000},
		"resultKind": "Schedule",
	}
	respCE, err := NewCloudEvent(EntityFunctionCalculationResponse, respPayload)
	if err != nil {
		t.Fatalf("failed to create function response event: %v", err)
	}
	stream.enqueue(respCE)

	select {
	case resp := <-respCh:
		if resp == nil {
			t.Fatal("expected non-nil response")
		}
		if !resp.Success {
			t.Error("expected success=true")
		}
		if resp.ResultKind != "Schedule" {
			t.Errorf("expected resultKind=%q, got %q", "Schedule", resp.ResultKind)
		}
		if resp.Result == nil {
			t.Fatal("expected non-nil Result")
		}
		var decoded map[string]any
		if err := json.Unmarshal(resp.Result, &decoded); err != nil {
			t.Fatalf("failed to unmarshal Result: %v", err)
		}
		if decoded["fireAfterMs"] != float64(1000) {
			t.Errorf("expected fireAfterMs=1000, got %v", decoded["fireAfterMs"])
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for function response")
	}

	stream.closeRecv()
	<-done
}

func TestStreaming_FunctionResponse_PropagatesError(t *testing.T) {
	h := newRetryableHarness(t, "req-fn-err")
	defer h.closeAndWait()

	respPayload := map[string]any{
		"requestId": "req-fn-err",
		"success":   false,
		"error": map[string]any{
			"message":   "function boom",
			"retryable": true,
		},
	}
	respCE, err := NewCloudEvent(EntityFunctionCalculationResponse, respPayload)
	if err != nil {
		t.Fatalf("failed to create function response event: %v", err)
	}
	h.stream.enqueue(respCE)

	resp := h.awaitResponse(t)
	if resp.Success {
		t.Error("expected success=false")
	}
	if resp.Error != "function boom" {
		t.Errorf("expected error message propagated, got %q", resp.Error)
	}
	if resp.Retryable == nil || !*resp.Retryable {
		t.Errorf("expected Retryable=true, got %v", resp.Retryable)
	}
}

func TestStreaming_KeepAliveTimeout(t *testing.T) {
	svc := newServiceForTest()
	// Set very short keep-alive for testing.
	svc.SetKeepAliveConfig(50*time.Millisecond, 100*time.Millisecond)

	ctx, cancel := context.WithCancel(m2mContext("tenant-1"))
	defer cancel()

	stream := newMockBidiStream(ctx)
	stream.enqueue(makeJoinEvent(t, "tenant-1", []string{}))

	done := make(chan error, 1)
	go func() {
		done <- svc.StartStreaming(stream)
	}()

	// Wait for greet.
	_ = stream.waitForSent(t, 2*time.Second)

	// Do NOT send any keep-alive — let the timeout fire.
	// The keep-alive loop should cancel the context after ~100ms.
	select {
	case err := <-done:
		// Stream should end due to context cancellation.
		if err == nil {
			// Also acceptable — the stream just ended.
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for keep-alive timeout to terminate stream")
	}
}

func TestStreaming_EmptyTenantInJoinEvent_UsesAuthTenant(t *testing.T) {
	svc := newServiceForTest()
	ctx, cancel := context.WithCancel(m2mContext("tenant-1"))
	defer cancel()

	stream := newMockBidiStream(ctx)

	// Join with empty joinedLegalEntityId — should use the auth tenant.
	joinPayload := map[string]any{
		"id":                  "join-event-2",
		"tags":                []string{"python"},
		"joinedLegalEntityId": "",
	}
	joinCE, err := NewCloudEvent(CalculationMemberJoinEvent, joinPayload)
	if err != nil {
		t.Fatalf("failed to create join event: %v", err)
	}
	stream.enqueue(joinCE)

	done := make(chan error, 1)
	go func() {
		done <- svc.StartStreaming(stream)
	}()

	// Wait for greet — should succeed.
	greetCE := stream.waitForSent(t, 2*time.Second)
	if greetCE.Type != CalculationMemberGreetEvent {
		t.Fatalf("expected greet event, got %s", greetCE.Type)
	}

	_, greetPayload, _ := ParseCloudEvent(greetCE)
	memberID := ExtractStringField(greetPayload, "memberId")

	// Verify member registered with auth tenant.
	member := svc.registry.Get(memberID)
	if member == nil {
		t.Fatal("member not registered")
	}
	if member.TenantID != "tenant-1" {
		t.Errorf("expected tenant 'tenant-1', got %q", member.TenantID)
	}

	stream.closeRecv()
	<-done
}

func TestHasRole(t *testing.T) {
	tests := []struct {
		name   string
		roles  []string
		target string
		want   bool
	}{
		{"present", []string{"ROLE_USER", "ROLE_M2M"}, "ROLE_M2M", true},
		{"absent", []string{"ROLE_USER"}, "ROLE_M2M", false},
		{"empty", nil, "ROLE_M2M", false},
		{"exact match only", []string{"ROLE_M2M_ADMIN"}, "ROLE_M2M", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := spi.HasRole(tt.roles, tt.target); got != tt.want {
				t.Errorf("HasRole(%v, %q) = %v, want %v", tt.roles, tt.target, got, tt.want)
			}
		})
	}
}
