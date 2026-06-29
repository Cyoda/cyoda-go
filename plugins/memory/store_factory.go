package memory

import (
	"context"
	"fmt"
	"os"
	"sync"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// Option is a functional option for NewStoreFactory.
type Option func(*StoreFactory)

// WithClock injects a custom Clock into the factory.
// Used by conformance tests to advance time deterministically.
func WithClock(c Clock) Option {
	return func(f *StoreFactory) { f.clock = c }
}

// ApplyFunc replays an opaque SchemaDelta onto a base schema.
type ApplyFunc func(base []byte, delta spi.SchemaDelta) ([]byte, error)

// WithApplyFunc installs the replay function used by ExtendSchema.
func WithApplyFunc(fn ApplyFunc) Option {
	return func(f *StoreFactory) { f.applyFunc = fn }
}

// SetApplyFunc installs the replay function used by ExtendSchema.
// May be called at most once — typically immediately after
// Plugin.NewFactory in app/app.go. Panics on double-call
// (programmer error).
//
// The parameter is the unnamed function type (not memory.ApplyFunc)
// so that an interface type-assertion in app/app.go can satisfy the
// setter uniformly across plugins.
func (f *StoreFactory) SetApplyFunc(fn func(base []byte, delta spi.SchemaDelta) ([]byte, error)) {
	if f.applyFunc != nil {
		panic("memory: SetApplyFunc called twice")
	}
	f.applyFunc = ApplyFunc(fn)
}

type StoreFactory struct {
	clock       Clock
	entityMu    sync.RWMutex
	modelMu     sync.RWMutex
	kvMu        sync.RWMutex
	msgMu       sync.RWMutex
	wfMu        sync.RWMutex
	smAuditMu   sync.RWMutex
	entityData  map[spi.TenantID]map[string][]entityVersion
	modelData   map[spi.TenantID]map[spi.ModelRef]*spi.ModelDescriptor
	kvData      map[spi.TenantID]map[string]map[string][]byte
	msgData     map[spi.TenantID]map[string]*messageEntry
	wfData      map[spi.TenantID]map[spi.ModelRef][]spi.WorkflowDefinition
	smAudit     map[spi.TenantID]map[string][]spi.StateMachineEvent // tenantID -> entityID -> events
	blobDir     string
	txManager   *TransactionManager
	searchStore *AsyncSearchStore
	applyFunc   ApplyFunc
}

func NewStoreFactory(opts ...Option) *StoreFactory {
	blobDir, err := os.MkdirTemp("", "cyoda-go-blobs-*")
	if err != nil {
		panic(fmt.Sprintf("failed to create blob temp dir: %v", err))
	}
	f := &StoreFactory{
		clock:      wallClock{},
		entityData: make(map[spi.TenantID]map[string][]entityVersion),
		modelData:  make(map[spi.TenantID]map[spi.ModelRef]*spi.ModelDescriptor),
		kvData:     make(map[spi.TenantID]map[string]map[string][]byte),
		msgData:    make(map[spi.TenantID]map[string]*messageEntry),
		wfData:     make(map[spi.TenantID]map[spi.ModelRef][]spi.WorkflowDefinition),
		smAudit:    make(map[spi.TenantID]map[string][]spi.StateMachineEvent),
		blobDir:    blobDir,
	}
	for _, o := range opts {
		o(f)
	}
	f.searchStore = newAsyncSearchStore(f.clock)
	f.initTransactionManager(&defaultUUIDGenerator{})
	return f
}

func resolveTenant(ctx context.Context) (spi.TenantID, error) {
	uc := spi.GetUserContext(ctx)
	if uc == nil {
		return "", fmt.Errorf("no user context in request — tenant cannot be resolved")
	}
	if uc.Tenant.ID == "" {
		return "", fmt.Errorf("user context has no tenant")
	}
	return uc.Tenant.ID, nil
}

func (f *StoreFactory) EntityStore(ctx context.Context) (spi.EntityStore, error) {
	tid, err := resolveTenant(ctx)
	if err != nil {
		return nil, err
	}
	return &EntityStore{tenant: tid, factory: f}, nil
}

func (f *StoreFactory) ModelStore(ctx context.Context) (spi.ModelStore, error) {
	tid, err := resolveTenant(ctx)
	if err != nil {
		return nil, err
	}
	return &ModelStore{tenant: tid, factory: f}, nil
}

func (f *StoreFactory) KeyValueStore(ctx context.Context) (spi.KeyValueStore, error) {
	tid, err := resolveTenant(ctx)
	if err != nil {
		return nil, err
	}
	return &KeyValueStore{tenant: tid, factory: f}, nil
}

func (f *StoreFactory) MessageStore(ctx context.Context) (spi.MessageStore, error) {
	tid, err := resolveTenant(ctx)
	if err != nil {
		return nil, err
	}
	return &MessageStore{tenant: tid, factory: f}, nil
}

func (f *StoreFactory) WorkflowStore(ctx context.Context) (spi.WorkflowStore, error) {
	tid, err := resolveTenant(ctx)
	if err != nil {
		return nil, err
	}
	return &WorkflowStore{tenant: tid, factory: f}, nil
}

func (f *StoreFactory) StateMachineAuditStore(ctx context.Context) (spi.StateMachineAuditStore, error) {
	tid, err := resolveTenant(ctx)
	if err != nil {
		return nil, err
	}
	return &StateMachineAuditStore{tenant: tid, factory: f}, nil
}

func (f *StoreFactory) AsyncSearchStore(_ context.Context) (spi.AsyncSearchStore, error) {
	return f.searchStore, nil
}

func (f *StoreFactory) Close() error {
	return os.RemoveAll(f.blobDir)
}

// TransactionManager implements spi.StoreFactory.
// Returns the TM registered via NewTransactionManager. Errors if none is set.
func (f *StoreFactory) TransactionManager(ctx context.Context) (spi.TransactionManager, error) {
	tm := f.GetTransactionManager()
	if tm == nil {
		return nil, fmt.Errorf("memory: TransactionManager not initialized (call NewTransactionManager first)")
	}
	return tm, nil
}

// newStoreFactory is the unexported constructor called by Plugin.NewFactory.
// It delegates to NewStoreFactory so the two paths stay in sync.
func newStoreFactory() *StoreFactory {
	return NewStoreFactory()
}

// SupportsCompositeUniqueKeys advertises composite-unique-key enforcement.
func (f *StoreFactory) SupportsCompositeUniqueKeys() bool { return true }

// initTransactionManager installs a TransactionManager on the factory.
// Called by NewStoreFactory; also callable from tests via plugin wiring.
func (f *StoreFactory) initTransactionManager(uuids spi.UUIDGenerator) {
	f.NewTransactionManager(uuids)
}
