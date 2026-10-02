package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// refreshFraction is the share of a token's life after which tokenSource
// fetches a new one, so a call never goes out with a token about to expire.
const refreshFraction = 0.8

// tokenSource holds the compute client's M2M client credentials and fetches
// its bearer from the server's token endpoint with the client_credentials
// grant. It returns the cached bearer until 80 % of the token's life has
// passed, then fetches a new one. A failed fetch is returned to the caller,
// which fails its call: a token past its refresh point is never reused.
//
// It is safe for concurrent use. Neither the secret nor a token is ever logged.
type tokenSource struct {
	tokenURL     string
	clientID     string
	clientSecret string
	hc           *http.Client

	mu        sync.Mutex
	token     string
	refreshAt time.Time
}

// newTokenSource returns a token source for the client clientID of the cyoda
// instance at httpBase (the token endpoint is httpBase/api/oauth/token).
func newTokenSource(httpBase, clientID, clientSecret string) *tokenSource {
	return &tokenSource{
		tokenURL:     strings.TrimRight(httpBase, "/") + "/api/oauth/token",
		clientID:     clientID,
		clientSecret: clientSecret,
		hc:           &http.Client{Timeout: 15 * time.Second},
	}
}

// Token returns a bearer that has not reached its refresh point, fetching a
// new one when the cached one has.
func (s *tokenSource) Token() (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	if s.token != "" && now.Before(s.refreshAt) {
		return s.token, nil
	}
	s.token = ""
	tok, expiresIn, err := s.fetch()
	if err != nil {
		return "", err
	}
	s.token = tok
	s.refreshAt = now.Add(time.Duration(float64(expiresIn) * refreshFraction * float64(time.Second)))
	return tok, nil
}

// fetch runs one client_credentials grant and returns the access token and
// its remaining life in seconds.
func (s *tokenSource) fetch() (string, int64, error) {
	form := url.Values{"grant_type": {"client_credentials"}}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", 0, fmt.Errorf("failed to build token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	// RFC 6749 §2.3.1: the credentials are form-urlencoded before Basic encoding.
	req.SetBasicAuth(url.QueryEscape(s.clientID), url.QueryEscape(s.clientSecret))
	resp, err := s.hc.Do(req)
	if err != nil {
		return "", 0, fmt.Errorf("failed to call the token endpoint: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", 0, fmt.Errorf("failed to read the token response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		// The body of a refusal carries an OAuth error code, never a token.
		return "", 0, fmt.Errorf("token endpoint answered %d: %s", resp.StatusCode, raw)
	}
	var body struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int64  `json:"expires_in"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return "", 0, fmt.Errorf("failed to decode the token response: %w", err)
	}
	if body.AccessToken == "" || body.ExpiresIn <= 0 {
		return "", 0, fmt.Errorf("token response has no access token or no positive expires_in")
	}
	return body.AccessToken, body.ExpiresIn, nil
}

// bearerCredentials is gRPC per-call credentials: each call (and each stream,
// when it opens) carries the bearer tok returns at that moment. An error from
// tok fails the call before it is sent.
type bearerCredentials struct {
	tok func() (string, error)
}

// GetRequestMetadata implements credentials.PerRPCCredentials.
func (b bearerCredentials) GetRequestMetadata(context.Context, ...string) (map[string]string, error) {
	tok, err := b.tok()
	if err != nil {
		return nil, fmt.Errorf("failed to get a bearer: %w", err)
	}
	return map[string]string{"authorization": "Bearer " + tok}, nil
}

// RequireTransportSecurity implements credentials.PerRPCCredentials. The test
// client dials cyoda without TLS.
func (bearerCredentials) RequireTransportSecurity() bool { return false }
