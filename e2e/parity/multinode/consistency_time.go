package multinode

import (
	"fmt"
	"net/http"
	"net/url"
	"testing"
	"time"

	parityclient "github.com/cyoda-platform/cyoda-go/e2e/parity/client"
)

// consistency_time.go — a save confirmed on one node is included by another
// node's consistency time, and a consistency time handed out by one node is
// never refused by another. Instants are the ones the servers hand out
// (verbatim); no process clock is read.

func init() {
	Register(
		NamedTest{Name: "ConsistencyTime_AcrossNodes", Fn: RunConsistencyTime_AcrossNodes},
	)
}

// RunConsistencyTime_AcrossNodes: for every ordered pair of nodes (A, B), a save
// confirmed on A is found by a read at the C node B hands out next, and by an
// async search submitted on B without a pointInTime; and the C node A handed
// out after the save passes node B's fence (never 400) and finds the save.
func RunConsistencyTime_AcrossNodes(t *testing.T, fixture MultiNodeFixture) {
	t.Helper()
	urls := fixture.BaseURLs()
	if len(urls) < 2 {
		t.Fatalf("need at least 2 nodes, got %d", len(urls))
	}
	tenant := fixture.NewTenant(t)
	const model = "ct-multi"
	clients := make([]*parityclient.Client, len(urls))
	for i, u := range urls {
		clients[i] = parityclient.NewClient(u, tenant.Token)
	}
	if err := clients[0].ImportModel(t, model, 1, `{"k":1}`); err != nil {
		t.Fatalf("import model: %v", err)
	}
	if err := clients[0].LockModel(t, model, 1); err != nil {
		t.Fatalf("lock model: %v", err)
	}

	for a, ca := range clients {
		id, err := ca.CreateEntity(t, model, 1, fmt.Sprintf(`{"k":%d}`, a))
		if err != nil {
			t.Fatalf("create on node %d: %v", a, err)
		}
		cFromA, err := ca.GetConsistencyTime(t)
		if err != nil {
			t.Fatalf("consistency time on node %d: %v", a, err)
		}
		for b, cb := range clients {
			if b == a {
				continue
			}
			// The C node B hands out includes A's confirmed save.
			cFromB, err := cb.GetConsistencyTime(t)
			if err != nil {
				t.Fatalf("consistency time on node %d: %v", b, err)
			}
			path := "/api/entity/" + id.String() + "?pointInTime=" + url.QueryEscape(cFromB)
			if status, body, err := cb.DoRaw(t, http.MethodGet, path, ""); err != nil || status != http.StatusOK {
				t.Errorf("save on node %d not found at node %d's C %s: status=%d err=%v body=%s", a, b, cFromB, status, err, body)
			}

			// A's C passes B's fence: the save is visible there too.
			path = "/api/entity/" + id.String() + "?pointInTime=" + url.QueryEscape(cFromA)
			if status, body, err := cb.DoRaw(t, http.MethodGet, path, ""); err != nil || status != http.StatusOK {
				t.Errorf("node %d's C %s refused or missing the save on node %d: status=%d err=%v body=%s", a, cFromA, b, status, err, body)
			}
			path = fmt.Sprintf("/api/entity/%s/1?pointInTime=%s", model, url.QueryEscape(cFromA))
			if status, body, err := cb.DoRaw(t, http.MethodGet, path, ""); err != nil || status != http.StatusOK {
				t.Errorf("list at node %d's C on node %d: status=%d err=%v body=%s", a, b, status, err, body)
			}

			// An async search submitted on B without a pointInTime includes it.
			res, err := cb.AwaitAsyncSearchResults(t, model, 1,
				fmt.Sprintf(`{"type":"simple","jsonPath":"$.k","operatorType":"EQUALS","value":%d}`, a), 30*time.Second)
			if err != nil {
				t.Errorf("async search on node %d: %v", b, err)
				continue
			}
			if !containsEntityID(res.Content, id) {
				t.Errorf("async search submitted on node %d misses the save confirmed on node %d (%s)", b, a, id)
			}
		}
	}
}
