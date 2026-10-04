package account

import (
	"crypto/rand"
	"encoding/base32"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	genapi "github.com/cyoda-platform/cyoda-go/api"
	"github.com/cyoda-platform/cyoda-go/internal/auth"
	"github.com/cyoda-platform/cyoda-go/internal/common"
)

// clientIDLen is the number of base32-hex characters in a generated clientId.
// 16 chars × 5 bits/char = 80 bits of entropy. Comfortably inside the
// OpenAPI maxLength=100; short enough to copy-paste.
const clientIDLen = 16

// generateClientID returns a random 16-char uppercase base32-hex string.
// Uses crypto/rand; never reuses entropy.
func generateClientID() (string, error) {
	// base32-hex with no padding: 5 bits per char. ceil(16*5/8) = 10 bytes input.
	buf := make([]byte, 10)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	s := base32.HexEncoding.WithPadding(base32.NoPadding).EncodeToString(buf)
	s = strings.ToUpper(s)
	if len(s) < clientIDLen {
		// base32 output is deterministic on input length; this is belt-and-suspenders.
		return "", errors.New("base32 encoder produced unexpected length")
	}
	return s[:clientIDLen], nil
}

// requireM2MStore writes 501 NOT_IMPLEMENTED and returns false when the M2M
// store is not wired (mock IAM mode). All four /clients adapters call this.
func (h *Handler) requireM2MStore(w http.ResponseWriter, r *http.Request) bool {
	if h.m2mClientStore == nil {
		writeRequiresJWTMode(w, r, "M2M client management")
		return false
	}
	return true
}

// gateM2MAdminRole gates POST /clients?withAdminRole=true on the IAM feature
// flag. Writes 404 FEATURE_DISABLED when the flag is off; returns false.
func (h *Handler) gateM2MAdminRole(w http.ResponseWriter, r *http.Request) bool {
	if !h.iam.M2MAdminRoleEnabled {
		common.WriteError(w, r, common.Operational(http.StatusNotFound,
			common.ErrCodeFeatureDisabled, "M2M admin-role grants are disabled"))
		return false
	}
	return true
}

// validateClientID writes 400 BAD_REQUEST and returns false when the
// path-param clientId is outside the client-id grammar (auth.ValidClientID,
// the OpenAPI pattern ^[A-Za-z0-9][A-Za-z0-9._-]{0,99}$).
func validateClientID(w http.ResponseWriter, r *http.Request, clientID string) bool {
	if !auth.ValidClientID(clientID) {
		common.WriteError(w, r, common.Operational(http.StatusBadRequest,
			common.ErrCodeBadRequest, "invalid clientId"))
		return false
	}
	return true
}

// toTechnicalUserDto maps a store record to its OpenAPI wire shape.
// Deliberately reads only the public-facing fields — never touches HashedSecret.
func toTechnicalUserDto(c *auth.M2MClient) genapi.TechnicalUserDto {
	roles := make([]string, len(c.Roles))
	copy(roles, c.Roles)
	return genapi.TechnicalUserDto{
		ClientId:       c.ClientID,
		CreationDate:   c.CreatedAt,
		LastUpdateDate: c.UpdatedAt,
		Roles:          roles,
		OnBehalfOf:     c.OnBehalfOf,
	}
}

// grantTypeFor picks the grant_type a client's credentials DTO reports: the
// token-exchange URN for an on-behalf-of client (§3.2 — it may only perform
// token exchanges), client_credentials otherwise.
func grantTypeFor(onBehalfOf bool) genapi.TechnicalUserCredentialsDtoGrantType {
	if onBehalfOf {
		return genapi.TechnicalUserCredentialsDtoGrantTypeUrnIetfParamsOauthGrantTypeTokenExchange
	}
	return genapi.TechnicalUserCredentialsDtoGrantTypeClientCredentials
}

