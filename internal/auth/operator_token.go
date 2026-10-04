package auth

import (
	"context"
	"crypto/rsa"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/common"
)

// OperatorTokenRequest is what `cyoda token` asks for. The token is a person
// token (user_roles), signed by the signing key from configuration.
type OperatorTokenRequest struct {
	Tenant   spi.TenantID
	UserID   string
	Roles    []string
	TTL      time.Duration
	Issuer   string
	Audience string    // empty: no aud claim
	Now      time.Time // zero: time.Now()
}

// ValidateOperatorTokenRequest checks everything MintOperatorToken checks
// except the key, so `cyoda token` can report a flag error before it reads
// the key.
func ValidateOperatorTokenRequest(req OperatorTokenRequest) error {
	if err := common.ValidateAPITenantID(req.Tenant); err != nil {
		return fmt.Errorf("tenant: %w", err)
	}
	if err := common.ValidateUserID(req.UserID); err != nil {
		return fmt.Errorf("user: %w", err)
	}
	if len(req.Roles) == 0 {
		return errors.New("at least one role is required")
	}
	for _, r := range req.Roles {
		if r == "" {
			return errors.New("a role is empty")
		}
	}
	// iat and exp are whole seconds, so a sub-second ttl would give a token
	// that has expired when it is issued.
	if req.TTL < time.Second {
		return errors.New("ttl must be at least 1s")
	}
	if req.Issuer == "" {
		return errors.New("issuer is required")
	}
	return nil
}

// MintOperatorToken signs an operator token with key. It adds no capability:
// whoever holds key can already sign any token the validator accepts. The
// token verifies only while key verifies on the cluster.
func MintOperatorToken(ctx context.Context, key *rsa.PrivateKey, req OperatorTokenRequest) (string, error) {
	if key == nil {
		return "", errors.New("no signing key")
	}
	if err := ValidateOperatorTokenRequest(req); err != nil {
		return "", err
	}
	kid, err := DeriveKID(&key.PublicKey)
	if err != nil {
		return "", err
	}
	now := req.Now
	if now.IsZero() {
		now = time.Now()
	}
	roles := append([]string(nil), req.Roles...)
	claims := map[string]any{
		"sub":          req.UserID,
		"iss":          req.Issuer,
		"caas_user_id": req.UserID,
		"caas_org_id":  string(req.Tenant),
		"user_roles":   roles,
		"iat":          now.Unix(),
		"exp":          now.Add(req.TTL).Unix(),
		"jti":          uuid.NewString(),
	}
	if req.Audience != "" {
		claims["aud"] = req.Audience
	}
	return Sign(ctx, claims, NewRSASigner(key), kid)
}
