package auth

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/common"
)

// --- Types ---

// KeyPair describes a signing key pair: public data only. The private key
// stays behind the Signer the store hands out and is never on this type.
type KeyPair struct {
	KID       string
	Audience  string // "human" | "client"
	Algorithm string // RS256 only
	PublicKey *rsa.PublicKey
	Active    bool
	ValidFrom time.Time
	ValidTo   *time.Time
	Bootstrap bool // the key built from configuration
}

// InWindow reports whether now is inside the key pair's window
// [ValidFrom, ValidTo): the only time it may sign tokens or verify them.
func (kp *KeyPair) InWindow(now time.Time) bool {
	return !now.Before(kp.ValidFrom) && (kp.ValidTo == nil || now.Before(*kp.ValidTo))
}

// Verifies reports whether the key pair may verify a token at now: inside its
// window, and active or in the grace period an invalidation gave it (an
// inactive key pair with a validTo). An inactive key pair with no validTo
// never verifies. Signing requires Active; verifying does not.
func (kp *KeyPair) Verifies(now time.Time) bool {
	return kp.InWindow(now) && (kp.Active || kp.ValidTo != nil)
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

// RotateOptions controls sibling invalidation when a trusted key is
// registered.
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

// KeyStore holds the signing key pairs. Signer, Current, VerificationKey and
// Published read the node copy; Issue, Invalidate, Reactivate and Delete read
// and write the store, so an admin decision is never made on a stale copy.
// ErrKeyPairNotFound means the key pair is absent; any other error is the
// store failing (ErrStoreStale and storage-unavailable errors answer 503).
type KeyStore interface {
	// Signer returns the key pair that signs new tokens for audience and its
	// Signer: the active key pair inside its window with the latest
	// ValidFrom, and on a tie the greater KID.
	Signer(audience string) (*KeyPair, Signer, error)
	// Current is the key pair Signer would choose, without the signer.
	Current(audience string) (*KeyPair, error)
	// VerificationKey returns the public key of kid if it may verify now.
	VerificationKey(kid string) (*rsa.PublicKey, error)
	// Published returns the key pairs to publish in JWKS.
	Published() ([]*KeyPair, error)
	Issue(ctx context.Context, req IssueRequest) (*KeyPair, error)
	Invalidate(ctx context.Context, kid string, graceSec int64) error
	Reactivate(ctx context.Context, kid string, from, to time.Time) (*KeyPair, error)
	Delete(ctx context.Context, kid string) error
}

// ErrTrustedKeyNotFound is returned by a TrustedKeyStore's Delete / Invalidate
// / Reactivate when the KID is not registered for the calling tenant. Adapters
// classify with errors.Is: it is the one failure of those three that means 404.
// Anything else is the store failing, not the key being absent — a distinction
// that has to survive, or a storage outage reads to the caller as "your key is
// gone".
var ErrTrustedKeyNotFound = errors.New("trusted key not found")

// TrustedKeyStore manages trusted external public keys. Register, Delete,
// Invalidate and Reactivate take a context because a cluster-backed
// implementation reads authoritative state through to the store on every
// call: an admin decision is never made on a possibly stale node copy. Get
// also takes a context, but is not one of those store-read methods: it
// serves the node copy when the copy is not stale, and reads through to the
// store only on a copy miss or while stale — a compromise for a method both
// admin handlers and ordinary lookups share. GetForVerification stays
// copy-only with no context — it is the hot, high-volume path, and a key's
// presence there is bounded by reconciliation, not by a need for ground
// truth on every call.
type TrustedKeyStore interface {
	Register(ctx context.Context, tk *TrustedKey, opts RotateOptions) error
	Get(ctx context.Context, tenantID spi.TenantID, kid string) (*TrustedKey, error)
	List(tenantID spi.TenantID) []*TrustedKey
	// GetForVerification returns the key a subject token names, for the
	// token-exchange grant. The key is found only in tenantID — the tenant of
	// the client exchanging the token — and only while within its validity
	// window; otherwise the error wraps ErrTrustedKeyNotFound. A key's tenant
	// is the tenant that registered it, never a claim in the token it signs.
	GetForVerification(tenantID spi.TenantID, kid string) (*TrustedKey, error)
	Delete(ctx context.Context, tenantID spi.TenantID, kid string) error
	Invalidate(ctx context.Context, tenantID spi.TenantID, kid string, gracePeriodSec int64) error
	Reactivate(ctx context.Context, tenantID spi.TenantID, kid string, validFrom, validTo time.Time) error
}

// M2MClientStore manages machine-to-machine clients.
type M2MClientStore interface {
	Create(clientID string, tenantID spi.TenantID, userID string, roles []string) (string, error)
	CreateWithSecret(clientID string, tenantID spi.TenantID, userID, secret string, roles []string) error
	Get(clientID string) (*M2MClient, error)
	// List returns all M2M clients within the given tenant. The store is
	// responsible for filtering — future persistent implementations can
	// push this down to the backend, avoiding loading the whole cluster
	// into memory for what the caller already knows is a per-tenant query.
	List(tenantID spi.TenantID) []*M2MClient
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

// --- Key-window helpers ---

// graceExpiry is the ValidTo an invalidation leaves on a key: now plus the
// grace period, but never later than the ValidTo the key already has. A grace
// period keeps a key valid for at most that long; it never lengthens a key's
// window, and never brings back a key that has already ended.
func graceExpiry(current *time.Time, now time.Time, gracePeriodSec int64) *time.Time {
	expiry := now.Add(time.Duration(gracePeriodSec) * time.Second)
	if current != nil && current.Before(expiry) {
		expiry = *current
	}
	return &expiry
}

// capReached reports whether tenantID already has max keys that can verify —
// active, or in a grace period until ValidTo — not counting exceptKID, the key
// being registered or reactivated. max <= 0 means unbounded.
func capReached(keys map[string]*TrustedKey, tenantID spi.TenantID, exceptKID string, max int, now time.Time) bool {
	if max <= 0 {
		return false
	}
	count := 0
	for _, k := range keys {
		if k.TenantID == tenantID && k.KID != exceptKID && windowOpen(k.ValidTo, now) {
			count++
		}
	}
	return count >= max
}

func errTrustedKeyCapReached() error {
	return common.Operational(http.StatusBadRequest, common.ErrCodeTrustedKeyCapReached, "trusted-key cap reached for tenant")
}

// windowOpen reports whether a key whose window ends at validTo is still
// within it: the lazy expiry filter the verification path applies, and the
// test for which siblings a rotation still has to end.
func windowOpen(validTo *time.Time, now time.Time) bool {
	return validTo == nil || now.Before(*validTo)
}

// --- InMemoryM2MClientStore ---

// ErrM2MClientNotFound is returned by InMemoryM2MClientStore.Get / .Delete /
// .ResetSecret / .VerifySecret when the requested clientID is not present.
// Adapters should use errors.Is for classification.
var ErrM2MClientNotFound = errors.New("m2m client not found")

// ErrM2MClientExists is returned by M2MClientStore.Create / .CreateWithSecret
// when the clientID is already present. The adapter's collision-retry loop
// in CreateTechnicalUser detects this via errors.Is and regenerates.
var ErrM2MClientExists = errors.New("m2m client already exists")

// dummyHash is a constant-time fallback compared against any unknown
// clientID so VerifySecret takes ~100ms in both the unknown-clientID
// and wrong-secret paths. Without this, response-time analysis on
// POST /oauth/token would reveal whether a given clientID exists.
// Generated once at init; the plaintext "dummy" is never referenced
// outside this comparison and never leaves the function.
var dummyHash = func() []byte {
	h, err := bcrypt.GenerateFromPassword([]byte("dummy-constant-time-pad"), bcrypt.DefaultCost)
	if err != nil {
		panic(fmt.Errorf("bcrypt dummy hash init: %w", err))
	}
	return h
}()

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
	if _, exists := s.clients[clientID]; exists {
		return "", fmt.Errorf("%w: %s", ErrM2MClientExists, clientID)
	}
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
	if _, exists := s.clients[clientID]; exists {
		return fmt.Errorf("%w: %s", ErrM2MClientExists, clientID)
	}
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
		return nil, fmt.Errorf("%w: %s", ErrM2MClientNotFound, clientID)
	}
	copied := *c
	copied.Roles = make([]string, len(c.Roles))
	copy(copied.Roles, c.Roles)
	return &copied, nil
}

