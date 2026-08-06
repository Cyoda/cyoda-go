package grpc

import (
	"context"
	"fmt"
	"log/slog"
	"runtime/debug"
	"sync/atomic"

	googlegrpc "google.golang.org/grpc"

	"github.com/cyoda-platform/cyoda-go/internal/common"
)

// UnaryRecoveryInterceptor converts a handler panic into an error instead of
// letting it kill the process. grpc-go does not recover handler panics — there
// is no equivalent of net/http's per-connection recover — so without this a
// single panic on any gRPC-only operation takes the node down.
//
// Mirrors internal/api/middleware/recovery.go: log with stack, mark the health
// flag, return a generic internal error carrying a ticket UUID. Marking health
// means the first recovered panic on any door takes the node to 503 DOWN
// permanently, which under a liveness probe is a restart. That is the existing
// HTTP contract, deliberately extended: a node that has panicked has unknown
// state, and restarting it is the correct response.
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

// recoverPanic logs the full panic detail (value plus stack) at ERROR, marks
// healthFlag unhealthy (nil-safe: some test constructors do not wire one), and
// returns a sanitized error — the panic value and stack never reach the
// client, only the generic message and a ticket UUID that correlates back to
// the log line.
func recoverPanic(rec any, method string, healthFlag *atomic.Bool) error {
	panicErr := fmt.Errorf("panic: %v", rec)
	slog.Error("panic recovered", "pkg", "grpc", "method", method,
		"err", panicErr, "stack", string(debug.Stack()))
	if healthFlag != nil {
		healthFlag.Store(false)
	}
	// Generic message plus a ticket UUID; the panic value stays in the log.
	appErr := common.Fatal("internal server error", panicErr)
	appErr.Detail = "panic recovered; check server logs for details"
	return appErr
}
