package dispatch

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/contract"
	internalgrpc "github.com/cyoda-platform/cyoda-go/internal/grpc"
)

// TestIntegration_HandOver_FullFlow walks a hand-over the whole way over the
// wire: the owner's router seals the request, the peer's dispatch handler opens
// it, runs its local procedure and seals what came back, and the owner reads
// the answer. The pieces are covered on their own (handover_test.go builds and
// reads, handler_test.go serves, forwarder_test.go carries); this pins the
// three kinds' results surviving the round trip composed.
func TestIntegration_HandOver_FullFlow(t *testing.T) {
	tests := []struct {
		kind   string
		result internalgrpc.CalloutResult
		check  func(t *testing.T, res internalgrpc.CalloutResult)
	}{
		{
			kind:   "processor",
			result: internalgrpc.CalloutResult{Entity: &spi.Entity{Meta: testEntity().Meta, Data: []byte(`{"result":"from-peer-processor"}`)}},
			check: func(t *testing.T, res internalgrpc.CalloutResult) {
				if res.Entity == nil || string(res.Entity.Data) != `{"result":"from-peer-processor"}` {
					t.Fatalf("entity = %+v, want the peer's data", res.Entity)
				}
			},
		},
		{
			kind:   "criteria",
			result: internalgrpc.CalloutResult{Matches: true, Reason: "amount above the minimum"},
			check: func(t *testing.T, res internalgrpc.CalloutResult) {
				if !res.Matches || res.Reason != "amount above the minimum" {
					t.Fatalf("matches = %v, reason = %q, want the peer's verdict", res.Matches, res.Reason)
				}
			},
		},
		{
			kind:   "function",
			result: internalgrpc.CalloutResult{Function: contract.FunctionResult{Kind: "Schedule", Value: json.RawMessage(`{"fireAfterMs":5}`)}},
			check: func(t *testing.T, res internalgrpc.CalloutResult) {
				if res.Function.Kind != "Schedule" || string(res.Function.Value) != `{"fireAfterMs":5}` {
					t.Fatalf("function = %+v, want the peer's result", res.Function)
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.kind, func(t *testing.T) {
			peerAuth := newAEAD(t)
			runner := &fakeRunner{result: internalgrpc.LocalResult{TriesUsed: 1, Result: tt.result}}
			peer := httptest.NewServer(newHandlerMux(t, runner, peerAuth))
			defer peer.Close()

			a := realRouter(t, true).HandOver(testContext(), contract.NodeInfo{NodeID: testSelfNodeID, Addr: peer.URL}, ownerCallout(t, tt.kind), 3, 1)
			if a.Failure != nil {
				t.Fatalf("Failure = %+v, want the peer's answer", a.Failure)
			}
			if !a.Connected || a.TriesUsed != 1 || a.Result == nil {
				t.Fatalf("%+v", a)
			}
			tt.check(t, *a.Result)
		})
	}
}

// ctxRunner is a peer's local procedure that runs until its context ends, and
// reports that it ended.
type ctxRunner struct {
	started chan struct{}
	ended   chan error
}

func (r *ctxRunner) RunLocal(ctx context.Context, _ internalgrpc.Callout, _ int) internalgrpc.LocalResult {
	close(r.started)
	select {
	case <-ctx.Done():
	case <-time.After(10 * time.Second): // bounded, so a context that never ends fails the test instead of hanging it
	}
	r.ended <- ctx.Err()
	return internalgrpc.LocalResult{CtxErr: ctx.Err()}
}

// The owner calling its callout off — the caller went away, or a callback's
// failed write marked the scope the callout was made from — ends a hand-over in
// progress on the peer: the peer's local procedure sees its context end and
// makes no further try.
func TestIntegration_HandOver_OwnersCancellationEndsThePeersRun(t *testing.T) {
	peerAuth := newAEAD(t)
	runner := &ctxRunner{started: make(chan struct{}), ended: make(chan error, 1)}
	peer := httptest.NewServer(newHandlerMux(t, runner, peerAuth))
	defer peer.Close()

	ctx, cancel := context.WithCancel(testContext())
	done := make(chan HandOverAnswer, 1)
	go func() {
		done <- realRouter(t, true).HandOver(ctx, contract.NodeInfo{NodeID: testSelfNodeID, Addr: peer.URL}, ownerCallout(t, "processor"), 3, 1)
	}()
	select {
	case <-runner.started:
	case <-time.After(5 * time.Second):
		t.Fatal("the peer never started its local procedure")
	}
	cancel()
	select {
	case err := <-runner.ended:
		if err == nil {
			t.Fatal("the owner's cancellation did not reach the peer's local procedure")
		}
	case <-time.After(15 * time.Second):
		t.Fatal("the peer's local procedure never ended")
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the owner's hand-over did not return")
	}
}
