package grpc

import (
	"context"
	"net/http"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/common"
)

// resolveEventTimeout applies spec D6/D10 for CloudEvent writes: validate,
// reject on a tx-token'd (joined) request, attach the feature deadline.
//
// A nil millis is a no-op — (ctx, no-op cancel, nil) — so a caller that never
// sends transactionTimeoutMs observes zero behavior change. A joined
// (tx-token'd) request is rejected rather than silently ignored:
// spi.GetTransaction(ctx) != nil is how a routed compute-node callback
// presents at param-resolution time, and honoring a client-supplied deadline
// on a participant would let it unilaterally abandon a transaction the owner
// still controls. Mirrors internal/domain/entity's resolveRequestTimeout (the
// HTTP door's equivalent of this same D6/D10 rule) — same shape, gRPC's *int
// field (transactionTimeoutMs) instead of HTTP's *int64
// (transactionTimeoutMillis).
func resolveEventTimeout(ctx context.Context, millis *int) (context.Context, context.CancelFunc, error) {
	if millis == nil {
		return ctx, func() {}, nil
	}
	if appErr := common.ValidateRequestTimeoutMillis(int64(*millis)); appErr != nil {
		return nil, nil, appErr
	}
	if spi.GetTransaction(ctx) != nil {
		return nil, nil, common.Operational(http.StatusBadRequest, common.ErrCodeBadRequest,
			"transactionTimeoutMs is not supported on a request that joins an open transaction")
	}
	tctx, cancel := common.WithRequestTimeout(ctx, int64(*millis))
	return tctx, cancel, nil
}
