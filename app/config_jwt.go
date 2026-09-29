package app

import (
	"encoding/base64"
	"fmt"
	"os"
	"strconv"
	"strings"
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
// base64-encoded PEM), CYODA_JWT_ISSUER (default "cyoda"), CYODA_JWT_AUDIENCE
// (default empty) and CYODA_JWT_EXPIRY_SECONDS (default 3600). Errors name the
// variable, never its value.
func LoadJWTSettings() (JWTSettings, error) {
	pemText, err := resolvePEMSecretEnv("CYODA_JWT_SIGNING_KEY")
	if err != nil {
		return JWTSettings{}, err
	}
	expiry := 3600
	if v, ok := os.LookupEnv("CYODA_JWT_EXPIRY_SECONDS"); ok && v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			return JWTSettings{}, fmt.Errorf("CYODA_JWT_EXPIRY_SECONDS must be a positive integer")
		}
		expiry = n
	}
	return JWTSettings{
		SigningKeyPEM: pemText,
		Issuer:        envString("CYODA_JWT_ISSUER", "cyoda"),
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
