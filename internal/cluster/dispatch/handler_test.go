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
	"github.com/cyoda-platform/cyoda-go/internal/cluster/token"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/contract"
	internalgrpc "github.com/cyoda-platform/cyoda-go/internal/grpc"
)

// testOwnRetries and testOwnAnswerLimitMax are what a test pnode's OWN
// configuration says — CYODA_RETRY_FIXED_NUM_RETRIES's default of 3 plus the
// first try, and CYODA_CALLOUT_RESPONSE_TIMEOUT_MAX_MS's default. They bound
// what this node decides for itself; a hand-over's numbers are the owner's and
// are not held against them.
const (
	testOwnMaxTries       = 4
	testOwnAnswerLimitMax = 60 * time.Second
)

// fakeRunner is the local procedure of the pnode that receives a hand-over.
type fakeRunner struct {
	result   internalgrpc.LocalResult
	onRun    func(ctx context.Context)
	gotCall  internalgrpc.Callout
	gotTries int
	gotCtx   context.Context
	calls    int
}

func (f *fakeRunner) RunLocal(ctx context.Context, call internalgrpc.Callout, maxTries int) internalgrpc.LocalResult {
	f.calls++
	f.gotCtx, f.gotCall, f.gotTries = ctx, call, maxTries
	if f.onRun != nil {
		f.onRun(ctx)
	}
	return f.result
}

func newHandlerMux(t *testing.T, runner LocalRunner, auth PeerAuth) *http.ServeMux {
	t.Helper()
	mux := http.NewServeMux()
	NewDispatchHandler(runner, auth).Register(mux)
	return mux
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

// signedRequestWithBinding builds an AEAD-wrapped request ready for the handler
// to verify, and returns the binding its answer opens under.
func signedRequestWithBinding(t *testing.T, auth *AEADPeerAuth, method, path string, plain []byte) (*http.Request, ResponseBinding) {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	wire, binding, err := auth.Sign(req, plain)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	req.Body = io.NopCloser(bytes.NewReader(wire))
	req.ContentLength = int64(len(wire))
	return req, binding
}

// signedRequest is signedRequestWithBinding for a test that does not read the answer.
func signedRequest(t *testing.T, auth *AEADPeerAuth, method, path string, plain []byte) *http.Request {
	t.Helper()
	req, _ := signedRequestWithBinding(t, auth, method, path, plain)
	return req
}

// decodeSealed opens the handler's answer as the owner would.
func decodeSealed(t *testing.T, auth *AEADPeerAuth, binding ResponseBinding, rec *httptest.ResponseRecorder) DispatchCalloutResponse {
	t.Helper()
	plain, err := auth.OpenResponse(rec.Header(), binding, rec.Body.Bytes())
	if err != nil {
		t.Fatalf("the answer does not open under its request's binding: %v (status %d)", err, rec.Code)
	}
	var resp DispatchCalloutResponse
	if err := json.Unmarshal(plain, &resp); err != nil {
		t.Fatalf("decode answer: %v", err)
	}
	return resp
}

// postHandOver sends req as the owner would and opens the sealed answer.
func postHandOver(t *testing.T, mux *http.ServeMux, auth *AEADPeerAuth, req DispatchCalloutRequest) DispatchCalloutResponse {
	t.Helper()
	plain, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	httpReq, binding := signedRequestWithBinding(t, auth, http.MethodPost, "/internal/dispatch/callout", plain)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httpReq)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	return decodeSealed(t, auth, binding, rec)
}

