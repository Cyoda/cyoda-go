package e2e_test

import (
	"net/http"
	"testing"
)

func TestModelDelete_Locked_409(t *testing.T) {
	const model = "e2e-del-locked"
	importModelE2E(t, model, 1)
	lockModelE2E(t, model, 1)

	resp := doAuth(t, http.MethodDelete, "/api/model/"+model+"/1", "")
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("delete locked model: expected 409, got %d: %s", resp.StatusCode, body)
	}
	assertErrorCode(t, body, "MODEL_ALREADY_LOCKED")
}
