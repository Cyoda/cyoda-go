package api

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cyoda-platform/cyoda-go/cmd/cyoda/help"
	"github.com/cyoda-platform/cyoda-go/cmd/cyoda/help/renderer"
)

func helpTestServer(t *testing.T, contextPath string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	RegisterHelpRoutes(mux, help.DefaultTree, contextPath, "dev")
	return httptest.NewServer(mux)
}

func TestGetFullTree(t *testing.T) {
	srv := helpTestServer(t, "/api")
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/api/help")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("status = %d", resp.StatusCode)
	}
	var payload renderer.HelpPayload
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	if payload.Schema != 1 {
		t.Errorf("schema = %d", payload.Schema)
	}
	if len(payload.Topics) == 0 {
		t.Error("no topics in payload")
	}
}

func TestGetSingleTopic(t *testing.T) {
	srv := helpTestServer(t, "/api")
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/api/help/cli")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("status = %d", resp.StatusCode)
	}
	var d renderer.TopicDescriptor
	if err := json.NewDecoder(resp.Body).Decode(&d); err != nil {
		t.Fatal(err)
	}
	if d.Topic != "cli" {
		t.Errorf("topic = %q", d.Topic)
	}
}

func TestGetUnknownTopic_404_RFC7807(t *testing.T) {
	srv := helpTestServer(t, "/api")
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/api/help/widgetry")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Errorf("status = %d", resp.StatusCode)
	}
	ct := resp.Header.Get("Content-Type")
	if !strings.Contains(ct, "application/problem+json") {
		t.Errorf("content-type = %q", ct)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "HELP_TOPIC_NOT_FOUND") {
		t.Errorf("body missing code: %q", body)
	}
}

func TestMalformedTopicPath_400(t *testing.T) {
	srv := helpTestServer(t, "/api")
	defer srv.Close()
	// %20 decodes to space — not in [A-Za-z0-9._-]
	resp, err := http.Get(srv.URL + "/api/help/foo%20bar")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "BAD_REQUEST") {
		t.Errorf("body missing code: %q", body)
	}
}


func TestMalformedTopicPath_400_LeadingDot(t *testing.T) {
	srv := helpTestServer(t, "/api")
	defer srv.Close()
	resp, _ := http.Get(srv.URL + "/api/help/.foo")
	if err := resp.Body.Close(); err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 400 {
		t.Errorf("status = %d, want 400 (leading dot)", resp.StatusCode)
	}
}

func TestMalformedTopicPath_400_TrailingDot(t *testing.T) {
	srv := helpTestServer(t, "/api")
	defer srv.Close()
	resp, _ := http.Get(srv.URL + "/api/help/foo.")
	if err := resp.Body.Close(); err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 400 {
		t.Errorf("status = %d, want 400 (trailing dot)", resp.StatusCode)
	}
}

func TestMalformedTopicPath_BareDot_RedirectsToCanonical(t *testing.T) {
	// Go's http.ServeMux always redirects dot-path segments (e.g. /help/.) to
	// the canonical path (/help) via 307 before our handler runs. The Go HTTP
	// client follows that redirect, so the observable status is 200 (full tree).
	// We document this here rather than asserting 400 (which we cannot produce
	// through the standard http.Client because URL normalization strips the dot
	// before transmission). The topicPathPattern regex does reject a bare "."
	// if it were ever delivered directly.
	srv := helpTestServer(t, "/api")
	defer srv.Close()
	resp, _ := http.Get(srv.URL + "/api/help/.")
	if err := resp.Body.Close(); err != nil {
		t.Fatal(err)
	}
	// After redirect the client ends up at /api/help and receives the full tree.
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200 (after ServeMux dot-redirect to /api/help)", resp.StatusCode)
	}
}


