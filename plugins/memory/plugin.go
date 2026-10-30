package memory

import (
	"context"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

func init() { spi.Register(&plugin{}) }

type plugin struct{}

func (p *plugin) Name() string { return "memory" }

func (p *plugin) NewFactory(
	ctx context.Context,
	getenv func(string) string,
	opts ...spi.FactoryOption,
) (spi.StoreFactory, error) {
	// The memory backend has no configuration and no blocking setup
	// work, so ctx and getenv are unused. The only option it reads is the
	// business clock.
	_ = ctx
	_ = getenv

	// newStoreFactory delegates to NewStoreFactory, which already calls
	// initTransactionManager internally — no separate call needed.
	var storeOpts []Option
	if c := spi.ApplyFactoryOptions(opts).Clock(); c != nil {
		storeOpts = append(storeOpts, WithBusinessClock(c))
	}
	factory := newStoreFactory(storeOpts...)
	return factory, nil
}
