package multinode

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cyoda/cyoda-go/e2e/externalapi/driver"
	parityclient "github.com/cyoda/cyoda-go/e2e/parity/client"
)

func init() {
	Register(
		NamedTest{Name: "ExternalAPI_10_01_LoadBalancerEndToEnd", Fn: RunExternalAPI_10_01_LoadBalancerEndToEnd},
		NamedTest{Name: "ExternalAPI_10_02_ReadbackReachesAllReplicas", Fn: RunExternalAPI_10_02_ReadbackReachesAllReplicas},
		NamedTest{Name: "ExternalAPI_10_03_ParallelUpdatesSameEntity", Fn: RunExternalAPI_10_03_ParallelUpdatesSameEntity},
	)
}

// RunExternalAPI_10_01_LoadBalancerEndToEnd — dictionary 10/01.
// Round-robin model+entity create across N nodes via separate Drivers;
// verify each Driver successfully reaches the cluster.
func RunExternalAPI_10_01_LoadBalancerEndToEnd(t *testing.T, fixture MultiNodeFixture) {
	t.Helper()
	urls := fixture.BaseURLs()
	if len(urls) < 2 {
		t.Fatalf("need at least 2 nodes for 10/01, got %d", len(urls))
	}
	tenant := fixture.NewTenant(t)

	// Driver per node. 10/01 only needs the first node for setup.
	d0 := driver.NewRemote(t, urls[0], tenant.Token)
	if err := d0.CreateModelFromSample("multi1", 1, `{"k":1}`); err != nil {
		t.Fatalf("create model on node 0: %v", err)
	}
	if err := d0.LockModel("multi1", 1); err != nil {
		t.Fatalf("lock on node 0: %v", err)
	}

	// Round-robin entity creation across all nodes.
	ids := make([]uuid.UUID, len(urls))
	for i, url := range urls {
		di := driver.NewRemote(t, url, tenant.Token)
		id, err := di.CreateEntity("multi1", 1, fmt.Sprintf(`{"k":%d}`, i))
		if err != nil {
			t.Fatalf("CreateEntity via node %d: %v", i, err)
		}
		ids[i] = id
	}

	// Each entity must be readable from any node (consistency).
	for i, id := range ids {
		for j, url := range urls {
			dj := driver.NewRemote(t, url, tenant.Token)
			got, err := dj.GetEntity(id)
			if err != nil {
				t.Errorf("read entity[%d] from node %d: %v", i, j, err)
				continue
			}
			if got.Data["k"] != float64(i) {
				t.Errorf("entity[%d] from node %d: got k=%v, want %d", i, j, got.Data["k"], i)
			}
		}
	}
}

// RunExternalAPI_10_02_ReadbackReachesAllReplicas — dictionary 10/02.
// Write to node A, then GET, list and search from node B (≠ A). Repeat for every (A,B) pair. This is the running check of the write-visibility contract: a successful write response means the write is visible to every subsequent read on every node.
func RunExternalAPI_10_02_ReadbackReachesAllReplicas(t *testing.T, fixture MultiNodeFixture) {
	t.Helper()
	urls := fixture.BaseURLs()
	if len(urls) < 2 {
		t.Fatalf("need at least 2 nodes for 10/02, got %d", len(urls))
	}
	tenant := fixture.NewTenant(t)

	dSetup := driver.NewRemote(t, urls[0], tenant.Token)
	if err := dSetup.CreateModelFromSample("multi2", 1, `{"k":1}`); err != nil {
		t.Fatalf("create model: %v", err)
	}
	if err := dSetup.LockModel("multi2", 1); err != nil {
		t.Fatalf("lock: %v", err)
	}

	for writerIdx, writerURL := range urls {
		dW := driver.NewRemote(t, writerURL, tenant.Token)
		id, err := dW.CreateEntity("multi2", 1, fmt.Sprintf(`{"k":%d}`, writerIdx))
		if err != nil {
			t.Fatalf("write via node %d: %v", writerIdx, err)
		}
		for readerIdx, readerURL := range urls {
			if readerIdx == writerIdx {
				continue
			}
			dR := driver.NewRemote(t, readerURL, tenant.Token)
			got, err := dR.GetEntity(id)
			if err != nil {
				t.Errorf("read via node %d (written via %d): %v", readerIdx, writerIdx, err)
				continue
			}
			if got.Data["k"] != float64(writerIdx) {
				t.Errorf("read via node %d (written via %d): got k=%v, want %d", readerIdx, writerIdx, got.Data["k"], writerIdx)
			}

			// The contract is not only read-by-id: a listing and a search
			// issued on the other node immediately after the write's
			// response must contain the entity too.
			listed, err := dR.ListEntitiesByModel("multi2", 1)
			if err != nil {
				t.Errorf("list via node %d (written via %d): %v", readerIdx, writerIdx, err)
			} else if !containsEntityID(listed, id) {
				t.Errorf("list via node %d (written via %d): entity %s missing", readerIdx, writerIdx, id)
			}
			found, err := dR.SyncSearch("multi2", 1,
				fmt.Sprintf(`{"type":"simple","jsonPath":"$.k","operatorType":"EQUALS","value":%d}`, writerIdx))
			if err != nil {
				t.Errorf("search via node %d (written via %d): %v", readerIdx, writerIdx, err)
			} else if !containsEntityID(found, id) {
				t.Errorf("search via node %d (written via %d): entity %s missing", readerIdx, writerIdx, id)
			}
		}
	}
}

