package auth

import (
	"context"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/common"
)

const trustedKeysNamespace = "trusted-keys"

// defaultMaxTrustedKeys caps the number of trusted keys a store will accept by
// default per tenant. Trusted keys are an admin-managed registry — a 100-key
// default covers expected operational use (rotations, multi-issuer federation)
// and defends against runaway registration if the admin endpoint is ever
// misconfigured. Override via WithMaxTrustedKeys.
const defaultMaxTrustedKeys = 100

// KVTrustedKeyStoreOption configures a KVTrustedKeyStore at construction time.
type KVTrustedKeyStoreOption func(*kvTrustedKeyStoreConfig)

type kvTrustedKeyStoreConfig struct {
	maxTrustedKeys int
}

// WithMaxTrustedKeys overrides the default per-tenant cap on registered trusted
// keys. Values <= 0 disable the cap (registration becomes unbounded — only use
// this in tests that exercise the unbounded path; production deployments must
// keep the default).
func WithMaxTrustedKeys(n int) KVTrustedKeyStoreOption {
	return func(c *kvTrustedKeyStoreConfig) {
		c.maxTrustedKeys = n
	}
}

// trustedKeyKey returns the KV key (within trustedKeysNamespace) for a
// (tenantID, kid). Layout "<tenantID>:<kid>" makes tenant isolation a
// storage-layer invariant.
func trustedKeyKey(tenantID spi.TenantID, kid string) string {
	return string(tenantID) + ":" + kid
}

// trustedKeyRecord is the JSON-serializable form of a TrustedKey.
type trustedKeyRecord struct {
	KID      string         `json:"kid"`
	TenantID string         `json:"tenantID,omitempty"`
	JWK      map[string]any `json:"jwk,omitempty"`
	Audience string         `json:"audience"`
	Issuers  []string       `json:"issuers,omitempty"`
	Active   bool           `json:"active"`
	// validFrom / validTo stored as RFC3339Nano strings for precision.
	ValidFrom string  `json:"validFrom"`
	ValidTo   *string `json:"validTo,omitempty"`
	// RSA public key in JWK-like format.
	N string `json:"n"` // base64url-encoded modulus
	E string `json:"e"` // base64url-encoded exponent
}

// KVTrustedKeyStore persists trusted keys via a KeyValueStore backend.
// It keeps an in-memory cache for fast reads and writes through to KV on
// mutations. All operations are tenant-scoped: KV keys use the layout
// "<tenantID>:<kid>" and cache reads verify the stored TenantID.
type KVTrustedKeyStore struct {
	mu   sync.RWMutex
	keys map[string]*TrustedKey // keyed by kid (KV key within namespace is tenantID:kid)
	kv   spi.KeyValueStore
	// ctx is a long-lived system context for KV operations. It is stored here
	// because the TrustedKeyStore interface does not accept context parameters.
	// This context must never carry cancellation or deadlines.
	ctx context.Context
	// maxPerTenant is the per-tenant cap enforced by Register; <=0 means unbounded.
	maxPerTenant int
}

// NewKVTrustedKeyStore creates a KVTrustedKeyStore, loading any existing keys
// from the KV backend. Pass WithMaxTrustedKeys to override the default cap.
func NewKVTrustedKeyStore(ctx context.Context, kv spi.KeyValueStore, opts ...KVTrustedKeyStoreOption) (*KVTrustedKeyStore, error) {
	cfg := kvTrustedKeyStoreConfig{maxTrustedKeys: defaultMaxTrustedKeys}
	for _, opt := range opts {
		opt(&cfg)
	}
	s := &KVTrustedKeyStore{
		keys: make(map[string]*TrustedKey),
		kv:   kv,
		// context.WithoutCancel ensures the long-lived store ctx never propagates
		// a cancellation or deadline from the caller. Defence in depth: a future
		// caller passing a request-scoped ctx would otherwise silently abort KV
		// operations on request completion.
		ctx:          context.WithoutCancel(ctx),
		maxPerTenant: cfg.maxTrustedKeys,
	}
	if err := s.loadAll(); err != nil {
		return nil, fmt.Errorf("failed to load trusted keys from KV store: %w", err)
	}
	return s, nil
}

func (s *KVTrustedKeyStore) loadAll() error {
	entries, err := s.kv.List(s.ctx, trustedKeysNamespace)
	if err != nil {
		return err
	}
	skipped := 0
	for kvKey, data := range entries {
		// Skip legacy entries: those stored before tenant-scoping used bare <kid>
		// keys with no ":" separator and no tenantID in the record.
		if !strings.Contains(kvKey, ":") {
			skipped++
			continue
		}
		tk, err := deserializeTrustedKey(data)
		if err != nil {
			return fmt.Errorf("failed to deserialize trusted key %q: %w", kvKey, err)
		}
		if tk.TenantID == "" {
			skipped++
			continue
		}
		s.keys[tk.KID] = tk
	}
	if skipped > 0 {
		slog.Warn("skipped pre-v0.8.0 trusted-key entries without tenant scope",
			"count", skipped,
			"namespace", trustedKeysNamespace)
	}
	return nil
}

