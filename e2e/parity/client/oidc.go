package client

import (
	"fmt"
	"net/http"
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
	return c.DoJSONBodyRaw(t, http.MethodPost, "/api/oauth/oidc/providers", body)
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
	return c.DoJSONBodyRaw(t, http.MethodPatch, path, patch)
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
	return c.DoJSONBodyRaw(t, http.MethodPost, path, nil)
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
	return c.DoJSONBodyRaw(t, http.MethodPost, path, nil)
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
	return c.DoJSONBodyRaw(t, http.MethodDelete, path, nil)
}

// ReactivateOidcProviderWithKeys issues
// POST /api/oauth/oidc/providers/{id}/reactivate with an explicit
// reactivateKeys payload. When reactivateKeys=true the service syncs the
// cached JWKS against the upstream (D19); false skips the sync.
// Returns the updated provider on success or an error on non-2xx.
func (c *Client) ReactivateOidcProviderWithKeys(t *testing.T, id uuid.UUID, reactivateKeys bool) (OidcProviderResponse, error) {
	t.Helper()
	path := fmt.Sprintf("/api/oauth/oidc/providers/%s/reactivate", id.String())
	body := map[string]any{"reactivateKeys": reactivateKeys}
	var resp OidcProviderResponse
	if _, err := c.doJSON(t, http.MethodPost, path, body, &resp); err != nil {
		return OidcProviderResponse{}, err
	}
	return resp, nil
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
	return c.DoJSONBodyRaw(t, http.MethodPost, "/api/oauth/oidc/providers/reload", nil)
}

// ProbeAuthRaw issues GET /api/oauth/oidc/providers and returns the raw
// HTTP status code without raising on non-2xx. Used by JWT-validation
// parity scenarios to verify that a given bearer token is accepted
// (200) or rejected (401) by the server. The endpoint requires only a
// valid JWT — no specific role — so it is the lightest authenticated
// surface available.
func (c *Client) ProbeAuthRaw(t *testing.T) (int, []byte, error) {
	t.Helper()
	return c.DoJSONBodyRaw(t, http.MethodGet, "/api/oauth/oidc/providers", nil)
}

// GetOidcProviderRaw issues PATCH /api/oauth/oidc/providers/{id} with an empty
// patch body to probe whether the provider is accessible to the caller. Returns
// the HTTP status code and raw body without raising on non-2xx. Used by
// cross-tenant management-isolation tests that need to assert 404 when a
// caller from tenant B tries to reach tenant A's provider ID (D1 stale-index
// defence treats the ID as not found, returning 404 rather than 403).
//
// An empty PATCH is used as the probe because the API has no GET-by-ID
// endpoint; PATCH with no fields is a no-op when the provider exists and
// returns OIDC_PROVIDER_NOT_FOUND (404) when it does not — identical to what
// a cross-tenant caller would see.
func (c *Client) GetOidcProviderRaw(t *testing.T, id uuid.UUID) (int, []byte, error) {
	t.Helper()
	path := fmt.Sprintf("/api/oauth/oidc/providers/%s", id.String())
	return c.DoJSONBodyRaw(t, http.MethodPatch, path, map[string]any{})
}
