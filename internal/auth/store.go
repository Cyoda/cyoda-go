package auth

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/hex"
	"fmt"
	"net/http"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/common"
)

// --- Types ---

// KeyPair holds an RSA key pair with metadata.
type KeyPair struct {
	KID        string
	Audience   string // "human" | "client"
	Algorithm  string // RS256 only in v0.8.0
	PublicKey  *rsa.PublicKey
	PrivateKey *rsa.PrivateKey
	Active     bool
	ValidFrom  time.Time
	ValidTo    *time.Time
}

// TrustedKey holds a trusted external public key.
type TrustedKey struct {
	KID       string
	TenantID  spi.TenantID
	JWK       map[string]any
	PublicKey *rsa.PublicKey
	Audience  string
	Issuers   []string
	Active    bool
	ValidFrom time.Time
	ValidTo   *time.Time
}

// RotateOptions controls sibling-invalidation behaviour during Save.
type RotateOptions struct {
	Invalidate     bool
	GracePeriodSec int64
}

// M2MClient represents a machine-to-machine client.
type M2MClient struct {
	ClientID     string
	HashedSecret string
	TenantID     spi.TenantID
	UserID       string
	Roles        []string
	CreatedAt    time.Time // set at Create/CreateWithSecret, never advanced
	UpdatedAt    time.Time // advanced on ResetSecret; equal to CreatedAt on fresh create
}

// --- Store Interfaces ---

// KeyStore manages RSA key pairs.
type KeyStore interface {
	Save(kp *KeyPair, opts RotateOptions) error
	Get(kid string) (*KeyPair, error)
	GetActive(audience string) (*KeyPair, error)
	List() []*KeyPair
	ListForVerification() []*KeyPair
	Delete(kid string) error
	Invalidate(kid string, gracePeriodSec int64) error
	Reactivate(kid string, validFrom, validTo time.Time) error
}

// TrustedKeyStore manages trusted external public keys.
type TrustedKeyStore interface {
	Register(tk *TrustedKey, opts RotateOptions) error
	Get(tenantID spi.TenantID, kid string) (*TrustedKey, error)
	List(tenantID spi.TenantID) []*TrustedKey
	ListForVerification() []*TrustedKey
	Delete(tenantID spi.TenantID, kid string) error
	Invalidate(tenantID spi.TenantID, kid string, gracePeriodSec int64) error
	Reactivate(tenantID spi.TenantID, kid string, validFrom, validTo time.Time) error
}

// M2MClientStore manages machine-to-machine clients.
type M2MClientStore interface {
	Create(clientID string, tenantID spi.TenantID, userID string, roles []string) (string, error)
	CreateWithSecret(clientID string, tenantID spi.TenantID, userID, secret string, roles []string) error
	Get(clientID string) (*M2MClient, error)
	List() []*M2MClient
	Delete(clientID string) error
	ResetSecret(clientID string) (string, error)
	VerifySecret(clientID, plaintext string) (bool, error)
}

// --- GenerateSecret ---

// GenerateSecret returns a random 32-byte hex string.
func GenerateSecret() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("failed to generate secret: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// --- InMemoryKeyStore ---

// InMemoryKeyStore stores RSA key pairs in memory.
type InMemoryKeyStore struct {
	mu   sync.RWMutex
	keys map[string]*KeyPair
}

// NewInMemoryKeyStore creates a new InMemoryKeyStore.
func NewInMemoryKeyStore() *InMemoryKeyStore {
	return &InMemoryKeyStore{
		keys: make(map[string]*KeyPair),
	}
}

// Save stores a key pair. When opts.Invalidate is true, all other active key
// pairs sharing the same Audience are marked inactive with a ValidTo expiry of
// now+GracePeriodSec. The new key pair itself is always stored active (it is
// never self-invalidated). All mutations are performed under a single Lock so
// concurrent rotations cannot leave two active keys for the same audience.
func (s *InMemoryKeyStore) Save(kp *KeyPair, opts RotateOptions) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if opts.Invalidate {
		now := time.Now()
		expiry := now.Add(time.Duration(opts.GracePeriodSec) * time.Second)
		for _, existing := range s.keys {
			if existing.Audience == kp.Audience && existing.Active && existing.KID != kp.KID {
				existing.Active = false
				e := expiry
				existing.ValidTo = &e
			}
		}
	}
	copied := *kp
	s.keys[kp.KID] = &copied
	return nil
}

// Get retrieves a key pair by KID.
func (s *InMemoryKeyStore) Get(kid string) (*KeyPair, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	kp, ok := s.keys[kid]
	if !ok {
		return nil, fmt.Errorf("key pair not found: %s", kid)
	}
	copied := *kp
	return &copied, nil
}

