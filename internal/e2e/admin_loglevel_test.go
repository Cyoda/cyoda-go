package e2e_test

// End-to-end coverage for the runtime log-level admin endpoint. The handler's
// branches are unit-tested in internal/api/admin_test.go; what these tests add
// is the wired route behind real JWT auth, and that a POST is observable by a
// subsequent GET (the level is process-global state, not per-request).

import (
	"encoding/json"
	"net/http"
	"testing"
)

func getLogLevelE2E(t *testing.T) string {
	t.Helper()
	resp := doAuthAgainst(t, serverURL, platformToken(t), http.MethodGet, "/api/admin/log-level", "")
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET log-level: status=%d body=%s", resp.StatusCode, body)
	}
	var cfg map[string]any
	if err := json.Unmarshal([]byte(body), &cfg); err != nil {
		t.Fatalf("GET log-level: decode %q: %v", body, err)
	}
	level, _ := cfg["level"].(string)
	if level == "" {
		t.Fatalf("GET log-level: no level in response: %s", body)
	}
	return level
}

// TestAdminLogLevel_SetRoundTrip asserts POST changes the level, reports the
// previous one, and that the change is visible to a subsequent GET.
func TestAdminLogLevel_SetRoundTrip(t *testing.T) {
	original := getLogLevelE2E(t)
	// Target a level that differs from the current one, so "previous" and the
	// post-POST GET both prove something even if the suite is already running
	// at debug.
	target := "debug"
	if original == target {
		target = "warn"
	}
	// Restore the level so this test cannot leak verbosity into the rest of
	// the suite (the level is process-global).
	defer func() {
		resp := doAuthAgainst(t, serverURL, platformToken(t), http.MethodPost, "/api/admin/log-level", `{"level":"`+original+`"}`)
		readBody(t, resp)
	}()

	resp := doAuthAgainst(t, serverURL, platformToken(t), http.MethodPost, "/api/admin/log-level", `{"level":"`+target+`"}`)
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST log-level: status=%d body=%s", resp.StatusCode, body)
	}

	var got struct {
		Level    string `json:"level"`
		Previous string `json:"previous"`
	}
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("POST log-level: decode %q: %v", body, err)
	}
	if got.Level != target {
		t.Errorf("POST log-level: level=%q, want %q", got.Level, target)
	}
	if got.Previous != original {
		t.Errorf("POST log-level: previous=%q, want %q", got.Previous, original)
	}

	if now := getLogLevelE2E(t); now != target {
		t.Errorf("GET after POST: level=%q, want %q — the change did not take effect process-wide", now, target)
	}
}

// TestAdminLogLevel_BadRequests covers the documented 400 paths.
func TestAdminLogLevel_BadRequests(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"malformed-json", `{not json`},
		{"empty-body", ``},
		{"missing-level", `{}`},
		{"empty-level", `{"level":""}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := doAuthAgainst(t, serverURL, platformToken(t), http.MethodPost, "/api/admin/log-level", tc.body)
			body := readBody(t, resp)
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("status=%d, want 400; body: %s", resp.StatusCode, body)
			}
			assertErrorCode(t, body, "BAD_REQUEST")
		})
	}
}

// TestAdminLogLevel_UnknownLevel asserts the wired route answers 400 for a
// level outside the accepted set, and that a subsequent GET shows the level
// was left unchanged rather than silently set to info.
func TestAdminLogLevel_UnknownLevel(t *testing.T) {
	original := getLogLevelE2E(t)

	resp := doAuthAgainst(t, serverURL, platformToken(t), http.MethodPost, "/api/admin/log-level", `{"level":"verbose"}`)
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status=%d, want 400; body: %s", resp.StatusCode, body)
	}
	assertErrorCode(t, body, "BAD_REQUEST")

	if now := getLogLevelE2E(t); now != original {
		t.Errorf("GET after a refused unknown level: level=%q, want unchanged %q", now, original)
	}
}