// loadOne loads a single trusted key for the given tenant from the KV backend
// into the in-memory cache. Returns an error if not found or tenant mismatch.
func (s *KVTrustedKeyStore) loadOne(tenantID spi.TenantID, kid string) error {
	data, err := s.kv.Get(s.ctx, trustedKeysNamespace, trustedKeyKey(tenantID, kid))
	if err != nil {
		return err
	}
	tk, err := deserializeTrustedKey(data)
	if err != nil {
		return fmt.Errorf("failed to deserialize trusted key %s:%s: %w", tenantID, kid, err)
	}
	if tk.TenantID != tenantID {
		return fmt.Errorf("trusted key not found: %s", kid)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.keys[kid] = tk
	return nil
}

func (s *KVTrustedKeyStore) persistWithKey(kvKey string, tk *TrustedKey) error {
	data, err := serializeTrustedKey(tk)
	if err != nil {
		return err
	}
	return s.kv.Put(s.ctx, trustedKeysNamespace, kvKey, data)
}

// Register adds or replaces a trusted key and persists it. Per the cyoda
// cloud trusted-key contract this is an upsert keyed on KID — re-registering
// an existing KID atomically replaces the JWK material under the same record,
// which makes the endpoint idempotent / retry-safe during key rotation.
//
// Cross-tenant KID collision returns 409 KEY_OWNED_BY_DIFFERENT_TENANT.
// Per-tenant cap (counts only currently-valid keys, excluding the KID being
// registered so same-KID upserts don't consume a slot) returns
// 400 TRUSTED_KEY_CAP_REACHED. When opts.Invalidate is true, all other active
// siblings in the same tenant are marked inactive with a gracePeriod ValidTo.
//
// Write order: new key FIRST, then siblings. This guarantees that a KV failure
// mid-sibling-flip never destroys the only active key:
//   - new-key write fails → no state change (rollback safe).
//   - sibling-flip fails → new key is active; stale siblings remain active
//     (operator can retry Register to clean up — strictly better than the
//     inverse where siblings are dead with no replacement).
func (s *KVTrustedKeyStore) Register(tk *TrustedKey, opts RotateOptions) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Cross-tenant collision guard.
	if existing, ok := s.keys[tk.KID]; ok && existing.TenantID != tk.TenantID {
		return common.Operational(http.StatusConflict, common.ErrCodeKeyOwnedByDifferentTenant, "key with this keyId belongs to a different tenant")
	}

	// Per-tenant cap: count only currently-valid keys (excluding the KID being
	// registered so same-KID upserts don't consume a slot).
	if s.maxPerTenant > 0 {
		now := time.Now()
		count := 0
		for _, k := range s.keys {
			if k.TenantID != tk.TenantID || k.KID == tk.KID {
				continue
			}
			if !k.Active {
				continue
			}
			if k.ValidTo != nil && !now.Before(*k.ValidTo) {
				continue
			}
			count++
		}
		if count >= s.maxPerTenant {
			return common.Operational(http.StatusBadRequest, common.ErrCodeTrustedKeyCapReached, "trusted-key cap reached for tenant")
		}
	}

	// Step 1: persist the new/updated entry to KV FIRST.
	// If this fails, no state has been mutated — safe to return.
	copied := *tk
	kvKey := trustedKeyKey(tk.TenantID, tk.KID)
	if err := s.persistWithKey(kvKey, &copied); err != nil {
		return fmt.Errorf("failed to persist trusted key: %w", err)
	}
	// Commit to cache after successful KV write.
	s.keys[copied.KID] = &copied

	// Step 2: invalidate siblings (best-effort). A sibling KV write failure
	// leaves the new key active (already committed above) and the sibling
	// unchanged — operator can retry Register to clean up stragglers.
	if opts.Invalidate {
		now := time.Now()
		expiry := now.Add(time.Duration(opts.GracePeriodSec) * time.Second)
		var failed []string
		for _, k := range s.keys {
			if k.TenantID != tk.TenantID || !k.Active || k.KID == tk.KID {
				continue
			}
			// Clone, mutate, persist to KV.
			sibling := *k
			sibling.Active = false
			e := expiry
			sibling.ValidTo = &e
			if err := s.persistWithKey(trustedKeyKey(k.TenantID, k.KID), &sibling); err != nil {
				failed = append(failed, k.KID)
				continue
			}
			// Commit sibling to cache after successful KV write.
			*k = sibling
		}
		if len(failed) > 0 {
			return fmt.Errorf("key %s registered, but failed to invalidate siblings %v (retry Register or invalidate manually)", tk.KID, failed)
		}
	}

	return nil
}

