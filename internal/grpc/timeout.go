package grpc

import (
	"context"
	"fmt"
	"net/http"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/common"
)

// resolveEventTimeout applies spec D6/D10 for CloudEvent writes and searches:
// validate, reject on a tx-token'd (joined) request, attach the feature
// deadline.
//
// A nil millis is a no-op — (ctx, no-op cancel, nil) — so a caller that never
// sends its timeout field observes zero behavior change. A joined
// (tx-token'd) request is rejected rather than silently ignored:
// spi.GetTransaction(ctx) != nil is how a routed compute-node callback
// presents at param-resolution time, and honoring a client-supplied deadline
// on a participant would let it unilaterally abandon a transaction the owner
// still controls. Mirrors internal/domain/entity's resolveRequestTimeout (the
// HTTP door's equivalent of this same D6/D10 rule) — same shape, gRPC's *int
// field instead of HTTP's *int64.
//
// fieldName names the CloudEvent field millis was decoded from (e.g.
// "transactionTimeoutMs" for the 5 write events, "timeoutMillis" for
// EntitySearchRequest) so the joined-transaction rejection message names the
// field the caller actually sent, not a field name borrowed from a different
// event.
func resolveEventTimeout(ctx context.Context, millis *int, fieldName string) (context.Context, context.CancelFunc, error) {
	if millis == nil {
		return ctx, func() {}, nil
	}
	if appErr := common.ValidateRequestTimeoutMillis(int64(*millis)); appErr != nil {
		return nil, nil, appErr
	}
	if spi.GetTransaction(ctx) != nil {
		return nil, nil, common.Operational(http.StatusBadRequest, common.ErrCodeBadRequest,
			fmt.Sprintf("%s is not supported on a request that joins an open transaction", fieldName))
	}
	tctx, cancel := common.WithRequestTimeout(ctx, int64(*millis))
	return tctx, cancel, nil
}
