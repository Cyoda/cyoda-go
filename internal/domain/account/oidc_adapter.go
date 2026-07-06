package account

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/google/uuid"
	openapi_types "github.com/oapi-codegen/runtime/types"

	genapi "github.com/cyoda-platform/cyoda-go/api"
	"github.com/cyoda-platform/cyoda-go/internal/auth"
	"github.com/cyoda-platform/cyoda-go/internal/auth/oidc"
	"github.com/cyoda-platform/cyoda-go/internal/common"
)

const (
	// oidcURIMaxLen is the maximum length for wellKnownConfigUri (spec §5.1 step 2).
	oidcURIMaxLen = 1000
	// oidcStringMaxLen is the maximum length for issuer/audience entries.
	oidcStringMaxLen = 1000
	// oidcListMaxItems is the maximum number of issuers or expectedAudiences.
	oidcListMaxItems = 10
	// oidcRolesClaimMaxLen is the maximum length for the rolesClaim field.
	oidcRolesClaimMaxLen = 100
)

// oidcAdapter maps the 7 generated OIDC handler signatures to calls into
// oidc.Service. It is installed on *Handler via WithOIDCAdapter.
type oidcAdapter struct {
	service           *oidc.Service
	defaultRolesClaim string
	requireHTTPS      bool
	allowPrivate      bool
}

// newOidcAdapter constructs an oidcAdapter. requireHTTPS and allowPrivate come
// from CYODA_OIDC_REQUIRE_HTTPS and CYODA_OIDC_ALLOW_PRIVATE_NETWORKS.
func newOidcAdapter(service *oidc.Service, defaultRolesClaim string, requireHTTPS, allowPrivate bool) *oidcAdapter {
	return &oidcAdapter{
		service:           service,
		defaultRolesClaim: defaultRolesClaim,
		requireHTTPS:      requireHTTPS,
		allowPrivate:      allowPrivate,
	}
}

// OidcAdapter is the cross-package handle returned by NewOidcAdapter. Internal
// methods delegate through to the unexported adapter type. This pattern keeps
// the adapter's implementation details unexported while allowing app-level
// wiring.
type OidcAdapter struct {
	adapter *oidcAdapter
}

// NewOidcAdapter constructs the OIDC HTTP adapter for the account handler.
// Callers (typically app/app.go bootstrap) build this once at startup and
// pass it to Handler via WithOIDCAdapter.
//
// `service` is the OIDC service (lifecycle ops). `defaultRolesClaim` is the
// global default for the roles claim name (per-provider override exists).
// `requireHTTPS` and `allowPrivate` come from CYODA_OIDC_REQUIRE_HTTPS and
// CYODA_OIDC_ALLOW_PRIVATE_NETWORKS respectively.
func NewOidcAdapter(service *oidc.Service, defaultRolesClaim string, requireHTTPS, allowPrivate bool) *OidcAdapter {
	return &OidcAdapter{adapter: newOidcAdapter(service, defaultRolesClaim, requireHTTPS, allowPrivate)}
}

