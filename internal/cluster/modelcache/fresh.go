package modelcache

import (
	"context"
	"fmt"

	spi "github.com/cyoda/cyoda-go-spi"
)

// GetFresh reads the model from storage, bypassing this node's cached entry
// (it may share a storage read already running on this node). The store must
// be the request-scoped store a CachingStoreFactory hands out, which offers
// RefreshAndGet; any other store is a wiring defect and the read fails rather
// than answering from a read that may be stale.
//
// A decision on the model's state uses it, so the decision is made on what
// storage returns and not on what this node cached: the model administration
// operations that gate on the state, and an entity create that read the model
// as not LOCKED before it refuses.
func GetFresh(ctx context.Context, store spi.ModelStore, ref spi.ModelRef) (*spi.ModelDescriptor, error) {
	type refresher interface {
		RefreshAndGet(ctx context.Context, ref spi.ModelRef) (*spi.ModelDescriptor, error)
	}
	r, ok := store.(refresher)
	if !ok {
		return nil, fmt.Errorf("model store %T cannot read the model fresh from storage", store)
	}
	return r.RefreshAndGet(ctx, ref)
}
