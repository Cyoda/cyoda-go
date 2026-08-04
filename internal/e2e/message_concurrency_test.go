package e2e_test

// Isolated single-backend concurrency coverage for the messaging domain.
// Messages have no per-subject serialisation point, so the invariant is that
// concurrent writers all commit and every message is independently readable —
// no lost write, no id collision, no torn payload.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"testing"
)

// TestMessage_ConcurrentCreates_AllCommitAndAreReadable fires N concurrent
// NewMessage calls on the same subject and asserts every one lands with a
// distinct id and its own payload intact.
func TestMessage_ConcurrentCreates_AllCommitAndAreReadable(t *testing.T) {
	const subject = "e2e-msg-concurrent"
	const n = 12

	ctx := e2eCtx(t)
	results := make([]httpResult, n)
	ready := make(chan struct{})

	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			body := fmt.Sprintf(`{"payload": {"seq": %d}, "metaData": {"source": "e2e"}}`, idx)
			<-ready
			results[idx] = resultOf(doAuthRaw(ctx, http.MethodPost, "/api/message/new/"+subject, body))
		}(i)
	}
	close(ready)
	wg.Wait()

	// Assert on the test goroutine — Fatal is only legal here.
	ids := make(map[string]int, n)
	for idx, res := range results {
		if res.err != nil {
			t.Fatalf("concurrent message %d: %v", idx, res.err)
		}
		if res.status != http.StatusOK {
			t.Fatalf("concurrent message %d: status=%d, want 200; body: %s", idx, res.status, res.body)
		}
		id := messageIDFromCreateBody(t, res.body)
		if prev, dup := ids[id]; dup {
			t.Fatalf("message id %s returned for both writer %d and writer %d — id collision", id, prev, idx)
		}
		ids[id] = idx
	}
	if len(ids) != n {
		t.Fatalf("got %d distinct message ids, want %d", len(ids), n)
	}

	// Every message must be readable and carry its own writer's payload.
	for id, writer := range ids {
		resp := doAuth(t, http.MethodGet, "/api/message/"+id, "")
		body := readBody(t, resp)
		if resp.StatusCode != http.StatusOK {
			t.Errorf("GET message %s (writer %d): status=%d; body: %s", id, writer, resp.StatusCode, body)
			continue
		}
		var msg struct {
			Content struct {
				Seq *float64 `json:"seq"`
			} `json:"content"`
		}
		if err := json.Unmarshal([]byte(body), &msg); err != nil {
			t.Errorf("GET message %s: decode %q: %v", id, body, err)
			continue
		}
		if msg.Content.Seq == nil {
			t.Errorf("GET message %s (writer %d): content.seq missing; body: %s", id, writer, body)
			continue
		}
		if int(*msg.Content.Seq) != writer {
			t.Errorf("GET message %s: content.seq=%d, want %d — payloads were crossed between concurrent writers",
				id, int(*msg.Content.Seq), writer)
		}
	}
}

// messageIDFromCreateBody extracts the single created message id from a
// NewMessage response body.
func messageIDFromCreateBody(t *testing.T, body string) string {
	t.Helper()
	var results []map[string]any
	if err := json.Unmarshal([]byte(body), &results); err != nil {
		t.Fatalf("parse create response %q: %v", body, err)
	}
	if len(results) == 0 {
		t.Fatalf("create response has no results: %s", body)
	}
	entityIDs, ok := results[0]["entityIds"].([]any)
	if !ok || len(entityIDs) == 0 {
		t.Fatalf("create response has no entityIds: %s", body)
	}
	id, _ := entityIDs[0].(string)
	if id == "" {
		t.Fatalf("create response has blank entityId: %s", body)
	}
	return id
}
