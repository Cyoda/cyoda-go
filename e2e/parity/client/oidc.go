package client

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// OidcProviderResponse is the parity view of OidcProviderResponseDto from
// the OIDC management API. Mirrors api/generated.go OidcProviderResponseDto.
type OidcProviderResponse struct {
	ID                 uuid.UUID `json:"id"`
	WellKnownConfigUri string    `json:"wellKnownConfigUri"`
	Active             bool      `json:"active"`
	CreatedAt          time.Time `json:"createdAt"`
	Issuers            *[]string `json:"issuers,omitempty"`
	ExpectedAudiences  *[]string `json:"expectedAudiences,omitempty"`
	RolesClaim         *string   `json:"rolesClaim,omitempty"`
}

// RegisterOidcProvider issues POST /api/oauth/oidc/providers with the
// given request body and decodes the response into OidcProviderResponse.
// Returns the provider or an error on non-2xx / decode failure.
func (c *Client) RegisterOidcProvider(t *testing.T, body map[string]any) (OidcProviderResponse, error) {
	t.Helper()
	var resp OidcProviderResponse
	if _, err := c.doJSON(t, http.MethodPost, "/api/oauth/oidc/providers", body, &resp); err != nil {
		return OidcProviderResponse{}, err
	}
	return resp, nil
}

// RegisterOidcProviderRaw issues POST /api/oauth/oidc/providers and returns
// the HTTP status code and raw body without raising on non-2xx. Used by
// negative-path tests that assert on the error envelope.
func (c *Client) RegisterOidcProviderRaw(t *testing.T, body map[string]any) (int, []byte, error) {
	t.Helper()
	return c.doJSONBodyRaw(t, http.MethodPost, "/api/oauth/oidc/providers", body)
}

// ListOidcProviders issues GET /api/oauth/oidc/providers with an optional
// activeOnly filter and decodes the response into []OidcProviderResponse.
func (c *Client) ListOidcProviders(t *testing.T, activeOnly bool) ([]OidcProviderResponse, error) {
	t.Helper()
	path := "/api/oauth/oidc/providers"
	if activeOnly {
		path += "?activeOnly=true"
	}
	var providers []OidcProviderResponse
	if _, err := c.doJSON(t, http.MethodGet, path, nil, &providers); err != nil {
		return nil, err
	}
	return providers, nil
}

// UpdateOidcProvider issues PATCH /api/oauth/oidc/providers/{id} with the
// given patch body and decodes the response into OidcProviderResponse.
func (c *Client) UpdateOidcProvider(t *testing.T, id uuid.UUID, patch map[string]any) (OidcProviderResponse, error) {
	t.Helper()
	path := fmt.Sprintf("/api/oauth/oidc/providers/%s", id.String())
	var resp OidcProviderResponse
	if _, err := c.doJSON(t, http.MethodPatch, path, patch, &resp); err != nil {
		return OidcProviderResponse{}, err
	}
	return resp, nil
}

// UpdateOidcProviderRaw issues PATCH /api/oauth/oidc/providers/{id} and
// returns the HTTP status code and raw body without raising on non-2xx.
func (c *Client) UpdateOidcProviderRaw(t *testing.T, id uuid.UUID, patch map[string]any) (int, []byte, error) {
	t.Helper()
	path := fmt.Sprintf("/api/oauth/oidc/providers/%s", id.String())
	return c.doJSONBodyRaw(t, http.MethodPatch, path, patch)
}

// InvalidateOidcProvider issues POST /api/oauth/oidc/providers/{id}/invalidate.
// Returns an error on non-2xx.
func (c *Client) InvalidateOidcProvider(t *testing.T, id uuid.UUID) error {
	t.Helper()
	path := fmt.Sprintf("/api/oauth/oidc/providers/%s/invalidate", id.String())
	_, err := c.doJSON(t, http.MethodPost, path, nil, nil)
	return err
}

