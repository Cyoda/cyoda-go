package auth

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
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

// TrustedKey holds a trusted external public key: the key an application
// signs the user assertions of its tenant with.
type TrustedKey struct {
	KID       string
	TenantID  spi.TenantID
	JWK       map[string]any
	PublicKey *rsa.PublicKey
	Issuers   []string
	Active    bool
	ValidFrom time.Time
	ValidTo   *time.Time
}

// Verifies reports whether the key may verify a subject token at now: active
// and inside its window [ValidFrom, ValidTo). Trusted keys have no grace
// period, so an inactive key never verifies.
func (tk *TrustedKey) Verifies(now time.Time) bool {
	return tk.Active && !now.Before(tk.ValidFrom) && windowOpen(tk.ValidTo, now)
}

// M2MClient represents a machine-to-machine client.
type M2MClient struct {
	ClientID     string
	HashedSecret string
	TenantID     spi.TenantID
	UserID       string
	Roles        []string
	OnBehalfOf   bool      // set at Create, immutable: may only perform token exchanges
	SecretGen    uint64    // 1 at Create, incremented by every successful ResetSecret
	CreatedAt    time.Time // set at Create, never advanced
	UpdatedAt    time.Time // advanced on ResetSecret; equal to CreatedAt on fresh create
}

// --- Store Interfaces ---

// KeyStore holds the signing key pairs. Signer, Current, VerificationKey and
// Published read the node copy; Issue, Invalidate, Reactivate and Delete read
// and write the store, so an admin decision is never made on a stale copy.
// ErrKeyPairNotFound means the key pair is absent; any other error is the
// store failing (ErrStoreStale and storage-unavailable errors answer 503).
type KeyStore interface {
	// Signer returns the key pair that signs new tokens and its Signer: the
	// active key pair inside its window with the latest ValidFrom, and on a
	// tie the greater KID.
	Signer() (*KeyPair, Signer, error)
	// Current is the key pair Signer would choose, without the signer.
	Current() (*KeyPair, error)
	// VerificationKey returns the public key of kid if it may verify now.
	// Every refusal wraps ErrKeyPairNotFound.
	VerificationKey(kid string) (*rsa.PublicKey, error)
	// Published returns the key pairs to publish in JWKS.
	Published() ([]*KeyPair, error)
	Issue(ctx context.Context, req IssueRequest) (*KeyPair, error)
	Invalidate(ctx context.Context, kid string, graceSec int64) error
	Reactivate(ctx context.Context, kid string, from, to time.Time) (*KeyPair, error)
	Delete(ctx context.Context, kid string) error
}

// ErrTrustedKeyNotFound is returned by a TrustedKeyStore when the KID is not
// registered for the calling tenant, and by GetForVerification also when the
// key may not verify now. Adapters classify with errors.Is: it is the one
// failure that means 404 on the admin endpoints and 400 on the token
// exchange. Anything else is the store failing, not the key being absent — a
// distinction that has to survive, or a storage outage reads to the caller as
// "your key is gone".
var ErrTrustedKeyNotFound = errors.New("trusted key not found")

// TrustedKeyStore manages trusted external public keys, one set per tenant.
// Key ids are unique within a tenant only, and every method acts on
// tenantID's keys alone: another tenant's key with the same kid is absent.
// Every method reads or writes the store itself — there is no node copy —
// so a change is in force on every node when the call returns. A store
// failure is returned wrapped, so a storage-unavailable error keeps its
// marker (interface{ StorageUnavailable() bool }).
type TrustedKeyStore interface {
	// Register adds or replaces tk (an upsert on its tenant and kid). With
	// invalidatePrevious, every other key of the tenant is invalidated at
	// once.
	Register(ctx context.Context, tk *TrustedKey, invalidatePrevious bool) error
	Get(ctx context.Context, tenantID spi.TenantID, kid string) (*TrustedKey, error)
	List(ctx context.Context, tenantID spi.TenantID) ([]*TrustedKey, error)
	// GetForVerification returns the key a subject token names, for the
	// token-exchange grant, read from the store on every call. The key is
	// found only in tenantID — the tenant of the client exchanging the
	// token — and only while it Verifies; otherwise the error wraps
	// ErrTrustedKeyNotFound. A key's tenant is the tenant that registered
	// it, never a claim in the token it signs.
	GetForVerification(ctx context.Context, tenantID spi.TenantID, kid string) (*TrustedKey, error)
	Delete(ctx context.Context, tenantID spi.TenantID, kid string) error
	// Invalidate ends the key at once: trusted keys have no grace period.
	Invalidate(ctx context.Context, tenantID spi.TenantID, kid string) error
	Reactivate(ctx context.Context, tenantID spi.TenantID, kid string, validFrom, validTo time.Time) error
}

