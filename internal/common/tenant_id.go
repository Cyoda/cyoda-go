package common

import (
	"errors"
	"fmt"
	"strings"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// MaxTenantIDLen bounds a tenant id at 100 bytes. The figure is not arbitrary:
// it matches the only length Cyoda Cloud declares on anything tenant-shaped,
// @ColumnValidation(maxLength = 100) on CSUser.legalEntityId, which Cloud's
// auto-enrollment already writes the caas_org_id claim into. A longer tenant
// works here and fails to persist a user there; capping at the same figure
// keeps the two tiers telling the caller the same thing.
const MaxTenantIDLen = 100

// ErrInvalidTenantID reports a tenant id outside the accepted grammar.
var ErrInvalidTenantID = errors.New("invalid tenant id")

// ValidateTenantID reports whether id is a tenant identifier cyoda-go admits:
//
//	^[A-Za-z0-9][A-Za-z0-9._-]{0,99}$
//
// Case is preserved and significant — spi.SystemTenantID is "SYSTEM" and
// Cyoda Cloud's local-issuer fallback is "CYODA", so folding case would both
// reject real tenants and merge ones that must stay distinct.
//
// Requiring the first byte to be alphanumeric is what makes "", ".", "..",
// ".hidden" and "-leading" unrepresentable rather than enumerated.
//
// The returned error NEVER contains id. A rejected tenant id is
// attacker-chosen and reaches slog through the auth failure path's detail
// field; echoing it there is the log-injection this validation exists to
// prevent. The error carries a reason and a byte offset instead.
func ValidateTenantID(id spi.TenantID) error {
	s := string(id)
	switch {
	case len(s) == 0:
		return fmt.Errorf("%w: empty", ErrInvalidTenantID)
	case len(s) > MaxTenantIDLen:
		return fmt.Errorf("%w: %d bytes exceeds the %d-byte limit",
			ErrInvalidTenantID, len(s), MaxTenantIDLen)
	case !isTenantAlphanumeric(s[0]):
		return fmt.Errorf("%w: must begin with a letter or digit", ErrInvalidTenantID)
	}
	for i := 1; i < len(s); i++ {
		if c := s[i]; !isTenantAlphanumeric(c) && c != '.' && c != '_' && c != '-' {
			return fmt.Errorf("%w: disallowed byte at offset %d", ErrInvalidTenantID, i)
		}
	}
	return nil
}

func isTenantAlphanumeric(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
}

// ErrReservedTenantID reports a tenant id that is valid but reserved for the
// machinery: no caller may act in it or address it.
var ErrReservedTenantID = errors.New("reserved tenant id")

// ValidateAPITenantID reports whether id is a tenant a caller may act in or
// address: the tenant grammar, and not SYSTEM in any letter case. SYSTEM is
// the machinery's tenant; its store holds the cluster's auth state. Stored
// records and internal contexts carry SYSTEM legitimately, so
// ValidateTenantID itself accepts it. The error never contains id.
func ValidateAPITenantID(id spi.TenantID) error {
	if err := ValidateTenantID(id); err != nil {
		return err
	}
	if strings.EqualFold(string(id), string(spi.SystemTenantID)) {
		return fmt.Errorf("%w: the machinery's tenant", ErrReservedTenantID)
	}
	return nil
}
