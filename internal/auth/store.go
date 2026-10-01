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
	// Signer returns the key pair that signs new tokens for audience and its
	// Signer: the active key pair inside its window with the latest
	// ValidFrom, and on a tie the greater KID.
	Signer(audience string) (*KeyPair, Signer, error)
	// Current is the key pair Signer would choose, without the signer.
	Current(audience string) (*KeyPair, error)
	// VerificationKey returns the public key of kid if it may verify now.
	// Every refusal wraps ErrKeyPairNotFound; one for a kid that names a key
	// pair of this store also wraps ErrKeyPairCannotVerify.
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
	Create(ctx context.Context, tenantID spi.TenantID, clientID, userID string, roles []string) (secret string, err error)
	Authenticate(ctx context.Context, clientID, secret string) (*M2MClient, error)
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
