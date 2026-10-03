package middleware

import (
	"net/http"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/contract"
)

// Auth wraps the next handler in an authentication step. The
// authService.Authenticate implementation is responsible for emitting the
// per-failure structured slog.Warn record (with reason slug and operator
// context) — see internal/auth/delegating.go. The middleware therefore does
// not log here: a duplicate WARN per failed request would violate the
// "one event = one log line" rule in .claude/rules/logging.md.
//
// On any auth failure the response is the uniform RFC 9457 problem-detail
// body with HTTP 401 and code UNAUTHORIZED, carrying no enumeration signal.
func Auth(authService contract.AuthenticationService) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx, err := authService.Authenticate(r.Context(), r)
			// A success without a principal is a failure: no request runs
			// unattributed.
			if err != nil || spi.GetUserContext(ctx) == nil {
				common.WriteError(w, r, common.Operational(http.StatusUnauthorized, common.ErrCodeUnauthorized, "authentication failed"))
				return
			}
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}
