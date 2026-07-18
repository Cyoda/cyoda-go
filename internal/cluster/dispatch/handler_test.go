package dispatch

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/contract"
)

// fakeLocalDispatcher implements contract.ExternalProcessingService for testing.
// capturedCtx records the ctx from the most recent dispatch call so tests can
// assert identity propagation.
type fakeLocalDispatcher struct {
	processorResult *spi.Entity
	processorErr    error
	criteriaResult  bool
	criteriaReason  string
	criteriaErr     error
	functionResult  contract.FunctionResult
	functionErr     error
	capturedCtx     context.Context
}

func (f *fakeLocalDispatcher) DispatchProcessor(
	ctx context.Context,
	_ *spi.Entity,
	_ spi.ProcessorDefinition,
	_, _, _ string,
) (*spi.Entity, error) {
	f.capturedCtx = ctx
	return f.processorResult, f.processorErr
}

func (f *fakeLocalDispatcher) DispatchCriteria(
	ctx context.Context,
	_ *spi.Entity,
	_ json.RawMessage,
	_, _, _, _, _ string,
) (bool, string, error) {
	f.capturedCtx = ctx
	return f.criteriaResult, f.criteriaReason, f.criteriaErr
}

func (f *fakeLocalDispatcher) DispatchFunction(
	ctx context.Context,
	_ *spi.Entity,
	_ spi.ScheduleFunction,
	_, _, _ string,
) (contract.FunctionResult, error) {
	f.capturedCtx = ctx
	return f.functionResult, f.functionErr
}

var testSecret32 = bytes.Repeat([]byte{0xAB}, 32)

// newAEAD builds an AEADPeerAuth keyed by testSecret32. Internal test helper.
func newAEAD(t *testing.T) *AEADPeerAuth {
	t.Helper()
	a, err := NewAEADPeerAuth(testSecret32, 30*time.Second)
	if err != nil {
		t.Fatalf("NewAEADPeerAuth: %v", err)
	}
	return a
}

// signedRequest builds an AEAD-wrapped POST request ready for the handler
// to verify. Convenience for tests that need an authenticated request body.
func signedRequest(t *testing.T, auth *AEADPeerAuth, method, path string, plain []byte) *http.Request {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	wire, err := auth.Sign(req, plain)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	req.Body = io.NopCloser(bytes.NewReader(wire))
	req.ContentLength = int64(len(wire))
	return req
}

func TestHandler_ProcessorSuccess(t *testing.T) {
	auth := newAEAD(t)
	fake := &fakeLocalDispatcher{
		processorResult: &spi.Entity{
			Meta: spi.EntityMeta{ID: "ent-1"},
			Data: []byte(`{"output":42}`),
		},
	}

	handler := NewDispatchHandler(fake, auth)
	mux := http.NewServeMux()
	handler.Register(mux)

	processor := spi.ProcessorDefinition{Name: "proc1", Type: "SCRIPT"}
	req := DispatchCalloutRequest{
		Kind:           "processor",
		Entity:         json.RawMessage(`{"foo":"bar"}`),
		EntityMeta:     spi.EntityMeta{ID: "ent-1"},
		Processor:      &processor,
		WorkflowName:   "wf",
		TransitionName: "t1",
		TxID:           "tx-1",
		TenantID:       "tenant-a",
		UserID:         "user-1",
		Roles:          []string{"ROLE_USER"},
	}
	plain, _ := json.Marshal(req)
	httpReq := signedRequest(t, auth, http.MethodPost, "/internal/dispatch/callout", plain)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httpReq)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp DispatchCalloutResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if !resp.Success {
		t.Errorf("expected success=true, got false (error: %s)", resp.Error)
	}
	if string(resp.EntityData) != `{"output":42}` {
		t.Errorf("unexpected entity data: %s", resp.EntityData)
	}
}

