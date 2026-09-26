package commontest

import (
	"context"

	spi "github.com/cyoda-platform/cyoda-go-spi"

	"github.com/cyoda-platform/cyoda-go/internal/common"
)

// SystemUserContext returns context.Background() carrying the platform
// system UserContext scoped to tenant (common.SystemUserContextValue): the
// identity background subsystems act as.
func SystemUserContext(tenant spi.TenantID) context.Context {
	return spi.WithUserContext(context.Background(), common.SystemUserContextValue(tenant))
}
