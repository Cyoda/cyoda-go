package e2e_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/cyoda-platform/cyoda-go/internal/common/commontest"
)

// An array clause applies its operatorType at every position whose entry in
// `values` is not null. Entities: A scores [90,40,75] tags [a,b], B scores
// [10,95,60] tags [c].

const scoresWorkflow = `{
	"importMode": "REPLACE",
	"workflows": [{
		"version": "1.1", "name": "scores-wf", "initialState": "NONE", "active": true,
		"states": {"NONE": {"transitions": [{"name": "init", "next": "CREATED", "manual": false}]}, "CREATED": {}}
	}]
}`

func setupScoresModel(t *testing.T, model string) (idA, idB string) {
	t.Helper()
	setupModelSampleWithWorkflow(t, model, `{"name":"S","scores":[1,2,3],"tags":["t"]}`, scoresWorkflow)
	idA = createEntityE2E(t, model, 1, `{"name":"A","scores":[90,40,75],"tags":["a","b"]}`)
	idB = createEntityE2E(t, model, 1, `{"name":"B","scores":[10,95,60],"tags":["c"]}`)
	return idA, idB
}

// arrayClauseRefusals is every 4xx row of the spec's error table that a
// condition body can trigger, with the code each surface answers.
var arrayClauseRefusals = []struct {
	name, cond, code string
}{
	{"missing operator", `{"type":"array","jsonPath":"$.scores[*]","values":[50]}`, "INVALID_CONDITION"},
	{"old spec shape", `{"type":"array","jsonPath":"$.scores[*]","operatorType":"EQUALS","value":["90"]}`, "INVALID_CONDITION"},
	{"unknown operator", `{"type":"array","jsonPath":"$.scores[*]","operatorType":"FROBNICATE","values":[50]}`, "INVALID_CONDITION"},
	{"all null", `{"type":"array","jsonPath":"$.scores[*]","operatorType":"EQUALS","values":[null,null]}`, "INVALID_CONDITION"},
	{"empty values", `{"type":"array","jsonPath":"$.scores[*]","operatorType":"EQUALS","values":[]}`, "INVALID_CONDITION"},
	{"object entry", `{"type":"array","jsonPath":"$.scores[*]","operatorType":"EQUALS","values":[{"a":1}]}`, "INVALID_CONDITION"},
	{"BETWEEN not a pair", `{"type":"array","jsonPath":"$.scores[*]","operatorType":"BETWEEN","values":[5]}`, "INVALID_CONDITION"},
	{"values not an array", `{"type":"array","jsonPath":"$.scores[*]","operatorType":"EQUALS","values":"x"}`, "INVALID_CONDITION"},
	// An array entry is a single value except under BETWEEN. On a STRING array
	// it would otherwise match on some backends and not on others; under
	// NOT_EQUAL a conditional delete would remove nearly every entity.
	{"array entry outside BETWEEN", `{"type":"array","jsonPath":"$.tags[*]","operatorType":"NOT_EQUAL","values":[["a","b"]]}`, "INVALID_CONDITION"},
	{"null BETWEEN bound", `{"type":"array","jsonPath":"$.scores[*]","operatorType":"BETWEEN","values":[[null,5]]}`, "INVALID_CONDITION"},
	// A BETWEEN bound is a scalar. On a STRING array an object or array bound
	// otherwise reaches the backend: PostgreSQL answers 500.
	{"object BETWEEN bound", `{"type":"array","jsonPath":"$.tags[*]","operatorType":"BETWEEN","values":[[{"a":1},"z"]]}`, "INVALID_CONDITION"},
	{"array BETWEEN bound", `{"type":"array","jsonPath":"$.tags[*]","operatorType":"BETWEEN_INCLUSIVE","values":[[["a"],"z"]]}`, "INVALID_CONDITION"},
	{"bare path", `{"type":"array","jsonPath":"$.scores","operatorType":"EQUALS","values":[90]}`, "INVALID_FIELD_PATH"},
	{"earlier wildcard", `{"type":"array","jsonPath":"$.o[*].scores[*]","operatorType":"EQUALS","values":[90]}`, "INVALID_FIELD_PATH"},
	{"text operator on numbers", `{"type":"array","jsonPath":"$.scores[*]","operatorType":"CONTAINS","values":["9"]}`, "CONDITION_TYPE_MISMATCH"},
}

