package multinode

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/cyoda-platform/cyoda-go/e2e/parity/client"
)

func init() {
	Register(
		NamedTest{
			Name: "UnknownModel_BoundedRefresh_ColdCacheSucceeds",
			Fn:   RunUnknownModel_BoundedRefresh_ColdCacheSucceeds,
		},
	)
}

// RunUnknownModel_BoundedRefresh_ColdCacheSucceeds proves that
// EnsureModelRegistered's bounded RefreshAndGet prevents a false 404 when a
// model-scoped operation reaches a node whose model cache is cold.
//
// Scenario:
//  1. Register (import + lock) the model on node A.
//  2. Issue GET /api/entity/stats/{model}/{version} on node B, which has never
//     seen this tenant's model (cold in-process cache).
//  3. Assert the op succeeds (200) — NOT 404 MODEL_NOT_FOUND.
//
// If the bounded refresh were removed (or EnsureModelRegistered only consulted
// its in-process cache), node B would return 404 because the model was written
// to the authoritative store via node A, not via node B. The test would fail on
// the MODEL_NOT_FOUND guard below.
//
// Not in the main parity registry — this is an isolated cluster test exercising
// the cluster-specific cold-cache→refresh path, not a cross-backend contract.
func RunUnknownModel_BoundedRefresh_ColdCacheSucceeds(t *testing.T, fixture MultiNodeFixture) {
	t.Helper()
	urls := fixture.BaseURLs()
	if len(urls) < 2 {
		t.Fatalf("need at least 2 nodes for cold-cache test, got %d", len(urls))
	}
	tenant := fixture.NewTenant(t)

	// Fresh tenant means this model name is unique within the test run;
	// no UUID suffix needed because tenant isolation provides the namespace.
	const modelName = "cold-cache-refresh"
	const modelVersion = 1

	// Step 1: register and lock the model on node A (index 0).
	cA := client.NewClient(urls[0], tenant.Token)
	if err := cA.ImportModel(t, modelName, modelVersion, `{"k":1}`); err != nil {
		t.Fatalf("node A ImportModel: %v", err)
	}
	if err := cA.LockModel(t, modelName, modelVersion); err != nil {
		t.Fatalf("node A LockModel: %v", err)
	}

	// Step 2: issue GET /api/entity/stats/{model}/{version} on node B (index 1).
	// Node B has no in-process knowledge of this tenant's model yet (cold cache).
	// EnsureModelRegistered must perform a bounded RefreshAndGet against the
	// shared authoritative store (postgres) and find the model registered via A.
	cB := client.NewClient(urls[1], tenant.Token)
	statsPath := fmt.Sprintf("/api/entity/stats/%s/%d", modelName, modelVersion)
	status, body, err := cB.DoJSONBodyRaw(t, http.MethodGet, statsPath, nil)
	if err != nil && status == 0 {
		t.Fatalf("node B transport error: %v", err)
	}

	// Step 3: must NOT be a false 404 (cold-cache regression guard).
	if status == http.StatusNotFound {
		var envelope struct {
			Properties struct {
				ErrorCode string `json:"errorCode"`
			} `json:"properties"`
		}
		_ = json.Unmarshal(body, &envelope)
		if envelope.Properties.ErrorCode == "MODEL_NOT_FOUND" {
			t.Fatalf("cold-cache false-404: node B returned MODEL_NOT_FOUND for a model registered on node A; "+
				"EnsureModelRegistered's bounded RefreshAndGet did not consult the authoritative store (body: %s)", string(body))
		}
		t.Fatalf("node B returned 404 (body: %s)", string(body))
	}
	if status != http.StatusOK {
		t.Fatalf("node B: status = %d (want 200); body: %s", status, string(body))
	}
}
