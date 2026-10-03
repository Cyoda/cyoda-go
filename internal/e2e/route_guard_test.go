package e2e_test

import (
	"net/http"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	cyodapb "github.com/cyoda-platform/cyoda-go/api/grpc/cyoda"
	internalgrpc "github.com/cyoda-platform/cyoda-go/internal/grpc"
)

// noM2MSuiteToken is a tenant-admin token for the shared server without
// ROLE_M2M. No client can obtain one from /oauth/token — every client holds
// ROLE_M2M — so it is signed here.
func noM2MSuiteToken(t *testing.T) string {
	t.Helper()
	tok, err := signServiceToken(e2eSignKey, e2eIssuer, "", "route-guard", "test-tenant", "route-guard", []string{"ROLE_ADMIN"})
	if err != nil {
		t.Fatalf("sign token: %v", err)
	}
	return tok
}

// TestRouteGuard_DataRouteRequiresM2M: a data route refuses a caller without
// ROLE_M2M with 403 FORBIDDEN before its handler runs (the model need not
// exist), on a generated route and on a hand-registered one.
func TestRouteGuard_DataRouteRequiresM2M(t *testing.T) {
	tok := noM2MSuiteToken(t)
	for _, rt := range []struct{ method, path, body string }{
		{http.MethodPost, "/api/entity/JSON/route-guard/1", `{"a":1}`},
		{http.MethodGet, "/api/entity/00000000-0000-0000-0000-000000000001/transitions", ""},
	} {
		resp := doAuthAgainst(t, serverURL, tok, rt.method, rt.path, rt.body)
		body := readBody(t, resp)
		if resp.StatusCode != http.StatusForbidden || !strings.Contains(body, "FORBIDDEN") || !strings.Contains(body, "this operation requires ROLE_M2M") {
			t.Errorf("%s %s without ROLE_M2M: %d %s, want 403 FORBIDDEN naming ROLE_M2M", rt.method, rt.path, resp.StatusCode, body)
		}
	}
}

// TestRouteGuard_AllowListedRoutesNeedNoM2M: GET /account, a client route
// (admin rule) and an /admin route (operator rule) answer without ROLE_M2M.
func TestRouteGuard_AllowListedRoutesNeedNoM2M(t *testing.T) {
	tok := noM2MSuiteToken(t)
	for _, path := range []string{"/api/account", "/api/clients"} {
		resp := doAuthAgainst(t, serverURL, tok, http.MethodGet, path, "")
		if body := readBody(t, resp); resp.StatusCode != http.StatusOK {
			t.Errorf("GET %s without ROLE_M2M: %d %s, want 200", path, resp.StatusCode, body)
		}
	}
	op, err := platformTokenRaw("ROLE_ADMIN")
	if err != nil {
		t.Fatalf("sign operator token: %v", err)
	}
	resp := doAuthAgainst(t, serverURL, op, http.MethodGet, "/api/admin/log-level", "")
	if body := readBody(t, resp); resp.StatusCode != http.StatusOK {
		t.Errorf("GET /api/admin/log-level as an operator without ROLE_M2M: %d %s, want 200", resp.StatusCode, body)
	}
}

// TestRouteGuard_GRPCRequiresM2M: a unary method and a server stream refuse a
// caller without ROLE_M2M with PermissionDenied.
func TestRouteGuard_GRPCRequiresM2M(t *testing.T) {
	h := newCalloutHarness(t, nil)
	tok, err := signServiceToken(h.signKey, "cyoda-callback-test", h.audience, "route-guard", "test-tenant", "route-guard", []string{"ROLE_ADMIN"})
	if err != nil {
		t.Fatalf("sign token: %v", err)
	}
	client := cyodapb.NewCloudEventsServiceClient(h.apiConn)

	create, err := internalgrpc.NewCloudEvent(internalgrpc.EntityCreateRequest, map[string]any{
		"id": "route-guard", "dataFormat": "JSON",
		"payload": map[string]any{"model": map[string]any{"name": "route-guard", "version": 1}, "data": map[string]any{"a": 1}},
	})
	if err != nil {
		t.Fatalf("build create: %v", err)
	}
	_, err = client.EntityManage(h.grpcCtxAs(tok, ""), create)
	if st, _ := status.FromError(err); st.Code() != codes.PermissionDenied || !strings.Contains(st.Message(), "requires ROLE_M2M") {
		t.Errorf("EntityManage without ROLE_M2M: %v, want PermissionDenied naming ROLE_M2M", err)
	}

	search, err := internalgrpc.NewCloudEvent(internalgrpc.EntitySearchRequest, map[string]any{
		"id": "route-guard-search", "model": map[string]any{"name": "route-guard", "version": 1},
		"condition": map[string]any{"type": "simple", "jsonPath": "$.a", "operatorType": "EQUALS", "value": 1},
	})
	if err != nil {
		t.Fatalf("build search: %v", err)
	}
	stream, err := client.EntitySearchCollection(h.grpcCtxAs(tok, ""), search)
	if err == nil {
		_, err = stream.Recv()
	}
	if st, _ := status.FromError(err); st.Code() != codes.PermissionDenied || !strings.Contains(st.Message(), "requires ROLE_M2M") {
		t.Errorf("EntitySearchCollection without ROLE_M2M: %v, want PermissionDenied naming ROLE_M2M", err)
	}
}
