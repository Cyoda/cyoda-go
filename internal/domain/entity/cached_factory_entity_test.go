package entity

import (
	"sync"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/cluster/modelcache"
)

// cachedFactory wraps a backend factory the way app/app.go does: one model
// cache per factory, shared by every service built on it, so a model change
// made through one service is seen by the others. The request scoped model
// store it hands out is the one every field-path check and model state gate
// requires: it parses and refreshes a model's schema and reads it fresh from
// storage, which a bare backend store does not offer.
func cachedFactory(f spi.StoreFactory) spi.StoreFactory {
	if c, ok := cachedFactories.Load(f); ok {
		return c.(spi.StoreFactory)
	}
	c, _ := cachedFactories.LoadOrStore(f, modelcache.NewCachingStoreFactory(f, nil, nil, time.Hour))
	return c.(spi.StoreFactory)
}

var cachedFactories sync.Map
