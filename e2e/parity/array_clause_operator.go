package parity

import (
	"net/http"
	"testing"
	"time"

	"github.com/cyoda-platform/cyoda-go/e2e/parity/client"
)

// RunArrayClauseOperator pins that an array clause's operatorType applies at
// every tested position, identically on every backend, on the pushed-down
// path, on the in-memory path (the clause inside NOT), and through async
// search, which stores the condition and re-parses it where the job runs. The
// refusals are identical on every backend too.
func RunArrayClauseOperator(t *testing.T, fixture BackendFixture) {
	tenant := fixture.NewTenant(t)
	c := client.NewClient(fixture.BaseURL(), tenant.Token)

	const modelName = "parity-array-clause-operator"
	const modelVersion = 1
	setupModelWithWorkflow(t, c, modelName, modelVersion, `{"name":"S","scores":[1,2,3],"tags":["t"]}`, searchWorkflowJSON)
	for _, doc := range []string{`{"name":"A","scores":[90,40,75],"tags":["a","b"]}`, `{"name":"B","scores":[10,95,60],"tags":["c"]}`} {
		if _, err := c.CreateEntity(t, modelName, modelVersion, doc); err != nil {
			t.Fatalf("CreateEntity: %v", err)
		}
	}

	const gt = `{"type":"array","jsonPath":"$.scores[*]","operatorType":"GREATER_THAN","values":[50,null,70]}`
	// Each case states its expected count, or (want < 0) the simple clause it
	// means: past the end of the array the answer is whatever the positional
	// simple clause answers under the vacuity rules (path-grammar.md §5).
	cases := []struct {
		name, condition string
		want            int
		equiv           string
	}{
		{"GREATER_THAN per position", gt, 1, ""},
		{"inside NOT", `{"type":"group","operator":"NOT","conditions":[` + gt + `]}`, 1, ""},
		{"BETWEEN per position", `{"type":"array","jsonPath":"$.scores[*]","operatorType":"BETWEEN_INCLUSIVE","values":[null,[90,100]]}`, 1, ""},
		{"IS_NULL past the end", `{"type":"array","jsonPath":"$.scores[*]","operatorType":"IS_NULL","values":[null,null,null,true]}`, -1,
			`{"type":"simple","jsonPath":"$.scores[3]","operatorType":"IS_NULL","value":true}`},
		{"NOT_EQUAL past the end", `{"type":"array","jsonPath":"$.scores[*]","operatorType":"NOT_EQUAL","values":[null,null,null,null,null,7]}`, -1,
			`{"type":"simple","jsonPath":"$.scores[5]","operatorType":"NOT_EQUAL","value":7}`},
	}
	for _, tc := range cases {
		results, err := c.SyncSearch(t, modelName, modelVersion, tc.condition)
		if err != nil {
			t.Fatalf("[%s] SyncSearch: %v", tc.name, err)
		}
		want := tc.want
		if want < 0 {
			same, err := c.SyncSearch(t, modelName, modelVersion, tc.equiv)
			if err != nil {
				t.Fatalf("[%s] simple equivalent: %v", tc.name, err)
			}
			want = len(same)
		}
		if len(results) != want {
			t.Errorf("[%s] want %d result(s), got %d", tc.name, want, len(results))
		}
	}

	page, err := c.AwaitAsyncSearchResults(t, modelName, modelVersion, gt, 30*time.Second)
	if err != nil {
		t.Fatalf("async: %v", err)
	}
	if len(page.Content) != 1 {
		t.Errorf("async GREATER_THAN: want 1 result, got %d", len(page.Content))
	}

	// Refused on every backend before any backend evaluates it. An array
	// operand outside BETWEEN, and an object or array BETWEEN bound, are
	// refused because the backends would otherwise each answer them
	// differently.
	for _, r := range []struct{ name, condition string }{
		{"clause testing nothing", `{"type":"array","jsonPath":"$.scores[*]","operatorType":"EQUALS","values":[null]}`},
		{"array entry under NOT_EQUAL", `{"type":"array","jsonPath":"$.tags[*]","operatorType":"NOT_EQUAL","values":[["a","b"]]}`},
		{"array value under NOT_EQUAL", `{"type":"simple","jsonPath":"$.name","operatorType":"NOT_EQUAL","value":["A","B"]}`},
		{"object BETWEEN bound", `{"type":"simple","jsonPath":"$.name","operatorType":"BETWEEN","value":[{"a":1},"z"]}`},
		{"array BETWEEN bound inside NOT", `{"type":"group","operator":"NOT","conditions":[{"type":"array","jsonPath":"$.tags[*]","operatorType":"BETWEEN","values":[[["a"],"z"]]}]}`},
	} {
		status, body, err := c.SyncSearchRaw(t, modelName, modelVersion, r.condition)
		if err != nil {
			t.Fatalf("[%s] SyncSearchRaw: %v", r.name, err)
		}
		if status != http.StatusBadRequest || !containsErrorCode(body, "INVALID_CONDITION") {
			t.Errorf("[%s] want 400 INVALID_CONDITION, got %d %s", r.name, status, body)
		}
	}
}
