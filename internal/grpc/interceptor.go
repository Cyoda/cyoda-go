package grpc

import (
	"context"
	"errors"
	"log/slog"
	"net/http"

	googlegrpc "google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/contract"
)

// UnaryAuthInterceptor creates a unary server interceptor for authentication.
// It extracts the authorization header from gRPC metadata, delegates to the
// AuthenticationService, and injects the resulting UserContext into the request
// context. The health Check is passed through unauthenticated (see
// isUnauthenticatedMethod).
func UnaryAuthInterceptor(authSvc contract.AuthenticationService) googlegrpc.UnaryServerInterceptor {
	return func(
		ctx context.Context,
		req any,
		info *googlegrpc.UnaryServerInfo,
		handler googlegrpc.UnaryHandler,
	) (any, error) {
		if isUnauthenticatedMethod(info.FullMethod) {
			return handler(ctx, req)
		}
		authCtx, err := authenticateFromMetadata(ctx, authSvc)
		if err != nil {
			slog.Warn("gRPC auth failed", "pkg", "grpc", "method", info.FullMethod, "err", err)
			return nil, status.Error(codes.Unauthenticated, "authentication failed")
		}
		return handler(authCtx, req)
	}
}

// isUnauthenticatedMethod reports whether a unary method is served without a
// token: only grpc.health.v1.Health/Check, by exact full-method name, because
// probes cannot authenticate. It answers serving or not serving and nothing
// else. Watch and List of the same service, and every stream, are
// authenticated and role-checked as any other method is.
func isUnauthenticatedMethod(fullMethod string) bool {
	return fullMethod == grpc_health_v1.Health_Check_FullMethodName
}

// StreamAuthInterceptor creates a stream server interceptor for authentication.
// It extracts the authorization header from gRPC metadata, delegates to the
// AuthenticationService, and wraps the stream with an authenticated context.
func StreamAuthInterceptor(authSvc contract.AuthenticationService) googlegrpc.StreamServerInterceptor {
	return func(
		srv any,
		ss googlegrpc.ServerStream,
		info *googlegrpc.StreamServerInfo,
		handler googlegrpc.StreamHandler,
	) error {
		authCtx, err := authenticateFromMetadata(ss.Context(), authSvc)
		if err != nil {
			slog.Warn("gRPC auth failed", "pkg", "grpc", "method", info.FullMethod, "err", err)
			return status.Error(codes.Unauthenticated, "authentication failed")
		}
		return handler(srv, &wrappedStream{ServerStream: ss, ctx: authCtx})
	}
}

// wrappedStream wraps a grpc.ServerStream to override its Context() method,
// allowing injection of an authenticated context.
type wrappedStream struct {
	googlegrpc.ServerStream
	ctx context.Context
}

// Context returns the authenticated context.
func (w *wrappedStream) Context() context.Context { return w.ctx }

// authenticateFromMetadata extracts the authorization header from incoming gRPC
// metadata, builds a minimal http.Request, and delegates to the
// AuthenticationService. On success it returns the context Authenticate
// returned: the UserContext and, for a client-credentials token, the
// client-token marker.
func authenticateFromMetadata(ctx context.Context, authSvc contract.AuthenticationService) (context.Context, error) {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		md = metadata.MD{}
	}

	r, _ := http.NewRequestWithContext(ctx, "GET", "/", nil)

	vals := md.Get("authorization")
	if len(vals) > 0 {
		r.Header.Set("Authorization", vals[0])
	}

	authCtx, err := authSvc.Authenticate(ctx, r)
	if err != nil {
		return nil, err
	}

	// A success without a principal is a failure: no call runs unattributed.
	uc := spi.GetUserContext(authCtx)
	if uc == nil {
		return nil, errors.New("authentication returned no principal")
	}
	slog.Debug("gRPC auth succeeded", "pkg", "grpc", "userId", uc.UserID, "tenantId", string(uc.Tenant.ID))
	return authCtx, nil
}
