package auth_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	genapi "github.com/cyoda-platform/cyoda-go/api"
	"github.com/cyoda-platform/cyoda-go/internal/auth"
	"github.com/cyoda-platform/cyoda-go/internal/common/commontest"
	"github.com/cyoda-platform/cyoda-go/internal/domain/account"
)

func TestAdapter_AlgorithmEnum_Coverage(t *testing.T) {
	uc := &spi.UserContext{UserID: "u", UserName: "u", Tenant: spi.Tenant{ID: "t1"}, Roles: []string{"ROLE_ADMIN"}}
	rejected := []string{"RS384", "RS512", "PS256", "PS384", "PS512", "ES256", "ES384", "ES512", "EdDSA"}
	for _, alg := range rejected {
		alg := alg
		t.Run(alg, func(t *testing.T) {
			h := account.New(nil, nil, auth.NewInMemoryKeyStore(), auth.NewInMemoryTrustedKeyStore(), auth.DefaultIAMFeatures())
			body, _ := json.Marshal(genapi.IssueJwtKeyPairRequestDto{Algorithm: genapi.IssueJwtKeyPairRequestDtoAlgorithm(alg), Audience: "client"})
			req := httptest.NewRequest("POST", "/oauth/keys/keypair", bytes.NewReader(body))
			req = req.WithContext(spi.WithUserContext(req.Context(), uc))
			w := httptest.NewRecorder()
			h.IssueJwtKeyPair(w, req)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("%s: status=%d want 400", alg, w.Code)
			}
			commontest.ExpectErrorCode(t, w.Result(), "UNSUPPORTED_ALGORITHM")
		})
	}
}

func TestAdapter_AlgorithmEnum_RS256_HappyPath(t *testing.T) {
	uc := &spi.UserContext{UserID: "u", UserName: "u", Tenant: spi.Tenant{ID: "t1"}, Roles: []string{"ROLE_ADMIN"}}
	h := account.New(nil, nil, auth.NewInMemoryKeyStore(), auth.NewInMemoryTrustedKeyStore(), auth.DefaultIAMFeatures())
	body, _ := json.Marshal(genapi.IssueJwtKeyPairRequestDto{Algorithm: "RS256", Audience: "client"})
	req := httptest.NewRequest("POST", "/oauth/keys/keypair", bytes.NewReader(body))
	req = req.WithContext(spi.WithUserContext(req.Context(), uc))
	w := httptest.NewRecorder()
	h.IssueJwtKeyPair(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
}
