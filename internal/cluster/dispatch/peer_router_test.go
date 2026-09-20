package dispatch

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/contract"
	internalgrpc "github.com/cyoda-platform/cyoda-go/internal/grpc"
)

// inOrderSelector picks the first candidate, so "selector order" is list order.
type inOrderSelector struct{}

func (inOrderSelector) Select(c []contract.NodeInfo) (contract.NodeInfo, error) {
	if len(c) == 0 {
		return contract.NodeInfo{}, errors.New("no candidates")
	}
	return c[0], nil
}

// answeringForwarder answers every hand-over with resp/err and keeps the request.
type answeringForwarder struct {
	resp  *DispatchCalloutResponse
	err   error
	got   DispatchCalloutRequest
	calls int
}

func (f *answeringForwarder) ForwardCallout(_ context.Context, _ string, req DispatchCalloutRequest) (*DispatchCalloutResponse, error) {
	f.calls++
	f.got = req
	return f.resp, f.err
}

func newTestRouter(t *testing.T, registry contract.NodeRegistry, fwd DispatchForwarder) *PeerRouter {
	t.Helper()
	r, err := NewPeerRouter(registry, "self-node", inOrderSelector{}, fwd, nil)
	if err != nil {
		t.Fatalf("NewPeerRouter: %v", err)
	}
	return r
}

func node(id string, alive bool, tenant string, tags ...string) contract.NodeInfo {
	return contract.NodeInfo{NodeID: id, Addr: "http://" + id, Alive: alive, Tags: map[string][]string{tenant: tags}}
}

func TestPeerRouter_Peers(t *testing.T) {
	registry := &stubNodeRegistry{nodes: []contract.NodeInfo{
		node("self-node", true, "tenant-1", "python"),
		node("dead", false, "tenant-1", "python"),
		node("other-tenant", true, "tenant-2", "python"),
		node("other-tag", true, "tenant-1", "java"),
		node("peer-b", true, "tenant-1", "python", "ml"),
		node("peer-a", true, "tenant-1", "python"),
		{NodeID: "no-list-yet", Addr: "http://x", Alive: true, Tags: map[string][]string{}},
	}}
	got := newTestRouter(t, registry, &answeringForwarder{}).Peers("tenant-1", "python,go")
	if len(got) != 2 || got[0].NodeID != "peer-b" || got[1].NodeID != "peer-a" {
		t.Fatalf("Peers = %+v, want [peer-b peer-a] — alive, not self, this tenant, any tag overlapping, in selector order", got)
	}
}

type failingRegistry struct{ stubNodeRegistry }

func (failingRegistry) List(context.Context) ([]contract.NodeInfo, error) {
	return nil, errors.New("registry down")
}

func TestPeerRouter_Peers_RegistryErrorIsNoPeers(t *testing.T) {
	if got := newTestRouter(t, &failingRegistry{}, &answeringForwarder{}).Peers("tenant-1", "python"); len(got) != 0 {
		t.Fatalf("Peers = %+v, want none", got)
	}
}

type signalRegistry struct {
	stubNodeRegistry
	sig *common.ChangeSignal
}

func (r *signalRegistry) Changed() <-chan struct{} { return r.sig.Changed() }

func TestPeerRouter_Changed_IsTheNodeRegistrys(t *testing.T) {
	registry := &signalRegistry{sig: common.NewChangeSignal()}
	ch := newTestRouter(t, registry, &answeringForwarder{}).Changed()
	select {
	case <-ch:
		t.Fatal("closed before any change")
	default:
	}
	registry.sig.Fire()
	select {
	case <-ch:
	default:
		t.Fatal("not closed by the registry's change")
	}
}

