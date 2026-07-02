package e2e_test

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"

	"github.com/cyoda-platform/cyoda-go/internal/cluster/token"
)

// callback_txjoin_errors_test.go — feature #287, loud-fail error-code coverage
// for compute-node callbacks presenting a transaction routing token over HTTP
// (real Postgres via the callback harness). Each case proves one row of the
// spec error table end-to-end through the TxJoin middleware:
//
//	non-empty token, unknown/closed txID → 404 TRANSACTION_NOT_FOUND
//	expired token                        → 410 TRANSACTION_EXPIRED
//	forged / bad-HMAC token              → 401 UNAUTHORIZED
//	token tenant ≠ caller tenant         → 403 FORBIDDEN
//	empty token (control)                → 2xx standalone
//
// Single-node uses an EPHEMERAL signer secret, so tokens the server will accept
// cannot be hand-signed by the test. Instead we reach the server's real signer
// via h.app.TokenSigner() to mint HMAC-valid-but-unjoinable / expired tokens,
// use a foreign signer for the forged case, and hold a REAL live transaction
// open (inside a blocking processor) to exercise the tenant check with a live
// token. The token value is never logged (Gate 3 / spec §8-H10).

// problemErrorCode extracts properties.errorCode from an RFC 9457 problem body.
func problemErrorCode(body string) string {
	var pd struct {
		Status     int `json:"status"`
		Properties struct {
			ErrorCode string `json:"errorCode"`
		} `json:"properties"`
	}
	_ = json.Unmarshal([]byte(body), &pd)
	return pd.Properties.ErrorCode
}

// randSuffix returns a short random hex string for per-case uniqueness.
func randSuffix(t *testing.T) string {
	t.Helper()
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		t.Fatalf("rand: %v", err)
	}
	return hex.EncodeToString(b)
}

// provisionTenant creates an M2M client at tenantID on THIS stack (bypassing the
// tenant-derived /clients HTTP surface) and returns its credentials.
func (h *callbackHarness) provisionTenant(t *testing.T, tenantID, userID string) (clientID, secret string) {
	t.Helper()
	authSvc := h.app.AuthService()
	if authSvc == nil {
		t.Fatal("provisionTenant requires JWT IAM mode (AuthService() returned nil)")
	}
	rb := make([]byte, 12)
	if _, err := rand.Read(rb); err != nil {
		t.Fatalf("rand: %v", err)
	}
	clientID = "cbxclient" + hex.EncodeToString(rb[:6])
	secret = hex.EncodeToString(rb[6:])
	if err := authSvc.M2MClientStore().CreateWithSecret(
		clientID, spi.TenantID(tenantID), userID, secret, []string{"ROLE_ADMIN", "ROLE_M2M"},
	); err != nil {
		t.Fatalf("CreateWithSecret: %v", err)
	}
	return clientID, secret
}

// fetchTokenFor obtains a client-credentials bearer for the given creds on this stack.
func (h *callbackHarness) fetchTokenFor(t *testing.T, clientID, secret string) string {
	t.Helper()
	form := url.Values{"grant_type": {"client_credentials"}}
	req, err := http.NewRequest(http.MethodPost, h.baseURL+"/api/oauth/token", strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatalf("token request: %v", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetBasicAuth(clientID, secret)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("token request failed: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("token endpoint returned %d: %s", resp.StatusCode, body)
	}
	var out map[string]any
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode token: %v", err)
	}
	tok, _ := out["access_token"].(string)
	if tok == "" {
		t.Fatal("empty access_token")
	}
	return tok
}

// doAuthBearer issues an HTTP request with an explicit bearer + optional tx-token.
func (h *callbackHarness) doAuthBearer(t *testing.T, bearer, method, path, body, txToken string) *http.Response {
	t.Helper()
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, h.baseURL+path, r)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+bearer)
	req.Header.Set("Content-Type", "application/json")
	if txToken != "" {
		req.Header.Set("X-Tx-Token", txToken)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s failed: %v", method, path, err)
	}
	return resp
}

