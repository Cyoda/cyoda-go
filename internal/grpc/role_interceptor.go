package grpc

import (
	"context"

	googlegrpc "google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// requireM2M refuses a context whose caller does not hold ROLE_M2M: no
// principal is Unauthenticated, a principal without the role
// PermissionDenied. Every method and stream of the service requires the role;
// gRPC has no allow-listed operation.
func requireM2M(ctx context.Context) error {
	uc := spi.GetUserContext(ctx)
	if uc == nil {
		return status.Error(codes.Unauthenticated, "authentication failed")
	}
	if !spi.HasRole(uc.Roles, "ROLE_M2M") {
		return status.Error(codes.PermissionDenied, "this operation requires ROLE_M2M")
	}
	return nil
}

// UnaryRequireM2M refuses a unary call whose caller lacks ROLE_M2M before
// its handler runs. It runs after the auth interceptor.
func UnaryRequireM2M() googlegrpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, _ *googlegrpc.UnaryServerInfo, handler googlegrpc.UnaryHandler) (any, error) {
		if err := requireM2M(ctx); err != nil {
			return nil, err
		}
		return handler(ctx, req)
	}
}

// StreamRequireM2M refuses a stream whose caller lacks ROLE_M2M before its
// handler runs. It runs after the auth interceptor. The compute stream keeps
// its own, stricter check.
func StreamRequireM2M() googlegrpc.StreamServerInterceptor {
	return func(srv any, ss googlegrpc.ServerStream, _ *googlegrpc.StreamServerInfo, handler googlegrpc.StreamHandler) error {
		if err := requireM2M(ss.Context()); err != nil {
			return err
		}
		return handler(srv, ss)
	}
}
