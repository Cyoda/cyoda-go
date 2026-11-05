package grpc

// A forwarded call that does not reach its owner is one log line, at ERROR,
// naming the request it answers — on the unary and the stream path.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	googlegrpc "google.golang.org/grpc"
	"google.golang.org/grpc/metadata"

	cepb "github.com/cyoda-platform/cyoda-go/api/grpc/cloudevents"
	"github.com/cyoda-platform/cyoda-go/internal/cluster/peeraddr"
	"github.com/cyoda-platform/cyoda-go/internal/cluster/proxy"
	"github.com/cyoda-platform/cyoda-go/internal/cluster/token"
	"github.com/cyoda-platform/cyoda-go/internal/contract"
)

func wantOneLineNaming(t *testing.T, logs *lockedBuffer, level, rpc, ceID string) {
	t.Helper()
	logs.mu.Lock()
	out := logs.b.String()
	logs.mu.Unlock()
	var lines []string
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "level=") && !strings.Contains(line, "level=DEBUG") {
			lines = append(lines, line)
		}
	}
	if len(lines) != 1 {
		t.Fatalf("log lines above DEBUG = %q, want one", lines)
	}
	for _, want := range []string{"level=" + level, "pkg=grpc", "rpc=" + rpc, "ceId=" + ceID} {
		if !strings.Contains(lines[0], want) {
			t.Errorf("log line lacks %s: %s", want, lines[0])
		}
	}
}

func TestTxRouteInterceptor_UnreachableOwnerIsOneLineNamingTheRequest(t *testing.T) {
	unreachable := errors.New("dial tcp 10.0.0.2:9090: connect: connection refused")
	t.Run("unary", func(t *testing.T) {
		ic, ctx := forwardingInterceptor(t, testProxyTimeout)
		ic.forwardUnary = func(context.Context, *proxy.ClientPool, string, *cepb.CloudEvent) (*cepb.CloudEvent, error) {
			return nil, unreachable
		}
		logs := captureLogs(t)
		if _, err := ic.unary()(ctx, &cepb.CloudEvent{Id: "req-log-unary", Type: EntityCreateRequest}, entityManageInfo(), mustNotRunUnary(t)); err != nil {
			t.Fatalf("expected envelope response, got gRPC err: %v", err)
		}
		wantOneLineNaming(t, logs, "ERROR", "entityManage", "req-log-unary")
	})
	t.Run("stream", func(t *testing.T) {
		ic, ctx := forwardingInterceptor(t, testProxyTimeout)
		ic.forwardStream = func(context.Context, *proxy.ClientPool, string, *cepb.CloudEvent) (googlegrpc.ServerStreamingClient[cepb.CloudEvent], error) {
			return nil, unreachable
		}
		logs := captureLogs(t)
		ss := &fakeServerStream{ctx: ctx, recv: []*cepb.CloudEvent{{Id: "req-log-stream", Type: EntityCreateCollectionRequest}}}
		if err := ic.stream()(nil, ss, entityManageCollectionInfo(), mustNotRunStream(t)); err != nil {
			t.Fatalf("expected an envelope on the stream, got err: %v", err)
		}
		wantOneLineNaming(t, logs, "ERROR", "entityManageCollection", "req-log-stream")
	})
}

// A token naming a node that is not live, and an owner address the peer
// address guard forbids, are each one WARN line naming the request, as on the
// HTTP door.
func TestTxRouteInterceptor_RefusalsBeforeTheHopAreOneWarnLine(t *testing.T) {
	t.Run("node not live", func(t *testing.T) {
		s, _ := token.NewSigner(make32(t))
		tok, _ := s.Issue(token.Claims{NodeID: "node-owner", TxRef: "tx-log-down", ExpiresAt: time.Now().Add(time.Minute).Unix(), Callout: "req-tx-log-down", Major: 1})
		reg := fakeRouteRegistry{nodes: map[string]contract.NodeInfo{
			"node-owner": {NodeID: "node-owner", Addr: "http://node-owner:8080", Alive: false},
		}}
		ic := newTxRouteInterceptor(s, reg, "local", noCalloutJoiner(t, s, fakeJoinTM{}), 9090, true, testProxyTimeout)
		ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs("tx-token", tok))
		logs := captureLogs(t)
		if _, err := ic.unary()(ctx, &cepb.CloudEvent{Id: "req-log-down", Type: EntityCreateRequest}, entityManageInfo(), mustNotRunUnary(t)); err != nil {
			t.Fatalf("expected envelope response, got gRPC err: %v", err)
		}
		wantOneLineNaming(t, logs, "WARN", "entityManage", "req-log-down")
		if out := logs.lines("WARN"); len(out) == 1 && !strings.Contains(out[0], "node-owner") {
			t.Errorf("the line does not name the node: %s", out[0])
		}
	})
	t.Run("address forbidden", func(t *testing.T) {
		ic, ctx := forwardingInterceptor(t, testProxyTimeout)
		ic.forwardUnary = func(context.Context, *proxy.ClientPool, string, *cepb.CloudEvent) (*cepb.CloudEvent, error) {
			return nil, fmt.Errorf("%w: %q resolved to 127.0.0.1 (loopback 127.0.0.1)", peeraddr.ErrForbiddenPeerAddress, "node-b")
		}
		logs := captureLogs(t)
		if _, err := ic.unary()(ctx, &cepb.CloudEvent{Id: "req-log-guard", Type: EntityCreateRequest}, entityManageInfo(), mustNotRunUnary(t)); err != nil {
			t.Fatalf("expected envelope response, got gRPC err: %v", err)
		}
		wantOneLineNaming(t, logs, "WARN", "entityManage", "req-log-guard")
	})
}
