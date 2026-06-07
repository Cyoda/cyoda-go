package auth

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cyoda-platform/cyoda-go/internal/common"
)

func strPtr(s string) *string { return &s }

func generateTestJWK(t *testing.T) json.RawMessage {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("failed to generate RSA key: %v", err)
	}
	n := base64.RawURLEncoding.EncodeToString(key.N.Bytes())
	e := base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.E)).Bytes())
	jwk := map[string]string{
		"kty": "RSA",
		"n":   n,
		"e":   e,
		"kid": "test-kid",
		"alg": "RS256",
		"use": "sig",
	}
	data, _ := json.Marshal(jwk)
	return json.RawMessage(data)
}

func TestTrustedKeysHandler_RegisterAndList(t *testing.T) {
	store := NewInMemoryTrustedKeyStore()
	handler := NewTrustedKeysHandler(store)

	jwk := generateTestJWK(t)
	validTo := "2027-01-01T00:00:00Z"
	body := registerTrustedKeyRequest{
		KeyID:     "ext-key-1",
		JWK:       jwk,
		Audience:  "my-service",
		ValidFrom: strPtr("2026-01-01T00:00:00Z"),
		ValidTo:   &validTo,
	}
	bodyBytes, _ := json.Marshal(body)

	// POST register
	req := adminReq(http.MethodPost, "/oauth/keys/trusted", bytes.NewReader(bodyBytes))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}

	var created trustedKeyInfoResponse
	if err := json.NewDecoder(rec.Body).Decode(&created); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if created.KID != "ext-key-1" {
		t.Errorf("expected kid ext-key-1, got %s", created.KID)
	}
	if created.Audience != "my-service" {
		t.Errorf("expected audience my-service, got %s", created.Audience)
	}
	if !created.Active {
		t.Error("expected active to be true")
	}
	if created.ValidFrom != "2026-01-01T00:00:00Z" {
		t.Errorf("expected validFrom 2026-01-01T00:00:00Z, got %s", created.ValidFrom)
	}
	if created.ValidTo == nil || *created.ValidTo != "2027-01-01T00:00:00Z" {
		t.Errorf("expected validTo 2027-01-01T00:00:00Z, got %v", created.ValidTo)
	}

	// GET list
	req = adminReq(http.MethodGet, "/oauth/keys/trusted", nil)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var list []trustedKeyInfoResponse
	if err := json.NewDecoder(rec.Body).Decode(&list); err != nil {
		t.Fatalf("failed to decode list: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("expected 1 key, got %d", len(list))
	}
	if list[0].KID != "ext-key-1" {
		t.Errorf("expected kid ext-key-1, got %s", list[0].KID)
	}
}

func TestTrustedKeysHandler_Invalidate(t *testing.T) {
	store := NewInMemoryTrustedKeyStore()
	handler := NewTrustedKeysHandler(store)

	jwk := generateTestJWK(t)
	body := registerTrustedKeyRequest{
		KeyID:     "ext-key-2",
		JWK:       jwk,
		Audience:  "svc",
		ValidFrom: strPtr("2026-01-01T00:00:00Z"),
	}
	bodyBytes, _ := json.Marshal(body)

	// Register
	req := adminReq(http.MethodPost, "/oauth/keys/trusted", bytes.NewReader(bodyBytes))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d", rec.Code)
	}

	// Invalidate
	req = adminReq(http.MethodPost, "/oauth/keys/trusted/ext-key-2/invalidate", nil)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	// Verify inactive via store
	tk, err := store.Get("ext-key-2")
	if err != nil {
		t.Fatalf("failed to get key: %v", err)
	}
	if tk.Active {
		t.Error("expected key to be inactive after invalidation")
	}
}

func TestTrustedKeysHandler_Reactivate(t *testing.T) {
	store := NewInMemoryTrustedKeyStore()
	handler := NewTrustedKeysHandler(store)

	jwk := generateTestJWK(t)
	body := registerTrustedKeyRequest{
		KeyID:     "ext-key-3",
		JWK:       jwk,
		Audience:  "svc",
		ValidFrom: strPtr("2026-01-01T00:00:00Z"),
	}
	bodyBytes, _ := json.Marshal(body)

	// Register
	req := adminReq(http.MethodPost, "/oauth/keys/trusted", bytes.NewReader(bodyBytes))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d", rec.Code)
	}

	// Invalidate first
	req = adminReq(http.MethodPost, "/oauth/keys/trusted/ext-key-3/invalidate", nil)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 for invalidate, got %d", rec.Code)
	}

	// Reactivate
	req = adminReq(http.MethodPost, "/oauth/keys/trusted/ext-key-3/reactivate", nil)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 for reactivate, got %d: %s", rec.Code, rec.Body.String())
	}

	// Verify active via store
	tk, err := store.Get("ext-key-3")
	if err != nil {
		t.Fatalf("failed to get key: %v", err)
	}
	if !tk.Active {
		t.Error("expected key to be active after reactivation")
	}
}