// RegisterOidcProvider implements POST /oauth/oidc/providers. ROLE_ADMIN required.
func (a *oidcAdapter) RegisterOidcProvider(w http.ResponseWriter, r *http.Request) {
	if !auth.RequireAdmin(w, r) {
		return
	}

	// Decode via map[string]json.RawMessage so we can apply the D24 tri-state
	// logic to optional fields even on registration. Issuers is optional on
	// register (may be absent, null, or a non-empty array).
	var raw map[string]json.RawMessage
	if err := boundedJSONDecode(w, r, 1<<20, &raw); err != nil {
		common.WriteError(w, r, common.Operational(http.StatusBadRequest, common.ErrCodeBadRequest, "invalid request body"))
		return
	}

	// wellKnownConfigUri is required.
	uriRaw, ok := raw["wellKnownConfigUri"]
	if !ok {
		common.WriteError(w, r, common.Operational(http.StatusBadRequest, common.ErrCodeBadRequest, "wellKnownConfigUri is required"))
		return
	}
	var wellKnown string
	if err := json.Unmarshal(uriRaw, &wellKnown); err != nil || wellKnown == "" {
		common.WriteError(w, r, common.Operational(http.StatusBadRequest, common.ErrCodeBadRequest, "wellKnownConfigUri is required and must be a non-empty string"))
		return
	}
	if len(wellKnown) > oidcURIMaxLen {
		common.WriteError(w, r, common.Operational(http.StatusBadRequest, common.ErrCodeBadRequest,
			fmt.Sprintf("wellKnownConfigUri must be <= %d characters", oidcURIMaxLen)))
		return
	}

	// SSRF + scheme validation.
	if err := oidc.ValidateRegisterURI(wellKnown, a.requireHTTPS, a.allowPrivate); err != nil {
		if errors.Is(err, oidc.ErrSSRFBlocked) {
			common.WriteError(w, r, common.Operational(http.StatusBadRequest, common.ErrCodeOIDCSSRFBlocked, "wellKnownConfigUri resolves to a blocked address range"))
			return
		}
		common.WriteError(w, r, common.Operational(http.StatusBadRequest, common.ErrCodeBadRequest, err.Error()))
		return
	}

	// Optional: issuers.
	issuers, _, err := decodeOptionalStringSlice(raw, "issuers")
	if err != nil {
		common.WriteError(w, r, common.Operational(http.StatusBadRequest, common.ErrCodeBadRequest, "invalid issuers field"))
		return
	}
	if err := validateStringList("issuers", issuers, oidcListMaxItems, oidcStringMaxLen); err != nil {
		common.WriteError(w, r, common.Operational(http.StatusBadRequest, common.ErrCodeBadRequest, err.Error()))
		return
	}

	// Optional: expectedAudiences.
	audiences, _, err := decodeOptionalStringSlice(raw, "expectedAudiences")
	if err != nil {
		common.WriteError(w, r, common.Operational(http.StatusBadRequest, common.ErrCodeBadRequest, "invalid expectedAudiences field"))
		return
	}
	if err := validateStringList("expectedAudiences", audiences, oidcListMaxItems, oidcStringMaxLen); err != nil {
		common.WriteError(w, r, common.Operational(http.StatusBadRequest, common.ErrCodeBadRequest, err.Error()))
		return
	}

	// Optional: rolesClaim.
	rolesClaim, _, err := decodeOptionalString(raw, "rolesClaim")
	if err != nil {
		common.WriteError(w, r, common.Operational(http.StatusBadRequest, common.ErrCodeBadRequest, "invalid rolesClaim field"))
		return
	}
	if rolesClaim != nil && len(*rolesClaim) > oidcRolesClaimMaxLen {
		common.WriteError(w, r, common.Operational(http.StatusBadRequest, common.ErrCodeBadRequest,
			fmt.Sprintf("rolesClaim must be <= %d characters", oidcRolesClaimMaxLen)))
		return
	}

	tenantID := tenantFromCtx(r)

	// OwnerLegalEntityID: the data model types this field as uuid.UUID (matching
	// cyoda-cloud's JWKOIDCEntity.ownerLegalEntityId). Non-UUID tenant IDs (e.g.
	// the bootstrap convenience string "default-tenant") must be rejected — silent
	// coercion to uuid.Nil would collide in KV storage across all non-UUID tenants
	// and produce a synthetic "nil tenant" identity at token-validation time.
	ownerID, parseErr := uuid.Parse(string(tenantID))
	if parseErr != nil {
		common.WriteError(w, r, common.Operational(
			http.StatusBadRequest,
			common.ErrCodeOidcInvalidTenant,
			"oidc provider registration requires a uuid-shaped tenant identifier; bootstrap deployments using the literal 'default-tenant' string must migrate to real tenant uuids",
		))
		return
	}

	in := oidc.RegisterInput{
		TenantID:           tenantID,
		WellKnownConfigURI: wellKnown,
		Issuers:            issuers,
		ExpectedAudiences:  audiences,
		RolesClaim:         rolesClaim,
		OwnerLegalEntityID: ownerID,
	}

	p, err := a.service.Register(r.Context(), in)
	if err != nil {
		if errors.Is(err, oidc.ErrProviderDuplicate) {
			common.WriteError(w, r, common.Operational(http.StatusConflict, common.ErrCodeOIDCProviderDuplicate,
				"provider with this wellKnownConfigUri already registered for this tenant"))
			return
		}
		if errors.Is(err, oidc.ErrSSRFBlocked) {
			common.WriteError(w, r, common.Operational(http.StatusBadRequest, common.ErrCodeOIDCSSRFBlocked,
				"wellKnownConfigUri resolves to a blocked address range"))
			return
		}
		common.WriteError(w, r, common.Internal("oidc.service.Register", err))
		return
	}

	slog.InfoContext(r.Context(), "oidc provider registered",
		"pkg", "account",
		"tenantId", string(tenantID),
		"providerUuid", p.ID.String(),
	)
	common.WriteJSON(w, http.StatusOK, toOidcProviderResponseDto(p))
}

