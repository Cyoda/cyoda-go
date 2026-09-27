package auth

import (
	"context"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"net/http"
	"strings"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/common"
)

const trustedKeysNamespace = "trusted-keys"

// topicTrustedKeys is the gossip topic for trusted-key change pings. The
// payload is always empty and receivers must never read or log it — it is
// arbitrary peer-controlled bytes; the ping's only information is that it
// arrived.
const topicTrustedKeys = "auth.trustedkeys"

// defaultMaxTrustedKeys caps the number of trusted keys a store will accept by
// default per tenant. Trusted keys are an admin-managed registry — a 100-key
// default covers expected operational use (rotations, multi-issuer federation)
// and defends against runaway registration if the admin endpoint is ever
// misconfigured. Override via WithMaxTrustedKeys.
const defaultMaxTrustedKeys = 100

// KVTrustedKeyStoreOption configures a KVTrustedKeyStore at construction time.
type KVTrustedKeyStoreOption func(*kvTrustedKeyStoreConfig)

type kvTrustedKeyStoreConfig struct {
	maxTrustedKeys    int
	broadcaster       spi.ClusterBroadcaster
	reconcileInterval time.Duration
	metrics           ReconcileMetrics
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

// WithReconcileInterval overrides the periodic KV-reconcile interval
// (default 60s, matching CYODA_AUTH_CACHE_RECONCILE_INTERVAL). Values <= 0
// fall back to the default. The actual per-tick wait is jittered ±10% to
// avoid a cross-node reconcile herd.
func WithReconcileInterval(d time.Duration) KVTrustedKeyStoreOption {
	return func(c *kvTrustedKeyStoreConfig) {
		if d > 0 {
			c.reconcileInterval = d
		}
	}
}

// WithReconcileMetrics wires reconcile health signals (consecutive failures,
// staleness seconds) to an observability backend. Defaults to
// NopReconcileMetrics when unset.
func WithReconcileMetrics(m ReconcileMetrics) KVTrustedKeyStoreOption {
	return func(c *kvTrustedKeyStoreConfig) {
		c.metrics = m
	}
}

// WithTrustedKeyBroadcaster wires the cluster gossip fast path: mutations
// publish a payload-free ping on topicTrustedKeys, and received pings
// trigger a coalesced Reconcile. Nil (single-node) disables the fast path;
// the periodic reconcile loop is unaffected.
func WithTrustedKeyBroadcaster(b spi.ClusterBroadcaster) KVTrustedKeyStoreOption {
	return func(c *kvTrustedKeyStoreConfig) {
		c.broadcaster = b
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

// KVTrustedKeyStore persists trusted keys via a KeyValueStore backend, kept
// current by a kvReplica. Hot verification reads the node copy; every admin
// method reads the store itself so an admin decision is never made on a
// possibly stale copy, and admin writes never hold the copy lock across the
// store call.
type KVTrustedKeyStore struct {
	rep          *kvReplica[*TrustedKey]
	kv           spi.KeyValueStore
	maxPerTenant int
}

// NewKVTrustedKeyStore creates a KVTrustedKeyStore, loading any existing keys
// from the KV backend. Pass WithMaxTrustedKeys to override the default cap.
func NewKVTrustedKeyStore(ctx context.Context, kv spi.KeyValueStore, opts ...KVTrustedKeyStoreOption) (*KVTrustedKeyStore, error) {
	cfg := kvTrustedKeyStoreConfig{maxTrustedKeys: defaultMaxTrustedKeys, reconcileInterval: defaultReconcileInterval, metrics: NopReconcileMetrics{}}
	for _, opt := range opts {
		opt(&cfg)
	}
	rep, err := newKVReplica(ctx, kv, replicaConfig[*TrustedKey]{
		name: "trusted-key", namespace: trustedKeysNamespace, topic: topicTrustedKeys,
		decode: decodeTrustedEntry, interval: cfg.reconcileInterval,
		broadcaster: cfg.broadcaster, metrics: cfg.metrics,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to load trusted keys from KV store: %w", err)
	}
	if rep.skippedAtLoad > 0 {
		slog.Warn("skipped pre-v0.8.0 trusted-key entries without tenant scope",
			"count", rep.skippedAtLoad, "namespace", trustedKeysNamespace)
	}
	return &KVTrustedKeyStore{rep: rep, kv: kv, maxPerTenant: cfg.maxTrustedKeys}, nil
}

// decodeTrustedEntry keys the copy by bare KID: a KID is unique across
// tenants, which Register enforces. Entries without a tenant prefix predate
// tenant scoping and are skipped.
func decodeTrustedEntry(kvKey string, data []byte) (string, *TrustedKey, bool, error) {
	if !strings.Contains(kvKey, ":") {
		return "", nil, false, nil
	}
	tk, err := deserializeTrustedKey(data)
	if err != nil {
		return "", nil, false, err
	}
	if tk.TenantID == "" {
		return "", nil, false, nil
	}
	return tk.KID, tk, true, nil
}

func (s *KVTrustedKeyStore) StartReconcileLoop(ctx context.Context) bool { return s.rep.Start(ctx) }

// storedKeys reads the whole namespace from the store: admin decisions are
// made on stored state, never on the copy.
func (s *KVTrustedKeyStore) storedKeys(ctx context.Context) (map[string][]byte, map[string]*TrustedKey, error) {
	entries, err := s.kv.List(ctx, trustedKeysNamespace)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to list trusted keys: %w", err)
	}
	keys := make(map[string]*TrustedKey, len(entries))
	for kvKey, data := range entries {
		if k, tk, ok, err := decodeTrustedEntry(kvKey, data); err == nil && ok {
			keys[k] = tk
		}
	}
	return entries, keys, nil
}

// storedKey reads one key of tenantID from the store.
func (s *KVTrustedKeyStore) storedKey(ctx context.Context, tenantID spi.TenantID, kid string) ([]byte, *TrustedKey, error) {
	data, err := s.kv.Get(ctx, trustedKeysNamespace, trustedKeyKey(tenantID, kid))
	if errors.Is(err, spi.ErrNotFound) {
		return nil, nil, fmt.Errorf("%w: %s", ErrTrustedKeyNotFound, kid)
	}
	if err != nil {
		return nil, nil, fmt.Errorf("failed to read trusted key: %w", err)
	}
	tk, err := deserializeTrustedKey(data)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to decode trusted key: %w", err)
	}
	if tk.TenantID != tenantID {
		return nil, nil, fmt.Errorf("%w: %s", ErrTrustedKeyNotFound, kid)
	}
	return data, tk, nil
}

// Register adds or replaces a trusted key and persists it. Per the cyoda
// cloud trusted-key contract this is an upsert keyed on KID — re-registering
// an existing KID atomically replaces the JWK material under the same record,
// which makes the endpoint idempotent / retry-safe during key rotation.
//
// Cross-tenant KID collision returns 409 KEY_OWNED_BY_DIFFERENT_TENANT.
// Per-tenant cap (counts every key that can still verify, excluding the KID
// being registered so same-KID upserts don't consume a slot) returns
// 400 TRUSTED_KEY_CAP_REACHED. When opts.Invalidate is true, every other key of
// the tenant whose window is still open is marked inactive with the grace
// expiry (graceExpiry).
//
// The decision (cap, collision, which siblings to flip) is made on state read
// straight from the store, never the node's copy, so a rotation always sees a
// sibling registered on another node that this node has not yet reconciled.
// All writes go through writeAll: if any of them fails, it tries to restore
// every write already applied, including the failing one; a restore that
// itself fails is logged at ERROR with the keys left changed, and a crash
// between writes (rather than a reported failure) can still leave the new
// key committed alongside a stale or half-flipped sibling, for the admin to
// repeat.
func (s *KVTrustedKeyStore) Register(ctx context.Context, tk *TrustedKey, opts RotateOptions) error {
	return s.rep.mutate(func() (func(map[string]*TrustedKey), bool, error) {
		entries, stored, err := s.storedKeys(ctx)
		if err != nil {
			return nil, false, err
		}
		if existing, ok := stored[tk.KID]; ok && existing.TenantID != tk.TenantID {
			return nil, false, common.Operational(http.StatusConflict, common.ErrCodeKeyOwnedByDifferentTenant, "key with this keyId belongs to a different tenant")
		}
		if capReached(stored, tk.TenantID, tk.KID, s.maxPerTenant, time.Now()) {
			return nil, false, errTrustedKeyCapReached()
		}
		created := copyTrustedKey(tk)
		data, err := serializeTrustedKey(created)
		if err != nil {
			return nil, false, err
		}
		kvKey := trustedKeyKey(tk.TenantID, tk.KID)
		writes := []kvWrite{{key: kvKey, value: data, prev: entries[kvKey]}}
		changed := []*TrustedKey{created}
		if opts.Invalidate {
			now := time.Now()
			for _, k := range stored {
				if k.TenantID != tk.TenantID || k.KID == tk.KID || !windowOpen(k.ValidTo, now) {
					continue
				}
				sib := copyTrustedKey(k)
				sib.Active = false
				sib.ValidTo = graceExpiry(k.ValidTo, now, opts.GracePeriodSec)
				b, err := serializeTrustedKey(sib)
				if err != nil {
					return nil, false, err
				}
				sk := trustedKeyKey(k.TenantID, k.KID)
				writes = append(writes, kvWrite{key: sk, value: b, prev: entries[sk]})
				changed = append(changed, sib)
			}
		}
		if err := s.rep.writeAll(ctx, writes); err != nil {
			return nil, true, err
		}
		return func(m map[string]*TrustedKey) {
			for _, k := range changed {
				m[k.KID] = k
			}
		}, true, nil
	})
}

// Get retrieves a trusted key by tenant and KID. When the node copy is not
// stale it is served from there for speed; otherwise (and on a copy miss) it
// reads through to the store for multi-node visibility. Returns an error
// wrapping ErrTrustedKeyNotFound for absence or cross-tenant; any other error
// is a store failure.
func (s *KVTrustedKeyStore) Get(ctx context.Context, tenantID spi.TenantID, kid string) (*TrustedKey, error) {
	if !s.rep.Stale() {
		var hit *TrustedKey
		s.rep.read(func(m map[string]*TrustedKey) {
			if tk, ok := m[kid]; ok {
				hit = copyTrustedKey(tk)
			}
		})
		if hit != nil {
			if hit.TenantID != tenantID {
				return nil, fmt.Errorf("%w: %s", ErrTrustedKeyNotFound, kid)
			}
			return hit, nil
		}
	}
	tk, found, err := s.rep.loadOne(ctx, trustedKeyKey(tenantID, kid))
	if err != nil {
		return nil, err
	}
	if !found || tk.TenantID != tenantID {
		return nil, fmt.Errorf("%w: %s", ErrTrustedKeyNotFound, kid)
	}
	return copyTrustedKey(tk), nil
}

// List returns all trusted keys for the given tenant, from the node copy.
func (s *KVTrustedKeyStore) List(tenantID spi.TenantID) []*TrustedKey {
	out := make([]*TrustedKey, 0)
	s.rep.read(func(m map[string]*TrustedKey) {
		for _, tk := range m {
			if tk.TenantID == tenantID {
				out = append(out, copyTrustedKey(tk))
			}
		}
	})
	return out
}

// GetForVerification implements TrustedKeyStore. It reads the node copy only:
// a key registered on another node is verifiable once gossip or the
// reconcile loop has brought it here. Never blocks on an admin write to this
// or any other key — it never touches the store.
func (s *KVTrustedKeyStore) GetForVerification(tenantID spi.TenantID, kid string) (*TrustedKey, error) {
	if s.rep.Stale() {
		// Fail closed: the copy can no longer prove this key was not revoked.
		// The reconcile loop is already logging at ERROR.
		return nil, fmt.Errorf("%w: %s (trusted-key cache stale)", ErrTrustedKeyNotFound, kid)
	}
	var out *TrustedKey
	s.rep.read(func(m map[string]*TrustedKey) {
		if tk, ok := m[kid]; ok && tk.TenantID == tenantID && windowOpen(tk.ValidTo, time.Now()) {
			out = copyTrustedKey(tk)
		}
	})
	if out == nil {
		return nil, fmt.Errorf("%w: %s", ErrTrustedKeyNotFound, kid)
	}
	return out, nil
}

// Delete removes a trusted key by tenant and KID. The store, not the node
// copy, decides whether the key exists: a node that has not yet reconciled a
// peer's delete must not resurrect the key by reporting it present.
func (s *KVTrustedKeyStore) Delete(ctx context.Context, tenantID spi.TenantID, kid string) error {
	return s.rep.mutate(func() (func(map[string]*TrustedKey), bool, error) {
		data, _, err := s.storedKey(ctx, tenantID, kid)
		if err != nil {
			return nil, false, err
		}
		if err := s.rep.writeAll(ctx, []kvWrite{{key: trustedKeyKey(tenantID, kid), prev: data}}); err != nil {
			return nil, true, err
		}
		return func(m map[string]*TrustedKey) { delete(m, kid) }, true, nil
	})
}

// Invalidate marks a trusted key as inactive, sets ValidTo to the grace
// expiry (graceExpiry: now+gracePeriodSec, never later than the key's current
// ValidTo), and persists. Returns an error wrapping ErrTrustedKeyNotFound if
// the key is not found in the store for this tenant — a stale node copy
// alone is never grounds to invalidate, and never grounds to report the key
// missing when it is not.
func (s *KVTrustedKeyStore) Invalidate(ctx context.Context, tenantID spi.TenantID, kid string, gracePeriodSec int64) error {
	return s.update(ctx, tenantID, kid, func(tk *TrustedKey, _ map[string]*TrustedKey) error {
		tk.Active = false
		tk.ValidTo = graceExpiry(tk.ValidTo, time.Now(), gracePeriodSec)
		return nil
	}, false)
}

// Reactivate sets a trusted key as active, updates its validity window, and
// persists. Returns an error if the key does not exist or belongs to a
// different tenant. validTo is required (non-zero), must be strictly in the
// future, and must be after validFrom.
func (s *KVTrustedKeyStore) Reactivate(ctx context.Context, tenantID spi.TenantID, kid string, validFrom, validTo time.Time) error {
	if validTo.IsZero() {
		return fmt.Errorf("validTo required for reactivation")
	}
	if !validTo.After(time.Now()) {
		return fmt.Errorf("validTo must be in the future")
	}
	if !validTo.After(validFrom) {
		return fmt.Errorf("validTo must be after validFrom")
	}
	return s.update(ctx, tenantID, kid, func(tk *TrustedKey, stored map[string]*TrustedKey) error {
		// A reactivated key verifies again, so it is held to the cap.
		if capReached(stored, tenantID, kid, s.maxPerTenant, time.Now()) {
			return errTrustedKeyCapReached()
		}
		tk.Active, tk.ValidFrom = true, validFrom
		vt := validTo
		tk.ValidTo = &vt
		return nil
	}, true)
}

// update changes one stored key, reading ground truth from the store (never
// the node copy) before applying change, and writing the result back through
// writeAll, which tries to undo a failed write (see Register's doc comment
// for what that guarantees and does not). needAll also lists the whole
// namespace first, for change functions (the cap check) that need it.
func (s *KVTrustedKeyStore) update(ctx context.Context, tenantID spi.TenantID, kid string,
	change func(tk *TrustedKey, stored map[string]*TrustedKey) error, needAll bool) error {
	return s.rep.mutate(func() (func(map[string]*TrustedKey), bool, error) {
		var stored map[string]*TrustedKey
		if needAll {
			var err error
			if _, stored, err = s.storedKeys(ctx); err != nil {
				return nil, false, err
			}
		}
		data, tk, err := s.storedKey(ctx, tenantID, kid)
		if err != nil {
			return nil, false, err
		}
		if err := change(tk, stored); err != nil {
			return nil, false, err
		}
		b, err := serializeTrustedKey(tk)
		if err != nil {
			return nil, false, err
		}
		if err := s.rep.writeAll(ctx, []kvWrite{{key: trustedKeyKey(tenantID, kid), value: b, prev: data}}); err != nil {
			return nil, true, err
		}
		return func(m map[string]*TrustedKey) { m[kid] = tk }, true, nil
	})
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