func TestTrustedKeysHandler_Delete(t *testing.T) {
	store := NewInMemoryTrustedKeyStore()
	handler := NewTrustedKeysHandler(store)

	jwk := generateTestJWK(t)
	body := registerTrustedKeyRequest{
		KeyID:     "ext-key-4",
		JWK:       jwk,
		Audience:  "svc",
		ValidFrom: strPtr("2026-01-01T00:00:00Z"),
	}
	bodyBytes, _ := json.Marshal(body)

	// Register
	req := adminReq(http.MethodPost, "/oauth/keys/trusted", bytes.NewReader(bodyBytes))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d", rec.Code)
	}

	// Delete
	req = adminReq(http.MethodDelete, "/oauth/keys/trusted/ext-key-4", nil)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d: %s", rec.Code, rec.Body.String())
	}

	// Verify list is empty
	req = adminReq(http.MethodGet, "/oauth/keys/trusted", nil)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	var list []trustedKeyInfoResponse
	if err := json.NewDecoder(rec.Body).Decode(&list); err != nil {
		t.Fatalf("failed to decode list: %v", err)
	}
	if len(list) != 0 {
		t.Errorf("expected empty list after delete, got %d", len(list))
	}
}

// TestTrustedKeysHandler_RegisterRejectsInvalidKID verifies the handler
// rejects KIDs that fail the KID character/length whitelist (#34 item 3).
// Registering a KID containing path-traversal segments, control characters,
// or exceeding the 256-char ceiling must return 400 BAD_REQUEST without
// reaching the store.
func TestTrustedKeysHandler_RegisterRejectsInvalidKID(t *testing.T) {
	store := NewInMemoryTrustedKeyStore()
	handler := NewTrustedKeysHandler(store)

	jwk := generateTestJWK(t)
	cases := []struct {
		name string
		kid  string
	}{
		{"path-traversal", "../etc/passwd"},
		{"null-byte", "key\x00id"},
		{"too-long", strings.Repeat("a", 1000)},
		{"empty", ""},
		{"slash", "ns/key"},
		{"space", "ns key"},
	}
	for _, tc := range cases {
		body := registerTrustedKeyRequest{
			KeyID:     tc.kid,
			JWK:       jwk,
			Audience:  "svc",
			ValidFrom: strPtr("2026-01-01T00:00:00Z"),
		}
		bodyBytes, _ := json.Marshal(body)
		req := adminReq(http.MethodPost, "/oauth/keys/trusted", bytes.NewReader(bodyBytes))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: expected 400, got %d (body=%q)", tc.name, rec.Code, rec.Body.String())
		}
		// Defence-in-depth: confirm nothing reached the store.
		if got := store.List(); len(got) != 0 {
			t.Errorf("%s: store leaked %d entries; expected 0", tc.name, len(got))
		}
	}
}

// TestTrustedKeysHandler_RegisterAcceptsValidKIDChars confirms the whitelist
// accepts the characters Cyoda Cloud's KID convention requires (alphanumeric
// plus '.', '_', '-') so the regex tightening in #34/3 doesn't block legit
// input.
func TestTrustedKeysHandler_RegisterAcceptsValidKIDChars(t *testing.T) {
	store := NewInMemoryTrustedKeyStore()
	handler := NewTrustedKeysHandler(store)

	body := registerTrustedKeyRequest{
		KeyID:     "issuer.example.com_key-1",
		JWK:       generateTestJWK(t),
		Audience:  "svc",
		ValidFrom: strPtr("2026-01-01T00:00:00Z"),
	}
	bodyBytes, _ := json.Marshal(body)
	req := adminReq(http.MethodPost, "/oauth/keys/trusted", bytes.NewReader(bodyBytes))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
}

// errorReturningTrustedKeyStore wraps any TrustedKeyStore and forces specific
// methods to return injected errors. Used by handler tests to verify error
// translation without standing up a flaky storage backend.
type errorReturningTrustedKeyStore struct {
	inner         TrustedKeyStore
	registerErr   error
	deleteErr     error
	invalidateErr error
	reactivateErr error
}