func TestHandOver_Classification(t *testing.T) {
	okResp := &DispatchCalloutResponse{Outcome: OutcomeOK, TriesUsed: intPtr(1), EntityData: []byte(`{"out":1}`)}
	tests := []struct {
		name  string
		resp  *DispatchCalloutResponse
		err   error
		check func(t *testing.T, a HandOverAnswer)
	}{
		{"answered", okResp, nil, func(t *testing.T, a HandOverAnswer) {
			if a.Failure != nil || !a.Connected || a.TriesUsed != 1 || string(a.Result.Entity.Data) != `{"out":1}` {
				t.Errorf("%+v", a)
			}
		}},
		{"lost: an error of no known stage", nil, errors.New("read tcp: connection reset"), assertLost},
		{"lost: after the connection was opened", nil, &ForwardError{Stage: StageAfterConnect, Err: errors.New("peer returned 502")}, assertLost},
		{"not connected: no try", nil, &ForwardError{Stage: StageNotConnected, Err: errors.New("dial tcp: connection refused")}, assertNotConnected},
		{"not connected: the address was refused", nil, &ForwardError{Stage: StageNotConnected, Err: ErrForbiddenPeerAddress}, assertNotConnected},
		{"proved before connecting: terminal", nil, &ForwardError{Stage: StageBeforeConnect, Err: errors.New("sign body")}, func(t *testing.T, a HandOverAnswer) {
			if a.Connected || a.TriesUsed != 0 || a.Failure == nil || a.Failure.Kind != contract.Terminal {
				t.Errorf("%+v", a)
			}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fwd := &answeringForwarder{resp: tt.resp, err: tt.err}
			a := newTestRouter(t, &stubNodeRegistry{}, fwd).HandOver(testContext(), node("peer-1", true, "tenant-1", "python"), ownerCallout(t, "processor"), 3, 2)
			tt.check(t, a)
			if fwd.calls != 1 {
				t.Errorf("forwarder called %d times", fwd.calls)
			}
		})
	}
}

// assertNotConnected: nothing left this pnode, so no try is used and the owner's
// loop reads it as it reads a peer with no compute member — ask the next peer.
func assertNotConnected(t *testing.T, a HandOverAnswer) {
	t.Helper()
	if a.Connected || a.TriesUsed != 0 || a.Failure == nil || a.Failure.Kind != contract.NoHandOff || len(a.Attempts) != 0 {
		t.Errorf("%+v", a)
	}
}

func TestHandOver_SendsTheHandOverFields(t *testing.T) {
	fwd := &answeringForwarder{resp: &DispatchCalloutResponse{Outcome: OutcomeOK, TriesUsed: intPtr(1)}}
	call := ownerCallout(t, "criteria")
	call.OwnerNodeID = "" // the router knows who the owner is
	newTestRouter(t, &stubNodeRegistry{}, fwd).HandOver(testContext(), node("peer-1", true, "tenant-1", "python"), call, 3, 2)
	if fwd.got.TriesLeft != 3 || fwd.got.Major != 2 || fwd.got.OwnerNodeID != "self-node" || fwd.got.RequestID != "rid-1" || fwd.got.AnswerLimitMs != 1500 {
		t.Errorf("request = %+v", fwd.got)
	}
}

func TestHandOver_NothingToHandOver_IsTerminal_NothingSent(t *testing.T) {
	fwd := &answeringForwarder{}
	router := newTestRouter(t, &stubNodeRegistry{}, fwd)
	peer := node("peer-1", true, "tenant-1", "python")

	noUser := router.HandOver(context.Background(), peer, ownerCallout(t, "processor"), 3, 2)
	noTries := router.HandOver(testContext(), peer, ownerCallout(t, "processor"), 0, 2)
	for name, a := range map[string]HandOverAnswer{"no user context": noUser, "no tries left": noTries} {
		if a.Connected || a.TriesUsed != 0 || a.Failure == nil || a.Failure.Kind != contract.Terminal {
			t.Errorf("%s: %+v", name, a)
		}
	}
	if fwd.calls != 0 {
		t.Errorf("forwarder called %d times", fwd.calls)
	}
}

