package app_test

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/cyoda-platform/cyoda-go/internal/common"
)

// The default-workflow fallback raises a warning with common.AddWarning, and an
// HTTP client receives it in a Cyoda-Warning header: on a write, and on the
// transitions read, which is HTTP-only. A request without a warning carries no
// such header.
func TestHTTPWarnings_ReachTheClient(t *testing.T) {
	srv := clockNode(t, nil)
	const sample = `{"k":1}`
	if st, raw := call(t, srv, http.MethodPost, "/model/import/JSON/SAMPLE_DATA/WarningsWiring/1", sample); st != http.StatusOK {
		t.Fatalf("import model: %d %s", st, raw)
	}
	if st, raw := call(t, srv, http.MethodPut, "/model/WarningsWiring/1/lock", ""); st != http.StatusOK {
		t.Fatalf("lock model: %d %s", st, raw)
	}

	do := func(method, path, body string) (*http.Response, []byte) {
		t.Helper()
		req, err := http.NewRequest(method, srv.URL+path, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var raw json.RawMessage
		_ = json.NewDecoder(resp.Body).Decode(&raw)
		return resp, raw
	}
	wantFallback := func(what string, resp *http.Response) {
		t.Helper()
		got := resp.Header.Values(common.WarningHeader)
		if len(got) != 1 {
			t.Fatalf("%s: %s = %q; want the one default-workflow warning", what, common.WarningHeader, got)
		}
		dec, err := url.PathUnescape(got[0])
		if err != nil || dec != "no workflows imported for model — using default workflow" {
			t.Fatalf("%s: %s = %q (decoded %q, %v)", what, common.WarningHeader, got[0], dec, err)
		}
	}

	resp, raw := do(http.MethodPost, "/entity/JSON/WarningsWiring/1", sample)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("create entity: %d %s", resp.StatusCode, raw)
	}
	wantFallback("create", resp)
	var created []struct {
		EntityIDs []string `json:"entityIds"`
	}
	if err := json.Unmarshal(raw, &created); err != nil || len(created) == 0 || len(created[0].EntityIDs) == 0 {
		t.Fatalf("create entity answered %s", raw)
	}
	id := created[0].EntityIDs[0]

	resp, raw = do(http.MethodGet, "/entity/"+id+"/transitions", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("transitions: %d %s", resp.StatusCode, raw)
	}
	wantFallback("transitions", resp)

	resp, raw = do(http.MethodGet, "/entity/"+id, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get entity: %d %s", resp.StatusCode, raw)
	}
	if got, ok := resp.Header[common.WarningHeader]; ok {
		t.Fatalf("a read that raised no warning carries %s: %q", common.WarningHeader, got)
	}
}