// Get retrieves a trusted key by tenant and KID. Cache hits verify that the
// cached entry belongs to the caller's tenant; cross-tenant cache hits fall
// through to a KV lookup. If the key is not in cache, it attempts to load it
// from the KV backend (multi-node visibility: a key registered on another node
// after this store was constructed will be found here).
func (s *KVTrustedKeyStore) Get(tenantID spi.TenantID, kid string) (*TrustedKey, error) {
	// Critical section 1: cache lookup.
	cached, ok := func() (*TrustedKey, bool) {
		s.mu.RLock()
		defer s.mu.RUnlock()
		v, ok := s.keys[kid]
		return v, ok
	}()

	if ok {
		if cached.TenantID == tenantID {
			return copyTrustedKey(cached), nil
		}
		// Cross-tenant cache hit — this tenant doesn't own this KID.
		return nil, fmt.Errorf("trusted key not found: %s", kid)
	}

	// Cache miss — try loading from KV backend (may have been registered on
	// another node after this instance started). Slow I/O outside any lock.
	if err := s.loadOne(tenantID, kid); err != nil {
		return nil, fmt.Errorf("trusted key not found: %s", kid)
	}

	// Critical section 2: re-read from cache after load.
	tk, ok := func() (*TrustedKey, bool) {
		s.mu.RLock()
		defer s.mu.RUnlock()
		v, ok := s.keys[kid]
		return v, ok
	}()
	if !ok || tk.TenantID != tenantID {
		return nil, fmt.Errorf("trusted key not found: %s", kid)
	}
	return copyTrustedKey(tk), nil
}

// List returns all trusted keys for the given tenant.
func (s *KVTrustedKeyStore) List(tenantID spi.TenantID) []*TrustedKey {
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := make([]*TrustedKey, 0)
	for _, tk := range s.keys {
		if tk.TenantID == tenantID {
			result = append(result, copyTrustedKey(tk))
		}
	}
	return result
}

// ListForVerification returns keys still within their validity window across
// all tenants. Used to populate the JWKS endpoint during grace periods.
func (s *KVTrustedKeyStore) ListForVerification() []*TrustedKey {
	s.mu.RLock()
	defer s.mu.RUnlock()
	now := time.Now()
	out := make([]*TrustedKey, 0, len(s.keys))
	for _, tk := range s.keys {
		if tk.ValidTo == nil || now.Before(*tk.ValidTo) {
			out = append(out, copyTrustedKey(tk))
		}
	}
	return out
}

// Delete removes a trusted key by tenant and KID. Returns an error if the key
// does not exist or belongs to a different tenant.
func (s *KVTrustedKeyStore) Delete(tenantID spi.TenantID, kid string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tk, ok := s.keys[kid]
	if !ok || tk.TenantID != tenantID {
		return fmt.Errorf("trusted key not found: %s", kid)
	}
	if err := s.kv.Delete(s.ctx, trustedKeysNamespace, trustedKeyKey(tenantID, kid)); err != nil {
		return fmt.Errorf("failed to delete trusted key from KV store: %w", err)
	}
	delete(s.keys, kid)
	return nil
}

// Invalidate marks a trusted key as inactive, sets ValidTo to
// now+gracePeriodSec, and persists. Returns an error if the key does not
// exist or belongs to a different tenant.
func (s *KVTrustedKeyStore) Invalidate(tenantID spi.TenantID, kid string, gracePeriodSec int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tk, ok := s.keys[kid]
	if !ok || tk.TenantID != tenantID {
		return fmt.Errorf("trusted key not found: %s", kid)
	}
	// Clone, mutate, persist to KV first (rollback safety).
	updated := *tk
	updated.Active = false
	expiry := time.Now().Add(time.Duration(gracePeriodSec) * time.Second)
	updated.ValidTo = &expiry
	if err := s.persistWithKey(trustedKeyKey(tenantID, kid), &updated); err != nil {
		return fmt.Errorf("failed to persist invalidation: %w", err)
	}
	// Commit to cache after successful KV write.
	s.keys[kid] = &updated
	return nil
}