func TestNonGET_Returns405(t *testing.T) {
	srv := helpTestServer(t, "/api")
	defer srv.Close()
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch} {
		t.Run(method, func(t *testing.T) {
			req, _ := http.NewRequest(method, srv.URL+"/api/help", nil)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusMethodNotAllowed {
				t.Errorf("%s / : status = %d, want 405", method, resp.StatusCode)
			}
			if got := resp.Header.Get("Allow"); got != "GET" {
				t.Errorf("%s / : Allow = %q, want \"GET\"", method, got)
			}
		})
	}

	// Same check for the subtree handler
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
		t.Run(method+"/topic", func(t *testing.T) {
			req, _ := http.NewRequest(method, srv.URL+"/api/help/cli", nil)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusMethodNotAllowed {
				t.Errorf("%s /cli : status = %d, want 405", method, resp.StatusCode)
			}
		})
	}
}

func TestGetSingleTopic_SlashSeparator(t *testing.T) {
	srv := helpTestServer(t, "/api")
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/api/help/cli/help")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	var d renderer.TopicDescriptor
	if err := json.NewDecoder(resp.Body).Decode(&d); err != nil {
		t.Fatal(err)
	}
	if d.Topic != "cli.help" {
		t.Errorf("topic = %q, want %q (slash form must resolve to dotted canonical)", d.Topic, "cli.help")
	}
}

func TestGetSingleTopic_MixedSeparators(t *testing.T) {
	srv := helpTestServer(t, "/api")
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/api/help/errors/VALIDATION_FAILED")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	var d renderer.TopicDescriptor
	if err := json.NewDecoder(resp.Body).Decode(&d); err != nil {
		t.Fatal(err)
	}
	if d.Topic != "errors.VALIDATION_FAILED" {
		t.Errorf("topic = %q, want %q", d.Topic, "errors.VALIDATION_FAILED")
	}
}

func TestMalformedTopicPath_DoubleSlash_CleanedByServeMux(t *testing.T) {
	// Go's http.ServeMux cleans consecutive slashes before the handler runs
	// (cli//help → cli/help). The observable effect is the same as a single
	// slash: the handler sees "cli/help", normalises to "cli.help", and
	// resolves to the cli.help topic (200). We cannot detect the double-slash
	// at the handler layer. This is documented behaviour, analogous to the
	// ServeMux dot-redirect behaviour tested in
	// TestMalformedTopicPath_BareDot_RedirectsToCanonical.
	srv := helpTestServer(t, "/api")
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/api/help/cli//help")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	// ServeMux cleans cli//help → cli/help before the handler; the topic
	// resolves to cli.help → 200.
	if resp.StatusCode != 200 {
		t.Errorf("status = %d, want 200 (ServeMux cleans double-slash before handler)", resp.StatusCode)
	}
}

func TestMalformedTopicPath_400_DoubleDot(t *testing.T) {
	srv := helpTestServer(t, "/api")
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/api/help/cli..help")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Errorf("status = %d, want 400 (double dot → empty segment)", resp.StatusCode)
	}
}

func TestMalformedTopicPath_400_MixedEmptySegment(t *testing.T) {
	srv := helpTestServer(t, "/api")
	defer srv.Close()
	// cli./help — dot followed by slash: normalises to "cli..help" → empty segment
	resp, err := http.Get(srv.URL + "/api/help/cli./help")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	// regex now admits / and . as path characters, so it passes the regex;
	// but normalization yields an empty segment → 400.
	if resp.StatusCode != 400 {
		t.Errorf("status = %d, want 400 (mixed with empty segment)", resp.StatusCode)
	}
}

func TestGetUnknownTopic_SlashForm_404(t *testing.T) {
	srv := helpTestServer(t, "/api")
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/api/help/cli/nonexistent")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "HELP_TOPIC_NOT_FOUND") {
		t.Errorf("response body missing HELP_TOPIC_NOT_FOUND: %q", body)
	}
}

func TestRespectsContextPath(t *testing.T) {
	srv := helpTestServer(t, "/v1/api")
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/v1/api/help")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("customized context path failed: %d", resp.StatusCode)
	}
	resp2, err := http.Get(srv.URL + "/api/help")
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode == 200 {
		t.Errorf("default /api/help should not respond when ContextPath is /v1/api")
	}
}
