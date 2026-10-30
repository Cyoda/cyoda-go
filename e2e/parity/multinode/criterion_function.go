package multinode

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/cyoda-platform/cyoda-go/e2e/parity"
	"github.com/cyoda-platform/cyoda-go/e2e/parity/client"
)

// criterion_function.go — a function condition nested in a group, handed over.
// The owner has no compute node for the tag; a peer does. The callout carries
// the function condition's own JSON and the peer builds it from that alone.

func init() {
	Register(
		NamedTest{Name: "CriterionFunction_HandOverFromAGroup", Fn: RunCriterionFunction_HandOverFromAGroup},
	)
}

// RunCriterionFunction_HandOverFromAGroup: AND(amount>5, function) on a
// transition of the owner's entity; the function's callout reaches the peer's
// compute client as a criterion callout under the function's name.
func RunCriterionFunction_HandOverFromAGroup(t *testing.T, fixture MultiNodeFixture) {
	urls := mnRequire(t, fixture, 2)
	tenant := fixture.NewTenant(t)
	owner := client.NewClient(urls[0], tenant.Token)
	const model, tag = "mn-cf", "mn-cf"

	remote := mnWarmUp(t, fixture, owner, tenant, 1, "mn-cf-w", tag)
	wf, _ := json.Marshal(map[string]any{
		"importMode": "REPLACE",
		"workflows": []any{map[string]any{
			"version": "1.5", "name": "mn-cf-wf", "initialState": "NONE", "active": true,
			"states": map[string]any{
				"NONE": map[string]any{"transitions": []any{map[string]any{
					"name": "init", "next": "ACTIVE", "manual": false,
					"criterion": map[string]any{"type": "group", "operator": "AND", "conditions": []any{
						map[string]any{"type": "simple", "jsonPath": "$.amount", "operatorType": "GREATER_THAN", "value": 5},
						map[string]any{"type": "function", "function": map[string]any{
							"name":      "always-true",
							"config":    map[string]any{"calculationNodesTags": tag},
							"criterion": map[string]any{"type": "simple", "jsonPath": "$.status", "operatorType": "EQUALS", "value": "new"},
						}},
					}},
				}}},
				"ACTIVE": map[string]any{},
			},
		}},
	})
	cbRouteSetupModel(t, owner, model, cbRouteSampleNoWriteback, string(wf))

	id, err := owner.CreateEntity(t, model, 1, mnSample)
	if err != nil {
		t.Fatalf("create through the owner: %v", err)
	}
	got, err := owner.GetEntity(t, id)
	if err != nil {
		t.Fatalf("read the entity: %v", err)
	}
	if got.Meta.State != "ACTIVE" {
		t.Errorf("state = %q; want ACTIVE", got.Meta.State)
	}
	// The warm-up's processor callout is in the same record; count the
	// criterion callouts only.
	var criteria []parity.ReceivedCallout
	deadline := time.Now().Add(5 * time.Second) // realtime: test poll deadline
	for {
		criteria = criteria[:0]
		for _, r := range remote.Received(t) {
			if r.Kind == "criterion" {
				criteria = append(criteria, r)
			}
		}
		if len(criteria) > 0 || time.Now().After(deadline) { // realtime: test poll deadline
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if len(criteria) != 1 || criteria[0].Name != "always-true" {
		t.Errorf("the peer's compute client received criterion callouts %+v; want exactly one named always-true", criteria)
	}
}
