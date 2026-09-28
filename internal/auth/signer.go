package auth

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
)

// Signer signs a SHA-256 digest with RSASSA-PKCS1-v1_5 (RS256). A key vault
// hands out one per key pair; no other code sees the private key behind it.
// ctx bounds a signing call that leaves the process (a KMS).
type Signer interface {
	Public() crypto.PublicKey
	Sign(ctx context.Context, digest []byte) ([]byte, error)
}

type rsaSigner struct{ key *rsa.PrivateKey }

// NewRSASigner wraps an RSA private key held in this process: the bootstrap
// key, a key the wrapped vault has opened, or a test key.
func NewRSASigner(key *rsa.PrivateKey) Signer { return rsaSigner{key: key} }

func (s rsaSigner) Public() crypto.PublicKey { return &s.key.PublicKey }

func (s rsaSigner) Sign(_ context.Context, digest []byte) ([]byte, error) {
	return rsa.SignPKCS1v15(rand.Reader, s.key, crypto.SHA256, digest)
}
