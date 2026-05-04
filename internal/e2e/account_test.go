package e2e_test

import (
	"net/http"
	"testing"
)

func TestAccount_Get(t *testing.T) {
	token := getToken(t, "test-client", "test-secret")
	req, err := e2eNewRequest(t, "GET", serverURL+"/api/account", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /account: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got status=%d", resp.StatusCode)
	}
	// The validator middleware checks the response body against the spec's
	// UserAccountInfoResponseDto schema automatically.
}
