package e2e_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

// TestAsyncDefault_FloorAhead: an async search submitted without a pointInTime
// takes its instant from the store's consistency time, so it sees every
// committed entity even when the commit stamps run ahead of this process's
// clock (the stamp floor an hour ahead). Defaulting to the process clock would
// select as-at a moment before the stamps and find nothing.
func TestAsyncDefault_FloorAhead(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: requires Docker + PostgreSQL")
	}
	s := newSchedDB(t)
	h := newStackOn(t, s, nil)
	_ = h.token(t)

	floor := time.Now().Add(time.Hour).UnixMicro()
	if _, err := s.pool.Exec(context.Background(), fmt.Sprintf("SELECT setval('cyoda_stamp_floor', %d, true)", floor)); err != nil {
		t.Fatalf("raise the stamp floor: %v", err)
	}

	const model = "e2e-async-floor-ahead"
	h.SetupModelWithWorkflow(t, model, secondaryWorkflow)
	for i := 0; i < 2; i++ {
		if _, status, body := h.CreateEntity(t, model, 1, fmt.Sprintf(`{"name":"e%d","amount":%d,"status":"new"}`, i, i)); status != http.StatusOK {
			t.Fatalf("create: %d %s", status, body)
		}
	}

	resp := h.DoAuth(t, http.MethodPost, fmt.Sprintf("/api/search/async/%s/1", model),
		`{"type":"lifecycle","field":"state","operatorType":"EQUALS","value":"STORED"}`, "")
	submitBody := h.readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("submit: %d %s", resp.StatusCode, submitBody)
	}
	jobID := strings.Trim(strings.TrimSpace(submitBody), `"`)

	var status string
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		r := h.DoAuth(t, http.MethodGet, "/api/search/async/"+jobID+"/status", "", "")
		var st map[string]any
		if err := json.Unmarshal([]byte(h.readBody(t, r)), &st); err != nil {
			t.Fatalf("status decode: %v", err)
		}
		status, _ = st["searchJobStatus"].(string)
		if status != "RUNNING" {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if status != "SUCCESSFUL" {
		t.Fatalf("job status = %q, want SUCCESSFUL", status)
	}
	r := h.DoAuth(t, http.MethodGet, "/api/search/async/"+jobID, "", "")
	body := h.readBody(t, r)
	var page struct {
		Content []any `json:"content"`
	}
	if err := json.Unmarshal([]byte(body), &page); err != nil {
		t.Fatalf("results decode: %v: %s", err, body)
	}
	if len(page.Content) != 2 {
		t.Fatalf("async search with no pointInTime found %d entities, want 2: %s", len(page.Content), body)
	}
}
