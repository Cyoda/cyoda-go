package e2e_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// A model's field set is established on exactly two doors: the explicit
// sample-data import, and the ChangeLevel-driven schema extension performed by
// an entity write. A field name the wire jsonPath grammar cannot address is
// refused on both — recording one would guarantee a field that can be written
// and never queried. These tests pin the HTTP contract of both doors on a real
// backend: 400 VALIDATION_FAILED, with the offending key named in the detail so
// the operator knows what to rename.

// problemDetail pulls the RFC 9457 detail string out of an error body.
func problemDetail(t *testing.T, body string) string {
	t.Helper()
	var pd struct {
		Detail string `json:"detail"`
	}
	if err := json.Unmarshal([]byte(body), &pd); err != nil {
		t.Fatalf("decode problem detail %q: %v", body, err)
	}
	return pd.Detail
}

func TestModelImport_UnaddressableFieldName_400(t *testing.T) {
	cases := []struct {
		name   string
		sample string
		want   string
	}{
		{"non-ascii", `{"café":5}`, `café`},
		{"space", `{"first name":"x"}`, `first name`},
		{"dot in key", `{"first.name":1}`, `first.name`},
		{"empty key", `{"":1}`, `""`},
		{"bracket", `{"a[0]":1}`, `a[0]`},
		{"nested", `{"ok":{"bad name":1}}`, `bad name`},
		{"in array element", `{"arr":[{"bad name":1}]}`, `bad name`},
	}
	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := fmt.Sprintf("e2e-fieldname-import-%d", i)
			resp := doAuth(t, http.MethodPost,
				fmt.Sprintf("/api/model/import/JSON/SAMPLE_DATA/%s/1", m), c.sample)
			body := readBody(t, resp)
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("import %s: expected 400, got %d: %s", c.sample, resp.StatusCode, body)
			}
			assertErrorCode(t, body, "VALIDATION_FAILED")
			if detail := problemDetail(t, body); !strings.Contains(detail, c.want) {
				t.Errorf("detail must name the offending field %q, got %q", c.want, detail)
			}

			// Fail-closed: nothing was recorded, so the model does not exist.
			exp := doAuth(t, http.MethodGet, "/api/model/export/SIMPLE_VIEW/"+m+"/1", "")
			expBody := readBody(t, exp)
			if exp.StatusCode != http.StatusNotFound {
				t.Errorf("rejected import must leave no model, got %d: %s", exp.StatusCode, expBody)
			}
		})
	}
}

// TestModelImport_AddressableFieldName_200 is the positive control on the same
// door: every name the grammar can spell still imports, and the resulting
// fields are addressable by a search jsonPath end to end.
func TestModelImport_AddressableFieldName_200(t *testing.T) {
	const m = "e2e-fieldname-ok"
	const sample = `{"_":1,"-":2,"_meta":3,"first-name":"x","first_name":"y",` +
		`"camelCase":1,"UPPER":1,"0abc":1,"x_1-2A":1,"nested":{"b-c":[{"d_1":1}]},"tags":["a"]}`
	importModelSampleE2E(t, m, 1, sample)

	exported := exportModelE2E(t, m, 1)
	if len(exported) == 0 {
		t.Fatal("expected a non-empty export for an accepted import")
	}
}

// TestEntityCreate_UnaddressableFieldName_400 covers the auto-evolution door.
// The model is LOCKED with a ChangeLevel, so the write is allowed to grow the
// field set — which is exactly when the name rule has to fire. Without this the
// rule would be enforced at explicit import only and the inconsistency would
// stay wide open.
func TestEntityCreate_UnaddressableFieldName_400(t *testing.T) {
	const m = "e2e-fieldname-extend"
	importModelE2E(t, m, 1)
	lockModelE2E(t, m, 1)
	setChangeLevelE2E(t, m, 1, "STRUCTURAL")

	before := exportModelE2E(t, m, 1)

	resp := doAuth(t, http.MethodPost, "/api/entity/JSON/"+m+"/1",
		`{"name":"Test","amount":100,"status":"new","first name":"nope"}`)
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("create with unaddressable field: expected 400, got %d: %s", resp.StatusCode, body)
	}
	assertErrorCode(t, body, "VALIDATION_FAILED")
	if detail := problemDetail(t, body); !strings.Contains(detail, "first name") {
		t.Errorf("detail must name the offending field, got %q", detail)
	}

	// The rejected extension must leave the schema untouched.
	after := exportModelE2E(t, m, 1)
	beforeJSON, _ := json.Marshal(before)
	afterJSON, _ := json.Marshal(after)
	if string(beforeJSON) != string(afterJSON) {
		t.Errorf("rejected extension mutated the schema\n  before: %s\n  after:  %s", beforeJSON, afterJSON)
	}
}

// TestEntityCreate_AddressableFieldName_200 is the positive control on the
// auto-evolution door: an addressable new field still extends the schema.
func TestEntityCreate_AddressableFieldName_200(t *testing.T) {
	const m = "e2e-fieldname-extend-ok"
	importModelE2E(t, m, 1)
	lockModelE2E(t, m, 1)
	setChangeLevelE2E(t, m, 1, "STRUCTURAL")

	createEntityE2E(t, m, 1, `{"name":"Test","amount":100,"status":"new","new-field_9":"yes"}`)

	exported, err := json.Marshal(exportModelE2E(t, m, 1))
	if err != nil {
		t.Fatalf("marshal export: %v", err)
	}
	if !strings.Contains(string(exported), "new-field_9") {
		t.Errorf("expected the extended schema to carry new-field_9, got %s", exported)
	}
}
