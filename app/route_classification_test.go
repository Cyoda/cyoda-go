package app_test

import (
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
	"gopkg.in/yaml.v3"

	genapi "github.com/cyoda-platform/cyoda-go/api"
	internalapi "github.com/cyoda-platform/cyoda-go/internal/api"
)

// routeClassificationAllowList is a literal copy of the design's allow-list:
// the operations guarded by the admin rule (clients, trusted keys) or the
// operator rule (key pairs), and GET /account.
var routeClassificationAllowList = []string{
	"GET /account",
	"GET /clients", "POST /clients",
	"DELETE /clients/{clientId}", "PUT /clients/{clientId}/secret",
	"GET /oauth/keys/trusted", "POST /oauth/keys/trusted",
	"DELETE /oauth/keys/trusted/{keyId}",
	"POST /oauth/keys/trusted/{keyId}/invalidate", "POST /oauth/keys/trusted/{keyId}/reactivate",
	"POST /oauth/keys/keypair", "GET /oauth/keys/keypair/current",
	"DELETE /oauth/keys/keypair/{keyId}",
	"POST /oauth/keys/keypair/{keyId}/invalidate", "POST /oauth/keys/keypair/{keyId}/reactivate",
}

// handRegisteredDataRoutes are the authenticated data routes app.go registers
// on its own mux, outside the generated router.
var handRegisteredDataRoutes = [][2]string{
	{"GET", "/entity/{entityId}/transitions"},
	{"GET", "/platform-api/entity/fetch/transitions"},
	{"POST", "/entity/stats/{entityName}/{modelVersion}/query"},
}

// handRegisteredOperatorRoutes are the /admin routes app.go registers on its
// own mux. The operator guard decides them; ROLE_M2M does not.
var handRegisteredOperatorRoutes = [][2]string{
	{"GET", "/admin/log-level"},
	{"POST", "/admin/log-level"},
	{"GET", "/admin/trace-sampler"},
	{"POST", "/admin/trace-sampler"},
}

type classifiedOp struct {
	method, path string
	op           *openapi3.Operation
}

// servedAuthenticatedOps returns every OpenAPI operation the server routes and
// authenticates with a bearer token. It leaves out exactly the operations that
// are unauthenticated by protocol — those declaring `security: []` and the
// token endpoint (basic auth) — and returns as unserved the operations whose
// tags api/config.yaml excludes from code generation (not routed at all). Any
// other operation without bearer security fails the test: classify it.
func servedAuthenticatedOps(t *testing.T) (served, unserved []classifiedOp) {
	t.Helper()
	doc, err := genapi.GetSwagger()
	if err != nil {
		t.Fatal(err)
	}
	excluded := codegenExcludedTags(t)
	for path, item := range doc.Paths.Map() {
		for method, op := range item.Operations() {
			co := classifiedOp{method, path, op}
			if hasExcludedTag(op, excluded) {
				unserved = append(unserved, co)
				continue
			}
			if op.Security != nil && len(*op.Security) == 0 {
				continue // security: [] — unauthenticated by declaration
			}
			if method == http.MethodPost && path == "/oauth/token" {
				continue // the token endpoint: the client authenticates with basic auth
			}
			if !requiresBearer(doc, op) {
				t.Errorf("%s %s: neither bearer-authenticated, `security: []`, nor the token endpoint; classify it", method, path)
				continue
			}
			served = append(served, co)
		}
	}
	sort.Slice(served, func(i, j int) bool { return served[i].method+served[i].path < served[j].method+served[j].path })
	return served, unserved
}

func codegenExcludedTags(t *testing.T) map[string]bool {
	t.Helper()
	raw, err := os.ReadFile("../api/config.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var cfg struct {
		OutputOptions struct {
			ExcludeTags []string `yaml:"exclude-tags"`
		} `yaml:"output-options"`
	}
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	out := map[string]bool{}
	for _, tag := range cfg.OutputOptions.ExcludeTags {
		out[tag] = true
	}
	return out
}

func hasExcludedTag(op *openapi3.Operation, excluded map[string]bool) bool {
	for _, tag := range op.Tags {
		if excluded[tag] {
			return true
		}
	}
	return false
}

// requiresBearer reports whether op is authenticated with the bearer scheme:
// its own security requirement when it declares one, else the document's.
func requiresBearer(doc *openapi3.T, op *openapi3.Operation) bool {
	reqs := doc.Security
	if op.Security != nil {
		reqs = *op.Security
	}
	for _, req := range reqs {
		if _, ok := req["bearerAuth"]; ok {
			return true
		}
	}
	return false
}

var pathParam = regexp.MustCompile(`\{[^}]+\}`)

