package memory

import (
	"context"
	"fmt"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// ModelStore is a tenant-scoped, in-memory implementation of spi.ModelStore.
type ModelStore struct {
	tenant  spi.TenantID
	factory *StoreFactory
}

func cloneDescriptor(src *spi.ModelDescriptor) *spi.ModelDescriptor {
	cp := *src
	if src.Schema != nil {
		cp.Schema = make([]byte, len(src.Schema))
		copy(cp.Schema, src.Schema)
	}
	cp.UniqueKeys = append([]spi.UniqueKey(nil), src.UniqueKeys...)
	return &cp
}

func (s *ModelStore) Save(ctx context.Context, desc *spi.ModelDescriptor) error {
	s.factory.modelMu.Lock()
	defer s.factory.modelMu.Unlock()
	if s.factory.modelData[s.tenant] == nil {
		s.factory.modelData[s.tenant] = make(map[spi.ModelRef]*spi.ModelDescriptor)
	}
	s.factory.modelData[s.tenant][desc.Ref] = cloneDescriptor(desc)
	return nil
}

func (s *ModelStore) Get(ctx context.Context, modelRef spi.ModelRef) (*spi.ModelDescriptor, error) {
	s.factory.modelMu.RLock()
	defer s.factory.modelMu.RUnlock()
	entry, ok := s.factory.modelData[s.tenant][modelRef]
	if !ok {
		return nil, fmt.Errorf("model %s not found: %w", modelRef, spi.ErrNotFound)
	}
	return cloneDescriptor(entry), nil
}

func (s *ModelStore) GetAll(ctx context.Context) ([]spi.ModelRef, error) {
	s.factory.modelMu.RLock()
	defer s.factory.modelMu.RUnlock()
	refs := make([]spi.ModelRef, 0, len(s.factory.modelData[s.tenant]))
	for ref := range s.factory.modelData[s.tenant] {
		refs = append(refs, ref)
	}
	return refs, nil
}

func (s *ModelStore) Delete(ctx context.Context, modelRef spi.ModelRef) error {
	s.factory.modelMu.Lock()
	defer s.factory.modelMu.Unlock()
	if _, ok := s.factory.modelData[s.tenant][modelRef]; !ok {
		return fmt.Errorf("model %s not found: %w", modelRef, spi.ErrNotFound)
	}
	delete(s.factory.modelData[s.tenant], modelRef)
	return nil
}

func (s *ModelStore) Lock(ctx context.Context, modelRef spi.ModelRef) error {
	s.factory.modelMu.Lock()
	defer s.factory.modelMu.Unlock()
	entry, ok := s.factory.modelData[s.tenant][modelRef]
	if !ok {
		return fmt.Errorf("model %s not found", modelRef)
	}
	entry.State = spi.ModelLocked
	return nil
}

func (s *ModelStore) Unlock(ctx context.Context, modelRef spi.ModelRef) error {
	s.factory.modelMu.Lock()
	defer s.factory.modelMu.Unlock()
	entry, ok := s.factory.modelData[s.tenant][modelRef]
	if !ok {
		return fmt.Errorf("model %s not found", modelRef)
	}
	entry.State = spi.ModelUnlocked
	return nil
}

func (s *ModelStore) IsLocked(ctx context.Context, modelRef spi.ModelRef) (bool, error) {
	s.factory.modelMu.RLock()
	defer s.factory.modelMu.RUnlock()
	entry, ok := s.factory.modelData[s.tenant][modelRef]
	if !ok {
		return false, fmt.Errorf("model %s not found", modelRef)
	}
	return entry.State == spi.ModelLocked, nil
}

func (s *ModelStore) SetChangeLevel(ctx context.Context, modelRef spi.ModelRef, level spi.ChangeLevel) error {
	s.factory.modelMu.Lock()
	defer s.factory.modelMu.Unlock()
	entry, ok := s.factory.modelData[s.tenant][modelRef]
	if !ok {
		return fmt.Errorf("model %s not found", modelRef)
	}
	entry.ChangeLevel = level
	return nil
}

// ExtendSchema applies the delta to the current schema via the
// injected ApplyFunc. Memory is single-writer so apply-in-place
// under the factory's model mutex is correct.
func (s *ModelStore) ExtendSchema(ctx context.Context, ref spi.ModelRef, delta spi.SchemaDelta) error {
	if len(delta) == 0 {
		return nil
	}
	if s.factory.applyFunc == nil {
		return fmt.Errorf("memory: ApplyFunc not wired (use WithApplyFunc)")
	}
	s.factory.modelMu.Lock()
	defer s.factory.modelMu.Unlock()

	entry, ok := s.factory.modelData[s.tenant][ref]
	if !ok {
		return fmt.Errorf("model %s not found: %w", ref, spi.ErrNotFound)
	}
	newSchema, err := s.factory.applyFunc(entry.Schema, delta)
	if err != nil {
		return fmt.Errorf("apply delta for %s: %w", ref, err)
	}
	// Replace the schema bytes. Other descriptor fields untouched.
	entry.Schema = newSchema
	return nil
}