// ListOidcProviders implements GET /oauth/oidc/providers. D21: any authenticated
// tenant member (not ROLE_ADMIN restricted).
func (a *oidcAdapter) ListOidcProviders(w http.ResponseWriter, r *http.Request, params genapi.ListOidcProvidersParams) {
	uc := spi.GetUserContext(r.Context())
	if uc == nil {
		common.WriteError(w, r, common.Operational(http.StatusUnauthorized, common.ErrCodeUnauthorized, "not authenticated"))
		return
	}

	tenantID := spi.TenantID(uc.Tenant.ID)

	activeOnly := params.ActiveOnly != nil && *params.ActiveOnly

	providers, err := a.service.ListByTenant(r.Context(), tenantID, activeOnly)
	if err != nil {
		common.WriteError(w, r, common.Internal("oidc.service.ListByTenant", err))
		return
	}

	out := make([]genapi.OidcProviderResponseDto, 0, len(providers))
	for _, p := range providers {
		out = append(out, toOidcProviderResponseDto(p))
	}
	common.WriteJSON(w, http.StatusOK, out)
}

// UpdateOidcProvider implements PATCH /oauth/oidc/providers/{id}. ROLE_ADMIN required.
// D24 tri-state decode: absent / null / value for issuers, expectedAudiences, rolesClaim.
func (a *oidcAdapter) UpdateOidcProvider(w http.ResponseWriter, r *http.Request, id openapi_types.UUID) {
	if !auth.RequireAdmin(w, r) {
		return
	}

	var raw map[string]json.RawMessage
	if err := boundedJSONDecode(w, r, 1<<20, &raw); err != nil {
		common.WriteError(w, r, common.Operational(http.StatusBadRequest, common.ErrCodeBadRequest, "invalid request body"))
		return
	}

	in := oidc.UpdateInput{
		TenantID:   tenantFromCtx(r),
		ProviderID: id.String(),
	}

	// issuers tri-state.
	issuers, issuersPresent, err := decodeOptionalStringSlice(raw, "issuers")
	if err != nil {
		common.WriteError(w, r, common.Operational(http.StatusBadRequest, common.ErrCodeBadRequest, "invalid issuers field"))
		return
	}
	if issuersPresent {
		if err := validateStringList("issuers", issuers, oidcListMaxItems, oidcStringMaxLen); err != nil {
			common.WriteError(w, r, common.Operational(http.StatusBadRequest, common.ErrCodeBadRequest, err.Error()))
			return
		}
		in.UpdateIssuers = true
		in.Issuers = issuers
	}

	// expectedAudiences tri-state.
	audiences, audiencesPresent, err := decodeOptionalStringSlice(raw, "expectedAudiences")
	if err != nil {
		common.WriteError(w, r, common.Operational(http.StatusBadRequest, common.ErrCodeBadRequest, "invalid expectedAudiences field"))
		return
	}
	if audiencesPresent {
		if err := validateStringList("expectedAudiences", audiences, oidcListMaxItems, oidcStringMaxLen); err != nil {
			common.WriteError(w, r, common.Operational(http.StatusBadRequest, common.ErrCodeBadRequest, err.Error()))
			return
		}
		in.UpdateAudiences = true
		in.ExpectedAudiences = audiences
	}

	// rolesClaim tri-state.
	rolesClaim, rolesClaimPresent, err := decodeOptionalString(raw, "rolesClaim")
	if err != nil {
		common.WriteError(w, r, common.Operational(http.StatusBadRequest, common.ErrCodeBadRequest, "invalid rolesClaim field"))
		return
	}
	if rolesClaimPresent {
		if rolesClaim != nil && len(*rolesClaim) > oidcRolesClaimMaxLen {
			common.WriteError(w, r, common.Operational(http.StatusBadRequest, common.ErrCodeBadRequest,
				fmt.Sprintf("rolesClaim must be <= %d characters", oidcRolesClaimMaxLen)))
			return
		}
		in.UpdateRolesClaim = true
		in.RolesClaim = rolesClaim
	}

	p, err := a.service.Update(r.Context(), in)
	if err != nil {
		if errors.Is(err, oidc.ErrProviderNotFound) {
			common.WriteError(w, r, common.Operational(http.StatusNotFound, common.ErrCodeOIDCProviderNotFound, "OIDC provider not found"))
			return
		}
		if errors.Is(err, oidc.ErrProviderInactive) {
			common.WriteError(w, r, common.Operational(http.StatusConflict, common.ErrCodeOIDCProviderInactive, "OIDC provider is invalidated"))
			return
		}
		common.WriteError(w, r, common.Internal("oidc.service.Update", err))
		return
	}

	common.WriteJSON(w, http.StatusOK, toOidcProviderResponseDto(p))
}