func TestHandler_CriteriaSuccess(t *testing.T) {
	auth := newAEAD(t)
	fake := &fakeLocalDispatcher{criteriaResult: true}

	handler := NewDispatchHandler(fake, auth)
	mux := http.NewServeMux()
	handler.Register(mux)

	req := DispatchCalloutRequest{
		Kind:           "criteria",
		Entity:         json.RawMessage(`{"foo":"bar"}`),
		EntityMeta:     spi.EntityMeta{ID: "ent-2"},
		Criterion:      json.RawMessage(`{"type":"eq","field":"x","value":1}`),
		Target:         "target",
		WorkflowName:   "wf",
		TransitionName: "t1",
		ProcessorName:  "proc1",
		TxID:           "tx-2",
		TenantID:       "tenant-a",
		UserID:         "user-1",
		Roles:          []string{"ROLE_USER"},
	}
	plain, _ := json.Marshal(req)
	httpReq := signedRequest(t, auth, http.MethodPost, "/internal/dispatch/callout", plain)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httpReq)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp DispatchCalloutResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if !resp.Success {
		t.Errorf("expected success=true")
	}
	if resp.Matches == nil || !*resp.Matches {
		t.Errorf("expected matches=true")
	}
}

func TestHandler_MissingAEADHeaders(t *testing.T) {
	handler := NewDispatchHandler(&fakeLocalDispatcher{}, newAEAD(t))
	mux := http.NewServeMux()
	handler.Register(mux)

	body := []byte(`{}`)
	httpReq := httptest.NewRequest(http.MethodPost, "/internal/dispatch/callout", bytes.NewReader(body))
	httpReq.Header.Set("Content-Type", "application/json")
	// No X-Dispatch-Timestamp header, plain JSON body — rejected by AEAD Verify.

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httpReq)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", rec.Code)
	}
}

func TestHandler_RejectsPlainJSONWithoutAEAD(t *testing.T) {
	// Even if someone sets the timestamp header, a plain JSON body fails AEAD.Open.
	handler := NewDispatchHandler(&fakeLocalDispatcher{}, newAEAD(t))
	mux := http.NewServeMux()
	handler.Register(mux)

	httpReq := httptest.NewRequest(http.MethodPost, "/internal/dispatch/callout",
		bytes.NewReader([]byte(`{"not":"encrypted"}`)))
	httpReq.Header.Set("Content-Type", DispatchContentType)
	httpReq.Header.Set(DispatchTimestampHdr, fmt.Sprintf("%d", time.Now().Unix()))

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httpReq)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for plain JSON, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestHandler_RejectsReplayedRequest(t *testing.T) {
	auth := newAEAD(t)
	handler := NewDispatchHandler(&fakeLocalDispatcher{
		processorResult: &spi.Entity{Meta: spi.EntityMeta{ID: "e"}, Data: []byte(`{}`)},
	}, auth)
	mux := http.NewServeMux()
	handler.Register(mux)

	// Sign once, then submit the same wire body twice.
	processor := spi.ProcessorDefinition{Name: "p", Type: "SCRIPT"}
	plain, _ := json.Marshal(DispatchCalloutRequest{
		Kind:     "processor",
		TenantID: "t", UserID: "u",
		Processor:    &processor,
		WorkflowName: "w", TransitionName: "t", TxID: "x",
		EntityMeta: spi.EntityMeta{ID: "e"},
		Entity:     json.RawMessage(`{}`),
	})
	first := signedRequest(t, auth, http.MethodPost, "/internal/dispatch/callout", plain)
	wire, _ := io.ReadAll(first.Body)
	ts := first.Header.Get(DispatchTimestampHdr)

	build := func() *http.Request {
		r := httptest.NewRequest(http.MethodPost, "/internal/dispatch/callout", bytes.NewReader(wire))
		r.Header.Set("Content-Type", DispatchContentType)
		r.Header.Set(DispatchTimestampHdr, ts)
		return r
	}

	rec1 := httptest.NewRecorder()
	mux.ServeHTTP(rec1, build())
	if rec1.Code != http.StatusOK {
		t.Fatalf("first request should succeed, got %d: %s", rec1.Code, rec1.Body.String())
	}

	rec2 := httptest.NewRecorder()
	mux.ServeHTTP(rec2, build())
	if rec2.Code != http.StatusForbidden {
		t.Fatalf("replay should be rejected with 403, got %d", rec2.Code)
	}
}

