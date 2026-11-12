package dispatch_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/cluster/dispatch"
	internalgrpc "github.com/cyoda-platform/cyoda-go/internal/grpc"
)

// oversizedRequest is a hand-over whose sealed envelope is above the ceiling
// both ends read to. The entity is padded past it, which is the only field of a
// hand-over that can grow that far.
func oversizedRequest() dispatch.DispatchCalloutRequest {
	req := makeProcessorReq()
	req.Entity = json.RawMessage(`{"pad":"` + strings.Repeat("x", dispatch.MaxEnvelopeSize) + `"}`)
	return req
}

// sizeRunner is a local procedure that answers with an entity of a given size.
type sizeRunner struct{ data []byte }

func (r *sizeRunner) RunLocal(_ context.Context, call internalgrpc.Callout, _ int) internalgrpc.LocalResult {
	return internalgrpc.LocalResult{
		TriesUsed: 1,
		Result:    internalgrpc.CalloutResult{Entity: &spi.Entity{Meta: call.Source.Entity.Meta, Data: r.data}},
	}
}

// An envelope this node can prove is too large would be refused by every peer
// identically, so it is Terminal and nothing is sent: the connection is not
// opened, no try is used, and the callout does not become a retryable 503 that
// fails the same way on every peer. Spec §6: what the owner can prove before it
// connects is Terminal, not a lost answer.
func TestHTTPForwarder_OversizedHandOverIsProvedBeforeConnecting(t *testing.T) {
	var reached atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reached.Store(true)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	f := dispatch.NewHTTPForwarder(newTestPeerAuth(t), time.Second).AllowLoopbackForTesting()
	_, err := f.ForwardCallout(context.Background(), testNodeID, srv.URL, oversizedRequest())
	if err == nil {
		t.Fatal("an oversized hand-over was sent")
	}
	var fe *dispatch.ForwardError
	if !errors.As(err, &fe) {
		t.Fatalf("err = %v, want a ForwardError", err)
	}
	if fe.Stage != dispatch.StageBeforeConnect {
		t.Errorf("stage = %v, want StageBeforeConnect", fe.Stage)
	}
	if reached.Load() {
		t.Error("the peer was connected to although the hand-over could not fit an envelope")
	}
}

// The answer leg has the mirror check: a peer that writes more than the ceiling
// is refused for its size, and the error says so rather than blaming the
// cipher. The outcome is a lost answer either way; what this pins is that an
// operator reading the log is told what happened.
func TestHTTPForwarder_OversizedAnswerIsRefusedForItsSize(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", dispatch.DispatchContentType)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(bytes.Repeat([]byte{7}, dispatch.MaxEnvelopeSize+1))
	}))
	t.Cleanup(srv.Close)

	f := dispatch.NewHTTPForwarder(newTestPeerAuth(t), time.Second).AllowLoopbackForTesting()
	_, err := f.ForwardCallout(context.Background(), testNodeID, srv.URL, makeProcessorReq())
	if err == nil {
		t.Fatal("an oversized answer was accepted")
	}
	if !strings.Contains(err.Error(), "too large") {
		t.Errorf("err = %v, want it to name the size", err)
	}
	var fe *dispatch.ForwardError
	if !errors.As(err, &fe) || fe.Stage != dispatch.StageAfterConnect {
		t.Errorf("err = %v, want StageAfterConnect", err)
	}
}

// A hand-over that a compute member answers with more than the envelope holds
// is not answered 200 by the receiving node: the owner reads at most the
// ceiling, so those bytes would arrive truncated and the node would believe it
// had answered `ok` when nothing readable reached the owner. It refuses its own
// answer with a status instead, which the owner reads as the lost answer it is.
func TestDispatchHandler_RefusesToWriteAnOversizedAnswer(t *testing.T) {
	auth := newTestPeerAuth(t)
	runner := &sizeRunner{data: bytes.Repeat([]byte("x"), dispatch.MaxEnvelopeSize+1)}
	mux := http.NewServeMux()
	dispatch.NewDispatchHandler(runner, auth).Register(mux)

	req := makeProcessorReq()
	req.RequestID, req.TriesLeft, req.AnswerLimitMs, req.OwnerNodeID, req.Major = "rid-size", 1, 1000, "node-owner", 1
	plain, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	httpReq := signAndBuildRequest(t, auth, http.MethodPost, "/internal/dispatch/callout", plain)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httpReq)

	if rec.Code == http.StatusOK {
		t.Fatalf("the node answered 200 with an answer larger than the envelope (%d bytes written)", rec.Body.Len())
	}
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", rec.Code)
	}
}
