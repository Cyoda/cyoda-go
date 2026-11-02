package grpc

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	googlegrpc "google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/contract"
	"github.com/cyoda-platform/cyoda-go/internal/health"
	"github.com/cyoda-platform/cyoda-go/internal/platformclock"
)

// healthTestAuth accepts "Bearer m2m" (ROLE_M2M) and "Bearer plain" (no
// roles) and refuses anything else, a missing header included.
type healthTestAuth struct{}

func (healthTestAuth) Authenticate(ctx context.Context, r *http.Request) (context.Context, error) {
	switch r.Header.Get("Authorization") {
	case "Bearer m2m":
		uc := m2mUser()
		ctx = spi.WithUserContext(ctx, uc)
		return contract.WithClientToken(ctx, contract.ClientToken{ClientID: uc.UserID, Gen: 1}), nil
	case "Bearer plain":
		return spi.WithUserContext(ctx, &spi.UserContext{UserID: "p", UserName: "p", Kind: spi.PrincipalService, Tenant: spi.Tenant{ID: "t", Name: "t"}}), nil
	default:
		return nil, errors.New("no valid token")
	}
}

// startHealthTestServer runs a real NewServer on a loopback listener whose
// readiness answers *readiness, and returns a health client and the latch.
func startHealthTestServer(t *testing.T, readiness *func() error) (grpc_health_v1.HealthClient, *health.Latch) {
	t.Helper()
	latch := health.NewLatch()
	srv := NewServer(healthTestAuth{}, nil, NewMemberRegistry(), nil, nil, nil, nil, nil, noCalloutJoiner(t, nil, nil), nil,
		"n", false, 0, true, latch, KeepAliveConfig{}, newFixedConsistency(),
		func() platformclock.State { return platformclock.State{} },
		func() error { return (*readiness)() })
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.GracefulStop)
	conn, err := googlegrpc.NewClient(lis.Addr().String(), googlegrpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return grpc_health_v1.NewHealthClient(conn), latch
}

func healthCtx(t *testing.T, token string) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	if token != "" {
		ctx = metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+token)
	}
	return ctx
}

func wantCode(t *testing.T, err error, want codes.Code) {
	t.Helper()
	if st, ok := status.FromError(err); !ok || st.Code() != want {
		t.Fatalf("err = %v, want %v", err, want)
	}
}

func TestHealthCheck_NoToken_Serving(t *testing.T) {
	ready := func() error { return nil }
	client, _ := startHealthTestServer(t, &ready)
	resp, err := client.Check(healthCtx(t, ""), &grpc_health_v1.HealthCheckRequest{Service: ""})
	if err != nil {
		t.Fatalf("Check without a token: %v", err)
	}
	if resp.GetStatus() != grpc_health_v1.HealthCheckResponse_SERVING {
		t.Fatalf("status = %v, want SERVING", resp.GetStatus())
	}
}

func TestHealthCheck_NotReady_NotServing(t *testing.T) {
	for _, reason := range []string{
		"node unhealthy: a panic was recovered and this node's state is unverified",
		"node draining: shutdown has begun",
	} {
		t.Run(reason, func(t *testing.T) {
			notReady := func() error { return errors.New(reason) }
			client, _ := startHealthTestServer(t, &notReady)
			resp, err := client.Check(healthCtx(t, ""), &grpc_health_v1.HealthCheckRequest{})
			if err != nil {
				t.Fatalf("Check: %v", err)
			}
			if resp.GetStatus() != grpc_health_v1.HealthCheckResponse_NOT_SERVING {
				t.Fatalf("status = %v, want NOT_SERVING", resp.GetStatus())
			}
		})
	}
}

func TestHealthCheck_OtherService_NotFound(t *testing.T) {
	ready := func() error { return nil }
	client, _ := startHealthTestServer(t, &ready)
	_, err := client.Check(healthCtx(t, ""), &grpc_health_v1.HealthCheckRequest{Service: "org.cyoda.cloud.api.grpc.CloudEventsService"})
	wantCode(t, err, codes.NotFound)
}

func TestHealthWatch_NoToken_Unauthenticated(t *testing.T) {
	ready := func() error { return nil }
	client, _ := startHealthTestServer(t, &ready)
	stream, err := client.Watch(healthCtx(t, ""), &grpc_health_v1.HealthCheckRequest{})
	if err == nil {
		_, err = stream.Recv()
	}
	wantCode(t, err, codes.Unauthenticated)
}

