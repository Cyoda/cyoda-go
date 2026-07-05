package common

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// EnsureModelRegistered returns a 404 MODEL_NOT_FOUND AppError when ref names a
// model that is not registered for the caller's tenant, nil when it is registered,
// and an Internal(500) AppError when the store returns a non-not-found error.
//
// It performs at most one bounded RefreshAndGet (singleflight-collapsed and
// negative-cached inside the caching model store) so a model just registered on
// a peer node is not falsely rejected — mirroring the write path's
// ValidateWithRefresh and the search path-validator's one-shot refresh. When the
// store has no RefreshAndGet (single-node / memory), the cached Get is
// authoritative.
//
// Decision tree:
//   - Get succeeds → nil (registered)
//   - Get fails with non-ErrNotFound → Internal(500) (store failure)
//   - Get fails with ErrNotFound, no RefreshAndGet → 404 MODEL_NOT_FOUND
//   - RefreshAndGet succeeds → nil (registered on peer)
//   - RefreshAndGet fails with ErrNotFound → 404 MODEL_NOT_FOUND
//   - RefreshAndGet fails with non-ErrNotFound → Internal(500) (store failure)
func EnsureModelRegistered(ctx context.Context, ms spi.ModelStore, ref spi.ModelRef) *AppError {
	_, err := ms.Get(ctx, ref)
	if err == nil {
		return nil
	}
	if !errors.Is(err, spi.ErrNotFound) {
		return Internal("failed to check model registration", err)
	}

	if refresher, ok := ms.(interface {
		RefreshAndGet(context.Context, spi.ModelRef) (*spi.ModelDescriptor, error)
	}); ok {
		_, rerr := refresher.RefreshAndGet(ctx, ref)
		if rerr == nil {
			return nil
		}
		if !errors.Is(rerr, spi.ErrNotFound) {
			return Internal("failed to refresh model registration", rerr)
		}
	}

	return Operational(http.StatusNotFound, ErrCodeModelNotFound,
		fmt.Sprintf("model %s/%s not found", ref.EntityName, ref.ModelVersion))
}
