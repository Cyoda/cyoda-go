package e2e_test

import (
	"fmt"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/cyoda-platform/cyoda-go/internal/common"
)

// http_warnings_test.go: an HTTP response carries each warning the request
// raised in a Cyoda-Warning header field line, percent-encoded.

// decodedWarnings returns resp's Cyoda-Warning values, percent-decoded.
func decodedWarnings(t *testing.T, resp *http.Response) []string {
	t.Helper()
	raw := resp.Header.Values(common.WarningHeader)
	out := make([]string, 0, len(raw))
	for _, v := range raw {
		d, err := url.PathUnescape(v)
		if err != nil {
			t.Fatalf("%s value %q does not percent-decode: %v", common.WarningHeader, v, err)
		}
		out = append(out, d)
	}
	return out
}

// A model with no imported workflow runs the default workflow, and the create
// says so in a Cyoda-Warning header. A response that raised no warning has no
// such header.
func TestHTTPWarnings_DefaultWorkflowFallback(t *testing.T) {
	model := "e2e-http-warn-default-" + randSuffix(t)
	importModel(t, model, 1)

	resp := doAuth(t, http.MethodPost, fmt.Sprintf("/api/entity/JSON/%s/1", model), `{"x":1}`)
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("create: %d %s", resp.StatusCode, body)
	}
	got := decodedWarnings(t, resp)
	if len(got) != 1 || got[0] != "no workflows imported for model — using default workflow" {
		t.Fatalf("%s = %q; want the default-workflow warning", common.WarningHeader, got)
	}

	resp = doAuth(t, http.MethodGet, fmt.Sprintf("/api/entity/stats/%s/1", model), "")
	_ = readBody(t, resp)
	if v, ok := resp.Header[common.WarningHeader]; ok {
		t.Fatalf("a response that raised no warning carries %s: %q", common.WarningHeader, v)
	}
}

// A compute member's warnings reach the HTTP client, prefixed with the
// callout's kind and name as in the gRPC `warnings` array; a CR or LF in the
// member's text cannot add a header of its own.
func TestHTTPWarnings_ComputeMemberWarnings(t *testing.T) {
	h := newCalloutHarness(t, calloutTuning(1, 100*time.Millisecond))
	sfx := randSuffix(t)
	tag, model := "http-warn-"+sfx, "http-warn-model-"+sfx
	reply := answerOK()
	reply.warnings = []string{"member note", "evil\r\nX-Injected: yes"}
	cn := h.AttachCnode(t, cnodeSpec{name: "warner", tags: []string{tag}, script: scriptAlways(reply)})
	defer cn.Detach(t)
	h.SetupModelWithWorkflow(t, model, chainWorkflowJSON("http-warn-wf", procSpec{"warn-proc", "SYNC",
		map[string]any{"calculationNodesTags": tag, "responseTimeoutMs": 2000}}))

	resp := h.DoAuth(t, http.MethodPost, fmt.Sprintf("/api/entity/JSON/%s/1", model), workflowSampleModel, "")
	body := h.readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("create: %d %s", resp.StatusCode, body)
	}
	got := decodedWarnings(t, resp)
	want := []string{"processor warn-proc: member note", "processor warn-proc: evil  X-Injected: yes"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("%s = %q; want %q", common.WarningHeader, got, want)
	}
	if v := resp.Header.Get("X-Injected"); v != "" {
		t.Fatalf("the member's text added a header of its own: X-Injected: %q", v)
	}
}
