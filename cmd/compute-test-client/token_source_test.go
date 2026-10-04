package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestComputeClientRefreshesTokenBeforeExpiry(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, sec, ok := r.BasicAuth()
		if !ok || id != "C1" || sec != "s" || r.FormValue("grant_type") != "client_credentials" {
			http.Error(w, "bad", http.StatusUnauthorized)
			return
		}
		n := calls.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": fmt.Sprintf("tok-%d", n), "token_type": "Bearer", "expires_in": 2})
	}))
	defer srv.Close()
	src := newTokenSource(srv.URL, "t1", "C1", "s")
	first, err := src.Token()
	if err != nil || first != "tok-1" {
		t.Fatalf("first = %q, %v", first, err)
	}
	time.Sleep(1800 * time.Millisecond) // past 80 % of 2 s
	second, err := src.Token()
	if err != nil || second == first {
		t.Fatalf("second = %q, %v; want a refreshed token", second, err)
	}
}

// TestTokenSourceReusesTokenBeforeRefreshPoint: concurrent callers before the
// refresh point share one fetched token.
func TestTokenSourceReusesTokenBeforeRefreshPoint(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": fmt.Sprintf("tok-%d", n), "token_type": "Bearer", "expires_in": 300})
	}))
	defer srv.Close()
	src := newTokenSource(srv.URL, "t1", "C1", "s")
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if tok, err := src.Token(); err != nil || tok != "tok-1" {
				t.Errorf("Token() = %q, %v; want tok-1", tok, err)
			}
		}()
	}
	wg.Wait()
	if n := calls.Load(); n != 1 {
		t.Errorf("token endpoint called %d times; want 1", n)
	}
}

// TestTokenSourceFailsClosedPastRefreshPoint: once the cached token reaches its
// refresh point, a failed fetch is an error, not the old token.
func TestTokenSourceFailsClosedPastRefreshPoint(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) > 1 {
			http.Error(w, `{"error":"invalid_client"}`, http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "tok-1", "token_type": "Bearer", "expires_in": 2})
	}))
	defer srv.Close()
	src := newTokenSource(srv.URL, "t1", "C1", "s")
	if _, err := src.Token(); err != nil {
		t.Fatalf("first Token(): %v", err)
	}
	time.Sleep(1800 * time.Millisecond) // past 80 % of 2 s
	if tok, err := src.Token(); err == nil || tok != "" {
		t.Fatalf("Token() past the refresh point with a failing endpoint = %q, %v; want an error and no token", tok, err)
	}
}

// TestTokenSourcePostsFormToTokenEndpoint: the grant goes to
// <base>/api/tenants/t1/oauth/token as a form, which the token endpoint requires.
func TestTokenSourcePostsFormToTokenEndpoint(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/tenants/t1/oauth/token" ||
			r.Header.Get("Content-Type") != "application/x-www-form-urlencoded" {
			http.Error(w, "bad", http.StatusBadRequest)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "tok", "token_type": "Bearer", "expires_in": 300})
	}))
	defer srv.Close()
	if tok, err := newTokenSource(srv.URL+"/", "t1", "C1", "s").Token(); err != nil || tok != "tok" {
		t.Fatalf("Token() = %q, %v", tok, err)
	}
}

// staticToken is a token func that always returns tok.
func staticToken(tok string) func() (string, error) {
	return func() (string, error) { return tok, nil }
}

// TestCallbackClientAsksForBearerPerRequest: every HTTP callback carries the
// bearer the token func returns at that moment, and a token error fails the
// callback before it is sent.
func TestCallbackClientAsksForBearerPerRequest(t *testing.T) {
	var got []string
	var mu sync.Mutex
	door := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		got = append(got, r.Header.Get("Authorization"))
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer door.Close()
	var n atomic.Int32
	var failNext atomic.Bool
	cb := newCallbackClient(door.URL, func() (string, error) {
		if failNext.Load() {
			return "", fmt.Errorf("no token")
		}
		return fmt.Sprintf("tok-%d", n.Add(1)), nil
	})
	for range 2 {
		if _, err := cb.do(t.Context(), http.MethodGet, "/api/entity/x", "", "", ""); err != nil {
			t.Fatalf("do: %v", err)
		}
	}
	failNext.Store(true)
	if _, err := cb.do(t.Context(), http.MethodGet, "/api/entity/x", "", "", ""); err == nil {
		t.Fatal("do succeeded although the token func failed")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 2 || got[0] != "Bearer tok-1" || got[1] != "Bearer tok-2" {
		t.Fatalf("Authorization headers = %q; want [Bearer tok-1 Bearer tok-2] and nothing sent on a token error", got)
	}
}

// TestBearerCredentialsAskPerCall: the gRPC per-call credentials carry the
// bearer the token func returns, and pass its error on.
func TestBearerCredentialsAskPerCall(t *testing.T) {
	creds := bearerCredentials{tok: staticToken("tok-a")}
	if creds.RequireTransportSecurity() {
		t.Error("RequireTransportSecurity = true; the test client dials without TLS")
	}
	md, err := creds.GetRequestMetadata(t.Context())
	if err != nil || md["authorization"] != "Bearer tok-a" {
		t.Fatalf("GetRequestMetadata = %v, %v; want authorization Bearer tok-a", md, err)
	}
	failing := bearerCredentials{tok: func() (string, error) { return "", fmt.Errorf("no token") }}
	if _, err := failing.GetRequestMetadata(t.Context()); err == nil {
		t.Fatal("GetRequestMetadata succeeded although the token func failed")
	}
}