// ErrInvalidClient is returned by M2MClientStore.Authenticate when there is
// no such client or the secret is wrong. The token endpoint answers it with
// 401 invalid_client.
var ErrInvalidClient = errors.New("invalid client")

// ErrM2MClientCapReached is returned by M2MClientStore.Create when the tenant
// already has the configured maximum number of clients.
var ErrM2MClientCapReached = errors.New("m2m client cap reached")

// ErrM2MClientNotFound is returned by M2MClientStore.Delete and ResetSecret
// when the client does not exist in the caller's tenant: absent, or another
// tenant's. Adapters classify with errors.Is.
var ErrM2MClientNotFound = errors.New("m2m client not found")

// ErrM2MClientExists is returned by M2MClientStore.Create when the clientID
// is already taken, in any tenant — that is, when an index entry exists for
// it, decodable or not. The adapter's collision-retry loop in
// CreateTechnicalUser detects this via errors.Is and regenerates.
var ErrM2MClientExists = errors.New("m2m client already exists")

// M2MClientStore manages machine-to-machine clients. The store enforces
// tenant isolation: List, Delete and ResetSecret act only on tenantID's
// clients, and another tenant's client is ErrM2MClientNotFound, exactly as an
// absent one. Any error other than the sentinels above is a server-side
// failure: a KV error, wrapped so a storage-unavailable one keeps its
// marker; stored data that does not decode (errM2MUndecodable); or a failure
// to generate or hash a secret, which wraps no KV error.
type M2MClientStore interface {
	Create(ctx context.Context, tenantID spi.TenantID, clientID, userID string, roles []string, onBehalfOf bool) (secret string, err error)
	// Authenticate is ErrInvalidClient for no such client or a wrong
	// secret, and ErrSecretCheckBusy when the node has no secret-check
	// capacity left to decide.
	Authenticate(ctx context.Context, clientID, secret string) (*M2MClient, error)
	// Lookup returns clientID's record without checking a secret: the current
	// record as the store holds it. ErrM2MClientNotFound when clientID is
	// outside the grammar or no record exists for it in any tenant.
	Lookup(ctx context.Context, clientID string) (*M2MClient, error)
	List(ctx context.Context, tenantID spi.TenantID) ([]*M2MClient, error)
	Delete(ctx context.Context, tenantID spi.TenantID, clientID string) error
	ResetSecret(ctx context.Context, tenantID spi.TenantID, clientID string) (secret string, c *M2MClient, err error)
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

// capReached reports whether keys — one tenant's keys — already hold max
// slots, not counting exceptKID, the key being registered or reactivated. A
// key holds a slot while it is active and its window has not ended: it
// verifies now, or will once its window opens. max <= 0 means unbounded.
func capReached(keys []*TrustedKey, exceptKID string, max int, now time.Time) bool {
	if max <= 0 {
		return false
	}
	count := 0
	for _, k := range keys {
		if k.KID != exceptKID && k.Active && windowOpen(k.ValidTo, now) {
			count++
		}
	}
	return count >= max
}

func errTrustedKeyCapReached() error {
	return common.Operational(http.StatusBadRequest, common.ErrCodeTrustedKeyCapReached, "trusted-key cap reached for tenant")
}

// windowOpen reports whether a key whose window ends at validTo has not yet
// ended.
func windowOpen(validTo *time.Time, now time.Time) bool {
	return validTo == nil || now.Before(*validTo)
}

// --- Constant-time pad ---

// dummyHash is compared against the secret of a token request that names no
// usable client, so Authenticate makes one bcrypt comparison whether or not
// the id exists. Without it, response-time analysis on POST /oauth/token
// would reveal whether a given clientID exists. Generated once at init; the
// plaintext is never referenced outside this comparison.
var dummyHash = func() []byte {
	h, err := bcrypt.GenerateFromPassword([]byte("dummy-constant-time-pad"), bcrypt.DefaultCost)
	if err != nil {
		panic(fmt.Errorf("bcrypt dummy hash init: %w", err))
	}
	return h
}()
