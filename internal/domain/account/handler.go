package account

import (
	"net/http"

	openapi_types "github.com/oapi-codegen/runtime/types"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	genapi "github.com/cyoda-platform/cyoda-go/api"
	"github.com/cyoda-platform/cyoda-go/internal/auth"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/contract"
)

type Handler struct {
	authSvc         contract.AuthenticationService
	authzSvc        contract.AuthorizationService
	keyStore        auth.KeyStore
	trustedKeyStore auth.TrustedKeyStore
	m2mClientStore  auth.M2MClientStore
	iam             auth.IAMFeatures
}

func New(authSvc contract.AuthenticationService, authzSvc contract.AuthorizationService,
	keyStore auth.KeyStore, trustedKeyStore auth.TrustedKeyStore, m2mClientStore auth.M2MClientStore,
	iam auth.IAMFeatures) *Handler {
	return &Handler{
		authSvc:         authSvc,
		authzSvc:        authzSvc,
		keyStore:        keyStore,
		trustedKeyStore: trustedKeyStore,
		m2mClientStore:  m2mClientStore,
		iam:             iam,
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

func (h *Handler) ListTechnicalUsers(w http.ResponseWriter, r *http.Request) {
	h.stub(w, r)
}

func (h *Handler) CreateTechnicalUser(w http.ResponseWriter, r *http.Request, params genapi.CreateTechnicalUserParams) {
	h.stub(w, r)
}

func (h *Handler) DeleteTechnicalUser(w http.ResponseWriter, r *http.Request, clientId string) {
	h.stub(w, r)
}

func (h *Handler) ResetTechnicalUserSecret(w http.ResponseWriter, r *http.Request, clientId string) {
	h.stub(w, r)
}

func (h *Handler) GetTechnicalUserToken(w http.ResponseWriter, r *http.Request, params genapi.GetTechnicalUserTokenParams) {
	h.stub(w, r)
}

func (h *Handler) ListOidcProviders(w http.ResponseWriter, r *http.Request, params genapi.ListOidcProvidersParams) {
	h.stub(w, r)
}

func (h *Handler) RegisterOidcProvider(w http.ResponseWriter, r *http.Request) {
	h.stub(w, r)
}

func (h *Handler) ReloadOidcProviders(w http.ResponseWriter, r *http.Request) {
	h.stub(w, r)
}

func (h *Handler) DeleteOidcProvider(w http.ResponseWriter, r *http.Request, id openapi_types.UUID) {
	h.stub(w, r)
}

func (h *Handler) UpdateOidcProvider(w http.ResponseWriter, r *http.Request, id openapi_types.UUID) {
	h.stub(w, r)
}

func (h *Handler) InvalidateOidcProvider(w http.ResponseWriter, r *http.Request, id openapi_types.UUID) {
	h.stub(w, r)
}

func (h *Handler) ReactivateOidcProvider(w http.ResponseWriter, r *http.Request, id openapi_types.UUID) {
	h.stub(w, r)
}