// GetActive returns the active key pair for the given audience with the latest
// ValidFrom timestamp. Keys whose ValidTo is in the past are skipped even if
// Active is still set (lazy expiry). Returns an error if no matching key is found.
func (s *InMemoryKeyStore) GetActive(audience string) (*KeyPair, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	now := time.Now()
	var best *KeyPair
	for _, kp := range s.keys {
		if kp.Audience != audience || !kp.Active {
			continue
		}
		if kp.ValidTo != nil && !now.Before(*kp.ValidTo) {
			continue
		}
		if best == nil || kp.ValidFrom.After(best.ValidFrom) {
			best = kp
		}
	}
	if best == nil {
		return nil, fmt.Errorf("no active key pair for audience %q", audience)
	}
	copied := *best
	return &copied, nil
}

// List returns all key pairs.
func (s *InMemoryKeyStore) List() []*KeyPair {
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := make([]*KeyPair, 0, len(s.keys))
	for _, kp := range s.keys {
		copied := *kp
		result = append(result, &copied)
	}
	return result
}

// ListForVerification returns key pairs that are still within their validity
// window (ValidTo is nil or in the future). This is used to populate the JWKS
// endpoint during grace periods so recently-rotated keys can still verify
// tokens issued before the rotation.
func (s *InMemoryKeyStore) ListForVerification() []*KeyPair {
	s.mu.RLock()
	defer s.mu.RUnlock()
	now := time.Now()
	out := make([]*KeyPair, 0, len(s.keys))
	for _, kp := range s.keys {
		if kp.ValidTo == nil || now.Before(*kp.ValidTo) {
			copied := *kp
			out = append(out, &copied)
		}
	}
	return out
}

// Delete removes a key pair by KID.
func (s *InMemoryKeyStore) Delete(kid string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.keys[kid]; !ok {
		return fmt.Errorf("key pair not found: %s", kid)
	}
	delete(s.keys, kid)
	return nil
}

// Invalidate marks a key pair as inactive and sets its ValidTo to
// now+gracePeriodSec so grace-period JWKS publishing still includes the key.
func (s *InMemoryKeyStore) Invalidate(kid string, gracePeriodSec int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	kp, ok := s.keys[kid]
	if !ok {
		return fmt.Errorf("key pair not found: %s", kid)
	}
	expiry := time.Now().Add(time.Duration(gracePeriodSec) * time.Second)
	kp.Active = false
	kp.ValidTo = &expiry
	return nil
}

// Reactivate sets a key pair as active and updates its validity window.
// validTo must be strictly in the future and after validFrom; if the key is
// already active the call is idempotent — Active remains true and ValidTo is
// extended to the supplied value.
func (s *InMemoryKeyStore) Reactivate(kid string, validFrom, validTo time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	kp, ok := s.keys[kid]
	if !ok {
		return fmt.Errorf("key pair not found: %s", kid)
	}
	if !validTo.After(time.Now()) {
		return fmt.Errorf("validTo must be in the future")
	}
	if !validTo.After(validFrom) {
		return fmt.Errorf("validTo must be after validFrom")
	}
	kp.Active = true
	kp.ValidFrom = validFrom
	vt := validTo
	kp.ValidTo = &vt
	return nil
}

// --- InMemoryTrustedKeyStore ---

// InMemoryTrustedKeyStore stores trusted external public keys in memory with
// full tenant scoping. Each KID is globally unique — cross-tenant KID
// collisions are rejected with KEY_OWNED_BY_DIFFERENT_TENANT (409).
type InMemoryTrustedKeyStore struct {
	mu           sync.RWMutex
	keys         map[string]*TrustedKey
	maxPerTenant int
}

// NewInMemoryTrustedKeyStore creates a new InMemoryTrustedKeyStore with no cap.
func NewInMemoryTrustedKeyStore() *InMemoryTrustedKeyStore {
	return NewInMemoryTrustedKeyStoreWithCap(0)
}

// NewInMemoryTrustedKeyStoreWithCap creates a new InMemoryTrustedKeyStore with
// a per-tenant cap on currently-valid (non-expired) keys. Values <= 0 disable
// the cap.
func NewInMemoryTrustedKeyStoreWithCap(cap int) *InMemoryTrustedKeyStore {
	return &InMemoryTrustedKeyStore{
		keys:         make(map[string]*TrustedKey),
		maxPerTenant: cap,
	}
}

