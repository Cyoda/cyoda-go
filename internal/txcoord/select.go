package txcoord

import (
	"context"
	"fmt"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// Select picks the coordination for the storage backend behind factory, once,
// at start-up, over txMgr, the transaction manager the rest of the server uses.
// A factory that offers spi.TransactionCoordinationCapable with a non-nil
// coordinator gets the durable coordination over it, and its transactions are
// location-independent (the bool): any pnode serves their callbacks. Any other
// factory, one that answers (nil, nil) included, gets the in-process
// coordination.
//
// An error from the backend fails start-up: the server does not run on a
// coordination that may not be the backend's.
func Select(ctx context.Context, factory spi.StoreFactory, txMgr spi.TransactionManager) (Coordination, bool, error) {
	capable, ok := factory.(spi.TransactionCoordinationCapable)
	if !ok {
		return New(txMgr), false, nil
	}
	co, err := capable.TransactionCoordinator(ctx)
	if err != nil {
		return nil, false, fmt.Errorf("failed to ask the storage backend for its transaction coordinator: %w", err)
	}
	if co == nil {
		return New(txMgr), false, nil
	}
	return NewDurable(co, txMgr), true, nil
}
