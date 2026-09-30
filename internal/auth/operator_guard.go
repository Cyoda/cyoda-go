package auth

import (
	"net/http"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/common"
)

// PlatformTenantID is the tenant whose admins are platform operators.
//
// A platform operator manages state every tenant shares: the signing key
// pairs, the OIDC reload of every tenant's providers, a node's log level and
// trace sampler. The rule is safe because no token source lets a tenant
// admin act in another tenant: OIDC tokens take the provider owner's tenant,
// M2M and trusted-key tokens take the client's tenant, and only the holder of
// the signing key (`cyoda token`) can name any tenant. So only the operator
// can reach PLATFORM, and PLATFORM admins can only create principals in
// PLATFORM.
const PlatformTenantID spi.TenantID = "PLATFORM"

// OperatorGuard gates the platform-wide admin endpoints. The zero value
// applies the JWT rule: ROLE_ADMIN in PlatformTenantID. Build it once at
// wiring time and share it.
type OperatorGuard struct {
	// anyTenant drops the tenant condition. Only MockOperatorGuard sets it.
	anyTenant bool
}

// MockOperatorGuard is the guard for mock IAM mode, where one fixed principal
// serves every request: ROLE_ADMIN in any tenant passes.
func MockOperatorGuard() OperatorGuard { return OperatorGuard{anyTenant: true} }

// Require reports whether the caller is a platform operator. Otherwise it
// writes 401 UNAUTHORIZED (no UserContext: the auth middleware was bypassed)
// or 403 FORBIDDEN, and returns false.
func (g OperatorGuard) Require(w http.ResponseWriter, r *http.Request) bool {
	uc := spi.GetUserContext(r.Context())
	if uc == nil {
		common.WriteError(w, r, common.Operational(
			http.StatusUnauthorized, common.ErrCodeUnauthorized, "authentication failed"))
		return false
	}
	if !spi.HasRole(uc.Roles, "ROLE_ADMIN") || (!g.anyTenant && uc.Tenant.ID != PlatformTenantID) {
		common.WriteError(w, r, common.Operational(
			http.StatusForbidden, common.ErrCodeForbidden, "platform operator required"))
		return false
	}
	return true
}