// Register adds or replaces a trusted key. Cross-tenant KID collision returns
// 409 KEY_OWNED_BY_DIFFERENT_TENANT. Per-tenant cap (counts only
// currently-valid keys) returns 400 TRUSTED_KEY_CAP_REACHED. When
// opts.Invalidate is true, all other active siblings in the same tenant
// partition are marked inactive with a gracePeriod ValidTo. Stores a shallow
// copy of *tk (ownership-mutability rule 4).
func (s *InMemoryTrustedKeyStore) Register(tk *TrustedKey, opts RotateOptions) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Cross-tenant collision guard.
	if existing, ok := s.keys[tk.KID]; ok && existing.TenantID != tk.TenantID {
		return common.Operational(http.StatusConflict, common.ErrCodeKeyOwnedByDifferentTenant, "key with this keyId belongs to a different tenant")
	}

	// Per-tenant cap: count only currently-valid keys (excluding the KID being
	// registered, so same-KID upserts don't consume a slot).
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

	// Atomic sibling invalidation within the same tenant.
	if opts.Invalidate {
		now := time.Now()
		expiry := now.Add(time.Duration(opts.GracePeriodSec) * time.Second)
		for _, k := range s.keys {
			if k.TenantID == tk.TenantID && k.Active && k.KID != tk.KID {
				k.Active = false
				e := expiry
				k.ValidTo = &e
			}
		}
	}

	// Shallow-copy on store (callers may continue using *tk after Register).
	copied := *tk
	s.keys[tk.KID] = &copied
	return nil
}

// Get retrieves a trusted key by tenant and KID. Returns an error if the key
// does not exist or belongs to a different tenant.
func (s *InMemoryTrustedKeyStore) Get(tenantID spi.TenantID, kid string) (*TrustedKey, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	tk, ok := s.keys[kid]
	if !ok || tk.TenantID != tenantID {
		return nil, fmt.Errorf("trusted key not found: %s", kid)
	}
	copied := *tk
	return &copied, nil
}

// List returns all trusted keys for the given tenant.
func (s *InMemoryTrustedKeyStore) List(tenantID spi.TenantID) []*TrustedKey {
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := make([]*TrustedKey, 0)
	for _, tk := range s.keys {
		if tk.TenantID == tenantID {
			copied := *tk
			result = append(result, &copied)
		}
	}
	return result
}

// ListForVerification returns keys still within their validity window across
// all tenants. Used to populate the JWKS endpoint during grace periods.
func (s *InMemoryTrustedKeyStore) ListForVerification() []*TrustedKey {
	s.mu.RLock()
	defer s.mu.RUnlock()
	now := time.Now()
	out := make([]*TrustedKey, 0, len(s.keys))
	for _, tk := range s.keys {
		if tk.ValidTo == nil || now.Before(*tk.ValidTo) {
			copied := *tk
			out = append(out, &copied)
		}
	}
	return out
}

// Delete removes a trusted key by tenant and KID.
func (s *InMemoryTrustedKeyStore) Delete(tenantID spi.TenantID, kid string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tk, ok := s.keys[kid]
	if !ok || tk.TenantID != tenantID {
		return fmt.Errorf("trusted key not found: %s", kid)
	}
	delete(s.keys, kid)
	return nil
}

// Invalidate marks a trusted key as inactive and sets ValidTo to
// now+gracePeriodSec so grace-period JWKS publishing still includes the key.
func (s *InMemoryTrustedKeyStore) Invalidate(tenantID spi.TenantID, kid string, gracePeriodSec int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tk, ok := s.keys[kid]
	if !ok || tk.TenantID != tenantID {
		return fmt.Errorf("trusted key not found: %s", kid)
	}
	expiry := time.Now().Add(time.Duration(gracePeriodSec) * time.Second)
	tk.Active = false
	tk.ValidTo = &expiry
	return nil
}

// Reactivate sets a trusted key as active and updates its validity window.
// validTo must be non-zero, strictly in the future, and after validFrom.
func (s *InMemoryTrustedKeyStore) Reactivate(tenantID spi.TenantID, kid string, validFrom, validTo time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tk, ok := s.keys[kid]
	if !ok || tk.TenantID != tenantID {
		return fmt.Errorf("trusted key not found: %s", kid)
	}
	if validTo.IsZero() {
		return fmt.Errorf("validTo required for reactivation")
	}
	if !validTo.After(time.Now()) {
		return fmt.Errorf("validTo must be in the future")
	}
	if !validTo.After(validFrom) {
		return fmt.Errorf("validTo must be after validFrom")
	}
	tk.Active = true
	tk.ValidFrom = validFrom
	vt := validTo
	tk.ValidTo = &vt
	return nil
}

