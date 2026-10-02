package grpc

import (
	"context"
	"log/slog"
	"net/http"

	googlegrpc "google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/contract"
)

// UnaryAuthInterceptor creates a unary server interceptor for authentication.
// It extracts the authorization header from gRPC metadata, delegates to the
// AuthenticationService, and injects the resulting UserContext into the request
// context.
func UnaryAuthInterceptor(authSvc contract.AuthenticationService) googlegrpc.UnaryServerInterceptor {
	return func(
		ctx context.Context,
		req any,
		info *googlegrpc.UnaryServerInfo,
		handler googlegrpc.UnaryHandler,
	) (any, error) {
		authCtx, err := authenticateFromMetadata(ctx, authSvc)
		if err != nil {
			slog.Warn("gRPC auth failed", "pkg", "grpc", "method", info.FullMethod, "err", err)
			return nil, status.Error(codes.Unauthenticated, "authentication failed")
		}
		return handler(authCtx, req)
	}
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

	uc := spi.GetUserContext(authCtx)
	slog.Debug("gRPC auth succeeded", "pkg", "grpc", "userId", uc.UserID, "tenantId", string(uc.Tenant.ID))
	return authCtx, nil
}