func TestHandOver_AddsThePeersErrorsToTheRequestDiagnostics(t *testing.T) {
	no := false
	fwd := &answeringForwarder{resp: &DispatchCalloutResponse{Outcome: "member_failed", TriesUsed: intPtr(1),
		MemberError: "card declined", MemberRetryable: &no,
		Warnings: []string{"processor p: slow"}, Errors: []string{"processor p: card declined"}}}
	ctx := common.WithDiagnostics(testContext())
	a := newTestRouter(t, &stubNodeRegistry{}, fwd).HandOver(ctx, node("peer-1", true, "tenant-1", "python"), ownerCallout(t, "processor"), 1, 1)

	if got := common.GetDiagnostics(ctx).GetErrors(); len(got) != 1 || got[0] != "processor p: card declined" {
		t.Errorf("errors = %v", got)
	}
	if len(a.Warnings) != 1 || a.Warnings[0] != "processor p: slow" {
		t.Errorf("Warnings = %v — returned for the caller to add, as the seam says", a.Warnings)
	}
	if got := common.GetDiagnostics(ctx).GetWarnings(); len(got) != 0 {
		t.Errorf("warnings were added twice over: %v", got)
	}
}

// --- why a peer was not connected to, by class ---

// timeoutErr is a net.Error that timed out, as the dialer reports a connect
// timeout it hit.
type timeoutErr struct{}

func (timeoutErr) Error() string   { return "i/o timeout" }
func (timeoutErr) Timeout() bool   { return true }
func (timeoutErr) Temporary() bool { return true }

// The class of a failure to connect is what an operator acts on — a refused
// address is a misconfiguration, a timeout is a network, a refused port is a
// dead peer — and today they are all one log line. The vocabulary is closed:
// the error's own text names the address and never becomes an attribute.
func TestNotConnectedReason(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want string
	}{
		{"the address guard refused it", ErrForbiddenPeerAddress, "address_refused"},
		{"the address guard, as the forwarder reports it",
			&ForwardError{Stage: StageNotConnected, Err: fmt.Errorf("validate peer address: %w", ErrForbiddenPeerAddress)}, "address_refused"},
		{"the name does not resolve", &net.DNSError{Err: "no such host", Name: "peer-1", IsNotFound: true}, "dns"},
		{"a name lookup that timed out is reported as the lookup it was",
			&net.DNSError{Err: "i/o timeout", Name: "peer-1", IsTimeout: true}, "dns"},
		{"the connect timeout ran out", &net.OpError{Op: "dial", Err: timeoutErr{}}, "timeout"},
		{"nothing is listening", &net.OpError{Op: "dial", Err: os.NewSyscallError("connect", syscall.ECONNREFUSED)}, "refused"},
		{"the host cannot be reached", &net.OpError{Op: "dial", Err: os.NewSyscallError("connect", syscall.EHOSTUNREACH)}, "unreachable"},
		{"the network cannot be reached", &net.OpError{Op: "dial", Err: os.NewSyscallError("connect", syscall.ENETUNREACH)}, "unreachable"},
		{"anything else", errors.New("something the socket did not name"), "other"},
		{"no error at all", nil, "other"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := notConnectedReason(tt.err); got != tt.want {
				t.Errorf("notConnectedReason = %q, want %q", got, tt.want)
			}
		})
	}
}

// --- what a peer writes is bounded before it reaches the client ---

func TestHandOver_BoundsThePeersDiagnostics(t *testing.T) {
	long := strings.Repeat("é", maxPeerDiagnosticRunes+10)
	many := make([]string, 0, maxPeerDiagnostics+4)
	attempts := make([]WireAttempt, 0, maxPeerDiagnostics+4)
	for i := range maxPeerDiagnostics + 4 {
		many = append(many, fmt.Sprintf("line %d", i))
		attempts = append(attempts, WireAttempt{MemberID: long, Kind: "member_failed", Cause: long})
	}
	fwd := &answeringForwarder{resp: &DispatchCalloutResponse{Outcome: "member_failed", TriesUsed: intPtr(1),
		MemberError: "declined", Warnings: append([]string{long}, many...), Errors: many, Attempts: attempts}}
	ctx := common.WithDiagnostics(testContext())
	a := newTestRouter(t, &stubNodeRegistry{}, fwd).HandOver(ctx, node("peer-1", true, "tenant-1", "python"), ownerCallout(t, "processor"), 1, 1)

	if len(a.Warnings) != maxPeerDiagnostics+1 || a.Warnings[len(a.Warnings)-1] != peerDiagnosticsOmittedWarning {
		t.Errorf("Warnings = %d entries, want %d and the note that the rest was omitted: %v", len(a.Warnings), maxPeerDiagnostics+1, a.Warnings)
	}
	if got := []rune(a.Warnings[0]); len(got) != maxPeerDiagnosticRunes+1 || got[len(got)-1] != '…' {
		t.Errorf("a long warning was not cut with a mark: %d runes", len(got))
	}
	if got := common.GetDiagnostics(ctx).GetErrors(); len(got) != maxPeerDiagnostics {
		t.Errorf("errors = %d entries, want %d", len(got), maxPeerDiagnostics)
	}
	if len(a.Attempts) != maxPeerDiagnostics {
		t.Errorf("Attempts = %d entries, want %d", len(a.Attempts), maxPeerDiagnostics)
	}
	for _, at := range a.Attempts {
		if len([]rune(at.Cause)) != maxPeerDiagnosticRunes+1 || len([]rune(at.MemberID)) != maxPeerDiagnosticRunes+1 {
			t.Errorf("an attempt's text was not cut: %d/%d runes", len([]rune(at.MemberID)), len([]rune(at.Cause)))
		}
	}
}

