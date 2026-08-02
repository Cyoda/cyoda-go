package e2e_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"
)

// updateEntityE2E updates an entity via the REST API.
func updateEntityE2E(t *testing.T, entityID, transition, payload string) {
	t.Helper()
	path := fmt.Sprintf("/api/entity/JSON/%s/%s", entityID, transition)
	resp := doAuth(t, http.MethodPut, path, payload)
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("updateEntity %s/%s: expected 200, got %d: %s", entityID, transition, resp.StatusCode, body)
	}
}

// latestChangeTimeE2E returns the server-stamped time of the entity's most
// recent version, read back from GET /api/entity/{id}/changes.
//
// Point-in-time boundaries must be built from this, never from time.Now():
// version times are stamped by the backend, and on postgres they come from the
// database — a different clock than the test process. Under load the Docker
// clock has been measured lagging the host by more than 10 ms, which silently
// resolves an as-at read to the wrong version. See e2e/parity/pit_time.go.
func latestChangeTimeE2E(t *testing.T, entityID string) time.Time {
	t.Helper()
	resp := doAuth(t, http.MethodGet, fmt.Sprintf("/api/entity/%s/changes", entityID), "")
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("getEntityChanges %s: expected 200, got %d: %s", entityID, resp.StatusCode, body)
	}
	var entries []struct {
		TimeOfChange time.Time `json:"timeOfChange"`
	}
	if err := json.Unmarshal([]byte(body), &entries); err != nil {
		t.Fatalf("failed to parse changes response: %v: %s", err, body)
	}
	if len(entries) == 0 {
		t.Fatalf("getEntityChanges %s returned no entries", entityID)
	}
	latest := entries[0].TimeOfChange
	for _, e := range entries[1:] {
		if e.TimeOfChange.After(latest) {
			latest = e.TimeOfChange
		}
	}
	return latest
}

// midpointBetweenE2E returns an instant strictly between two server-stamped
// version times, formatted for the pointInTime query parameter.
//
// Deliberately a local reimplementation of parity.MidpointBetween rather than a
// call to it: importing e2e/parity here would pull in the scenario registry and
// its init()s for the sake of five lines.
func midpointBetweenE2E(t *testing.T, earlier, later time.Time) string {
	t.Helper()
	if !earlier.Before(later) {
		t.Fatalf("version timestamps not strictly increasing: earlier=%s later=%s",
			earlier.Format(time.RFC3339Nano), later.Format(time.RFC3339Nano))
	}
	return earlier.Add(later.Sub(earlier) / 2).UTC().Format(time.RFC3339Nano)
}

// getEntityData retrieves an entity and returns the parsed data map.
// If pointInTime is non-empty, it's appended as a query parameter.
func getEntityData(t *testing.T, entityID, pointInTime string) map[string]any {
	t.Helper()
	path := fmt.Sprintf("/api/entity/%s", entityID)
	if pointInTime != "" {
		path += "?pointInTime=" + pointInTime
	}
	resp := doAuth(t, http.MethodGet, path, "")
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("getEntity %s (pit=%s): expected 200, got %d: %s", entityID, pointInTime, resp.StatusCode, body)
	}

	var envelope map[string]any
	if err := json.Unmarshal([]byte(body), &envelope); err != nil {
		t.Fatalf("failed to parse entity response: %v", err)
	}

	// data can be a map or a JSON string — handle both.
	switch d := envelope["data"].(type) {
	case map[string]any:
		return d
	case string:
		var data map[string]any
		if err := json.Unmarshal([]byte(d), &data); err != nil {
			t.Fatalf("failed to parse entity data string: %v", err)
		}
		return data
	default:
		t.Fatalf("unexpected data type: %T", envelope["data"])
		return nil
	}
}

// getEntityAtTransactionID issues GET /api/entity/{id}?transactionId=<tx>
// and returns the (status, body) pair. Used by tests that need to assert
// both positive (200 + at-tx snapshot) and negative (404 + ENTITY_NOT_FOUND)
// outcomes for the transactionId-scoped GET path. Issue #150.
func getEntityAtTransactionID(t *testing.T, entityID, txID string) (int, string) {
	t.Helper()
	path := fmt.Sprintf("/api/entity/%s?transactionId=%s", entityID, txID)
	resp := doAuth(t, http.MethodGet, path, "")
	return resp.StatusCode, readBody(t, resp)
}
