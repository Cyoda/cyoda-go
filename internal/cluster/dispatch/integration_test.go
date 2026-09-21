package dispatch

import (
	"encoding/json"
	"net/http/httptest"
	"testing"

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

			a := realRouter(t, true).HandOver(testContext(), contract.NodeInfo{NodeID: "peer-b", Addr: peer.URL}, ownerCallout(t, tt.kind), 3, 1)
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