// An answer that was not believed relays none of the peer's text, so there is
// nothing left for a note about what was left out to refer to.
func TestHandOver_LostAnswerGetsNoOmittedNote(t *testing.T) {
	many := make([]string, 0, maxPeerDiagnostics+4)
	for i := range maxPeerDiagnostics + 4 {
		many = append(many, fmt.Sprintf("line %d", i))
	}
	// triesUsed above triesLeft: the answer is not believed at all.
	fwd := &answeringForwarder{resp: &DispatchCalloutResponse{Outcome: OutcomeOK, TriesUsed: intPtr(9),
		EntityData: []byte(`{}`), Warnings: many}}
	a := newTestRouter(t, &stubNodeRegistry{}, fwd).HandOver(testContext(), node("peer-1", true, "tenant-1", "python"), ownerCallout(t, "processor"), 1, 1)

	assertLost(t, a)
	if len(a.Warnings) != 0 {
		t.Errorf("Warnings = %v, want none: nothing of the peer's text was relayed", a.Warnings)
	}
}

func TestHandOver_AnswerWithinTheBoundsGetsNoNote(t *testing.T) {
	fwd := &answeringForwarder{resp: &DispatchCalloutResponse{Outcome: OutcomeOK, TriesUsed: intPtr(1),
		EntityData: []byte(`{}`), Warnings: []string{"slow"}}}
	a := newTestRouter(t, &stubNodeRegistry{}, fwd).HandOver(testContext(), node("peer-1", true, "tenant-1", "python"), ownerCallout(t, "processor"), 1, 1)
	if len(a.Warnings) != 1 || a.Warnings[0] != "slow" {
		t.Errorf("Warnings = %v", a.Warnings)
	}
}

func TestHandOver_PeerErrorCodeThisNodeDoesNotKnowIsNotRelayed(t *testing.T) {
	fwd := &answeringForwarder{resp: &DispatchCalloutResponse{Outcome: "terminal", TriesUsed: intPtr(1),
		ErrorCode: "MADE_UP_CODE", ErrorStatus: 418, ErrorRetryable: true,
		Attempts: []WireAttempt{{MemberID: "m1", Kind: "terminal", Cause: "bad payload"}}}}
	a := newTestRouter(t, &stubNodeRegistry{}, fwd).HandOver(testContext(), node("peer-1", true, "tenant-1", "python"), ownerCallout(t, "processor"), 1, 1)
	if a.Failure == nil || a.Failure.Kind != contract.Terminal {
		t.Fatalf("Failure = %+v", a.Failure)
	}
	if a.Failure.Code != "" || strings.Contains(a.Failure.Message, "MADE_UP_CODE") {
		t.Errorf("a code this build does not define reached the client: %+v", a.Failure)
	}
}

