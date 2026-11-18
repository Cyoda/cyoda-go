package parity

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/cyoda/cyoda-go/e2e/parity/client"
)

// RunSchemaNumericFoldCarveout asserts the property that replaces
// byte-identical convergence for numeric-leaf widening.
//
// A value a leaf already holds does not move the model, so whether a write
// contributes its type depends on what the leaf declared when the write was
// judged — which depends on what arrived before it, and on what arrived
// concurrently. Writing 2147483648 then 12.5 into an [INTEGER] leaf leaves
// [UNBOUND_DECIMAL]; the reverse order leaves [DOUBLE], because by then the
// leaf holds the larger number. Both are correct: each admits every value
// that was written, and each is a widening of what came before.
//
// That is the property asserted here, in both orders: every written value is
// findable afterward (SyncSearch EQUALS), and the two orders reach two
// DIFFERENT models — order A's amount declares UNBOUND_DECIMAL, order B's
// declares DOUBLE. Asserting only "findable" would pass for "anything goes";
// asserting the specific declared type per order is what makes this a
// carve-out with a property, not the absence of one.
//
// Byte-identity is asserted for structural extension by
// RunSchemaExtensionConcurrentConvergence, where it still holds.
func RunSchemaNumericFoldCarveout(t *testing.T, fixture BackendFixture) {
	orders := []struct {
		values   []string
		wantType string
	}{
		{values: []string{"2147483648", "12.5"}, wantType: "UNBOUND_DECIMAL"},
		{values: []string{"12.5", "2147483648"}, wantType: "DOUBLE"},
	}

	for _, order := range orders {
		tenant := fixture.NewTenant(t)
		c := client.NewClient(fixture.BaseURL(), tenant.Token)

		const modelName = "numeric-fold-carveout"
		const modelVersion = 1

		seed, _ := json.Marshal(map[string]any{"amount": 1})
		if err := c.ImportModel(t, modelName, modelVersion, string(seed)); err != nil {
			t.Fatalf("ImportModel: %v", err)
		}
		if err := c.LockModel(t, modelName, modelVersion); err != nil {
			t.Fatalf("LockModel: %v", err)
		}
		if err := c.SetChangeLevel(t, modelName, modelVersion, "TYPE"); err != nil {
			t.Fatalf("SetChangeLevel: %v", err)
		}

		for _, v := range order.values {
			body := `{"amount":` + v + `}`
			if _, err := c.CreateEntity(t, modelName, modelVersion, body); err != nil {
				t.Fatalf("order %v: write %s: %v", order.values, v, err)
			}
		}

		// The property: every value that was written is findable in the
		// model that resulted. Not "the model is a particular set of bytes".
		for _, v := range order.values {
			cond := fmt.Sprintf(`{"type":"simple","jsonPath":"$.amount","operatorType":"EQUALS","value":%s}`, v)
			hits, err := c.SyncSearch(t, modelName, modelVersion, cond)
			if err != nil {
				t.Fatalf("order %v: SyncSearch %s: %v", order.values, v, err)
			}
			if len(hits) == 0 {
				t.Errorf("order %v: %s was written but cannot be found", order.values, v)
			}
		}

		// The other half of the property: the two orders reach two DIFFERENT
		// models, and each is the specific widening §6 predicts — not just
		// "some" widening.
		raw, err := c.ExportModel(t, "SIMPLE_VIEW", modelName, modelVersion)
		if err != nil {
			t.Fatalf("order %v: ExportModel: %v", order.values, err)
		}
		if !strings.Contains(string(raw), order.wantType) {
			t.Errorf("order %v: expected %s classification for amount; schema: %s", order.values, order.wantType, raw)
		}
	}
}
