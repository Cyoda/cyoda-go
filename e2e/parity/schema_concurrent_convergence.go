package parity

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sync"
	"testing"

	"github.com/cyoda/cyoda-go/e2e/parity/client"
)

// RunSchemaExtensionConcurrentConvergence asserts B-I7 for STRUCTURAL
// extension: N concurrent new-field extensions on the same model all succeed
// and the final fold is byte-identical to a serial replay via the in-memory
// oracle. This asserts permutation invariance of delta application (B-I5)
// through the HTTP layer.
//
// If a backend's output depends on delta application order (e.g. field order
// in the exported schema reflects insertion order), this test will fail. That
// failure is the invariant we want to catch — do not loosen the assertion for
// the shapes tested here.
//
// Byte-identity does NOT extend to numeric- and temporal-leaf widening. A
// value a leaf already holds does not move the model, so whether a write
// contributes its type depends on the snapshot the writing node held — the
// ordinary state during a cross-node gossip window. The weaker property that
// does hold there (every fold is monotone and admits every written value) is
// asserted by RunSchemaNumericFoldCarveout. Adding a numeric leaf to the
// scenario below would make it flaky, not stricter.
func RunSchemaExtensionConcurrentConvergence(t *testing.T, fixture BackendFixture) {
	const N = 10
	tenant := fixture.NewTenant(t)
	// client.Client is goroutine-safe: the struct is read-only after
	// construction, and *http.Client is documented safe for concurrent use.
	c := client.NewClient(fixture.BaseURL(), tenant.Token)

	const modelName = "b-i7-concurrent"
	const modelVersion = 1

	// Seed with field_seed. The initial sample doc defines the schema.
	seedBody := map[string]string{"field_seed": "seed"}
	seedRaw, _ := json.Marshal(seedBody)
	if err := c.ImportModel(t, modelName, modelVersion, string(seedRaw)); err != nil {
		t.Fatalf("ImportModel: %v", err)
	}
	if err := c.LockModel(t, modelName, modelVersion); err != nil {
		t.Fatalf("LockModel: %v", err)
	}
	if err := c.SetChangeLevel(t, modelName, modelVersion, "STRUCTURAL"); err != nil {
		t.Fatalf("SetChangeLevel STRUCTURAL: %v", err)
	}

	// Fan out N goroutines, each creates an entity with one NEW field.
	var wg sync.WaitGroup
	errs := make([]error, N)
	wg.Add(N)
	for i := 0; i < N; i++ {
		i := i
		go func() {
			defer wg.Done()
			body := map[string]string{"field_seed": "x", fmt.Sprintf("field_%d", i): "v"}
			raw, _ := json.Marshal(body)
			if _, err := c.CreateEntity(t, modelName, modelVersion, string(raw)); err != nil {
				errs[i] = err
			}
		}()
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Errorf("goroutine %d CreateEntity: %v", i, err)
		}
	}

	got, err := c.ExportModel(t, "SIMPLE_VIEW", modelName, modelVersion)
	if err != nil {
		t.Fatalf("ExportModel: %v", err)
	}

	// Oracle: replay serially in a canonical order {seed, 0, 1, ..., N-1}.
	// If the backend preserves insertion order, concurrent interleaving
	// will diverge from this serial replay — that's what this test catches.
	bodies := make([]map[string]string, 0, N+1)
	bodies = append(bodies, seedBody)
	for i := 0; i < N; i++ {
		bodies = append(bodies, map[string]string{"field_seed": "x", fmt.Sprintf("field_%d", i): "v"})
	}
	expected, err := expectedSimpleViewFromBodies(bodies, "LOCKED")
	if err != nil {
		t.Fatalf("oracle: %v", err)
	}
	if !bytes.Equal(got, expected) {
		t.Errorf("%s: concurrent-fold != serial-replay-fold\n  got:      %s\n  expected: %s",
			t.Name(), string(got), string(expected))
	}
}
