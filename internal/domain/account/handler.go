package account

import (
	"errors"
	"log/slog"
	"net/http"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	genapi "github.com/cyoda-platform/cyoda-go/api"
	"github.com/cyoda-platform/cyoda-go/internal/auth"
	"github.com/cyoda-platform/cyoda-go/internal/common"
)

type Handler struct {
	keyStore        auth.KeyStore
	trustedKeyStore auth.TrustedKeyStore
	m2mClientStore  auth.M2MClientStore
	iam             auth.IAMFeatures
	operator        auth.OperatorGuard
}

func New(keyStore auth.KeyStore, trustedKeyStore auth.TrustedKeyStore, m2mClientStore auth.M2MClientStore,
	iam auth.IAMFeatures, operator auth.OperatorGuard) *Handler {
	return &Handler{
		keyStore:        keyStore,
		trustedKeyStore: trustedKeyStore,
		m2mClientStore:  m2mClientStore,
		iam:             iam,
		operator:        operator,
	}
}

func (h *Handler) stub(w http.ResponseWriter, r *http.Request) {
	common.WriteError(w, r, common.Operational(http.StatusNotImplemented, common.ErrCodeNotImplemented, "not yet implemented"))
}

func (h *Handler) AccountGet(w http.ResponseWriter, r *http.Request) {
	uc := spi.GetUserContext(r.Context())
	if uc == nil {
		common.WriteError(w, r, common.Operational(http.StatusUnauthorized, common.ErrCodeUnauthorized, "not authenticated"))
		return
	}

	roles := make([]map[string]string, len(uc.Roles))
	for i, role := range uc.Roles {
		roles[i] = map[string]string{"id": role}
	}

	resp := map[string]any{
		"userAccountInfo": map[string]any{
			"userId":   uc.UserID,
			"userName": uc.UserName,
			"legalEntity": map[string]string{
				"id":   string(uc.Tenant.ID),
				"name": uc.Tenant.Name,
			},
			"roles": roles,
			"currentSubscription": map[string]any{
				"id":            "unlimited",
				"legalEntityId": string(uc.Tenant.ID),
				"status":        "ACTIVE",
				"tierName":      "unlimited",
				"periodFrom":    "2020-01-01T00:00:00Z",
				"limits":        []any{},
			},
		},
	}
	common.WriteJSON(w, http.StatusOK, resp)
}

func (h *Handler) AccountSubscriptionsGet(w http.ResponseWriter, r *http.Request) {
	h.stub(w, r)
}

// GetTechnicalUserToken — defensive interface-satisfaction stub for
// POST /oauth/token. The real handler is the auth-service token handler
// mounted on the public mux at app/app.go (the /oauth/token entry, every
// method), which intercepts before the chi router can reach this method. Arriving
// here means a routing regression — log + 500.
func (h *Handler) GetTechnicalUserToken(w http.ResponseWriter, r *http.Request, params genapi.GetTechnicalUserTokenParams) {
	slog.WarnContext(r.Context(),
		"chi /oauth/token reached — should be intercepted by public mux; routing regression?",
		"method", r.Method, "path", r.URL.Path)
	common.WriteError(w, r,
		common.Internal("getTechnicalUserToken-unreachable",
			errors.New("routing regression: chi served POST /oauth/token")))
}
