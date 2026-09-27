package auth

import (
	"context"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"
)

const (
	signingKeysNamespace = "signing-keys"
	topicSigningKeys     = "auth.signingkeys"
	recordKindIssued     = "issued"
	recordKindBootstrap  = "bootstrap"
)

// signingRecord is the stored form of an issued key pair or of the bootstrap
// key's state. Unknown JSON fields are ignored.
type signingRecord struct {
	Kind      string          `json:"kind"`
	KID       string          `json:"kid"`
	Audience  string          `json:"audience,omitempty"`
	Algorithm string          `json:"algorithm,omitempty"`
	Active    bool            `json:"active"`
	ValidFrom string          `json:"validFrom"`
	ValidTo   *string         `json:"validTo,omitempty"`
	PublicKey string          `json:"publicKey,omitempty"` // SPKI DER, base64
	Vault     *vaultReference `json:"vault,omitempty"`
	Deleted   bool            `json:"deleted,omitempty"`
}

type vaultReference struct {
	Kind   string `json:"kind"`
	Owner  string `json:"owner"`
	Sealed string `json:"sealed"` // base64
}

type keyClass int

const (
	classOwned keyClass = iota
	classBroken
	classRetired
	classBootstrapState
	classForeignBootstrap
	classUndecodable
)

// signingEntry is one record of the node copy, classified for this node.
type signingEntry struct {
	class   keyClass
	reason  string  // broken or undecodable: the reason class, never key material
	pair    KeyPair // public data; for undecodable only KID is set
	deleted bool    // bootstrap state
	signer  Signer  // owned only
}

