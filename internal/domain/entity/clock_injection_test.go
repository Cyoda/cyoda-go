package entity

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	wfengine "github.com/cyoda-platform/cyoda-go/internal/domain/workflow"
	"github.com/cyoda-platform/cyoda-go/internal/testing/fakeclock"
	"github.com/cyoda-platform/cyoda-go/internal/txgate"
	"github.com/cyoda-platform/cyoda-go/plugins/memory"
)

// dateCapturingFactory records the pre-commit meta dates of every entity the
// handler hands to Save, before the store re-stamps them.
type dateCapturingFactory struct {
	spi.StoreFactory
	mu       sync.Mutex
	created  []time.Time
	modified []time.Time
}

func (f *dateCapturingFactory) EntityStore(ctx context.Context) (spi.EntityStore, error) {
	d, err := f.StoreFactory.EntityStore(ctx)
	if err != nil {
		return nil, err
	}
	return &dateCapturingStore{EntityStore: d, f: f}, nil
}

type dateCapturingStore struct {
	spi.EntityStore
	f *dateCapturingFactory
}

func (s *dateCapturingStore) Save(ctx context.Context, e *spi.Entity) (int64, error) {
	s.f.mu.Lock()
	s.f.created = append(s.f.created, e.Meta.CreationDate)
	s.f.modified = append(s.f.modified, e.Meta.LastModifiedDate)
	s.f.mu.Unlock()
	return s.EntityStore.Save(ctx, e)
}

func TestEntityPreCommitDatesFromClock(t *testing.T) {
	at := time.Date(2015, 3, 1, 0, 0, 0, 0, time.UTC)
	mem := memory.NewStoreFactory()
	t.Cleanup(func() { mem.Close() })
	f := &dateCapturingFactory{StoreFactory: mem}
	ctx := orderTestCtx()
	registerOrderModel(t, ctx, f)
	txMgr, err := f.TransactionManager(ctx)
	if err != nil {
		t.Fatalf("TransactionManager: %v", err)
	}
	clk := fakeclock.New(at)
	engine := wfengine.NewEngine(f, common.NewDefaultUUIDGenerator(), txMgr, wfengine.WithClock(clk))
	h := New(f, txMgr, common.NewDefaultUUIDGenerator(), engine, txgate.New(), newTestConsistencyFor(txMgr), clk)

	if _, err := h.CreateEntity(ctx, CreateEntityInput{
		EntityName: "Order", ModelVersion: "1", Format: "JSON",
		Data: json.RawMessage(`{"email":"a@b.com","accountId":"x"}`),
	}); err != nil {
		t.Fatalf("CreateEntity: %v", err)
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.created) == 0 {
		t.Fatal("no entity reached Save")
	}
	for i := range f.created {
		if !f.created[i].Equal(at) || !f.modified[i].Equal(at) {
			t.Errorf("save %d: CreationDate=%v LastModifiedDate=%v, want both %v", i, f.created[i], f.modified[i], at)
		}
	}
}
