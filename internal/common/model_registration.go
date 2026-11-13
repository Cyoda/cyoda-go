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
// It is one Get on the store it is handed; there is no refresh. A model
// registered on a peer node becomes visible when this node's cached entry
// expires or the peer's invalidation arrives.
func EnsureModelRegistered(ctx context.Context, ms spi.ModelStore, ref spi.ModelRef) *AppError {
	_, err := ms.Get(ctx, ref)
	if err == nil {
		return nil
	}
	if !errors.Is(err, spi.ErrNotFound) {
		return Internal("failed to check model registration", err)
	}
	return Operational(http.StatusNotFound, ErrCodeModelNotFound,
		fmt.Sprintf("model %s/%s not found", ref.EntityName, ref.ModelVersion))
}