// InvalidateOidcProvider implements POST /oauth/oidc/providers/{id}/invalidate. ROLE_ADMIN required.
func (a *oidcAdapter) InvalidateOidcProvider(w http.ResponseWriter, r *http.Request, id openapi_types.UUID) {
	if !auth.RequireAdmin(w, r) {
		return
	}

	tenantID := tenantFromCtx(r)
	if err := a.service.Invalidate(r.Context(), tenantID, id.String()); err != nil {
		if errors.Is(err, oidc.ErrProviderNotFound) {
			common.WriteError(w, r, common.Operational(http.StatusNotFound, common.ErrCodeOIDCProviderNotFound, "OIDC provider not found"))
			return
		}
		common.WriteError(w, r, common.Internal("oidc.service.Invalidate", err))
		return
	}

	w.WriteHeader(http.StatusOK)
}

// ReactivateOidcProvider implements POST /oauth/oidc/providers/{id}/reactivate. ROLE_ADMIN required.
func (a *oidcAdapter) ReactivateOidcProvider(w http.ResponseWriter, r *http.Request, id openapi_types.UUID) {
	if !auth.RequireAdmin(w, r) {
		return
	}

	var req genapi.ReactivateOidcProviderRequestDto
	if r.ContentLength != 0 {
		if err := boundedJSONDecode(w, r, 1<<20, &req); err != nil {
			common.WriteError(w, r, common.Operational(http.StatusBadRequest, common.ErrCodeBadRequest, "invalid request body"))
			return
		}
	}

	// Default ReactivateKeys=true when absent.
	reactivateKeys := true
	if req.ReactivateKeys != nil {
		reactivateKeys = *req.ReactivateKeys
	}

	in := oidc.ReactivateInput{
		TenantID:       tenantFromCtx(r),
		ProviderID:     id.String(),
		ReactivateKeys: reactivateKeys,
	}

	p, err := a.service.Reactivate(r.Context(), in)
	if err != nil {
		if errors.Is(err, oidc.ErrProviderNotFound) {
			common.WriteError(w, r, common.Operational(http.StatusNotFound, common.ErrCodeOIDCProviderNotFound, "OIDC provider not found"))
			return
		}
		common.WriteError(w, r, common.Internal("oidc.service.Reactivate", err))
		return
	}

	common.WriteJSON(w, http.StatusOK, toOidcProviderResponseDto(p))
}

// DeleteOidcProvider implements DELETE /oauth/oidc/providers/{id}. ROLE_ADMIN required.
func (a *oidcAdapter) DeleteOidcProvider(w http.ResponseWriter, r *http.Request, id openapi_types.UUID) {
	if !auth.RequireAdmin(w, r) {
		return
	}

	tenantID := tenantFromCtx(r)
	if err := a.service.Delete(r.Context(), tenantID, id.String()); err != nil {
		if errors.Is(err, oidc.ErrProviderNotFound) {
			common.WriteError(w, r, common.Operational(http.StatusNotFound, common.ErrCodeOIDCProviderNotFound, "OIDC provider not found"))
			return
		}
		common.WriteError(w, r, common.Internal("oidc.service.Delete", err))
		return
	}

	w.WriteHeader(http.StatusOK)
}

