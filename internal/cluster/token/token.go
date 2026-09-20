package token

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/cyoda-platform/cyoda-go/internal/fence"
)

var (
	ErrTokenExpired   = errors.New("token expired")
	ErrTokenInvalid   = errors.New("token invalid")
	ErrTokenTampered  = errors.New("token signature mismatch")
	ErrSecretTooShort = errors.New("HMAC secret must be at least 32 bytes")
)

// Pair names one callout at one fencing number. It is the fence's type: the
// pass carries what the fence judges.
type Pair = fence.Pair

// Claims is what a pass says. NodeID is the owner — the pnode that holds the
// transaction and whose fence judges the pass. Callout, Major and Minor name
// the try the pass was minted for; Outer names every enclosing callout, for a
// callout made from inside a callback.
type Claims struct {
	NodeID    string `json:"n"`
	TxRef     string `json:"t"`
	ExpiresAt int64  `json:"e"` // Unix seconds
	Callout   string `json:"c"`
	Major     uint32 `json:"j"`
	Minor     uint32 `json:"i,omitempty"`
	Outer     []Pair `json:"o,omitempty"`
}

type Signer struct {
	secret []byte
}

func NewSigner(secret []byte) (*Signer, error) {
	if len(secret) < 32 {
		return nil, ErrSecretTooShort
	}
	return &Signer{secret: secret}, nil
}

// Issue signs claims as given. Whether they make a valid pass is Verify's to
// say.
func (s *Signer) Issue(claims Claims) (string, error) {
	payload, err := json.Marshal(claims)
	if err != nil {
		return "", fmt.Errorf("failed to marshal claims: %w", err)
	}

	sig := s.sign(payload)
	return base64.RawURLEncoding.EncodeToString(payload) + "." +
		base64.RawURLEncoding.EncodeToString(sig), nil
}

func (s *Signer) Verify(tok string) (*Claims, error) {
	dot := -1
	for i := len(tok) - 1; i >= 0; i-- {
		if tok[i] == '.' {
			dot = i
			break
		}
	}
	if dot < 0 {
		return nil, ErrTokenInvalid
	}

	payloadB64 := tok[:dot]
	sigB64 := tok[dot+1:]

	payload, err := base64.RawURLEncoding.DecodeString(payloadB64)
	if err != nil {
		return nil, ErrTokenInvalid
	}
	sig, err := base64.RawURLEncoding.DecodeString(sigB64)
	if err != nil {
		return nil, ErrTokenInvalid
	}

	expected := s.sign(payload)
	if !hmac.Equal(sig, expected) {
		return nil, ErrTokenTampered
	}

	var claims Claims
	if err := json.Unmarshal(payload, &claims); err != nil {
		return nil, ErrTokenInvalid
	}

	if !named(Pair{Callout: claims.Callout, Major: claims.Major}) {
		return nil, ErrTokenInvalid
	}
	for _, p := range claims.Outer {
		if !named(p) {
			return nil, ErrTokenInvalid
		}
	}

	if time.Now().Unix() > claims.ExpiresAt {
		return nil, ErrTokenExpired
	}

	return &claims, nil
}

// named reports whether p names a callout at a number that can ever be
// current: the first try of a callout carries major 1.
func named(p Pair) bool { return p.Callout != "" && p.Major != 0 }

func (s *Signer) sign(payload []byte) []byte {
	mac := hmac.New(sha256.New, s.secret)
	mac.Write(payload)
	return mac.Sum(nil)
}
