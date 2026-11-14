package model

import (
	"context"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// getOnlyStore answers Get from a stale descriptor and offers no way to read
// storage fresh, as a bare backend store does.
type getOnlyStore struct {
	spi.ModelStore
	stale *spi.ModelDescriptor
}

func (s getOnlyStore) Get(context.Context, spi.ModelRef) (*spi.ModelDescriptor, error) {
	return s.stale, nil
}

// TestGetModelFresh_StoreThatCannotReadFreshFailsClosed pins that the admin
// state gate never decides on a read that may be stale: a store without
// RefreshAndGet is a wiring defect, so the read fails instead of falling back
// to Get.
func TestGetModelFresh_StoreThatCannotReadFreshFailsClosed(t *testing.T) {
	ref := spi.ModelRef{EntityName: "m", ModelVersion: "1"}
	desc, err := getModelFresh(context.Background(), getOnlyStore{stale: &spi.ModelDescriptor{Ref: ref}}, ref)
	if err == nil {
		t.Fatalf("getModelFresh answered %+v from Get; want an error", desc)
	}
}
