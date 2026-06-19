package entity_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/app"

	_ "github.com/cyoda-platform/cyoda-go/plugins/memory"
)

// newTestAppAndServer creates an App and an httptest.Server for transition tests.
func newTestAppAndServer(t *testing.T) (*app.App, *httptest.Server) {
	t.Helper()
	cfg := app.DefaultConfig()
	cfg.ContextPath = ""
	a := app.New(cfg)
	srv := httptest.NewServer(a.Handler())
	t.Cleanup(srv.Close)
	return a, srv
}

// testCtx returns a context with the mock user context for direct store access.
func testCtx() context.Context {
	return spi.WithUserContext(context.Background(), &spi.UserContext{
		UserID:   "mock-user",
		UserName: "Mock User",
		Tenant: spi.Tenant{
			ID:   "mock-tenant",
			Name: "Mock Tenant",
		},
		Roles: []string{"ROLE_ADMIN"},
	})
}

// createEntityViaAPI imports a model, locks it, creates an entity, and returns the entity ID.
func createEntityViaAPI(t *testing.T, base, entityName string, version int, sampleData string) string {
	t.Helper()
	importAndLockModel(t, base, entityName, version, sampleData)

	resp := doCreateEntity(t, base, "JSON", entityName, version, sampleData)
	expectStatus(t, resp, http.StatusOK)
	body := readBody(t, resp)

	var createArr []map[string]any
	if err := json.Unmarshal(body, &createArr); err != nil {
		t.Fatalf("failed to parse create response: %v", err)
	}
	entityIDs := createArr[0]["entityIds"].([]any)
	return entityIDs[0].(string)
}

func TestGetTransitions_DefaultWorkflow(t *testing.T) {
	_, srv := newTestAppAndServer(t)

	entityID := createEntityViaAPI(t, srv.URL, "TransDefault", 1, `{"name":"test"}`)

	resp, err := http.Get(fmt.Sprintf("%s/entity/%s/transitions", srv.URL, entityID))
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	expectStatus(t, resp, http.StatusOK)

	body := readBody(t, resp)
	var transitions []string
	if err := json.Unmarshal(body, &transitions); err != nil {
		t.Fatalf("failed to parse transitions: %v", err)
	}

	// Default workflow: CREATED state has UPDATE and DELETE transitions.
	if len(transitions) != 2 {
		t.Fatalf("expected 2 transitions, got %d: %v", len(transitions), transitions)
	}

	found := map[string]bool{}
	for _, tr := range transitions {
		found[tr] = true
	}
	if !found["UPDATE"] || !found["DELETE"] {
		t.Fatalf("expected UPDATE and DELETE transitions, got %v", transitions)
	}
}

func TestGetTransitions_WithCustomWorkflow(t *testing.T) {
	a, srv := newTestAppAndServer(t)

	entityID := createEntityViaAPI(t, srv.URL, "TransCustom", 1, `{"status":"pending"}`)

	// Save a custom workflow with transitions from CREATED state.
	modelRef := spi.ModelRef{EntityName: "TransCustom", ModelVersion: "1"}
	ctx := testCtx()
	wfStore, err := a.StoreFactory().WorkflowStore(ctx)
	if err != nil {
		t.Fatalf("failed to get workflow store: %v", err)
	}
	err = wfStore.Save(ctx, modelRef, []spi.WorkflowDefinition{
		{
			Version:      "1.0",
			Name:         "custom-wf",
			InitialState: "NONE",
			Active:       true,
			States: map[string]spi.StateDefinition{
				"NONE": {
					Transitions: []spi.TransitionDefinition{
						{Name: "NEW", Next: "CREATED"},
					},
				},
				"CREATED": {
					Transitions: []spi.TransitionDefinition{
						{Name: "nda_signed", Next: "NDA_SIGNED", Manual: true},
						{Name: "nda_expired", Next: "NDA_EXPIRED", Manual: true},
						{Name: "decline_nda", Next: "DECLINED", Manual: true},
					},
				},
				"NDA_SIGNED":  {Transitions: []spi.TransitionDefinition{}},
				"NDA_EXPIRED": {Transitions: []spi.TransitionDefinition{}},
				"DECLINED":    {Transitions: []spi.TransitionDefinition{}},
			},
		},
	})
	if err != nil {
		t.Fatalf("failed to save workflow: %v", err)
	}

	resp, err := http.Get(fmt.Sprintf("%s/entity/%s/transitions", srv.URL, entityID))
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	expectStatus(t, resp, http.StatusOK)

	body := readBody(t, resp)
	var transitions []string
	if err := json.Unmarshal(body, &transitions); err != nil {
		t.Fatalf("failed to parse transitions: %v", err)
	}

	if len(transitions) != 3 {
		t.Fatalf("expected 3 transitions, got %d: %v", len(transitions), transitions)
	}

	found := map[string]bool{}
	for _, tr := range transitions {
		found[tr] = true
	}
	if !found["nda_signed"] || !found["nda_expired"] || !found["decline_nda"] {
		t.Fatalf("expected nda_signed, nda_expired, decline_nda; got %v", transitions)
	}
}

