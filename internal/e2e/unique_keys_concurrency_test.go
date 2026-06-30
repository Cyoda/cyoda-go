package e2e_test

// unique_keys_concurrency_test.go — isolated single-backend (postgres) concurrency
// test for composite unique keys.
//
// Verifies the First-Committer-Wins invariant through the full HTTP stack:
// two concurrent entity-create requests with the SAME composite key value must
// not both commit. Exactly one succeeds; the other is rejected with a 409
// (either UNIQUE_VIOLATION or a retryable CONFLICT); exactly one live entity
// persists.
//
// This is an isolated single-backend test — NOT in the cross-backend parity suite.
// See .claude/rules/test-coverage.md ("Concurrency/race: isolated single-backend
// e2e, never the shared parity suite").

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
)

// TestUniqueKeysConcurrency_Postgres verifies First-Committer-Wins for composite
// unique keys under concurrent load on the postgres backend via the full HTTP stack.
//
// Consistency invariants asserted (interleave-agnostic):
//   - Exactly one of the two concurrent requests succeeds (200 OK).
//   - Exactly one is rejected with 409 — UNIQUE_VIOLATION (the unique constraint
//     fires) or a retryable CONFLICT (serialization abort under SERIALIZABLE
//     isolation). Both are valid loser outcomes.
//   - After both requests complete, exactly one live entity holds the contested
//     key value (no torn write, no duplicate).
func TestUniqueKeysConcurrency_Postgres(t *testing.T) {
	const model = "e2e-uk-concurrent-pg"

	// Set up a model whose schema includes an "email" field, then declare a
	// unique key on $.email before locking.
	const sample = `{"email":"seed@x.com","name":"Seed","amount":0,"status":"draft"}`
	importModelWithSample(t, model, 1, sample)

	keysJSON := `{"uniqueKeys":[{"id":"email-key","fields":["$.email"]}]}`
	status, body := setUniqueKeysE2E(t, model, 1, keysJSON)
	if status != http.StatusOK {
		t.Fatalf("setUniqueKeys: expected 200, got %d: %s", status, body)
	}

	lockModelE2E(t, model, 1)

	wfJSON := fmt.Sprintf(`{
		"importMode": "REPLACE",
		"workflows": [{
			"version": "1.1", "name": "%s-wf", "initialState": "NONE", "active": true,
			"states": {
				"NONE":    {"transitions": [{"name": "init", "next": "CREATED", "manual": false}]},
				"CREATED": {}
			}
		}]
	}`, model)
	wfStatus, wfBody := importWorkflowE2E(t, model, 1, wfJSON)
	if wfStatus != http.StatusOK {
		t.Fatalf("importWorkflow: expected 200, got %d: %s", wfStatus, wfBody)
	}

	// Fetch the auth token in the test goroutine — calling t.Fatal from a
	// spawned goroutine is unsafe per Go's testing contract.
	token := getToken(t, "testclient", "testsecret")

	// Both goroutines race to create an entity with the SAME email value.
	const sharedEmail = "concurrent@x.com"
	postURL := serverURL + fmt.Sprintf("/api/entity/JSON/%s/1", model)

	type result struct {
		status int
		body   string
	}
	// Each goroutine writes to its own index — no data race on the slice.
	results := make([]result, 2)
	var wg sync.WaitGroup

	for i := 0; i < 2; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Distinct payloads so only the unique key (email) is shared,
			// not every field. This ensures both creates are otherwise valid.
			payload := fmt.Sprintf(
				`{"email":%q,"name":"User%d","amount":%d,"status":"draft"}`,
				sharedEmail, i, i)
			req, err := http.NewRequest(http.MethodPost, postURL, strings.NewReader(payload))
			if err != nil {
				results[i] = result{status: -1, body: err.Error()}
				return
			}
			req.Header.Set("Authorization", "Bearer "+token)
			req.Header.Set("Content-Type", "application/json")

			// Do NOT use doAuth: it auto-retries retryable 409s, which would
			// mask whether the server returns UNIQUE_VIOLATION or a retryable
			// CONFLICT. We observe the raw first response.
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				results[i] = result{status: -1, body: err.Error()}
				return
			}
			raw, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			results[i] = result{status: resp.StatusCode, body: string(raw)}
		}()
	}
	wg.Wait()

	// --- Consistency assertions (not tied to which goroutine wins) ---

	successCount := 0
	loserCount := 0
	for idx, r := range results {
		switch r.status {
		case http.StatusOK:
			successCount++
		case http.StatusConflict:
			// Both outcomes are valid for the loser:
			//   UNIQUE_VIOLATION  — the unique constraint fired directly.
			//   retryable CONFLICT — a serialization abort (40001/40P01) before
			//                        the constraint check (SERIALIZABLE isolation).
			// Either proves the second commit was blocked; no torn write.
			loserCount++
		default:
			t.Errorf("goroutine %d: unexpected status %d:\n%s", idx, r.status, r.body)
		}
	}

	if successCount != 1 {
		t.Errorf("expected exactly 1 success (200), got %d\n  [0] %d: %s\n  [1] %d: %s",
			successCount,
			results[0].status, results[0].body,
			results[1].status, results[1].body)
	}
	if loserCount != 1 {
		t.Errorf("expected exactly 1 conflict/loser (409), got %d\n  [0] %d: %s\n  [1] %d: %s",
			loserCount,
			results[0].status, results[0].body,
			results[1].status, results[1].body)
	}

	// --- No torn write: exactly one live entity must persist ---
	//
	// Both requests used the same email (the unique key), so if both committed
	// we'd see 2 entities — which the unique constraint must prevent.
	listPath := fmt.Sprintf("/api/entity/%s/1?pageSize=100&pageNumber=0", model)
	listResp := doAuth(t, http.MethodGet, listPath, "")
	listBody := readBody(t, listResp)
	if listResp.StatusCode != http.StatusOK {
		t.Fatalf("list entities: expected 200, got %d: %s", listResp.StatusCode, listBody)
	}

	var entities []map[string]any
	if err := json.Unmarshal([]byte(listBody), &entities); err != nil {
		t.Fatalf("list entities: parse error: %v\nbody: %s", err, listBody)
	}
	if len(entities) != 1 {
		t.Errorf("expected exactly 1 live entity (no torn write), got %d\nentities: %v",
			len(entities), entities)
	}
}