// Reactivate sets a trusted key as active, updates its validity window, and
// persists. Returns an error if the key does not exist or belongs to a
// different tenant. Enforces the same contract as InMemoryTrustedKeyStore:
// validTo is required (non-zero), must be strictly in the future, and must
// be after validFrom.
func (s *KVTrustedKeyStore) Reactivate(tenantID spi.TenantID, kid string, validFrom, validTo time.Time) error {
	if validTo.IsZero() {
		return fmt.Errorf("validTo required for reactivation")
	}
	if !validTo.After(time.Now()) {
		return fmt.Errorf("validTo must be in the future")
	}
	if !validTo.After(validFrom) {
		return fmt.Errorf("validTo must be after validFrom")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tk, ok := s.keys[kid]
	if !ok || tk.TenantID != tenantID {
		return fmt.Errorf("trusted key not found: %s", kid)
	}
	// Clone, mutate, persist to KV first (rollback safety).
	updated := *tk
	updated.Active = true
	updated.ValidFrom = validFrom
	vt := validTo
	updated.ValidTo = &vt
	if err := s.persistWithKey(trustedKeyKey(tenantID, kid), &updated); err != nil {
		return fmt.Errorf("failed to persist reactivation: %w", err)
	}
	// Commit to cache after successful KV write.
	s.keys[kid] = &updated
	return nil
}

// --- Serialization ---

func serializeTrustedKey(tk *TrustedKey) ([]byte, error) {
	rec := trustedKeyRecord{
		KID:       tk.KID,
		TenantID:  string(tk.TenantID),
		JWK:       tk.JWK,
		Audience:  tk.Audience,
		Issuers:   tk.Issuers,
		Active:    tk.Active,
		ValidFrom: tk.ValidFrom.UTC().Format(time.RFC3339Nano),
		N:         encodeBase64URL(tk.PublicKey.N.Bytes()),
		E:         encodeBase64URL(big.NewInt(int64(tk.PublicKey.E)).Bytes()),
	}
	if tk.ValidTo != nil {
		s := tk.ValidTo.UTC().Format(time.RFC3339Nano)
		rec.ValidTo = &s
	}
	return json.Marshal(rec)
}

func deserializeTrustedKey(data []byte) (*TrustedKey, error) {
	var rec trustedKeyRecord
	if err := json.Unmarshal(data, &rec); err != nil {
		return nil, err
	}

	nBytes, err := decodeBase64URL(rec.N)
	if err != nil {
		return nil, fmt.Errorf("invalid n value: %w", err)
	}
	eBytes, err := decodeBase64URL(rec.E)
	if err != nil {
		return nil, fmt.Errorf("invalid e value: %w", err)
	}

	eVal, err := validateRSAPublicExponent(new(big.Int).SetBytes(eBytes))
	if err != nil {
		return nil, fmt.Errorf("invalid e value: %w", err)
	}
	pubKey := &rsa.PublicKey{
		N: new(big.Int).SetBytes(nBytes),
		E: eVal,
	}

	validFrom, err := time.Parse(time.RFC3339Nano, rec.ValidFrom)
	if err != nil {
		return nil, fmt.Errorf("invalid validFrom: %w", err)
	}

	var validTo *time.Time
	if rec.ValidTo != nil {
		t, err := time.Parse(time.RFC3339Nano, *rec.ValidTo)
		if err != nil {
			return nil, fmt.Errorf("invalid validTo: %w", err)
		}
		validTo = &t
	}

	return &TrustedKey{
		KID:       rec.KID,
		TenantID:  spi.TenantID(rec.TenantID),
		JWK:       rec.JWK,
		PublicKey: pubKey,
		Audience:  rec.Audience,
		Issuers:   rec.Issuers,
		Active:    rec.Active,
		ValidFrom: validFrom,
		ValidTo:   validTo,
	}, nil
}

func encodeBase64URL(data []byte) string {
	return base64.RawURLEncoding.EncodeToString(data)
}

func copyTrustedKey(tk *TrustedKey) *TrustedKey {
	copied := *tk
	if tk.Issuers != nil {
		copied.Issuers = make([]string, len(tk.Issuers))
		copy(copied.Issuers, tk.Issuers)
	}
	if tk.PublicKey != nil {
		pubCopy := *tk.PublicKey
		pubCopy.N = new(big.Int).Set(tk.PublicKey.N)
		copied.PublicKey = &pubCopy
	}
	if tk.ValidTo != nil {
		vt := *tk.ValidTo
		copied.ValidTo = &vt
	}
	if tk.JWK != nil {
		jwkCopy := make(map[string]any, len(tk.JWK))
		for k, v := range tk.JWK {
			jwkCopy[k] = v
		}
		copied.JWK = jwkCopy
	}
	return &copied
}

// TrustedKeyKVKeyForTesting exposes trustedKeyKey for cross-package tests
// that need to predict KV keys (e.g. for injection mocks).
func TrustedKeyKVKeyForTesting(tenantID spi.TenantID, kid string) string {
	return trustedKeyKey(tenantID, kid)
}
