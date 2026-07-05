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

func TestModelLock_AlreadyLocked_409(t *testing.T) {
	const m = "e2e-lock-twice"
	importModelE2E(t, m, 1)
	lockModelE2E(t, m, 1)
	resp := doAuth(t, http.MethodPut, "/api/model/"+m+"/1/lock", "")
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("relock: expected 409, got %d: %s", resp.StatusCode, body)
	}
	assertErrorCode(t, body, "MODEL_ALREADY_LOCKED")
}

func TestModelUnlock_AlreadyUnlocked_409(t *testing.T) {
	const m = "e2e-unlock-unlocked"
	importModelE2E(t, m, 1) // stays UNLOCKED
	resp := doAuth(t, http.MethodPut, "/api/model/"+m+"/1/unlock", "")
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("unlock unlocked: expected 409, got %d: %s", resp.StatusCode, body)
	}
	assertErrorCode(t, body, "MODEL_ALREADY_UNLOCKED")
}

func TestModelUnlock_HasEntities_409(t *testing.T) {
	const m = "e2e-unlock-entities"
	importModelE2E(t, m, 1)
	lockModelE2E(t, m, 1)
	createEntityE2E(t, m, 1, `{"name":"x"}`)
	resp := doAuth(t, http.MethodPut, "/api/model/"+m+"/1/unlock", "")
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("unlock with entities: expected 409, got %d: %s", resp.StatusCode, body)
	}
	assertErrorCode(t, body, "MODEL_HAS_ENTITIES")
}

func TestModelImport_LockedModel_409(t *testing.T) {
	const m = "e2e-import-locked"
	importModelE2E(t, m, 1)
	lockModelE2E(t, m, 1)
	resp := doAuth(t, http.MethodPost,
		"/api/model/import/JSON/SAMPLE_DATA/"+m+"/1", `{"name":"x"}`)
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("re-import locked: expected 409, got %d: %s", resp.StatusCode, body)
	}
	assertErrorCode(t, body, "MODEL_ALREADY_LOCKED")
}

func TestModelImport_UnsupportedConverter_400(t *testing.T) {
	const m = "e2e-import-conv"
	resp := doAuth(t, http.MethodPost,
		"/api/model/import/JSON/JSON_SCHEMA/"+m+"/1", `{"name":"x"}`)
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("import JSON_SCHEMA converter: expected 400, got %d: %s", resp.StatusCode, body)
	}
	assertErrorCode(t, body, "BAD_REQUEST")
}