// InvalidateOidcProviderRaw issues POST /api/oauth/oidc/providers/{id}/invalidate
// and returns the HTTP status code and raw body without raising on non-2xx.
func (c *Client) InvalidateOidcProviderRaw(t *testing.T, id uuid.UUID) (int, []byte, error) {
	t.Helper()
	path := fmt.Sprintf("/api/oauth/oidc/providers/%s/invalidate", id.String())
	return c.doJSONBodyRaw(t, http.MethodPost, path, nil)
}

// ReactivateOidcProvider issues POST /api/oauth/oidc/providers/{id}/reactivate.
// Returns the updated provider on success or an error on non-2xx.
func (c *Client) ReactivateOidcProvider(t *testing.T, id uuid.UUID) (OidcProviderResponse, error) {
	t.Helper()
	path := fmt.Sprintf("/api/oauth/oidc/providers/%s/reactivate", id.String())
	var resp OidcProviderResponse
	if _, err := c.doJSON(t, http.MethodPost, path, nil, &resp); err != nil {
		return OidcProviderResponse{}, err
	}
	return resp, nil
}

// ReactivateOidcProviderRaw issues POST /api/oauth/oidc/providers/{id}/reactivate
// and returns the HTTP status code and raw body without raising on non-2xx.
func (c *Client) ReactivateOidcProviderRaw(t *testing.T, id uuid.UUID) (int, []byte, error) {
	t.Helper()
	path := fmt.Sprintf("/api/oauth/oidc/providers/%s/reactivate", id.String())
	return c.doJSONBodyRaw(t, http.MethodPost, path, nil)
}

// DeleteOidcProvider issues DELETE /api/oauth/oidc/providers/{id}.
// Returns an error on non-2xx.
func (c *Client) DeleteOidcProvider(t *testing.T, id uuid.UUID) error {
	t.Helper()
	path := fmt.Sprintf("/api/oauth/oidc/providers/%s", id.String())
	_, err := c.doJSON(t, http.MethodDelete, path, nil, nil)
	return err
}

// DeleteOidcProviderRaw issues DELETE /api/oauth/oidc/providers/{id}
// and returns the HTTP status code and raw body without raising on non-2xx.
func (c *Client) DeleteOidcProviderRaw(t *testing.T, id uuid.UUID) (int, []byte, error) {
	t.Helper()
	path := fmt.Sprintf("/api/oauth/oidc/providers/%s", id.String())
	return c.doJSONBodyRaw(t, http.MethodDelete, path, nil)
}

// ReloadOidcProviders issues POST /api/oauth/oidc/providers/reload.
// Returns an error on non-2xx.
func (c *Client) ReloadOidcProviders(t *testing.T) error {
	t.Helper()
	_, err := c.doJSON(t, http.MethodPost, "/api/oauth/oidc/providers/reload", nil, nil)
	return err
}

// ReloadOidcProvidersRaw issues POST /api/oauth/oidc/providers/reload
// and returns the HTTP status code and raw body without raising on non-2xx.
func (c *Client) ReloadOidcProvidersRaw(t *testing.T) (int, []byte, error) {
	t.Helper()
	return c.doJSONBodyRaw(t, http.MethodPost, "/api/oauth/oidc/providers/reload", nil)
}

// doJSONBodyRaw issues an HTTP request with an optional JSON-marshalled body
// and returns (status, body, transport-error) without raising on non-2xx.
// This is the negative-path companion to doJSON, following the *Raw naming
// convention established by the rest of the parity client.
func (c *Client) doJSONBodyRaw(t *testing.T, method, path string, body any) (int, []byte, error) {
	t.Helper()

	var bodyReader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return 0, nil, fmt.Errorf("marshal request body: %w", err)
		}
		bodyReader = strings.NewReader(string(raw))
	} else {
		bodyReader = strings.NewReader("")
	}

	req, err := http.NewRequestWithContext(t.Context(), method, c.baseURL+path, bodyReader)
	if err != nil {
		return 0, nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("transport: %w", err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp.StatusCode, raw, nil
}