func TestHandler_Outcomes(t *testing.T) {
	yes := true
	tests := []struct {
		name   string
		kind   string
		result internalgrpc.LocalResult
		check  func(t *testing.T, r DispatchCalloutResponse)
	}{
		{"processor answered", "processor",
			internalgrpc.LocalResult{TriesUsed: 1, Result: internalgrpc.CalloutResult{Entity: &spi.Entity{Data: []byte(`{"output":42}`)}}},
			func(t *testing.T, r DispatchCalloutResponse) {
				if r.Outcome != OutcomeOK || string(r.EntityData) != `{"output":42}` {
					t.Errorf("%+v", r)
				}
			}},
		{"criterion answered, with its reason", "criteria",
			internalgrpc.LocalResult{TriesUsed: 1, Result: internalgrpc.CalloutResult{Matches: true, Reason: "amount 5 below minimum 10"}},
			func(t *testing.T, r DispatchCalloutResponse) {
				if r.Outcome != OutcomeOK || r.Matches == nil || !*r.Matches || r.Reason != "amount 5 below minimum 10" {
					t.Errorf("%+v", r)
				}
			}},
		{"function answered", "function",
			internalgrpc.LocalResult{TriesUsed: 1, Result: internalgrpc.CalloutResult{Function: contract.FunctionResult{Kind: "Schedule", Value: json.RawMessage(`{"fireAfterMs":5}`)}}},
			func(t *testing.T, r DispatchCalloutResponse) {
				if r.Outcome != OutcomeOK || r.ResultKind != "Schedule" || string(r.Result) != `{"fireAfterMs":5}` {
					t.Errorf("%+v", r)
				}
			}},
		{"the cnode said it failed", "processor",
			internalgrpc.LocalResult{TriesUsed: 1, Failure: &contract.CalloutFailure{Kind: contract.MemberFailed, Message: "card declined", Retryable: &yes}},
			func(t *testing.T, r DispatchCalloutResponse) {
				if r.Outcome != "member_failed" || r.MemberError != "card declined" || r.MemberRetryable == nil || !*r.MemberRetryable {
					t.Errorf("%+v", r)
				}
			}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			auth := newAEAD(t)
			runner := &fakeRunner{result: tt.result}
			tt.check(t, postHandOver(t, newHandlerMux(t, runner, auth), auth, validRequest(t, tt.kind)))
			if runner.calls != 1 {
				t.Errorf("RunLocal called %d times", runner.calls)
			}
		})
	}
}

func TestHandler_GivesRunLocalTheOwnersTriesAndAnswerLimit(t *testing.T) {
	auth := newAEAD(t)
	runner := &fakeRunner{result: internalgrpc.LocalResult{
		TriesUsed: 2,
		Result:    internalgrpc.CalloutResult{Matches: true},
		Attempts:  []contract.CalloutAttempt{{MemberID: "m1", Kind: contract.NoAnswer, Cause: "DISPATCH_TIMEOUT: criteria dispatch timed out after 1500ms: no response"}},
	}}
	req := validRequest(t, "criteria") // triesLeft 2, answerLimitMs 1500
	resp := postHandOver(t, newHandlerMux(t, runner, auth), auth, req)

	if runner.gotTries != 2 {
		t.Errorf("maxTries = %d, want the hand-over's triesLeft", runner.gotTries)
	}
	if runner.gotCall.AnswerLimit != 1500*time.Millisecond {
		t.Errorf("AnswerLimit = %s, want the owner's", runner.gotCall.AnswerLimit)
	}
	if resp.Outcome != OutcomeOK || resp.TriesUsed == nil || *resp.TriesUsed != 2 || len(resp.Attempts) != 1 || resp.Attempts[0].MemberID != "m1" {
		t.Errorf("two tries in one exchange must be reported as two: %+v", resp)
	}
}

func TestHandler_BuildsTheCalloutFromTheRequest(t *testing.T) {
	auth := newAEAD(t)
	runner := &fakeRunner{result: internalgrpc.LocalResult{TriesUsed: 1, Result: internalgrpc.CalloutResult{Entity: &spi.Entity{Data: []byte(`{}`)}}}}
	req := validRequest(t, "processor") // owner-node, major 5, outer (outer-rid,3,1)
	postHandOver(t, newHandlerMux(t, runner, auth), auth, req)

	call := runner.gotCall
	if call.OwnerNodeID != "owner-node" || call.RequestID != "rid-1" || call.TxID != "tx-1" {
		t.Errorf("callout = %+v", call)
	}
	if len(call.Outer) != 1 || call.Outer[0] != (token.Pair{Callout: "outer-rid", Major: 3, Minor: 1}) {
		t.Errorf("Outer = %+v", call.Outer)
	}
	if major, minor := call.Number.Next(); major != 5 || minor != 1 {
		t.Errorf("first try numbered (%d,%d), want (5,1)", major, minor)
	}
	uc := spi.GetUserContext(runner.gotCtx)
	if uc == nil || uc.Tenant.ID != "tenant-1" || uc.UserID != "user-1" || uc.Kind != spi.PrincipalUser {
		t.Errorf("user context = %+v", uc)
	}
	if id, ok := PeerIdentityFromContext(runner.gotCtx); !ok || id.AuthMethod() != "aead-v1" {
		t.Errorf("peer identity = %+v, %v", id, ok)
	}
}

