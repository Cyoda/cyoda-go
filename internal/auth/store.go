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
	TenantID     string
	UserID       string
	Roles        []string
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
	Register(tk *TrustedKey) error
	Get(kid string) (*TrustedKey, error)
	List() []*TrustedKey
	Delete(kid string) error
	Invalidate(kid string) error
	Reactivate(kid string) error
}

// M2MClientStore manages machine-to-machine clients.
type M2MClientStore interface {
	Create(clientID, tenantID, userID string, roles []string) (string, error)
	CreateWithSecret(clientID, tenantID, userID, secret string, roles []string) error
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
	s.keys[kp.KID] = kp
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

// InMemoryTrustedKeyStore stores trusted external public keys in memory.
type InMemoryTrustedKeyStore struct {
	mu             sync.RWMutex
	keys           map[string]*TrustedKey
	maxTrustedKeys int
}

// InMemoryTrustedKeyStoreOption configures an InMemoryTrustedKeyStore at
// construction time. Mirrors the KV-backed store's option pattern so tests
// and callers see a single contract for capping the trusted-key registry.
type InMemoryTrustedKeyStoreOption func(*InMemoryTrustedKeyStore)

// WithInMemoryMaxTrustedKeys overrides the default cap on registered trusted
// keys for the in-memory store. Values <= 0 disable the cap.
func WithInMemoryMaxTrustedKeys(n int) InMemoryTrustedKeyStoreOption {
	return func(s *InMemoryTrustedKeyStore) {
		s.maxTrustedKeys = n
	}
}

// NewInMemoryTrustedKeyStore creates a new InMemoryTrustedKeyStore. The
// default cap matches KVTrustedKeyStore's defaultMaxTrustedKeys; pass
// WithInMemoryMaxTrustedKeys to override (e.g. in tests exercising the cap).
func NewInMemoryTrustedKeyStore(opts ...InMemoryTrustedKeyStoreOption) *InMemoryTrustedKeyStore {
	s := &InMemoryTrustedKeyStore{
		keys:           make(map[string]*TrustedKey),
		maxTrustedKeys: defaultMaxTrustedKeys,
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// Register adds a trusted key. Re-registering an existing KID is an
// idempotent upsert and never trips the registry cap; only a brand-new KID
// at full capacity is rejected with a 409 Conflict AppError. The capacity
// check and the insert are performed under a single Lock so concurrent
// registrations cannot collectively exceed the cap. This mirrors
// KVTrustedKeyStore.Register so that tests using the in-memory variant
// observe the same bound as production code paths.
func (s *InMemoryTrustedKeyStore) Register(tk *TrustedKey) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.keys[tk.KID]; !exists && s.maxTrustedKeys > 0 && len(s.keys) >= s.maxTrustedKeys {
		return common.Operational(http.StatusConflict, common.ErrCodeConflict, "trusted-key registry full")
	}
	s.keys[tk.KID] = copyTrustedKey(tk)
	return nil
}

// Get retrieves a trusted key by KID.
func (s *InMemoryTrustedKeyStore) Get(kid string) (*TrustedKey, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	tk, ok := s.keys[kid]
	if !ok {
		return nil, fmt.Errorf("trusted key not found: %s", kid)
	}
	return copyTrustedKey(tk), nil
}

// List returns all trusted keys.
func (s *InMemoryTrustedKeyStore) List() []*TrustedKey {
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := make([]*TrustedKey, 0, len(s.keys))
	for _, tk := range s.keys {
		result = append(result, copyTrustedKey(tk))
	}
	return result
}

// Delete removes a trusted key by KID.
func (s *InMemoryTrustedKeyStore) Delete(kid string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.keys[kid]; !ok {
		return fmt.Errorf("trusted key not found: %s", kid)
	}
	delete(s.keys, kid)
	return nil
}

// Invalidate marks a trusted key as inactive.
func (s *InMemoryTrustedKeyStore) Invalidate(kid string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tk, ok := s.keys[kid]
	if !ok {
		return fmt.Errorf("trusted key not found: %s", kid)
	}
	tk.Active = false
	return nil
}

// Reactivate marks a trusted key as active.
func (s *InMemoryTrustedKeyStore) Reactivate(kid string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tk, ok := s.keys[kid]
	if !ok {
		return fmt.Errorf("trusted key not found: %s", kid)
	}
	tk.Active = true
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
func (s *InMemoryM2MClientStore) Create(clientID, tenantID, userID string, roles []string) (string, error) {
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

	s.mu.Lock()
	defer s.mu.Unlock()
	s.clients[clientID] = &M2MClient{
		ClientID:     clientID,
		HashedSecret: string(hashed),
		TenantID:     tenantID,
		UserID:       userID,
		Roles:        rolesCopy,
	}
	return secret, nil
}

// CreateWithSecret adds an M2M client with a caller-provided plaintext secret.
func (s *InMemoryM2MClientStore) CreateWithSecret(clientID, tenantID, userID, secret string, roles []string) error {
	hashed, err := bcrypt.GenerateFromPassword([]byte(secret), bcrypt.DefaultCost)
	if err != nil {
		return fmt.Errorf("failed to hash secret: %w", err)
	}

	rolesCopy := make([]string, len(roles))
	copy(rolesCopy, roles)

	s.mu.Lock()
	defer s.mu.Unlock()
	s.clients[clientID] = &M2MClient{
		ClientID:     clientID,
		HashedSecret: string(hashed),
		TenantID:     tenantID,
		UserID:       userID,
		Roles:        rolesCopy,
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

	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.clients[clientID]
	if !ok {
		return "", fmt.Errorf("m2m client not found: %s", clientID)
	}
	c.HashedSecret = string(hashed)
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