func TestGetTransitions_TerminalState(t *testing.T) {
	a, srv := newTestAppAndServer(t)

	entityID := createEntityViaAPI(t, srv.URL, "TransTerminal", 1, `{"done":true}`)

	// Save a custom workflow where CREATED transitions to DECLINED via a non-manual transition,
	// and DECLINED is terminal.
	modelRef := spi.ModelRef{EntityName: "TransTerminal", ModelVersion: "1"}
	ctx := testCtx()
	wfStore, err := a.StoreFactory().WorkflowStore(ctx)
	if err != nil {
		t.Fatalf("failed to get workflow store: %v", err)
	}
	err = wfStore.Save(ctx, modelRef, []spi.WorkflowDefinition{
		{
			Version:      "1.0",
			Name:         "terminal-wf",
			InitialState: "NONE",
			Active:       true,
			States: map[string]spi.StateDefinition{
				"NONE": {
					Transitions: []spi.TransitionDefinition{
						{Name: "NEW", Next: "CREATED"},
					},
				},
				"CREATED": {
					Transitions: []spi.TransitionDefinition{
						{Name: "DECLINE", Next: "DECLINED", Manual: true},
					},
				},
				"DECLINED": {
					Transitions: []spi.TransitionDefinition{},
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("failed to save workflow: %v", err)
	}

	// Manually update the entity state to DECLINED via the entity store.
	entityStore, err := a.StoreFactory().EntityStore(ctx)
	if err != nil {
		t.Fatalf("failed to get entity store: %v", err)
	}
	ent, err := entityStore.Get(ctx, entityID)
	if err != nil {
		t.Fatalf("failed to get entity: %v", err)
	}
	ent.Meta.State = "DECLINED"
	ent.Meta.LastModifiedDate = time.Now()
	if _, err := entityStore.Save(ctx, ent); err != nil {
		t.Fatalf("failed to save entity: %v", err)
	}

	resp, err := http.Get(fmt.Sprintf("%s/entity/%s/transitions", srv.URL, entityID))
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	expectStatus(t, resp, http.StatusOK)

	body := readBody(t, resp)
	var transitions []string
	if err := json.Unmarshal(body, &transitions); err != nil {
		t.Fatalf("failed to parse transitions: %v", err)
	}

	if len(transitions) != 0 {
		t.Fatalf("expected 0 transitions for terminal state, got %d: %v", len(transitions), transitions)
	}
}

func TestGetTransitions_EntityNotFound(t *testing.T) {
	_, srv := newTestAppAndServer(t)

	resp, err := http.Get(fmt.Sprintf("%s/entity/%s/transitions", srv.URL, "nonexistent-entity-id"))
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	expectStatus(t, resp, http.StatusNotFound)
}

func TestGetTransitions_BothParamsBadRequest(t *testing.T) {
	_, srv := newTestAppAndServer(t)

	url := fmt.Sprintf("%s/entity/some-id/transitions?pointInTime=%s&transactionId=some-tx",
		srv.URL, time.Now().UTC().Format(time.RFC3339))
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	expectStatus(t, resp, http.StatusBadRequest)

	body := readBody(t, resp)
	var pd map[string]any
	if err := json.Unmarshal(body, &pd); err != nil {
		t.Fatalf("failed to parse error response: %v", err)
	}
	detail, _ := pd["detail"].(string)
	if detail == "" {
		t.Fatal("expected non-empty detail in error response")
	}
	if !contains(detail, "mutually exclusive") {
		t.Fatalf("expected 'mutually exclusive' in detail, got: %s", detail)
	}
}

func TestFetchTransitions_PlatformLibrary(t *testing.T) {
	a, srv := newTestAppAndServer(t)

	entityID := createEntityViaAPI(t, srv.URL, "FetchModel", 1, `{"value":42}`)

	// Save a custom workflow for the model.
	modelRef := spi.ModelRef{EntityName: "FetchModel", ModelVersion: "1"}
	ctx := testCtx()
	wfStore, err := a.StoreFactory().WorkflowStore(ctx)
	if err != nil {
		t.Fatalf("failed to get workflow store: %v", err)
	}
	err = wfStore.Save(ctx, modelRef, []spi.WorkflowDefinition{
		{
			Version:      "1.0",
			Name:         "fetch-wf",
			InitialState: "NONE",
			Active:       true,
			States: map[string]spi.StateDefinition{
				"NONE": {
					Transitions: []spi.TransitionDefinition{
						{Name: "NEW", Next: "CREATED"},
					},
				},
				"CREATED": {
					Transitions: []spi.TransitionDefinition{
						{Name: "approve", Next: "APPROVED", Manual: true},
						{Name: "reject", Next: "REJECTED", Manual: true},
					},
				},
				"APPROVED": {Transitions: []spi.TransitionDefinition{}},
				"REJECTED": {Transitions: []spi.TransitionDefinition{}},
			},
		},
	})
	if err != nil {
		t.Fatalf("failed to save workflow: %v", err)
	}

	url := fmt.Sprintf("%s/platform-api/entity/fetch/transitions?entityClass=FetchModel.1&entityId=%s", srv.URL, entityID)
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	expectStatus(t, resp, http.StatusOK)

	body := readBody(t, resp)
	var transitions []string
	if err := json.Unmarshal(body, &transitions); err != nil {
		t.Fatalf("failed to parse transitions: %v", err)
	}

	if len(transitions) != 2 {
		t.Fatalf("expected 2 transitions, got %d: %v", len(transitions), transitions)
	}

	found := map[string]bool{}
	for _, tr := range transitions {
		found[tr] = true
	}
	if !found["approve"] || !found["reject"] {
		t.Fatalf("expected approve and reject, got %v", transitions)
	}
}

func TestFetchTransitions_MissingParams(t *testing.T) {
	_, srv := newTestAppAndServer(t)

	// Missing both params
	resp, err := http.Get(fmt.Sprintf("%s/platform-api/entity/fetch/transitions", srv.URL))
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	expectStatus(t, resp, http.StatusBadRequest)

	// Missing entityId
	resp, err = http.Get(fmt.Sprintf("%s/platform-api/entity/fetch/transitions?entityClass=Offer.1", srv.URL))
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	expectStatus(t, resp, http.StatusBadRequest)

	// Missing entityClass
	resp, err = http.Get(fmt.Sprintf("%s/platform-api/entity/fetch/transitions?entityId=some-id", srv.URL))
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	expectStatus(t, resp, http.StatusBadRequest)
}

func TestFetchTransitions_MalformedEntityClass(t *testing.T) {
	_, srv := newTestAppAndServer(t)

	// No dot
	resp, err := http.Get(fmt.Sprintf("%s/platform-api/entity/fetch/transitions?entityClass=Offer&entityId=some-id", srv.URL))
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	expectStatus(t, resp, http.StatusBadRequest)

	// Trailing dot
	resp, err = http.Get(fmt.Sprintf("%s/platform-api/entity/fetch/transitions?entityClass=Offer.&entityId=some-id", srv.URL))
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	expectStatus(t, resp, http.StatusBadRequest)
}

func TestGetTransitions_WithTransactionId(t *testing.T) {
	_, srv := newTestAppAndServer(t)

	importAndLockModel(t, srv.URL, "TransTxID", 1, `{"name":"test"}`)

	// Create an entity — the response includes the transactionId.
	resp := doCreateEntity(t, srv.URL, "JSON", "TransTxID", 1, `{"name":"test"}`)
	expectStatus(t, resp, http.StatusOK)
	body := readBody(t, resp)

	var createArr []map[string]any
	if err := json.Unmarshal(body, &createArr); err != nil {
		t.Fatalf("failed to parse create response: %v", err)
	}
	entityIDs := createArr[0]["entityIds"].([]any)
	entityID := entityIDs[0].(string)
	txID := createArr[0]["transactionId"].(string)
	if txID == "" {
		t.Fatal("expected non-empty transactionId in create response")
	}

	// Call GET /entity/{entityId}/transitions?transactionId=<txId>
	url := fmt.Sprintf("%s/entity/%s/transitions?transactionId=%s", srv.URL, entityID, txID)
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	expectStatus(t, resp, http.StatusOK)

	body = readBody(t, resp)
	var transitions []string
	if err := json.Unmarshal(body, &transitions); err != nil {
		t.Fatalf("failed to parse transitions: %v", err)
	}

	// Default workflow: CREATED state has UPDATE and DELETE transitions.
	if len(transitions) != 2 {
		t.Fatalf("expected 2 transitions, got %d: %v", len(transitions), transitions)
	}
	found := map[string]bool{}
	for _, tr := range transitions {
		found[tr] = true
	}
	if !found["UPDATE"] || !found["DELETE"] {
		t.Fatalf("expected UPDATE and DELETE transitions, got %v", transitions)
	}
}

func TestGetTransitions_InvalidPointInTime(t *testing.T) {
	_, srv := newTestAppAndServer(t)

	entityID := createEntityViaAPI(t, srv.URL, "TransInvalidPIT", 1, `{"name":"test"}`)

	url := fmt.Sprintf("%s/entity/%s/transitions?pointInTime=not-a-date", srv.URL, entityID)
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	expectStatus(t, resp, http.StatusBadRequest)

	body := readBody(t, resp)
	var pd map[string]any
	if err := json.Unmarshal(body, &pd); err != nil {
		t.Fatalf("failed to parse error response: %v", err)
	}
	detail, _ := pd["detail"].(string)
	if detail == "" {
		t.Fatal("expected non-empty detail in error response")
	}
	if !contains(detail, "invalid pointInTime") {
		t.Fatalf("expected 'invalid pointInTime' in detail, got: %s", detail)
	}
}

// contains checks if s contains substr.
func contains(s, substr string) bool {
	return len(s) >= len(substr) && searchSubstring(s, substr)
}

func searchSubstring(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
