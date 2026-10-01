package auth

import (
	"context"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sort"
	"sync"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// ErrKeyPairNotFound: no such key pair on this node (absent, retired, foreign
// bootstrap state, deleted bootstrap, or no signer for an audience) → 404.
var ErrKeyPairNotFound = errors.New("key pair not found")

// ErrKeyPairBroken: the key pair the rules select cannot be used → 500.
var ErrKeyPairBroken = errors.New("key pair cannot be used")

// ErrKeyPairCannotVerify: the kid names a key pair of this store (the
// bootstrap key or a stored record) that may not verify now — ahead of its
// window, invalidated past its grace period, deleted, retired or broken.
// It always comes wrapped with ErrKeyPairNotFound.
var ErrKeyPairCannotVerify = errors.New("key pair cannot verify now")

type storeStaleError struct{}

func (storeStaleError) Error() string {
	return "signing-key store stale: no successful re-read within the bound"
}

// StorageUnavailable marks the error for common.Internal: 503, retryable.
func (storeStaleError) StorageUnavailable() bool { return true }

// ErrStoreStale: the node copy has had no successful re-read for the
// fail-closed bound.
var ErrStoreStale error = storeStaleError{}

// DeriveKID is the KID of a bootstrap key: hex of the first 16 bytes of
// SHA-256 over its PKIX public key, the same on every node with the same key.
func DeriveKID(pub *rsa.PublicKey) (string, error) {
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		return "", fmt.Errorf("failed to marshal public key for KID: %w", err)
	}
	h := sha256.Sum256(der)
	return hex.EncodeToString(h[:16]), nil
}

type KVKeyStoreConfig struct {
	Bootstrap         *rsa.PrivateKey
	BootstrapAudience string
	Vault             KeyVault // nil: the wrapped vault of Bootstrap
	Broadcaster       spi.ClusterBroadcaster
	ReconcileInterval time.Duration
	Metrics           ReconcileMetrics
}

type bootstrapKey struct {
	kid      string
	audience string
	public   *rsa.PublicKey
	signer   Signer
}

// KVKeyStore keeps the signing key pairs of the cluster in the KV store and a
// classified copy on every node. Hot paths read the copy; admin changes read
// and write the store (kv_key_store_admin.go).
type KVKeyStore struct {
	rep   *kvReplica[*signingEntry]
	kv    spi.KeyValueStore
	vault KeyVault
	cls   *classifier
	boot  bootstrapKey

	logMu       sync.Mutex
	lastRetired []string
	lastBroken  []string
	lastIgnored []string
}