// The context the local procedure runs under carries exactly what the peer
// sent, and nothing more: the tenant is the wire's, named by its id alone. A
// peer is authenticated by the cluster-wide key and its identity names no
// tenant, so nothing else about the tenant may be invented here.
func TestHandler_ContextCarriesOnlyWhatThePeerSent(t *testing.T) {
	auth := newAEAD(t)
	runner := &fakeRunner{result: internalgrpc.LocalResult{TriesUsed: 1, Result: internalgrpc.CalloutResult{Entity: &spi.Entity{Data: []byte(`{}`)}}}}
	postHandOver(t, newHandlerMux(t, runner, auth), auth, validRequest(t, "processor"))

	uc := spi.GetUserContext(runner.gotCtx)
	if uc == nil {
		t.Fatal("no user context")
	}
	if uc.Tenant.Name != "" {
		t.Errorf("Tenant.Name = %q, want empty: the peer sent no tenant name", uc.Tenant.Name)
	}
	if len(uc.Roles) != 1 || uc.Roles[0] != "ROLE_USER" {
		t.Errorf("Roles = %v, want the request's", uc.Roles)
	}
}

// The pass the receiving pnode mints names the OWNER's node, not its own, so a
// callback from the cnode it hands the work to is routed to the pnode that holds
// the transaction. The real local procedure mints it, over a real signer.
func TestHandler_PassMintedOnThePeerNamesTheOwner(t *testing.T) {
	signer, err := token.NewSigner(testSecret32)
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}
	reg, cnode := attachedCnode(t, "tenant-1", "python")
	local := internalgrpc.NewProcessorDispatcher(reg, internalgrpc.NewRoundRobinSelector(reg),
		signer, 5*time.Second, testOwnAnswerLimitMax, 30*time.Second)

	auth := newAEAD(t)
	resp := postHandOver(t, newHandlerMux(t, local, auth), auth, validRequest(t, "processor"))
	if resp.Outcome != OutcomeOK {
		t.Fatalf("%+v", resp)
	}

	claims, err := signer.Verify(cnode.onlyPass(t))
	if err != nil {
		t.Fatalf("verify the pass the cnode was given: %v", err)
	}
	if claims.NodeID != "owner-node" {
		t.Errorf("pass NodeID = %q, want the owner's, not the receiving pnode's", claims.NodeID)
	}
	if claims.TxRef != "tx-1" || claims.Callout != "rid-1" {
		t.Errorf("pass TxRef/Callout = %q/%q", claims.TxRef, claims.Callout)
	}
	if claims.Major != 5 || claims.Minor != 1 {
		t.Errorf("pass numbered (%d,%d), want (5,1) — minor 1 under the hand-over's major", claims.Major, claims.Minor)
	}
	if len(claims.Outer) != 1 || claims.Outer[0] != (token.Pair{Callout: "outer-rid", Major: 3, Minor: 1}) {
		t.Errorf("pass Outer = %+v, want the hand-over's enclosing pairs", claims.Outer)
	}
}

// The handler holds a LocalRunner and nothing that could reach another pnode:
// with no cnode of its own it says so, and the owner asks the next pnode.
func TestHandler_NoLocalCnode_AnswersNoHandOff(t *testing.T) {
	auth := newAEAD(t)
	runner := &fakeRunner{result: internalgrpc.LocalResult{Failure: &contract.CalloutFailure{
		Kind: contract.NoHandOff, Code: common.ErrCodeNoComputeMemberForTag,
		Err: fmt.Errorf("%w: tags %q", contract.ErrNoMatchingMember, "python")}}}
	resp := postHandOver(t, newHandlerMux(t, runner, auth), auth, validRequest(t, "processor"))
	if resp.Outcome != "no_handoff" || resp.TriesUsed == nil || *resp.TriesUsed != 0 || resp.ErrorCode != common.ErrCodeNoComputeMemberForTag {
		t.Errorf("%+v", resp)
	}
}

