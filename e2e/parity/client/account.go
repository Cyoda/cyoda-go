package client

import (
	"net/http"
	"testing"
)

// ProbeAuthRaw issues GET /api/account and returns the raw HTTP status code
// without raising on non-2xx. Used by JWT-validation parity scenarios to
// verify that a given bearer token is accepted (200) or rejected (401) by
// the server. The endpoint requires only a valid JWT — no specific role —
// so it is the lightest authenticated surface available.
func (c *Client) ProbeAuthRaw(t *testing.T) (int, []byte, error) {
	t.Helper()
	return c.DoJSONBodyRaw(t, http.MethodGet, "/api/account", nil)
}
