package client

import (
	"context"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/google/uuid"
)

// cleanupTimeout bounds a request sent from a t.Cleanup.
const cleanupTimeout = 30 * time.Second

// requestOnCleanup registers a t.Cleanup that sends method path (no body)
// with the client's token. A scenario uses it to remove what it created on a
// shared server however the scenario ends.
//
// The request runs on its own bounded context: t.Context() is cancelled
// before cleanups run, so a helper that builds its request on it never
// sends. A transport error, or any status other than 200, 204 or 404 (already
// gone), is reported with t.Errorf. The report names the method, path and
// status, never the token.
func (c *Client) requestOnCleanup(t testing.TB, method, path string) {
	t.Helper()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, nil)
		if err != nil {
			t.Errorf("cleanup %s %s: build request: %v", method, path, err)
			return
		}
		if c.token != "" {
			req.Header.Set("Authorization", "Bearer "+c.token)
		}
		resp, err := c.http.Do(req)
		if err != nil {
			t.Errorf("cleanup %s %s: %v", method, path, err)
			return
		}
		resp.Body.Close()
		switch resp.StatusCode {
		case http.StatusOK, http.StatusNoContent, http.StatusNotFound:
		default:
			t.Errorf("cleanup %s %s: status %d, want 200, 204 or 404", method, path, resp.StatusCode)
		}
	})
}

// DeleteKeyPairOnCleanup deletes the key pair when the test ends, so a
// scenario that fails part-way does not leave it signing on a shared server.
func (c *Client) DeleteKeyPairOnCleanup(t testing.TB, kid string) {
	t.Helper()
	c.requestOnCleanup(t, http.MethodDelete, "/api/oauth/keys/keypair/"+url.PathEscape(kid))
}

// DeleteEntityOnCleanup deletes the entity when the test ends, so a
// scenario that fails part-way does not leave, for example, a scheduled
// task retrying for the rest of a shared server's life.
func (c *Client) DeleteEntityOnCleanup(t testing.TB, id uuid.UUID) {
	t.Helper()
	c.requestOnCleanup(t, http.MethodDelete, "/api/entity/"+id.String())
}
