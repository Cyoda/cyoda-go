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
	"sort"
	"sync"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// trustedKeysNamespacePrefix prefixes the KV namespace that holds one
// tenant's trusted keys.
const trustedKeysNamespacePrefix = "trusted-keys:"

// trustedKeysNamespace is the KV namespace holding tenant's trusted keys,
// keyed by kid. Tenant ids cannot contain ':', so namespaces cannot alias.
func trustedKeysNamespace(t spi.TenantID) string { return trustedKeysNamespacePrefix + string(t) }

// trustedKeyRecord is the JSON-serializable form of a TrustedKey.
type trustedKeyRecord struct {
	KID      string         `json:"kid"`
	TenantID string         `json:"tenantID"`
	JWK      map[string]any `json:"jwk,omitempty"`
	Issuers  []string       `json:"issuers,omitempty"`
	Active   bool           `json:"active"`
	// validFrom / validTo stored as RFC3339Nano strings for precision.
	ValidFrom string  `json:"validFrom"`
	ValidTo   *string `json:"validTo,omitempty"`
	// RSA public key in JWK-like format.
	N string `json:"n"` // base64url-encoded modulus
	E string `json:"e"` // base64url-encoded exponent
}

// errTrustedKeyUndecodable marks a stored record that does not decode, or
// that does not match the namespace and KV key it is stored under.
var errTrustedKeyUndecodable = errors.New("stored trusted-key record does not decode")

// KVTrustedKeyStore stores trusted keys in the SYSTEM-tenant KV store: one
// namespace per tenant, keyed by kid. Key ids are unique within a tenant
// only. There is no node copy: every call reads or writes the store, so a
// change is visible to every node when the call returns, and the token
// exchange reads the key from the store on every exchange. The KV store never
// joins a transaction (the spi.KeyValueStore contract), so a key change never
// rides on a caller's entity transaction.
//
// Changes to one tenant's keys are serialized on this node, so the cap check
// and the sibling invalidation of a rotation see every change made on this
// node before them. Nothing coordinates two nodes. Register, Invalidate,
// Reactivate and Delete read a key and write it back with Put, not the SPI's
// conditional writes, so two changes to one tenant's keys at the same moment
// on two nodes resolve by last write, and the cap can be exceeded by one key
// per node. Two races on one key matter:
//
//   - An Invalidate (or a rotation's write that ends a previous key) on one
//     node racing a Delete of the same key on another reads the key before
//     the delete and writes it after, bringing the deleted key back as an
//     inactive record. That record never verifies (Verifies requires
//     Active), so revocation is unaffected, but it reappears in List until it
//     is deleted again.
//   - A Reactivate on one node racing a Delete of that key on another brings
//     the deleted key back ACTIVE, so it verifies again until it is deleted
//     again.
//
// A Register of the same key id that lands after a Delete leaves the key
// registered; that is a re-registration, not a lost update.
//
// Do not run changes to one key concurrently: one key-management operation at
// a time per key.
type KVTrustedKeyStore struct {
	kv           spi.KeyValueStore
	maxPerTenant int
	locks        [tenantLockStripes]sync.Mutex
}

// NewKVTrustedKeyStore returns a store over kv. maxPerTenant caps the keys
// of one tenant that can verify a subject token; <= 0: no cap.
func NewKVTrustedKeyStore(kv spi.KeyValueStore, maxPerTenant int) *KVTrustedKeyStore {
	return &KVTrustedKeyStore{kv: kv, maxPerTenant: maxPerTenant}
}

func (s *KVTrustedKeyStore) lock(t spi.TenantID) *sync.Mutex {
	return &s.locks[tenantStripe(t)]
}

// read reads and decodes kid's record in tenant's namespace. An absent
// record wraps ErrTrustedKeyNotFound; a record that does not decode is an
// error wrapping errTrustedKeyUndecodable, logged at ERROR; any other error
// is the store failing, wrapped so a storage-unavailable error keeps its
// marker.
func (s *KVTrustedKeyStore) read(ctx context.Context, tenant spi.TenantID, kid string) (*TrustedKey, error) {
	data, err := s.kv.Get(ctx, trustedKeysNamespace(tenant), kid)
	if errors.Is(err, spi.ErrNotFound) {
		return nil, fmt.Errorf("%w: %s", ErrTrustedKeyNotFound, kid)
	}
	if err != nil {
		return nil, fmt.Errorf("failed to read trusted key: %w", err)
	}
	tk, err := decodeTrustedKey(tenant, kid, data)
	if err != nil {
		slog.Error("trusted-key record does not decode", "pkg", "auth", "tenant", string(tenant), "kvKey", kid)
		return nil, err
	}
	return tk, nil
}