// toTechnicalUserCredentialsDto wraps a freshly-issued plaintext secret in
// the spec-conformant response shape. grant_type is the token-exchange URN
// for an on-behalf-of client and client_credentials otherwise (§3.2).
// expires_at=0 means "never expires".
func toTechnicalUserCredentialsDto(c *auth.M2MClient, plaintextSecret string) genapi.TechnicalUserCredentialsDto {
	rolesCopy := make([]string, len(c.Roles))
	copy(rolesCopy, c.Roles)
	return genapi.TechnicalUserCredentialsDto{
		ClientId:              c.ClientID,
		ClientSecret:          plaintextSecret,
		GrantType:             grantTypeFor(c.OnBehalfOf),
		ClientSecretExpiresAt: 0,
		Roles:                 rolesCopy,
		OnBehalfOf:            c.OnBehalfOf,
	}
}

// writeM2MClientError answers a Delete or ResetSecret failure. The store
// enforces tenant isolation, so an absent client and another tenant's client
// are the same ErrM2MClientNotFound and get the same 404 — no cross-tenant
// existence oracle. A reset with no free secret-check slot is 503
// SERVER_BUSY. Anything else is the store failing: 503 when it reports
// itself unavailable, otherwise 500 with a ticket.
func writeM2MClientError(w http.ResponseWriter, r *http.Request, op string, err error) {
	if errors.Is(err, auth.ErrM2MClientNotFound) {
		common.WriteError(w, r, common.Operational(http.StatusNotFound,
			common.ErrCodeM2MClientNotFound, "M2M client not found"))
		return
	}
	if writeSecretCheckBusy(w, r, err) {
		return
	}
	common.WriteError(w, r, common.Internal(op, err))
}

// writeSecretCheckBusy answers ErrSecretCheckBusy — no free slot to hash a
// new secret, nothing written — with a retryable 503 SERVER_BUSY and
// Retry-After: 1, and reports whether it did.
func writeSecretCheckBusy(w http.ResponseWriter, r *http.Request, err error) bool {
	if !errors.Is(err, auth.ErrSecretCheckBusy) {
		return false
	}
	w.Header().Set("Retry-After", "1")
	common.WriteError(w, r, common.Operational(http.StatusServiceUnavailable,
		common.ErrCodeServerBusy, "the node is busy and could not hash a client secret — retry").AsRetryable())
	return true
}

// CreateTechnicalUser implements POST /clients?withAdminRole=<bool>&onBehalfOf=<bool>.
// Generates a 16-char base32-hex clientId and returns the freshly issued
// plaintext secret exactly once.
func (h *Handler) CreateTechnicalUser(w http.ResponseWriter, r *http.Request, params genapi.CreateTechnicalUserParams) {
	if !auth.RequireAdmin(w, r) {
		return
	}
	if !h.requireM2MStore(w, r) {
		return
	}

	withAdmin := parseWithAdminRole(params.WithAdminRole)
	if withAdmin && !h.gateM2MAdminRole(w, r) {
		return
	}

	onBehalfOf := parseOnBehalfOf(params.OnBehalfOf)
	if withAdmin && onBehalfOf {
		common.WriteError(w, r, common.Operational(http.StatusBadRequest,
			common.ErrCodeBadRequest, "an on-behalf-of client cannot hold ROLE_ADMIN"))
		return
	}

	tID := tenantFromCtx(r)
	if onBehalfOf && tID == auth.PlatformTenantID {
		common.WriteError(w, r, common.Operational(http.StatusBadRequest,
			common.ErrCodeBadRequest, "the PLATFORM tenant cannot hold an on-behalf-of client"))
		return
	}

	roles := []string{"ROLE_M2M"}
	if withAdmin {
		roles = append(roles, "ROLE_ADMIN")
	}

	// Generate clientID and Create atomically — Store rejects collisions
	// via ErrM2MClientExists. Retry once on the astronomical (~1 in 2^80)
	// collision; a second collision is a defect (signals broken entropy
	// source) and returns 500.
	var clientID string
	var secret string
	for attempt := 0; attempt < 2; attempt++ {
		cid, err := generateClientID()
		if err != nil {
			common.WriteError(w, r, common.Internal("generateClientID", err))
			return
		}
		sec, createErr := h.m2mClientStore.Create(r.Context(), tID, cid, cid, roles, onBehalfOf)
		if createErr == nil {
			clientID = cid
			secret = sec
			break
		}
		if errors.Is(createErr, auth.ErrM2MClientExists) {
			// Astronomical-probability collision; loop once more.
			continue
		}
		if errors.Is(createErr, auth.ErrM2MClientCapReached) {
			common.WriteError(w, r, common.Operational(http.StatusBadRequest,
				common.ErrCodeM2MClientCapReached, "M2M client cap reached for tenant"))
			return
		}
		if writeSecretCheckBusy(w, r, createErr) {
			return
		}
		common.WriteError(w, r, common.Internal("m2mClientStore.Create", createErr))
		return
	}
	if clientID == "" {
		common.WriteError(w, r, common.Internal("generateClientID-collision",
			errors.New("clientId collision after retry")))
		return
	}

	slog.InfoContext(r.Context(), "M2M client created",
		"pkg", "account",
		"tenantId", tID,
		"clientId", clientID,
		"roles", roles,
		"onBehalfOf", onBehalfOf,
	)
	auth.SetNoStore(w.Header())
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(toTechnicalUserCredentialsDto(
		&auth.M2MClient{ClientID: clientID, Roles: roles, OnBehalfOf: onBehalfOf}, secret))
}

