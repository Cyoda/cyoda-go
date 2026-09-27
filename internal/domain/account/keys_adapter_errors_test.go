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

func adminReq(method, path, body string) *http.Request {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	return r.WithContext(spi.WithUserContext(r.Context(), &spi.UserContext{
		UserID: "a", Roles: []string{"ROLE_ADMIN"}, Tenant: spi.Tenant{ID: "t"},
	}))
}

func TestKeyPairAdapters_StoreErrorsAreNot404(t *testing.T) {
	future := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	calls := map[string]func(h *Handler, w http.ResponseWriter){
		"issue": func(h *Handler, w http.ResponseWriter) {
			h.IssueJwtKeyPair(w, adminReq("POST", "/oauth/keys/keypair", `{"algorithm":"RS256","audience":"client"}`))
		},
		"current": func(h *Handler, w http.ResponseWriter) {
			h.GetCurrentJwtKeyPair(w, adminReq("GET", "/oauth/keys/keypair/current", ""), genapi.GetCurrentJwtKeyPairParams{Audience: "client"})
		},
		"delete": func(h *Handler, w http.ResponseWriter) {
			h.DeleteJwtKeyPair(w, adminReq("DELETE", "/oauth/keys/keypair/k", ""), "k")
		},
		"invalidate": func(h *Handler, w http.ResponseWriter) {
			h.InvalidateJwtKeyPair(w, adminReq("POST", "/oauth/keys/keypair/k/invalidate", ""), "k")
		},
		"reactivate": func(h *Handler, w http.ResponseWriter) {
			h.ReactivateJwtKeyPair(w, adminReq("POST", "/oauth/keys/keypair/k/reactivate", `{"validTo":"`+future+`"}`), "k")
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