func TestHealthWatch_PlainToken_PermissionDenied(t *testing.T) {
	ready := func() error { return nil }
	client, _ := startHealthTestServer(t, &ready)
	stream, err := client.Watch(healthCtx(t, "plain"), &grpc_health_v1.HealthCheckRequest{})
	if err == nil {
		_, err = stream.Recv()
	}
	wantCode(t, err, codes.PermissionDenied)
}

func TestHealthList_PlainToken_PermissionDenied(t *testing.T) {
	ready := func() error { return nil }
	client, _ := startHealthTestServer(t, &ready)
	_, err := client.List(healthCtx(t, "plain"), &grpc_health_v1.HealthListRequest{})
	wantCode(t, err, codes.PermissionDenied)
}

func TestHealthList_NoToken_Unauthenticated(t *testing.T) {
	ready := func() error { return nil }
	client, _ := startHealthTestServer(t, &ready)
	_, err := client.List(healthCtx(t, ""), &grpc_health_v1.HealthListRequest{})
	wantCode(t, err, codes.Unauthenticated)
}

func TestHealthList_M2M_Unimplemented(t *testing.T) {
	ready := func() error { return nil }
	client, _ := startHealthTestServer(t, &ready)
	_, err := client.List(healthCtx(t, "m2m"), &grpc_health_v1.HealthListRequest{})
	wantCode(t, err, codes.Unimplemented)
}

func TestHealthWatch_M2M_Unimplemented(t *testing.T) {
	ready := func() error { return nil }
	client, _ := startHealthTestServer(t, &ready)
	stream, err := client.Watch(healthCtx(t, "m2m"), &grpc_health_v1.HealthCheckRequest{})
	if err == nil {
		_, err = stream.Recv()
	}
	if errors.Is(err, io.EOF) {
		t.Fatal("Watch ended cleanly, want Unimplemented")
	}
	wantCode(t, err, codes.Unimplemented)
}

func TestHealthCheck_Panic_InternalAndLatched(t *testing.T) {
	var panicked atomic.Bool
	var latch *health.Latch
	readiness := func() error {
		if panicked.CompareAndSwap(false, true) {
			panic("readiness exploded")
		}
		if latch.Tripped() {
			return errors.New("node unhealthy")
		}
		return nil
	}
	client, l := startHealthTestServer(t, &readiness)
	latch = l

	_, err := client.Check(healthCtx(t, ""), &grpc_health_v1.HealthCheckRequest{})
	wantCode(t, err, codes.Internal)
	if !latch.Tripped() {
		t.Fatal("a panic in Check did not trip the health latch")
	}

	resp, err := client.Check(healthCtx(t, ""), &grpc_health_v1.HealthCheckRequest{})
	if err != nil {
		t.Fatalf("Check after the panic: %v", err)
	}
	if resp.GetStatus() != grpc_health_v1.HealthCheckResponse_NOT_SERVING {
		t.Fatalf("status after the panic = %v, want NOT_SERVING", resp.GetStatus())
	}
}

// The exemption is the exact Check method name: a name that only starts
// with it is authenticated and role-checked as every other method is.
func TestHealthExemption_ExactMethodOnly(t *testing.T) {
	handler := func(context.Context, any) (any, error) {
		t.Fatal("handler reached without authentication")
		return nil, nil
	}
	for _, method := range []string{
		"/grpc.health.v1.Health/CheckX",
		"/grpc.health.v1.Health/Chec",
		"/grpc.health.v1.Health/List",
		"/grpc.health.v1.Health/check",
	} {
		info := &googlegrpc.UnaryServerInfo{FullMethod: method}
		_, err := UnaryAuthInterceptor(healthTestAuth{})(context.Background(), nil, info, handler)
		if st, ok := status.FromError(err); !ok || st.Code() != codes.Unauthenticated {
			t.Fatalf("auth interceptor on %s without a token: err = %v, want Unauthenticated", method, err)
		}
		_, err = UnaryRequireM2M()(context.Background(), nil, info, handler)
		if st, ok := status.FromError(err); !ok || st.Code() != codes.Unauthenticated {
			t.Fatalf("ROLE_M2M interceptor on %s without a principal: err = %v, want Unauthenticated", method, err)
		}
	}
}

func TestNewServer_NilReadinessPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("NewServer with a nil readiness check did not panic")
		}
	}()
	NewServer(nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, "n", false, 0, true, nil, KeepAliveConfig{}, newFixedConsistency(),
		func() platformclock.State { return platformclock.State{} }, nil)
}
