package e2e_test

import (
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
)

// TestModelSchemaExtensions_ConcurrentUpdatesNoConflict is the regression
// test for the hot-row serialization bug this effort fixes.
//
// Before the fix: with ChangeLevel set on a model, concurrent entity updates
// whose payloads each imply (at most) an additive schema extension all call
// validateOrExtend → modelStore.Save(desc), which updates the single models
// row under REPEATABLE READ. First committer wins; every other commit fails
// with SQLSTATE 40001 surfaced as a CONFLICT 409.
//
// After the fix: validateOrExtend computes a SchemaDelta via schema.Diff
// and calls modelStore.ExtendSchema, which inserts per-tx rows into
// model_schema_extensions — no hot-row contention. All concurrent writers
// commit.
func TestModelSchemaExtensions_ConcurrentUpdatesNoConflict(t *testing.T) {
	const modelName = "e2e-schema-ext-concurrent"
	const version = 1
	const N = 8

	// 1. Bring the model up: import, lock, STRUCTURAL.
	importModelE2E(t, modelName, version)
	lockModelE2E(t, modelName, version)
	setChangeLevelE2E(t, modelName, version, "STRUCTURAL")

	// 2. Fire N concurrent entity creates. Each payload introduces a
	// distinct new property (field_i) so every write genuinely triggers
	// an ExtendSchema path. Under the old Save-based path, N-1 of these
	// would fail with 40001/CONFLICT.
	ctx := e2eCtx(t)
	results := make([]httpResult, N)
	var wg sync.WaitGroup
	for i := 0; i < N; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			payload := fmt.Sprintf(
				`{"name":"Test-%d","amount":%d,"status":"new","field_%d":"val_%d"}`,
				i, i*10, i, i,
			)
			path := fmt.Sprintf("/api/entity/JSON/%s/%d", modelName, version)
			results[i] = resultOf(doAuthRaw(ctx, http.MethodPost, path, payload))
		}()
	}
	wg.Wait()

	// Classify on the test goroutine — Fatal is only legal here.
	var conflicts, other, okCount int
	for i, res := range results {
		if res.err != nil {
			t.Fatalf("goroutine %d transport error: %v", i, res.err)
		}
		switch res.status {
		case http.StatusOK:
			okCount++
		case http.StatusConflict:
			conflicts++
			t.Logf("goroutine %d got 409 CONFLICT: %s", i, res.body)
		default:
			other++
			t.Logf("goroutine %d got %d: %s", i, res.status, res.body)
		}
	}

	if conflicts != 0 {
		t.Errorf("regression: %d of %d concurrent entity updates returned CONFLICT (409). ExtendSchema must eliminate hot-row contention.",
			conflicts, N)
	}
	if other != 0 {
		t.Errorf("%d of %d concurrent entity updates returned neither 200 nor 409 (unexpected status).",
			other, N)
	}
	if okCount != N {
		t.Errorf("expected all %d concurrent creates to succeed, got %d", N, okCount)
	}

	// 3. After the storm, the folded schema must reflect every new field.
	schema := exportModelE2E(t, modelName, version)
	raw := fmt.Sprintf("%v", schema)
	for i := 0; i < N; i++ {
		want := fmt.Sprintf("field_%d", i)
		if !strings.Contains(raw, want) {
			t.Errorf("folded schema missing %q after concurrent extensions: %s", want, raw)
		}
	}
}

// TestModelSchemaExtensions_SequentialFoldAcrossRequests asserts that
// the Postgres Get-fold correctly replays the extension log across
// multiple HTTP requests. Each POST appends a delta; the final export
// must reflect every accumulated field.
//
// This is the single-node read-side correctness check. Multi-node
// self-healing via RefreshAndGet on a stale cache is covered by unit
// tests (internal/domain/entity handler_validate_refresh_test.go and
// internal/cluster/modelcache integration_test.go); the end-to-end
// multi-node variant waits on the deferred factory-level caching
// wrap (TODO(G1-followup)).
func TestModelSchemaExtensions_SequentialFoldAcrossRequests(t *testing.T) {
	const modelName = "e2e-schema-ext-sequential"
	const version = 1

	importModelE2E(t, modelName, version)
	lockModelE2E(t, modelName, version)
	setChangeLevelE2E(t, modelName, version, "STRUCTURAL")

	// Six sequential writes, each adding a new field.
	for i := 0; i < 6; i++ {
		payload := fmt.Sprintf(
			`{"name":"Sequential-%d","amount":%d,"status":"new","seq_field_%d":"val_%d"}`,
			i, i, i, i,
		)
		path := fmt.Sprintf("/api/entity/JSON/%s/%d", modelName, version)
		resp := doAuth(t, http.MethodPost, path, payload)
		body := readBody(t, resp)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("create #%d failed: status=%d body=%s", i, resp.StatusCode, body)
		}
	}

	schema := exportModelE2E(t, modelName, version)
	raw := fmt.Sprintf("%v", schema)
	for i := 0; i < 6; i++ {
		want := fmt.Sprintf("seq_field_%d", i)
		if !strings.Contains(raw, want) {
			t.Errorf("folded schema missing %q after sequential writes: %s", want, raw)
		}
	}
}
