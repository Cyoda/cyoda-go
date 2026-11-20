package entity

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"testing"

	spi "github.com/cyoda/cyoda-go-spi"
	"github.com/cyoda/cyoda-go/internal/common"
	"github.com/cyoda/cyoda-go/internal/domain/model/schema"
	wfengine "github.com/cyoda/cyoda-go/internal/domain/workflow"
	"github.com/cyoda/cyoda-go/internal/platformclock"
	"github.com/cyoda/cyoda-go/internal/txcoord"
	"github.com/cyoda/cyoda-go/plugins/memory"
)

// staleStateModelStore is a model store double. Its Get answers the stored
// model with the state a node's cache may hold (cachedState); its
// RefreshAndGet, the read that skips the cache, answers it with the state
// storage holds (freshState). It records how often each was called.
type staleStateModelStore struct {
	spi.ModelStore
	cachedState spi.ModelState
	freshState  spi.ModelState
	freshErr    error // when set, the fresh read answers it

	mu        sync.Mutex
	gets      int
	refreshes int
}

func (s *staleStateModelStore) withState(ctx context.Context, ref spi.ModelRef, state spi.ModelState) (*spi.ModelDescriptor, error) {
	d, err := s.ModelStore.Get(ctx, ref)
	if err != nil {
		return nil, err
	}
	c := *d
	c.State = state
	return &c, nil
}

func (s *staleStateModelStore) Get(ctx context.Context, ref spi.ModelRef) (*spi.ModelDescriptor, error) {
	func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.gets++
	}()
	return s.withState(ctx, ref, s.cachedState)
}

func (s *staleStateModelStore) RefreshAndGet(ctx context.Context, ref spi.ModelRef) (*spi.ModelDescriptor, error) {
	func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.refreshes++
	}()
	if s.freshErr != nil {
		return nil, s.freshErr
	}
	return s.withState(ctx, ref, s.freshState)
}

func (s *staleStateModelStore) refreshCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.refreshes
}

// staleStateFactory hands the entity handler the double as its model store.
type staleStateFactory struct {
	spi.StoreFactory
	ms *staleStateModelStore
}

func (f *staleStateFactory) ModelStore(context.Context) (spi.ModelStore, error) {
	return f.ms, nil
}

// newStaleStateHandler stores the "Person" model LOCKED and returns a handler
// whose model store reports it as cachedState through Get and as freshState
// through RefreshAndGet. The workflow engine reads the stored model.
func newStaleStateHandler(t *testing.T, cachedState, freshState spi.ModelState) (*Handler, context.Context, *staleStateModelStore) {
	t.Helper()
	ctx := spi.WithUserContext(context.Background(), &spi.UserContext{
		UserID:   "reread-test-user",
		UserName: "Reread Test",
		Tenant:   spi.Tenant{ID: "reread-tenant", Name: "Reread"},
		Roles:    []string{"user"},
	})
	inner := memory.NewStoreFactory()
	txMgr := mustTxMgr(t, inner)

	node := schema.NewObjectNode()
	node.SetChild("name", schema.NewLeafNode(schema.String))
	raw, err := schema.Marshal(node)
	if err != nil {
		t.Fatalf("schema.Marshal: %v", err)
	}
	stored, err := inner.ModelStore(ctx)
	if err != nil {
		t.Fatalf("ModelStore: %v", err)
	}
	if err := stored.Save(ctx, &spi.ModelDescriptor{
		Ref:    spi.ModelRef{EntityName: "Person", ModelVersion: "1"},
		State:  spi.ModelLocked,
		Schema: raw,
	}); err != nil {
		t.Fatalf("ModelStore.Save: %v", err)
	}

	double := &staleStateModelStore{ModelStore: stored, cachedState: cachedState, freshState: freshState}
	engine := wfengine.NewEngine(cachedFactory(inner), common.NewDefaultUUIDGenerator(), txMgr, platformclock.Real())
	h := New(&staleStateFactory{StoreFactory: cachedFactory(inner), ms: double}, txMgr, common.NewDefaultUUIDGenerator(), engine, txcoord.New(txMgr), newTestConsistencyFor(txMgr), platformclock.Real())
	return h, ctx, double
}

