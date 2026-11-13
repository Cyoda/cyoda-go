package common

import (
	"context"
	"errors"
	"net/http"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

type fakeModelStore struct {
	spi.ModelStore // embed for unused methods; nil is fine (never called)
	getErr         error
}

func (f fakeModelStore) Get(context.Context, spi.ModelRef) (*spi.ModelDescriptor, error) {
	if f.getErr != nil {
		return nil, f.getErr
	}
	return &spi.ModelDescriptor{}, nil
}

// refreshingStore has a RefreshAndGet that finds the model; EnsureModelRegistered
// must not call it.
type refreshingStore struct {
	fakeModelStore
}

func (r refreshingStore) RefreshAndGet(context.Context, spi.ModelRef) (*spi.ModelDescriptor, error) {
	return &spi.ModelDescriptor{}, nil
}

func TestEnsureModelRegistered(t *testing.T) {
	ref := spi.ModelRef{EntityName: "Order", ModelVersion: "1"}

	if err := EnsureModelRegistered(context.Background(), fakeModelStore{}, ref); err != nil {
		t.Fatalf("registered model: want nil, got %v", err)
	}

	// Not found, no refresh capability → 404.
	err := EnsureModelRegistered(context.Background(), fakeModelStore{getErr: spi.ErrNotFound}, ref)
	if err == nil || err.Status != http.StatusNotFound || err.Code != ErrCodeModelNotFound {
		t.Fatalf("unknown model: want 404 MODEL_NOT_FOUND, got %v", err)
	}

	// Not found: the answer is the Get's, even when the store could refresh.
	if err := EnsureModelRegistered(context.Background(), refreshingStore{fakeModelStore: fakeModelStore{getErr: spi.ErrNotFound}}, ref); err == nil || err.Status != http.StatusNotFound {
		t.Fatalf("refreshable store, unknown model: want 404 from Get alone, got %v", err)
	}

	// A store failure on Get is Internal/500, NOT 404.
	err = EnsureModelRegistered(context.Background(), fakeModelStore{getErr: errors.New("boom")}, ref)
	if err == nil || err.Status != http.StatusInternalServerError {
		t.Fatalf("store error: want 500 Internal, got %v", err)
	}
}
