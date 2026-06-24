package oidc

import (
	"crypto/sha256"
	"encoding/hex"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// Namespace is the single KV namespace per D2.
const Namespace = "oidc-providers"

// providerBlobKey returns "<tenantID>:<provider-uuid>".
func providerBlobKey(t spi.TenantID, providerID string) string {
	return string(t) + ":" + providerID
}

// uriIndexKey returns "<tenantID>:uri:<sha256-hex>".
func uriIndexKey(t spi.TenantID, uri string) string {
	return string(t) + ":uri:" + sha256Hex(uri)
}

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}
