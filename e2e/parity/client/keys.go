package client

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

// IssueKeyPairRaw issues POST /api/oauth/keys/keypair with the given body
// (e.g. {"algorithm": "RS256"}).
func (c *Client) IssueKeyPairRaw(t *testing.T, body map[string]any) (int, []byte, error) {
	t.Helper()
	return c.DoJSONBodyRaw(t, http.MethodPost, "/api/oauth/keys/keypair", body)
}

// CurrentKeyPairRaw issues GET /api/oauth/keys/keypair/current.
func (c *Client) CurrentKeyPairRaw(t *testing.T) (int, []byte, error) {
	t.Helper()
	return c.DoJSONBodyRaw(t, http.MethodGet, "/api/oauth/keys/keypair/current", nil)
}

// InvalidateKeyPairRaw issues POST /api/oauth/keys/keypair/{id}/invalidate.
func (c *Client) InvalidateKeyPairRaw(t *testing.T, kid string) (int, []byte, error) {
	t.Helper()
	return c.DoJSONBodyRaw(t, http.MethodPost, "/api/oauth/keys/keypair/"+url.PathEscape(kid)+"/invalidate", nil)
}

// InvalidateKeyPairWithGraceRaw issues POST /api/oauth/keys/keypair/{id}/invalidate
// with gracePeriodSec set to grace (whole seconds).
func (c *Client) InvalidateKeyPairWithGraceRaw(t *testing.T, kid string, grace time.Duration) (int, []byte, error) {
	t.Helper()
	return c.DoJSONBodyRaw(t, http.MethodPost, "/api/oauth/keys/keypair/"+url.PathEscape(kid)+"/invalidate",
		map[string]any{"gracePeriodSec": int64(grace / time.Second)})
}

// ReactivateKeyPairRaw issues POST /api/oauth/keys/keypair/{id}/reactivate.
func (c *Client) ReactivateKeyPairRaw(t *testing.T, kid string, validTo time.Time) (int, []byte, error) {
	t.Helper()
	return c.DoJSONBodyRaw(t, http.MethodPost, "/api/oauth/keys/keypair/"+url.PathEscape(kid)+"/reactivate",
		map[string]any{"validTo": validTo.UTC().Format(time.RFC3339)})
}

// DeleteKeyPairRaw issues DELETE /api/oauth/keys/keypair/{id}.
func (c *Client) DeleteKeyPairRaw(t *testing.T, kid string) (int, []byte, error) {
	t.Helper()
	return c.DoJSONBodyRaw(t, http.MethodDelete, "/api/oauth/keys/keypair/"+url.PathEscape(kid), nil)
}

// JWKSKIDs returns the KIDs the public JWKS endpoint publishes.
func (c *Client) JWKSKIDs(t *testing.T) (map[string]bool, error) {
	t.Helper()
	code, body, err := c.DoJSONBodyRaw(t, http.MethodGet, "/api/.well-known/jwks.json", nil)
	if err != nil {
		return nil, err
	}
	if code != http.StatusOK {
		return nil, fmt.Errorf("jwks: status %d", code)
	}
	var set struct {
		Keys []struct {
			Kid string `json:"kid"`
		} `json:"keys"`
	}
	if err := json.Unmarshal(body, &set); err != nil {
		return nil, err
	}
	out := map[string]bool{}
	for _, k := range set.Keys {
		out[k.Kid] = true
	}
	return out, nil
}

// CreateClientRaw creates an M2M client of the caller's tenant via
// POST /api/clients[?withAdminRole=true][&onBehalfOf=true]. The response body
// carries the plaintext secret exactly once (fields client_id, client_secret)
// — callers must not log it. onBehalfOf is never combined with withAdminRole.
func (c *Client) CreateClientRaw(t *testing.T, withAdminRole, onBehalfOf bool) (int, []byte, error) {
	t.Helper()
	path := "/api/clients"
	var q []string
	if withAdminRole {
		q = append(q, "withAdminRole=true")
	}
	if onBehalfOf {
		q = append(q, "onBehalfOf=true")
	}
	if len(q) > 0 {
		path += "?" + strings.Join(q, "&")
	}
	return c.DoJSONBodyRaw(t, http.MethodPost, path, nil)
}

// ListClientsRaw issues GET /api/clients — the caller's tenant's M2M
// clients.
func (c *Client) ListClientsRaw(t *testing.T) (int, []byte, error) {
	t.Helper()
	return c.DoJSONBodyRaw(t, http.MethodGet, "/api/clients", nil)
}

// DeleteClientRaw issues DELETE /api/clients/{clientId}.
func (c *Client) DeleteClientRaw(t *testing.T, clientID string) (int, []byte, error) {
	t.Helper()
	return c.DoJSONBodyRaw(t, http.MethodDelete, "/api/clients/"+url.PathEscape(clientID), nil)
}

// ResetClientSecretRaw issues PUT /api/clients/{clientId}/secret. The
// response body carries the freshly issued plaintext secret exactly once
// (field client_secret) — callers must not log it.
func (c *Client) ResetClientSecretRaw(t *testing.T, clientID string) (int, []byte, error) {
	t.Helper()
	return c.DoJSONBodyRaw(t, http.MethodPut, "/api/clients/"+url.PathEscape(clientID)+"/secret", nil)
}

// FetchClientCredentialsToken runs the client_credentials grant against
// baseURL as clientID of tenant and returns the bearer token. The token is a credential: never
// log it, never include it in test-failure messages.
func FetchClientCredentialsToken(ctx context.Context, baseURL, tenant, clientID, secret string) (string, int, error) {
	form := url.Values{"grant_type": {"client_credentials"}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(baseURL, "/")+"/api/tenants/"+tenant+"/oauth/token", strings.NewReader(form.Encode()))
	if err != nil {
		return "", 0, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetBasicAuth(clientID, secret)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", 0, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return "", resp.StatusCode, nil
	}
	var out struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return "", resp.StatusCode, err
	}
	return out.AccessToken, resp.StatusCode, nil
}

// TokenKID returns the "kid" of a JWT's header, or "" when the token is not a
// JWT with a decodable header. It reads only the header, never the claims.
func TokenKID(tok string) string {
	header, _, ok := strings.Cut(tok, ".")
	if !ok {
		return ""
	}
	raw, err := base64.RawURLEncoding.DecodeString(header)
	if err != nil {
		return ""
	}
	var h struct {
		Kid string `json:"kid"`
	}
	if json.Unmarshal(raw, &h) != nil {
		return ""
	}
	return h.Kid
}
