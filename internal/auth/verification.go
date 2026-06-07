package auth

import "fmt"

// getTrustedKeyByKID resolves a trusted key by kid without tenant scoping.
// Used exclusively by token.go's token-exchange / JWT-bearer-assertion grant.
// Iterates ListForVerification() so lazy ValidTo filter applies; past-ValidTo
// entries are excluded. Bypasses tenant scoping by design — caller (token.go)
// enforces principal-tenant invariant at the existing client.TenantID == subOrgID
// check (spec §7.1).
func getTrustedKeyByKID(store TrustedKeyStore, kid string) (*TrustedKey, error) {
	for _, tk := range store.ListForVerification() {
		if tk.KID == kid {
			return tk, nil
		}
	}
	return nil, fmt.Errorf("trusted key not found")
}