func TestHandler_DiagnosticsOfTheTriesTravelBack(t *testing.T) {
	auth := newAEAD(t)
	runner := &fakeRunner{
		result: internalgrpc.LocalResult{TriesUsed: 1, Failure: &contract.CalloutFailure{Kind: contract.MemberFailed, Message: "card declined"}},
		onRun: func(ctx context.Context) {
			common.AddWarning(ctx, "processor myProcessor: slow")
			common.AddError(ctx, "processor myProcessor: card declined")
		},
	}
	resp := postHandOver(t, newHandlerMux(t, runner, auth), auth, validRequest(t, "processor"))
	if len(resp.Warnings) != 1 || resp.Warnings[0] != "processor myProcessor: slow" || len(resp.Errors) != 1 || resp.Errors[0] != "processor myProcessor: card declined" {
		t.Errorf("warnings %v, errors %v", resp.Warnings, resp.Errors)
	}
}

// What the peer logged for itself stays there: only client-safe text travels.
func TestHandler_InternalDetailStaysOnThePeer(t *testing.T) {
	auth := newAEAD(t)
	runner := &fakeRunner{result: internalgrpc.LocalResult{TriesUsed: 1,
		Failure:  &contract.CalloutFailure{Kind: contract.Terminal, Message: "auth context unavailable for dispatch", Err: errors.New("principal svc-7 at 10.0.0.5:5432")},
		Attempts: []contract.CalloutAttempt{{MemberID: "m1", Kind: contract.Terminal, Cause: "auth context unavailable for dispatch"}}}}
	mux := newHandlerMux(t, runner, auth)
	plain, _ := json.Marshal(validRequest(t, "processor"))
	httpReq, binding := signedRequestWithBinding(t, auth, http.MethodPost, "/internal/dispatch/callout", plain)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httpReq)
	opened, err := auth.OpenResponse(rec.Header(), binding, rec.Body.Bytes())
	if err != nil {
		t.Fatalf("OpenResponse: %v", err)
	}
	if strings.Contains(string(opened), "10.0.0.5") || strings.Contains(string(opened), "svc-7") {
		t.Errorf("the answer carries the peer's internal detail: %s", opened)
	}
}

// A replayed request keeps its bare 403. The owner reads it as a lost answer,
// which is what it could read before; an authenticated "nothing was handed
// over" for a replay would be a weapon — see ErrNonceReplayed.
func TestHandler_ReplayedRequest_IsABareForbidden(t *testing.T) {
	auth := newAEAD(t)
	runner := &fakeRunner{result: internalgrpc.LocalResult{TriesUsed: 1, Result: internalgrpc.CalloutResult{Entity: &spi.Entity{Data: []byte(`{}`)}}}}
	mux := newHandlerMux(t, runner, auth)

	plain, _ := json.Marshal(validRequest(t, "processor"))
	first, binding := signedRequestWithBinding(t, auth, http.MethodPost, "/internal/dispatch/callout", plain)
	wire, _ := io.ReadAll(first.Body)
	build := func() *http.Request {
		r := httptest.NewRequest(http.MethodPost, "/internal/dispatch/callout", bytes.NewReader(wire))
		r.Header.Set("Content-Type", DispatchContentType)
		r.Header.Set(DispatchTimestampHdr, first.Header.Get(DispatchTimestampHdr))
		return r
	}

	rec1 := httptest.NewRecorder()
	mux.ServeHTTP(rec1, build())
	if resp := decodeSealed(t, auth, binding, rec1); resp.Outcome != OutcomeOK {
		t.Fatalf("first: %+v", resp)
	}
	rec2 := httptest.NewRecorder()
	mux.ServeHTTP(rec2, build())
	if rec2.Code != http.StatusForbidden {
		t.Fatalf("replay: status %d, want 403", rec2.Code)
	}
	// The owner cannot read a "nothing was handed over" out of it: there is no
	// sealed answer bound to the request at all.
	if _, err := auth.OpenResponse(rec2.Header(), binding, rec2.Body.Bytes()); err == nil {
		t.Error("the replay was answered with something the owner can open as an outcome")
	}
	if runner.calls != 1 {
		t.Errorf("RunLocal called %d times: the replay reached a cnode", runner.calls)
	}
}