func NewKVKeyStore(ctx context.Context, kv spi.KeyValueStore, cfg KVKeyStoreConfig) (*KVKeyStore, error) {
	kid, err := DeriveKID(&cfg.Bootstrap.PublicKey)
	if err != nil {
		return nil, err
	}
	vault := cfg.Vault
	if vault == nil {
		if vault, err = NewWrappedVault(cfg.Bootstrap, kid); err != nil {
			return nil, err
		}
	}
	s := &KVKeyStore{
		kv: kv, vault: vault, cls: newClassifier(vault, kid),
		boot: bootstrapKey{kid: kid, audience: cfg.BootstrapAudience, public: &cfg.Bootstrap.PublicKey, signer: NewRSASigner(cfg.Bootstrap)},
	}
	openCtx := context.WithoutCancel(ctx)
	rep, err := newKVReplica(ctx, kv, replicaConfig[*signingEntry]{
		name: "signing-key", namespace: signingKeysNamespace, topic: topicSigningKeys,
		decode: func(k string, d []byte) (string, *signingEntry, bool, error) {
			return k, s.cls.classify(openCtx, k, d), true, nil
		},
		interval: cfg.ReconcileInterval, broadcaster: cfg.Broadcaster, metrics: cfg.Metrics,
		afterChange: s.afterChange,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to load signing keys from KV store: %w", err)
	}
	s.rep = rep
	s.rep.read(s.afterChange)
	s.logRevokedBootstrap(slog.LevelInfo)
	return s, nil
}

// ReconcileInterval is the store's re-read interval, the default applied.
func (s *KVKeyStore) ReconcileInterval() time.Duration { return s.rep.cfg.interval }
func (s *KVKeyStore) Start(ctx context.Context)        { s.rep.Start(ctx) }

type bootstrapView struct {
	usable bool // false: deleted, or its stored state cannot be read
	pair   KeyPair
}

// bootstrapView applies the stored state to the configured bootstrap key.
// Absent state is the default (active, no window); any record at the
// bootstrap KID other than a readable bootstrap state refuses the key.
func (s *KVKeyStore) bootstrapView(recs map[string]*signingEntry) bootstrapView {
	pair := KeyPair{KID: s.boot.kid, Audience: s.boot.audience, Algorithm: "RS256", PublicKey: s.boot.public, Active: true, Bootstrap: true}
	e, ok := recs[s.boot.kid]
	if !ok {
		return bootstrapView{usable: true, pair: pair}
	}
	if e.class != classBootstrapState || e.deleted {
		return bootstrapView{usable: false, pair: pair}
	}
	pair.Active, pair.ValidFrom, pair.ValidTo = e.pair.Active, e.pair.ValidFrom, e.pair.ValidTo
	return bootstrapView{usable: true, pair: pair}
}

// selectSigner applies the signing rule: among owned and broken
// issued key pairs and the bootstrap key of the audience that are active and
// inside their window, the latest validFrom wins, then the greater KID. A
// broken winner, or any undecodable record, fails; another key is never
// chosen instead.
func (s *KVKeyStore) selectSigner(audience string) (*KeyPair, Signer, error) {
	if s.rep.Stale() {
		return nil, nil, ErrStoreStale
	}
	now := time.Now()
	var (
		best        *KeyPair
		bestSigner  Signer
		bestBroken  string
		undecodable []string
	)
	consider := func(p KeyPair, sg Signer, broken string) {
		if p.Audience != audience || !p.Active || !p.InWindow(now) {
			return
		}
		if best == nil || p.ValidFrom.After(best.ValidFrom) || (p.ValidFrom.Equal(best.ValidFrom) && p.KID > best.KID) {
			pc := p
			best, bestSigner, bestBroken = &pc, sg, broken
		}
	}
	s.rep.read(func(recs map[string]*signingEntry) {
		for _, e := range recs {
			switch e.class {
			case classOwned:
				consider(e.pair, e.signer, "")
			case classBroken:
				consider(e.pair, nil, e.reason)
			case classUndecodable:
				undecodable = append(undecodable, e.pair.KID)
			}
		}
		if bv := s.bootstrapView(recs); bv.usable {
			consider(bv.pair, s.boot.signer, "")
		}
	})
	if len(undecodable) > 0 {
		sort.Strings(undecodable)
		return nil, nil, fmt.Errorf("%w: undecodable records %v", ErrKeyPairBroken, undecodable)
	}
	if best == nil {
		return nil, nil, fmt.Errorf("%w: no signing key for audience %q", ErrKeyPairNotFound, audience)
	}
	if bestBroken != "" {
		return nil, nil, fmt.Errorf("%w: %s (%s)", ErrKeyPairBroken, best.KID, bestBroken)
	}
	return best, bestSigner, nil
}

func (s *KVKeyStore) Signer(audience string) (*KeyPair, Signer, error) {
	return s.selectSigner(audience)
}

func (s *KVKeyStore) Current(audience string) (*KeyPair, error) {
	kp, _, err := s.selectSigner(audience)
	return kp, err
}

// VerificationKey returns the public key a token's KID names, if that key
// pair may verify on this node now: owned or the signing key from
// configuration, not deleted, and Verifies(now) — an invalidated key pair
// verifies until the end of its grace period. A kid this store knows that
// may not verify now also answers ErrKeyPairCannotVerify. Every refusal
// wraps ErrKeyPairNotFound. There is no store read on this path.
func (s *KVKeyStore) VerificationKey(kid string) (*rsa.PublicKey, error) {
	if s.rep.Stale() {
		return nil, fmt.Errorf("%w: %s (store stale)", ErrKeyPairNotFound, kid)
	}
	now := time.Now()
	var (
		pub   *rsa.PublicKey
		known bool
	)
	s.rep.read(func(recs map[string]*signingEntry) {
		if kid == s.boot.kid {
			known = true
			if bv := s.bootstrapView(recs); bv.usable && bv.pair.Verifies(now) {
				pub = bv.pair.PublicKey
			}
			return
		}
		e, ok := recs[kid]
		known = ok && e.class != classIgnored
		if ok && e.class == classOwned && e.pair.Verifies(now) {
			pub = e.pair.PublicKey
		}
	})
	switch {
	case pub != nil:
		return pub, nil
	case known:
		return nil, fmt.Errorf("%w: %w: %s", ErrKeyPairCannotVerify, ErrKeyPairNotFound, kid)
	default:
		return nil, fmt.Errorf("%w: %s", ErrKeyPairNotFound, kid)
	}
}

// publishable reports whether a key pair belongs in JWKS: its window has not
// ended (a future validFrom is fine — JWKS publishes a key ahead of its
// window opening), and it is not the one shape Verifies(now) always refuses,
// active or not: an inactive record with no validTo. That shape is not
// reachable through the admin API (Invalidate always sets validTo via
// graceExpiry), but Published must not publish a key that can never verify,
// regardless of how the record arrived.
func publishable(kp *KeyPair, now time.Time) bool {
	return windowOpen(kp.ValidTo, now) && (kp.Active || kp.ValidTo != nil)
}

// Published returns the key pairs for JWKS: owned ones and the bootstrap key
// whose window has not ended, including those in a grace period.
func (s *KVKeyStore) Published() ([]*KeyPair, error) {
	if s.rep.Stale() {
		return nil, ErrStoreStale
	}
	now := time.Now()
	var out []*KeyPair
	s.rep.read(func(recs map[string]*signingEntry) {
		for _, e := range recs {
			if e.class == classOwned && publishable(&e.pair, now) {
				p := e.pair
				out = append(out, &p)
			}
		}
		if bv := s.bootstrapView(recs); bv.usable && publishable(&bv.pair, now) {
			p := bv.pair
			out = append(out, &p)
		}
	})
	sort.Slice(out, func(i, j int) bool { return out[i].KID < out[j].KID })
	return out, nil
}

// afterChange is the replica's afterChange callback: it receives the copy
// directly (never through s.rep, which a construction-time gossip message
// could still be racing to assign) and logs class-set changes and evicts
// cached signers of records that are no longer owned (deleted, retired, or
// reclassified) so they leave memory instead of accumulating.
func (s *KVKeyStore) afterChange(recs map[string]*signingEntry) {
	s.logClassChanges(recs)
	s.retainOwnedSigners(recs)
}

// retainOwnedSigners drops every cached signer whose record is not currently
// classified owned in the copy.
func (s *KVKeyStore) retainOwnedSigners(recs map[string]*signingEntry) {
	owned := map[string]bool{}
	for k, e := range recs {
		if e.class == classOwned {
			owned[k] = true
		}
	}
	s.cls.signers.retain(owned)
}

// logClassChanges logs when the set of retired, of broken and undecodable, or
// of ignored records changes — not on every re-read. Only KV keys and reason
// classes are logged, never record values.
func (s *KVKeyStore) logClassChanges(recs map[string]*signingEntry) {
	var retired, broken, ignored []string
	for k, e := range recs {
		switch e.class {
		case classRetired:
			retired = append(retired, k)
		case classBroken, classUndecodable:
			broken = append(broken, k+" ("+e.reason+")")
		case classIgnored:
			ignored = append(ignored, k)
		}
	}
	sort.Strings(retired)
	sort.Strings(broken)
	sort.Strings(ignored)
	s.logMu.Lock()
	defer s.logMu.Unlock()
	if !slices.Equal(retired, s.lastRetired) {
		if len(retired) > 0 {
			slog.Warn("signing key pairs owned by another bootstrap key are retired on this node",
				"pkg", "auth", "count", len(retired), "kids", retired)
		}
		s.lastRetired = retired
	}
	if !slices.Equal(broken, s.lastBroken) {
		if len(broken) > 0 {
			slog.Error("signing key records this node cannot use",
				"pkg", "auth", "records", broken)
		}
		s.lastBroken = broken
	}
	if !slices.Equal(ignored, s.lastIgnored) {
		if len(ignored) > 0 {
			slog.Error("signing key records at keys that cannot be a key id are ignored",
				"pkg", "auth", "keys", ignored)
		}
		s.lastIgnored = ignored
	}
}

// ownedIssuedCount counts the issued key pairs this node's vault owns: owned
// records, and broken ones the vault attempted to open and failed to (any
// reason but "unknown vault kind" — a record in that class was never sealed
// under this bootstrap key's wrap scheme at all, so replacing the PEM would
// not unseal it, and it must not inflate this count).
func (s *KVKeyStore) ownedIssuedCount() int {
	n := 0
	s.rep.read(func(recs map[string]*signingEntry) {
		for _, e := range recs {
			if e.class == classOwned || (e.class == classBroken && e.reason != "unknown vault kind") {
				n++
			}
		}
	})
	return n
}

// logRevokedBootstrap reports that the signing key from configuration has
// been invalidated or deleted through the API: tokens from `cyoda token`
// stop verifying (after any grace period). When the key also owns issued key
// pairs, it adds that CYODA_JWT_SIGNING_KEY still unseals them: invalidating
// or deleting the key through the API does not protect them, and only
// replacing CYODA_JWT_SIGNING_KEY does.
func (s *KVKeyStore) logRevokedBootstrap(level slog.Level) {
	var revoked bool
	s.rep.read(func(recs map[string]*signingEntry) {
		bv := s.bootstrapView(recs)
		revoked = !bv.usable || !bv.pair.Active
	})
	if !revoked {
		return
	}
	slog.Log(context.Background(), level,
		"the signing key from CYODA_JWT_SIGNING_KEY is invalidated or deleted: tokens from cyoda token are refused once any grace period ends",
		"pkg", "auth")
	if s.vault.Owner() != s.boot.kid {
		return
	}
	if n := s.ownedIssuedCount(); n > 0 {
		slog.Log(context.Background(), level,
			"the signing key no longer signs, but CYODA_JWT_SIGNING_KEY still unseals the issued key pairs it owns; replace it if it may be exposed",
			"pkg", "auth", "ownedKeyPairs", n)
	}
}
