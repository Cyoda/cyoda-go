package txcoord

import (
	"context"
	"fmt"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// Select picks the coordination for the storage backend behind factory, once,
// at start-up. A factory that offers spi.TransactionCoordinationCapable with a
// non-nil coordinator gets the durable coordination over it, and its
// transactions are location-independent (the bool): any pnode serves their
// callbacks. Any other factory, one that answers (nil, nil) included, gets
// the in-process coordination inProcess builds.
//
// An error from the backend fails start-up: the server does not run on a
// coordination that may not be the backend's.
func Select(ctx context.Context, factory spi.StoreFactory, inProcess func() Coordination) (Coordination, bool, error) {
	capable, ok := factory.(spi.TransactionCoordinationCapable)
	if !ok {
		return inProcess(), false, nil
	}
	co, err := capable.TransactionCoordinator(ctx)
	if err != nil {
		return nil, false, fmt.Errorf("failed to ask the storage backend for its transaction coordinator: %w", err)
	}
	if co == nil {
		return inProcess(), false, nil
	}
	txMgr, err := factory.TransactionManager(ctx)
	if err != nil {
		return nil, false, fmt.Errorf("failed to get the storage backend's transaction manager: %w", err)
	}
	return NewDurable(co, txMgr), true, nil
}