func TestHandler_PopulatesPeerIdentityInContext(t *testing.T) {
	auth := newAEAD(t)
	fake := &fakeLocalDispatcher{
		processorResult: &spi.Entity{Meta: spi.EntityMeta{ID: "e"}, Data: []byte(`{}`)},
	}
	handler := NewDispatchHandler(fake, auth)
	mux := http.NewServeMux()
	handler.Register(mux)

	processor := spi.ProcessorDefinition{Name: "p", Type: "SCRIPT"}
	plain, _ := json.Marshal(DispatchCalloutRequest{
		Kind:     "processor",
		TenantID: "t", UserID: "u", TxID: "tx",
		Processor:    &processor,
		WorkflowName: "w", TransitionName: "t",
		EntityMeta: spi.EntityMeta{ID: "e"},
		Entity:     json.RawMessage(`{}`),
	})
	httpReq := signedRequest(t, auth, http.MethodPost, "/internal/dispatch/callout", plain)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httpReq)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}

	id, ok := PeerIdentityFromContext(fake.capturedCtx)
	if !ok {
		t.Fatal("PeerIdentity not found in dispatcher ctx — handler failed to propagate")
	}
	if id.AuthMethod() != "aead-v1" {
		t.Errorf("AuthMethod = %q, want aead-v1", id.AuthMethod())
	}
}

func TestNewAEADPeerAuth_SecretTooShort(t *testing.T) {
	_, err := NewAEADPeerAuth([]byte("short"), 30*time.Second)
	if err == nil {
		t.Fatal("expected error for short secret")
	}
	if !errors.Is(err, ErrSharedSecretTooShort) {
		t.Errorf("expected ErrSharedSecretTooShort, got %v", err)
	}
}

func TestHandler_ProcessorError_SanitizedResponse(t *testing.T) {
	auth := newAEAD(t)
	fake := &fakeLocalDispatcher{
		processorErr: fmt.Errorf("connection refused: dial tcp 10.0.0.1:5432"),
	}
	handler := NewDispatchHandler(fake, auth)
	mux := http.NewServeMux()
	handler.Register(mux)

	processor := spi.ProcessorDefinition{Name: "proc1", Type: "SCRIPT"}
	plain, _ := json.Marshal(DispatchCalloutRequest{
		Kind:           "processor",
		Entity:         json.RawMessage(`{"foo":"bar"}`),
		EntityMeta:     spi.EntityMeta{ID: "ent-1"},
		Processor:      &processor,
		WorkflowName:   "wf",
		TransitionName: "t1",
		TxID:           "tx-1",
		TenantID:       "tenant-a",
		UserID:         "user-1",
		Roles:          []string{"ROLE_USER"},
	})
	httpReq := signedRequest(t, auth, http.MethodPost, "/internal/dispatch/callout", plain)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httpReq)

	var resp DispatchCalloutResponse
	_ = json.NewDecoder(rec.Body).Decode(&resp)
	if resp.Success {
		t.Fatal("expected success=false")
	}
	if strings.Contains(resp.Error, "10.0.0.1") {
		t.Errorf("error response must not contain internal details, got %q", resp.Error)
	}
	if strings.Contains(resp.Error, "connection refused") {
		t.Errorf("error response must not contain internal details, got %q", resp.Error)
	}
}

func TestHandleCriteria_PropagatesReason(t *testing.T) {
	auth := newAEAD(t)
	fake := &fakeLocalDispatcher{criteriaResult: false, criteriaReason: "peer reason here"}

	handler := NewDispatchHandler(fake, auth)
	mux := http.NewServeMux()
	handler.Register(mux)

	req := DispatchCalloutRequest{
		Kind:           "criteria",
		Entity:         json.RawMessage(`{"foo":"bar"}`),
		EntityMeta:     spi.EntityMeta{ID: "ent-2"},
		Criterion:      json.RawMessage(`{"type":"eq","field":"x","value":1}`),
		Target:         "target",
		WorkflowName:   "wf",
		TransitionName: "t1",
		ProcessorName:  "proc1",
		TxID:           "tx-2",
		TenantID:       "tenant-a",
		UserID:         "user-1",
		Roles:          []string{"ROLE_USER"},
	}
	plain, _ := json.Marshal(req)
	httpReq := signedRequest(t, auth, http.MethodPost, "/internal/dispatch/callout", plain)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httpReq)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp DispatchCalloutResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Reason != "peer reason here" {
		t.Errorf("expected peer reason propagated, got %q", resp.Reason)
	}
}