// TestCallbackErr_LoudFailCodes proves each documented status/error code a
// callback token can trigger through the HTTP TxJoin middleware. Subtests share
// one full stack (postgres + member) for speed; each uses isolated names.
func TestCallbackErr_LoudFailCodes(t *testing.T) {
	h := newCallbackHarness(t)
	_ = h.token(t) // prime the bootstrap (tenant-A) bearer used by h.callback/DoAuth

	// A syntactically valid entity path; the token is rejected in the middleware
	// before the handler runs, so the id need not resolve to a real entity.
	probePath := "/api/entity/00000000-0000-0000-0000-000000000000"

	// non-empty token, unknown txID → 404 TRANSACTION_NOT_FOUND.
	// HMAC-valid (server's own signer) but references a tx that was never
	// registered, so Join fails ErrTxNotFound rather than being rejected as forged.
	t.Run("NotFound_404", func(t *testing.T) {
		tok, err := h.app.TokenSigner().Issue("local", "no-such-tx-"+randSuffix(t), time.Now().Add(time.Minute))
		if err != nil {
			t.Fatalf("Issue: %v", err)
		}
		resp := h.DoAuth(t, http.MethodGet, probePath, "", tok)
		body := h.readBody(t, resp)
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("status = %d; want 404 (body: %s)", resp.StatusCode, body)
		}
		if code := problemErrorCode(body); code != "TRANSACTION_NOT_FOUND" {
			t.Fatalf("errorCode = %q; want TRANSACTION_NOT_FOUND (body: %s)", code, body)
		}
	})

	// expired token → 410 TRANSACTION_EXPIRED. HMAC-valid but past its deadline,
	// so Verify fails ErrTokenExpired before any Join is attempted.
	t.Run("Expired_410", func(t *testing.T) {
		tok, err := h.app.TokenSigner().Issue("local", "tx-"+randSuffix(t), time.Now().Add(-10*time.Second))
		if err != nil {
			t.Fatalf("Issue: %v", err)
		}
		resp := h.DoAuth(t, http.MethodGet, probePath, "", tok)
		body := h.readBody(t, resp)
		if resp.StatusCode != http.StatusGone {
			t.Fatalf("status = %d; want 410 (body: %s)", resp.StatusCode, body)
		}
		if code := problemErrorCode(body); code != "TRANSACTION_EXPIRED" {
			t.Fatalf("errorCode = %q; want TRANSACTION_EXPIRED (body: %s)", code, body)
		}
	})

	// forged / bad-HMAC token → 401 UNAUTHORIZED. Signed by a DIFFERENT secret,
	// so the server's signer rejects the signature (ErrTokenTampered).
	t.Run("Forged_401", func(t *testing.T) {
		forger, err := token.NewSigner([]byte("forged-secret-key-at-least-32-bytes!!"))
		if err != nil {
			t.Fatalf("NewSigner(forger): %v", err)
		}
		tok, err := forger.Issue("local", "tx-"+randSuffix(t), time.Now().Add(time.Minute))
		if err != nil {
			t.Fatalf("Issue: %v", err)
		}
		resp := h.DoAuth(t, http.MethodGet, probePath, "", tok)
		body := h.readBody(t, resp)
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("status = %d; want 401 (body: %s)", resp.StatusCode, body)
		}
		if code := problemErrorCode(body); code != "UNAUTHORIZED" {
			t.Fatalf("errorCode = %q; want UNAUTHORIZED (body: %s)", code, body)
		}
	})

	// empty token (control) → 2xx standalone. No X-Tx-Token header: the middleware
	// passes through and the create runs as a standalone (non-joined) operation.
	t.Run("EmptyToken_Standalone_2xx", func(t *testing.T) {
		const solo = "cbx-standalone"
		h.SetupModelWithWorkflow(t, solo, secondaryWorkflow)
		id, status, body := h.CreateEntity(t, solo, 1, `{"name":"solo","amount":1,"status":"new"}`)
		if status != http.StatusOK {
			t.Fatalf("standalone create status = %d; want 200 (body: %s)", status, body)
		}
		if id == "" {
			t.Fatalf("standalone create returned no entity id (body: %s)", body)
		}
	})

	// token tenant ≠ caller tenant → 403 FORBIDDEN. A blocking SYNC processor
	// holds a REAL transaction T (owned by tenant A) open; we present T's live
	// token while authenticated as a second tenant B → the Join tenant check
	// rejects it. This is the only case needing a live tx, so it is orchestrated.
	t.Run("CrossTenant_403", func(t *testing.T) {
		const primary = "cbx-primary"

		tokenCh := make(chan string, 1)
		release := make(chan struct{})
		h.RegisterProc("cbx-hold", func(rc *reqCtx) (map[string]any, error) {
			tokenCh <- rc.token // live token for T (tenant A); never logged
			<-release           // hold T open until the cross-tenant probe completes
			return nil, nil
		})

		primaryWF := `{
			"importMode": "REPLACE",
			"workflows": [{
				"version": "1.1", "name": "cbx-primary-wf", "initialState": "NONE", "active": true,
				"states": {
					"NONE":   {"transitions": [{"name": "init", "next": "ACTIVE", "manual": false,
						"processors": [{"type": "calculator", "name": "cbx-hold", "executionMode": "SYNC",
							"config": {"attachEntity": true, "calculationNodesTags": ""}}]
					}]},
					"ACTIVE": {}
				}
			}]
		}`
		h.SetupModelWithWorkflow(t, primary, primaryWF)

		// A second tenant on this same stack.
		clientB, secretB := h.provisionTenant(t, "tenant-b", "user-b")
		bearerB := h.fetchTokenFor(t, clientB, secretB)

		// Begin T as tenant A (standalone create, empty token). It blocks inside
		// the processor. h.callback is goroutine-safe and takes no *testing.T.
		type createRes struct {
			status int
			body   string
		}
		done := make(chan createRes, 1)
		go func() {
			res, err := h.callback(http.MethodPost, fmt.Sprintf("/api/entity/JSON/%s/1", primary), `{"name":"parent","amount":1,"status":"new"}`, "")
			if err != nil {
				done <- createRes{status: -1, body: err.Error()}
				return
			}
			done <- createRes{status: res.StatusCode, body: res.Body}
		}()

		// Capture T's live token from inside the running processor.
		var liveToken string
		select {
		case liveToken = <-tokenCh:
		case <-time.After(15 * time.Second):
			t.Fatal("timeout: hold processor did not run")
		}
		if liveToken == "" {
			t.Fatal("engine attached an empty tx-token to the calc request")
		}

		// Present tenant A's live token while authenticated as tenant B → 403.
		resp := h.doAuthBearer(t, bearerB, http.MethodGet, probePath, "", liveToken)
		body := h.readBody(t, resp)
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("cross-tenant status = %d; want 403 (body: %s)", resp.StatusCode, body)
		}
		if code := problemErrorCode(body); code != "FORBIDDEN" {
			t.Fatalf("cross-tenant errorCode = %q; want FORBIDDEN (body: %s)", code, body)
		}

		// Release the held transaction and let the primary create finish.
		close(release)
		select {
		case cr := <-done:
			if cr.status != http.StatusOK {
				t.Fatalf("primary create after release: status=%d body=%s", cr.status, cr.body)
			}
		case <-time.After(15 * time.Second):
			t.Fatal("timeout: primary create did not complete after release")
		}
	})
}
