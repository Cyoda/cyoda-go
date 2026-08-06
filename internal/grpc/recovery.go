package grpc

import (
	"context"
	"fmt"
	"log/slog"
	"runtime/debug"
	"sync/atomic"

	"github.com/google/uuid"
	googlegrpc "google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/cyoda-platform/cyoda-go/internal/common"
)

// UnaryRecoveryInterceptor converts a handler panic into an error instead of
// letting it kill the process. grpc-go does not recover handler panics — there
// is no equivalent of net/http's per-connection recover — so without this a
// single panic on any gRPC-only operation takes the node down.
//
// Mirrors internal/api/middleware/recovery.go: log with stack, mark the health
// flag, return a generic internal error carrying a ticket UUID as a proper
// gRPC status (codes.Internal). Marking health means the first recovered
// panic on any door takes the node to 503 DOWN permanently, which under a
// liveness probe is a restart. That is the existing HTTP contract,
// deliberately extended: a node that has panicked has unknown state, and
// restarting it is the correct response.
func UnaryRecoveryInterceptor(healthFlag *atomic.Bool) googlegrpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *googlegrpc.UnaryServerInfo, handler googlegrpc.UnaryHandler) (resp any, err error) {
		defer func() {
			if rec := recover(); rec != nil {
				resp = nil
				err = recoverPanic(rec, info.FullMethod, healthFlag)
			}
		}()
		return handler(ctx, req)
	}
}

// StreamRecoveryInterceptor is UnaryRecoveryInterceptor for streaming methods.
func StreamRecoveryInterceptor(healthFlag *atomic.Bool) googlegrpc.StreamServerInterceptor {
	return func(srv any, ss googlegrpc.ServerStream, info *googlegrpc.StreamServerInfo, handler googlegrpc.StreamHandler) (err error) {
		defer func() {
			if rec := recover(); rec != nil {
				err = recoverPanic(rec, info.FullMethod, healthFlag)
			}
		}()
		return handler(srv, ss)
	}
}

// recoverPanic logs the full panic detail (value plus stack) at ERROR under a
// freshly minted ticket UUID, marks healthFlag unhealthy (nil-safe: some test
// constructors do not wire one), and returns a sanitized gRPC status error —
// the panic value and stack never reach the client, only a generic message
// carrying the same ticket, so an operator handed the client-visible ticket
// can grep the server log for the matching ERROR line.
//
// The message format ("SERVER_ERROR: internal error [ticket: <uuid>]") and
// the "ticket" log field match internal/grpc/errors.go's buildErrorFields —
// this package's existing internal/fatal-error convention — rather than
// inventing a second one. codes.Internal is returned (not a bare
// *common.AppError, which grpc-go falls back to codes.Unknown for) so
// monitoring and retry dispatch see the same precision every other RPC-level
// failure in this package already returns.
func recoverPanic(rec any, method string, healthFlag *atomic.Bool) error {
	panicErr := fmt.Errorf("panic: %v", rec)
	ticket := uuid.New().String()
	slog.Error("panic recovered", "pkg", "grpc", "method", method,
		"ticket", ticket, "err", panicErr, "stack", string(debug.Stack()))
	if healthFlag != nil {
		healthFlag.Store(false)
	}
	message := fmt.Sprintf("%s: internal error [ticket: %s]", common.ErrCodeServerError, ticket)
	return status.Error(codes.Internal, message)
}
