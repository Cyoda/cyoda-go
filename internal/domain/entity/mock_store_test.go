package entity_test

import (
	"context"
	"errors"
	"iter"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// failingStoreFactory is a mock that returns a failingEntityStore from EntityStore().
type failingStoreFactory struct {
	err error
}

func (f *failingStoreFactory) EntityStore(_ context.Context) (spi.EntityStore, error) {
	return &failingEntityStore{err: f.err}, nil
}
func (f *failingStoreFactory) ModelStore(_ context.Context) (spi.ModelStore, error) {
	return nil, f.err
}
func (f *failingStoreFactory) KeyValueStore(_ context.Context) (spi.KeyValueStore, error) {
	return nil, f.err
}
func (f *failingStoreFactory) MessageStore(_ context.Context) (spi.MessageStore, error) {
	return nil, f.err
}
func (f *failingStoreFactory) WorkflowStore(_ context.Context) (spi.WorkflowStore, error) {
	return nil, f.err
}
func (f *failingStoreFactory) StateMachineAuditStore(_ context.Context) (spi.StateMachineAuditStore, error) {
	return nil, f.err
}
func (f *failingStoreFactory) AsyncSearchStore(_ context.Context) (spi.AsyncSearchStore, error) {
	return nil, f.err
}
func (f *failingStoreFactory) ScheduledTaskStore(_ context.Context) (spi.ScheduledTaskStore, error) {
	return nil, f.err
}
func (f *failingStoreFactory) TransactionManager(_ context.Context) (spi.TransactionManager, error) {
	return nil, f.err
}
func (f *failingStoreFactory) Close() error { return nil }

// failingEntityStore always returns the configured error from Get/GetAsAt.
type failingEntityStore struct {
	err error
}

func (s *failingEntityStore) Save(_ context.Context, _ *spi.Entity) (int64, error) {
	return 0, s.err
}
func (s *failingEntityStore) SaveAll(_ context.Context, _ iter.Seq[*spi.Entity]) ([]int64, error) {
	return nil, s.err
}
func (s *failingEntityStore) CompareAndSave(_ context.Context, _ *spi.Entity, _ string) (int64, error) {
	return 0, s.err
}
func (s *failingEntityStore) Get(_ context.Context, _ string) (*spi.Entity, error) {
	return nil, s.err
}
func (s *failingEntityStore) GetAsAt(_ context.Context, _ string, _ time.Time) (*spi.Entity, error) {
	return nil, s.err
}
func (s *failingEntityStore) GetAll(_ context.Context, _ spi.ModelRef) ([]*spi.Entity, error) {
	return nil, s.err
}
func (s *failingEntityStore) GetAllAsAt(_ context.Context, _ spi.ModelRef, _ time.Time) ([]*spi.Entity, error) {
	return nil, s.err
}
func (s *failingEntityStore) Delete(_ context.Context, _ string) error {
	return s.err
}
func (s *failingEntityStore) DeleteAll(_ context.Context, _ spi.ModelRef) error {
	return s.err
}
func (s *failingEntityStore) Exists(_ context.Context, _ string) (bool, error) {
	return false, s.err
}
func (s *failingEntityStore) Count(_ context.Context, _ spi.ModelRef) (int64, error) {
	return 0, s.err
}
func (s *failingEntityStore) CountByState(_ context.Context, _ spi.ModelRef, _ []string) (map[string]int64, error) {
	return nil, s.err
}
func (s *failingEntityStore) GetVersionHistory(_ context.Context, _ string) ([]spi.EntityVersion, error) {
	return nil, s.err
}

// modelStoreGetErr is a spi.ModelStore that returns the configured error from
// Get. All other methods are unused by the tests that inject this mock and
// return nil/zero. Used to verify that the service layer classifies Get
// errors correctly (spi.ErrNotFound → 404, anything else → 5xx).
type modelStoreGetErr struct {
	err error
}

func (m *modelStoreGetErr) Save(_ context.Context, _ *spi.ModelDescriptor) error { return nil }
func (m *modelStoreGetErr) Get(_ context.Context, _ spi.ModelRef) (*spi.ModelDescriptor, error) {
	return nil, m.err
}
func (m *modelStoreGetErr) GetAll(_ context.Context) ([]spi.ModelRef, error) { return nil, nil }
func (m *modelStoreGetErr) Delete(_ context.Context, _ spi.ModelRef) error   { return nil }
func (m *modelStoreGetErr) Lock(_ context.Context, _ spi.ModelRef) error     { return nil }
func (m *modelStoreGetErr) Unlock(_ context.Context, _ spi.ModelRef) error   { return nil }
func (m *modelStoreGetErr) IsLocked(_ context.Context, _ spi.ModelRef) (bool, error) {
	return false, nil
}
func (m *modelStoreGetErr) SetChangeLevel(_ context.Context, _ spi.ModelRef, _ spi.ChangeLevel) error {
	return nil
}
func (m *modelStoreGetErr) ExtendSchema(_ context.Context, _ spi.ModelRef, _ spi.SchemaDelta) error {
	return nil
}

// Compile-time contract check.
var _ spi.ModelStore = (*modelStoreGetErr)(nil)

// modelGetErrFactory is a spi.StoreFactory that returns a modelStoreGetErr
// from ModelStore(). EntityStore and the other stores are unused by the
// CreateEntity test paths that reach ModelStore.Get first.
type modelGetErrFactory struct {
	getErr error
}

func (f *modelGetErrFactory) EntityStore(_ context.Context) (spi.EntityStore, error) {
	return nil, errUnusedEntity
}
func (f *modelGetErrFactory) ModelStore(_ context.Context) (spi.ModelStore, error) {
	return &modelStoreGetErr{err: f.getErr}, nil
}
func (f *modelGetErrFactory) KeyValueStore(_ context.Context) (spi.KeyValueStore, error) {
	return nil, errUnusedEntity
}
func (f *modelGetErrFactory) MessageStore(_ context.Context) (spi.MessageStore, error) {
	return nil, errUnusedEntity
}
func (f *modelGetErrFactory) WorkflowStore(_ context.Context) (spi.WorkflowStore, error) {
	return nil, errUnusedEntity
}
func (f *modelGetErrFactory) StateMachineAuditStore(_ context.Context) (spi.StateMachineAuditStore, error) {
	return nil, errUnusedEntity
}
func (f *modelGetErrFactory) AsyncSearchStore(_ context.Context) (spi.AsyncSearchStore, error) {
	return nil, errUnusedEntity
}
func (f *modelGetErrFactory) ScheduledTaskStore(_ context.Context) (spi.ScheduledTaskStore, error) {
	return nil, errUnusedEntity
}
func (f *modelGetErrFactory) TransactionManager(_ context.Context) (spi.TransactionManager, error) {
	return nil, errUnusedEntity
}
func (f *modelGetErrFactory) Close() error { return nil }

// errUnusedEntity is a sentinel for store accessors the CreateEntity-path
// tests never reach because the ModelStore.Get error short-circuits.
var errUnusedEntity = errors.New("store not used by this test")