// ReloadOidcProviders implements POST /oauth/oidc/providers/reload. ROLE_ADMIN required per §5.6.
func (a *oidcAdapter) ReloadOidcProviders(w http.ResponseWriter, r *http.Request) {
	if !auth.RequireAdmin(w, r) {
		return
	}

	if err := a.service.ReloadAll(r.Context()); err != nil {
		common.WriteError(w, r, common.Internal("oidc.service.ReloadAll", err))
		return
	}

	w.WriteHeader(http.StatusOK)
}

// toOidcProviderResponseDto maps a domain OidcProvider to the wire DTO.
// Every field declared in the OpenAPI OidcProviderResponseDto schema is
// populated. Slice fields are defensively copied so the response is
// independent of the caller's view of the domain object.
func toOidcProviderResponseDto(p *oidc.OidcProvider) genapi.OidcProviderResponseDto {
	dto := genapi.OidcProviderResponseDto{
		Id:                 p.ID,
		WellKnownConfigUri: p.WellKnownConfigURI,
		Active:             p.Active(),
		CreatedAt:          p.CreatedAt,
	}
	if len(p.Issuers) > 0 {
		issuers := make([]string, len(p.Issuers))
		copy(issuers, p.Issuers)
		dto.Issuers = &issuers
	}
	if len(p.ExpectedAudiences) > 0 {
		audiences := make([]string, len(p.ExpectedAudiences))
		copy(audiences, p.ExpectedAudiences)
		dto.ExpectedAudiences = &audiences
	}
	if p.RolesClaim != nil {
		s := *p.RolesClaim
		dto.RolesClaim = &s
	}
	return dto
}

// validateStringList checks that list does not exceed maxItems entries, that
// no entry is empty, and that no entry exceeds maxLen characters.
func validateStringList(field string, list []string, maxItems, maxLen int) error {
	if len(list) > maxItems {
		return fmt.Errorf("%s must have at most %d entries", field, maxItems)
	}
	for i, s := range list {
		if s == "" {
			return fmt.Errorf("%s[%d]: must not be empty", field, i)
		}
		if len(s) > maxLen {
			return fmt.Errorf("%s[%d]: too long (got %d chars, max %d)", field, i, len(s), maxLen)
		}
	}
	return nil
}

// ---------- D24 tri-state decoder helpers ----------

// decodeOptionalStringSlice implements PATCH tri-state per D24.
// Returns (value, present, err) where:
//   - present=false → field absent in PATCH body; caller leaves existing field unchanged.
//   - present=true, value=nil → field was null OR empty array (both clear the field).
//   - present=true, value=[...] → field set to the non-empty list.
func decodeOptionalStringSlice(raw map[string]json.RawMessage, field string) ([]string, bool, error) {
	rm, ok := raw[field]
	if !ok {
		return nil, false, nil
	}
	if bytes.Equal(bytes.TrimSpace(rm), []byte("null")) {
		return nil, true, nil
	}
	var arr []string
	if err := json.Unmarshal(rm, &arr); err != nil {
		return nil, true, err
	}
	if len(arr) == 0 {
		return nil, true, nil
	}
	return arr, true, nil
}

// decodeOptionalString handles rolesClaim tri-state: absent / null / non-empty string.
// Returns (value, present, err) where:
//   - present=false → field absent; caller leaves existing field unchanged.
//   - present=true, value=nil → field was null (revert to defaultRolesClaim).
//   - present=true, value=&s → field set to s.
func decodeOptionalString(raw map[string]json.RawMessage, field string) (*string, bool, error) {
	rm, ok := raw[field]
	if !ok {
		return nil, false, nil
	}
	if bytes.Equal(bytes.TrimSpace(rm), []byte("null")) {
		return nil, true, nil
	}
	var s string
	if err := json.Unmarshal(rm, &s); err != nil {
		return nil, true, err
	}
	return &s, true, nil
}