func (s *errorReturningTrustedKeyStore) Register(tk *TrustedKey) error {
	if s.registerErr != nil {
		return s.registerErr
	}
	return s.inner.Register(tk)
}
func (s *errorReturningTrustedKeyStore) Get(kid string) (*TrustedKey, error) { return s.inner.Get(kid) }
func (s *errorReturningTrustedKeyStore) List() []*TrustedKey                 { return s.inner.List() }
func (s *errorReturningTrustedKeyStore) Delete(kid string) error {
	if s.deleteErr != nil {
		return s.deleteErr
	}
	return s.inner.Delete(kid)
}
func (s *errorReturningTrustedKeyStore) Invalidate(kid string) error {
	if s.invalidateErr != nil {
		return s.invalidateErr
	}
	return s.inner.Invalidate(kid)
}
func (s *errorReturningTrustedKeyStore) Reactivate(kid string) error {
	if s.reactivateErr != nil {
		return s.reactivateErr
	}
	return s.inner.Reactivate(kid)
}

// TestTrustedKeysHandler_Register5xxDoesNotLeakRawError guards #68 item 14:
// a backend persistence error must not echo into the HTTP response body. The
// previous handler returned fmt.Sprintf("failed to register key: %s", err) at
// status 500, leaking arbitrary internal error strings (potentially KV
// connection details).
func TestTrustedKeysHandler_Register5xxDoesNotLeakRawError(t *testing.T) {
	store := &errorReturningTrustedKeyStore{
		inner:       NewInMemoryTrustedKeyStore(),
		registerErr: fmt.Errorf("postgres: connection to db.internal:5432 refused: secret-token-xyz"),
	}
	handler := NewTrustedKeysHandler(store)

	body := registerTrustedKeyRequest{
		KeyID:     "leak-test",
		JWK:       generateTestJWK(t),
		Audience:  "svc",
		ValidFrom: strPtr("2026-01-01T00:00:00Z"),
	}
	bodyBytes, _ := json.Marshal(body)
	req := adminReq(http.MethodPost, "/oauth/keys/trusted", bytes.NewReader(bodyBytes))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d: %s", rec.Code, rec.Body.String())
	}
	body2 := rec.Body.String()
	for _, leaked := range []string{"postgres", "db.internal", "5432", "secret-token-xyz", "connection"} {
		if strings.Contains(body2, leaked) {
			t.Errorf("response body leaked %q: %q", leaked, body2)
		}
	}
}

