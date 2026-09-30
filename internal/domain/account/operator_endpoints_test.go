package account_test

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/auth"
	"github.com/cyoda-platform/cyoda-go/internal/domain/account"

	genapi "github.com/cyoda-platform/cyoda-go/api"
)

func ucReq(method, path string, body []byte, uc *spi.UserContext) *http.Request {
	var r *http.Request
	if body != nil {
		r = httptest.NewRequest(method, path, bytes.NewReader(body))
	} else {
		r = httptest.NewRequest(method, path, nil)
	}
	return r.WithContext(spi.WithUserContext(r.Context(), uc))
}

func operatorUC() *spi.UserContext {
	return &spi.UserContext{UserID: "op", Tenant: spi.Tenant{ID: auth.PlatformTenantID}, Roles: []string{"ROLE_ADMIN"}}
}

// keyPairCalls invokes each key-pair handler as uc. The kid is well formed;
// the guard must refuse before it is looked up.
func keyPairCalls(h *account.Handler, uc *spi.UserContext) map[string]func() *httptest.ResponseRecorder {
	const kid = "0123456789abcdef0123456789abcdef"
	run := func(f func(w http.ResponseWriter)) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		f(w)
		return w
	}
	validTo := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	return map[string]func() *httptest.ResponseRecorder{
		"issue": func() *httptest.ResponseRecorder {
			return run(func(w http.ResponseWriter) {
				h.IssueJwtKeyPair(w, ucReq("POST", "/oauth/keys/keypair", []byte(`{"algorithm":"RS256","audience":"human"}`), uc))
			})
		},
		"current": func() *httptest.ResponseRecorder {
			return run(func(w http.ResponseWriter) {
				h.GetCurrentJwtKeyPair(w, ucReq("GET", "/oauth/keys/keypair/current?audience=human", nil, uc),
					genapiCurrentParams("human"))
			})
		},
		"invalidate": func() *httptest.ResponseRecorder {
			return run(func(w http.ResponseWriter) { h.InvalidateJwtKeyPair(w, ucReq("POST", "/x", nil, uc), kid) })
		},
		"reactivate": func() *httptest.ResponseRecorder {
			return run(func(w http.ResponseWriter) {
				h.ReactivateJwtKeyPair(w, ucReq("POST", "/x", []byte(`{"validTo":"`+validTo+`"}`), uc), kid)
			})
		},
		"delete": func() *httptest.ResponseRecorder {
			return run(func(w http.ResponseWriter) { h.DeleteJwtKeyPair(w, ucReq("DELETE", "/x", nil, uc), kid) })
		},
	}
}

func genapiCurrentParams(aud string) genapi.GetCurrentJwtKeyPairParams {
	return genapi.GetCurrentJwtKeyPairParams{Audience: genapi.GetCurrentJwtKeyPairParamsAudience(aud)}
}

func TestKeyPairEndpoints_RefuseTenantAdmin(t *testing.T) {
	ks := newTestKeyStore(t)
	h := account.New(nil, nil, ks, newTestTrustedStore(t), nil, auth.DefaultIAMFeatures(), auth.OperatorGuard{})
	for name, call := range keyPairCalls(h, adminUC()) { // adminUC: ROLE_ADMIN in tenant t1
		t.Run(name, func(t *testing.T) {
			w := call()
			if w.Code != http.StatusForbidden {
				t.Fatalf("status = %d, want 403; body %s", w.Code, w.Body.String())
			}
		})
	}
	// Nothing was issued: the only key is the bootstrap key.
	if _, err := ks.Current("human"); err == nil {
		t.Error("a tenant admin's refused issue left a human key pair behind")
	}
}

func TestKeyPairEndpoints_AdmitPlatformOperator(t *testing.T) {
	h := account.New(nil, nil, newTestKeyStore(t), newTestTrustedStore(t), nil, auth.DefaultIAMFeatures(), auth.OperatorGuard{})
	w := keyPairCalls(h, operatorUC())["issue"]()
	if w.Code != http.StatusOK {
		t.Fatalf("issue as operator: status = %d, body %s", w.Code, w.Body.String())
	}
}

func TestKeyPairEndpoints_MockGuard_Answer501(t *testing.T) {
	mockUC := &spi.UserContext{UserID: "mock-user-001", Tenant: spi.Tenant{ID: "mock-tenant"}, Roles: []string{"ROLE_ADMIN", "ROLE_M2M"}}
	h := account.New(nil, nil, nil, nil, nil, auth.DefaultIAMFeatures(), auth.MockOperatorGuard())
	for name, call := range keyPairCalls(h, mockUC) {
		t.Run(name, func(t *testing.T) {
			if w := call(); w.Code != http.StatusNotImplemented {
				t.Fatalf("status = %d, want 501; body %s", w.Code, w.Body.String())
			}
		})
	}
}
