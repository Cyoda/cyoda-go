package auth

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/binary"
	"errors"
	"fmt"
	"math/big"
	"slices"
)

// KeyMeta is what a key vault binds to a sealed private key.
type KeyMeta struct {
	KID       string
	Audience  string
	Algorithm string
	Owner     string
	SPKI      []byte // empty for Generate
}

// KeyVault creates and opens the private keys of issued key pairs. It hands
// out a Signer; no other code sees private-key material. Owner is the value
// the vault writes into every record it seals and the only owner whose
// records it can open.
type KeyVault interface {
	Kind() string
	Owner() string
	Generate(ctx context.Context, meta KeyMeta) (spki, sealed []byte, s Signer, err error)
	Open(ctx context.Context, meta KeyMeta, sealed []byte) (Signer, error)
}

// WrappedVaultKind is the vault kind that seals private keys under a key
// derived from the bootstrap key.
const WrappedVaultKind = "wrapped"

const (
	wrapKeyInfo = "cyoda-signing-key-wrap-v1"
	sealLabel   = "cyoda-signing-key-v1"
)

// ErrUnseal is returned when a sealed key cannot be opened. The wrapped
// message names only the reason class, never key material.
var ErrUnseal = errors.New("sealed key cannot be opened")

type wrappedVault struct {
	owner string
	aead  cipher.AEAD
}

// NewWrappedVault returns the vault that seals private keys with AES-256-GCM
// under a key derived from the bootstrap key; owner is the bootstrap KID.
func NewWrappedVault(bootstrap *rsa.PrivateKey, owner string) (KeyVault, error) {
	key, err := deriveWrappingKey(bootstrap)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("failed to build the wrapping cipher: %w", err)
	}
	aead, err := cipher.NewGCMWithRandomNonce(block)
	if err != nil {
		return nil, fmt.Errorf("failed to build the wrapping cipher: %w", err)
	}
	return &wrappedVault{owner: owner, aead: aead}, nil
}

// deriveWrappingKey derives the wrapping key from the bootstrap key's primes:
// sorted ascending, each left-padded to the modulus length. The primes are the
// same whatever encoding supplied the key; the private exponent is not used,
// because two different values are valid for one key.
func deriveWrappingKey(k *rsa.PrivateKey) ([]byte, error) {
	size := (k.N.BitLen() + 7) / 8
	primes := slices.Clone(k.Primes)
	slices.SortFunc(primes, func(a, b *big.Int) int { return a.Cmp(b) })
	ikm := make([]byte, 0, size*len(primes))
	for _, p := range primes {
		ikm = append(ikm, p.FillBytes(make([]byte, size))...)
	}
	key, err := hkdf.Key(sha256.New, ikm, nil, wrapKeyInfo, 32)
	if err != nil {
		return nil, fmt.Errorf("failed to derive the wrapping key: %w", err)
	}
	return key, nil
}

// associatedData binds every field that must not be swapped under a sealed
// key. The active flag and the window are not bound: changing them never
// re-seals.
func associatedData(m KeyMeta) []byte {
	spkiHash := sha256.Sum256(m.SPKI)
	var b []byte
	for _, f := range [][]byte{[]byte(sealLabel), []byte(m.KID), []byte(m.Audience), []byte(m.Algorithm), []byte(m.Owner), spkiHash[:]} {
		b = binary.BigEndian.AppendUint32(b, uint32(len(f)))
		b = append(b, f...)
	}
	return b
}

func (v *wrappedVault) Kind() string  { return WrappedVaultKind }
func (v *wrappedVault) Owner() string { return v.owner }

func (v *wrappedVault) Generate(_ context.Context, meta KeyMeta) ([]byte, []byte, Signer, error) {
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("failed to generate RSA key: %w", err)
	}
	spki, err := x509.MarshalPKIXPublicKey(&priv.PublicKey)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("failed to marshal public key: %w", err)
	}
	pk8, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("failed to marshal private key: %w", err)
	}
	meta.Owner, meta.SPKI = v.owner, spki
	sealed := v.aead.Seal(nil, nil, pk8, associatedData(meta))
	return spki, sealed, NewRSASigner(priv), nil
}

func (v *wrappedVault) Open(_ context.Context, meta KeyMeta, sealed []byte) (Signer, error) {
	if meta.Owner != v.owner {
		return nil, fmt.Errorf("%w: owner mismatch", ErrUnseal)
	}
	pk8, err := v.aead.Open(nil, nil, sealed, associatedData(meta))
	if err != nil {
		return nil, fmt.Errorf("%w: decryption", ErrUnseal)
	}
	parsed, err := x509.ParsePKCS8PrivateKey(pk8)
	if err != nil {
		return nil, fmt.Errorf("%w: not PKCS#8", ErrUnseal)
	}
	priv, ok := parsed.(*rsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("%w: not RSA", ErrUnseal)
	}
	spki, err := x509.MarshalPKIXPublicKey(&priv.PublicKey)
	if err != nil || !bytes.Equal(spki, meta.SPKI) {
		return nil, fmt.Errorf("%w: public key mismatch", ErrUnseal)
	}
	return NewRSASigner(priv), nil
}
