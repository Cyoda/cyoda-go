package common

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// EnsureModelRegistered returns a 404 MODEL_NOT_FOUND AppError when ref names a
// model that is not registered for the caller's tenant, and nil otherwise.
//
// It performs at most one bounded RefreshAndGet (singleflight-collapsed and
// negative-cached inside the caching model store) so a model just registered on
// a peer node is not falsely rejected — mirroring the write path's
// ValidateWithRefresh and the search path-validator's one-shot refresh. When the
// store has no RefreshAndGet (single-node / memory), the cached Get is
// authoritative.
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
		if _, rerr := refresher.RefreshAndGet(ctx, ref); rerr == nil {
			return nil
		}
	}

	return Operational(http.StatusNotFound, ErrCodeModelNotFound,
		fmt.Sprintf("model %s/%s not found", ref.EntityName, ref.ModelVersion))
}
