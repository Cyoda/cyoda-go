package client_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/cyoda/cyoda-go/e2e/parity/client"
)

func TestCreateEntitiesCollection_POSTsHeterogeneousBody(t *testing.T) {
	var gotMethod, gotPath, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		body, _ := io.ReadAll(r.Body)
		gotBody = string(body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`[{"transactionId":"tx1","entityIds":["` +
			`00000000-0000-0000-0000-000000000001","` +
			`00000000-0000-0000-0000-000000000002"]}]`))
	}))
	defer srv.Close()

	c := client.NewClient(srv.URL, "fake-token")
	items := []client.CollectionItem{
		{ModelName: "family", ModelVersion: 1, Payload: `{"a":1}`},
		{ModelName: "pets", ModelVersion: 1, Payload: `{"b":"x"}`},
	}
	ids, err := c.CreateEntitiesCollection(t, items)
	if err != nil {
		t.Fatalf("CreateEntitiesCollection: %v", err)
	}
	if gotMethod != http.MethodPost {
		t.Errorf("method: got %q, want POST", gotMethod)
	}
	if gotPath != "/api/entity/JSON" {
		t.Errorf("path: got %q, want /api/entity/JSON", gotPath)
	}
	var raw []map[string]any
	if err := json.Unmarshal([]byte(gotBody), &raw); err != nil {
		t.Fatalf("body not a JSON array: %v (body=%s)", err, gotBody)
	}
	if len(raw) != 2 {
		t.Fatalf("body items: got %d, want 2", len(raw))
	}
	if m, ok := raw[0]["model"].(map[string]any); !ok || m["name"] != "family" {
		t.Errorf("items[0].model.name: got %v, want family", raw[0]["model"])
	}
	if raw[0]["payload"] != `{"a":1}` {
		t.Errorf("items[0].payload: got %v, want {\"a\":1}", raw[0]["payload"])
	}
	if len(ids) != 2 {
		t.Errorf("returned ids: got %d, want 2", len(ids))
	}
}