func fmtTime(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

func fmtTimePtr(t *time.Time) *string {
	if t == nil {
		return nil
	}
	s := fmtTime(*t)
	return &s
}

func encodeSigningRecord(rec signingRecord) ([]byte, error) {
	b, err := json.Marshal(rec)
	if err != nil {
		return nil, fmt.Errorf("failed to encode signing-key record: %w", err)
	}
	return b, nil
}

func defaultBootstrapRecord(kid string) signingRecord {
	return signingRecord{Kind: recordKindBootstrap, KID: kid, Active: true, ValidFrom: fmtTime(time.Time{})}
}

// decodeSigningRecord parses and validates one stored record.
func decodeSigningRecord(kvKey string, data []byte) (signingRecord, KeyPair, []byte, []byte, error) {
	var rec signingRecord
	if err := json.Unmarshal(data, &rec); err != nil {
		return rec, KeyPair{}, nil, nil, fmt.Errorf("not a record: %w", err)
	}
	if rec.KID != kvKey {
		return rec, KeyPair{}, nil, nil, errors.New("kid does not match its key")
	}
	from, err := time.Parse(time.RFC3339Nano, rec.ValidFrom)
	if err != nil {
		return rec, KeyPair{}, nil, nil, fmt.Errorf("invalid validFrom: %w", err)
	}
	var to *time.Time
	if rec.ValidTo != nil {
		t, err := time.Parse(time.RFC3339Nano, *rec.ValidTo)
		if err != nil {
			return rec, KeyPair{}, nil, nil, fmt.Errorf("invalid validTo: %w", err)
		}
		to = &t
	}
	pair := KeyPair{KID: rec.KID, Active: rec.Active, ValidFrom: from, ValidTo: to}
	switch rec.Kind {
	case recordKindBootstrap:
		pair.Bootstrap, pair.Algorithm = true, "RS256"
		return rec, pair, nil, nil, nil
	case recordKindIssued:
	default:
		return rec, KeyPair{}, nil, nil, fmt.Errorf("unknown record kind %q", rec.Kind)
	}
	if rec.Audience != "client" && rec.Audience != "human" {
		return rec, KeyPair{}, nil, nil, fmt.Errorf("invalid audience %q", rec.Audience)
	}
	if rec.Algorithm != "RS256" || rec.Vault == nil || rec.Vault.Kind == "" || rec.Vault.Owner == "" {
		return rec, KeyPair{}, nil, nil, errors.New("incomplete issued record")
	}
	spki, err := base64.StdEncoding.DecodeString(rec.PublicKey)
	if err != nil {
		return rec, KeyPair{}, nil, nil, errors.New("invalid public key encoding")
	}
	parsed, err := x509.ParsePKIXPublicKey(spki)
	if err != nil {
		return rec, KeyPair{}, nil, nil, errors.New("invalid public key")
	}
	pub, ok := parsed.(*rsa.PublicKey)
	if !ok {
		return rec, KeyPair{}, nil, nil, errors.New("public key is not RSA")
	}
	sealed, err := base64.StdEncoding.DecodeString(rec.Vault.Sealed)
	if err != nil {
		return rec, KeyPair{}, nil, nil, errors.New("invalid sealed encoding")
	}
	pair.Audience, pair.Algorithm, pair.PublicKey = rec.Audience, rec.Algorithm, pub
	return rec, pair, spki, sealed, nil
}

type cachedSigner struct {
	fingerprint [32]byte
	signer      Signer
}

// signerFingerprint hashes every field bound to the sealed key (KID,
// audience, algorithm, owner, SPKI — the same set the vault authenticates as
// AEAD associated data, spec §5.2/§5.3) together with the sealed bytes
// themselves, each length-prefixed to keep the concatenation unambiguous. A
// record rewritten with the same sealed bytes but a different bound field —
// or the same bound fields under new sealed bytes — must never be treated as
// the same cached signer.
func signerFingerprint(meta KeyMeta, sealed []byte) [32]byte {
	var b []byte
	for _, f := range [][]byte{[]byte(meta.KID), []byte(meta.Audience), []byte(meta.Algorithm), []byte(meta.Owner), meta.SPKI, sealed} {
		b = binary.BigEndian.AppendUint32(b, uint32(len(f)))
		b = append(b, f...)
	}
	return sha256.Sum256(b)
}

// signerCache keeps each opened signer until any field bound to the sealed
// key, or the sealed bytes, change (spec §5.3).
type signerCache struct {
	mu sync.Mutex
	m  map[string]cachedSigner
}

// get returns the cached signer for kid if its fingerprint still matches fp.
func (sc *signerCache) get(kid string, fp [32]byte) (Signer, bool) {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	cached, ok := sc.m[kid]
	if !ok || cached.fingerprint != fp {
		return nil, false
	}
	return cached.signer, true
}

// put stores the opened signer for kid keyed by its fingerprint.
func (sc *signerCache) put(kid string, fp [32]byte, s Signer) {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	sc.m[kid] = cachedSigner{fingerprint: fp, signer: s}
}

// retain drops every cached signer whose KID is not in kids. KVKeyStore's
// retainOwnedSigners calls it from the replica's afterChange callback after
// every swap or apply, with the owned KIDs of that copy, so signers of
// records no longer owned (deleted, retired, or reclassified) leave memory
// (spec §5.3).
func (sc *signerCache) retain(kids map[string]bool) {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	for kid := range sc.m {
		if !kids[kid] {
			delete(sc.m, kid)
		}
	}
}

type classifier struct {
	vault   KeyVault
	bootKID string
	signers *signerCache
}

func newClassifier(vault KeyVault, bootKID string) *classifier {
	return &classifier{vault: vault, bootKID: bootKID, signers: &signerCache{m: map[string]cachedSigner{}}}
}

// classify decides what this node does with one stored record (spec §5.5).
func (c *classifier) classify(ctx context.Context, kvKey string, data []byte) *signingEntry {
	rec, pair, spki, sealed, err := decodeSigningRecord(kvKey, data)
	if err != nil {
		return &signingEntry{class: classUndecodable, reason: "undecodable", pair: KeyPair{KID: kvKey}}
	}
	if rec.Kind == recordKindIssued && kvKey == c.bootKID {
		// Two keys can never share one KID; the bootstrap KID is reserved
		// for bootstrap-state records, so an issued record there is refused
		// rather than trusted (fail closed, spec §5.5).
		return &signingEntry{class: classUndecodable, reason: "issued record at the bootstrap key id", pair: KeyPair{KID: kvKey}}
	}
	if rec.Kind == recordKindBootstrap {
		if kvKey == c.bootKID {
			return &signingEntry{class: classBootstrapState, pair: pair, deleted: rec.Deleted}
		}
		return &signingEntry{class: classForeignBootstrap, pair: pair}
	}
	if rec.Vault.Kind != c.vault.Kind() {
		return &signingEntry{class: classBroken, reason: "unknown vault kind", pair: pair}
	}
	if rec.Vault.Owner != c.vault.Owner() {
		return &signingEntry{class: classRetired, pair: pair}
	}
	signer, err := c.open(ctx, KeyMeta{KID: rec.KID, Audience: rec.Audience, Algorithm: rec.Algorithm, Owner: rec.Vault.Owner, SPKI: spki}, sealed)
	if err != nil {
		reason := "cannot open"
		if errors.Is(err, ErrUnseal) {
			reason = err.Error() // "sealed key cannot be opened: <class>"
		}
		return &signingEntry{class: classBroken, reason: reason, pair: pair}
	}
	return &signingEntry{class: classOwned, pair: pair, signer: signer}
}

func (c *classifier) open(ctx context.Context, meta KeyMeta, sealed []byte) (Signer, error) {
	fp := signerFingerprint(meta, sealed)
	if s, ok := c.signers.get(meta.KID, fp); ok {
		return s, nil
	}
	s, err := c.vault.Open(ctx, meta, sealed)
	if err != nil {
		return nil, err
	}
	c.signers.put(meta.KID, fp, s)
	return s, nil
}
