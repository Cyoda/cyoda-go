package e2e_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

// consistencyTimeOf asks the platform for the consistency time and returns the
// string exactly as the server wrote it.
func consistencyTimeOf(t *testing.T) string {
	t.Helper()
	resp := doAuth(t, http.MethodGet, "/api/entity/consistency-time", "")
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /entity/consistency-time: %d: %s", resp.StatusCode, body)
	}
	var dto struct {
		ConsistencyTime string `json:"consistencyTime"`
	}
	if err := json.Unmarshal([]byte(body), &dto); err != nil {
		t.Fatalf("decode: %v: %s", err, body)
	}
	if dto.ConsistencyTime == "" {
		t.Fatalf("consistencyTime missing: %s", body)
	}
	return dto.ConsistencyTime
}

// TestConsistencyTimeEndpoint_ReturnsTime: 200 with a parseable RFC 3339 time
// that is not in the future.
func TestConsistencyTimeEndpoint_ReturnsTime(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: requires Docker + PostgreSQL")
	}
	c := consistencyTimeOf(t)
	got, err := time.Parse(time.RFC3339Nano, c)
	if err != nil {
		t.Fatalf("consistencyTime %q is not RFC 3339: %v", c, err)
	}
	if got.After(time.Now().Add(time.Minute)) {
		t.Errorf("consistencyTime %s is in the future", c)
	}
}

// TestConsistencyTimeEndpoint_RequiresAuthentication: no token, 401.
func TestConsistencyTimeEndpoint_RequiresAuthentication(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: requires Docker + PostgreSQL")
	}
	resp, err := http.Get(serverURL + "/api/entity/consistency-time")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	if body := readBody(t, resp); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("without a token: %d %s, want 401", resp.StatusCode, body)
	}
}

// TestConsistencyTimeEndpoint_RequiresM2M: a caller without ROLE_M2M, 403.
func TestConsistencyTimeEndpoint_RequiresM2M(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: requires Docker + PostgreSQL")
	}
	resp := doAuthAgainst(t, serverURL, noM2MSuiteToken(t), http.MethodGet, "/api/entity/consistency-time", "")
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusForbidden || !strings.Contains(body, "this operation requires ROLE_M2M") {
		t.Errorf("without ROLE_M2M: %d %s, want 403 naming ROLE_M2M", resp.StatusCode, body)
	}
}

// TestConsistencyTimeEndpoint_StringIsAcceptedVerbatim: the string the endpoint
// returns, passed unchanged as pointInTime, is never refused and shows an
// entity saved before it was taken.
func TestConsistencyTimeEndpoint_StringIsAcceptedVerbatim(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: requires Docker + PostgreSQL")
	}
	const model = "e2e-consistency-time"
	setupStatsModel(t, model)
	id := createEntityE2E(t, model, 1, `{"variantId":"v1","price":1.0}`)

	c := consistencyTimeOf(t)
	resp := doAuth(t, http.MethodGet, fmt.Sprintf("/api/entity/%s?pointInTime=%s", id, c), "")
	if body := readBody(t, resp); resp.StatusCode != http.StatusOK {
		t.Fatalf("GET entity at the consistency time %q: %d: %s, want 200", c, resp.StatusCode, body)
	}
}