// readAll returns tenant's keys, sorted by kid. Records that do not decode
// are skipped and logged at ERROR with their keys.
func (s *KVTrustedKeyStore) readAll(ctx context.Context, tenant spi.TenantID) ([]*TrustedKey, error) {
	entries, err := s.kv.List(ctx, trustedKeysNamespace(tenant))
	if err != nil {
		return nil, fmt.Errorf("failed to list trusted keys: %w", err)
	}
	out := make([]*TrustedKey, 0, len(entries))
	var bad []string
	for kid, data := range entries {
		tk, err := decodeTrustedKey(tenant, kid, data)
		if err != nil {
			bad = append(bad, kid)
			continue
		}
		out = append(out, tk)
	}
	if len(bad) > 0 {
		sort.Strings(bad)
		slog.Error("trusted-key records do not decode and are skipped", "pkg", "auth", "tenant", string(tenant), "kvKeys", bad)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].KID < out[j].KID })
	return out, nil
}

func (s *KVTrustedKeyStore) write(ctx context.Context, tk *TrustedKey) error {
	data, err := serializeTrustedKey(tk)
	if err != nil {
		return err
	}
	if err := s.kv.Put(ctx, trustedKeysNamespace(tk.TenantID), tk.KID, data); err != nil {
		return fmt.Errorf("failed to write trusted key: %w", err)
	}
	return nil
}

// Register adds or replaces tk in its tenant: an upsert keyed on kid, so a
// retried registration succeeds. The per-tenant cap counts every key of the
// tenant that can verify, except tk.KID itself, so an upsert takes no new
// slot; at the cap the error is 400 TRUSTED_KEY_CAP_REACHED.
//
// When invalidatePrevious is true, every other key of the tenant that is
// active or inside its window is made inactive with ValidTo = now; the tenant
// then holds one key that can verify, so the cap never refuses it. The KV SPI
// has no multi-key write, so those keys are written first and tk last: a
// failure stops at the failing write and leaves tk absent, with the previous
// keys either unchanged or already ended — no exchange is accepted that the
// admin asked to end, and a retry completes the rotation.
func (s *KVTrustedKeyStore) Register(ctx context.Context, tk *TrustedKey, invalidatePrevious bool) error {
	if _, err := serializeTrustedKey(tk); err != nil {
		return err
	}
	mu := s.lock(tk.TenantID)
	mu.Lock()
	defer mu.Unlock()
	keys, err := s.readAll(ctx, tk.TenantID)
	if err != nil {
		return err
	}
	now := time.Now()
	if !invalidatePrevious && capReached(keys, tk.KID, s.maxPerTenant, now) {
		return errTrustedKeyCapReached()
	}
	if invalidatePrevious {
		for _, k := range keys {
			if k.KID == tk.KID || (!k.Active && !windowOpen(k.ValidTo, now)) {
				continue
			}
			k.Active = false
			k.ValidTo = graceExpiry(k.ValidTo, now, 0)
			if err := s.write(ctx, k); err != nil {
				return err
			}
		}
	}
	return s.write(ctx, tk)
}

// Get returns tenant's key kid. An absent key wraps ErrTrustedKeyNotFound; a
// key of another tenant is absent from this tenant's namespace. Any other
// error is the store failing, or a stored record that does not decode.
func (s *KVTrustedKeyStore) Get(ctx context.Context, tenantID spi.TenantID, kid string) (*TrustedKey, error) {
	tk, err := s.read(ctx, tenantID, kid)
	if err != nil {
		return nil, err
	}
	return tk, nil
}

// List returns tenant's keys, sorted by kid. A store failure is returned,
// wrapped so a storage-unavailable error keeps its marker.
func (s *KVTrustedKeyStore) List(ctx context.Context, tenantID spi.TenantID) ([]*TrustedKey, error) {
	return s.readAll(ctx, tenantID)
}

// GetForVerification reads tenant's key kid from the store, on every call.
// A kid outside the trusted-key grammar is not read. An absent key, an
// inactive one, or one outside its window [ValidFrom, ValidTo) wraps
// ErrTrustedKeyNotFound; any other error is the store failing (wrapped so a
// storage-unavailable error keeps its marker) or a stored record that does
// not decode.
func (s *KVTrustedKeyStore) GetForVerification(ctx context.Context, tenantID spi.TenantID, kid string) (*TrustedKey, error) {
	if !MatchesTrustedKIDPattern(kid) {
		return nil, fmt.Errorf("%w: kid outside the grammar", ErrTrustedKeyNotFound)
	}
	tk, err := s.read(ctx, tenantID, kid)
	if err != nil {
		return nil, err
	}
	if !tk.Verifies(time.Now()) {
		return nil, fmt.Errorf("%w: %s", ErrTrustedKeyNotFound, kid)
	}
	return tk, nil
}

