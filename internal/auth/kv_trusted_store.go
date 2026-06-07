package auth

import (
	"context"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"sync"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/common"
)

const trustedKeysNamespace = "trusted-keys"

// defaultMaxTrustedKeys caps the number of trusted keys a store will accept by
// default. Trusted keys are an admin-managed registry — a 100-key default
// covers expected operational use (rotations, multi-issuer federation) and
// defends against runaway registration if the admin endpoint is ever
// misconfigured. Override via WithMaxTrustedKeys.
//
// TODO(#163): when the trusted-key registry becomes tenant-scoped, move this
// cap to per-tenant; otherwise a hostile admin in one tenant can lock out
// every other tenant from registering keys.
const defaultMaxTrustedKeys = 100

// KVTrustedKeyStoreOption configures a KVTrustedKeyStore at construction time.
type KVTrustedKeyStoreOption func(*kvTrustedKeyStoreConfig)

type kvTrustedKeyStoreConfig struct {
	maxTrustedKeys int
}

// WithMaxTrustedKeys overrides the default cap on registered trusted keys.
// Values <= 0 disable the cap (registration becomes unbounded — only use this
// in tests that exercise the unbounded path; production deployments must keep
// the default).
func WithMaxTrustedKeys(n int) KVTrustedKeyStoreOption {
	return func(c *kvTrustedKeyStoreConfig) {
		c.maxTrustedKeys = n
	}
}

// trustedKeyRecord is the JSON-serializable form of a TrustedKey.
type trustedKeyRecord struct {
	KID       string   `json:"kid"`
	Audience  string   `json:"audience"`
	Issuers   []string `json:"issuers,omitempty"`
	Active    bool     `json:"active"`
	ValidFrom string   `json:"validFrom"`
	ValidTo   *string  `json:"validTo,omitempty"`
	// RSA public key in JWK-like format.
	N string `json:"n"` // base64url-encoded modulus
	E string `json:"e"` // base64url-encoded exponent
}

// KVTrustedKeyStore persists trusted keys via a KeyValueStore backend.
// It keeps an in-memory cache for fast reads and writes through to KV on mutations.
type KVTrustedKeyStore struct {
	mu   sync.RWMutex
	keys map[string]*TrustedKey
	kv   spi.KeyValueStore
	// ctx is a long-lived system context for KV operations. It is stored here
	// because the TrustedKeyStore interface does not accept context parameters.
	// This context must never carry cancellation or deadlines.
	ctx context.Context
	// maxTrustedKeys is the cap enforced by Register; <=0 means unbounded.
	maxTrustedKeys int
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
		// a cancellation or deadline from the caller (#34 item 5). Defence in
		// depth: a future caller passing a request-scoped ctx would otherwise
		// silently abort KV operations on request completion.
		ctx:            context.WithoutCancel(ctx),
		maxTrustedKeys: cfg.maxTrustedKeys,
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
	for _, data := range entries {
		tk, err := deserializeTrustedKey(data)
		if err != nil {
			return fmt.Errorf("failed to deserialize trusted key: %w", err)
		}
		s.keys[tk.KID] = tk
	}
	return nil
}