// staleStateDoors are the two create doors: a single create and a collection
// create.
var staleStateDoors = []struct {
	name   string
	create func(h *Handler, ctx context.Context) (*EntityTransactionResult, error)
}{
	{"Create", func(h *Handler, ctx context.Context) (*EntityTransactionResult, error) {
		return h.CreateEntity(ctx, CreateEntityInput{EntityName: "Person", ModelVersion: "1", Format: "JSON", Data: json.RawMessage(`{"name":"A"}`)})
	}},
	{"CreateCollection", func(h *Handler, ctx context.Context) (*EntityTransactionResult, error) {
		return h.CreateEntityCollection(ctx, []CollectionItem{
			{ModelName: "Person", ModelVersion: 1, Payload: json.RawMessage(`{"name":"A"}`)},
			{ModelName: "Person", ModelVersion: 1, Payload: json.RawMessage(`{"name":"B"}`)},
		})
	}},
}

// TestCreate_CachedUnlocked_FreshLocked_Succeeds: a create that reads the
// model as not LOCKED (a node's stale view after a lock) reads it again from
// storage; storage says LOCKED, so the create goes on and succeeds.
func TestCreate_CachedUnlocked_FreshLocked_Succeeds(t *testing.T) {
	for _, door := range staleStateDoors {
		t.Run(door.name, func(t *testing.T) {
			h, ctx, double := newStaleStateHandler(t, spi.ModelUnlocked, spi.ModelLocked)
			res, err := door.create(h, ctx)
			if err != nil {
				t.Fatalf("create: %v; want success after the fresh read says LOCKED", err)
			}
			if len(res.EntityIDs) == 0 {
				t.Fatal("create returned no entity ids")
			}
			if n := double.refreshCount(); n != 1 {
				t.Errorf("fresh reads = %d, want 1", n)
			}
		})
	}
}

// TestCreate_CachedUnlocked_FreshUnlocked_409: when the fresh read also says
// not LOCKED, the create is refused with 409 MODEL_NOT_LOCKED.
func TestCreate_CachedUnlocked_FreshUnlocked_409(t *testing.T) {
	for _, door := range staleStateDoors {
		t.Run(door.name, func(t *testing.T) {
			h, ctx, double := newStaleStateHandler(t, spi.ModelUnlocked, spi.ModelUnlocked)
			_, err := door.create(h, ctx)
			var appErr *common.AppError
			if !errors.As(err, &appErr) || appErr.Status != http.StatusConflict || appErr.Code != common.ErrCodeModelNotLocked {
				t.Fatalf("create err = %v; want 409 %s", err, common.ErrCodeModelNotLocked)
			}
			if n := double.refreshCount(); n != 1 {
				t.Errorf("fresh reads = %d, want 1", n)
			}
		})
	}
}

// TestCreate_CachedLocked_NoFreshRead: a create that reads the model as
// LOCKED does not read it again.
func TestCreate_CachedLocked_NoFreshRead(t *testing.T) {
	for _, door := range staleStateDoors {
		t.Run(door.name, func(t *testing.T) {
			h, ctx, double := newStaleStateHandler(t, spi.ModelLocked, spi.ModelUnlocked)
			if _, err := door.create(h, ctx); err != nil {
				t.Fatalf("create: %v", err)
			}
			if n := double.refreshCount(); n != 0 {
				t.Errorf("fresh reads = %d, want 0", n)
			}
		})
	}
}

// TestCreate_CachedUnlocked_FreshGone_404: when the model is deleted between
// the two reads, the fresh read finds no model and the create answers 404
// MODEL_NOT_FOUND.
func TestCreate_CachedUnlocked_FreshGone_404(t *testing.T) {
	for _, door := range staleStateDoors {
		t.Run(door.name, func(t *testing.T) {
			h, ctx, double := newStaleStateHandler(t, spi.ModelUnlocked, spi.ModelUnlocked)
			double.freshErr = fmt.Errorf("model gone: %w", spi.ErrNotFound)
			_, err := door.create(h, ctx)
			var appErr *common.AppError
			if !errors.As(err, &appErr) || appErr.Status != http.StatusNotFound || appErr.Code != common.ErrCodeModelNotFound {
				t.Fatalf("create err = %v; want 404 %s", err, common.ErrCodeModelNotFound)
			}
		})
	}
}
