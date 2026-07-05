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

type refreshingStore struct {
	fakeModelStore
	refreshErr error
}

func (r refreshingStore) RefreshAndGet(context.Context, spi.ModelRef) (*spi.ModelDescriptor, error) {
	if r.refreshErr != nil {
		return nil, r.refreshErr
	}
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

	// Not found in cache but refresh finds it → nil (multi-node peer registration).
	if err := EnsureModelRegistered(context.Background(), refreshingStore{fakeModelStore: fakeModelStore{getErr: spi.ErrNotFound}}, ref); err != nil {
		t.Fatalf("peer-registered model: want nil after refresh, got %v", err)
	}

	// Not found in cache and refresh also not found → 404 MODEL_NOT_FOUND.
	err = EnsureModelRegistered(context.Background(), refreshingStore{fakeModelStore: fakeModelStore{getErr: spi.ErrNotFound}, refreshErr: spi.ErrNotFound}, ref)
	if err == nil || err.Status != http.StatusNotFound || err.Code != ErrCodeModelNotFound {
		t.Fatalf("refresh-miss: want 404 MODEL_NOT_FOUND, got %v", err)
	}

	// RefreshAndGet returns a non-ErrNotFound error (store failure) → Internal/500, NOT 404.
	storeErr := errors.New("boom")
	err = EnsureModelRegistered(context.Background(), refreshingStore{fakeModelStore: fakeModelStore{getErr: spi.ErrNotFound}, refreshErr: storeErr}, ref)
	if err == nil || err.Status != http.StatusInternalServerError {
		t.Fatalf("refresh-store-error: want 500 Internal, got %v", err)
	}
}
