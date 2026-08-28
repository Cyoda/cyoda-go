package e2e_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// importModelRawE2E posts sample data and returns the raw status and body, so a
// test can assert a rejection instead of asserting success.
func importModelRawE2E(t *testing.T, entityName string, modelVersion int, sample string) (int, string) {
	t.Helper()
	path := fmt.Sprintf("/api/model/import/JSON/SAMPLE_DATA/%s/%d", entityName, modelVersion)
	resp := doAuth(t, http.MethodPost, path, sample)
	return resp.StatusCode, readBody(t, resp)
}

// createEntityRawE2E posts an entity payload and returns the raw status and
// body.
func createEntityRawE2E(t *testing.T, entityName string, modelVersion int, payload string) (int, string) {
	t.Helper()
	path := fmt.Sprintf("/api/entity/JSON/%s/%d", entityName, modelVersion)
	resp := doAuth(t, http.MethodPost, path, payload)
	return resp.StatusCode, readBody(t, resp)
}

// rootBucketE2E returns the "$" descriptor bucket of a SIMPLE_VIEW export.
func rootBucketE2E(t *testing.T, entityName string, modelVersion int) map[string]any {
	t.Helper()
	exported := exportModelE2E(t, entityName, modelVersion)
	model, ok := exported["model"].(map[string]any)
	if !ok {
		t.Fatalf("export has no model: %v", exported)
	}
	root, ok := model["$"].(map[string]any)
	if !ok {
		t.Fatalf("export has no $ bucket: %v", model)
	}
	return root
}

// TestModelKindEnforcement_WriteRejectsKindMismatch drives the strict
// validation door — an entity write against a locked model with no changeLevel
// — through the full HTTP stack. A field declared STRING has a one-element kind
// set, and an array or an object is outside it however its contents are typed.
func TestModelKindEnforcement_WriteRejectsKindMismatch(t *testing.T) {
	const model = "e2e-kind-enforce"
	importModelSampleE2E(t, model, 1, `{"s":"x","n":1,"a":["x"],"o":{"k":"v"}}`)
	lockModelE2E(t, model, 1)

	before := exportModelE2E(t, model, 1)

	rejected := []struct {
		name, payload, wantMsg string
	}{
		{"array into a STRING field", `{"s":["A"]}`, "expected scalar, got array"},
		{"object into a STRING field", `{"s":{"k":"v"}}`, "expected scalar, got object"},
		{"object element in an array of STRING", `{"a":[{"k":"v"}]}`, "expected scalar, got object"},
		{"array element in an array of STRING", `{"a":[["A"]]}`, "expected scalar, got array"},
		{"scalar into an array field", `{"a":"x"}`, "expected array, got string"},
		{"array into an object field", `{"o":["x"]}`, "expected object, got array"},
	}
	for _, tc := range rejected {
		t.Run(tc.name, func(t *testing.T) {
			status, body := createEntityRawE2E(t, model, 1, tc.payload)
			if status != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body: %s", status, body)
			}
			if !strings.Contains(body, tc.wantMsg) {
				t.Errorf("body must explain the kind mismatch (%q); body: %s", tc.wantMsg, body)
			}
		})
	}

	// A scalar type mismatch keeps its own dictionary-aligned code.
	t.Run("number into a STRING field", func(t *testing.T) {
		status, body := createEntityRawE2E(t, model, 1, `{"s":1}`)
		if status != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400; body: %s", status, body)
		}
		if !strings.Contains(body, "INCOMPATIBLE_TYPE") {
			t.Errorf("body must carry INCOMPATIBLE_TYPE; body: %s", body)
		}
	})

	// Happy path: values of the declared kinds are still stored.
	t.Run("declared kinds accepted", func(t *testing.T) {
		status, body := createEntityRawE2E(t, model, 1, `{"s":"ok","n":2,"a":["y"],"o":{"k":"v"}}`)
		if status != http.StatusOK {
			t.Fatalf("status = %d, want 200; body: %s", status, body)
		}
	})

	// Fail closed: nothing above widened the model.
	after := exportModelE2E(t, model, 1)
	beforeJSON, _ := json.Marshal(before)
	afterJSON, _ := json.Marshal(after)
	if string(beforeJSON) != string(afterJSON) {
		t.Errorf("model changed under rejected writes\n  before: %s\n  after:  %s", beforeJSON, afterJSON)
	}
}

