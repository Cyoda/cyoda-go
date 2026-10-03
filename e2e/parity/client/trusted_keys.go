package client

import (
	"net/http"
	"net/url"
	"testing"
)

// RegisterTrustedKeyRaw issues POST /api/oauth/keys/trusted with the given
// body (keyId, jwk and the optional fields).
func (c *Client) RegisterTrustedKeyRaw(t *testing.T, body map[string]any) (int, []byte, error) {
	t.Helper()
	return c.DoJSONBodyRaw(t, http.MethodPost, "/api/oauth/keys/trusted", body)
}

// ListTrustedKeysRaw issues GET /api/oauth/keys/trusted.
func (c *Client) ListTrustedKeysRaw(t *testing.T) (int, []byte, error) {
	t.Helper()
	return c.DoJSONBodyRaw(t, http.MethodGet, "/api/oauth/keys/trusted", nil)
}

// InvalidateTrustedKeyRaw issues POST /api/oauth/keys/trusted/{keyId}/invalidate,
// which has no body.
func (c *Client) InvalidateTrustedKeyRaw(t *testing.T, kid string) (int, []byte, error) {
	t.Helper()
	return c.DoJSONBodyRaw(t, http.MethodPost, "/api/oauth/keys/trusted/"+url.PathEscape(kid)+"/invalidate", nil)
}

// ReactivateTrustedKeyRaw issues POST /api/oauth/keys/trusted/{keyId}/reactivate
// with the given body (validFrom, validTo).
func (c *Client) ReactivateTrustedKeyRaw(t *testing.T, kid string, body map[string]any) (int, []byte, error) {
	t.Helper()
	return c.DoJSONBodyRaw(t, http.MethodPost, "/api/oauth/keys/trusted/"+url.PathEscape(kid)+"/reactivate", body)
}

// DeleteTrustedKeyRaw issues DELETE /api/oauth/keys/trusted/{keyId}.
func (c *Client) DeleteTrustedKeyRaw(t *testing.T, kid string) (int, []byte, error) {
	t.Helper()
	return c.DoJSONBodyRaw(t, http.MethodDelete, "/api/oauth/keys/trusted/"+url.PathEscape(kid), nil)
}
