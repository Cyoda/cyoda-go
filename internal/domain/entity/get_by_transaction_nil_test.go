package entity

// TestGetEntityByTransactionID_NilEntityVersion pins the final-review
// finding that getEntityByTransactionID dereferenced v.Entity unchecked.
// spi.EntityStore.GetVersionByTransaction's contract says a DELETED
// tombstone (no payload) never matches, so a nil-error return is documented
// to always carry a populated Entity — but a backend that violates that
// contract (a plausible implementation slip: a sqlite pushdown querying the
// transaction-ID column directly and returning a tombstone row) would panic
// this call, and via the handler's unrecovered-panic path latch healthFlag
// false and take the node out of service. Pre-branch, the equivalent linear
// scan over versions skipped nil-entity versions structurally, so this is a
// regression the streamlined direct lookup introduced. The fix treats a
// payload-less version the same as "no matching version" (spi.ErrNotFound).

import (
	"context"
	"errors"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// nilVersionEntityStore embeds a nil spi.EntityStore and overrides only
// GetVersionByTransaction, returning a version whose Entity field is nil
// (contract-violating tombstone shape) with a nil error — the exact
// combination that used to panic on v.Entity dereference.
type nilVersionEntityStore struct {
	spi.EntityStore
}

func (s *nilVersionEntityStore) GetVersionByTransaction(context.Context, string, string) (*spi.EntityVersion, error) {
	return &spi.EntityVersion{Entity: nil, Version: 1}, nil
}

func TestGetEntityByTransactionID_NilEntityVersion(t *testing.T) {
	store := &nilVersionEntityStore{}

	ent, err := getEntityByTransactionID(context.Background(), store, "e1", "tx-1")
	if ent != nil {
		t.Errorf("expected nil entity, got %+v", ent)
	}
	if !errors.Is(err, spi.ErrNotFound) {
		t.Fatalf("expected spi.ErrNotFound, got %v", err)
	}
}

// nilVersionStoreFactory hands out a nilVersionEntityStore from EntityStore()
// so TestGetEntity_ByTransactionID_NilEntityVersion_Returns404 can drive the
// full Handler.GetEntity path — before the fix, this reached
// ingest.DecodeStoredJSON(ent.Data, ...) with ent == nil and panicked.
type nilVersionStoreFactory struct{}

func (nilVersionStoreFactory) EntityStore(context.Context) (spi.EntityStore, error) {
	return &nilVersionEntityStore{}, nil
}
func (nilVersionStoreFactory) ModelStore(context.Context) (spi.ModelStore, error) { return nil, nil }
func (nilVersionStoreFactory) KeyValueStore(context.Context) (spi.KeyValueStore, error) {
	return nil, nil
}
func (nilVersionStoreFactory) MessageStore(context.Context) (spi.MessageStore, error) {
	return nil, nil
}
func (nilVersionStoreFactory) WorkflowStore(context.Context) (spi.WorkflowStore, error) {
	return nil, nil
}
func (nilVersionStoreFactory) StateMachineAuditStore(context.Context) (spi.StateMachineAuditStore, error) {
	return nil, nil
}
func (nilVersionStoreFactory) AsyncSearchStore(context.Context) (spi.AsyncSearchStore, error) {
	return nil, nil
}
func (nilVersionStoreFactory) ScheduledTaskStore(context.Context) (spi.ScheduledTaskStore, error) {
	return nil, nil
}
func (nilVersionStoreFactory) TransactionManager(context.Context) (spi.TransactionManager, error) {
	return nil, nil
}
func (nilVersionStoreFactory) Close() error { return nil }

func TestGetEntity_ByTransactionID_NilEntityVersion_Returns404(t *testing.T) {
	h := &Handler{factory: nilVersionStoreFactory{}}

	_, err := h.GetEntity(context.Background(), GetOneEntityInput{
		EntityID:      "e1",
		TransactionID: "tx-1",
	})
	if err == nil {
		t.Fatal("expected an error (404 ENTITY_NOT_FOUND), got nil — and no panic, which is the regression this guards")
	}
}