// A PATCH is validated strictly too — it must never widen the model — so it
// answers a kind mismatch the same way.
func TestModelKindEnforcement_PatchRejectsKindMismatch(t *testing.T) {
	const model = "e2e-kind-enforce-patch"
	importModelSampleE2E(t, model, 1, `{"s":"x"}`)
	lockModelE2E(t, model, 1)

	entityID, createTxID := createEntityE2EWithTxID(t, model, 1, `{"s":"ok"}`)

	resp := patchEntity(t,
		fmt.Sprintf("/api/entity/JSON/%s", entityID),
		"application/merge-patch+json",
		createTxID,
		`{"s":["A"]}`,
	)
	patchBody := readBody(t, resp)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("patch status = %d, want 400; body: %s", resp.StatusCode, patchBody)
	}
	if !strings.Contains(patchBody, "expected scalar, got array") {
		t.Errorf("patch body must explain the kind mismatch; body: %s", patchBody)
	}
}

// TestModelImport_ArrayBodyIsADocumentCollection drives the sample-data import
// door: an array body registers the merge of the documents, and the registered
// model admits each of them.
func TestModelImport_ArrayBodyIsADocumentCollection(t *testing.T) {
	const model = "e2e-import-collection"
	importModelSampleE2E(t, model, 1,
		`[{"name":"A","tags":["A","B"]},{"name":"B","sku":1}]`)
	lockModelE2E(t, model, 1)

	root := rootBucketE2E(t, model, 1)
	for key, want := range map[string]string{
		".name":    "STRING",
		".sku":     "INTEGER",
		".tags[*]": "STRING",
	} {
		if got := root[key]; got != want {
			t.Errorf("%s = %v, want %v; bucket: %v", key, got, want, root)
		}
	}

	for _, payload := range []string{`{"name":"A","tags":["A","B"]}`, `{"name":"B","sku":1}`} {
		status, body := createEntityRawE2E(t, model, 1, payload)
		if status != http.StatusOK {
			t.Errorf("write %s: status = %d, want 200; body: %s", payload, status, body)
		}
	}
}

// A body that is neither a document nor a collection of documents is refused,
// and leaves no model behind.
func TestModelImport_NonDocumentBodyRejected(t *testing.T) {
	cases := []struct{ name, model, sample string }{
		{"scalar body", "e2e-import-scalar", `"x"`},
		{"array of scalars", "e2e-import-scalar-array", `["A","B"]`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, body := importModelRawE2E(t, tc.model, 1, tc.sample)
			if status != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body: %s", status, body)
			}
			if !strings.Contains(body, "VALIDATION_FAILED") {
				t.Errorf("body must carry VALIDATION_FAILED; body: %s", body)
			}
			path := fmt.Sprintf("/api/model/export/SIMPLE_VIEW/%s/%d", tc.model, 1)
			resp := doAuth(t, http.MethodGet, path, "")
			exportBody := readBody(t, resp)
			if resp.StatusCode == http.StatusOK {
				t.Errorf("a rejected import must leave no model behind; export: %s", exportBody)
			}
		})
	}
}

// TestModelExport_DescribesEveryDeclaredBranch pins the export as a faithful
// description of what the model enforces: one wildcard hop per array level, and
// both branches of a field observed as a scalar and as an array.
func TestModelExport_DescribesEveryDeclaredBranch(t *testing.T) {
	const model = "e2e-export-branches"
	// Two imports while UNLOCKED: the second makes `poly` a kind union.
	importModelSampleE2E(t, model, 1, `{"m":[["A"],["B","C"]],"poly":"x"}`)
	importModelSampleE2E(t, model, 1, `{"poly":["A","B"]}`)
	lockModelE2E(t, model, 1)

	root := rootBucketE2E(t, model, 1)

	if _, stale := root[".m[*]"]; stale {
		t.Errorf(".m[*] must not stand in for an array of arrays; bucket: %v", root)
	}
	if got := root[".m[*][*]"]; got != "STRING" {
		t.Errorf(".m[*][*] = %v, want STRING; bucket: %v", got, root)
	}
	if got := root[".poly"]; got != "STRING" {
		t.Errorf(".poly = %v, want STRING (the scalar branch); bucket: %v", got, root)
	}
	if got := root[".poly[*]"]; got != "STRING" {
		t.Errorf(".poly[*] = %v, want STRING (the array branch); bucket: %v", got, root)
	}

	// Both declared branches really are admissible, which is what makes the
	// two-branch rendering the honest one.
	for _, payload := range []string{`{"poly":"z"}`, `{"poly":["A"]}`, `{"m":[["A"]]}`} {
		status, body := createEntityRawE2E(t, model, 1, payload)
		if status != http.StatusOK {
			t.Errorf("write %s: status = %d, want 200; body: %s", payload, status, body)
		}
	}
}
