package grpc

// A forwarded call that does not reach its owner is one log line, at ERROR,
// naming the request it answers — on the unary and the stream path.

import (
	"context"
	"errors"
	"strings"
	"testing"

	googlegrpc "google.golang.org/grpc"

	cepb "github.com/cyoda-platform/cyoda-go/api/grpc/cloudevents"
	"github.com/cyoda-platform/cyoda-go/internal/cluster/proxy"
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