func TestHandler_CriteriaError_SanitizedResponse(t *testing.T) {
	auth := newAEAD(t)
	fake := &fakeLocalDispatcher{
		criteriaErr: fmt.Errorf("pq: password authentication failed for user admin"),
	}
	handler := NewDispatchHandler(fake, auth)
	mux := http.NewServeMux()
	handler.Register(mux)

	plain, _ := json.Marshal(DispatchCalloutRequest{
		Kind:           "criteria",
		Entity:         json.RawMessage(`{"foo":"bar"}`),
		EntityMeta:     spi.EntityMeta{ID: "ent-2"},
		Criterion:      json.RawMessage(`{"type":"eq"}`),
		Target:         "target",
		WorkflowName:   "wf",
		TransitionName: "t1",
		ProcessorName:  "proc1",
		TxID:           "tx-2",
		TenantID:       "tenant-a",
		UserID:         "user-1",
		Roles:          []string{"ROLE_USER"},
	})
	httpReq := signedRequest(t, auth, http.MethodPost, "/internal/dispatch/callout", plain)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httpReq)

	var resp DispatchCalloutResponse
	_ = json.NewDecoder(rec.Body).Decode(&resp)
	if resp.Success {
		t.Fatal("expected success=false")
	}
	if strings.Contains(resp.Error, "password") {
		t.Errorf("error response must not contain internal details, got %q", resp.Error)
	}
}

// TestHandler_ErrorTaxonomy_AppError verifies that when the local dispatch
// (processor/criteria/function) fails with an *common.AppError, the handler
// classifies ErrorCode/ErrorStatus/ErrorRetryable on the response from that
// AppError, so a forwarding node can re-mint the same taxonomy instead of
// collapsing every peer failure into a generic error (B1, final review).
func TestHandler_ErrorTaxonomy_AppError(t *testing.T) {
	auth := newAEAD(t)
	appErr := common.Operational(http.StatusServiceUnavailable, common.ErrCodeDispatchTimeout,
		"processor dispatch timed out after 3000ms").AsRetryable()

	cases := []struct {
		name string
		req  DispatchCalloutRequest
		fake *fakeLocalDispatcher
	}{
		{
			name: "processor",
			req: DispatchCalloutRequest{
				Kind: "processor", TenantID: "t", UserID: "u", TxID: "tx",
				Processor:      &spi.ProcessorDefinition{Name: "p", Type: "SCRIPT"},
				WorkflowName:   "w",
				TransitionName: "t",
				EntityMeta:     spi.EntityMeta{ID: "e"},
				Entity:         json.RawMessage(`{}`),
			},
			fake: &fakeLocalDispatcher{processorErr: appErr},
		},
		{
			name: "criteria",
			req: DispatchCalloutRequest{
				Kind: "criteria", TenantID: "t", UserID: "u", TxID: "tx",
				Criterion:      json.RawMessage(`{"type":"eq"}`),
				Target:         "target",
				WorkflowName:   "w",
				TransitionName: "t",
				ProcessorName:  "p",
				EntityMeta:     spi.EntityMeta{ID: "e"},
				Entity:         json.RawMessage(`{}`),
			},
			fake: &fakeLocalDispatcher{criteriaErr: appErr},
		},
		{
			name: "function",
			req: DispatchCalloutRequest{
				Kind: "function", TenantID: "t", UserID: "u", TxID: "tx",
				Function:       &spi.ScheduleFunction{Name: "fn", ResultKind: "Schedule"},
				WorkflowName:   "w",
				TransitionName: "t",
				EntityMeta:     spi.EntityMeta{ID: "e"},
				Entity:         json.RawMessage(`{}`),
			},
			fake: &fakeLocalDispatcher{functionErr: appErr},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			handler := NewDispatchHandler(tc.fake, auth)
			mux := http.NewServeMux()
			handler.Register(mux)

			plain, _ := json.Marshal(tc.req)
			httpReq := signedRequest(t, auth, http.MethodPost, "/internal/dispatch/callout", plain)

			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, httpReq)

			var resp DispatchCalloutResponse
			if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
				t.Fatalf("decode response: %v", err)
			}
			if resp.Success {
				t.Fatal("expected success=false")
			}
			if resp.ErrorCode != common.ErrCodeDispatchTimeout {
				t.Errorf("ErrorCode = %q, want %q", resp.ErrorCode, common.ErrCodeDispatchTimeout)
			}
			if resp.ErrorStatus != http.StatusServiceUnavailable {
				t.Errorf("ErrorStatus = %d, want %d", resp.ErrorStatus, http.StatusServiceUnavailable)
			}
			if !resp.ErrorRetryable {
				t.Error("ErrorRetryable = false, want true")
			}
			// The client-facing Error text must stay generic — no AppError
			// internal message content (coordinated with B2).
			if strings.Contains(resp.Error, "3000ms") {
				t.Errorf("Error field leaked AppError detail: %q", resp.Error)
			}
		})
	}
}