// parseWithAdminRole reads the *bool query param. nil/absent → false.
func parseWithAdminRole(p *bool) bool {
	if p == nil {
		return false
	}
	return *p
}

// parseOnBehalfOf reads the *bool query param. nil/absent → false.
func parseOnBehalfOf(p *bool) bool {
	if p == nil {
		return false
	}
	return *p
}

// DeleteTechnicalUser implements DELETE /clients/{clientId}.
func (h *Handler) DeleteTechnicalUser(w http.ResponseWriter, r *http.Request, clientID string) {
	if !auth.RequireAdmin(w, r) {
		return
	}
	if !h.requireM2MStore(w, r) {
		return
	}
	if !validateClientID(w, r, clientID) {
		return
	}

	tID := tenantFromCtx(r)
	if err := h.m2mClientStore.Delete(r.Context(), tID, clientID); err != nil {
		writeM2MClientError(w, r, "m2mClientStore.Delete", err)
		return
	}

	slog.InfoContext(r.Context(), "M2M client deleted",
		"pkg", "account",
		"tenantId", tID,
		"clientId", clientID,
	)
	resp := genapi.DeleteTechnicalUser200ResponseDto{
		Message:  "M2M client deleted successfully",
		ClientId: clientID,
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

// ResetTechnicalUserSecret implements PUT /clients/{clientId}/secret.
func (h *Handler) ResetTechnicalUserSecret(w http.ResponseWriter, r *http.Request, clientID string) {
	if !auth.RequireAdmin(w, r) {
		return
	}
	if !h.requireM2MStore(w, r) {
		return
	}
	if !validateClientID(w, r, clientID) {
		return
	}

	tID := tenantFromCtx(r)
	secret, client, err := h.m2mClientStore.ResetSecret(r.Context(), tID, clientID)
	if err != nil {
		writeM2MClientError(w, r, "m2mClientStore.ResetSecret", err)
		return
	}

	slog.InfoContext(r.Context(), "M2M client secret rotated",
		"pkg", "account",
		"tenantId", tID,
		"clientId", clientID,
	)
	auth.SetNoStore(w.Header())
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(toTechnicalUserCredentialsDto(client, secret))
}

// ListTechnicalUsers implements GET /clients.
func (h *Handler) ListTechnicalUsers(w http.ResponseWriter, r *http.Request) {
	if !auth.RequireAdmin(w, r) {
		return
	}
	if !h.requireM2MStore(w, r) {
		return
	}
	tID := tenantFromCtx(r)
	clients, err := h.m2mClientStore.List(r.Context(), tID)
	if err != nil {
		common.WriteError(w, r, common.Internal("m2mClientStore.List", err))
		return
	}
	out := make([]genapi.TechnicalUserDto, 0, len(clients))
	for _, c := range clients {
		out = append(out, toTechnicalUserDto(c))
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}