func TestSearch_Sync_ArrayClauseOperator(t *testing.T) {
	const model = "e2e-array-op-sync"
	setupScoresModel(t, model)
	for _, tc := range []struct {
		name, cond, want string // want: the one entity name expected
	}{
		{"GREATER_THAN at 0 and 2", `{"type":"array","jsonPath":"$.scores[*]","operatorType":"GREATER_THAN","values":[50,null,70]}`, "A"},
		{"BETWEEN at 1", `{"type":"array","jsonPath":"$.scores[*]","operatorType":"BETWEEN_INCLUSIVE","values":[null,[30,45]]}`, "A"},
		{"Cloud alias operation", `{"type":"array","jsonPath":"$.scores[*]","operation":"LESS_THAN","values":[50]}`, "B"},
	} {
		status, results := directSearch(t, model, 1, tc.cond)
		if status != http.StatusOK {
			t.Fatalf("%s: expected 200, got %d", tc.name, status)
		}
		if len(results) != 1 || extractDataString(t, results[0], "name") != tc.want {
			t.Errorf("%s: want only %s, got %v", tc.name, tc.want, results)
		}
	}
}

// IS_NULL at a position answers what the positional simple clause answers.
// The comparison, not a fixed count, is the contract: the array clause is the
// AND of its positional simple clauses.
func TestSearch_Sync_ArrayClauseIsNullPerPosition(t *testing.T) {
	const model = "e2e-array-op-isnull"
	setupScoresModel(t, model)
	for _, tc := range []struct{ array, simple string }{
		{`{"type":"array","jsonPath":"$.scores[*]","operatorType":"IS_NULL","values":[null,true]}`,
			`{"type":"simple","jsonPath":"$.scores[1]","operatorType":"IS_NULL"}`},
		{`{"type":"array","jsonPath":"$.scores[*]","operatorType":"IS_NULL","values":[null,null,null,true]}`,
			`{"type":"simple","jsonPath":"$.scores[3]","operatorType":"IS_NULL"}`},
	} {
		aStatus, aResults := directSearch(t, model, 1, tc.array)
		sStatus, sResults := directSearch(t, model, 1, tc.simple)
		if aStatus != http.StatusOK || sStatus != http.StatusOK {
			t.Fatalf("%s: expected 200 for both, got array %d, simple %d", tc.array, aStatus, sStatus)
		}
		got, want := extractDataStrings(t, aResults, "name"), extractDataStrings(t, sResults, "name")
		slices.Sort(got)
		slices.Sort(want)
		if !slices.Equal(got, want) {
			t.Errorf("%s: array clause matched %v, positional simple clause matched %v", tc.array, got, want)
		}
	}
}

func TestSearch_Sync_ArrayClauseRefusals(t *testing.T) {
	const model = "e2e-array-op-sync-refuse"
	setupScoresModel(t, model)
	for _, tc := range arrayClauseRefusals {
		resp := doAuth(t, http.MethodPost, fmt.Sprintf("/api/search/direct/%s/1", model), tc.cond)
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("%s: expected 400, got %d; body: %s", tc.name, resp.StatusCode, readBody(t, resp))
		}
		commontest.ExpectErrorCode(t, resp, tc.code)
		resp.Body.Close()
	}
}

func TestSearch_Async_ArrayClauseOperator(t *testing.T) {
	const model = "e2e-array-op-async"
	idA, _ := setupScoresModel(t, model)
	jobID := submitAsyncSearch(t, model, 1, `{"type":"array","jsonPath":"$.scores[*]","operatorType":"GREATER_THAN","values":[50,null,70]}`)
	if s := waitForAsyncSearch(t, jobID, 10*time.Second); s != "SUCCESSFUL" {
		t.Fatalf("expected SUCCESSFUL, got %q", s)
	}
	resp := doAuth(t, http.MethodGet, fmt.Sprintf("/api/search/async/%s?pageSize=10&pageNumber=0", jobID), "")
	body := readBody(t, resp)
	var page struct {
		Content []struct {
			Meta struct {
				ID string `json:"id"`
			} `json:"meta"`
		} `json:"content"`
	}
	if err := json.Unmarshal([]byte(body), &page); err != nil {
		t.Fatalf("decode: %v; body %s", err, body)
	}
	if len(page.Content) != 1 || page.Content[0].Meta.ID != idA {
		t.Errorf("want only %s, got %s", idA, body)
	}
}

func TestSearch_Async_ArrayClauseRefusals_NoJobIssued(t *testing.T) {
	const model = "e2e-array-op-async-refuse"
	setupScoresModel(t, model)
	for _, tc := range arrayClauseRefusals {
		resp := doAuth(t, http.MethodPost, fmt.Sprintf("/api/search/async/%s/1", model), tc.cond)
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("%s: expected 400, got %d; body: %s", tc.name, resp.StatusCode, readBody(t, resp))
		}
		commontest.ExpectErrorCode(t, resp, tc.code)
		resp.Body.Close()
	}
	if n := queryDB(t, "test-tenant", "SELECT count(*) FROM search_jobs WHERE model_name = $1", model); n != 0 {
		t.Errorf("refused conditions issued %d job(s)", n)
	}
}