// TestTrustedKeysHandler_RegisterForwardsAppErrorStatus verifies that a 409
// returned by the store (e.g. registry-full from #34/2, or future
// cross-tenant KID collision per the cloud contract) reaches the client as
// 409, not as 500.
func TestTrustedKeysHandler_RegisterForwardsAppErrorStatus(t *testing.T) {
	store := &errorReturningTrustedKeyStore{
		inner:       NewInMemoryTrustedKeyStore(),
		registerErr: common.Operational(http.StatusConflict, common.ErrCodeConflict, "trusted-key registry full"),
	}
	handler := NewTrustedKeysHandler(store)

	body := registerTrustedKeyRequest{
		KeyID:     "dupe",
		JWK:       generateTestJWK(t),
		Audience:  "svc",
		ValidFrom: strPtr("2026-01-01T00:00:00Z"),
	}
	bodyBytes, _ := json.Marshal(body)
	req := adminReq(http.MethodPost, "/oauth/keys/trusted", bytes.NewReader(bodyBytes))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestTrustedKeysHandler_Invalidate404DoesNotLeakRawError guards #68 item 14
// for the invalidate 404 site.
func TestTrustedKeysHandler_Invalidate404DoesNotLeakRawError(t *testing.T) {
	store := &errorReturningTrustedKeyStore{
		inner:         NewInMemoryTrustedKeyStore(),
		invalidateErr: fmt.Errorf("storage backend leaks: secret-internal-detail"),
	}
	handler := NewTrustedKeysHandler(store)

	req := adminReq(http.MethodPost, "/oauth/keys/trusted/some-kid/invalidate", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if strings.Contains(rec.Body.String(), "secret-internal-detail") {
		t.Errorf("response leaked raw error: %q", rec.Body.String())
	}
}

// TestTrustedKeysHandler_Reactivate404DoesNotLeakRawError guards #68 item 14
// for the reactivate 404 site.
func TestTrustedKeysHandler_Reactivate404DoesNotLeakRawError(t *testing.T) {
	store := &errorReturningTrustedKeyStore{
		inner:         NewInMemoryTrustedKeyStore(),
		reactivateErr: fmt.Errorf("storage backend leaks: secret-internal-detail"),
	}
	handler := NewTrustedKeysHandler(store)

	req := adminReq(http.MethodPost, "/oauth/keys/trusted/some-kid/reactivate", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if strings.Contains(rec.Body.String(), "secret-internal-detail") {
		t.Errorf("response leaked raw error: %q", rec.Body.String())
	}
}

// TestParseRSAPublicKeyFromJWK_RejectsOversizedExponent guards against silent
// truncation when an RSA public-key exponent does not fit in int (#34 item 4).
// Today's code does int(big.Int.Int64()) which silently mis-decodes anything
// > MaxInt; verification then fails downstream in a confusing way. Validate
// explicitly: positive, fits in int, odd.
func TestParseRSAPublicKeyFromJWK_RejectsOversizedExponent(t *testing.T) {
	cases := []struct {
		name    string
		eBase64 string
	}{
		// 8 bytes = 2^63 - cannot fit in signed int64 / int.
		{"too-large", base64.RawURLEncoding.EncodeToString([]byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff})},
		// 0 — not positive.
		{"zero", base64.RawURLEncoding.EncodeToString([]byte{0x00})},
		// 4 — even, invalid as RSA public exponent.
		{"even", base64.RawURLEncoding.EncodeToString([]byte{0x04})},
	}

	// Use a real modulus to isolate exponent validation from N-validation.
	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	nB64 := base64.RawURLEncoding.EncodeToString(rsaKey.N.Bytes())

	for _, tc := range cases {
		jwk := json.RawMessage(`{"kty":"RSA","n":"` + nB64 + `","e":"` + tc.eBase64 + `"}`)
		_, err := parseRSAPublicKeyFromJWK(jwk)
		if err == nil {
			t.Errorf("%s: expected error, got nil", tc.name)
		}
	}
}

// TestDeserializeTrustedKey_RejectsOversizedExponent mirrors the JWK guard for
// the persistence boundary (#34 item 4). A corrupt KV record must be rejected
// rather than silently truncated to a different exponent.
func TestDeserializeTrustedKey_RejectsOversizedExponent(t *testing.T) {
	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	nB64 := base64.RawURLEncoding.EncodeToString(rsaKey.N.Bytes())
	// 9 bytes = > 64 bits, cannot fit in int.
	eOverflow := base64.RawURLEncoding.EncodeToString([]byte{0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00})
	rec := trustedKeyRecord{
		KID:       "bad-e",
		Audience:  "svc",
		Active:    true,
		ValidFrom: "2026-01-01T00:00:00Z",
		N:         nB64,
		E:         eOverflow,
	}
	data, _ := json.Marshal(rec)
	_, err = deserializeTrustedKey(data)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
}

func TestTrustedKeysHandler_RegisterInvalidJWK(t *testing.T) {
	store := NewInMemoryTrustedKeyStore()
	handler := NewTrustedKeysHandler(store)

	body := registerTrustedKeyRequest{
		KeyID:     "bad-key",
		JWK:       json.RawMessage(`{"kty":"RSA"}`),
		Audience:  "svc",
		ValidFrom: strPtr("2026-01-01T00:00:00Z"),
	}
	bodyBytes, _ := json.Marshal(body)

	req := adminReq(http.MethodPost, "/oauth/keys/trusted", bytes.NewReader(bodyBytes))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestTrustedKeysHandler_UnmatchedRoute_ReturnsRFC9457NotFound covers the
// last surviving http.Error site in ServeHTTP: a request that authenticates
// but matches no method/path branch must come back as a problem-detail 404
// with errorCode=NOT_FOUND, not a plain text/plain body. Otherwise admin
// clients see a different content negotiation here than they do for every
// other 4xx in the handler.
func TestTrustedKeysHandler_UnmatchedRoute_ReturnsRFC9457NotFound(t *testing.T) {
	handler := NewTrustedKeysHandler(NewInMemoryTrustedKeyStore())

	// PUT is not handled by any branch in ServeHTTP.
	req := adminReq(http.MethodPut, "/oauth/keys/trusted", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d (body=%q)", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/problem+json" {
		t.Fatalf("expected Content-Type=application/problem+json, got %q", ct)
	}
	var pd common.ProblemDetail
	if err := json.NewDecoder(rec.Body).Decode(&pd); err != nil {
		t.Fatalf("failed to decode problem-detail: %v", err)
	}
	code, _ := pd.Props["errorCode"].(string)
	if code != common.ErrCodeNotFound {
		t.Errorf("expected errorCode=%s, got %s", common.ErrCodeNotFound, code)
	}
}

func TestTrustedKeysHandler_DeleteNotFound(t *testing.T) {
	store := NewInMemoryTrustedKeyStore()
	handler := NewTrustedKeysHandler(store)

	req := adminReq(http.MethodDelete, "/oauth/keys/trusted/nonexistent", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestTrustedKeysHandler_DeleteNotFound_GenericMessage guards #34 item 6:
// the 404 response detail must carry a generic "key not found" message rather
// than the raw store error string. The previous handler called
// http.Error(w, err.Error(), 404) which leaked the store's internal phrasing
// ("trusted key not found: <kid>") into the response body. The instance
// field still legitimately echoes the URL path (RFC 9457).
//
// Also pins the errorCode to TRUSTED_KEY_NOT_FOUND (#34/6 follow-up): the
// original landing of #34/6 emitted BAD_REQUEST as the errorCode on a 404
// status, which was incoherent. The dedicated TRUSTED_KEY_NOT_FOUND code
// makes the response programmatically distinguishable from BAD_REQUEST 400s.
func TestTrustedKeysHandler_DeleteNotFound_GenericMessage(t *testing.T) {
	store := NewInMemoryTrustedKeyStore()
	handler := NewTrustedKeysHandler(store)

	req := adminReq(http.MethodDelete, "/oauth/keys/trusted/some-kid", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", rec.Code, rec.Body.String())
	}

	var pd struct {
		Detail     string         `json:"detail"`
		Properties map[string]any `json:"properties"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &pd); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if !strings.Contains(pd.Detail, "key not found") {
		t.Errorf("detail = %q, want contains 'key not found'", pd.Detail)
	}
	// The detail must NOT contain the raw store error phrasing.
	if strings.Contains(pd.Detail, "trusted key not found:") {
		t.Errorf("detail leaked raw store error: %q", pd.Detail)
	}
	if got, _ := pd.Properties["errorCode"].(string); got != common.ErrCodeTrustedKeyNotFound {
		t.Errorf("errorCode = %q, want %q", got, common.ErrCodeTrustedKeyNotFound)
	}
}

// TestTrustedKeysHandler_LifecycleEnforcesKIDWhitelist covers MED-4: every
// lifecycle endpoint (Register, Delete, Invalidate, Reactivate) must apply
// the same {keyId} character whitelist. Previously only Register validated
// the input, leaving DELETE/Invalidate/Reactivate accepting arbitrary
// strings — including ones that could produce noisy logs or downstream
// store errors with attacker-controlled fragments.
//
// Whitelist: alnum + '-' + '_' + '.', length 1..128.
func TestTrustedKeysHandler_LifecycleEnforcesKIDWhitelist(t *testing.T) {
	// Path-safe but disallowed: a tilde is not in the whitelist set.
	const malformed = "kid~with~tilde"

	type tcase struct {
		name string
		req  func() *http.Request
	}
	jwk := generateTestJWK(t)
	body := registerTrustedKeyRequest{
		KeyID:    malformed,
		JWK:      jwk,
		Audience: "svc",
	}
	bodyBytes, _ := json.Marshal(body)
	cases := []tcase{
		{"register", func() *http.Request {
			req := adminReq(http.MethodPost, "/oauth/keys/trusted", bytes.NewReader(bodyBytes))
			req.Header.Set("Content-Type", "application/json")
			return req
		}},
		{"delete", func() *http.Request {
			return adminReq(http.MethodDelete, "/oauth/keys/trusted/"+malformed, nil)
		}},
		{"invalidate", func() *http.Request {
			return adminReq(http.MethodPost, "/oauth/keys/trusted/"+malformed+"/invalidate", nil)
		}},
		{"reactivate", func() *http.Request {
			return adminReq(http.MethodPost, "/oauth/keys/trusted/"+malformed+"/reactivate", nil)
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			handler := NewTrustedKeysHandler(NewInMemoryTrustedKeyStore())
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, tc.req())

			if rec.Code != http.StatusBadRequest {
				t.Fatalf("expected 400 for malformed keyId, got %d (body=%q)", rec.Code, rec.Body.String())
			}
			if ct := rec.Header().Get("Content-Type"); ct != "application/problem+json" {
				t.Fatalf("expected Content-Type=application/problem+json, got %q", ct)
			}
			var pd common.ProblemDetail
			if err := json.NewDecoder(rec.Body).Decode(&pd); err != nil {
				t.Fatalf("failed to decode problem-detail: %v", err)
			}
			code, _ := pd.Props["errorCode"].(string)
			if code != common.ErrCodeBadRequest {
				t.Errorf("expected errorCode=%s, got %s", common.ErrCodeBadRequest, code)
			}
		})
	}
}