// sendWithoutM2M sends method path (parameters filled with a UUID) with a
// ROLE_ADMIN token that lacks ROLE_M2M, and reports the response.
func sendWithoutM2M(h http.Handler, tok, method, path string) *httptest.ResponseRecorder {
	concrete := pathParam.ReplaceAllString(path, "00000000-0000-0000-0000-000000000001")
	req := httptest.NewRequest(method, concrete, strings.NewReader("{}"))
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

func refusedForM2M(rr *httptest.ResponseRecorder) bool {
	return rr.Code == http.StatusForbidden && strings.Contains(rr.Body.String(), "this operation requires ROLE_M2M")
}

// TestRouteClassification_EveryRouteClassified sends a token without ROLE_M2M to
// every served OpenAPI operation and every hand-registered route. An
// allow-listed operation may answer anything but this refusal; every other one
// answers 403 "this operation requires ROLE_M2M" before its handler runs.
func TestRouteClassification_EveryRouteClassified(t *testing.T) {
	a, key := jwtAppWithKey(t)
	h, tok := a.Handler(), mintAppToken(t, key, "ROLE_ADMIN")

	served, unserved := servedAuthenticatedOps(t)
	if len(served) == 0 {
		t.Fatal("no served operations found in the OpenAPI document")
	}
	routes := make([][2]string, 0, len(served)+len(handRegisteredDataRoutes))
	for _, op := range served {
		routes = append(routes, [2]string{op.method, op.path})
	}
	routes = append(routes, handRegisteredDataRoutes...)
	for _, rt := range routes {
		rr := sendWithoutM2M(h, tok, rt[0], rt[1])
		allowed := internalapi.IsAllowListed(rt[0], rt[1])
		if allowed == refusedForM2M(rr) {
			t.Errorf("%s %s: allow-listed=%v but refused-for-ROLE_M2M=%v (status %d)",
				rt[0], rt[1], allowed, refusedForM2M(rr), rr.Code)
		}
	}

	// The /admin routes are operator-guarded, not ROLE_M2M-guarded: a tenant
	// admin is refused by the operator rule.
	for _, rt := range handRegisteredOperatorRoutes {
		rr := sendWithoutM2M(h, tok, rt[0], rt[1])
		if refusedForM2M(rr) || rr.Code != http.StatusForbidden || !strings.Contains(rr.Body.String(), "platform operator required") {
			t.Errorf("%s %s: status %d body %s, want the operator guard's 403", rt[0], rt[1], rr.Code, rr.Body.String())
		}
	}

	// An operation excluded from code generation is not routed. If one becomes
	// routed it must be classified above, so this skip fails loudly then.
	for _, op := range unserved {
		if rr := sendWithoutM2M(h, tok, op.method, op.path); rr.Code != http.StatusNotFound && rr.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s %s: excluded from code generation but answered %d; classify it", op.method, op.path, rr.Code)
		}
	}
}

// TestRouteClassification_RefusedBeforeJoin: a caller without ROLE_M2M that
// presents a transaction token is refused for the role before the join layer
// reads the token, on a generated route and on a hand-registered one.
func TestRouteClassification_RefusedBeforeJoin(t *testing.T) {
	a, key := jwtAppWithKey(t)
	h, tok := a.Handler(), mintAppToken(t, key, "ROLE_ADMIN")
	for _, rt := range [][2]string{
		{http.MethodGet, "/entity/00000000-0000-0000-0000-000000000001"},
		{http.MethodGet, "/entity/00000000-0000-0000-0000-000000000001/transitions"},
	} {
		req := httptest.NewRequest(rt[0], rt[1], nil)
		req.Header.Set("Authorization", "Bearer "+tok)
		req.Header.Set("X-Tx-Token", "not-a-pass")
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		if !refusedForM2M(rr) {
			t.Errorf("%s %s with a transaction token, without ROLE_M2M: %d %s, want the ROLE_M2M refusal",
				rt[0], rt[1], rr.Code, rr.Body.String())
		}
	}
}

// TestRouteClassification_AllowListMatchesSpec: the guard's allow-list is
// exactly the design's set, and every entry is an operation of the OpenAPI
// document (no stale entry).
func TestRouteClassification_AllowListMatchesSpec(t *testing.T) {
	got := internalapi.AllowList()
	sort.Strings(got)
	want := append([]string(nil), routeClassificationAllowList...)
	sort.Strings(want)
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("allow-list:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	served, _ := servedAuthenticatedOps(t)
	ops := map[string]bool{}
	for _, op := range served {
		ops[op.method+" "+op.path] = true
	}
	for _, entry := range want {
		if !ops[entry] {
			t.Errorf("allow-list entry %q is not a served operation of the OpenAPI document", entry)
		}
	}
}

// TestRouteClassification_OpenAPIDocumentsTheRefusal: every served operation
// outside the allow-list documents 403 with the ROLE_M2M cause.
func TestRouteClassification_OpenAPIDocumentsTheRefusal(t *testing.T) {
	served, _ := servedAuthenticatedOps(t)
	for _, op := range served {
		if internalapi.IsAllowListed(op.method, op.path) {
			continue
		}
		resp := op.op.Responses.Value("403")
		if resp == nil || resp.Value == nil || resp.Value.Description == nil {
			t.Errorf("%s %s: no 403 response documented", op.method, op.path)
			continue
		}
		if !strings.Contains(*resp.Value.Description, "ROLE_M2M") {
			t.Errorf("%s %s: 403 description %q does not name ROLE_M2M", op.method, op.path, *resp.Value.Description)
		}
	}
}
