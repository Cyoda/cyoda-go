package app

import (
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/cyoda-platform/cyoda-go/internal/auth"
)

// JWTSettings are the four variables a token signer needs. LoadJWTSettings
// resolves exactly these, so `cyoda token` does not depend on (or panic over)
// any other part of the configuration.
type JWTSettings struct {
	SigningKeyPEM string
	Issuer        string
	Audience      string
	ExpirySeconds int
}

// LoadJWTSettings resolves CYODA_JWT_SIGNING_KEY (or _FILE; PEM or
// base64-encoded PEM), CYODA_JWT_ISSUER (default "cyoda" when unset; an
// explicitly empty value is an error), CYODA_JWT_AUDIENCE
// (default empty) and CYODA_JWT_EXPIRY_SECONDS (default 300; an integer from
// 1 to auth.MaxJWTExpirySeconds). Errors name the variable, never its value. The
// server (DefaultConfig) and `cyoda token` both read these variables here, so
// they accept and refuse the same values.
func LoadJWTSettings() (JWTSettings, error) {
	pemText, err := resolvePEMSecretEnv("CYODA_JWT_SIGNING_KEY")
	if err != nil {
		return JWTSettings{}, err
	}
	expiry := 300
	if v, ok := os.LookupEnv("CYODA_JWT_EXPIRY_SECONDS"); ok && v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 || n > auth.MaxJWTExpirySeconds {
			return JWTSettings{}, fmt.Errorf("CYODA_JWT_EXPIRY_SECONDS must be an integer from 1 to %d", auth.MaxJWTExpirySeconds)
		}
		expiry = n
	}
	issuer := envString("CYODA_JWT_ISSUER", "cyoda")
	if issuer == "" {
		return JWTSettings{}, errors.New("CYODA_JWT_ISSUER must not be empty")
	}
	return JWTSettings{
		SigningKeyPEM: pemText,
		Issuer:        issuer,
		Audience:      envString("CYODA_JWT_AUDIENCE", ""),
		ExpirySeconds: expiry,
	}, nil
}

// resolvePEMSecretEnv is ResolveSecretEnv plus PEM normalisation: a value
// that does not start with "-----BEGIN" is decoded as base64 when it decodes.
func resolvePEMSecretEnv(name string) (string, error) {
	v, err := ResolveSecretEnv(name)
	if err != nil {
		return "", err
	}
	if v == "" || strings.HasPrefix(v, "-----BEGIN") {
		return v, nil
	}
	decoded, err := base64.StdEncoding.DecodeString(v)
	if err != nil {
		return v, nil
	}
	return string(decoded), nil
}