func TestDeleteConditional_ArrayClauseOperator(t *testing.T) {
	const model = "e2e-array-op-delete"
	idA, idB := setupScoresModel(t, model)
	resp := doAuth(t, http.MethodDelete, fmt.Sprintf("/api/entity/%s/1", model),
		`{"type":"array","jsonPath":"$.scores[*]","operatorType":"GREATER_THAN","values":[null,90]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, readBody(t, resp))
	}
	resp.Body.Close()
	if r := doAuth(t, http.MethodGet, "/api/entity/"+idB, ""); r.StatusCode != http.StatusNotFound {
		t.Errorf("B (scores[1]=95) must be deleted, got %d", r.StatusCode)
	}
	if r := doAuth(t, http.MethodGet, "/api/entity/"+idA, ""); r.StatusCode != http.StatusOK {
		t.Errorf("A (scores[1]=40) must survive, got %d", r.StatusCode)
	}
}

func TestDeleteConditional_ArrayClauseRefusals_NothingDeleted(t *testing.T) {
	const model = "e2e-array-op-delete-refuse"
	idA, idB := setupScoresModel(t, model)
	for _, tc := range arrayClauseRefusals {
		resp := doAuth(t, http.MethodDelete, fmt.Sprintf("/api/entity/%s/1", model), tc.cond)
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("%s: expected 400, got %d; body: %s", tc.name, resp.StatusCode, readBody(t, resp))
		}
		commontest.ExpectErrorCode(t, resp, tc.code)
		resp.Body.Close()
	}
	for _, id := range []string{idA, idB} {
		if r := doAuth(t, http.MethodGet, "/api/entity/"+id, ""); r.StatusCode != http.StatusOK {
			t.Errorf("entity %s must survive refused deletes, got %d", id, r.StatusCode)
		}
	}
}

func TestGroupedStats_ArrayClauseCondition(t *testing.T) {
	const model = "e2e-array-op-stats"
	setupScoresModel(t, model)
	path := fmt.Sprintf("/api/entity/stats/%s/1/query", model)
	resp := doAuth(t, http.MethodPost, path, `{"groupBy":["$.name"],"condition":`+
		`{"type":"array","jsonPath":"$.scores[*]","operatorType":"GREATER_THAN","values":[50,null,70]}}`)
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, body)
	}
	buckets := decodeBuckets(t, body)
	if len(buckets) != 1 || findBucket(buckets, "$.name", "A") == nil {
		t.Errorf("want one bucket for A, got %s", body)
	}
	// A simple clause is refused on structure before types too.
	if r := doAuth(t, http.MethodPost, path, `{"groupBy":["$.name"],"condition":`+
		`{"type":"simple","jsonPath":"$.scores[0]","operatorType":"EQUALS","value":{"a":1}}}`); r.StatusCode != http.StatusBadRequest {
		t.Fatalf("simple object operand: expected 400, got %d; body: %s", r.StatusCode, readBody(t, r))
	} else {
		commontest.ExpectErrorCode(t, r, "INVALID_CONDITION")
		r.Body.Close()
	}
	for _, tc := range arrayClauseRefusals {
		r := doAuth(t, http.MethodPost, path, `{"groupBy":["$.name"],"condition":`+tc.cond+`}`)
		if r.StatusCode != http.StatusBadRequest {
			t.Fatalf("%s: expected 400, got %d; body: %s", tc.name, r.StatusCode, readBody(t, r))
		}
		commontest.ExpectErrorCode(t, r, tc.code)
		r.Body.Close()
	}
}

// A body that does not parse as a condition is 400 INVALID_CONDITION on both
// search doors; a missing or unknown type is such a body.
func TestSearch_UnparseableCondition_IsInvalidCondition(t *testing.T) {
	const model = "e2e-search-unparseable"
	setupScoresModel(t, model)
	for _, door := range []string{"direct", "async"} {
		for _, body := range []string{`{}`, `{"type":"bogus"}`, `not json`} {
			resp := doAuth(t, http.MethodPost, fmt.Sprintf("/api/search/%s/%s/1", door, model), body)
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("%s %s: expected 400, got %d; body: %s", door, body, resp.StatusCode, readBody(t, resp))
			}
			commontest.ExpectErrorCode(t, resp, "INVALID_CONDITION")
			resp.Body.Close()
		}
	}
}

// One malformed condition gets one code on every surface: the checks that need
// no model (structure, patterns) run before the checks that do (paths, types).
func TestConditionValidationOrder_SameCodeOnEverySurface(t *testing.T) {
	const model = "e2e-cond-order"
	setupScoresModel(t, model)
	for _, tc := range []struct{ name, cond string }{
		{"structural fault on an undeclared path", `{"type":"simple","jsonPath":"$.nope","value":1}`},
		{"bad pattern on a numeric field", `{"type":"simple","jsonPath":"$.scores[0]","operatorType":"MATCHES_PATTERN","value":"("}`},
		{"bad pattern on an undeclared path", `{"type":"simple","jsonPath":"$.nope","operatorType":"MATCHES_PATTERN","value":"("}`},
	} {
		for _, s := range []struct{ name, method, path, body string }{
			{"search direct", http.MethodPost, fmt.Sprintf("/api/search/direct/%s/1", model), tc.cond},
			{"search async", http.MethodPost, fmt.Sprintf("/api/search/async/%s/1", model), tc.cond},
			{"conditional delete", http.MethodDelete, fmt.Sprintf("/api/entity/%s/1", model), tc.cond},
			{"grouped stats", http.MethodPost, fmt.Sprintf("/api/entity/stats/%s/1/query", model),
				`{"groupBy":["$.name"],"condition":` + tc.cond + `}`},
		} {
			resp := doAuth(t, s.method, s.path, s.body)
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("%s / %s: expected 400, got %d; body: %s", tc.name, s.name, resp.StatusCode, readBody(t, resp))
			}
			commontest.ExpectErrorCode(t, resp, "INVALID_CONDITION")
			resp.Body.Close()
		}
	}
}

// A simple clause's BETWEEN bound is a scalar too. Unchecked, PostgreSQL
// answered 500 for the pushed-down form, and inside NOT the in-memory path
// compared against the bound's printed form, so a conditional delete removed
// the entities that comparison chose.
func TestConditionSurfaces_BetweenNonScalarBound_IsInvalidCondition(t *testing.T) {
	const model = "e2e-between-nonscalar-bound"
	idA, idB := setupScoresModel(t, model)
	for _, leaf := range []string{
		`{"type":"simple","jsonPath":"$.name","operatorType":"BETWEEN","value":[{"a":1},"z"]}`,
		`{"type":"simple","jsonPath":"$.name","operatorType":"BETWEEN_INCLUSIVE","value":[["a"],"z"]}`,
	} {
		for _, cond := range []string{leaf, `{"type":"group","operator":"NOT","conditions":[` + leaf + `]}`} {
			for _, s := range []struct{ name, method, path, body string }{
				{"search direct", http.MethodPost, fmt.Sprintf("/api/search/direct/%s/1", model), cond},
				{"search async", http.MethodPost, fmt.Sprintf("/api/search/async/%s/1", model), cond},
				{"conditional delete", http.MethodDelete, fmt.Sprintf("/api/entity/%s/1", model), cond},
				{"grouped stats", http.MethodPost, fmt.Sprintf("/api/entity/stats/%s/1/query", model),
					`{"groupBy":["$.name"],"condition":` + cond + `}`},
			} {
				resp := doAuth(t, s.method, s.path, s.body)
				if resp.StatusCode != http.StatusBadRequest {
					t.Errorf("%s %s: expected 400, got %d; body: %s", s.name, cond, resp.StatusCode, readBody(t, resp))
					continue
				}
				commontest.ExpectErrorCode(t, resp, "INVALID_CONDITION")
				resp.Body.Close()
			}
		}
	}
	for _, id := range []string{idA, idB} {
		if r := doAuth(t, http.MethodGet, "/api/entity/"+id, ""); r.StatusCode != http.StatusOK {
			t.Errorf("entity %s must survive refused deletes, got %d", id, r.StatusCode)
		}
	}
}

// An unknown model is 404 MODEL_NOT_FOUND before any condition check, on every
// endpoint that takes a condition.
func TestConditionEndpoints_UnknownModel_404BeforeConditionChecks(t *testing.T) {
	const model = "e2e-cond-unknown-model"
	for _, cond := range []string{
		`{"type":"simple","jsonPath":"$.x"}`,
		`{"type":"simple","jsonPath":"$.x","operatorType":"MATCHES_PATTERN","value":"("}`,
	} {
		for _, s := range []struct{ name, method, path, body string }{
			{"search direct", http.MethodPost, fmt.Sprintf("/api/search/direct/%s/1", model), cond},
			{"search async", http.MethodPost, fmt.Sprintf("/api/search/async/%s/1", model), cond},
			{"conditional delete", http.MethodDelete, fmt.Sprintf("/api/entity/%s/1", model), cond},
			{"grouped stats", http.MethodPost, fmt.Sprintf("/api/entity/stats/%s/1/query", model),
				`{"groupBy":["$.name"],"condition":` + cond + `}`},
		} {
			resp := doAuth(t, s.method, s.path, s.body)
			if resp.StatusCode != http.StatusNotFound {
				t.Errorf("%s %s: expected 404, got %d; body: %s", s.name, cond, resp.StatusCode, readBody(t, resp))
				continue
			}
			commontest.ExpectErrorCode(t, resp, "MODEL_NOT_FOUND")
			resp.Body.Close()
		}
	}
}
