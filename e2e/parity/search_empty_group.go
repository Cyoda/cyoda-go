package parity

import (
	"testing"

	"github.com/cyoda-platform/cyoda-go/e2e/parity/client"
)

// RunSearchEmptyGroupIdentities asserts the group-identity contract for
// explicit empty group conditions, uniformly across backends: an empty AND is
// the AND identity (true, matches everything) and an empty OR is the OR
// identity (false, matches nothing) — both standalone and nested inside a
// parent group. The SQL backends previously pushed a childless OR down as an
// empty WHERE fragment, turning "match nothing" into "match everything" (and,
// nested, into malformed SQL); the kernel is authoritative for these shapes.
func RunSearchEmptyGroupIdentities(t *testing.T, fixture BackendFixture) {
	tenant := fixture.NewTenant(t)
	c := client.NewClient(fixture.BaseURL(), tenant.Token)

	const modelName = "parity-search-empty-group"
	const modelVersion = 1
	setupSearchModel(t, c, modelName, modelVersion)

	for _, payload := range []string{
		`{"name":"Alice","amount":100,"status":"active"}`,
		`{"name":"Bob","amount":50,"status":"active"}`,
		`{"name":"Carol","amount":200,"status":"inactive"}`,
	} {
		if _, err := c.CreateEntity(t, modelName, modelVersion, payload); err != nil {
			t.Fatalf("CreateEntity %s: %v", payload, err)
		}
	}

	cases := []struct {
		name string
		cond string
		want int
	}{
		{
			name: "empty OR matches nothing",
			cond: `{"type":"group","operator":"OR","conditions":[]}`,
			want: 0,
		},
		{
			name: "empty AND matches everything",
			cond: `{"type":"group","operator":"AND","conditions":[]}`,
			want: 3,
		},
		{
			name: "empty OR nested in AND makes the conjunction false",
			cond: `{"type":"group","operator":"AND","conditions":[` +
				`{"type":"simple","jsonPath":"$.status","operatorType":"EQUALS","value":"active"},` +
				`{"type":"group","operator":"OR","conditions":[]}]}`,
			want: 0,
		},
		{
			name: "empty AND nested in OR makes the disjunction true",
			cond: `{"type":"group","operator":"OR","conditions":[` +
				`{"type":"simple","jsonPath":"$.status","operatorType":"EQUALS","value":"no-such-status"},` +
				`{"type":"group","operator":"AND","conditions":[]}]}`,
			want: 3,
		},
	}

	for _, tc := range cases {
		results, err := c.SyncSearch(t, modelName, modelVersion, tc.cond)
		if err != nil {
			t.Errorf("%s: SyncSearch: %v", tc.name, err)
			continue
		}
		if len(results) != tc.want {
			t.Errorf("%s: got %d results, want %d", tc.name, len(results), tc.want)
		}
	}
}
