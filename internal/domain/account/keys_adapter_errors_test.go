package account

import (
	"context"
	"crypto/rsa"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	genapi "github.com/cyoda-platform/cyoda-go/api"
	"github.com/cyoda-platform/cyoda-go/internal/auth"
)

type failingKeyStore struct{ err error }

func (f failingKeyStore) Signer(string) (*auth.KeyPair, auth.Signer, error) { return nil, nil, f.err }
func (f failingKeyStore) Current(string) (*auth.KeyPair, error)             { return nil, f.err }
func (f failingKeyStore) VerificationKey(string) (*rsa.PublicKey, error)    { return nil, f.err }
func (f failingKeyStore) Published() ([]*auth.KeyPair, error)               { return nil, f.err }
func (f failingKeyStore) Issue(context.Context, auth.IssueRequest) (*auth.KeyPair, error) {
	return nil, f.err
}
func (f failingKeyStore) Invalidate(context.Context, string, int64) error { return f.err }
func (f failingKeyStore) Reactivate(context.Context, string, time.Time, time.Time) (*auth.KeyPair, error) {
	return nil, f.err
}
func (f failingKeyStore) Delete(context.Context, string) error { return f.err }

type unavailable struct{}

func (unavailable) Error() string            { return "down" }
func (unavailable) StorageUnavailable() bool { return true }

// operatorReq builds a request as a platform operator (ROLE_ADMIN in
// auth.PlatformTenantID) — required by the key-pair handlers this file tests.
func operatorReq(method, path, body string) *http.Request {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	return r.WithContext(spi.WithUserContext(r.Context(), &spi.UserContext{
		UserID: "a", Roles: []string{"ROLE_ADMIN"}, Tenant: spi.Tenant{ID: auth.PlatformTenantID},
	}))
}

// wellFormedKID has the key-pair id form: 32 lowercase hex characters.
const wellFormedKID = "0123456789abcdef0123456789abcdef"

func TestKeyPairAdapters_StoreErrorsAreNot404(t *testing.T) {
	future := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	calls := map[string]func(h *Handler, w http.ResponseWriter){
		"issue": func(h *Handler, w http.ResponseWriter) {
			h.IssueJwtKeyPair(w, operatorReq("POST", "/oauth/keys/keypair", `{"algorithm":"RS256","audience":"client"}`))
		},
		"current": func(h *Handler, w http.ResponseWriter) {
			h.GetCurrentJwtKeyPair(w, operatorReq("GET", "/oauth/keys/keypair/current", ""), genapi.GetCurrentJwtKeyPairParams{Audience: "client"})
		},
		"delete": func(h *Handler, w http.ResponseWriter) {
			h.DeleteJwtKeyPair(w, operatorReq("DELETE", "/oauth/keys/keypair/"+wellFormedKID, ""), wellFormedKID)
		},
		"invalidate": func(h *Handler, w http.ResponseWriter) {
			h.InvalidateJwtKeyPair(w, operatorReq("POST", "/oauth/keys/keypair/"+wellFormedKID+"/invalidate", ""), wellFormedKID)
		},
		"reactivate": func(h *Handler, w http.ResponseWriter) {
			h.ReactivateJwtKeyPair(w, operatorReq("POST", "/oauth/keys/keypair/"+wellFormedKID+"/reactivate", `{"validTo":"`+future+`"}`), wellFormedKID)
		},
	}
	for name, call := range calls {
		for _, tc := range []struct {
			err  error
			want int
		}{
			{errors.New("boom"), http.StatusInternalServerError},
			{unavailable{}, http.StatusServiceUnavailable},
			{auth.ErrStoreStale, http.StatusServiceUnavailable},
			{auth.ErrKeyPairNotFound, http.StatusNotFound},
		} {
			h := &Handler{keyStore: failingKeyStore{err: tc.err}, iam: auth.DefaultIAMFeatures()}
			w := httptest.NewRecorder()
			call(h, w)
			if w.Code != tc.want {
				t.Errorf("%s with %v: status %d, want %d", name, tc.err, w.Code, tc.want)
			}
		}
	}
}

// A keyId that is not 32 lowercase hex characters — the form of every issued
// and bootstrap KID — is refused with 400 before any store call: the store
// here would answer 404 for anything it is asked.
func TestKeyPairAdapters_MalformedKeyId_400(t *testing.T) {
	future := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	calls := map[string]func(h *Handler, w http.ResponseWriter, kid string){
		"delete": func(h *Handler, w http.ResponseWriter, kid string) {
			h.DeleteJwtKeyPair(w, operatorReq("DELETE", "/", ""), kid)
		},
		"invalidate": func(h *Handler, w http.ResponseWriter, kid string) {
			h.InvalidateJwtKeyPair(w, operatorReq("POST", "/", ""), kid)
		},
		"reactivate": func(h *Handler, w http.ResponseWriter, kid string) {
			h.ReactivateJwtKeyPair(w, operatorReq("POST", "/", `{"validTo":"`+future+`"}`), kid)
		},
	}
	for name, call := range calls {
		for _, kid := range []string{"", "not-a-kid", strings.ToUpper(wellFormedKID), wellFormedKID[1:], wellFormedKID + "0", "../" + wellFormedKID[3:]} {
			h := &Handler{keyStore: failingKeyStore{err: auth.ErrKeyPairNotFound}, iam: auth.DefaultIAMFeatures()}
			w := httptest.NewRecorder()
			call(h, w, kid)
			if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "BAD_REQUEST") || !strings.Contains(w.Body.String(), "invalid keyId format") {
				t.Errorf("%s %q: status %d body %s, want 400 BAD_REQUEST invalid keyId format", name, kid, w.Code, w.Body.String())
			}
		}
	}
}