func TestHandler_FullReplayCache_IsAnAuthenticatedNoHandOff(t *testing.T) {
	auth := newAEAD(t)
	auth.nonces = newNonceCache(time.Minute, 1, time.Now)
	runner := &fakeRunner{result: internalgrpc.LocalResult{TriesUsed: 1, Result: internalgrpc.CalloutResult{Entity: &spi.Entity{Data: []byte(`{}`)}}}}
	mux := newHandlerMux(t, runner, auth)

	if resp := postHandOver(t, mux, auth, validRequest(t, "processor")); resp.Outcome != OutcomeOK {
		t.Fatalf("first: %+v", resp)
	}
	resp := postHandOver(t, mux, auth, validRequest(t, "processor"))
	if resp.Outcome != "no_handoff" || *resp.TriesUsed != 0 || runner.calls != 1 {
		t.Errorf("second: %+v, RunLocal calls %d", resp, runner.calls)
	}
}

func TestHandler_RequestThatCannotBeRun_IsAnAuthenticatedTerminal(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*DispatchCalloutRequest)
	}{
		{"unknown kind", func(r *DispatchCalloutRequest) { r.Kind = "bogus" }},
		{"entity of another tenant", func(r *DispatchCalloutRequest) { r.EntityMeta.TenantID = "tenant-2" }},
		{"entity with no tenant", func(r *DispatchCalloutRequest) { r.EntityMeta.TenantID = "" }},
		{"no tries", func(r *DispatchCalloutRequest) { r.TriesLeft = 0 }},
		{"no answer limit", func(r *DispatchCalloutRequest) { r.AnswerLimitMs = 0 }},
		{"no owner", func(r *DispatchCalloutRequest) { r.OwnerNodeID = "" }},
		{"entity that is JSON null", func(r *DispatchCalloutRequest) { r.Entity = json.RawMessage(`null`) }},
		{"more enclosing pairs than can be sane", func(r *DispatchCalloutRequest) {
			r.Outer = make([]WirePair, maxOuterPairs+1)
			for i := range r.Outer {
				r.Outer[i] = WirePair{Callout: fmt.Sprintf("outer-%d", i), Major: 1}
			}
		}},
		{"no tenant at all", func(r *DispatchCalloutRequest) { r.TenantID, r.EntityMeta.TenantID = "", "" }},
		// This one is refused by toCallout rather than validate: both refusals
		// must reach the owner in the same shape.
		{"a criterion that does not parse", func(r *DispatchCalloutRequest) {
			r.Kind, r.Processor = "criteria", nil
			r.Criterion = json.RawMessage(`{"function":7}`)
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			auth := newAEAD(t)
			runner := &fakeRunner{}
			req := validRequest(t, "processor")
			tt.mutate(&req)
			resp := postHandOver(t, newHandlerMux(t, runner, auth), auth, req)
			if resp.Outcome != "terminal" || resp.TriesUsed == nil || *resp.TriesUsed != 0 ||
				resp.ErrorStatus != http.StatusInternalServerError || resp.ErrorCode != common.ErrCodeServerError {
				t.Errorf("%+v", resp)
			}
			if runner.calls != 0 {
				t.Error("RunLocal was called for a request that must not run")
			}
			raw, _ := json.Marshal(resp)
			if strings.Contains(string(raw), "tenant-2") {
				t.Errorf("the answer names a peer-supplied tenant: %s", raw)
			}
		})
	}
}

