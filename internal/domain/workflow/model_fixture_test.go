package workflow

import (
	"context"
	"errors"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// lockTestModel makes ref exist LOCKED in factory's model store, as the API
// saves an entity only into a LOCKED model. A model the test already stored
// keeps its schema and keys; a missing one is stored with none.
func lockTestModel(t testing.TB, factory spi.StoreFactory, ctx context.Context, ref spi.ModelRef) {
	t.Helper()
	ms, err := factory.ModelStore(ctx)
	if err != nil {
		t.Fatalf("ModelStore: %v", err)
	}
	if _, err := ms.Get(ctx, ref); errors.Is(err, spi.ErrNotFound) {
		if err := ms.Save(ctx, &spi.ModelDescriptor{Ref: ref, State: spi.ModelUnlocked}); err != nil {
			t.Fatalf("save model %s: %v", ref, err)
		}
	} else if err != nil {
		t.Fatalf("get model %s: %v", ref, err)
	}
	if err := ms.Lock(ctx, ref); err != nil {
		t.Fatalf("lock model %s: %v", ref, err)
	}
}

// saveLockedModel stores desc and leaves it LOCKED. A model already stored
// LOCKED (by lockTestModel, before the test registers its schema) is
// unlocked first: a LOCKED model cannot be replaced.
func saveLockedModel(t testing.TB, factory spi.StoreFactory, ctx context.Context, desc *spi.ModelDescriptor) {
	t.Helper()
	ms, err := factory.ModelStore(ctx)
	if err != nil {
		t.Fatalf("ModelStore: %v", err)
	}
	if locked, err := ms.IsLocked(ctx, desc.Ref); err == nil && locked {
		if err := ms.Unlock(ctx, desc.Ref); err != nil {
			t.Fatalf("unlock model %s: %v", desc.Ref, err)
		}
	} else if err != nil && !errors.Is(err, spi.ErrNotFound) {
		t.Fatalf("model %s: %v", desc.Ref, err)
	}
	if err := ms.Save(ctx, desc); err != nil {
		t.Fatalf("save model %s: %v", desc.Ref, err)
	}
	if err := ms.Lock(ctx, desc.Ref); err != nil {
		t.Fatalf("lock model %s: %v", desc.Ref, err)
	}
}
