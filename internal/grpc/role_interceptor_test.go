package grpc

import (
	"context"
	"testing"

	googlegrpc "google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	cyodapb "github.com/cyoda-platform/cyoda-go/api/grpc/cyoda"
)

func roleCtx(roles ...string) context.Context {
	return spi.WithUserContext(context.Background(), &spi.UserContext{
		UserID: "caller", Tenant: spi.Tenant{ID: "acme", Name: "acme"}, Roles: roles,
	})
}

type ctxStream struct {
	googlegrpc.ServerStream
	ctx context.Context
}

func (s ctxStream) Context() context.Context { return s.ctx }

func wantRoleRefusal(t *testing.T, name string, err error, code codes.Code) {
	t.Helper()
	st, ok := status.FromError(err)
	if !ok || st.Code() != code {
		t.Fatalf("%s: err = %v, want %v", name, err, code)
	}
	if code == codes.PermissionDenied && st.Message() != "this operation requires ROLE_M2M" {
		t.Fatalf("%s: message %q, want the ROLE_M2M cause", name, st.Message())
	}
}

// TestRoleInterceptor_EveryMethodRequiresM2M: every unary method and every
// stream of the service refuses a principal without ROLE_M2M before its
// handler runs, refuses no principal as Unauthenticated, and calls the handler
// for a ROLE_M2M principal.
func TestRoleInterceptor_EveryMethodRequiresM2M(t *testing.T) {
	desc := cyodapb.CloudEventsService_ServiceDesc
	if len(desc.Methods) == 0 || len(desc.Streams) == 0 {
		t.Fatal("service descriptor lists no methods or no streams")
	}
	unary := UnaryRequireM2M()
	for _, m := range desc.Methods {
		full := "/" + desc.ServiceName + "/" + m.MethodName
		info := &googlegrpc.UnaryServerInfo{FullMethod: full}
		called := false
		handler := func(context.Context, any) (any, error) { called = true; return "ok", nil }

		_, err := unary(roleCtx("ROLE_ADMIN"), nil, info, handler)
		wantRoleRefusal(t, full+" ROLE_ADMIN", err, codes.PermissionDenied)
		_, err = unary(context.Background(), nil, info, handler)
		wantRoleRefusal(t, full+" no principal", err, codes.Unauthenticated)
		if called {
			t.Fatalf("%s: handler ran for a refused caller", full)
		}
		if _, err := unary(roleCtx("ROLE_M2M"), nil, info, handler); err != nil || !called {
			t.Fatalf("%s ROLE_M2M: err=%v called=%v, want the handler called", full, err, called)
		}
	}

	stream := StreamRequireM2M()
	for _, s := range desc.Streams {
		full := "/" + desc.ServiceName + "/" + s.StreamName
		info := &googlegrpc.StreamServerInfo{FullMethod: full, IsServerStream: s.ServerStreams, IsClientStream: s.ClientStreams}
		called := false
		handler := func(any, googlegrpc.ServerStream) error { called = true; return nil }

		wantRoleRefusal(t, full+" ROLE_ADMIN", stream(nil, ctxStream{ctx: roleCtx("ROLE_ADMIN")}, info, handler), codes.PermissionDenied)
		wantRoleRefusal(t, full+" no principal", stream(nil, ctxStream{ctx: context.Background()}, info, handler), codes.Unauthenticated)
		if called {
			t.Fatalf("%s: handler ran for a refused caller", full)
		}
		if err := stream(nil, ctxStream{ctx: roleCtx("ROLE_M2M")}, info, handler); err != nil || !called {
			t.Fatalf("%s ROLE_M2M: err=%v called=%v, want the handler called", full, err, called)
		}
	}
}