// TestHandler_ErrorTaxonomy_NoMatchingMember verifies that when the local
// dispatch fails with contract.ErrNoMatchingMember (a peer that lost its
// matching member between gossip and forward), the handler classifies the
// response as the NO_COMPUTE_MEMBER_FOR_TAG/503/retryable trio.
func TestHandler_ErrorTaxonomy_NoMatchingMember(t *testing.T) {
	auth := newAEAD(t)
	noMemberErr := fmt.Errorf("%w: tags %q", contract.ErrNoMatchingMember, "python")

	cases := []struct {
		name string
		req  DispatchCalloutRequest
		fake *fakeLocalDispatcher
	}{
		{
			name: "processor",
			req: DispatchCalloutRequest{
				Kind: "processor", TenantID: "t", UserID: "u", TxID: "tx",
				Processor:      &spi.ProcessorDefinition{Name: "p", Type: "SCRIPT"},
				WorkflowName:   "w",
				TransitionName: "t",
				EntityMeta:     spi.EntityMeta{ID: "e"},
				Entity:         json.RawMessage(`{}`),
			},
			fake: &fakeLocalDispatcher{processorErr: noMemberErr},
		},
		{
			name: "criteria",
			req: DispatchCalloutRequest{
				Kind: "criteria", TenantID: "t", UserID: "u", TxID: "tx",
				Criterion:      json.RawMessage(`{"type":"eq"}`),
				Target:         "target",
				WorkflowName:   "w",
				TransitionName: "t",
				ProcessorName:  "p",
				EntityMeta:     spi.EntityMeta{ID: "e"},
				Entity:         json.RawMessage(`{}`),
			},
			fake: &fakeLocalDispatcher{criteriaErr: noMemberErr},
		},
		{
			name: "function",
			req: DispatchCalloutRequest{
				Kind: "function", TenantID: "t", UserID: "u", TxID: "tx",
				Function:       &spi.ScheduleFunction{Name: "fn", ResultKind: "Schedule"},
				WorkflowName:   "w",
				TransitionName: "t",
				EntityMeta:     spi.EntityMeta{ID: "e"},
				Entity:         json.RawMessage(`{}`),
			},
			fake: &fakeLocalDispatcher{functionErr: noMemberErr},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			handler := NewDispatchHandler(tc.fake, auth)
			mux := http.NewServeMux()
			handler.Register(mux)

			plain, _ := json.Marshal(tc.req)
			httpReq := signedRequest(t, auth, http.MethodPost, "/internal/dispatch/callout", plain)

			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, httpReq)

			var resp DispatchCalloutResponse
			if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
				t.Fatalf("decode response: %v", err)
			}
			if resp.Success {
				t.Fatal("expected success=false")
			}
			if resp.ErrorCode != common.ErrCodeNoComputeMemberForTag {
				t.Errorf("ErrorCode = %q, want %q", resp.ErrorCode, common.ErrCodeNoComputeMemberForTag)
			}
			if resp.ErrorStatus != http.StatusServiceUnavailable {
				t.Errorf("ErrorStatus = %d, want %d", resp.ErrorStatus, http.StatusServiceUnavailable)
			}
			if !resp.ErrorRetryable {
				t.Error("ErrorRetryable = false, want true")
			}
		})
	}
}

// TestHandler_UnknownCalloutKind covers the default branch of handleCallout:
// an unrecognized Kind must return 400 with a descriptive message (C2).
func TestHandler_UnknownCalloutKind(t *testing.T) {
	auth := newAEAD(t)
	handler := NewDispatchHandler(&fakeLocalDispatcher{}, auth)
	mux := http.NewServeMux()
	handler.Register(mux)

	plain, _ := json.Marshal(DispatchCalloutRequest{
		Kind: "bogus-kind", TenantID: "t", UserID: "u",
		EntityMeta: spi.EntityMeta{ID: "e"},
		Entity:     json.RawMessage(`{}`),
	})
	httpReq := signedRequest(t, auth, http.MethodPost, "/internal/dispatch/callout", plain)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httpReq)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "unknown callout kind") {
		t.Errorf("expected body to mention unknown callout kind, got %q", rec.Body.String())
	}
}
