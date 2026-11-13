package entity

import (
	"context"
	"sync"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/domain/model/schema"
)

// refreshingStore is a ModelStore fake that implements RefreshAndGet
// with a queue: each Get returns the head of getQueue; each
// RefreshAndGet returns the head of refreshQueue.
type refreshingStore struct {
	mu           sync.Mutex
	getQueue     []*spi.ModelDescriptor
	refreshQueue []*spi.ModelDescriptor
	getCount     int
	refreshCount int
}

func (s *refreshingStore) Get(_ context.Context, _ spi.ModelRef) (*spi.ModelDescriptor, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.getCount++
	if len(s.getQueue) == 0 {
		// Fallback to last refresh result if queue drained.
		if len(s.refreshQueue) > 0 {
			return s.refreshQueue[0], nil
		}
		return nil, nil
	}
	d := s.getQueue[0]
	s.getQueue = s.getQueue[1:]
	return d, nil
}

func (s *refreshingStore) RefreshAndGet(_ context.Context, _ spi.ModelRef) (*spi.ModelDescriptor, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refreshCount++
	if len(s.refreshQueue) == 0 {
		return nil, nil
	}
	d := s.refreshQueue[0]
	s.refreshQueue = s.refreshQueue[1:]
	return d, nil
}

// Satisfy the rest of spi.ModelStore with no-ops.
func (s *refreshingStore) Save(context.Context, *spi.ModelDescriptor) error     { return nil }
func (s *refreshingStore) GetAll(context.Context) ([]spi.ModelRef, error)       { return nil, nil }
func (s *refreshingStore) Delete(context.Context, spi.ModelRef) error           { return nil }
func (s *refreshingStore) Lock(context.Context, spi.ModelRef) error             { return nil }
func (s *refreshingStore) Unlock(context.Context, spi.ModelRef) error           { return nil }
func (s *refreshingStore) IsLocked(context.Context, spi.ModelRef) (bool, error) { return true, nil }
func (s *refreshingStore) SetChangeLevel(context.Context, spi.ModelRef, spi.ChangeLevel) error {
	return nil
}
func (s *refreshingStore) ExtendSchema(context.Context, spi.ModelRef, spi.SchemaDelta) error {
	return nil
}

// Compile-time check that refreshingStore satisfies the SPI contract.
var _ spi.ModelStore = (*refreshingStore)(nil)

func (s *refreshingStore) GetCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.getCount
}

func (s *refreshingStore) RefreshCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.refreshCount
}

// buildDescriptorWithFields constructs a LOCKED descriptor whose Schema
// encodes an object node with the given named string-leaf children.
func buildDescriptorWithFields(t *testing.T, ref spi.ModelRef, fields ...string) *spi.ModelDescriptor {
	t.Helper()
	node := schema.NewObjectNode()
	for _, f := range fields {
		node.SetChild(f, schema.NewLeafNode(schema.String))
	}
	raw, err := schema.Marshal(node)
	if err != nil {
		t.Fatalf("schema.Marshal: %v", err)
	}
	return &spi.ModelDescriptor{
		Ref:    ref,
		State:  spi.ModelLocked,
		Schema: raw,
	}
}

// failingModelStore is refreshingStore with either of its two reads failing, as
// a saturated pool or a cancelled statement makes them fail.
type failingModelStore struct {
	*refreshingStore
	getErr     error
	refreshErr error
}

func (s *failingModelStore) Get(ctx context.Context, ref spi.ModelRef) (*spi.ModelDescriptor, error) {
	if s.getErr != nil {
		return nil, s.getErr
	}
	return s.refreshingStore.Get(ctx, ref)
}

func (s *failingModelStore) RefreshAndGet(ctx context.Context, ref spi.ModelRef) (*spi.ModelDescriptor, error) {
	if s.refreshErr != nil {
		return nil, s.refreshErr
	}
	return s.refreshingStore.RefreshAndGet(ctx, ref)
}