// How many tries the hand-over may make and how long a cnode is given to answer
// are the OWNER's decisions: the receiving pnode runs them as sent, even where
// its own configuration would have chosen smaller ones. Holding them against its
// own settings would fail a serviceable callout whenever two nodes' settings
// differ — which they do through any rolling configuration change.
func TestHandler_RunsTheOwnersTriesAndAnswerLimitWhateverThisNodesSettings(t *testing.T) {
	auth := newAEAD(t)
	runner := &fakeRunner{result: internalgrpc.LocalResult{TriesUsed: 1,
		Result: internalgrpc.CalloutResult{Entity: &spi.Entity{Data: []byte(`{}`)}}}}

	req := validRequest(t, "processor")
	req.TriesLeft = testOwnMaxTries + 3
	req.AnswerLimitMs = testOwnAnswerLimitMax.Milliseconds() * 2

	if resp := postHandOver(t, newHandlerMux(t, runner, auth), auth, req); resp.Outcome != OutcomeOK {
		t.Fatalf("%+v", resp)
	}
	if runner.gotTries != req.TriesLeft {
		t.Errorf("maxTries = %d, want the owner's %d", runner.gotTries, req.TriesLeft)
	}
	if want := time.Duration(req.AnswerLimitMs) * time.Millisecond; runner.gotCall.AnswerLimit != want {
		t.Errorf("AnswerLimit = %s, want the owner's %s", runner.gotCall.AnswerLimit, want)
	}
}

func TestHandler_BodyThatDoesNotParse_IsAnAuthenticatedTerminal(t *testing.T) {
	auth := newAEAD(t)
	runner := &fakeRunner{}
	httpReq, binding := signedRequestWithBinding(t, auth, http.MethodPost, "/internal/dispatch/callout", []byte(`{"kind":`))
	rec := httptest.NewRecorder()
	newHandlerMux(t, runner, auth).ServeHTTP(rec, httpReq)
	if resp := decodeSealed(t, auth, binding, rec); resp.Outcome != "terminal" || runner.calls != 0 {
		t.Errorf("%+v", resp)
	}
}

func TestHandler_AnswerIsSealedForItsRequest(t *testing.T) {
	auth := newAEAD(t)
	runner := &fakeRunner{result: internalgrpc.LocalResult{TriesUsed: 1,
		Result: internalgrpc.CalloutResult{Entity: &spi.Entity{Data: []byte(`{"output":42}`)}}}}
	mux := newHandlerMux(t, runner, auth)

	plain, _ := json.Marshal(validRequest(t, "processor"))
	httpReq, binding := signedRequestWithBinding(t, auth, http.MethodPost, "/internal/dispatch/callout", plain)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httpReq)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); got != DispatchContentType {
		t.Errorf("Content-Type = %q, want %q", got, DispatchContentType)
	}
	if bytes.Contains(rec.Body.Bytes(), []byte("output")) {
		t.Error("the answer is on the wire in the clear")
	}
	if resp := decodeSealed(t, auth, binding, rec); string(resp.EntityData) != `{"output":42}` {
		t.Errorf("EntityData = %s", resp.EntityData)
	}
}

func TestHandler_MissingAEADHeaders(t *testing.T) {
	mux := newHandlerMux(t, &fakeRunner{}, newAEAD(t))

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
	mux := newHandlerMux(t, &fakeRunner{}, newAEAD(t))

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

// The peer reconstructs a UserContext carrying the SAME principal Kind the
// originating node had. Without it, the peer's local dispatch — which calls
// AttachAuthContext just like single-node dispatch — would fail the callout
// closed on an unset Kind, whatever the originating principal's real kind was.
func TestHandler_ReconstructsPrincipalKindInContext(t *testing.T) {
	for _, kind := range []spi.PrincipalKind{spi.PrincipalUser, spi.PrincipalService, spi.PrincipalSystem} {
		t.Run(string(kind), func(t *testing.T) {
			auth := newAEAD(t)
			runner := &fakeRunner{result: internalgrpc.LocalResult{TriesUsed: 1,
				Result: internalgrpc.CalloutResult{Entity: &spi.Entity{Data: []byte(`{}`)}}}}
			req := validRequest(t, "processor")
			req.PrincipalKind = kind
			postHandOver(t, newHandlerMux(t, runner, auth), auth, req)

			uc := spi.GetUserContext(runner.gotCtx)
			if uc == nil {
				t.Fatal("expected UserContext in the local procedure's ctx")
			}
			if uc.Kind != kind {
				t.Errorf("Kind = %q, want %q", uc.Kind, kind)
			}
		})
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
