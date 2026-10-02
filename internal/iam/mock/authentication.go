package mock

import (
	"context"
	"net/http"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/contract"
)

type AuthenticationService struct {
	DefaultUser *spi.UserContext
}

func NewAuthenticationService(defaultUser *spi.UserContext) *AuthenticationService {
	return &AuthenticationService{DefaultUser: defaultUser}
}

// Authenticate accepts every request as the default principal. The context
// also carries a client-token marker for that principal (generation 0) — mock
// mode has no client store to check it against.
func (s *AuthenticationService) Authenticate(ctx context.Context, r *http.Request) (context.Context, error) {
	// A defensive copy so concurrent requests cannot mutate the shared default.
	uc := *s.DefaultUser
	roles := make([]string, len(s.DefaultUser.Roles))
	copy(roles, s.DefaultUser.Roles)
	uc.Roles = roles
	ctx = spi.WithUserContext(ctx, &uc)
	return contract.WithClientToken(ctx, contract.ClientToken{ClientID: uc.UserID}), nil
}
