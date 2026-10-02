package contract

import (
	"context"
	"net/http"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

type AuthenticationService interface {
	// Authenticate returns ctx carrying the caller's *spi.UserContext and, for a
	// client-credentials token, its ClientToken.
	Authenticate(ctx context.Context, r *http.Request) (context.Context, error)
}

// ClientToken marks a request authenticated with a client-credentials token:
// the client's id and the secret generation the token was issued under.
type ClientToken struct {
	ClientID string
	Gen      uint64
}

type clientTokenKey struct{}

// WithClientToken returns ctx carrying ct.
func WithClientToken(ctx context.Context, ct ClientToken) context.Context {
	return context.WithValue(ctx, clientTokenKey{}, ct)
}

// ClientTokenFrom returns the ClientToken in ctx, and false when the request
// was not authenticated with a client-credentials token.
func ClientTokenFrom(ctx context.Context) (ClientToken, bool) {
	ct, ok := ctx.Value(clientTokenKey{}).(ClientToken)
	return ct, ok
}

type AuthorizationService interface {
	HasRole(ctx context.Context, user *spi.UserContext, role string) bool
	CheckAccess(ctx context.Context, user *spi.UserContext, resource string, operation string) error
}