// Delete removes tenant's key kid, decodable or not: the record is in the
// caller's tenant's namespace, so the delete stays within that tenant. An
// absent key wraps ErrTrustedKeyNotFound.
func (s *KVTrustedKeyStore) Delete(ctx context.Context, tenantID spi.TenantID, kid string) error {
	mu := s.lock(tenantID)
	mu.Lock()
	defer mu.Unlock()
	if _, err := s.kv.Get(ctx, trustedKeysNamespace(tenantID), kid); errors.Is(err, spi.ErrNotFound) {
		return fmt.Errorf("%w: %s", ErrTrustedKeyNotFound, kid)
	} else if err != nil {
		return fmt.Errorf("failed to read trusted key: %w", err)
	}
	if err := s.kv.Delete(ctx, trustedKeysNamespace(tenantID), kid); err != nil {
		return fmt.Errorf("failed to delete trusted key: %w", err)
	}
	return nil
}

// Invalidate ends tenant's key kid at once: inactive, with ValidTo = now, or
// its current ValidTo if that is earlier. An absent key wraps
// ErrTrustedKeyNotFound; a record that does not decode cannot be changed (a
// store error, not not-found — Delete removes it).
func (s *KVTrustedKeyStore) Invalidate(ctx context.Context, tenantID spi.TenantID, kid string) error {
	mu := s.lock(tenantID)
	mu.Lock()
	defer mu.Unlock()
	tk, err := s.read(ctx, tenantID, kid)
	if err != nil {
		return err
	}
	tk.Active = false
	tk.ValidTo = graceExpiry(tk.ValidTo, time.Now(), 0)
	return s.write(ctx, tk)
}

// Reactivate makes tenant's key kid active with the window
// [validFrom, validTo). validTo is required (non-zero), must be strictly in
// the future, and must be after validFrom. A reactivated key verifies again,
// so it is held to the per-tenant cap. An absent key wraps
// ErrTrustedKeyNotFound.
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
	mu := s.lock(tenantID)
	mu.Lock()
	defer mu.Unlock()
	tk, err := s.read(ctx, tenantID, kid)
	if err != nil {
		return err
	}
	keys, err := s.readAll(ctx, tenantID)
	if err != nil {
		return err
	}
	if capReached(keys, kid, s.maxPerTenant, time.Now()) {
		return errTrustedKeyCapReached()
	}
	vt := validTo
	tk.Active, tk.ValidFrom, tk.ValidTo = true, validFrom, &vt
	return s.write(ctx, tk)
}

// --- Serialization ---

// serializeTrustedKey refuses a key whose window decodeTrustedKey could not
// read back (StorableTime): such a record would be undecodable.
func serializeTrustedKey(tk *TrustedKey) ([]byte, error) {
	if !StorableTime(tk.ValidFrom) {
		return nil, errors.New("failed to encode trusted-key record: validFrom out of range")
	}
	if tk.ValidTo != nil && !StorableTime(*tk.ValidTo) {
		return nil, errors.New("failed to encode trusted-key record: validTo out of range")
	}
	rec := trustedKeyRecord{
		KID:       tk.KID,
		TenantID:  string(tk.TenantID),
		JWK:       tk.JWK,
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

// decodeTrustedKey decodes a record and binds it to the namespace tenant and
// KV key it is stored under. Every failure wraps errTrustedKeyUndecodable.
func decodeTrustedKey(tenant spi.TenantID, kid string, data []byte) (*TrustedKey, error) {
	tk, err := deserializeTrustedKey(data)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", errTrustedKeyUndecodable, err)
	}
	if tk.KID != kid || tk.TenantID != tenant {
		return nil, fmt.Errorf("%w: record does not match its key or namespace", errTrustedKeyUndecodable)
	}
	return tk, nil
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

	// Deserialize accepts exactly the range serializeTrustedKey writes
	// (StorableTime), so both sides agree on what a record can hold.
	validFrom, err := time.Parse(time.RFC3339Nano, rec.ValidFrom)
	if err != nil {
		return nil, fmt.Errorf("invalid validFrom: %w", err)
	}
	if !StorableTime(validFrom) {
		return nil, errors.New("validFrom out of range")
	}

	var validTo *time.Time
	if rec.ValidTo != nil {
		t, err := time.Parse(time.RFC3339Nano, *rec.ValidTo)
		if err != nil {
			return nil, fmt.Errorf("invalid validTo: %w", err)
		}
		if !StorableTime(t) {
			return nil, errors.New("validTo out of range")
		}
		validTo = &t
	}

	return &TrustedKey{
		KID:       rec.KID,
		TenantID:  spi.TenantID(rec.TenantID),
		JWK:       rec.JWK,
		PublicKey: pubKey,
		Issuers:   rec.Issuers,
		Active:    rec.Active,
		ValidFrom: validFrom,
		ValidTo:   validTo,
	}, nil
}

func encodeBase64URL(data []byte) string {
	return base64.RawURLEncoding.EncodeToString(data)
}