// List returns all M2M clients belonging to tenantID.
func (s *InMemoryM2MClientStore) List(tenantID spi.TenantID) []*M2MClient {
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := make([]*M2MClient, 0, len(s.clients))
	for _, c := range s.clients {
		if c.TenantID != tenantID {
			continue
		}
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
		return fmt.Errorf("%w: %s", ErrM2MClientNotFound, clientID)
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
		return "", fmt.Errorf("%w: %s", ErrM2MClientNotFound, clientID)
	}
	c.HashedSecret = string(hashed)
	c.UpdatedAt = now
	return secret, nil
}

// VerifySecret reports whether plaintext matches the stored bcrypt hash
// for clientID. Returns (false, ErrM2MClientNotFound) when the client
// does not exist; the comparison still runs against a dummy hash so the
// timing profile matches the wrong-secret case and clientID existence
// cannot be inferred from response latency.
func (s *InMemoryM2MClientStore) VerifySecret(clientID, plaintext string) (bool, error) {
	var hashCopy []byte
	var found bool
	func() {
		s.mu.RLock()
		defer s.mu.RUnlock()
		if c, ok := s.clients[clientID]; ok {
			// Copy the hash so we can release the lock before the slow bcrypt
			// call — otherwise concurrent writes (ResetSecret, Delete, Create)
			// wait ~100ms on every token request.
			hashCopy = []byte(c.HashedSecret)
			found = true
		}
	}()

	if !found {
		// Constant-time compare against the dummy hash to match the
		// existing-client timing. Discard the result.
		_ = bcrypt.CompareHashAndPassword(dummyHash, []byte(plaintext))
		return false, fmt.Errorf("%w: %s", ErrM2MClientNotFound, clientID)
	}
	err := bcrypt.CompareHashAndPassword(hashCopy, []byte(plaintext))
	return err == nil, nil
}
