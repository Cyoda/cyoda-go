package dispatch

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/cyoda-platform/cyoda-go/internal/clusterstatus"
	"github.com/cyoda-platform/cyoda-go/internal/lifecycle"
)

type fakeStatusSource struct {
	calls  int
	status clusterstatus.NodeStatus
}

func (f *fakeStatusSource) Status(context.Context) clusterstatus.NodeStatus {
	f.calls++
	return f.status
}

func sampleStatus() clusterstatus.NodeStatus {
	return clusterstatus.NodeStatus{
		State:    lifecycle.Normal,
		Database: true,
		Version:  "v1.2.3",
		Clock:    clusterstatus.Clock{Controllable: true, Version: 7, Writer: "node-a"},
	}
}

func nodeAuth(t *testing.T, nodeID string) *AEADPeerAuth {
	t.Helper()
	a, err := NewAEADPeerAuth(testSecret32, nodeID, 30*time.Second)
	if err != nil {
		t.Fatalf("NewAEADPeerAuth(%s): %v", nodeID, err)
	}
	return a
}

func statusMux(auth PeerAuth, src StatusSource) http.Handler {
	mux := http.NewServeMux()
	NewStatusHandler(auth, src).Register(mux)
	return mux
}

func statusServer(t *testing.T, auth PeerAuth, src StatusSource) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(statusMux(auth, src))
	t.Cleanup(srv.Close)
	return srv
}

func TestAskStatus_RoundTrip(t *testing.T) {
	src := &fakeStatusSource{status: sampleStatus()}
	srv := statusServer(t, nodeAuth(t, "node-b"), src)
	asker := NewHTTPForwarder(nodeAuth(t, "node-a"), time.Second).AllowLoopbackForTesting()
	got, err := asker.AskStatus(context.Background(), "node-b", srv.URL)
	if err != nil {
		t.Fatalf("AskStatus: %v", err)
	}
	if got != src.status {
		t.Fatalf("AskStatus = %+v, want %+v", got, src.status)
	}
}

func TestStatusRoute_RefusesEnvelopeForAnotherPath(t *testing.T) {
	src := &fakeStatusSource{status: sampleStatus()}
	rec := postSigned(t, statusMux(nodeAuth(t, "node-b"), src), "/internal/dispatch/callout", "node-b", StatusPath)
	if rec.Code != http.StatusForbidden || src.calls != 0 {
		t.Fatalf("status = %d, source calls = %d; want bare 403 and none", rec.Code, src.calls)
	}
}

func TestStatusRoute_RefusesEnvelopeForAnotherNode(t *testing.T) {
	src := &fakeStatusSource{status: sampleStatus()}
	rec := postSigned(t, statusMux(nodeAuth(t, "node-b"), src), StatusPath, "node-x", StatusPath)
	if rec.Code != http.StatusForbidden || src.calls != 0 {
		t.Fatalf("status = %d, source calls = %d; want bare 403 and none", rec.Code, src.calls)
	}
}

func TestStatusRoute_ReplayCacheFullIsSealedRefusal(t *testing.T) {
	inner := nodeAuth(t, testSelfNodeID)
	src := &fakeStatusSource{status: sampleStatus()}
	req, binding := signedRequestWithBinding(t, nodeAuth(t, testSelfNodeID), http.MethodPost, StatusPath, []byte("{}"))
	rec := httptest.NewRecorder()
	statusMux(replayFullAuth{inner}, src).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || src.calls != 0 {
		t.Fatalf("status = %d, source calls = %d; want 200 and none", rec.Code, src.calls)
	}
	plain, err := inner.OpenResponse(rec.Header(), binding, rec.Body.Bytes())
	if err != nil {
		t.Fatalf("answer does not open: %v", err)
	}
	var ans StatusAnswer
	if err := json.Unmarshal(plain, &ans); err != nil || ans.Refused == "" || ans.Status != nil {
		t.Fatalf("answer = %+v, %v; want a refusal", ans, err)
	}
}

func TestAskStatus_SealedRefusalIsError(t *testing.T) {
	src := &fakeStatusSource{status: sampleStatus()}
	srv := statusServer(t, replayFullAuth{nodeAuth(t, testSelfNodeID)}, src)
	_, err := NewHTTPForwarder(nodeAuth(t, testSelfNodeID), time.Second).AllowLoopbackForTesting().AskStatus(context.Background(), testSelfNodeID, srv.URL)
	if err == nil || !strings.Contains(err.Error(), "replay cache") {
		t.Fatalf("err = %v, want the replay-cache refusal", err)
	}
	if src.calls != 0 {
		t.Fatalf("source calls = %d, want none", src.calls)
	}
}

func TestAskStatus_AnswerWithoutStatusIsError(t *testing.T) {
	auth := nodeAuth(t, testSelfNodeID)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _, binding, err := auth.Verify(r)
		if err != nil {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		writeSealed(w, auth, binding, StatusAnswer{})
	}))
	t.Cleanup(srv.Close)
	_, err := NewHTTPForwarder(nodeAuth(t, testSelfNodeID), time.Second).AllowLoopbackForTesting().AskStatus(context.Background(), testSelfNodeID, srv.URL)
	if err == nil || !strings.Contains(err.Error(), "no status") {
		t.Fatalf("err = %v, want an answer-without-status error", err)
	}
}

func TestAskStatus_InvalidAnswerIsError(t *testing.T) {
	bad := sampleStatus()
	bad.State = "bogus"
	srv := statusServer(t, nodeAuth(t, "node-b"), &fakeStatusSource{status: bad})
	asker := NewHTTPForwarder(nodeAuth(t, "node-a"), time.Second).AllowLoopbackForTesting()
	if _, err := asker.AskStatus(context.Background(), "node-b", srv.URL); err == nil || !strings.Contains(err.Error(), "bogus") {
		t.Fatalf("err = %v, want the invalid state refused", err)
	}
}

func TestAskStatus_LoopbackRefusedWithoutOptIn(t *testing.T) {
	src := &fakeStatusSource{status: sampleStatus()}
	srv := statusServer(t, nodeAuth(t, "node-b"), src)
	_, err := NewHTTPForwarder(nodeAuth(t, "node-a"), time.Second).AskStatus(context.Background(), "node-b", srv.URL)
	var fe *ForwardError
	if !errors.As(err, &fe) || fe.Stage != StageNotConnected {
		t.Fatalf("err = %v, want a not-connected refusal of the loopback address", err)
	}
	if src.calls != 0 {
		t.Fatalf("source calls = %d, want none", src.calls)
	}
}

func TestAskStatus_HonoursContext(t *testing.T) {
	block := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-block }))
	t.Cleanup(func() { close(block); srv.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now() // realtime: measures that the call returns promptly
	_, err := NewHTTPForwarder(nodeAuth(t, testSelfNodeID), 10*time.Second).AllowLoopbackForTesting().AskStatus(ctx, testSelfNodeID, srv.URL)
	if err == nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want a deadline error", err)
	}
	if time.Since(start) > 3*time.Second { // realtime: as above
		t.Fatal("AskStatus outlived its context")
	}
}
