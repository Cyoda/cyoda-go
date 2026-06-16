package account

import (
	"crypto/rand"
	"encoding/base32"
	"errors"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	genapi "github.com/cyoda-platform/cyoda-go/api"
	"github.com/cyoda-platform/cyoda-go/internal/auth"
	"github.com/cyoda-platform/cyoda-go/internal/common"
)

// clientIDPattern enforces the OpenAPI schema for path-param + generated
// clientId: ^[A-Za-z0-9]+$, length 1..100.
var clientIDPattern = regexp.MustCompile(`^[A-Za-z0-9]{1,100}$`)

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
// path-param clientId is empty or violates the OpenAPI pattern.
func validateClientID(w http.ResponseWriter, r *http.Request, clientID string) bool {
	if clientID == "" || !clientIDPattern.MatchString(clientID) {
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

// clientBelongsToTenant returns true iff the store record's TenantID matches
// the caller's tenant. After Task 2 promoted M2MClient.TenantID to spi.TenantID,
// the comparison is type-direct.
func clientBelongsToTenant(c *auth.M2MClient, callerTenant spi.TenantID) bool {
	return c.TenantID == callerTenant
}

// Placeholders consume slog + time so the import block stays stable until
// the operation methods (Tasks 6-9) reference them directly.
var _ = slog.LevelInfo
var _ = time.Now
