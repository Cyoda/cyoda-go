package app

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/plugins/memory"
)

// nilJobClaimStore is a real AsyncSearchStore with one deliberate defect:
// ClaimStale hands back a slice containing a nil element. search.FailStaleJobs
// dereferences job.TenantID for every element it is given, so a nil there
// panics — and, in an unrecovered goroutine, takes the whole process down.
// No current plugin returns nil, so this is hardening: the reaper tick must
// survive a misbehaving store the way every other engine-work goroutine
// does.
type nilJobClaimStore struct {
	spi.AsyncSearchStore
}

func (s *nilJobClaimStore) ClaimStale(context.Context, time.Duration, int) ([]*spi.SearchJob, error) {
	return []*spi.SearchJob{nil}, nil
}

// TestSearchReaperTick_RecoversPanicAndLatchesHealth pins that one reaper
// tick recovers a panic raised anywhere beneath it, latches the node-health
// flag the same way the async-search executor's own recovery does, and
// returns normally so the ticker loop keeps running.
func TestSearchReaperTick_RecoversPanicAndLatchesHealth(t *testing.T) {
	base := memory.NewStoreFactory()
	defer base.Close()
	realAsync, err := base.AsyncSearchStore(context.Background())
	if err != nil {
		t.Fatalf("AsyncSearchStore: %v", err)
	}

	health := &atomic.Bool{}
	health.Store(true)

	// Must not panic out of the call.
	searchReaperTick(context.Background(), &nilJobClaimStore{AsyncSearchStore: realAsync},
		time.Hour, 5*time.Minute, health)

	if health.Load() {
		t.Error("healthFlag = true after a recovered panic in the reaper; a node that has panicked has state nothing has verified")
	}
}

// TestSearchReaperTick_HealthyStoreLeavesHealthAlone is the control: a tick
// against a well-behaved store must not latch the node unhealthy.
func TestSearchReaperTick_HealthyStoreLeavesHealthAlone(t *testing.T) {
	base := memory.NewStoreFactory()
	defer base.Close()
	realAsync, err := base.AsyncSearchStore(context.Background())
	if err != nil {
		t.Fatalf("AsyncSearchStore: %v", err)
	}

	health := &atomic.Bool{}
	health.Store(true)

	searchReaperTick(context.Background(), realAsync, time.Hour, 5*time.Minute, health)

	if !health.Load() {
		t.Error("healthFlag = false after an uneventful reaper tick")
	}
}
