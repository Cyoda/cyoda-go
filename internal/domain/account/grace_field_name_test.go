package account_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cyoda-platform/cyoda-go/internal/auth"
	"github.com/cyoda-platform/cyoda-go/internal/domain/account"
)

// A 400 for an out-of-range grace period names the field the request
// actually carries: invalidateGracePeriodSec on issue and register, which is
// not the gracePeriodSec of the invalidate endpoints.
func TestGracePeriodRangeError_NamesInvalidateGracePeriodSec(t *testing.T) {
	feats := auth.DefaultIAMFeatures()
	feats.TrustedKeyRegistrationEnabled = true
	h := account.New(nil, nil, newTestKeyStore(t), newTestTrustedStore(t), nil, feats)
	jwkBytes, _ := json.Marshal(rsaJWK(t, "k"))

	for _, grace := range []string{"-1", "9999999999"} {
		t.Run("issue/"+grace, func(t *testing.T) {
			body := []byte(`{"algorithm":"RS256","audience":"client","invalidateCurrent":true,"invalidateGracePeriodSec":` + grace + `}`)
			w := httptest.NewRecorder()
			h.IssueJwtKeyPair(w, adminReq(t, "POST", "/", body))
			assertGraceFieldNamed(t, w)
		})
		t.Run("register/"+grace, func(t *testing.T) {
			body := append([]byte(`{"keyId":"k","jwk":`), jwkBytes...)
			body = append(body, []byte(`,"audience":"human","invalidatePrevious":true,"invalidateGracePeriodSec":`+grace+`}`)...)
			w := httptest.NewRecorder()
			h.RegisterTrustedKey(w, adminReq(t, "POST", "/", body))
			assertGraceFieldNamed(t, w)
		})
	}
}

func assertGraceFieldNamed(t *testing.T, w *httptest.ResponseRecorder) {
	t.Helper()
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status=%d want 400; body=%s", w.Code, w.Body.String())
	}
	var problem struct {
		Detail string `json:"detail"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &problem); err != nil {
		t.Fatalf("decode problem detail: %v; body=%s", err, w.Body.String())
	}
	if !strings.Contains(problem.Detail, "invalidateGracePeriodSec must be") {
		t.Errorf("detail %q does not name invalidateGracePeriodSec", problem.Detail)
	}
}
