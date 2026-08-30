package search_test

import (
	"net/http"
	"strings"
	"testing"
)

// Regression test for issue #90.
//
// Previously, unknown operator strings silently fell through to a regex
// match path (via mapOperator's default branch) instead of being rejected.
// AI agents and developers could not tell their search was broken —
// queries with typo'd operators produced zero or wrong results with no
// diagnostic. All 26 canonical operators must be accepted; everything
// else must yield HTTP 400 INVALID_CONDITION.
//
// The error code was originally BAD_REQUEST here (and INVALID_CONDITION on
// grouped stats for the identical input — two codes for one error class).
// operator-semantics.md §4 settles it: "An operator name outside this set
// is 400 INVALID_CONDITION, on every surface that carries a condition."

func TestSyncSearch_UnknownOperator_Returns400(t *testing.T) {
	srv := newTestServer(t)
	importAndLockModel(t, srv.URL, "person", 1, `{"name":"Alice","age":30}`)

	body := `{"type":"simple","jsonPath":"$.name","operatorType":"NOT_EQUALS","value":"Alice"}`
	resp, err := http.Post(srv.URL+"/search/direct/person/1", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("post sync search: %v", err)
	}
	defer resp.Body.Close()
	respBody := readBody(t, resp)

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body: %s", resp.StatusCode, respBody)
	}
	bs := string(respBody)
	if !strings.Contains(bs, "INVALID_CONDITION") {
		t.Errorf("body missing INVALID_CONDITION code: %s", bs)
	}
	// The valid operator list should be surfaced so callers can self-correct.
	if !strings.Contains(bs, "EQUALS") {
		t.Errorf("body missing valid-operator list hint (e.g. EQUALS): %s", bs)
	}
}

func TestSyncSearch_AllCanonicalOperators_Accepted(t *testing.T) {
	srv := newTestServer(t)
	importAndLockModel(t, srv.URL, "person", 1, `{"name":"Alice","age":30,"tags":["a"]}`)

	// The 22 canonical operators per cmd/cyoda/help/content/search.md. We
	// only care that each is ACCEPTED (no 400); match semantics are covered
	// elsewhere. IS_NULL/NOT_NULL take no value; BETWEEN takes a 2-array.
	cases := []struct {
		op      string
		valueJS string
	}{
		{"EQUALS", `"Alice"`},
		{"NOT_EQUAL", `"Alice"`},
		{"GREATER_THAN", `25`},
		{"LESS_THAN", `40`},
		{"GREATER_OR_EQUAL", `25`},
		{"LESS_OR_EQUAL", `40`},
		{"CONTAINS", `"Ali"`},
		{"NOT_CONTAINS", `"xyz"`},
		{"STARTS_WITH", `"Ali"`},
		{"NOT_STARTS_WITH", `"xyz"`},
		{"ENDS_WITH", `"ce"`},
		{"NOT_ENDS_WITH", `"xyz"`},
		{"LIKE", `"Ali%"`},
		{"IS_NULL", `null`},
		{"NOT_NULL", `null`},
		{"BETWEEN", `[25, 40]`},
		{"BETWEEN_INCLUSIVE", `[25, 40]`},
		{"MATCHES_PATTERN", `"^Al.*"`},
		{"IEQUALS", `"alice"`},
		{"INOT_EQUAL", `"bob"`},
		{"ICONTAINS", `"ali"`},
		{"INOT_CONTAINS", `"xyz"`},
		{"ISTARTS_WITH", `"ali"`},
		{"INOT_STARTS_WITH", `"xyz"`},
		{"IENDS_WITH", `"CE"`},
		{"INOT_ENDS_WITH", `"xyz"`},
	}

	for _, tc := range cases {
		t.Run(tc.op, func(t *testing.T) {
			body := `{"type":"simple","jsonPath":"$.name","operatorType":"` + tc.op + `","value":` + tc.valueJS + `}`
			resp, err := http.Post(srv.URL+"/search/direct/person/1", "application/json", strings.NewReader(body))
			if err != nil {
				t.Fatalf("post sync search: %v", err)
			}
			defer resp.Body.Close()
			if resp.StatusCode == http.StatusBadRequest {
				t.Errorf("operator %s rejected as 400: %s", tc.op, readBody(t, resp))
			}
		})
	}
}
