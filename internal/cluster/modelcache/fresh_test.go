package modelcache_test

import (
	"context"
	"testing"

	spi "github.com/cyoda/cyoda-go-spi"
	"github.com/cyoda/cyoda-go/internal/cluster/modelcache"
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

// TestGetFresh_StoreThatCannotReadFreshFailsClosed pins that a decision on the
// model's state is never made on a read that may be stale: a store without
// RefreshAndGet is a wiring defect, so the read fails instead of falling back
// to Get.
func TestGetFresh_StoreThatCannotReadFreshFailsClosed(t *testing.T) {
	ref := spi.ModelRef{EntityName: "m", ModelVersion: "1"}
	desc, err := modelcache.GetFresh(context.Background(), getOnlyStore{stale: &spi.ModelDescriptor{Ref: ref}}, ref)
	if err == nil {
		t.Fatalf("GetFresh answered %+v from Get; want an error", desc)
	}
}