func TestHandOver_PeerErrorCodeThisNodeKnowsSurvives(t *testing.T) {
	fwd := &answeringForwarder{resp: &DispatchCalloutResponse{Outcome: "terminal", TriesUsed: intPtr(1),
		ErrorCode: common.ErrCodeScheduleFunctionInvalidResult, ErrorStatus: http.StatusInternalServerError,
		Attempts: []WireAttempt{{MemberID: "m1", Kind: "terminal", Cause: "not a schedule"}}}}
	a := newTestRouter(t, &stubNodeRegistry{}, fwd).HandOver(testContext(), node("peer-1", true, "tenant-1", "python"), ownerCallout(t, "processor"), 1, 1)
	if a.Failure == nil || a.Failure.Code != common.ErrCodeScheduleFunctionInvalidResult {
		t.Fatalf("Failure = %+v, want the peer's classification kept", a.Failure)
	}
}

// --- over the wire ---

func realRouter(t *testing.T, loopback bool) *PeerRouter {
	t.Helper()
	fwd := NewHTTPForwarder(newAEAD(t), 5*time.Second)
	if loopback {
		fwd = fwd.AllowLoopbackForTesting()
	}
	return newTestRouter(t, &stubNodeRegistry{}, fwd)
}

func TestHandOver_PeerRefusesConnection_NoTryUsed(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	addr := srv.URL
	srv.Close() // the port now refuses connections

	assertNotConnected(t, realRouter(t, true).HandOver(testContext(), contract.NodeInfo{NodeID: "gone", Addr: addr}, ownerCallout(t, "processor"), 3, 1))
}

func TestHandOver_ForbiddenAddress_IsNotConnected_NoTryUsed(t *testing.T) {
	assertNotConnected(t, realRouter(t, false).HandOver(testContext(), contract.NodeInfo{NodeID: "p", Addr: "http://127.0.0.1:9"}, ownerCallout(t, "processor"), 3, 1))
}

