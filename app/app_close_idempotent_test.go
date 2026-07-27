package app

import (
	"context"
	"sync/atomic"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// countingStoreFactory records the number of Close() invocations. The other
// SPI methods are never exercised in the Shutdown/Close path under test.
type countingStoreFactory struct {
	closeCalls atomic.Int32
}

func (f *countingStoreFactory) EntityStore(context.Context) (spi.EntityStore, error) {
	return nil, nil
}
func (f *countingStoreFactory) ModelStore(context.Context) (spi.ModelStore, error) {
	return nil, nil
}
func (f *countingStoreFactory) KeyValueStore(context.Context) (spi.KeyValueStore, error) {
	return nil, nil
}
func (f *countingStoreFactory) MessageStore(context.Context) (spi.MessageStore, error) {
	return nil, nil
}
func (f *countingStoreFactory) WorkflowStore(context.Context) (spi.WorkflowStore, error) {
	return nil, nil
}
func (f *countingStoreFactory) StateMachineAuditStore(context.Context) (spi.StateMachineAuditStore, error) {
	return nil, nil
}
func (f *countingStoreFactory) AsyncSearchStore(context.Context) (spi.AsyncSearchStore, error) {
	return nil, nil
}
func (f *countingStoreFactory) ScheduledTaskStore(context.Context) (spi.ScheduledTaskStore, error) {
	return nil, nil
}
func (f *countingStoreFactory) TransactionManager(context.Context) (spi.TransactionManager, error) {
	return nil, nil
}
func (f *countingStoreFactory) Close() error {
	f.closeCalls.Add(1)
	return nil
}

// TestApp_ShutdownThenClose_StoreFactoryClosedExactlyOnce pins the
// invariant that storeFactory.Close() is called exactly once across the
// Shutdown() then Close() teardown sequence used by runServers (#26
// follow-up). Prior to the fix Shutdown() also closed the factory,
// resulting in a double close which most plugins surface as
// "use of closed connection" errors during graceful drain.
func TestApp_ShutdownThenClose_StoreFactoryClosedExactlyOnce(t *testing.T) {
	fake := &countingStoreFactory{}
	a := &App{storeFactory: fake}

	a.Shutdown()
	if err := a.Close(); err != nil {
		t.Fatalf("Close returned error: %v", err)
	}

	if got := fake.closeCalls.Load(); got != 1 {
		t.Fatalf("storeFactory.Close called %d times across Shutdown+Close; want exactly 1", got)
	}
}