// loadOne loads a single trusted key from the KV backend into the in-memory cache.
func (s *KVTrustedKeyStore) loadOne(kid string) error {
	data, err := s.kv.Get(s.ctx, trustedKeysNamespace, kid)
	if err != nil {
		return err
	}
	tk, err := deserializeTrustedKey(data)
	if err != nil {
		return fmt.Errorf("failed to deserialize trusted key %s: %w", kid, err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.keys[tk.KID] = tk
	return nil
}

func (s *KVTrustedKeyStore) persist(tk *TrustedKey) error {
	data, err := serializeTrustedKey(tk)
	if err != nil {
		return err
	}
	return s.kv.Put(s.ctx, trustedKeysNamespace, tk.KID, data)
}

// Register adds or replaces a trusted key and persists it. Per the cyoda
// cloud trusted-key contract this is an upsert keyed on KID — re-registering
// an existing KID atomically replaces the JWK material under the same record,
// which makes the endpoint idempotent / retry-safe during key rotation.
//
// 409 Conflict is reserved for the registry-full guard. Cross-tenant KID
// collision and RotateOptions.Invalidate are handled in the InMemoryTrustedKeyStore
// today; deep tenant-scoping of KVTrustedKeyStore is tracked in Task 8.
//
// The cap check only fires on insert (new KID), not on upsert — replacing
// an existing record does not grow the registry.
//
// TODO(#281-task8): tenant-scope Register; honour opts.Invalidate sibling
// invalidation and cross-tenant collision guard.
func (s *KVTrustedKeyStore) Register(tk *TrustedKey, _ RotateOptions) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	_, exists := s.keys[tk.KID]
	if !exists && s.maxTrustedKeys > 0 && len(s.keys) >= s.maxTrustedKeys {
		return common.Operational(http.StatusConflict, common.ErrCodeConflict, "trusted-key registry full")
	}

	copied := copyTrustedKey(tk)
	if err := s.persist(copied); err != nil {
		return fmt.Errorf("failed to persist trusted key: %w", err)
	}
	s.keys[copied.KID] = copied
	return nil
}

// Get retrieves a trusted key by KID. The tenantID parameter is accepted for
// interface compliance but tenant isolation is not yet enforced here — Task 8
// will add the tenant check.
//
// If the key is not in the in-memory cache, it attempts to load it from the KV
// backend. This handles the multi-node case where a key was registered on
// another node after this store was constructed.
//
// TODO(#281-task8): tenant-scope Get; reject keys owned by different tenant.
func (s *KVTrustedKeyStore) Get(_ spi.TenantID, kid string) (*TrustedKey, error) {
	s.mu.RLock()
	tk, ok := s.keys[kid]
	s.mu.RUnlock()
	if ok {
		return copyTrustedKey(tk), nil
	}

	// Cache miss — try loading from KV backend (may have been registered on another node).
	if err := s.loadOne(kid); err != nil {
		return nil, fmt.Errorf("trusted key not found: %s", kid)
	}

	s.mu.RLock()
	defer s.mu.RUnlock()
	tk, ok = s.keys[kid]
	if !ok {
		return nil, fmt.Errorf("trusted key not found: %s", kid)
	}
	return copyTrustedKey(tk), nil
}

// List returns all trusted keys. The tenantID parameter is accepted for
// interface compliance but tenant filtering is not yet enforced here — Task 8
// will add per-tenant filtering.
//
// TODO(#281-task8): tenant-scope List; return only keys owned by tenantID.
func (s *KVTrustedKeyStore) List(_ spi.TenantID) []*TrustedKey {
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := make([]*TrustedKey, 0, len(s.keys))
	for _, tk := range s.keys {
		result = append(result, copyTrustedKey(tk))
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

// Delete removes a trusted key and persists the deletion. The tenantID
// parameter is accepted for interface compliance but tenant isolation is not
// yet enforced here — Task 8 will add the tenant check.
//
// TODO(#281-task8): tenant-scope Delete; reject operations from different tenant.
func (s *KVTrustedKeyStore) Delete(_ spi.TenantID, kid string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.keys[kid]; !ok {
		return fmt.Errorf("trusted key not found: %s", kid)
	}
	if err := s.kv.Delete(s.ctx, trustedKeysNamespace, kid); err != nil {
		return fmt.Errorf("failed to delete trusted key from KV store: %w", err)
	}
	delete(s.keys, kid)
	return nil
}

// Invalidate marks a trusted key as inactive and persists. The tenantID
// parameter is accepted for interface compliance but tenant isolation is not
// yet enforced here — Task 8 will add the tenant check and gracePeriodSec
// support.
//
// TODO(#281-task8): tenant-scope Invalidate; apply gracePeriodSec ValidTo.
func (s *KVTrustedKeyStore) Invalidate(_ spi.TenantID, kid string, _ int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tk, ok := s.keys[kid]
	if !ok {
		return fmt.Errorf("trusted key not found: %s", kid)
	}
	tk.Active = false
	if err := s.persist(tk); err != nil {
		tk.Active = true // rollback in-memory on failure
		return fmt.Errorf("failed to persist invalidation: %w", err)
	}
	return nil
}

// Reactivate marks a trusted key as active and persists. The tenantID,
// validFrom, and validTo parameters are accepted for interface compliance but
// tenant isolation and window updates are not yet enforced here — Task 8 will
// implement those.
//
// TODO(#281-task8): tenant-scope Reactivate; apply validFrom/validTo window.
func (s *KVTrustedKeyStore) Reactivate(_ spi.TenantID, kid string, _, _ time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tk, ok := s.keys[kid]
	if !ok {
		return fmt.Errorf("trusted key not found: %s", kid)
	}
	tk.Active = true
	if err := s.persist(tk); err != nil {
		tk.Active = false // rollback in-memory on failure
		return fmt.Errorf("failed to persist reactivation: %w", err)
	}
	return nil
}

// --- Serialization ---

func serializeTrustedKey(tk *TrustedKey) ([]byte, error) {
	rec := trustedKeyRecord{
		KID:       tk.KID,
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
	return &copied
}