// RunExternalAPI_10_03_ParallelUpdatesSameEntity — dictionary 10/03.
// Concurrent updates from N nodes to the same entity must serialise
// without data loss. The system uses SI+FCW (first-committer-wins) at
// the transaction layer, so under contention some writers commit
// cleanly and the rest abort with 409 CONFLICT, retryable: true. This
// test does not retry on 409; it just asserts that the final state
// reflects exactly one of the racing writers' payloads (i.e. the
// surviving committer's value), with no torn or corrupt state.
func RunExternalAPI_10_03_ParallelUpdatesSameEntity(t *testing.T, fixture MultiNodeFixture) {
	t.Helper()
	urls := fixture.BaseURLs()
	if len(urls) < 2 {
		t.Fatalf("need at least 2 nodes for 10/03, got %d", len(urls))
	}
	tenant := fixture.NewTenant(t)

	d0 := driver.NewRemote(t, urls[0], tenant.Token)
	if err := d0.CreateModelFromSample("multi3", 1, `{"counter":0}`); err != nil {
		t.Fatalf("create model: %v", err)
	}
	if err := d0.LockModel("multi3", 1); err != nil {
		t.Fatalf("lock: %v", err)
	}
	id, err := d0.CreateEntity("multi3", 1, `{"counter":0}`)
	if err != nil {
		t.Fatalf("create entity: %v", err)
	}

	// N goroutines, one per node, each issuing a counter-set update.
	var wg sync.WaitGroup
	for i, url := range urls {
		wg.Add(1)
		go func(idx int, u string) {
			defer wg.Done()
			di := driver.NewRemote(t, u, tenant.Token)
			body := fmt.Sprintf(`{"counter":%d}`, idx+1)
			if err := di.UpdateEntityData(id, body); err != nil {
				t.Logf("10/03 goroutine %d: UpdateEntityData returned %v (FCW: loser aborts on commit; final GET asserts a surviving writer's payload is visible)", idx, err)
			}
		}(i, url)
	}
	wg.Wait()

	// Wait briefly for cluster gossip to converge.
	time.Sleep(200 * time.Millisecond)

	// Final state must reflect a surviving committer's payload — one of
	// the racing values 1..N. Under SI+FCW the value is whichever writer
	// committed without an FCW abort; this test does not constrain which.
	got, err := d0.GetEntity(id)
	if err != nil {
		t.Fatalf("final read: %v", err)
	}
	final, ok := got.Data["counter"].(float64)
	if !ok {
		t.Fatalf("counter not a number: %v", got.Data["counter"])
	}
	if int(final) < 1 || int(final) > len(urls) {
		t.Errorf("final counter: got %v, want 1..%d (one of the parallel writes)", final, len(urls))
	}
}

func containsEntityID(results []parityclient.EntityResult, id uuid.UUID) bool {
	for _, r := range results {
		if r.Meta.ID == id.String() {
			return true
		}
	}
	return false
}
