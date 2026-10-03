package auth_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/auth"
	"github.com/cyoda-platform/cyoda-go/internal/common"
)

func guardRequest(uc *spi.UserContext) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/x", nil)
	if uc != nil {
		r = r.WithContext(spi.WithUserContext(r.Context(), uc))
	}
	return r
}

func guardUC(tenant string, roles ...string) *spi.UserContext {
	return &spi.UserContext{UserID: "u", Tenant: spi.Tenant{ID: spi.TenantID(tenant)}, Roles: roles}
}

// guardErrorCode decodes an RFC 9457 body and returns its errorCode property
// (the same shape internal/auth/admin_authz_test.go decodes).
func guardErrorCode(t *testing.T, body []byte) string {
	t.Helper()
	var pd common.ProblemDetail
	if err := json.Unmarshal(body, &pd); err != nil {
		t.Fatalf("decode problem detail %s: %v", body, err)
	}
	code, _ := pd.Props["errorCode"].(string)
	return code
}

// guardProblemDetail decodes an RFC 9457 body and returns its detail field.
func guardProblemDetail(t *testing.T, body []byte) string {
	t.Helper()
	var pd common.ProblemDetail
	if err := json.Unmarshal(body, &pd); err != nil {
		t.Fatalf("decode problem detail %s: %v", body, err)
	}
	return pd.Detail
}

func TestOperatorGuard(t *testing.T) {
	cases := []struct {
		name       string
		guard      auth.OperatorGuard
		uc         *spi.UserContext
		wantOK     bool
		wantCode   int
		wantErr    string
		wantDetail string
	}{
		{"jwt: no user context", auth.OperatorGuard{}, nil, false, 401, "UNAUTHORIZED", "authentication failed"},
		{"jwt: tenant admin", auth.OperatorGuard{}, guardUC("acme", "ROLE_ADMIN"), false, 403, "FORBIDDEN", "platform operator required"},
		{"jwt: PLATFORM admin", auth.OperatorGuard{}, guardUC("PLATFORM", "ROLE_ADMIN"), true, 0, "", ""},
		{"jwt: PLATFORM admin among other roles", auth.OperatorGuard{}, guardUC("PLATFORM", "ROLE_M2M", "ROLE_ADMIN"), true, 0, "", ""},
		{"jwt: PLATFORM without ROLE_ADMIN", auth.OperatorGuard{}, guardUC("PLATFORM", "ROLE_M2M"), false, 403, "FORBIDDEN", "platform operator required"},
		{"jwt: lower-case platform admin", auth.OperatorGuard{}, guardUC("platform", "ROLE_ADMIN"), false, 403, "FORBIDDEN", "platform operator required"},
		{"jwt: SYSTEM admin", auth.OperatorGuard{}, guardUC("SYSTEM", "ROLE_ADMIN"), false, 403, "FORBIDDEN", "platform operator required"},
		{"mock: no user context", auth.MockOperatorGuard(), nil, false, 401, "UNAUTHORIZED", "authentication failed"},
		{"mock: admin in any tenant", auth.MockOperatorGuard(), guardUC("mock-tenant", "ROLE_ADMIN"), true, 0, "", ""},
		{"mock: no ROLE_ADMIN", auth.MockOperatorGuard(), guardUC("mock-tenant", "ROLE_M2M"), false, 403, "FORBIDDEN", "platform operator required"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			ok := tc.guard.Require(w, guardRequest(tc.uc))
			if ok != tc.wantOK {
				t.Fatalf("Require = %v, want %v (status %d, body %s)", ok, tc.wantOK, w.Code, w.Body.String())
			}
			if tc.wantOK {
				if w.Body.Len() != 0 {
					t.Errorf("an allowed caller must get no response written, got %d %s", w.Code, w.Body.String())
				}
				return
			}
			if w.Code != tc.wantCode {
				t.Errorf("status = %d, want %d", w.Code, tc.wantCode)
			}
			if got := guardErrorCode(t, w.Body.Bytes()); got != tc.wantErr {
				t.Errorf("errorCode = %q, want %q", got, tc.wantErr)
			}
			if got := guardProblemDetail(t, w.Body.Bytes()); !strings.Contains(got, tc.wantDetail) {
				t.Errorf("detail = %q, want it to contain %q", got, tc.wantDetail)
			}
		})
	}
}

func TestPlatformTenantID(t *testing.T) {
	if auth.PlatformTenantID != "PLATFORM" {
		t.Fatalf("PlatformTenantID = %q, want PLATFORM", auth.PlatformTenantID)
	}
}