// --- InMemoryM2MClientStore ---

// InMemoryM2MClientStore stores M2M clients in memory.
type InMemoryM2MClientStore struct {
	mu      sync.RWMutex
	clients map[string]*M2MClient
}

// NewInMemoryM2MClientStore creates a new InMemoryM2MClientStore.
func NewInMemoryM2MClientStore() *InMemoryM2MClientStore {
	return &InMemoryM2MClientStore{
		clients: make(map[string]*M2MClient),
	}
}

// Create adds an M2M client, hashing the provided plaintext secret with bcrypt.
// Returns the plaintext secret for the caller to deliver to the client.
func (s *InMemoryM2MClientStore) Create(clientID string, tenantID spi.TenantID, userID string, roles []string) (string, error) {
	secret, err := GenerateSecret()
	if err != nil {
		return "", fmt.Errorf("failed to generate secret: %w", err)
	}

	hashed, err := bcrypt.GenerateFromPassword([]byte(secret), bcrypt.DefaultCost)
	if err != nil {
		return "", fmt.Errorf("failed to hash secret: %w", err)
	}

	rolesCopy := make([]string, len(roles))
	copy(rolesCopy, roles)

	now := time.Now().UTC()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.clients[clientID] = &M2MClient{
		ClientID:     clientID,
		HashedSecret: string(hashed),
		TenantID:     tenantID,
		UserID:       userID,
		Roles:        rolesCopy,
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	return secret, nil
}

// CreateWithSecret adds an M2M client with a caller-provided plaintext secret.
func (s *InMemoryM2MClientStore) CreateWithSecret(clientID string, tenantID spi.TenantID, userID, secret string, roles []string) error {
	hashed, err := bcrypt.GenerateFromPassword([]byte(secret), bcrypt.DefaultCost)
	if err != nil {
		return fmt.Errorf("failed to hash secret: %w", err)
	}

	rolesCopy := make([]string, len(roles))
	copy(rolesCopy, roles)

	now := time.Now().UTC()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.clients[clientID] = &M2MClient{
		ClientID:     clientID,
		HashedSecret: string(hashed),
		TenantID:     tenantID,
		UserID:       userID,
		Roles:        rolesCopy,
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	return nil
}

// Get retrieves an M2M client by client ID.
func (s *InMemoryM2MClientStore) Get(clientID string) (*M2MClient, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	c, ok := s.clients[clientID]
	if !ok {
		return nil, fmt.Errorf("m2m client not found: %s", clientID)
	}
	copied := *c
	copied.Roles = make([]string, len(c.Roles))
	copy(copied.Roles, c.Roles)
	return &copied, nil
}

// List returns all M2M clients.
func (s *InMemoryM2MClientStore) List() []*M2MClient {
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := make([]*M2MClient, 0, len(s.clients))
	for _, c := range s.clients {
		copied := *c
		copied.Roles = make([]string, len(c.Roles))
		copy(copied.Roles, c.Roles)
		result = append(result, &copied)
	}
	return result
}

// Delete removes an M2M client by client ID.
func (s *InMemoryM2MClientStore) Delete(clientID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.clients[clientID]; !ok {
		return fmt.Errorf("m2m client not found: %s", clientID)
	}
	delete(s.clients, clientID)
	return nil
}

// ResetSecret generates a new random secret for the client and returns the plaintext.
func (s *InMemoryM2MClientStore) ResetSecret(clientID string) (string, error) {
	secret, err := GenerateSecret()
	if err != nil {
		return "", fmt.Errorf("failed to generate secret: %w", err)
	}

	hashed, err := bcrypt.GenerateFromPassword([]byte(secret), bcrypt.DefaultCost)
	if err != nil {
		return "", fmt.Errorf("failed to hash secret: %w", err)
	}

	now := time.Now().UTC()
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.clients[clientID]
	if !ok {
		return "", fmt.Errorf("m2m client not found: %s", clientID)
	}
	c.HashedSecret = string(hashed)
	c.UpdatedAt = now
	return secret, nil
}

// VerifySecret checks whether the plaintext secret matches the stored hash.
func (s *InMemoryM2MClientStore) VerifySecret(clientID, plaintext string) (bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	c, ok := s.clients[clientID]
	if !ok {
		return false, fmt.Errorf("m2m client not found: %s", clientID)
	}
	err := bcrypt.CompareHashAndPassword([]byte(c.HashedSecret), []byte(plaintext))
	return err == nil, nil
}