func TestHandOver_OverTheWire_BadAnswersAreNoAnswer(t *testing.T) {
	peerAuth := newAEAD(t)
	var replay []byte
	tests := []struct {
		name   string
		answer func(w http.ResponseWriter, binding ResponseBinding)
	}{
		{"403 before anything was done", func(w http.ResponseWriter, _ ResponseBinding) { http.Error(w, "forbidden", http.StatusForbidden) }},
		{"500 from panic recovery", func(w http.ResponseWriter, _ ResponseBinding) { http.Error(w, "boom", http.StatusInternalServerError) }},
		{"502 from an intermediary", func(w http.ResponseWriter, _ ResponseBinding) { http.Error(w, "bad gateway", http.StatusBadGateway) }},
		{"200 in the clear", func(w http.ResponseWriter, _ ResponseBinding) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"outcome":"no_handoff","triesUsed":0}`))
		}},
		{"sealed, truncated", func(w http.ResponseWriter, b ResponseBinding) {
			wire, _ := peerAuth.SealResponse(w.Header(), b, []byte(`{"outcome":"no_handoff","triesUsed":0}`))
			_, _ = w.Write(wire[:len(wire)-5])
		}},
		{"sealed for an earlier request", func(w http.ResponseWriter, b ResponseBinding) {
			wire, _ := peerAuth.SealResponse(w.Header(), b, []byte(`{"outcome":"no_handoff","triesUsed":0}`))
			if replay == nil {
				replay = wire
			}
			_, _ = w.Write(replay)
		}},
		{"sealed, without an outcome", func(w http.ResponseWriter, b ResponseBinding) {
			wire, _ := peerAuth.SealResponse(w.Header(), b, []byte(`{"success":true}`))
			_, _ = w.Write(wire)
		}},
		{"sealed, not JSON", func(w http.ResponseWriter, b ResponseBinding) {
			wire, _ := peerAuth.SealResponse(w.Header(), b, []byte(`<html>`))
			_, _ = w.Write(wire)
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _, binding, err := peerAuth.Verify(r)
				if err != nil {
					t.Errorf("Verify: %v", err)
				}
				tt.answer(w, binding)
			}))
			defer srv.Close()
			router := realRouter(t, true)
			peer := contract.NodeInfo{NodeID: "p", Addr: srv.URL}
			if tt.name == "sealed for an earlier request" {
				// the first answer is genuine — a sealed no_handoff — and is believed
				if first := router.HandOver(testContext(), peer, ownerCallout(t, "processor"), 3, 1); first.Connected || first.TriesUsed != 0 {
					t.Fatalf("first answer: %+v", first)
				}
			}
			assertLost(t, router.HandOver(testContext(), peer, ownerCallout(t, "processor"), 3, 1))
		})
	}
}

// The cnode's own message and verdict reach the owner through a hand-over, and
// the peer's two tries are counted as two.
func TestHandOver_ThroughTheHandler_MemberMessageAndVerdictSurvive(t *testing.T) {
	yes := true
	peerAuth := newAEAD(t)
	runner := &fakeRunner{result: internalgrpc.LocalResult{
		TriesUsed: 2,
		Failure:   &contract.CalloutFailure{Kind: contract.MemberFailed, Message: "card declined", Retryable: &yes},
		Attempts: []contract.CalloutAttempt{
			{MemberID: "m1", Kind: contract.NoHandOff, Cause: "COMPUTE_MEMBER_DISCONNECTED: processor compute member disconnected"},
			{MemberID: "m2", Kind: contract.MemberFailed, Cause: "card declined"},
		}}}
	srv := httptest.NewServer(newHandlerMux(t, runner, peerAuth))
	defer srv.Close()

	a := realRouter(t, true).HandOver(testContext(), contract.NodeInfo{NodeID: "peer-1", Addr: srv.URL}, ownerCallout(t, "processor"), 3, 4)

	if a.Failure == nil || a.Failure.Kind != contract.MemberFailed || a.Failure.Message != "card declined" || a.Failure.Retryable == nil || !*a.Failure.Retryable {
		t.Fatalf("Failure = %+v", a.Failure)
	}
	if !a.Connected || a.TriesUsed != 2 || len(a.Attempts) != 2 || a.Attempts[1].MemberID != "m2" {
		t.Errorf("%+v", a)
	}
	if runner.gotTries != 3 || runner.gotCall.OwnerNodeID != "self-node" {
		t.Errorf("peer got tries=%d owner=%q", runner.gotTries, runner.gotCall.OwnerNodeID)
	}
	if major, minor := runner.gotCall.Number.Next(); major != 4 || minor != 1 {
		t.Errorf("peer numbers its tries (%d,%d), want (4,1)", major, minor)
	}
}

func TestPeerRouter_CountsHandOversByOutcome(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })

	fwd := &answeringForwarder{}
	router, err := NewPeerRouter(&stubNodeRegistry{}, "self-node", inOrderSelector{}, fwd, mp.Meter("test"))
	if err != nil {
		t.Fatalf("NewPeerRouter: %v", err)
	}
	peer := node("peer-1", true, "tenant-1", "python")
	script := []struct {
		resp *DispatchCalloutResponse
		err  error
	}{
		{&DispatchCalloutResponse{Outcome: OutcomeOK, TriesUsed: intPtr(1), EntityData: []byte(`{}`)}, nil},
		{&DispatchCalloutResponse{Outcome: OutcomeOK, TriesUsed: intPtr(1), EntityData: []byte(`{}`)}, nil},
		{&DispatchCalloutResponse{Outcome: "no_handoff", TriesUsed: intPtr(0)}, nil},
		{&DispatchCalloutResponse{Outcome: "member_failed", TriesUsed: intPtr(1), MemberError: "x"}, nil},
		{&DispatchCalloutResponse{Outcome: "terminal", TriesUsed: intPtr(1)}, nil},
		{nil, errors.New("reset")},
		{nil, &ForwardError{Stage: StageNotConnected, Err: errors.New("refused")}},
	}
	for _, s := range script {
		fwd.resp, fwd.err = s.resp, s.err
		router.HandOver(testContext(), peer, ownerCallout(t, "processor"), 3, 1)
	}

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect: %v", err)
	}
	got := map[string]int64{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != "cyoda.callout.handovers" {
				continue
			}
			for _, dp := range m.Data.(metricdata.Sum[int64]).DataPoints {
				outcome, _ := dp.Attributes.Value("outcome")
				got[outcome.AsString()] = dp.Value
			}
		}
	}
	want := map[string]int64{"ok": 2, "no_handoff": 1, "member_failed": 1, "terminal": 1, "no_answer": 1, "not_connected": 1}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("outcome %q = %d, want %d (all: %v)", k, got[k], v, got)
		}
	}
}
