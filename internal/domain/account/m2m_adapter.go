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
		common.WriteError(w, r, common.Operational(http.StatusNotImplemented,
			common.ErrCodeNotImplemented, "M2M client management requires JWT IAM mode"))
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
// the OpenAPI pattern ^[A-Za-z0-9]{1,100}$).
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
	}
}

// toTechnicalUserCredentialsDto wraps a freshly-issued plaintext secret in
// the spec-conformant response shape. The grant_type is always
// "client_credentials" per RFC 7591 §3.2.1. expires_at=0 means "never expires".
func toTechnicalUserCredentialsDto(clientID, plaintextSecret string, roles []string) genapi.TechnicalUserCredentialsDto {
	rolesCopy := make([]string, len(roles))
	copy(rolesCopy, roles)
	return genapi.TechnicalUserCredentialsDto{
		ClientId:              clientID,
		ClientSecret:          plaintextSecret,
		GrantType:             genapi.TechnicalUserCredentialsDtoGrantType("client_credentials"),
		ClientSecretExpiresAt: 0,
		Roles:                 rolesCopy,
	}
}

// writeM2MClientError answers a Delete or ResetSecret failure. The store
// enforces tenant isolation, so an absent client and another tenant's client
// are the same ErrM2MClientNotFound and get the same 404 — no cross-tenant
// existence oracle. Anything else is the store failing: 503 when it reports
// itself unavailable, otherwise 500 with a ticket.
func writeM2MClientError(w http.ResponseWriter, r *http.Request, op string, err error) {
	if errors.Is(err, auth.ErrM2MClientNotFound) {
		common.WriteError(w, r, common.Operational(http.StatusNotFound,
			common.ErrCodeM2MClientNotFound, "M2M client not found"))
		return
	}
	common.WriteError(w, r, common.Internal(op, err))
}

// CreateTechnicalUser implements POST /clients?withAdminRole=<bool>.
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

	tID := tenantFromCtx(r)
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
		sec, createErr := h.m2mClientStore.Create(r.Context(), tID, cid, cid, roles)
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
	)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(toTechnicalUserCredentialsDto(clientID, secret, roles))
}

// parseWithAdminRole reads the *bool query param. nil/absent → false.
func parseWithAdminRole(p *bool) bool {
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
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(toTechnicalUserCredentialsDto(clientID, secret, client.Roles))
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
