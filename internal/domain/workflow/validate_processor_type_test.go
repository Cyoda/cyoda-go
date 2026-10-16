package workflow_test

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/common/commontest"
)

// TestImport_ProcessorType pins the import rule for a processor's type:
// externalized is the only value accepted, an omitted or empty type is stored
// (and so exported) as externalized, and every other value is refused with
// 400 VALIDATION_FAILED naming the value and the allowed one.
func TestImport_ProcessorType(t *testing.T) {
	cases := []struct {
		name string
		// typeMember is the processor's "type" member as raw JSON, or "" to
		// omit it.
		typeMember string
		wantStatus int
		wantDetail []string
	}{
		{name: "omitted", typeMember: "", wantStatus: http.StatusOK},
		{name: "empty", typeMember: `"type": "",`, wantStatus: http.StatusOK},
		{name: "externalized", typeMember: `"type": "externalized",`, wantStatus: http.StatusOK},
		{name: "internalized", typeMember: `"type": "internalized",`, wantStatus: http.StatusBadRequest,
			wantDetail: []string{`processor "p"`, `unknown type "internalized"`, "allowed: externalized"}},
		{name: "banana", typeMember: `"type": "banana",`, wantStatus: http.StatusBadRequest,
			wantDetail: []string{`processor "p"`, `unknown type "banana"`, "allowed: externalized"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := newTestServer(t)
			importModel(t, srv.URL, "Order", 1)

			body := `{"importMode":"REPLACE","workflows":[{
				"version":"1.1","name":"type-wf","initialState":"S1","active":true,
				"states":{"S1":{"transitions":[{"name":"go","next":"S2","manual":true,
					"processors":[{` + tc.typeMember + `"name":"p","executionMode":"SYNC",
						"config":{"calculationNodesTags":"workers"}}]}]},
					"S2":{}}}]}`

			resp := doWorkflowImport(t, srv.URL, "Order", 1, body)
			if resp.StatusCode != tc.wantStatus {
				b, _ := io.ReadAll(resp.Body)
				resp.Body.Close()
				t.Fatalf("import status = %d, want %d; body: %s", resp.StatusCode, tc.wantStatus, b)
			}

			if tc.wantStatus != http.StatusOK {
				defer resp.Body.Close()
				commontest.ExpectErrorCode(t, resp, common.ErrCodeValidationFailed)
				var pd struct {
					Detail string `json:"detail"`
				}
				if err := json.NewDecoder(resp.Body).Decode(&pd); err != nil {
					t.Fatalf("decode problem detail: %v", err)
				}
				for _, want := range tc.wantDetail {
					if !strings.Contains(pd.Detail, want) {
						t.Errorf("detail %q must contain %q", pd.Detail, want)
					}
				}
				return
			}
			resp.Body.Close()

			wfs := readWorkflows(t, doWorkflowExport(t, srv.URL, "Order", 1))
			if len(wfs) != 1 {
				t.Fatalf("expected 1 workflow, got %d", len(wfs))
			}
			procs := wfs[0].States["S1"].Transitions[0].Processors
			if len(procs) != 1 {
				t.Fatalf("expected 1 processor, got %+v", procs)
			}
			if procs[0].Type != "externalized" {
				t.Errorf("exported processor type = %q, want %q", procs[0].Type, "externalized")
			}
		})
	}
}
