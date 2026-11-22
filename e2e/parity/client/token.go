package client

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// TokenExchangeGrant and JWTTokenType are the RFC 8693 grant type and the
// only subject token type the token exchange accepts.
const (
	TokenExchangeGrant = "urn:ietf:params:oauth:grant-type:token-exchange"
	JWTTokenType       = "urn:ietf:params:oauth:token-type:jwt"
)

// ExchangeTokenRaw runs the token-exchange grant against the client's base
// URL as clientID of tenant (HTTP Basic), with form as the body. grant_type is set;
// subject_token_type defaults to the JWT type when form does not carry the
// key. The client's own bearer token is not sent. It returns (status, body,
// transport-error) without raising on non-2xx. A 200 body carries a token:
// never log it.
func (c *Client) ExchangeTokenRaw(t *testing.T, tenant, clientID, secret string, form url.Values) (int, []byte, error) {
	t.Helper()
	f := url.Values{}
	for k, v := range form {
		f[k] = append([]string(nil), v...)
	}
	f.Set("grant_type", TokenExchangeGrant)
	if _, set := f["subject_token_type"]; !set {
		f.Set("subject_token_type", JWTTokenType)
	}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, c.baseURL+"/api/tenants/"+tenant+"/oauth/token", strings.NewReader(f.Encode()))
	if err != nil {
		return 0, nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetBasicAuth(clientID, secret)
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("transport: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, raw, nil
}
