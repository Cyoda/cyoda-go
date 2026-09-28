# Signing Key Pairs Shared by the Cluster — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Store JWT signing key pairs in the SYSTEM-tenant KV store — private keys sealed under a key derived from the bootstrap key — so a key pair issued, invalidated, reactivated or deleted on any node takes effect on every node and survives a restart, including revocations of the bootstrap key.

**Architecture:** A generic replicated-KV component (node copy + gossip change message + periodic re-read + admin write path with compensation) is extracted from the trusted-key store; the trusted-key store and a new `KVKeyStore` both use it. A `KeyVault` interface creates and opens private keys and hands out a `Signer`; the default wrapped vault seals with AES-256-GCM under an HKDF key derived from the bootstrap key's primes. Records are classified per node (owned, broken, retired, bootstrap state, foreign, undecodable); hot paths read only the node copy.

**Tech Stack:** Go 1.26 (`crypto/hkdf`, `cipher.NewGCMWithRandomNonce`), SPI `KeyValueStore` and `ClusterBroadcaster`, memory/sqlite/postgres plugins, testcontainers PostgreSQL, parity harness.

**Spec:** `docs/superpowers/specs/2026-09-26-285-cluster-signing-keys-design.md` (read it before any task). Facts: `docs/superpowers/research/2026-09-26-285-signing-keys-research.md`.

## Global Constraints

- Branch `feat/285-cluster-signing-keys` in worktree `.claude/worktrees/feat+285-cluster-signing-keys`; the PR targets `release/v0.9.0`.
- Go 1.26; `log/slog` only; wrap errors with `fmt.Errorf("failed to X: %w", err)`.
- Never log private keys, sealed bytes, full records, PEMs or tokens (Gate 3). KIDs and KV keys may be logged.
- 4xx: domain detail and error code. 5xx: `common.Internal` (generic message + ticket UUID; 503 when the error carries the storage-unavailable marker).
- No issue numbers (`#285`, `#624`, …) in code, comments, log messages, help topics, OpenAPI or error docs. Commit messages and the PR body may carry them.
- No test hooks in production code: fakes live in `_test.go` files.
- TDD for every task: failing test first, run it, see it fail for the stated reason, then implement.
- Iterate with `go test ./internal/auth/...` (one package) or `make test`. Never add `-count=1`. Never hand-roll `go test ./...` as verification.
- Every commit ends with the trailer `Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>`.
- KV namespace `signing-keys`; gossip topic `auth.signingkeys`; metrics `auth.signingkeys.reconcile_consecutive_failures`, `auth.signingkeys.reconcile_staleness_seconds`; trusted-key names unchanged.
- HKDF info `"cyoda-signing-key-wrap-v1"`; associated-data label `"cyoda-signing-key-v1"`.
- No new environment variable and no new error code.

## Review Focus

1. **A bootstrap key whose audience is `human` (`CYODA_JWT_BOOTSTRAP_AUDIENCE=human`)** — a `client` rotation with `invalidateCurrent` must not invalidate it. Test: Task 7, `TestKVKeyStore_RotationLeavesOtherAudienceBootstrap`.
2. **The same bootstrap key supplied as PKCS#1 on one node and PKCS#8 on another** — both must derive the same wrapping key, or one node retires the other's key pairs. Test: Task 2, `TestWrappingKey_IndependentOfEncoding`.
3. **A multi-prime RSA bootstrap key** — all primes enter the derivation, sorted. Test: Task 2, `TestWrappingKey_MultiPrime`.
4. **A stored record with fields this version does not know** (written by a later version during a rolling upgrade) — it must still decode, not become undecodable and stop all signing. Test: Task 5, `TestDecodeSigningRecord_IgnoresUnknownFields`.
5. **Two admins issuing key pairs on the same node at the same moment** — both succeed with distinct KIDs, and the signer choice is deterministic. Test: Task 7, `TestKVKeyStore_ConcurrentIssueOnOneNode`.

## Execution streams

| Stream | Tasks | Depends on |
|---|---|---|
| A — crypto | 1 → 2 | — |
| B — replication | 3 → 4 | — |
| C — signing-key store | 5 → 6 → 7 → 8 | A and B |
| D — tests and docs | 9, 10, 11, 13 (in parallel) | 8 |
| E — cassandra | 12 | — (separate repository) |
| Final | 14 | all |

Streams A, B and E are disjoint and may run concurrently in separate worktrees.

---

### Task 1: `Signer` and `jwt.Sign` over a signer

**Files:**
- Create: `internal/auth/signer.go`, `internal/auth/signer_test.go`
- Modify: `internal/auth/jwt.go:17-41` (`Sign`), `internal/auth/token.go:65-110,221-245` (callers), `e2e/parity/fixtureutil/fixtureutil.go:160,190,226,249,287`
- Modify (tests, mechanical): `internal/auth/delegating_test.go`, `jwt_alg_pinning_test.go`, `jwt_test.go`, `local_validator_integration_test.go`, `test_helpers_test.go`, `token_test.go`, `validator_test.go`, `internal/e2e/attribution_test.go`, `internal/e2e/auth_failures_test.go`, `internal/e2e/token_exchange_test.go`, `internal/grpc/interceptor_test.go`

**Interfaces:**
- Produces: `type Signer interface { Public() crypto.PublicKey; Sign(ctx context.Context, digest []byte) ([]byte, error) }`; `func NewRSASigner(key *rsa.PrivateKey) Signer`; `func Sign(ctx context.Context, claims map[string]any, signer Signer, kid string) (string, error)`.

- [ ] **Step 1: Write the failing test** — `internal/auth/signer_test.go`:

```go
package auth_test

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"testing"

	"github.com/cyoda-platform/cyoda-go/internal/auth"
)

func TestRSASigner_SignsPKCS1v15SHA256(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	s := auth.NewRSASigner(key)
	if pub, ok := s.Public().(*rsa.PublicKey); !ok || !pub.Equal(&key.PublicKey) {
		t.Fatalf("Public() = %v, want the key's public key", s.Public())
	}
	digest := sha256.Sum256([]byte("payload"))
	sig, err := s.Sign(context.Background(), digest[:])
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	if err := rsa.VerifyPKCS1v15(&key.PublicKey, crypto.SHA256, digest[:], sig); err != nil {
		t.Fatalf("signature does not verify: %v", err)
	}
}

func TestSign_UsesSigner(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	tok, err := auth.Sign(context.Background(), map[string]any{"sub": "x"}, auth.NewRSASigner(key), "kid-1")
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	parsed, err := auth.Parse(tok)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if parsed.Header["kid"] != "kid-1" {
		t.Fatalf("kid = %v", parsed.Header["kid"])
	}
	h := sha256.Sum256([]byte(parsed.SigningInput))
	if err := rsa.VerifyPKCS1v15(&key.PublicKey, crypto.SHA256, h[:], parsed.Signature); err != nil {
		t.Fatalf("token signature does not verify: %v", err)
	}
}
```

- [ ] **Step 2: Run it and see it fail**

Run: `go test ./internal/auth/ -run 'TestRSASigner|TestSign_UsesSigner'`
Expected: build failure — `undefined: auth.NewRSASigner`, and `Sign` has the wrong argument list.

- [ ] **Step 3: Implement** — `internal/auth/signer.go`:

```go
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
```

In `internal/auth/jwt.go` replace `Sign`:

```go
// Sign creates a signed RS256 JWT with signer.
func Sign(ctx context.Context, claims map[string]any, signer Signer, kid string) (string, error) {
	header := map[string]string{"alg": "RS256", "typ": "JWT", "kid": kid}
	headerJSON, err := json.Marshal(header)
	if err != nil {
		return "", fmt.Errorf("failed to marshal header: %w", err)
	}
	claimsJSON, err := json.Marshal(claims)
	if err != nil {
		return "", fmt.Errorf("failed to marshal claims: %w", err)
	}
	signingInput := base64.RawURLEncoding.EncodeToString(headerJSON) + "." +
		base64.RawURLEncoding.EncodeToString(claimsJSON)
	hash := sha256.Sum256([]byte(signingInput))
	sig, err := signer.Sign(ctx, hash[:])
	if err != nil {
		return "", fmt.Errorf("failed to sign token: %w", err)
	}
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}
```

Remove the imports `crypto`, `crypto/rand` and `crypto/rsa` from `jwt.go` only if `go vet` reports them unused (`crypto/rsa` stays: `ParseRSAPrivateKeyFromPEM` uses it).

`internal/auth/token.go`: `handleClientCredentials` gains the request so it can pass its context. Change the dispatch at `:67` to `h.handleClientCredentials(w, r, clientID)`, the signature to `func (h *tokenHandler) handleClientCredentials(w http.ResponseWriter, r *http.Request, clientID string)`, and both signing calls (`:102`, `:241`) to:

```go
token, err := Sign(r.Context(), claims, NewRSASigner(kp.PrivateKey), kp.KID)
```

(`NewRSASigner(kp.PrivateKey)` is temporary; Task 8 replaces it with the store's signer.)

Every other caller changes from `auth.Sign(claims, key, kid)` to `auth.Sign(context.Background(), claims, auth.NewRSASigner(key), kid)` (inside package `auth`: `Sign(context.Background(), claims, NewRSASigner(key), kid)`). In `fixtureutil.go` the key is `ks.Key`. Add the `context` import where missing. Find every site with:

```bash
grep -rn "auth\.Sign(\|[^.a-zA-Z]Sign(claims\|= Sign(" --include='*.go' . | grep -v "func Sign" | grep -v internal/cluster/dispatch
```

- [ ] **Step 4: Run the tests**

Run: `go test ./internal/auth/... ./internal/grpc/... ./e2e/parity/fixtureutil/...` then `go vet ./...`
Expected: PASS; vet clean. The grep above returns no call site with the old argument list.

- [ ] **Step 5: Commit**

```bash
git add -A internal/auth internal/e2e internal/grpc e2e/parity/fixtureutil
git commit -m "refactor(auth): sign JWTs through a Signer

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 2: The wrapped key vault

**Files:**
- Create: `internal/auth/key_vault.go`, `internal/auth/key_vault_internal_test.go`, `internal/auth/testdata/vault/bootstrap-pkcs8.pem`, `internal/auth/testdata/vault/golden.json`

**Interfaces:**
- Consumes: `Signer`, `NewRSASigner` (Task 1).
- Produces:
  - `type KeyMeta struct { KID, Audience, Algorithm, Owner string; SPKI []byte }`
  - `type KeyVault interface { Kind() string; Owner() string; Generate(ctx context.Context, meta KeyMeta) (spki, sealed []byte, s Signer, err error); Open(ctx context.Context, meta KeyMeta, sealed []byte) (Signer, error) }`
  - `func NewWrappedVault(bootstrap *rsa.PrivateKey, owner string) (KeyVault, error)`
  - `const WrappedVaultKind = "wrapped"`
  - `var ErrUnseal = errors.New("sealed key cannot be opened")`; `Open` wraps it with a reason class: `decryption`, `not RSA`, `public key mismatch`, `owner mismatch`.

- [ ] **Step 1: Create the fixture key** (a fixed test key; never a production key):

```bash
mkdir -p internal/auth/testdata/vault
openssl genpkey -algorithm RSA -pkeyopt rsa_keygen_bits:2048 -out internal/auth/testdata/vault/bootstrap-pkcs8.pem
```

- [ ] **Step 2: Write the failing tests** — `internal/auth/key_vault_internal_test.go` (package `auth`, because the derivation is unexported):

```go
package auth

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"flag"
	"math/big"
	"os"
	"testing"
)

var updateGolden = flag.Bool("update-vault-golden", false, "rewrite testdata/vault/golden.json")

func loadFixtureKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	raw, err := os.ReadFile("testdata/vault/bootstrap-pkcs8.pem")
	if err != nil {
		t.Fatal(err)
	}
	k, err := ParseRSAPrivateKeyFromPEM(raw)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

type vaultGolden struct {
	WrappingKeyHex string  `json:"wrappingKeyHex"`
	Meta           KeyMeta `json:"meta"`
	SealedHex      string  `json:"sealedHex"`
}

// The wrapping key and a sealed record are pinned: a change to the
// derivation or the sealing format would retire every stored key pair.
func TestWrappedVault_Golden(t *testing.T) {
	key := loadFixtureKey(t)
	wk, err := deriveWrappingKey(key)
	if err != nil {
		t.Fatal(err)
	}
	if *updateGolden {
		v, _ := NewWrappedVault(key, "owner-kid")
		meta := KeyMeta{KID: "golden-kid", Audience: "client", Algorithm: "RS256", Owner: "owner-kid"}
		spki, sealed, _, err := v.Generate(context.Background(), meta)
		if err != nil {
			t.Fatal(err)
		}
		meta.SPKI = spki
		g := vaultGolden{WrappingKeyHex: hex.EncodeToString(wk), Meta: meta, SealedHex: hex.EncodeToString(sealed)}
		b, _ := json.MarshalIndent(g, "", "  ")
		if err := os.WriteFile("testdata/vault/golden.json", b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	raw, err := os.ReadFile("testdata/vault/golden.json")
	if err != nil {
		t.Fatal(err)
	}
	var g vaultGolden
	if err := json.Unmarshal(raw, &g); err != nil {
		t.Fatal(err)
	}
	if hex.EncodeToString(wk) != g.WrappingKeyHex {
		t.Fatalf("wrapping key changed: got %x", wk)
	}
	v, _ := NewWrappedVault(key, "owner-kid")
	sealed, _ := hex.DecodeString(g.SealedHex)
	if _, err := v.Open(context.Background(), g.Meta, sealed); err != nil {
		t.Fatalf("golden sealed record no longer opens: %v", err)
	}
}

func TestWrappingKey_IndependentOfEncoding(t *testing.T) {
	key := loadFixtureKey(t)
	pkcs1 := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	again, err := ParseRSAPrivateKeyFromPEM(pkcs1)
	if err != nil {
		t.Fatal(err)
	}
	a, _ := deriveWrappingKey(key)
	b, _ := deriveWrappingKey(again)
	if !bytes.Equal(a, b) {
		t.Fatal("PKCS#1 and PKCS#8 encodings of one key derive different wrapping keys")
	}
}

func TestWrappingKey_MultiPrime(t *testing.T) {
	//nolint:staticcheck // multi-prime keys are deprecated but still parse
	key, err := rsa.GenerateMultiPrimeKey(rand.Reader, 3, 2048)
	if err != nil {
		t.Skipf("multi-prime generation unavailable: %v", err)
	}
	reordered := *key
	reordered.Primes = []*big.Int{key.Primes[2], key.Primes[0], key.Primes[1]}
	a, _ := deriveWrappingKey(key)
	b, _ := deriveWrappingKey(&reordered)
	if !bytes.Equal(a, b) {
		t.Fatal("prime order changes the wrapping key")
	}
}

func newTestVault(t *testing.T) (KeyVault, KeyMeta) {
	t.Helper()
	v, err := NewWrappedVault(loadFixtureKey(t), "owner-kid")
	if err != nil {
		t.Fatal(err)
	}
	return v, KeyMeta{KID: "k1", Audience: "client", Algorithm: "RS256", Owner: "owner-kid"}
}

func TestWrappedVault_RoundTrip(t *testing.T) {
	v, meta := newTestVault(t)
	spki, sealed, s, err := v.Generate(context.Background(), meta)
	if err != nil {
		t.Fatal(err)
	}
	meta.SPKI = spki
	opened, err := v.Open(context.Background(), meta, sealed)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if !opened.Public().(*rsa.PublicKey).Equal(s.Public()) {
		t.Fatal("opened key differs from generated key")
	}
}

// Every bound field must stop the sealed key opening when it changes.
func TestWrappedVault_AssociatedDataBindsEachField(t *testing.T) {
	v, meta := newTestVault(t)
	spki, sealed, _, err := v.Generate(context.Background(), meta)
	if err != nil {
		t.Fatal(err)
	}
	meta.SPKI = spki
	other, _ := rsa.GenerateKey(rand.Reader, 2048)
	otherSPKI, _ := x509.MarshalPKIXPublicKey(&other.PublicKey)
	cases := map[string]func(m *KeyMeta){
		"kid":       func(m *KeyMeta) { m.KID = "k2" },
		"audience":  func(m *KeyMeta) { m.Audience = "human" },
		"algorithm": func(m *KeyMeta) { m.Algorithm = "RS512" },
		"spki":      func(m *KeyMeta) { m.SPKI = otherSPKI },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			m := meta
			mutate(&m)
			if _, err := v.Open(context.Background(), m, sealed); !errors.Is(err, ErrUnseal) {
				t.Fatalf("Open with changed %s: err = %v, want ErrUnseal", name, err)
			}
		})
	}
}

func TestWrappedVault_RefusesOtherOwner(t *testing.T) {
	v, meta := newTestVault(t)
	spki, sealed, _, _ := v.Generate(context.Background(), meta)
	meta.SPKI, meta.Owner = spki, "another-owner"
	if _, err := v.Open(context.Background(), meta, sealed); !errors.Is(err, ErrUnseal) {
		t.Fatalf("err = %v, want ErrUnseal", err)
	}
}

func TestWrappedVault_OtherBootstrapKeyCannotOpen(t *testing.T) {
	v, meta := newTestVault(t)
	spki, sealed, _, _ := v.Generate(context.Background(), meta)
	meta.SPKI = spki
	otherKey, _ := rsa.GenerateKey(rand.Reader, 2048)
	w, _ := NewWrappedVault(otherKey, "owner-kid")
	if _, err := w.Open(context.Background(), meta, sealed); !errors.Is(err, ErrUnseal) {
		t.Fatalf("err = %v, want ErrUnseal", err)
	}
}
```

- [ ] **Step 3: Run and see them fail**

Run: `go test ./internal/auth/ -run 'TestWrapped|TestWrappingKey'`
Expected: build failure — `undefined: deriveWrappingKey`, `NewWrappedVault`, `KeyMeta`.

- [ ] **Step 4: Implement** — `internal/auth/key_vault.go`:

```go
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
	priv, ok := parsed.(*rsa.PrivateKey)
	if err != nil || !ok {
		return nil, fmt.Errorf("%w: not RSA", ErrUnseal)
	}
	spki, err := x509.MarshalPKIXPublicKey(&priv.PublicKey)
	if err != nil || !bytes.Equal(spki, meta.SPKI) {
		return nil, fmt.Errorf("%w: public key mismatch", ErrUnseal)
	}
	return NewRSASigner(priv), nil
}
```

- [ ] **Step 5: Write the golden file once, then pin it**

Run: `go test ./internal/auth/ -run TestWrappedVault_Golden -update-vault-golden` (writes `testdata/vault/golden.json`), then `go test ./internal/auth/ -run 'TestWrapped|TestWrappingKey'`
Expected: PASS without the flag. Check the golden file contains no private key: it holds only the wrapping-key hex (test-only), metadata and ciphertext.

- [ ] **Step 6: Commit**

```bash
git add internal/auth/key_vault.go internal/auth/key_vault_internal_test.go internal/auth/testdata/vault
git commit -m "feat(auth): wrapped key vault sealing private keys under the bootstrap key

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 3: The replicated KV component

A new, self-contained component. Nothing uses it until Task 4.

**Files:**
- Create: `internal/auth/replica.go`, `internal/auth/replica_internal_test.go`
- Modify: `internal/auth/reconcile_metrics.go:30-43` (metric-name prefix)

**Interfaces:**
- Consumes: `coalescingRunner` (`coalesce.go`), `ReconcileMetrics`, constants `maxReconcileAttempts`, `stalenessMultiplier`, `errorEscalationThreshold`, `errReconcileContention`, `defaultReconcileInterval`, `jitteredInterval` — all currently in `kv_trusted_store.go`; **move** them to `replica.go` in this task.
- Produces (package-internal):
  - `type kvWrite struct { key string; value []byte; prev []byte }` — `value == nil` deletes; `prev == nil` means the key was absent.
  - `type replicaConfig[R any] struct { name, namespace, topic string; decode func(kvKey string, data []byte) (copyKey string, rec R, ok bool, err error); interval time.Duration; broadcaster spi.ClusterBroadcaster; metrics ReconcileMetrics; afterChange func() }`
  - `func newKVReplica[R any](ctx context.Context, kv spi.KeyValueStore, cfg replicaConfig[R]) (*kvReplica[R], error)` — field `skippedAtLoad int`
  - methods: `read(fn func(map[string]R))`, `Reconcile(ctx) error`, `Start(ctx) bool`, `Stale() bool`, `mutate(fn func() (apply func(map[string]R), wrote bool, err error)) error`, `loadOne(ctx, kvKey string) (R, bool, error)`, `writeAll(ctx, []kvWrite) error`
  - `func NewOTelReconcileMetrics(meter metric.Meter, prefix string) (ReconcileMetrics, error)` — gauges `<prefix>.reconcile_consecutive_failures`, `<prefix>.reconcile_staleness_seconds`.

- [ ] **Step 1: Write the failing tests** — `internal/auth/replica_internal_test.go`:

```go
package auth

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/plugins/memory"
)

func replicaSystemCtx() context.Context {
	return spi.WithUserContext(context.Background(), &spi.UserContext{
		UserID: "system", Tenant: spi.Tenant{ID: spi.SystemTenantID, Name: "System"},
	})
}

func newReplicaKV(t *testing.T) spi.KeyValueStore {
	t.Helper()
	kv, err := memory.NewStoreFactory().KeyValueStore(replicaSystemCtx())
	if err != nil {
		t.Fatal(err)
	}
	return kv
}

// stringDecode keeps every value as its string; a value "bad" fails, "skip" is skipped.
func stringDecode(kvKey string, data []byte) (string, string, bool, error) {
	switch string(data) {
	case "bad":
		return "", "", false, errors.New("bad record")
	case "skip":
		return "", "", false, nil
	}
	return kvKey, string(data), true, nil
}

func newStringReplica(t *testing.T, kv spi.KeyValueStore, bc spi.ClusterBroadcaster) *kvReplica[string] {
	t.Helper()
	r, err := newKVReplica(replicaSystemCtx(), kv, replicaConfig[string]{
		name: "test", namespace: "ns", topic: "t", decode: stringDecode,
		interval: 50 * time.Millisecond, broadcaster: bc,
	})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func snapshot(r *kvReplica[string]) map[string]string {
	out := map[string]string{}
	r.read(func(m map[string]string) {
		for k, v := range m {
			out[k] = v
		}
	})
	return out
}

func TestReplica_LoadSkipsAndFailsStrict(t *testing.T) {
	ctx := replicaSystemCtx()
	kv := newReplicaKV(t)
	_ = kv.Put(ctx, "ns", "a", []byte("1"))
	_ = kv.Put(ctx, "ns", "s", []byte("skip"))
	r := newStringReplica(t, kv, nil)
	if got := snapshot(r); got["a"] != "1" || len(got) != 1 || r.skippedAtLoad != 1 {
		t.Fatalf("copy = %v skipped = %d", got, r.skippedAtLoad)
	}
	_ = kv.Put(ctx, "ns", "b", []byte("bad"))
	if _, err := newKVReplica(ctx, kv, replicaConfig[string]{name: "test", namespace: "ns", decode: stringDecode}); err == nil {
		t.Fatal("initial load must fail on an undecodable record")
	}
}

func TestReplica_ReconcileSwapsAndSkipsBadOnReRead(t *testing.T) {
	ctx := replicaSystemCtx()
	kv := newReplicaKV(t)
	r := newStringReplica(t, kv, nil)
	_ = kv.Put(ctx, "ns", "a", []byte("1"))
	_ = kv.Put(ctx, "ns", "b", []byte("bad"))
	if err := r.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if got := snapshot(r); got["a"] != "1" || len(got) != 1 {
		t.Fatalf("copy = %v", got)
	}
}

type blockingKV struct {
	spi.KeyValueStore
	putEntered chan struct{}
	release    chan struct{}
	failKey    string
}

func (b *blockingKV) Put(ctx context.Context, ns, key string, v []byte) error {
	if key == b.failKey {
		return fmt.Errorf("injected failure")
	}
	if b.putEntered != nil {
		b.putEntered <- struct{}{}
		<-b.release
	}
	return b.KeyValueStore.Put(ctx, ns, key, v)
}

// A slow store write under the admin mutex never blocks readers of the copy.
func TestReplica_MutateDoesNotBlockReaders(t *testing.T) {
	kv := &blockingKV{KeyValueStore: newReplicaKV(t), putEntered: make(chan struct{}), release: make(chan struct{})}
	r := newStringReplica(t, kv, nil)
	done := make(chan error)
	go func() {
		done <- r.mutate(func() (func(map[string]string), bool, error) {
			if err := r.writeAll(replicaSystemCtx(), []kvWrite{{key: "a", value: []byte("1")}}); err != nil {
				return nil, true, err
			}
			return func(m map[string]string) { m["a"] = "1" }, true, nil
		})
	}()
	<-kv.putEntered
	readDone := make(chan struct{})
	go func() { snapshot(r); close(readDone) }()
	select {
	case <-readDone:
	case <-time.After(time.Second):
		t.Fatal("a reader waited for a store write")
	}
	close(kv.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if snapshot(r)["a"] != "1" {
		t.Fatal("apply did not reach the copy")
	}
}

func TestReplica_WriteAllRestoresOnFailure(t *testing.T) {
	ctx := replicaSystemCtx()
	base := newReplicaKV(t)
	_ = base.Put(ctx, "ns", "old", []byte("before"))
	kv := &blockingKV{KeyValueStore: base, failKey: "third"}
	r := newStringReplica(t, kv, nil)
	err := r.writeAll(ctx, []kvWrite{
		{key: "new", value: []byte("n")},
		{key: "old", value: []byte("after"), prev: []byte("before")},
		{key: "third", value: []byte("x")},
	})
	if err == nil {
		t.Fatal("expected the injected failure")
	}
	if _, err := base.Get(ctx, "ns", "new"); !errors.Is(err, spi.ErrNotFound) {
		t.Fatalf("new key not removed: %v", err)
	}
	if v, _ := base.Get(ctx, "ns", "old"); string(v) != "before" {
		t.Fatalf("old key = %q, want restored", v)
	}
}

// A re-read that overlaps a local change is discarded and retried.
func TestReplica_ReconcileYieldsToLocalChange(t *testing.T) {
	ctx := replicaSystemCtx()
	kv := &listHookKV{KeyValueStore: newReplicaKV(t)}
	r := newStringReplica(t, kv, nil)
	var once sync.Once
	kv.onList = func() {
		once.Do(func() {
			_ = r.mutate(func() (func(map[string]string), bool, error) {
				_ = kv.KeyValueStore.Put(ctx, "ns", "local", []byte("v"))
				return func(m map[string]string) { m["local"] = "v" }, false, nil
			})
		})
	}
	if err := r.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if snapshot(r)["local"] != "v" {
		t.Fatal("re-read overwrote a local change")
	}
}

type listHookKV struct {
	spi.KeyValueStore
	onList func()
	gets   atomic.Int32
	onGet  func()
}

func (h *listHookKV) List(ctx context.Context, ns string) (map[string][]byte, error) {
	if h.onList != nil {
		h.onList()
	}
	return h.KeyValueStore.List(ctx, ns)
}

func (h *listHookKV) Get(ctx context.Context, ns, key string) ([]byte, error) {
	v, err := h.KeyValueStore.Get(ctx, ns, key)
	if h.gets.Add(1) == 1 && h.onGet != nil {
		h.onGet()
	}
	return v, err
}

// A single-record load that raced a delete must not put the record back.
func TestReplica_LoadOneIsGenerationGuarded(t *testing.T) {
	ctx := replicaSystemCtx()
	base := newReplicaKV(t)
	_ = base.Put(ctx, "ns", "k", []byte("v"))
	kv := &listHookKV{KeyValueStore: base}
	r := newStringReplica(t, kv, nil)
	r.read(func(m map[string]string) {}) // loaded
	_ = r.mutate(func() (func(map[string]string), bool, error) {
		return func(m map[string]string) { delete(m, "k") }, false, nil
	})
	kv.onGet = func() {
		_ = r.mutate(func() (func(map[string]string), bool, error) {
			_ = base.Delete(ctx, "ns", "k")
			return func(m map[string]string) { delete(m, "k") }, true, nil
		})
	}
	_, found, err := r.loadOne(ctx, "k")
	if err != nil {
		t.Fatal(err)
	}
	if found || snapshot(r)["k"] != "" {
		t.Fatal("a stale single-record read overwrote a newer delete")
	}
}

func TestReplica_PingTriggersReRead(t *testing.T) {
	ctx := replicaSystemCtx()
	kv := newReplicaKV(t)
	bc := newInternalBroadcaster()
	a := newStringReplica(t, kv, bc)
	b := newStringReplica(t, kv, bc)
	_ = a.mutate(func() (func(map[string]string), bool, error) {
		_ = kv.Put(ctx, "ns", "x", []byte("1"))
		return func(m map[string]string) { m["x"] = "1" }, true, nil
	})
	deadline := time.Now().Add(2 * time.Second)
	for snapshot(b)["x"] != "1" {
		if time.Now().After(deadline) {
			t.Fatal("peer never re-read after the change message")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestReplica_StaleOnlyAfterLoopStartsAndBound(t *testing.T) {
	kv := &failingListKV{KeyValueStore: newReplicaKV(t)}
	r := newStringReplica(t, kv, nil)
	if r.Stale() {
		t.Fatal("stale before the loop started")
	}
	kv.fail.Store(true)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r.Start(ctx)
	deadline := time.Now().Add(3 * time.Second)
	for !r.Stale() {
		if time.Now().After(deadline) {
			t.Fatal("never became stale with failing re-reads")
		}
		time.Sleep(20 * time.Millisecond)
	}
	kv.fail.Store(false)
	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if r.Stale() {
		t.Fatal("still stale after a successful re-read")
	}
}

type failingListKV struct {
	spi.KeyValueStore
	fail atomic.Bool
}

func (f *failingListKV) List(ctx context.Context, ns string) (map[string][]byte, error) {
	if f.fail.Load() {
		return nil, errors.New("list down")
	}
	return f.KeyValueStore.List(ctx, ns)
}

type internalBroadcaster struct {
	mu sync.Mutex
	hs map[string][]func([]byte)
}

func newInternalBroadcaster() *internalBroadcaster {
	return &internalBroadcaster{hs: map[string][]func([]byte){}}
}

func (b *internalBroadcaster) Broadcast(topic string, p []byte) {
	b.mu.Lock()
	hs := append([]func([]byte){}, b.hs[topic]...)
	b.mu.Unlock()
	for _, h := range hs {
		h(p)
	}
}

func (b *internalBroadcaster) Subscribe(topic string, h func([]byte)) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.hs[topic] = append(b.hs[topic], h)
}

```

- [ ] **Step 2: Run and see them fail**

Run: `go test ./internal/auth/ -run TestReplica`
Expected: build failure — `undefined: newKVReplica`, `kvReplica`, `kvWrite`, `replicaConfig`.

- [ ] **Step 3: Implement** — `internal/auth/replica.go`. Move `defaultReconcileInterval`, `maxReconcileAttempts`, `stalenessMultiplier`, `errorEscalationThreshold`, `errReconcileContention` and `jitteredInterval` here from `kv_trusted_store.go` (delete them there). `errReconcileContention`'s message becomes `"re-read gave up after repeated mid-rebuild mutations"`.

```go
package auth

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

const defaultReconcileInterval = 60 * time.Second

// maxReconcileAttempts bounds the generation-guard retry of a re-read and of a
// single-record load. Only continuous local change can exhaust it.
const maxReconcileAttempts = 5

// stalenessMultiplier × interval is the fail-closed bound: a copy with no
// successful re-read for that long is stale.
const stalenessMultiplier = 10

// errorEscalationThreshold is the consecutive-failure count from which re-read
// failures log at ERROR instead of WARN.
const errorEscalationThreshold = 3

// errReconcileContention marks a re-read that gave up because local changes
// kept landing mid-rebuild. It is not a store failure.
var errReconcileContention = errors.New("re-read gave up after repeated mid-rebuild mutations")

// kvWrite is one step of a multi-record write. value nil deletes the key. prev
// is the key's bytes before the write, nil when it was absent; a failed
// multi-record write restores it.
type kvWrite struct {
	key   string
	value []byte
	prev  []byte
}

type replicaConfig[R any] struct {
	name      string // log prefix, e.g. "trusted-key"
	namespace string
	topic     string
	// decode turns one entry into the copy's key and record. ok=false skips
	// the entry (counted at load); err skips it with an ERROR on a re-read and
	// fails the initial load.
	decode      func(kvKey string, data []byte) (copyKey string, rec R, ok bool, err error)
	interval    time.Duration
	broadcaster spi.ClusterBroadcaster
	metrics     ReconcileMetrics
	// afterChange runs after every swap or apply, with no lock held.
	afterChange func()
}

// kvReplica keeps a node copy of one KV namespace. A change message from a
// peer and the periodic re-read rebuild the copy; a local admin change updates
// it directly. Hot paths read the copy only. Admin changes run one at a time
// under the admin mutex and never hold the copy lock across a store call.
type kvReplica[R any] struct {
	cfg replicaConfig[R]
	kv  spi.KeyValueStore
	ctx context.Context // process lifetime: no cancellation, no deadline

	mu   sync.RWMutex // the copy lock
	recs map[string]R
	gen  atomic.Uint64 // bumped by every swap and apply

	adminMu     sync.Mutex
	reconcileMu sync.Mutex
	ping        coalescingRunner
	loopStarted atomic.Bool

	epoch    time.Time    // monotonic reference for staleness
	lastOK   atomic.Int64 // time.Since(epoch) at the last successful re-read
	failures atomic.Int64

	skippedAtLoad int
}

func newKVReplica[R any](ctx context.Context, kv spi.KeyValueStore, cfg replicaConfig[R]) (*kvReplica[R], error) {
	if cfg.interval <= 0 {
		cfg.interval = defaultReconcileInterval
	}
	if cfg.metrics == nil {
		cfg.metrics = NopReconcileMetrics{}
	}
	r := &kvReplica[R]{cfg: cfg, kv: kv, ctx: context.WithoutCancel(ctx), epoch: time.Now()}
	entries, err := kv.List(r.ctx, cfg.namespace)
	if err != nil {
		return nil, fmt.Errorf("failed to load %s records: %w", cfg.name, err)
	}
	recs, skipped, err := r.build(entries, true)
	if err != nil {
		return nil, err
	}
	r.recs, r.skippedAtLoad = recs, skipped
	r.stampOK()
	if cfg.broadcaster != nil {
		cfg.broadcaster.Subscribe(cfg.topic, r.handlePing)
	}
	return r, nil
}

func (r *kvReplica[R]) build(entries map[string][]byte, strict bool) (map[string]R, int, error) {
	recs := make(map[string]R, len(entries))
	skipped := 0
	for kvKey, data := range entries {
		k, rec, ok, err := r.cfg.decode(kvKey, data)
		if err != nil {
			if strict {
				return nil, 0, fmt.Errorf("failed to decode %s record %q: %w", r.cfg.name, kvKey, err)
			}
			slog.Error(r.cfg.name+" reconcile: skipping undeserializable record",
				"pkg", "auth", "kvKey", kvKey, "error", err.Error())
			continue
		}
		if !ok {
			skipped++
			continue
		}
		recs[k] = rec
	}
	return recs, skipped, nil
}

// read runs fn with the copy under the read lock. fn must not keep the map.
func (r *kvReplica[R]) read(fn func(recs map[string]R)) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	fn(r.recs)
}

func (r *kvReplica[R]) stampOK() { r.lastOK.Store(int64(time.Since(r.epoch))) }

func (r *kvReplica[R]) age() time.Duration {
	return time.Since(r.epoch) - time.Duration(r.lastOK.Load())
}

// Stale reports whether the copy has had no successful re-read for the
// fail-closed bound. Always false until the loop has started.
func (r *kvReplica[R]) Stale() bool {
	return r.loopStarted.Load() && r.age() > stalenessMultiplier*r.cfg.interval
}

// Reconcile rebuilds the copy from the store. A rebuild that overlapped a
// local change is discarded and retried; a failed List keeps the old copy.
func (r *kvReplica[R]) Reconcile(ctx context.Context) error {
	r.reconcileMu.Lock()
	defer r.reconcileMu.Unlock()
	for attempt := 0; attempt < maxReconcileAttempts; attempt++ {
		gen := r.gen.Load()
		entries, err := r.kv.List(ctx, r.cfg.namespace)
		if err != nil {
			n := r.failures.Add(1)
			r.cfg.metrics.SetReconcileConsecutiveFailures(int(n))
			r.cfg.metrics.SetReconcileStalenessSeconds(r.age().Seconds())
			msg := r.cfg.name + " reconcile failed; serving last-known state until it succeeds"
			if n > errorEscalationThreshold {
				slog.Error(msg, "pkg", "auth", "consecutiveFailures", n, "error", err.Error())
			} else {
				slog.Warn(msg, "pkg", "auth", "consecutiveFailures", n, "error", err.Error())
			}
			return fmt.Errorf("failed to list %s records: %w", r.cfg.name, err)
		}
		fresh, _, _ := r.build(entries, false)
		swapped := func() bool {
			r.mu.Lock()
			defer r.mu.Unlock()
			if r.gen.Load() != gen {
				return false
			}
			r.recs = fresh
			r.gen.Add(1)
			return true
		}()
		if swapped {
			r.stampOK()
			r.failures.Store(0)
			r.cfg.metrics.SetReconcileConsecutiveFailures(0)
			r.cfg.metrics.SetReconcileStalenessSeconds(0)
			if r.cfg.afterChange != nil {
				r.cfg.afterChange()
			}
			return nil
		}
	}
	stale := r.age().Seconds()
	r.cfg.metrics.SetReconcileStalenessSeconds(stale)
	msg := r.cfg.name + " reconcile: giving up after repeated mid-rebuild mutations"
	fields := []any{"pkg", "auth", "attempts", maxReconcileAttempts, "stalenessSeconds", int64(stale + 0.5)}
	if r.age() > (stalenessMultiplier/2)*r.cfg.interval {
		slog.Error(msg, fields...)
	} else {
		slog.Warn(msg, fields...)
	}
	return errReconcileContention
}

// handlePing runs on the broadcaster's receive goroutine: it must not block
// and must not panic. The payload is never read or logged.
func (r *kvReplica[R]) handlePing(_ []byte) {
	defer func() {
		if rec := recover(); rec != nil {
			slog.Error(r.cfg.name+" ping handler panic", "pkg", "auth", "panic", rec)
		}
	}()
	r.ping.Trigger(r.reconcileOnce)
}

// reconcileOnce is one bounded re-read on its own deadline, recover-wrapped:
// it runs detached from any caller that could handle a panic.
func (r *kvReplica[R]) reconcileOnce() {
	defer func() {
		if rec := recover(); rec != nil {
			slog.Error(r.cfg.name+" reconcile panic", "pkg", "auth", "panic", rec)
		}
	}()
	ctx, cancel := context.WithTimeout(r.ctx, r.cfg.interval)
	defer cancel()
	_ = r.Reconcile(ctx)
}

// Start runs the periodic re-read until ctx ends. Returns false if it already runs.
func (r *kvReplica[R]) Start(ctx context.Context) bool {
	if !r.loopStarted.CompareAndSwap(false, true) {
		return false
	}
	go func() {
		for {
			timer := time.NewTimer(jitteredInterval(r.cfg.interval))
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
				r.reconcileOnce()
			}
		}
	}()
	return true
}

// jitteredInterval returns d × [0.9, 1.1).
func jitteredInterval(d time.Duration) time.Duration {
	return time.Duration(float64(d) * (0.9 + 0.2*rand.Float64()))
}

func (r *kvReplica[R]) broadcast() {
	if r.cfg.broadcaster != nil {
		r.cfg.broadcaster.Broadcast(r.cfg.topic, nil)
	}
}

// mutate runs one admin change on this node. fn runs under the admin mutex
// with no copy lock held: it reads and writes the store with the caller's
// context and returns the change to the copy (nil for none) and whether it
// wrote the store. The copy lock is held only while apply runs. A change
// message goes out whenever the store was written, even if fn then failed,
// so peers re-read what is actually stored.
func (r *kvReplica[R]) mutate(fn func() (apply func(recs map[string]R), wrote bool, err error)) error {
	r.adminMu.Lock()
	defer r.adminMu.Unlock()
	apply, wrote, err := fn()
	if apply != nil {
		func() {
			r.mu.Lock()
			defer r.mu.Unlock()
			apply(r.recs)
			r.gen.Add(1)
		}()
		if r.cfg.afterChange != nil {
			r.cfg.afterChange()
		}
	}
	if wrote {
		r.broadcast()
	}
	return err
}

// loadOne reads one record through to the store and puts it in the copy,
// unless a change committed while it read — then it reads again, so an older
// read never overwrites a newer copy. found=false: absent or skipped.
func (r *kvReplica[R]) loadOne(ctx context.Context, kvKey string) (R, bool, error) {
	var zero R
	for attempt := 0; attempt < maxReconcileAttempts; attempt++ {
		gen := r.gen.Load()
		data, err := r.kv.Get(ctx, r.cfg.namespace, kvKey)
		if errors.Is(err, spi.ErrNotFound) {
			return zero, false, nil
		}
		if err != nil {
			return zero, false, fmt.Errorf("failed to read %s record: %w", r.cfg.name, err)
		}
		k, rec, ok, err := r.cfg.decode(kvKey, data)
		if err != nil {
			return zero, false, fmt.Errorf("failed to decode %s record: %w", r.cfg.name, err)
		}
		if !ok {
			return zero, false, nil
		}
		stored := func() bool {
			r.mu.Lock()
			defer r.mu.Unlock()
			if r.gen.Load() != gen {
				return false
			}
			r.recs[k] = rec
			r.gen.Add(1)
			return true
		}()
		if stored {
			return rec, true, nil
		}
	}
	return zero, false, errReconcileContention
}

// writeAll applies writes in order. If one fails it restores every write
// already applied, newest first, and returns the error. Restores run on a
// context the caller cannot cancel; a restore that fails is logged at ERROR
// with every key left changed.
func (r *kvReplica[R]) writeAll(ctx context.Context, writes []kvWrite) error {
	for i, w := range writes {
		if err := r.put(ctx, w.key, w.value); err != nil {
			rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), r.cfg.interval)
			defer cancel()
			var stuck []string
			for j := i - 1; j >= 0; j-- {
				if rerr := r.put(rctx, writes[j].key, writes[j].prev); rerr != nil {
					stuck = append(stuck, writes[j].key)
				}
			}
			if len(stuck) > 0 {
				slog.Error(r.cfg.name+" multi-record write failed and could not be fully undone",
					"pkg", "auth", "keysLeftChanged", stuck)
			}
			return fmt.Errorf("failed to write %s record %q: %w", r.cfg.name, w.key, err)
		}
	}
	return nil
}

func (r *kvReplica[R]) put(ctx context.Context, key string, value []byte) error {
	if value == nil {
		if err := r.kv.Delete(ctx, r.cfg.namespace, key); err != nil && !errors.Is(err, spi.ErrNotFound) {
			return err
		}
		return nil
	}
	return r.kv.Put(ctx, r.cfg.namespace, key, value)
}
```

In `reconcile_metrics.go` change `NewOTelReconcileMetrics(meter metric.Meter)` to `NewOTelReconcileMetrics(meter metric.Meter, prefix string)` with gauge names `prefix + ".reconcile_consecutive_failures"` and `prefix + ".reconcile_staleness_seconds"`, and update its one caller at `app/app.go:286` to pass `"auth.trustedkeys"`.

`kv_trusted_store.go` keeps compiling unchanged apart from the moved constants (it still has its own copies of the reconcile methods until Task 4; rename nothing there yet).

- [ ] **Step 4: Run the tests**

Run: `go test ./internal/auth/...` and `go vet ./internal/auth/ ./app/`
Expected: PASS, including every existing trusted-key test.

- [ ] **Step 5: Commit**

```bash
git add internal/auth/replica.go internal/auth/replica_internal_test.go internal/auth/reconcile_metrics.go internal/auth/kv_trusted_store.go app/app.go
git commit -m "feat(auth): replicated KV component — node copy, re-read, admin write path

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 4: The trusted-key store on the component; admin writes read the store

**Files:**
- Modify: `internal/auth/kv_trusted_store.go` (rewrite onto `kvReplica`), `internal/auth/store.go:95-107` (`TrustedKeyStore` gains `ctx` on admin methods) and `InMemoryTrustedKeyStore` (same signatures; ctx unused — the type is deleted in Task 8)
- Modify: `internal/domain/account/trusted_adapter.go` (pass `r.Context()`; Register's partial-success comment goes), every test calling these methods (`internal/auth/kv_trusted_store_test.go`, `kv_trusted_store_reconcile_test.go`, `internal/domain/account/trusted_adapter_test.go`, `trusted_key_outage_test.go`, `internal/auth/token_test.go`)
- Test: `internal/auth/kv_trusted_store_admin_test.go` (new)

**Interfaces:**
- Consumes: `kvReplica`, `kvWrite`, `replicaConfig` (Task 3).
- Produces:

```go
type TrustedKeyStore interface {
	Register(ctx context.Context, tk *TrustedKey, opts RotateOptions) error
	Get(ctx context.Context, tenantID spi.TenantID, kid string) (*TrustedKey, error)
	List(tenantID spi.TenantID) []*TrustedKey
	GetForVerification(tenantID spi.TenantID, kid string) (*TrustedKey, error)
	Delete(ctx context.Context, tenantID spi.TenantID, kid string) error
	Invalidate(ctx context.Context, tenantID spi.TenantID, kid string, gracePeriodSec int64) error
	Reactivate(ctx context.Context, tenantID spi.TenantID, kid string, validFrom, validTo time.Time) error
}
```

`Get` returns an error wrapping `ErrTrustedKeyNotFound` for absence (and cross-tenant); any other error is a store failure. `KVTrustedKeyStore` keeps `Reconcile(ctx)` and `StartReconcileLoop(ctx) bool` (delegating to the replica).

- [ ] **Step 1: Write the failing tests** — `internal/auth/kv_trusted_store_admin_test.go`:

```go
package auth_test

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"errors"
	"fmt"
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/auth"
)

func newTrustedKey(t *testing.T, tenant spi.TenantID, kid string, from time.Time) *auth.TrustedKey {
	t.Helper()
	priv, _ := rsa.GenerateKey(rand.Reader, 2048)
	return &auth.TrustedKey{KID: kid, TenantID: tenant, PublicKey: &priv.PublicKey,
		JWK: map[string]any{"kty": "RSA", "kid": kid}, Audience: "human", Active: true, ValidFrom: from}
}

// Node B has not seen A's delete; its invalidate must not write the key back.
func TestKVTrustedKeyStore_StaleCopyCannotResurrectDeleted(t *testing.T) {
	ctx := systemCtx()
	kv := mustNewMemoryKV(t, ctx)
	a, _ := auth.NewKVTrustedKeyStore(ctx, kv)
	b, _ := auth.NewKVTrustedKeyStore(ctx, kv)
	tID := spi.TenantID("t")
	if err := a.Register(ctx, newTrustedKey(t, tID, "k", time.Now()), auth.RotateOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := b.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if err := a.Delete(ctx, tID, "k"); err != nil {
		t.Fatal(err)
	}
	err := b.Invalidate(ctx, tID, "k", 0)
	if !errors.Is(err, auth.ErrTrustedKeyNotFound) {
		t.Fatalf("invalidate on the stale node: err = %v, want ErrTrustedKeyNotFound", err)
	}
	if _, err := kv.Get(ctx, "trusted-keys", auth.TrustedKeyKVKeyForTesting(tID, "k")); !errors.Is(err, spi.ErrNotFound) {
		t.Fatalf("deleted key is back in the store: %v", err)
	}
}

// A rotation on B must invalidate a sibling A registered that B has not seen.
func TestKVTrustedKeyStore_RotationSeesSiblingRegisteredElsewhere(t *testing.T) {
	ctx := systemCtx()
	kv := mustNewMemoryKV(t, ctx)
	a, _ := auth.NewKVTrustedKeyStore(ctx, kv)
	b, _ := auth.NewKVTrustedKeyStore(ctx, kv)
	tID := spi.TenantID("t")
	_ = a.Register(ctx, newTrustedKey(t, tID, "k1", time.Now()), auth.RotateOptions{})
	if err := b.Register(ctx, newTrustedKey(t, tID, "k2", time.Now().Add(time.Second)), auth.RotateOptions{Invalidate: true}); err != nil {
		t.Fatal(err)
	}
	if err := a.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	k1, err := a.Get(ctx, tID, "k1")
	if err != nil {
		t.Fatal(err)
	}
	if k1.Active {
		t.Fatal("sibling registered on another node stayed active after rotation")
	}
}

type slowPutKV struct {
	spi.KeyValueStore
	entered chan struct{}
	release chan struct{}
}

func (s *slowPutKV) Put(ctx context.Context, ns, key string, v []byte) error {
	s.entered <- struct{}{}
	<-s.release
	return s.KeyValueStore.Put(ctx, ns, key, v)
}

// A slow admin write never delays verification.
func TestKVTrustedKeyStore_VerificationNotBlockedBySlowWrite(t *testing.T) {
	ctx := systemCtx()
	mem := mustNewMemoryKV(t, ctx)
	tID := spi.TenantID("t")
	pre, _ := auth.NewKVTrustedKeyStore(ctx, mem)
	_ = pre.Register(ctx, newTrustedKey(t, tID, "live", time.Now()), auth.RotateOptions{})
	kv := &slowPutKV{KeyValueStore: mem, entered: make(chan struct{}), release: make(chan struct{})}
	s, _ := auth.NewKVTrustedKeyStore(ctx, kv)
	go func() { _ = s.Register(ctx, newTrustedKey(t, tID, "new", time.Now()), auth.RotateOptions{}) }()
	<-kv.entered
	done := make(chan error)
	go func() { _, err := s.GetForVerification(tID, "live"); done <- err }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("verification waited for an admin store write")
	}
	close(kv.release)
}

// A failed sibling write undoes the whole rotation.
func TestKVTrustedKeyStore_RotationCompensatesOnSiblingFailure(t *testing.T) {
	ctx := systemCtx()
	mem := mustNewMemoryKV(t, ctx)
	tID := spi.TenantID("t")
	pre, _ := auth.NewKVTrustedKeyStore(ctx, mem)
	_ = pre.Register(ctx, newTrustedKey(t, tID, "a", time.Now()), auth.RotateOptions{})
	kv := &failingKV{KeyValueStore: mem, failOn: auth.TrustedKeyKVKeyForTesting(tID, "a")}
	s, _ := auth.NewKVTrustedKeyStore(ctx, kv)
	err := s.Register(ctx, newTrustedKey(t, tID, "b", time.Now().Add(time.Second)), auth.RotateOptions{Invalidate: true, GracePeriodSec: 60})
	if err == nil {
		t.Fatal("expected the sibling failure")
	}
	if _, err := mem.Get(ctx, "trusted-keys", auth.TrustedKeyKVKeyForTesting(tID, "b")); !errors.Is(err, spi.ErrNotFound) {
		t.Fatalf("new key left in the store after a failed rotation: %v", err)
	}
	if _, err := s.Get(ctx, tID, "b"); !errors.Is(err, auth.ErrTrustedKeyNotFound) {
		t.Fatalf("new key left in the copy: %v", err)
	}
	a, _ := s.Get(ctx, tID, "a")
	if a == nil || !a.Active {
		t.Fatal("sibling changed by a failed rotation")
	}
}

type getFailKV struct{ spi.KeyValueStore }

func (g getFailKV) Get(context.Context, string, string) ([]byte, error) {
	return nil, fmt.Errorf("store down")
}

// A store failure is not "not found".
func TestKVTrustedKeyStore_GetStoreFailureIsNotNotFound(t *testing.T) {
	ctx := systemCtx()
	s, _ := auth.NewKVTrustedKeyStore(ctx, getFailKV{mustNewMemoryKV(t, ctx)})
	_, err := s.Get(ctx, "t", "missing")
	if err == nil || errors.Is(err, auth.ErrTrustedKeyNotFound) {
		t.Fatalf("err = %v, want a store error", err)
	}
}
```

Rewrite the existing `TestKVTrustedKeyStore_PartialKVFailureKeepsNewKey` (`kv_trusted_store_test.go:620-660`): its old assertion — the new key stays after a sibling failure — contradicts the new rule. Delete it; `TestKVTrustedKeyStore_RotationCompensatesOnSiblingFailure` replaces it.

- [ ] **Step 2: Run and see them fail**

Run: `go test ./internal/auth/ -run 'TestKVTrustedKeyStore_(StaleCopy|RotationSees|VerificationNotBlocked|RotationCompensates|GetStoreFailure)'`
Expected: build failure on the `ctx` arguments; after adding the signatures only, the behaviour assertions fail (the invalidate resurrects, the rotation misses `k1`, verification blocks, the new key stays).

- [ ] **Step 3: Implement** — rewrite `kv_trusted_store.go`. Keep unchanged: the option types and functions, `trustedKeyKey`, `trustedKeyRecord`, `serializeTrustedKey`, `deserializeTrustedKey`, `encodeBase64URL`, `copyTrustedKey`, `TrustedKeyKVKeyForTesting`, `defaultMaxTrustedKeys`, the namespace and topic constants. Delete: `buildTrustedKeyMap`, the store's own `Reconcile`/`handlePing`/`reconcileOnce`/`reconcileAge`/`reconcileStale`/`broadcastChanged`/`loadOne`/`persistWithKey` and its mutex, generation, coalescer and staleness fields. The new store:

```go
type KVTrustedKeyStore struct {
	rep          *kvReplica[*TrustedKey]
	kv           spi.KeyValueStore
	maxPerTenant int
}

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

func (s *KVTrustedKeyStore) Reconcile(ctx context.Context) error    { return s.rep.Reconcile(ctx) }
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

func (s *KVTrustedKeyStore) GetForVerification(tenantID spi.TenantID, kid string) (*TrustedKey, error) {
	if s.rep.Stale() {
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

func (s *KVTrustedKeyStore) Invalidate(ctx context.Context, tenantID spi.TenantID, kid string, gracePeriodSec int64) error {
	return s.update(ctx, tenantID, kid, func(tk *TrustedKey, _ map[string]*TrustedKey) error {
		tk.Active = false
		tk.ValidTo = graceExpiry(tk.ValidTo, time.Now(), gracePeriodSec)
		return nil
	}, false)
}

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

// update changes one stored key. needAll lists the namespace first (the cap).
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
```

`InMemoryTrustedKeyStore` in `store.go`: add a leading `_ context.Context` parameter to `Register`, `Get`, `Delete`, `Invalidate`, `Reactivate` so it still satisfies the interface; make its `Get` wrap `ErrTrustedKeyNotFound`.

`trusted_adapter.go`: every store call passes `r.Context()`. In `RegisterTrustedKey` replace the "partial success" comment and branch with:

```go
	if err := h.trustedKeyStore.Register(r.Context(), tk, auth.RotateOptions{Invalidate: invalidate, GracePeriodSec: grace}); err != nil {
		common.WriteError(w, r, trustedKeyMutationError(err))
		return
	}
```

and route the trusted-key `Get` adapter's error through `trustedKeyMutationError` as well, so a store failure is 5xx, not 404.

Update the remaining callers found by `go vet ./...` (tests) to pass `ctx` (`systemCtx()` in `internal/auth` tests, `context.Background()` or the request context elsewhere).

- [ ] **Step 4: Run the tests**

Run: `go test ./internal/auth/... ./internal/domain/account/...` then `go vet ./...`
Expected: PASS — the new tests and every existing trusted-key test (reconcile, staleness, legacy-entry WARN with `count=2`, cap, cross-tenant 409).

- [ ] **Step 5: Commit**

```bash
git add -A internal/auth internal/domain/account
git commit -m "fix(auth): trusted-key admin writes read the store and never block verification

A node that had not yet seen a delete could write the key back, a rotation
could miss a key registered on another node, a failed rotation stayed half
applied, and an admin write held the lock verification reads under across
the store call.

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 5: Signing-key records and classification

**Files:**
- Create: `internal/auth/signing_records.go`, `internal/auth/signing_records_internal_test.go`
- Modify: `internal/auth/store.go:21-31` (`KeyPair` gains `Bootstrap bool`; additive)

**Interfaces:**
- Consumes: `KeyVault`, `KeyMeta`, `ErrUnseal`, `NewWrappedVault` (Task 2).
- Produces:
  - constants `signingKeysNamespace = "signing-keys"`, `topicSigningKeys = "auth.signingkeys"`, `recordKindIssued = "issued"`, `recordKindBootstrap = "bootstrap"`
  - `type signingRecord struct` (JSON, below), `type vaultReference struct`
  - `type keyClass int` with `classOwned, classBroken, classRetired, classBootstrapState, classForeignBootstrap, classUndecodable`
  - `type signingEntry struct { class keyClass; reason string; pair KeyPair; deleted bool; signer Signer }`
  - `func decodeSigningRecord(kvKey string, data []byte) (signingRecord, KeyPair, []byte /*spki*/, []byte /*sealed*/, error)`
  - `func encodeSigningRecord(rec signingRecord) ([]byte, error)`
  - `func defaultBootstrapRecord(kid string) signingRecord`
  - `type classifier struct { vault KeyVault; bootKID string; signers *signerCache }` with `func newClassifier(vault KeyVault, bootKID string) *classifier` and `func (c *classifier) classify(ctx context.Context, kvKey string, data []byte) *signingEntry`
  - `func fmtTime(t time.Time) string`, `func fmtTimePtr(t *time.Time) *string`

- [ ] **Step 1: Write the failing tests** — `internal/auth/signing_records_internal_test.go`:

```go
package auth

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type countingVault struct {
	KeyVault
	opens atomic.Int32
}

func (c *countingVault) Open(ctx context.Context, m KeyMeta, sealed []byte) (Signer, error) {
	c.opens.Add(1)
	return c.KeyVault.Open(ctx, m, sealed)
}

func issuedRecord(t *testing.T, v KeyVault, kid, aud string) []byte {
	t.Helper()
	meta := KeyMeta{KID: kid, Audience: aud, Algorithm: "RS256", Owner: v.Owner()}
	spki, sealed, _, err := v.Generate(context.Background(), meta)
	if err != nil {
		t.Fatal(err)
	}
	vt := time.Now().Add(time.Hour)
	b, err := encodeSigningRecord(signingRecord{
		Kind: recordKindIssued, KID: kid, Audience: aud, Algorithm: "RS256", Active: true,
		ValidFrom: fmtTime(time.Now()), ValidTo: fmtTimePtr(&vt),
		PublicKey: base64.StdEncoding.EncodeToString(spki),
		Vault:     &vaultReference{Kind: v.Kind(), Owner: v.Owner(), Sealed: base64.StdEncoding.EncodeToString(sealed)},
	})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func testClassifier(t *testing.T) (*classifier, *countingVault) {
	t.Helper()
	v, _ := NewWrappedVault(loadFixtureKey(t), "boot-kid")
	cv := &countingVault{KeyVault: v}
	return newClassifier(cv, "boot-kid"), cv
}

func TestClassify_Owned(t *testing.T) {
	c, v := testClassifier(t)
	e := c.classify(context.Background(), "k1", issuedRecord(t, v, "k1", "client"))
	if e.class != classOwned || e.signer == nil || e.pair.PublicKey == nil || e.pair.Audience != "client" {
		t.Fatalf("entry = %+v", e)
	}
}

func TestClassify_SignerOpenedOnceWhileSealedUnchanged(t *testing.T) {
	c, v := testClassifier(t)
	rec := issuedRecord(t, v, "k1", "client")
	c.classify(context.Background(), "k1", rec)
	c.classify(context.Background(), "k1", rec)
	if n := v.opens.Load(); n != 1 {
		t.Fatalf("opened %d times, want 1", n)
	}
}

func TestClassify_Retired(t *testing.T) {
	c, _ := testClassifier(t)
	other, _ := rsa.GenerateKey(rand.Reader, 2048)
	ov, _ := NewWrappedVault(other, "other-boot")
	if e := c.classify(context.Background(), "k1", issuedRecord(t, ov, "k1", "client")); e.class != classRetired {
		t.Fatalf("class = %v, want retired", e.class)
	}
}

func TestClassify_BrokenReasons(t *testing.T) {
	c, v := testClassifier(t)
	good := issuedRecord(t, v, "k1", "client")
	mutate := func(fn func(r *signingRecord)) []byte {
		var r signingRecord
		_ = json.Unmarshal(good, &r)
		fn(&r)
		b, _ := encodeSigningRecord(r)
		return b
	}
	cases := map[string][]byte{
		"decryption": mutate(func(r *signingRecord) {
			s, _ := base64.StdEncoding.DecodeString(r.Vault.Sealed)
			s[len(s)-1] ^= 1
			r.Vault.Sealed = base64.StdEncoding.EncodeToString(s)
		}),
		"unknown vault kind": mutate(func(r *signingRecord) { r.Vault.Kind = "kms-x" }),
	}
	for want, rec := range cases {
		e := c.classify(context.Background(), "k1", rec)
		if e.class != classBroken || !strings.Contains(e.reason, want) {
			t.Fatalf("%s: entry = %+v", want, e)
		}
	}
}

func TestClassify_BootstrapAndForeign(t *testing.T) {
	c, _ := testClassifier(t)
	own, _ := encodeSigningRecord(signingRecord{Kind: recordKindBootstrap, KID: "boot-kid", Active: false, ValidFrom: fmtTime(time.Time{}), Deleted: true})
	if e := c.classify(context.Background(), "boot-kid", own); e.class != classBootstrapState || !e.deleted {
		t.Fatalf("own bootstrap = %+v", e)
	}
	other, _ := encodeSigningRecord(signingRecord{Kind: recordKindBootstrap, KID: "old-boot", Active: true, ValidFrom: fmtTime(time.Time{})})
	if e := c.classify(context.Background(), "old-boot", other); e.class != classForeignBootstrap {
		t.Fatalf("foreign bootstrap = %+v", e)
	}
}

func TestClassify_Undecodable(t *testing.T) {
	c, v := testClassifier(t)
	for name, data := range map[string][]byte{
		"not json":     []byte("{"),
		"kid mismatch": issuedRecord(t, v, "other", "client"),
		"bad kind":     []byte(`{"kind":"x","kid":"k1","validFrom":"2026-01-01T00:00:00Z"}`),
	} {
		if e := c.classify(context.Background(), "k1", data); e.class != classUndecodable || e.pair.KID != "k1" {
			t.Fatalf("%s: entry = %+v", name, e)
		}
	}
}

// Review focus: a record written by a later version with extra fields.
func TestDecodeSigningRecord_IgnoresUnknownFields(t *testing.T) {
	_, v := testClassifier(t)
	var m map[string]any
	_ = json.Unmarshal(issuedRecord(t, v, "k1", "client"), &m)
	m["futureField"] = map[string]any{"x": 1}
	b, _ := json.Marshal(m)
	if _, _, _, _, err := decodeSigningRecord("k1", b); err != nil {
		t.Fatalf("unknown field made the record undecodable: %v", err)
	}
}
```

- [ ] **Step 2: Run and see them fail**

Run: `go test ./internal/auth/ -run 'TestClassify|TestDecodeSigningRecord'`
Expected: build failure — `undefined: encodeSigningRecord`, `signingRecord`, `newClassifier`.

- [ ] **Step 3: Implement** — add `Bootstrap bool // the key built from configuration` to `KeyPair` in `store.go`. `internal/auth/signing_records.go`:

```go
package auth

import (
	"context"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
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
	pub, ok := parsed.(*rsa.PublicKey)
	if err != nil || !ok {
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
	sealedHash [32]byte
	signer     Signer
}

// signerCache keeps each opened signer until its record's sealed bytes change.
type signerCache struct {
	mu sync.Mutex
	m  map[string]cachedSigner
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
	h := sha256.Sum256(sealed)
	c.signers.mu.Lock()
	cached, ok := c.signers.m[meta.KID]
	c.signers.mu.Unlock()
	if ok && cached.sealedHash == h {
		return cached.signer, nil
	}
	s, err := c.vault.Open(ctx, meta, sealed)
	if err != nil {
		return nil, err
	}
	c.signers.mu.Lock()
	c.signers.m[meta.KID] = cachedSigner{sealedHash: h, signer: s}
	c.signers.mu.Unlock()
	return s, nil
}
```

- [ ] **Step 4: Run the tests**

Run: `go test ./internal/auth/ -run 'TestClassify|TestDecodeSigningRecord'`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/auth/signing_records.go internal/auth/signing_records_internal_test.go internal/auth/store.go
git commit -m "feat(auth): signing-key records and per-node classification

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 6: `KVKeyStore` — reads from the node copy

**Files:**
- Create: `internal/auth/kv_key_store.go`, `internal/auth/kv_key_store_test.go`
- Modify: `internal/auth/service.go:63-68` (move KID derivation into `DeriveKID`; `NewAuthService` calls it)

**Interfaces:**
- Consumes: Tasks 2, 3, 5.
- Produces:

```go
var ErrKeyPairNotFound = errors.New("key pair not found")
var ErrKeyPairBroken   = errors.New("key pair cannot be used")
var ErrStoreStale error = storeStaleError{} // StorageUnavailable() == true

func DeriveKID(pub *rsa.PublicKey) (string, error)

type KVKeyStoreConfig struct {
	Bootstrap         *rsa.PrivateKey
	BootstrapAudience string
	Vault             KeyVault // nil: the wrapped vault of Bootstrap
	Broadcaster       spi.ClusterBroadcaster
	ReconcileInterval time.Duration
	Metrics           ReconcileMetrics
}

func NewKVKeyStore(ctx context.Context, kv spi.KeyValueStore, cfg KVKeyStoreConfig) (*KVKeyStore, error)
func (s *KVKeyStore) Signer(audience string) (*KeyPair, Signer, error)
func (s *KVKeyStore) Current(audience string) (*KeyPair, error)
func (s *KVKeyStore) VerificationKey(kid string) (*rsa.PublicKey, error)
func (s *KVKeyStore) Published() ([]*KeyPair, error)
func (s *KVKeyStore) BootstrapKID() string
func (s *KVKeyStore) Reconcile(ctx context.Context) error
func (s *KVKeyStore) Start(ctx context.Context)
```

- [ ] **Step 1: Write the failing tests** — `internal/auth/kv_key_store_test.go` (package `auth_test`):

```go
package auth_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/auth"
)

func newBootstrap(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func newKeyStore(t *testing.T, kv spi.KeyValueStore, boot *rsa.PrivateKey, aud string) *auth.KVKeyStore {
	t.Helper()
	s, err := auth.NewKVKeyStore(systemCtx(), kv, auth.KVKeyStoreConfig{Bootstrap: boot, BootstrapAudience: aud})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestKVKeyStore_BootstrapSignsByDefault(t *testing.T) {
	boot := newBootstrap(t)
	s := newKeyStore(t, mustNewMemoryKV(t, systemCtx()), boot, "client")
	kp, signer, err := s.Signer("client")
	if err != nil {
		t.Fatal(err)
	}
	if !kp.Bootstrap || kp.KID != s.BootstrapKID() || !signer.Public().(*rsa.PublicKey).Equal(&boot.PublicKey) {
		t.Fatalf("signer = %+v", kp)
	}
	if _, err := s.VerificationKey(s.BootstrapKID()); err != nil {
		t.Fatal(err)
	}
	pub, err := s.Published()
	if err != nil || len(pub) != 1 || pub[0].KID != s.BootstrapKID() {
		t.Fatalf("published = %v, %v", pub, err)
	}
	if _, _, err := s.Signer("human"); !errors.Is(err, auth.ErrKeyPairNotFound) {
		t.Fatalf("human signer: err = %v, want ErrKeyPairNotFound", err)
	}
}

func TestDeriveKID_MatchesBootstrapKID(t *testing.T) {
	boot := newBootstrap(t)
	s := newKeyStore(t, mustNewMemoryKV(t, systemCtx()), boot, "client")
	kid, err := auth.DeriveKID(&boot.PublicKey)
	if err != nil || kid != s.BootstrapKID() || len(kid) != 32 {
		t.Fatalf("kid = %q, err = %v", kid, err)
	}
}

func TestKVKeyStore_UnknownKIDIsNotFound(t *testing.T) {
	s := newKeyStore(t, mustNewMemoryKV(t, systemCtx()), newBootstrap(t), "client")
	if _, err := s.VerificationKey("nope"); !errors.Is(err, auth.ErrKeyPairNotFound) {
		t.Fatalf("err = %v", err)
	}
}

// An undecodable record at the bootstrap KID refuses the bootstrap key.
func TestKVKeyStore_UndecodableBootstrapStateRefusesBootstrap(t *testing.T) {
	ctx := systemCtx()
	kv := mustNewMemoryKV(t, ctx)
	boot := newBootstrap(t)
	kid, _ := auth.DeriveKID(&boot.PublicKey)
	_ = kv.Put(ctx, "signing-keys", kid, []byte("{"))
	s := newKeyStore(t, kv, boot, "client")
	if _, err := s.VerificationKey(kid); !errors.Is(err, auth.ErrKeyPairNotFound) {
		t.Fatalf("verification: err = %v", err)
	}
	if _, _, err := s.Signer("client"); !errors.Is(err, auth.ErrKeyPairBroken) {
		t.Fatalf("signer: err = %v, want ErrKeyPairBroken", err)
	}
}

func TestKVKeyStore_UndecodableRecordStopsSigning(t *testing.T) {
	ctx := systemCtx()
	kv := mustNewMemoryKV(t, ctx)
	_ = kv.Put(ctx, "signing-keys", "garbage", []byte("not json"))
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	defer slog.SetDefault(prev)
	s := newKeyStore(t, kv, newBootstrap(t), "client")
	if _, _, err := s.Signer("client"); !errors.Is(err, auth.ErrKeyPairBroken) {
		t.Fatalf("err = %v, want ErrKeyPairBroken", err)
	}
	if !strings.Contains(buf.String(), "garbage") || !strings.Contains(buf.String(), "level=ERROR") {
		t.Fatalf("expected an ERROR naming the record; log: %s", buf.String())
	}
}

func TestKVKeyStore_StaleFailsClosed(t *testing.T) {
	ctx := systemCtx()
	kv := &toggleListKV{KeyValueStore: mustNewMemoryKV(t, ctx)}
	boot := newBootstrap(t)
	s, err := auth.NewKVKeyStore(ctx, kv, auth.KVKeyStoreConfig{Bootstrap: boot, BootstrapAudience: "client", ReconcileInterval: 20 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	kv.fail.Store(true)
	loop, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.Start(loop)
	deadline := time.Now().Add(3 * time.Second)
	for {
		_, err := s.Published()
		if errors.Is(err, auth.ErrStoreStale) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("never became stale")
		}
		time.Sleep(20 * time.Millisecond)
	}
	var su interface{ StorageUnavailable() bool }
	if !errors.As(auth.ErrStoreStale, &su) || !su.StorageUnavailable() {
		t.Fatal("ErrStoreStale must carry the storage-unavailable marker")
	}
	if _, err := s.VerificationKey(s.BootstrapKID()); !errors.Is(err, auth.ErrKeyPairNotFound) {
		t.Fatalf("verification while stale: err = %v", err)
	}
	if _, _, err := s.Signer("client"); !errors.Is(err, auth.ErrStoreStale) {
		t.Fatalf("signer while stale: err = %v", err)
	}
}
```

Add to `kv_trusted_store_reconcile_test.go` (same package) a reusable `toggleListKV`:

```go
type toggleListKV struct {
	spi.KeyValueStore
	fail atomic.Bool
}

func (k *toggleListKV) List(ctx context.Context, ns string) (map[string][]byte, error) {
	if k.fail.Load() {
		return nil, errors.New("list down")
	}
	return k.KeyValueStore.List(ctx, ns)
}
```

- [ ] **Step 2: Run and see them fail**

Run: `go test ./internal/auth/ -run 'TestKVKeyStore|TestDeriveKID'`
Expected: build failure — `undefined: auth.NewKVKeyStore`, `auth.KVKeyStoreConfig`, `auth.DeriveKID`, `auth.ErrStoreStale`.

- [ ] **Step 3: Implement** — `internal/auth/kv_key_store.go`:

```go
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
		afterChange: s.logClassChanges,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to load signing keys from KV store: %w", err)
	}
	s.rep = rep
	s.logClassChanges()
	s.logRevokedBootstrap(slog.LevelInfo)
	return s, nil
}

func (s *KVKeyStore) BootstrapKID() string                 { return s.boot.kid }
func (s *KVKeyStore) Reconcile(ctx context.Context) error  { return s.rep.Reconcile(ctx) }
func (s *KVKeyStore) Start(ctx context.Context)            { s.rep.Start(ctx) }

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

// selectSigner applies the signing rule (spec §5.8): among owned and broken
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
// pair may verify on this node now: owned or the bootstrap key, active and
// inside its window. There is no store read on this path.
func (s *KVKeyStore) VerificationKey(kid string) (*rsa.PublicKey, error) {
	if s.rep.Stale() {
		return nil, fmt.Errorf("%w: %s (store stale)", ErrKeyPairNotFound, kid)
	}
	now := time.Now()
	var pub *rsa.PublicKey
	s.rep.read(func(recs map[string]*signingEntry) {
		if kid == s.boot.kid {
			if bv := s.bootstrapView(recs); bv.usable && bv.pair.Active && bv.pair.InWindow(now) {
				pub = bv.pair.PublicKey
			}
			return
		}
		if e, ok := recs[kid]; ok && e.class == classOwned && e.pair.Active && e.pair.InWindow(now) {
			pub = e.pair.PublicKey
		}
	})
	if pub == nil {
		return nil, fmt.Errorf("%w: %s", ErrKeyPairNotFound, kid)
	}
	return pub, nil
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
			if e.class == classOwned && windowOpen(e.pair.ValidTo, now) {
				p := e.pair
				out = append(out, &p)
			}
		}
		if bv := s.bootstrapView(recs); bv.usable && windowOpen(bv.pair.ValidTo, now) {
			p := bv.pair
			out = append(out, &p)
		}
	})
	sort.Slice(out, func(i, j int) bool { return out[i].KID < out[j].KID })
	return out, nil
}

// logClassChanges logs when the set of retired, or of broken and undecodable,
// records changes — not on every re-read.
func (s *KVKeyStore) logClassChanges() {
	var retired, broken []string
	s.rep.read(func(recs map[string]*signingEntry) {
		for k, e := range recs {
			switch e.class {
			case classRetired:
				retired = append(retired, k)
			case classBroken, classUndecodable:
				broken = append(broken, k+" ("+e.reason+")")
			}
		}
	})
	sort.Strings(retired)
	sort.Strings(broken)
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
}

// ownedIssuedCount counts the issued key pairs this node's vault owns.
func (s *KVKeyStore) ownedIssuedCount() int {
	n := 0
	s.rep.read(func(recs map[string]*signingEntry) {
		for _, e := range recs {
			if e.class == classOwned || e.class == classBroken {
				n++
			}
		}
	})
	return n
}

// logRevokedBootstrap warns that revoking the bootstrap key through the API
// does not protect the key pairs sealed under it (spec §4).
func (s *KVKeyStore) logRevokedBootstrap(level slog.Level) {
	if s.vault.Owner() != s.boot.kid {
		return
	}
	var revoked bool
	s.rep.read(func(recs map[string]*signingEntry) {
		bv := s.bootstrapView(recs)
		revoked = !bv.usable || !bv.pair.Active
	})
	n := s.ownedIssuedCount()
	if !revoked || n == 0 {
		return
	}
	slog.Log(context.Background(), level,
		"the bootstrap signing key no longer signs, but CYODA_JWT_SIGNING_KEY still unseals the issued key pairs it owns; replace it if it may be exposed",
		"pkg", "auth", "ownedKeyPairs", n)
}
```

(`logRevokedBootstrap` is called with `slog.LevelWarn` by the admin writes in Task 7.)

In `service.go` replace the inline KID derivation (`:63-68`) with `signingKID, err := DeriveKID(&privateKey.PublicKey)`.

- [ ] **Step 4: Run the tests**

Run: `go test ./internal/auth/...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/auth/kv_key_store.go internal/auth/kv_key_store_test.go internal/auth/kv_trusted_store_reconcile_test.go internal/auth/service.go
git commit -m "feat(auth): KV-backed signing-key store — node-copy reads and signer rule

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 7: `KVKeyStore` — admin writes

**Files:**
- Create: `internal/auth/kv_key_store_admin.go`, `internal/auth/kv_key_store_admin_test.go`

**Interfaces:**
- Consumes: Tasks 3, 5, 6.
- Produces:

```go
type IssueRequest struct {
	Audience       string
	ValidFrom      time.Time
	ValidTo        time.Time
	Invalidate     bool
	GracePeriodSec int64
}
func (s *KVKeyStore) Issue(ctx context.Context, req IssueRequest) (*KeyPair, error)
func (s *KVKeyStore) Invalidate(ctx context.Context, kid string, graceSec int64) error
func (s *KVKeyStore) Reactivate(ctx context.Context, kid string, from, to time.Time) (*KeyPair, error)
func (s *KVKeyStore) Delete(ctx context.Context, kid string) error
```

Validation of the request fields stays in the adapter (`keys_adapter.go`); the store does not repeat it.

- [ ] **Step 1: Write the failing tests** — `internal/auth/kv_key_store_admin_test.go`:

```go
package auth_test

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/auth"
)

func issue(t *testing.T, s *auth.KVKeyStore, aud string, invalidate bool) *auth.KeyPair {
	t.Helper()
	now := time.Now()
	kp, err := s.Issue(systemCtx(), auth.IssueRequest{Audience: aud, ValidFrom: now, ValidTo: now.Add(time.Hour), Invalidate: invalidate})
	if err != nil {
		t.Fatal(err)
	}
	return kp
}

func TestKVKeyStore_IssueSignsAndIsSharedThroughTheStore(t *testing.T) {
	ctx := systemCtx()
	kv := mustNewMemoryKV(t, ctx)
	boot := newBootstrap(t)
	a := newKeyStore(t, kv, boot, "client")
	b := newKeyStore(t, kv, boot, "client")
	kp := issue(t, a, "client", false)
	signerKP, _, err := a.Signer("client")
	if err != nil || signerKP.KID != kp.KID {
		t.Fatalf("A signs with %v, err %v; want %s", signerKP, err, kp.KID)
	}
	if err := b.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := b.VerificationKey(kp.KID); err != nil {
		t.Fatalf("B cannot verify: %v", err)
	}
	c := newKeyStore(t, kv, boot, "client") // a restart
	if got, _, err := c.Signer("client"); err != nil || got.KID != kp.KID {
		t.Fatalf("after restart: %v, %v", got, err)
	}
}

// Req 4: the stored bytes never hold the private key.
func TestKVKeyStore_StoredValueHasNoPrivateKey(t *testing.T) {
	ctx := systemCtx()
	kv := mustNewMemoryKV(t, ctx)
	boot := newBootstrap(t)
	s := newKeyStore(t, kv, boot, "client")
	kp := issue(t, s, "client", false)
	raw, _ := kv.Get(ctx, "signing-keys", kp.KID)
	var rec struct {
		PublicKey string `json:"publicKey"`
		Vault     struct{ Owner, Sealed string } `json:"vault"`
	}
	_ = json.Unmarshal(raw, &rec)
	spki, _ := base64.StdEncoding.DecodeString(rec.PublicKey)
	sealed, _ := base64.StdEncoding.DecodeString(rec.Vault.Sealed)
	priv := mustOpenPrivate(t, boot, s.BootstrapKID(), kp.KID, rec.Vault.Owner, spki, sealed)
	for _, der := range [][]byte{x509.MarshalPKCS1PrivateKey(priv), mustPKCS8(t, priv)} {
		for _, form := range [][]byte{der, []byte(base64.StdEncoding.EncodeToString(der))} {
			if bytes.Contains(raw, form) {
				t.Fatal("stored record contains private-key DER")
			}
		}
	}
}

func mustPKCS8(t *testing.T, k *rsa.PrivateKey) []byte {
	t.Helper()
	b, err := x509.MarshalPKCS8PrivateKey(k)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// Req 5: a vault that never exports keys signs through the store.
type remoteVault struct {
	mu   sync.Mutex
	keys map[string]*rsa.PrivateKey // stands in for keys held by a KMS
}

type remoteSigner struct {
	v   *remoteVault
	ref string
	pub *rsa.PublicKey
}

func (r remoteSigner) Public() crypto.PublicKey { return r.pub }
func (r remoteSigner) Sign(ctx context.Context, d []byte) ([]byte, error) {
	r.v.mu.Lock()
	k := r.v.keys[r.ref]
	r.v.mu.Unlock()
	return auth.NewRSASigner(k).Sign(ctx, d)
}
func (v *remoteVault) Kind() string  { return "remote" }
func (v *remoteVault) Owner() string { return "remote-scope" }
func (v *remoteVault) Generate(_ context.Context, m auth.KeyMeta) ([]byte, []byte, auth.Signer, error) {
	k, _ := rsa.GenerateKey(rand.Reader, 2048)
	v.mu.Lock()
	v.keys[m.KID] = k
	v.mu.Unlock()
	spki, _ := x509.MarshalPKIXPublicKey(&k.PublicKey)
	return spki, []byte(m.KID), remoteSigner{v, m.KID, &k.PublicKey}, nil
}
func (v *remoteVault) Open(_ context.Context, m auth.KeyMeta, sealed []byte) (auth.Signer, error) {
	v.mu.Lock()
	k, ok := v.keys[string(sealed)]
	v.mu.Unlock()
	if !ok {
		return nil, auth.ErrUnseal
	}
	return remoteSigner{v, string(sealed), &k.PublicKey}, nil
}

func TestKVKeyStore_NonExportingVaultSigns(t *testing.T) {
	ctx := systemCtx()
	rv := &remoteVault{keys: map[string]*rsa.PrivateKey{}}
	s, err := auth.NewKVKeyStore(ctx, mustNewMemoryKV(t, ctx), auth.KVKeyStoreConfig{Bootstrap: newBootstrap(t), BootstrapAudience: "human", Vault: rv})
	if err != nil {
		t.Fatal(err)
	}
	kp := issue(t, s, "client", false)
	got, signer, err := s.Signer("client")
	if err != nil || got.KID != kp.KID {
		t.Fatalf("%v %v", got, err)
	}
	tok, err := auth.Sign(ctx, map[string]any{"sub": "x"}, signer, got.KID)
	if err != nil {
		t.Fatal(err)
	}
	parsed, _ := auth.Parse(tok)
	h := sha256.Sum256([]byte(parsed.SigningInput))
	if err := rsa.VerifyPKCS1v15(kp.PublicKey, crypto.SHA256, h[:], parsed.Signature); err != nil {
		t.Fatal(err)
	}
}

func TestKVKeyStore_RotationInvalidatesSiblingsAndBootstrap(t *testing.T) {
	ctx := systemCtx()
	kv := mustNewMemoryKV(t, ctx)
	s := newKeyStore(t, kv, newBootstrap(t), "client")
	old := issue(t, s, "client", false)
	humanKey := issue(t, s, "human", false)
	nw := issue(t, s, "client", true)
	if _, err := s.VerificationKey(old.KID); !errors.Is(err, auth.ErrKeyPairNotFound) {
		t.Fatal("old client key still verifies after rotation")
	}
	if _, err := s.VerificationKey(s.BootstrapKID()); !errors.Is(err, auth.ErrKeyPairNotFound) {
		t.Fatal("bootstrap key (audience client) still verifies after rotation")
	}
	if _, err := s.VerificationKey(humanKey.KID); err != nil {
		t.Fatal("a key of another audience was invalidated")
	}
	if got, _, _ := s.Signer("client"); got.KID != nw.KID {
		t.Fatalf("signer = %s, want %s", got.KID, nw.KID)
	}
}

// Review focus: bootstrap audience human; a client rotation leaves it alone.
func TestKVKeyStore_RotationLeavesOtherAudienceBootstrap(t *testing.T) {
	s := newKeyStore(t, mustNewMemoryKV(t, systemCtx()), newBootstrap(t), "human")
	issue(t, s, "client", true)
	if _, err := s.VerificationKey(s.BootstrapKID()); err != nil {
		t.Fatalf("human bootstrap key invalidated by a client rotation: %v", err)
	}
}

type failPutKV struct {
	spi.KeyValueStore
	failKey string
}

func (f *failPutKV) Put(ctx context.Context, ns, key string, v []byte) error {
	if key == f.failKey {
		return errors.New("injected failure")
	}
	return f.KeyValueStore.Put(ctx, ns, key, v)
}

func TestKVKeyStore_RotationCompensatesOnSiblingFailure(t *testing.T) {
	ctx := systemCtx()
	mem := mustNewMemoryKV(t, ctx)
	boot := newBootstrap(t)
	pre := newKeyStore(t, mem, boot, "human")
	old := issue(t, pre, "client", false)
	before, _ := mem.Get(ctx, "signing-keys", old.KID)
	bc := newFakeBroadcaster()
	var pings int
	bc.Subscribe("auth.signingkeys", func([]byte) { pings++ })
	s, _ := auth.NewKVKeyStore(ctx, &failPutKV{KeyValueStore: mem, failKey: old.KID},
		auth.KVKeyStoreConfig{Bootstrap: boot, BootstrapAudience: "human", Broadcaster: bc})
	now := time.Now()
	_, err := s.Issue(ctx, auth.IssueRequest{Audience: "client", ValidFrom: now, ValidTo: now.Add(time.Hour), Invalidate: true})
	if err == nil {
		t.Fatal("expected the sibling failure")
	}
	entries, _ := mem.List(ctx, "signing-keys")
	if len(entries) != 1 {
		t.Fatalf("store holds %d records after a failed rotation, want only the old key", len(entries))
	}
	if after, _ := mem.Get(ctx, "signing-keys", old.KID); !bytes.Equal(before, after) {
		t.Fatal("sibling changed by a failed rotation")
	}
	if pings == 0 {
		t.Fatal("no change message after a failed rotation that wrote the store")
	}
}

func TestKVKeyStore_BootstrapDeleteIsTerminalAndSurvivesRestart(t *testing.T) {
	ctx := systemCtx()
	kv := mustNewMemoryKV(t, ctx)
	boot := newBootstrap(t)
	s := newKeyStore(t, kv, boot, "client")
	issue(t, s, "client", false)
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	defer slog.SetDefault(prev)
	if err := s.Delete(ctx, s.BootstrapKID()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "still unseals") || !strings.Contains(buf.String(), "level=WARN") {
		t.Fatalf("missing WARN about the PEM; log: %s", buf.String())
	}
	r := newKeyStore(t, kv, boot, "client") // restart
	if _, err := r.VerificationKey(r.BootstrapKID()); !errors.Is(err, auth.ErrKeyPairNotFound) {
		t.Fatal("deleted bootstrap key verifies after restart")
	}
	for name, call := range map[string]func() error{
		"reactivate": func() error { _, err := r.Reactivate(ctx, r.BootstrapKID(), time.Now(), time.Now().Add(time.Hour)); return err },
		"invalidate": func() error { return r.Invalidate(ctx, r.BootstrapKID(), 0) },
		"delete":     func() error { return r.Delete(ctx, r.BootstrapKID()) },
	} {
		if err := call(); !errors.Is(err, auth.ErrKeyPairNotFound) {
			t.Fatalf("%s after delete: err = %v, want ErrKeyPairNotFound", name, err)
		}
	}
	// A rotation never touches a deleted bootstrap key.
	issue(t, r, "client", true)
	raw, _ := kv.Get(ctx, "signing-keys", r.BootstrapKID())
	if !strings.Contains(string(raw), `"deleted":true`) {
		t.Fatalf("deleted flag lost: %s", raw)
	}
}

func TestKVKeyStore_NoWarnWithoutOwnedPairs(t *testing.T) {
	ctx := systemCtx()
	s := newKeyStore(t, mustNewMemoryKV(t, ctx), newBootstrap(t), "client")
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	defer slog.SetDefault(prev)
	if err := s.Invalidate(ctx, s.BootstrapKID(), 0); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(buf.String(), "still unseals") {
		t.Fatal("WARN logged with no owned key pairs")
	}
}

func TestKVKeyStore_BootstrapInvalidateReactivateAcrossRestart(t *testing.T) {
	ctx := systemCtx()
	kv := mustNewMemoryKV(t, ctx)
	boot := newBootstrap(t)
	s := newKeyStore(t, kv, boot, "client")
	_ = s.Invalidate(ctx, s.BootstrapKID(), 0)
	r := newKeyStore(t, kv, boot, "client")
	if _, err := r.VerificationKey(r.BootstrapKID()); !errors.Is(err, auth.ErrKeyPairNotFound) {
		t.Fatal("invalidated bootstrap key back after restart")
	}
	kp, err := r.Reactivate(ctx, r.BootstrapKID(), time.Now().Add(-time.Second), time.Now().Add(time.Hour))
	if err != nil || !kp.Active || kp.ValidTo == nil {
		t.Fatalf("reactivate: %+v %v", kp, err)
	}
	if _, err := r.VerificationKey(r.BootstrapKID()); err != nil {
		t.Fatal(err)
	}
}

func TestKVKeyStore_RetiredAndForeignAreNotFound(t *testing.T) {
	ctx := systemCtx()
	kv := mustNewMemoryKV(t, ctx)
	a := newKeyStore(t, kv, newBootstrap(t), "client")
	kp := issue(t, a, "client", false)
	_ = a.Invalidate(ctx, a.BootstrapKID(), 60) // writes a bootstrap-state record
	b := newKeyStore(t, kv, newBootstrap(t), "client") // another bootstrap key
	for _, kid := range []string{kp.KID, a.BootstrapKID()} {
		if err := b.Invalidate(ctx, kid, 0); !errors.Is(err, auth.ErrKeyPairNotFound) {
			t.Fatalf("invalidate %s: %v", kid, err)
		}
		if err := b.Delete(ctx, kid); !errors.Is(err, auth.ErrKeyPairNotFound) {
			t.Fatalf("delete %s: %v", kid, err)
		}
		if _, err := b.Reactivate(ctx, kid, time.Now(), time.Now().Add(time.Hour)); !errors.Is(err, auth.ErrKeyPairNotFound) {
			t.Fatalf("reactivate %s: %v", kid, err)
		}
	}
	if got, _, _ := b.Signer("client"); got.KID != b.BootstrapKID() {
		t.Fatalf("retired key signs on another bootstrap key's node: %s", got.KID)
	}
	pub, _ := b.Published()
	for _, p := range pub {
		if p.KID == kp.KID {
			t.Fatal("retired key published in JWKS")
		}
	}
}

func TestKVKeyStore_StaleCopyCannotResurrectDeleted(t *testing.T) {
	ctx := systemCtx()
	kv := mustNewMemoryKV(t, ctx)
	boot := newBootstrap(t)
	a := newKeyStore(t, kv, boot, "client")
	kp := issue(t, a, "client", false)
	b := newKeyStore(t, kv, boot, "client")
	if err := a.Delete(ctx, kp.KID); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Reactivate(ctx, kp.KID, time.Now(), time.Now().Add(time.Hour)); !errors.Is(err, auth.ErrKeyPairNotFound) {
		t.Fatalf("stale node reactivated a deleted key: %v", err)
	}
	if _, err := kv.Get(ctx, "signing-keys", kp.KID); !errors.Is(err, spi.ErrNotFound) {
		t.Fatal("deleted key back in the store")
	}
}

func TestKVKeyStore_RotationSeesSiblingIssuedElsewhere(t *testing.T) {
	ctx := systemCtx()
	kv := mustNewMemoryKV(t, ctx)
	boot := newBootstrap(t)
	a := newKeyStore(t, kv, boot, "human")
	b := newKeyStore(t, kv, boot, "human")
	k1 := issue(t, a, "client", false)
	issue(t, b, "client", true) // b has not re-read
	if err := a.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := a.VerificationKey(k1.KID); !errors.Is(err, auth.ErrKeyPairNotFound) {
		t.Fatal("sibling issued on another node stayed active")
	}
}

func TestKVKeyStore_UndecodableCanBeDeleted(t *testing.T) {
	ctx := systemCtx()
	kv := mustNewMemoryKV(t, ctx)
	_ = kv.Put(ctx, "signing-keys", "junk", []byte("{"))
	s := newKeyStore(t, kv, newBootstrap(t), "client")
	if err := s.Delete(ctx, "junk"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Signer("client"); err != nil {
		t.Fatalf("signing still blocked after removing the undecodable record: %v", err)
	}
}

// Review focus: two issues at once on one node.
func TestKVKeyStore_ConcurrentIssueOnOneNode(t *testing.T) {
	s := newKeyStore(t, mustNewMemoryKV(t, systemCtx()), newBootstrap(t), "human")
	var wg sync.WaitGroup
	kids := make([]string, 2)
	for i := range kids {
		wg.Add(1)
		go func(i int) { defer wg.Done(); kids[i] = issue(t, s, "client", false).KID }(i)
	}
	wg.Wait()
	if kids[0] == kids[1] {
		t.Fatal("duplicate KID")
	}
	first, _, _ := s.Signer("client")
	second, _, _ := s.Signer("client")
	if first.KID != second.KID {
		t.Fatal("signer choice not deterministic")
	}
}
```

Add the helper `mustOpenPrivate` at the bottom of this file. It needs the private key, which the public API never returns, so it re-derives it from the sealed bytes with a second wrapped vault and the standard library:

```go
// mustOpenPrivate returns the private key sealed in a record, so the test can
// search the stored bytes for it. The vault's public API returns only a
// Signer, so it uses OpenPrivateKeyForTest from export_internal_test.go.
func mustOpenPrivate(t *testing.T, boot *rsa.PrivateKey, owner, kid, recOwner string, spki, sealed []byte) *rsa.PrivateKey {
	t.Helper()
	k, err := auth.OpenPrivateKeyForTest(boot, owner, auth.KeyMeta{KID: kid, Audience: "client", Algorithm: "RS256", Owner: recOwner, SPKI: spki}, sealed)
	if err != nil {
		t.Fatal(err)
	}
	return k
}
```

and create `internal/auth/export_internal_test.go` (a `_test.go` file in package `auth`, so it is not production code):

```go
package auth

import (
	"context"
	"crypto/rsa"
)

// OpenPrivateKeyForTest opens a sealed key and returns the RSA key behind the
// signer. Test-only: it lives in a _test.go file.
func OpenPrivateKeyForTest(boot *rsa.PrivateKey, owner string, meta KeyMeta, sealed []byte) (*rsa.PrivateKey, error) {
	v, err := NewWrappedVault(boot, owner)
	if err != nil {
		return nil, err
	}
	s, err := v.Open(context.Background(), meta, sealed)
	if err != nil {
		return nil, err
	}
	return s.(rsaSigner).key, nil
}
```

- [ ] **Step 2: Run and see them fail**

Run: `go test ./internal/auth/ -run 'TestKVKeyStore_(Issue|StoredValue|NonExporting|Rotation|Bootstrap|NoWarn|Retired|StaleCopy|Undecodable|Concurrent)'`
Expected: build failure — `s.Issue`, `s.Invalidate`, `s.Reactivate`, `s.Delete` undefined.

- [ ] **Step 3: Implement** — `internal/auth/kv_key_store_admin.go`:

```go
package auth

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// IssueRequest is a validated POST /oauth/keys/keypair.
type IssueRequest struct {
	Audience       string
	ValidFrom      time.Time
	ValidTo        time.Time
	Invalidate     bool
	GracePeriodSec int64
}

func newKID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("failed to generate key id: %w", err)
	}
	return hex.EncodeToString(b), nil
}

func notFound(kid string) error { return fmt.Errorf("%w: %s", ErrKeyPairNotFound, kid) }

// Issue creates a key pair and, with Invalidate, ends every sibling of its
// audience (spec §5.7). The new record is written before the siblings; if a
// sibling write fails, every write is undone.
func (s *KVKeyStore) Issue(ctx context.Context, req IssueRequest) (*KeyPair, error) {
	var issued *KeyPair
	var bootstrapTouched bool
	err := s.rep.mutate(func() (func(map[string]*signingEntry), bool, error) {
		kid, err := newKID()
		if err != nil {
			return nil, false, err
		}
		meta := KeyMeta{KID: kid, Audience: req.Audience, Algorithm: "RS256", Owner: s.vault.Owner()}
		spki, sealed, _, err := s.vault.Generate(ctx, meta)
		if err != nil {
			return nil, false, fmt.Errorf("failed to generate key pair: %w", err)
		}
		vt := req.ValidTo
		data, err := encodeSigningRecord(signingRecord{
			Kind: recordKindIssued, KID: kid, Audience: req.Audience, Algorithm: "RS256", Active: true,
			ValidFrom: fmtTime(req.ValidFrom), ValidTo: fmtTimePtr(&vt),
			PublicKey: base64.StdEncoding.EncodeToString(spki),
			Vault:     &vaultReference{Kind: s.vault.Kind(), Owner: s.vault.Owner(), Sealed: base64.StdEncoding.EncodeToString(sealed)},
		})
		if err != nil {
			return nil, false, err
		}
		writes := []kvWrite{{key: kid, value: data}}
		if req.Invalidate {
			sib, touched, err := s.siblingWrites(ctx, req.Audience, kid, req.GracePeriodSec)
			if err != nil {
				return nil, false, err
			}
			writes, bootstrapTouched = append(writes, sib...), touched
		}
		if err := s.rep.writeAll(ctx, writes); err != nil {
			return nil, true, err
		}
		entries := s.classifyWrites(ctx, writes)
		p := entries[kid].pair
		issued = &p
		return func(recs map[string]*signingEntry) {
			for k, e := range entries {
				recs[k] = e
			}
		}, true, nil
	})
	if err != nil {
		return nil, err
	}
	if bootstrapTouched {
		s.logRevokedBootstrap(slog.LevelWarn)
	}
	return issued, nil
}

// siblingWrites lists the stored records and returns the writes that end the
// siblings of a rotation: owned and broken issued records of the audience
// whose window is open, and the bootstrap key of that audience unless it is
// deleted. Only the active flag and validTo change.
func (s *KVKeyStore) siblingWrites(ctx context.Context, audience, newKID string, grace int64) ([]kvWrite, bool, error) {
	all, err := s.kv.List(ctx, signingKeysNamespace)
	if err != nil {
		return nil, false, fmt.Errorf("failed to list signing keys: %w", err)
	}
	now := time.Now()
	end := func(r *signingRecord, validTo *time.Time) {
		r.Active = false
		r.ValidTo = fmtTimePtr(graceExpiry(validTo, now, grace))
	}
	var writes []kvWrite
	for k, data := range all {
		if k == newKID || k == s.boot.kid {
			continue
		}
		e := s.cls.classify(ctx, k, data)
		if (e.class != classOwned && e.class != classBroken) || e.pair.Audience != audience || !windowOpen(e.pair.ValidTo, now) {
			continue
		}
		rec, _, _, _, _ := decodeSigningRecord(k, data)
		end(&rec, e.pair.ValidTo)
		b, err := encodeSigningRecord(rec)
		if err != nil {
			return nil, false, err
		}
		writes = append(writes, kvWrite{key: k, value: b, prev: data})
	}
	sort.Slice(writes, func(i, j int) bool { return writes[i].key < writes[j].key })
	touched := false
	if s.boot.audience == audience {
		prev, present := all[s.boot.kid]
		rec := defaultBootstrapRecord(s.boot.kid)
		eligible := true
		var validTo *time.Time
		if present {
			e := s.cls.classify(ctx, s.boot.kid, prev)
			if e.class != classBootstrapState || e.deleted {
				eligible = false
			} else {
				rec, _, _, _, _ = decodeSigningRecord(s.boot.kid, prev)
				validTo = e.pair.ValidTo
			}
		}
		if eligible && windowOpen(validTo, now) {
			end(&rec, validTo)
			b, err := encodeSigningRecord(rec)
			if err != nil {
				return nil, false, err
			}
			writes = append(writes, kvWrite{key: s.boot.kid, value: b, prev: prev})
			touched = true
		}
	}
	return writes, touched, nil
}

func (s *KVKeyStore) classifyWrites(ctx context.Context, writes []kvWrite) map[string]*signingEntry {
	out := make(map[string]*signingEntry, len(writes))
	for _, w := range writes {
		out[w.key] = s.cls.classify(ctx, w.key, w.value)
	}
	return out
}

// changeable reads one record this node may change: an owned or broken issued
// key pair, or this node's bootstrap key unless deleted (absent state → the
// default record, prev nil). Everything else is not found.
func (s *KVKeyStore) changeable(ctx context.Context, kid string) ([]byte, signingRecord, error) {
	data, err := s.kv.Get(ctx, signingKeysNamespace, kid)
	if errors.Is(err, spi.ErrNotFound) {
		if kid == s.boot.kid {
			return nil, defaultBootstrapRecord(kid), nil
		}
		return nil, signingRecord{}, notFound(kid)
	}
	if err != nil {
		return nil, signingRecord{}, fmt.Errorf("failed to read signing key: %w", err)
	}
	e := s.cls.classify(ctx, kid, data)
	ok := (kid == s.boot.kid && e.class == classBootstrapState && !e.deleted) ||
		(kid != s.boot.kid && (e.class == classOwned || e.class == classBroken))
	if !ok {
		return nil, signingRecord{}, notFound(kid)
	}
	var rec signingRecord
	if err := json.Unmarshal(data, &rec); err != nil {
		return nil, signingRecord{}, notFound(kid)
	}
	return data, rec, nil
}

// updateState changes the active flag or the window of one record.
func (s *KVKeyStore) updateState(ctx context.Context, kid string, change func(r *signingRecord, now time.Time)) (*KeyPair, error) {
	var out *KeyPair
	err := s.rep.mutate(func() (func(map[string]*signingEntry), bool, error) {
		prev, rec, err := s.changeable(ctx, kid)
		if err != nil {
			return nil, false, err
		}
		change(&rec, time.Now())
		data, err := encodeSigningRecord(rec)
		if err != nil {
			return nil, false, err
		}
		if err := s.rep.writeAll(ctx, []kvWrite{{key: kid, value: data, prev: prev}}); err != nil {
			return nil, true, err
		}
		e := s.cls.classify(ctx, kid, data)
		p := e.pair
		if kid == s.boot.kid {
			p.Audience, p.PublicKey = s.boot.audience, s.boot.public
		}
		out = &p
		return func(recs map[string]*signingEntry) { recs[kid] = e }, true, nil
	})
	return out, err
}

func (s *KVKeyStore) Invalidate(ctx context.Context, kid string, graceSec int64) error {
	_, err := s.updateState(ctx, kid, func(r *signingRecord, now time.Time) {
		var validTo *time.Time
		if r.ValidTo != nil {
			if t, perr := time.Parse(time.RFC3339Nano, *r.ValidTo); perr == nil {
				validTo = &t
			}
		}
		r.Active = false
		r.ValidTo = fmtTimePtr(graceExpiry(validTo, now, graceSec))
	})
	if err == nil && kid == s.boot.kid {
		s.logRevokedBootstrap(slog.LevelWarn)
	}
	return err
}

func (s *KVKeyStore) Reactivate(ctx context.Context, kid string, from, to time.Time) (*KeyPair, error) {
	return s.updateState(ctx, kid, func(r *signingRecord, _ time.Time) {
		r.Active = true
		r.ValidFrom = fmtTime(from)
		r.ValidTo = fmtTimePtr(&to)
	})
}

// Delete removes an issued key pair (owned, broken or undecodable), or marks
// the bootstrap key deleted — terminal: no API call removes that record. An
// undecodable record at the bootstrap KID is replaced by a deleted state.
func (s *KVKeyStore) Delete(ctx context.Context, kid string) error {
	if kid != s.boot.kid {
		return s.rep.mutate(func() (func(map[string]*signingEntry), bool, error) {
			data, err := s.kv.Get(ctx, signingKeysNamespace, kid)
			if errors.Is(err, spi.ErrNotFound) {
				return nil, false, notFound(kid)
			}
			if err != nil {
				return nil, false, fmt.Errorf("failed to read signing key: %w", err)
			}
			e := s.cls.classify(ctx, kid, data)
			if e.class != classOwned && e.class != classBroken && e.class != classUndecodable {
				return nil, false, notFound(kid)
			}
			if err := s.rep.writeAll(ctx, []kvWrite{{key: kid, prev: data}}); err != nil {
				return nil, true, err
			}
			return func(recs map[string]*signingEntry) { delete(recs, kid) }, true, nil
		})
	}
	err := s.rep.mutate(func() (func(map[string]*signingEntry), bool, error) {
		data, err := s.kv.Get(ctx, signingKeysNamespace, kid)
		rec := defaultBootstrapRecord(kid)
		switch {
		case errors.Is(err, spi.ErrNotFound):
			data = nil
		case err != nil:
			return nil, false, fmt.Errorf("failed to read signing key: %w", err)
		default:
			e := s.cls.classify(ctx, kid, data)
			if e.class == classBootstrapState && e.deleted {
				return nil, false, notFound(kid)
			}
			if e.class == classBootstrapState {
				rec, _, _, _, _ = decodeSigningRecord(kid, data)
			}
		}
		rec.Deleted, rec.Active = true, false
		enc, err := encodeSigningRecord(rec)
		if err != nil {
			return nil, false, err
		}
		if err := s.rep.writeAll(ctx, []kvWrite{{key: kid, value: enc, prev: data}}); err != nil {
			return nil, true, err
		}
		e := s.cls.classify(ctx, kid, enc)
		return func(recs map[string]*signingEntry) { recs[kid] = e }, true, nil
	})
	if err == nil {
		s.logRevokedBootstrap(slog.LevelWarn)
	}
	return err
}
```

- [ ] **Step 4: Run the tests**

Run: `go test ./internal/auth/...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/auth/kv_key_store_admin.go internal/auth/kv_key_store_admin_test.go internal/auth/export_internal_test.go
git commit -m "feat(auth): signing-key admin writes — rotation with compensation, persisted bootstrap revocation

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 8: Switch over — `KeyStore` interface, service wiring, adapters, OpenAPI, deletions

**Files:**
- Modify: `internal/auth/store.go` (new `KeyStore` interface; `KeyPair.PrivateKey` removed; `InMemoryKeyStore`, `InMemoryTrustedKeyStore`, `NewInMemoryTrustedKeyStoreWithCap` deleted), `internal/auth/service.go` (rewrite), `internal/auth/token.go:83,102,221,241`, `internal/auth/key_source.go:34-52`, `internal/auth/jwks.go:25-66`
- Modify: `internal/domain/account/keys_adapter.go` (all five handlers), `app/app.go:271-310,372-382,611-617`
- Modify: `api/openapi.yaml` (503 on the five key-pair operations), then `go generate ./api`
- Modify (tests): every test that used the deleted in-memory stores or `NewAuthService` — `internal/auth/{store_test,keypair_signing_test,local_key_source_test,verification_test,jwks_test,token_test,delegating_test,integration_test,local_validator_integration_test,service_bootstrap_test,kv_trusted_store_test}.go`, `internal/grpc/interceptor_test.go`, `internal/domain/account/{keys_adapter_test,trusted_adapter_test,trusted_key_outage_test}.go`
- Test: `internal/domain/account/keys_adapter_errors_test.go` (new), `internal/auth/jwks_stale_test.go` (new)

**Interfaces:**
- Consumes: Tasks 4, 6, 7.
- Produces:

```go
// KeyPair: public data only.
type KeyPair struct {
	KID       string
	Audience  string
	Algorithm string
	PublicKey *rsa.PublicKey
	Active    bool
	ValidFrom time.Time
	ValidTo   *time.Time
	Bootstrap bool
}

type KeyStore interface {
	Signer(audience string) (*KeyPair, Signer, error)
	Current(audience string) (*KeyPair, error)
	VerificationKey(kid string) (*rsa.PublicKey, error)
	Published() ([]*KeyPair, error)
	Issue(ctx context.Context, req IssueRequest) (*KeyPair, error)
	Invalidate(ctx context.Context, kid string, graceSec int64) error
	Reactivate(ctx context.Context, kid string, from, to time.Time) (*KeyPair, error)
	Delete(ctx context.Context, kid string) error
}

type AuthConfig struct {
	SigningKeyPEM     string
	Issuer            string
	ExpirySeconds     int
	IAMFeatures       IAMFeatures
	KV                spi.KeyValueStore      // SYSTEM tenant; required
	Broadcaster       spi.ClusterBroadcaster // nil on a single node
	ReconcileInterval time.Duration
	TrustedKeyMetrics ReconcileMetrics
	SigningKeyMetrics ReconcileMetrics
}

func NewAuthService(ctx context.Context, config AuthConfig) (*AuthService, error)
func (s *AuthService) Start(ctx context.Context)
func NewJWKSHandler(ks KeyStore, retryAfter time.Duration) *JWKSHandler
```

- [ ] **Step 1: Write the failing tests**

`internal/domain/account/keys_adapter_errors_test.go` — every admin endpoint maps a store error to 500/503, never 404:

```go
package account

import (
	"context"
	"crypto/rsa"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	genapi "github.com/cyoda-platform/cyoda-go/api"
	"github.com/cyoda-platform/cyoda-go/internal/auth"
)

type failingKeyStore struct{ err error }

func (f failingKeyStore) Signer(string) (*auth.KeyPair, auth.Signer, error) { return nil, nil, f.err }
func (f failingKeyStore) Current(string) (*auth.KeyPair, error)             { return nil, f.err }
func (f failingKeyStore) VerificationKey(string) (*rsa.PublicKey, error)    { return nil, f.err }
func (f failingKeyStore) Published() ([]*auth.KeyPair, error)               { return nil, f.err }
func (f failingKeyStore) Issue(context.Context, auth.IssueRequest) (*auth.KeyPair, error) {
	return nil, f.err
}
func (f failingKeyStore) Invalidate(context.Context, string, int64) error { return f.err }
func (f failingKeyStore) Reactivate(context.Context, string, time.Time, time.Time) (*auth.KeyPair, error) {
	return nil, f.err
}
func (f failingKeyStore) Delete(context.Context, string) error { return f.err }

type unavailable struct{}

func (unavailable) Error() string            { return "down" }
func (unavailable) StorageUnavailable() bool { return true }

func adminReq(method, path, body string) *http.Request {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	return r.WithContext(spi.WithUserContext(r.Context(), &spi.UserContext{
		UserID: "a", Roles: []string{"ROLE_ADMIN"}, Tenant: spi.Tenant{ID: "t"},
	}))
}

func TestKeyPairAdapters_StoreErrorsAreNot404(t *testing.T) {
	future := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	calls := map[string]func(h *Handler, w http.ResponseWriter){
		"issue": func(h *Handler, w http.ResponseWriter) {
			h.IssueJwtKeyPair(w, adminReq("POST", "/oauth/keys/keypair", `{"algorithm":"RS256","audience":"client"}`))
		},
		"current": func(h *Handler, w http.ResponseWriter) {
			h.GetCurrentJwtKeyPair(w, adminReq("GET", "/oauth/keys/keypair/current", ""), genapi.GetCurrentJwtKeyPairParams{Audience: "client"})
		},
		"delete": func(h *Handler, w http.ResponseWriter) {
			h.DeleteJwtKeyPair(w, adminReq("DELETE", "/oauth/keys/keypair/k", ""), "k")
		},
		"invalidate": func(h *Handler, w http.ResponseWriter) {
			h.InvalidateJwtKeyPair(w, adminReq("POST", "/oauth/keys/keypair/k/invalidate", ""), "k")
		},
		"reactivate": func(h *Handler, w http.ResponseWriter) {
			h.ReactivateJwtKeyPair(w, adminReq("POST", "/oauth/keys/keypair/k/reactivate", `{"validTo":"`+future+`"}`), "k")
		},
	}
	for name, call := range calls {
		for _, tc := range []struct {
			err  error
			want int
		}{
			{errors.New("boom"), http.StatusInternalServerError},
			{unavailable{}, http.StatusServiceUnavailable},
			{auth.ErrStoreStale, http.StatusServiceUnavailable},
			{auth.ErrKeyPairNotFound, http.StatusNotFound},
		} {
			h := &Handler{keyStore: failingKeyStore{err: tc.err}, iam: auth.DefaultIAMFeatures()}
			w := httptest.NewRecorder()
			call(h, w)
			if w.Code != tc.want {
				t.Errorf("%s with %v: status %d, want %d", name, tc.err, w.Code, tc.want)
			}
		}
	}
}
```

`internal/auth/jwks_stale_test.go`:

```go
package auth_test

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/cyoda-platform/cyoda-go/internal/auth"
)

type staleKeyStore struct{ auth.KeyStore }

func (staleKeyStore) Published() ([]*auth.KeyPair, error) { return nil, auth.ErrStoreStale }

func TestJWKS_StaleAnswers503WithRetryAfter(t *testing.T) {
	h := auth.NewJWKSHandler(staleKeyStore{}, 30*time.Second)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/.well-known/jwks.json", nil))
	if w.Code != http.StatusServiceUnavailable || w.Header().Get("Retry-After") != "30" {
		t.Fatalf("status %d, Retry-After %q", w.Code, w.Header().Get("Retry-After"))
	}
}
```

- [ ] **Step 2: Run and see them fail**

Run: `go test ./internal/domain/account/ -run TestKeyPairAdapters_StoreErrorsAreNot404` and `go test ./internal/auth/ -run TestJWKS_Stale`
Expected: build failure — `failingKeyStore` does not implement the old `auth.KeyStore`; `NewJWKSHandler` has one parameter.

- [ ] **Step 3: Implement**

`store.go`: replace `KeyPair` and `KeyStore` with the definitions above; delete `InMemoryKeyStore` and its methods, `InMemoryTrustedKeyStore` and its methods, `NewInMemoryTrustedKeyStore`, `NewInMemoryTrustedKeyStoreWithCap`. Keep `graceExpiry`, `capReached`, `windowOpen`, `errTrustedKeyCapReached`, `RotateOptions`, the M2M types and store.

`service.go` — rewrite:

```go
package auth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

type AuthConfig struct {
	SigningKeyPEM     string
	Issuer            string
	ExpirySeconds     int
	IAMFeatures       IAMFeatures
	KV                spi.KeyValueStore      // SYSTEM-tenant KV store; required
	Broadcaster       spi.ClusterBroadcaster // nil on a single node
	ReconcileInterval time.Duration
	TrustedKeyMetrics ReconcileMetrics
	SigningKeyMetrics ReconcileMetrics
}

type AuthService struct {
	keyStore     *KVKeyStore
	trustedStore *KVTrustedKeyStore
	m2mStore     *InMemoryM2MClientStore
	issuer       string
	handler      http.Handler
	adminHandler http.Handler
}

// NewAuthService builds and loads both key stores; a failed load fails.
// Start runs their re-read loops.
func NewAuthService(ctx context.Context, config AuthConfig) (*AuthService, error) {
	if config.IAMFeatures == (IAMFeatures{}) {
		config.IAMFeatures = DefaultIAMFeatures()
	}
	if err := config.IAMFeatures.Validate(); err != nil {
		return nil, fmt.Errorf("invalid IAM features: %w", err)
	}
	if config.KV == nil {
		return nil, errors.New("auth service requires a KV store")
	}
	privateKey, err := ParseRSAPrivateKeyFromPEM([]byte(config.SigningKeyPEM))
	if err != nil {
		return nil, fmt.Errorf("failed to parse signing key: %w", err)
	}
	trustedOpts := []KVTrustedKeyStoreOption{
		WithMaxTrustedKeys(config.IAMFeatures.TrustedKeyMaxPerTenant),
		WithReconcileInterval(config.ReconcileInterval),
	}
	if config.TrustedKeyMetrics != nil {
		trustedOpts = append(trustedOpts, WithReconcileMetrics(config.TrustedKeyMetrics))
	}
	if config.Broadcaster != nil {
		trustedOpts = append(trustedOpts, WithTrustedKeyBroadcaster(config.Broadcaster))
	}
	trustedStore, err := NewKVTrustedKeyStore(ctx, config.KV, trustedOpts...)
	if err != nil {
		return nil, err
	}
	keyStore, err := NewKVKeyStore(ctx, config.KV, KVKeyStoreConfig{
		Bootstrap: privateKey, BootstrapAudience: config.IAMFeatures.BootstrapAudience,
		Broadcaster: config.Broadcaster, ReconcileInterval: config.ReconcileInterval,
		Metrics: config.SigningKeyMetrics,
	})
	if err != nil {
		return nil, err
	}
	m2mStore := NewInMemoryM2MClientStore()
	retryAfter := config.ReconcileInterval
	if retryAfter <= 0 {
		retryAfter = defaultReconcileInterval
	}
	publicMux := http.NewServeMux()
	publicMux.Handle("GET /.well-known/jwks.json", NewJWKSHandler(keyStore, retryAfter))
	publicMux.Handle("POST /oauth/token", NewTokenHandler(keyStore, trustedStore, m2mStore, config.Issuer, config.ExpirySeconds))
	return &AuthService{
		keyStore: keyStore, trustedStore: trustedStore, m2mStore: m2mStore,
		issuer: config.Issuer, handler: publicMux, adminHandler: http.NewServeMux(),
	}, nil
}

// Start runs both stores' re-read loops until ctx ends.
func (s *AuthService) Start(ctx context.Context) {
	s.trustedStore.StartReconcileLoop(ctx)
	s.keyStore.Start(ctx)
}

func (s *AuthService) Handler() http.Handler                 { return s.handler }
func (s *AuthService) AdminHandler() http.Handler            { return s.adminHandler }
func (s *AuthService) Issuer() string                        { return s.issuer }
func (s *AuthService) KeyStore() KeyStore                    { return s.keyStore }
func (s *AuthService) TrustedKeyStore() TrustedKeyStore      { return s.trustedStore }
func (s *AuthService) M2MClientStore() M2MClientStore        { return s.m2mStore }
func (s *AuthService) SigningKID() string                    { return s.keyStore.BootstrapKID() }
```

Keep the existing doc comments of `Handler`, `AdminHandler`, `Issuer`, `KeyStore`, `TrustedKeyStore`, `M2MClientStore`, `SigningKID`.

`token.go`: in both grants replace the `GetActive` + `Sign` pair with:

```go
	kp, signer, err := h.keyStore.Signer("client")
	if err != nil {
		writeTokenServerError(w, "keyStore.Signer", err)
		return
	}
	...
	token, err := Sign(r.Context(), claims, signer, kp.KID)
```

`key_source.go` — `localKeySource.GetKey`:

```go
func (s *localKeySource) GetKey(kid string) (*rsa.PublicKey, error) {
	pub, err := s.ks.VerificationKey(kid)
	if err != nil {
		// Double %w: callers match ErrKeyNotFound; the store error is diagnostic.
		return nil, fmt.Errorf("%w (kid=%q): %w", ErrKeyNotFound, kid, err)
	}
	return pub, nil
}
```

(Remove the now-unused `time` and `errors` imports if nothing else uses them.)

`jwks.go`:

```go
type JWKSHandler struct {
	keyStore   KeyStore
	retryAfter time.Duration
}

// NewJWKSHandler serves the key set; while the store is stale it answers 503
// with Retry-After instead of an empty set, which would make external
// verifiers drop their cached keys.
func NewJWKSHandler(keyStore KeyStore, retryAfter time.Duration) *JWKSHandler {
	return &JWKSHandler{keyStore: keyStore, retryAfter: retryAfter}
}

func (h *JWKSHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	allKeys, err := h.keyStore.Published()
	if err != nil {
		w.Header().Set("Retry-After", strconv.Itoa(int(h.retryAfter.Seconds())))
		common.WriteError(w, r, common.Internal("jwks", err))
		return
	}
	// ... unchanged: build entries from allKeys and encode.
}
```

(`jwks.go` gains imports `strconv`, `time` and `internal/common`; `internal/auth` already imports `internal/common`, `store.go:16`.)

`keys_adapter.go` — add the error mapper and rewrite the handlers' store calls:

```go
// keyPairError: not found keeps 404 KEYPAIR_NOT_FOUND; every other failure
// goes through common.Internal (500 with a ticket, or 503 when storage is
// unavailable or the node's copy is stale).
func keyPairError(err error) *common.AppError {
	if errors.Is(err, auth.ErrKeyPairNotFound) {
		return common.Operational(http.StatusNotFound, common.ErrCodeKeypairNotFound, "key pair not found")
	}
	return common.Internal("key-pair store", err)
}
```

- `IssueJwtKeyPair`: keep all validation (`:33-94`); delete the `rsa.GenerateKey`/KID block (`:96-111`); call

```go
	kp, err := h.keyStore.Issue(r.Context(), auth.IssueRequest{
		Audience: string(req.Audience), ValidFrom: validFrom, ValidTo: validTo,
		Invalidate: invalidate, GracePeriodSec: grace,
	})
	if err != nil {
		common.WriteError(w, r, keyPairError(err))
		return
	}
```

- `GetCurrentJwtKeyPair`: `kp, err := h.keyStore.Current(string(params.Audience))`; on `ErrKeyPairNotFound` keep the message `"no active key pair for audience"`; otherwise `keyPairError(err)`.
- `DeleteJwtKeyPair`: `h.keyStore.Delete(r.Context(), keyId)` → `keyPairError`.
- `InvalidateJwtKeyPair`: `h.keyStore.Invalidate(r.Context(), keyId, grace)` → `keyPairError`.
- `ReactivateJwtKeyPair`: `kp, err := h.keyStore.Reactivate(r.Context(), keyId, validFrom, validTo)` → `keyPairError`; delete the follow-up `Get`.
- Remove unused imports (`crypto/rand`, `crypto/rsa`, `encoding/hex`).

`app/app.go`:
- Delete `:285-309` (the reconcile metrics, `trustedOpts`, `NewKVTrustedKeyStore`, `StartReconcileLoop`). Keep `systemCtx` and `kvStore` (`:271-284`; the OIDC store uses `kvStore`) and the D7 broadcaster check (`:311-318`).
- Replace the `NewAuthService` call at `:372-382` (it already sits after the OIDC bootstrap and before its first use, the validator at `:388`) with:

```go
		trustedMetrics, err := auth.NewOTelReconcileMetrics(observability.Meter(), "auth.trustedkeys")
		if err != nil { slog.Error("startup failure", "phase", "auth-reconcile-metrics-init", "error", err.Error()); os.Exit(1) }
		signingMetrics, err := auth.NewOTelReconcileMetrics(observability.Meter(), "auth.signingkeys")
		if err != nil { slog.Error("startup failure", "phase", "auth-reconcile-metrics-init", "error", err.Error()); os.Exit(1) }
		var authBroadcaster spi.ClusterBroadcaster
		if gossipReg != nil {
			authBroadcaster = gossipReg // typed-nil guard, as for the cache broadcaster
		}
		authSvc, err = auth.NewAuthService(systemCtx, auth.AuthConfig{
			SigningKeyPEM: cfg.IAM.JWTSigningKey, Issuer: cfg.IAM.JWTIssuer, ExpirySeconds: cfg.IAM.JWTExpiry,
			IAMFeatures: cfg.IAM.AuthIAMFeatures(), KV: kvStore, Broadcaster: authBroadcaster,
			ReconcileInterval: cfg.IAM.AuthCacheReconcileInterval,
			TrustedKeyMetrics: trustedMetrics, SigningKeyMetrics: signingMetrics,
		})
		if err != nil { slog.Error("startup failure", "phase", "auth-service", "error", err.Error()); os.Exit(1) }
		authSvc.Start(systemCtx)
```

- `:611-617`: unchanged (`authSvc.KeyStore()` now returns the new interface).

`api/openapi.yaml`: after each `"500":` block of the five operations under `/oauth/keys/keypair` (`POST`), `/oauth/keys/keypair/current` (`GET`), `/oauth/keys/keypair/{keyId}` (`DELETE`), `/oauth/keys/keypair/{keyId}/invalidate` (`POST`), `/oauth/keys/keypair/{keyId}/reactivate` (`POST`), add:

```yaml
        "503":
          $ref: '#/components/responses/ServiceUnavailable'
```

Then `go generate ./api` and check `git diff --stat api/` shows only the regenerated spec embedding.

Tests: replace every use of the deleted stores with the KV-backed ones over the memory plugin, and every `NewAuthService(auth.AuthConfig{...})` with `NewAuthService(ctx, auth.AuthConfig{..., KV: <memory KV of the SYSTEM tenant>})`. Add to `internal/auth/test_helpers_test.go`:

```go
// newTestKeyStore is the signing-key store every package test uses.
func newTestKeyStore(t *testing.T, boot *rsa.PrivateKey) *auth.KVKeyStore {
	t.Helper()
	s, err := auth.NewKVKeyStore(systemCtx(), mustNewMemoryKV(t, systemCtx()),
		auth.KVKeyStoreConfig{Bootstrap: boot, BootstrapAudience: "client"})
	if err != nil {
		t.Fatal(err)
	}
	return s
}
```

Tests that asserted `InMemoryKeyStore` internals (`store_test.go` key-pair section, `keypair_signing_test.go`) are rewritten against `KVKeyStore`'s public methods with the same behavioural assertions (issue-ahead does not sign, latest `validFrom` wins, tie → greater KID, window ends verification); delete assertions that only exercised the deleted type's map handling.

- [ ] **Step 4: Run the tests and the exit checks**

Run: `go test ./internal/... ./app/...` then `go vet ./...` then:

```bash
grep -rn "InMemoryKeyStore\|InMemoryTrustedKeyStore" --include='*.go' .
grep -rn "rsa.PrivateKey" internal --include='*.go' | grep -v _test
grep -rn "rsa.GenerateKey" internal --include='*.go' | grep -v _test
```

Expected: tests PASS; vet clean; the first grep is empty; the second lists only `jwt.go` (PEM parsing), `signer.go`, `key_vault.go`, `kv_key_store.go` (config field); the third lists only `key_vault.go`.

- [ ] **Step 5: Run `make test`**

Run: `make test`
Expected: green (unit + cross-backend parity). The OpenAPI conformance validator accepts the new 503 responses.

- [ ] **Step 6: Commit**

```bash
git add -A internal app api
git commit -m "feat(auth)!: signing key pairs shared by the cluster and persisted

Key pairs issued, invalidated, reactivated or deleted on any node now take
effect on every node and survive a restart; a revoked bootstrap key stays
revoked. Key-pair store failures answer 500/503 instead of 404.

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 9: In-process end-to-end tests (PostgreSQL)

**Files:**
- Modify: `internal/e2e/callback_harness_test.go:186-205` (split key generation out)
- Create: `internal/e2e/signing_keys_test.go`

**Interfaces:**
- Consumes: Task 8 behaviour; harness helpers `newSchedDB` (`scheduler_harness_test.go:41`), `h.fetchToken`, `h.grpcCtxAs` (`callout_scenarios_helpers_test.go:222`), `h.apiConn`, `h.baseURL`.
- Produces (test helpers): `func newCalloutHarnessWithKey(t *testing.T, key *rsa.PrivateKey, configure func(*app.Config)) *callbackHarness`; `func newKeyStackOn(t *testing.T, s *schedDB, key *rsa.PrivateKey) *callbackHarness`.

- [ ] **Step 1: Split the harness** — in `callback_harness_test.go`, `newCalloutHarness` becomes:

```go
func newCalloutHarness(t *testing.T, configure func(*app.Config)) *callbackHarness {
	t.Helper()
	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate RSA key: %v", err)
	}
	return newCalloutHarnessWithKey(t, rsaKey, configure)
}

// newCalloutHarnessWithKey is newCalloutHarness with a given JWT signing key,
// so a test can restart a stack with the same bootstrap key.
func newCalloutHarnessWithKey(t *testing.T, rsaKey *rsa.PrivateKey, configure func(*app.Config)) *callbackHarness {
	t.Helper()
	keyBytes, err := x509.MarshalPKCS8PrivateKey(rsaKey)
	// ... the rest of the current body, unchanged; h.signKey = rsaKey as today.
}
```

Add to `signing_keys_test.go`:

```go
// newKeyStackOn opens a stack on s's database with the given bootstrap key —
// a restart of a node, or a node configured with another key.
func newKeyStackOn(t *testing.T, s *schedDB, key *rsa.PrivateKey) *callbackHarness {
	t.Helper()
	return newCalloutHarnessWithKey(t, key, func(cfg *app.Config) {
		t.Setenv("CYODA_POSTGRES_URL", s.url)
	})
}
```

- [ ] **Step 2: Write the tests** — `internal/e2e/signing_keys_test.go` (also helpers for the calls):

```go
package e2e_test

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	cyodapb "github.com/cyoda-platform/cyoda-go/api/grpc/cyoda"
	"github.com/cyoda-platform/cyoda-go/app"
	"github.com/cyoda-platform/cyoda-go/internal/auth"
	internalgrpc "github.com/cyoda-platform/cyoda-go/internal/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func (h *callbackHarness) keyCall(t *testing.T, method, path, body string) (int, []byte) {
	t.Helper()
	req, _ := http.NewRequest(method, h.baseURL+"/api"+path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+h.fetchToken(t))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

func (h *callbackHarness) issueKey(t *testing.T, aud string, invalidate bool) string {
	t.Helper()
	body := `{"algorithm":"RS256","audience":"` + aud + `"` + map[bool]string{true: `,"invalidateCurrent":true`, false: ""}[invalidate] + `}`
	code, b := h.keyCall(t, "POST", "/oauth/keys/keypair", body)
	if code != http.StatusOK {
		t.Fatalf("issue: %d %s", code, b)
	}
	var out struct{ KeyId string }
	_ = json.Unmarshal(b, &out)
	return out.KeyId
}

func tokenKID(t *testing.T, tok string) string {
	t.Helper()
	p, err := auth.Parse(tok)
	if err != nil {
		t.Fatal(err)
	}
	kid, _ := p.Header["kid"].(string)
	return kid
}

func (h *callbackHarness) authedStatus(t *testing.T, tok string) int {
	t.Helper()
	req, _ := http.NewRequest("GET", h.baseURL+"/api/model/", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

func (h *callbackHarness) jwksKIDs(t *testing.T) map[string]bool {
	t.Helper()
	resp, err := http.Get(h.baseURL + "/api/.well-known/jwks.json")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var set struct{ Keys []struct{ Kid string } }
	_ = json.NewDecoder(resp.Body).Decode(&set)
	out := map[string]bool{}
	for _, k := range set.Keys {
		out[k.Kid] = true
	}
	return out
}

func bootstrapToken(t *testing.T, key *rsa.PrivateKey) string {
	t.Helper()
	kid, _ := auth.DeriveKID(&key.PublicKey)
	now := time.Now()
	tok, err := auth.Sign(context.Background(), map[string]any{
		"sub": "boot-user", "iss": "cyoda-callback-test", "caas_user_id": "boot-user",
		"caas_org_id": "test-tenant", "scopes": []string{"ROLE_ADMIN"}, "caas_tier": "unlimited",
		"exp": now.Add(time.Hour).Unix(), "iat": now.Unix(), "jti": "j-" + kid[:8],
	}, auth.NewRSASigner(key), kid)
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

func genKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func TestSigningKeys_IssuedPairSurvivesRestart(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: requires Docker + PostgreSQL")
	}
	s, key := newSchedDB(t), genKey(t)
	h1 := newKeyStackOn(t, s, key)
	kid := h1.issueKey(t, "client", false)
	h2 := newKeyStackOn(t, s, key)
	tok := h2.fetchToken(t)
	if tokenKID(t, tok) != kid {
		t.Fatalf("restarted node signs with %s, want the issued %s", tokenKID(t, tok), kid)
	}
	if code := h2.authedStatus(t, tok); code != http.StatusOK {
		t.Fatalf("token from the issued key: %d", code)
	}
	if !h2.jwksKIDs(t)[kid] {
		t.Fatal("issued key missing from JWKS after restart")
	}
}

func TestSigningKeys_BootstrapRevocationSurvivesRestart(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: requires Docker + PostgreSQL")
	}
	s, key := newSchedDB(t), genKey(t)
	bootKID, _ := auth.DeriveKID(&key.PublicKey)
	h1 := newKeyStackOn(t, s, key)
	h1.issueKey(t, "client", false) // tokens now come from an issued key
	if code, b := h1.keyCall(t, "POST", "/oauth/keys/keypair/"+bootKID+"/invalidate", ""); code != http.StatusOK {
		t.Fatalf("invalidate bootstrap: %d %s", code, b)
	}
	h2 := newKeyStackOn(t, s, key)
	if code := h2.authedStatus(t, bootstrapToken(t, key)); code != http.StatusUnauthorized {
		t.Fatalf("invalidated bootstrap key accepted after restart: %d", code)
	}
	if code, b := h2.keyCall(t, "DELETE", "/oauth/keys/keypair/"+bootKID, ""); code != http.StatusOK {
		t.Fatalf("delete bootstrap: %d %s", code, b)
	}
	h3 := newKeyStackOn(t, s, key)
	future := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	if code, _ := h3.keyCall(t, "POST", "/oauth/keys/keypair/"+bootKID+"/reactivate", `{"validTo":"`+future+`"}`); code != http.StatusNotFound {
		t.Fatalf("reactivate after delete: %d, want 404", code)
	}
	if code := h3.authedStatus(t, bootstrapToken(t, key)); code != http.StatusUnauthorized {
		t.Fatalf("deleted bootstrap key accepted after restart: %d", code)
	}
}

func TestSigningKeys_AnotherBootstrapKeyRetiresIssuedPairs(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: requires Docker + PostgreSQL")
	}
	s := newSchedDB(t)
	h1 := newKeyStackOn(t, s, genKey(t))
	kid := h1.issueKey(t, "client", false)
	key2 := genKey(t)
	h2 := newKeyStackOn(t, s, key2)
	boot2, _ := auth.DeriveKID(&key2.PublicKey)
	if got := tokenKID(t, h2.fetchToken(t)); got != boot2 {
		t.Fatalf("node with a new bootstrap key signs with %s, want its bootstrap %s", got, boot2)
	}
	if h2.jwksKIDs(t)[kid] {
		t.Fatal("retired key published")
	}
	for _, c := range []struct{ method, path, body string }{
		{"POST", "/oauth/keys/keypair/" + kid + "/invalidate", ""},
		{"DELETE", "/oauth/keys/keypair/" + kid, ""},
	} {
		if code, _ := h2.keyCall(t, c.method, c.path, c.body); code != http.StatusNotFound {
			t.Fatalf("%s %s on a retired key: %d, want 404", c.method, c.path, code)
		}
	}
}

func TestSigningKeys_BrokenSignerFailsClosed(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: requires Docker + PostgreSQL")
	}
	s, key := newSchedDB(t), genKey(t)
	h1 := newKeyStackOn(t, s, key)
	kid := h1.issueKey(t, "client", false)
	var raw []byte
	ctx := context.Background()
	if err := s.pool.QueryRow(ctx, `SELECT value FROM kv_store WHERE tenant_id='SYSTEM' AND namespace='signing-keys' AND key=$1`, kid).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var rec map[string]any
	_ = json.Unmarshal(raw, &rec)
	vault := rec["vault"].(map[string]any)
	sealed, _ := base64.StdEncoding.DecodeString(vault["sealed"].(string))
	sealed[len(sealed)-1] ^= 1
	vault["sealed"] = base64.StdEncoding.EncodeToString(sealed)
	tampered, _ := json.Marshal(rec)
	if _, err := s.pool.Exec(ctx, `UPDATE kv_store SET value=$1 WHERE tenant_id='SYSTEM' AND namespace='signing-keys' AND key=$2`, tampered, kid); err != nil {
		t.Fatal(err)
	}
	h2 := newKeyStackOn(t, s, key)
	// /oauth/token must fail closed: the broken key is the selected signer.
	req, _ := http.NewRequest("POST", h2.baseURL+"/api/oauth/token", strings.NewReader("grant_type=client_credentials"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetBasicAuth("testclient", "testsecret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("/oauth/token with a broken signer: %d, want 500", resp.StatusCode)
	}
}
```

The harness seeds a cached token at the end of construction (`h.token(t)`), which fails on a stack whose signer is broken. So in Step 1 the body of `newCalloutHarnessWithKey` moves into `newCalloutHarnessUnseeded(t, key, configure)` — everything except the final `h.token(t)` — and `newCalloutHarnessWithKey` becomes `h := newCalloutHarnessUnseeded(t, rsaKey, configure); h.token(t); return h`. Add next to `newKeyStackOn`:

```go
func newKeyStackOnUnseeded(t *testing.T, s *schedDB, key *rsa.PrivateKey) *callbackHarness {
	t.Helper()
	return newCalloutHarnessUnseeded(t, key, func(cfg *app.Config) {
		t.Setenv("CYODA_POSTGRES_URL", s.url)
	})
}
```

In `TestSigningKeys_BrokenSignerFailsClosed` use `h2 := newKeyStackOnUnseeded(t, s, key)`, and after the token assertion add:

```go
	req, _ = http.NewRequest("GET", h2.baseURL+"/api/oauth/keys/keypair/current?audience=client", nil)
	req.Header.Set("Authorization", "Bearer "+bootstrapToken(t, key))
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("current with a broken signer: %d, want 500", resp.StatusCode)
	}
```

Add the stored-bytes and gRPC tests:

```go
func TestSigningKeys_StoredValueHasNoPrivateKey(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: requires Docker + PostgreSQL")
	}
	s, key := newSchedDB(t), genKey(t)
	h := newKeyStackOn(t, s, key)
	kid := h.issueKey(t, "client", false)
	var raw []byte
	if err := s.pool.QueryRow(context.Background(), `SELECT value FROM kv_store WHERE namespace='signing-keys' AND key=$1`, kid).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	for _, marker := range []string{"PRIVATE KEY", "MIIE"} {
		if strings.Contains(string(raw), marker) {
			t.Fatalf("stored record contains %q", marker)
		}
	}
	var rec struct{ PublicKey string }
	_ = json.Unmarshal(raw, &rec)
	spki, _ := base64.StdEncoding.DecodeString(rec.PublicKey)
	if _, err := x509.ParsePKIXPublicKey(spki); err != nil {
		t.Fatal("public key not stored in the clear")
	}
}

func TestSigningKeys_GRPCFollowsKeyState(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: requires Docker + PostgreSQL")
	}
	h := newCalloutHarness(t, nil)
	kid := h.issueKey(t, "client", false)
	tok := h.fetchToken(t)
	if tokenKID(t, tok) != kid {
		t.Fatal("token not signed with the issued key")
	}
	call := func() error {
		ce, _ := internalgrpc.NewCloudEvent(internalgrpc.EntityGetRequest, map[string]any{"id": "sk", "entityId": "00000000-0000-0000-0000-000000000000"})
		_, err := cyodapb.NewCloudEventsServiceClient(h.apiConn).EntitySearch(h.grpcCtxAs(tok, ""), ce)
		return err
	}
	if err := call(); status.Code(err) == codes.Unauthenticated {
		t.Fatalf("token from an issued key refused on gRPC: %v", err)
	}
	if code, b := h.keyCall(t, "POST", "/oauth/keys/keypair/"+kid+"/invalidate", ""); code != http.StatusOK {
		t.Fatalf("invalidate: %d %s", code, b)
	}
	if err := call(); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("invalidated key accepted on gRPC: %v", err)
	}
}
```

(`keyCall` fetches a fresh token each call, so after the invalidate it gets a bootstrap-signed one. The `cyodapb` import path matches `callback_txjoin_grpc_bounds_test.go:13`.)

- [ ] **Step 3: Run the tests**

Run: `go test ./internal/e2e/ -run TestSigningKeys`
Expected: PASS. Each test proves its point against a real PostgreSQL. Before Task 8 they would fail: confirm by checking out Task 7's commit and running one test (`TestSigningKeys_IssuedPairSurvivesRestart` fails: the restarted node signs with the bootstrap key).

- [ ] **Step 4: Commit**

```bash
git add internal/e2e/callback_harness_test.go internal/e2e/signing_keys_test.go
git commit -m "test(e2e): signing key pairs survive restart, retire on a new bootstrap key, fail closed, follow state on gRPC

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 10: Single-node parity scenario (memory, sqlite, postgres, cassandra)

**Files:**
- Create: `e2e/parity/client/keys.go`, `e2e/parity/signing_keys.go`
- Modify: `e2e/parity/registry.go` (one entry)

**Interfaces:**
- Produces (client):

```go
func (c *Client) IssueKeyPairRaw(t *testing.T, body map[string]any) (int, []byte, error)
func (c *Client) CurrentKeyPairRaw(t *testing.T, audience string) (int, []byte, error)
func (c *Client) InvalidateKeyPairRaw(t *testing.T, kid string) (int, []byte, error)
func (c *Client) ReactivateKeyPairRaw(t *testing.T, kid string, validTo time.Time) (int, []byte, error)
func (c *Client) DeleteKeyPairRaw(t *testing.T, kid string) (int, []byte, error)
func (c *Client) JWKSKIDs(t *testing.T) (map[string]bool, error)
func (c *Client) CreateClientRaw(t *testing.T, withAdminRole bool) (int, []byte, error)
func FetchClientCredentialsToken(ctx context.Context, baseURL, clientID, secret string) (string, int, error)
```

- [ ] **Step 1: Write the client helpers** — `e2e/parity/client/keys.go`:

```go
package client

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

func (c *Client) IssueKeyPairRaw(t *testing.T, body map[string]any) (int, []byte, error) {
	t.Helper()
	return c.DoJSONBodyRaw(t, http.MethodPost, "/api/oauth/keys/keypair", body)
}

func (c *Client) CurrentKeyPairRaw(t *testing.T, audience string) (int, []byte, error) {
	t.Helper()
	return c.DoJSONBodyRaw(t, http.MethodGet, "/api/oauth/keys/keypair/current?audience="+url.QueryEscape(audience), nil)
}

func (c *Client) InvalidateKeyPairRaw(t *testing.T, kid string) (int, []byte, error) {
	t.Helper()
	return c.DoJSONBodyRaw(t, http.MethodPost, "/api/oauth/keys/keypair/"+url.PathEscape(kid)+"/invalidate", nil)
}

func (c *Client) ReactivateKeyPairRaw(t *testing.T, kid string, validTo time.Time) (int, []byte, error) {
	t.Helper()
	return c.DoJSONBodyRaw(t, http.MethodPost, "/api/oauth/keys/keypair/"+url.PathEscape(kid)+"/reactivate",
		map[string]any{"validTo": validTo.UTC().Format(time.RFC3339)})
}

func (c *Client) DeleteKeyPairRaw(t *testing.T, kid string) (int, []byte, error) {
	t.Helper()
	return c.DoJSONBodyRaw(t, http.MethodDelete, "/api/oauth/keys/keypair/"+url.PathEscape(kid), nil)
}

// JWKSKIDs returns the KIDs the public JWKS endpoint publishes.
func (c *Client) JWKSKIDs(t *testing.T) (map[string]bool, error) {
	t.Helper()
	code, body, err := c.DoJSONBodyRaw(t, http.MethodGet, "/api/.well-known/jwks.json", nil)
	if err != nil {
		return nil, err
	}
	if code != http.StatusOK {
		return nil, fmt.Errorf("jwks: status %d", code)
	}
	var set struct {
		Keys []struct {
			Kid string `json:"kid"`
		} `json:"keys"`
	}
	if err := json.Unmarshal(body, &set); err != nil {
		return nil, err
	}
	out := map[string]bool{}
	for _, k := range set.Keys {
		out[k.Kid] = true
	}
	return out, nil
}

// CreateClientRaw creates an M2M client of the caller's tenant.
func (c *Client) CreateClientRaw(t *testing.T, withAdminRole bool) (int, []byte, error) {
	t.Helper()
	path := "/api/clients"
	if withAdminRole {
		path += "?withAdminRole=true"
	}
	return c.DoJSONBodyRaw(t, http.MethodPost, path, nil)
}

// FetchClientCredentialsToken runs the client_credentials grant against baseURL.
// The token is a credential: never log it.
func FetchClientCredentialsToken(ctx context.Context, baseURL, clientID, secret string) (string, int, error) {
	form := url.Values{"grant_type": {"client_credentials"}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(baseURL, "/")+"/api/oauth/token", strings.NewReader(form.Encode()))
	if err != nil {
		return "", 0, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetBasicAuth(clientID, secret)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", 0, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return "", resp.StatusCode, nil
	}
	var out struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return "", resp.StatusCode, err
	}
	return out.AccessToken, resp.StatusCode, nil
}
```

- [ ] **Step 2: Write the scenario** — `e2e/parity/signing_keys.go`:

```go
package parity

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/cyoda-platform/cyoda-go/e2e/parity/client"
)

// RunSigningKeyPairLifecycle drives one key pair through issue, current,
// JWKS, invalidate, reactivate and delete on every backend. It uses audience
// human — nothing signs with it — and never invalidateCurrent, so the shared
// server's own tokens are untouched.
func RunSigningKeyPairLifecycle(t *testing.T, fixture BackendFixture) {
	tenant := fixture.NewTenant(t)
	c := client.NewClient(fixture.BaseURL(), tenant.Token)

	code, body, err := c.IssueKeyPairRaw(t, map[string]any{"algorithm": "RS256", "audience": "human"})
	if err != nil || code != http.StatusOK {
		t.Fatalf("issue: %d %s %v", code, body, err)
	}
	var kp struct {
		KeyId  string `json:"keyId"`
		Active bool   `json:"active"`
	}
	_ = json.Unmarshal(body, &kp)
	t.Cleanup(func() { _, _, _ = c.DeleteKeyPairRaw(t, kp.KeyId) })

	if code, body, _ := c.CurrentKeyPairRaw(t, "human"); code != http.StatusOK || !jsonHasKID(body, kp.KeyId) {
		t.Fatalf("current: %d %s", code, body)
	}
	if kids, err := c.JWKSKIDs(t); err != nil || !kids[kp.KeyId] {
		t.Fatalf("JWKS missing the issued key: %v", err)
	}
	if code, body, _ := c.InvalidateKeyPairRaw(t, kp.KeyId); code != http.StatusOK {
		t.Fatalf("invalidate: %d %s", code, body)
	}
	if code, _, _ := c.CurrentKeyPairRaw(t, "human"); code != http.StatusNotFound {
		t.Fatalf("current after invalidate: %d, want 404", code)
	}
	if code, body, _ := c.ReactivateKeyPairRaw(t, kp.KeyId, time.Now().Add(time.Hour)); code != http.StatusOK || !jsonHasKID(body, kp.KeyId) {
		t.Fatalf("reactivate: %d %s", code, body)
	}
	if code, body, _ := c.DeleteKeyPairRaw(t, kp.KeyId); code != http.StatusOK {
		t.Fatalf("delete: %d %s", code, body)
	}
	if code, body, _ := c.DeleteKeyPairRaw(t, kp.KeyId); code != http.StatusNotFound || !jsonHasCode(body, "KEYPAIR_NOT_FOUND") {
		t.Fatalf("second delete: %d %s, want 404 KEYPAIR_NOT_FOUND", code, body)
	}
}

func jsonHasKID(body []byte, kid string) bool {
	var v struct {
		KeyId string `json:"keyId"`
	}
	return json.Unmarshal(body, &v) == nil && v.KeyId == kid
}

func jsonHasCode(body []byte, code string) bool {
	var v struct {
		ErrorCode string `json:"errorCode"`
	}
	return json.Unmarshal(body, &v) == nil && v.ErrorCode == code
}
```

(The problem body carries the code as `errorCode`, `internal/common/errors.go:427`. If `e2e/parity` already has a helper that reads it, use that instead.)

Register in `e2e/parity/registry.go` (in the IAM section, or at the end of `allTests`):

```go
	{"SigningKeyPairLifecycle", RunSigningKeyPairLifecycle},
```

- [ ] **Step 3: Run it on the in-tree backends**

Run: `make test` (runs the parity suites uncached against memory, sqlite and postgres).
Expected: `SigningKeyPairLifecycle` passes on all three. TDD note: this scenario is a characterisation of the contract, not the failing test for the change — it also passes on the in-memory store before Task 8. Its job is cross-backend parity: the lifecycle now round-trips through each plugin's KV store. The failing-first proofs of sharing and persistence are Tasks 7, 9 and 11.

- [ ] **Step 4: Commit**

```bash
git add e2e/parity/client/keys.go e2e/parity/signing_keys.go e2e/parity/registry.go
git commit -m "test(parity): signing key-pair lifecycle on every backend

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 11: Multi-node tests (PostgreSQL)

**Files:**
- Create: `e2e/parity/multinode/signing_keys.go` (shared-cluster scenario, registered), `e2e/parity/postgres/signing_keys_cluster_test.go` (own cluster)
- Modify: `e2e/parity/multinode/registry.go:1-10` (the comment says cassandra runs these scenarios; it does not)

**Interfaces:**
- Consumes: Task 10 client helpers; `MustSetupMultiNodeWithEnv` (`e2e/parity/postgres/multinode_fixture.go:199`), `PauseDatabase`/`UnpauseDatabase` (`:137`, `:150`) by type assertion.

- [ ] **Step 1: Shared-cluster scenario** — `e2e/parity/multinode/signing_keys.go`:

```go
package multinode

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/cyoda-platform/cyoda-go/e2e/parity/client"
)

func init() {
	Register(NamedTest{Name: "SigningKeyPairFollowsTheCluster", Fn: RunSigningKeyPairFollowsTheCluster})
}

// eventually polls cond every 200 ms until it holds or the bound passes. The
// bound covers a lost change message: one re-read at the default interval.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(70 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("never happened: %s", what)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func statusWith(t *testing.T, baseURL, token string) int {
	t.Helper()
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, baseURL+"/api/model/", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

// newM2MClient creates a client on one node; M2M clients are still per node,
// so its tokens must be fetched from that node.
func newM2MClient(t *testing.T, c *client.Client) (string, string) {
	t.Helper()
	code, body, err := c.CreateClientRaw(t, false)
	if err != nil || code != http.StatusOK {
		t.Fatalf("create client: %d %v", code, err)
	}
	var cred struct{ ClientId, ClientSecret string }
	_ = json.Unmarshal(body, &cred)
	return cred.ClientId, cred.ClientSecret
}

// RunSigningKeyPairFollowsTheCluster: a key pair issued on node A signs on A
// and verifies on B; invalidate, reactivate and delete on A take effect on B.
// Audience client without invalidateCurrent: the fixture's bootstrap-signed
// tokens stay valid for later scenarios. The key pair is deleted at the end.
func RunSigningKeyPairFollowsTheCluster(t *testing.T, fixture MultiNodeFixture) {
	urls := fixture.BaseURLs()
	tenant := fixture.NewTenant(t)
	a := client.NewClient(urls[0], tenant.Token)
	b := client.NewClient(urls[1], tenant.Token)

	code, body, err := a.IssueKeyPairRaw(t, map[string]any{"algorithm": "RS256", "audience": "client"})
	if err != nil || code != http.StatusOK {
		t.Fatalf("issue on A: %d %v", code, err)
	}
	var kp struct{ KeyId string }
	_ = json.Unmarshal(body, &kp)
	t.Cleanup(func() { _, _, _ = a.DeleteKeyPairRaw(t, kp.KeyId) })

	id, secret := newM2MClient(t, a)
	tok, code, err := client.FetchClientCredentialsToken(context.Background(), urls[0], id, secret)
	if err != nil || code != http.StatusOK {
		t.Fatalf("token from A: %d %v", code, err)
	}
	eventually(t, "B accepts a token signed with the key issued on A", func() bool {
		return statusWith(t, urls[1], tok) == http.StatusOK
	})
	eventually(t, "B publishes the key in JWKS", func() bool {
		kids, err := b.JWKSKIDs(t)
		return err == nil && kids[kp.KeyId]
	})
	idB, secretB := newM2MClient(t, b)
	eventually(t, "B signs with the key issued on A", func() bool {
		tokB, _, err := client.FetchClientCredentialsToken(context.Background(), urls[1], idB, secretB)
		return err == nil && headerKID(tokB) == kp.KeyId
	})

	if code, _, _ := a.InvalidateKeyPairRaw(t, kp.KeyId); code != http.StatusOK {
		t.Fatalf("invalidate on A: %d", code)
	}
	eventually(t, "B refuses the invalidated key", func() bool {
		return statusWith(t, urls[1], tok) == http.StatusUnauthorized
	})
	if code, _, _ := a.ReactivateKeyPairRaw(t, kp.KeyId, time.Now().Add(time.Hour)); code != http.StatusOK {
		t.Fatalf("reactivate on A: %d", code)
	}
	eventually(t, "B accepts the reactivated key", func() bool {
		return statusWith(t, urls[1], tok) == http.StatusOK
	})
	if code, _, _ := a.DeleteKeyPairRaw(t, kp.KeyId); code != http.StatusOK {
		t.Fatalf("delete on A: %d", code)
	}
	eventually(t, "B refuses the deleted key", func() bool {
		return statusWith(t, urls[1], tok) == http.StatusUnauthorized
	})
}

func headerKID(tok string) string {
	parts := strings.SplitN(tok, ".", 3)
	if len(parts) < 2 {
		return ""
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return ""
	}
	var h struct {
		Kid string `json:"kid"`
	}
	_ = json.Unmarshal(raw, &h)
	return h.Kid
}
```

(The import block also needs `encoding/base64` and `strings`.)

- [ ] **Step 2: Own-cluster test** — `e2e/parity/postgres/signing_keys_cluster_test.go`:

```go
package postgres

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/cyoda-platform/cyoda-go/e2e/parity/client"
)

// TestSigningKeys_OwnCluster revokes the bootstrap key, so it runs on its own
// cluster: the shared cluster signs its fixture tokens with that key.
func TestSigningKeys_OwnCluster(t *testing.T) {
	fix, cleanup := MustSetupMultiNodeWithEnv(t, 2, []string{
		"CYODA_AUTH_CACHE_RECONCILE_INTERVAL=1s",
		"CYODA_IAM_M2M_ADMIN_ROLE_ENABLED=true",
	})
	defer cleanup()
	urls := fix.BaseURLs()
	tenant := fix.NewTenant(t) // bootstrap-signed, ROLE_ADMIN
	bootKID := headerKIDOf(tenant.Token)
	a := client.NewClient(urls[0], tenant.Token)

	// Token path before touching the bootstrap key: an issued key and an
	// admin M2M client on A, so later calls do not depend on the bootstrap key.
	issueOn(t, a, false) // K1
	adminID, adminSecret := createAdminClient(t, a)
	t1 := fetch(t, urls[0], adminID, adminSecret)
	waitStatus(t, urls[1], t1, http.StatusOK, "B accepts K1's token")

	t.Run("rotation on A ends the old key and the bootstrap key on B", func(t *testing.T) {
		k2 := issueOn(t, client.NewClient(urls[0], t1), true)
		t2 := fetch(t, urls[0], adminID, adminSecret)
		if headerKIDOf(t2) != k2 {
			t.Fatalf("A signs with %s, want %s", headerKIDOf(t2), k2)
		}
		waitStatus(t, urls[1], t1, http.StatusUnauthorized, "B refuses K1")
		waitStatus(t, urls[1], tenant.Token, http.StatusUnauthorized, "B refuses the bootstrap key")
		waitStatus(t, urls[1], t2, http.StatusOK, "B accepts K2")
	})

	admin := func() *client.Client { return client.NewClient(urls[0], fetch(t, urls[0], adminID, adminSecret)) }

	t.Run("bootstrap reactivate, delete and terminal delete across nodes", func(t *testing.T) {
		code, _, _ := admin().ReactivateKeyPairRaw(t, bootKID, time.Now().Add(time.Hour))
		if code != http.StatusOK {
			t.Fatalf("reactivate bootstrap on A: %d", code)
		}
		waitStatus(t, urls[1], tenant.Token, http.StatusOK, "B accepts the reactivated bootstrap key")
		if code, _, _ := admin().DeleteKeyPairRaw(t, bootKID); code != http.StatusOK {
			t.Fatalf("delete bootstrap on A: %d", code)
		}
		waitStatus(t, urls[1], tenant.Token, http.StatusUnauthorized, "B refuses the deleted bootstrap key")
		if code, _, _ := admin().ReactivateKeyPairRaw(t, bootKID, time.Now().Add(time.Hour)); code != http.StatusNotFound {
			t.Fatalf("reactivate after delete: %d, want 404", code)
		}
		if code, _, _ := admin().InvalidateKeyPairRaw(t, bootKID); code != http.StatusNotFound {
			t.Fatalf("invalidate after delete: %d, want 404", code)
		}
	})

	t.Run("a node that cannot read its database fails closed", func(t *testing.T) {
		pauser, ok := fix.(interface {
			PauseDatabase(*testing.T)
			UnpauseDatabase(*testing.T)
		})
		if !ok {
			t.Fatal("fixture does not expose PauseDatabase")
		}
		tok := fetch(t, urls[0], adminID, adminSecret)
		waitStatus(t, urls[1], tok, http.StatusOK, "B accepts the current key")
		pauser.PauseDatabase(t)
		// 10 × 1 s re-read interval, plus margin.
		time.Sleep(13 * time.Second)
		if code := statusOf(t, urls[1]+"/api/.well-known/jwks.json", ""); code != http.StatusServiceUnavailable {
			t.Errorf("JWKS on a stale node: %d, want 503", code)
		}
		if code := statusOf(t, urls[1]+"/api/model/", tok); code != http.StatusUnauthorized {
			t.Errorf("stale node accepted a token: %d, want 401", code)
		}
		pauser.UnpauseDatabase(t)
		waitStatus(t, urls[1], tok, http.StatusOK, "B recovers after the database returns")
	})
}

func issueOn(t *testing.T, c *client.Client, invalidate bool) string {
	t.Helper()
	body := map[string]any{"algorithm": "RS256", "audience": "client"}
	if invalidate {
		body["invalidateCurrent"] = true
	}
	code, b, err := c.IssueKeyPairRaw(t, body)
	if err != nil || code != http.StatusOK {
		t.Fatalf("issue: %d %v", code, err)
	}
	var kp struct{ KeyId string }
	_ = json.Unmarshal(b, &kp)
	return kp.KeyId
}

func createAdminClient(t *testing.T, c *client.Client) (string, string) {
	t.Helper()
	code, body, err := c.CreateClientRaw(t, true)
	if err != nil || code != http.StatusOK {
		t.Fatalf("create admin client: %d %v", code, err)
	}
	var cred struct{ ClientId, ClientSecret string }
	_ = json.Unmarshal(body, &cred)
	return cred.ClientId, cred.ClientSecret
}

func fetch(t *testing.T, baseURL, id, secret string) string {
	t.Helper()
	tok, code, err := client.FetchClientCredentialsToken(context.Background(), baseURL, id, secret)
	if err != nil || code != http.StatusOK {
		t.Fatalf("token: %d %v", code, err)
	}
	return tok
}

func statusOf(t *testing.T, u, token string) int {
	t.Helper()
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, u, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

func waitStatus(t *testing.T, baseURL, token string, want int, what string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for statusOf(t, baseURL+"/api/model/", token) != want {
		if time.Now().After(deadline) {
			t.Fatalf("never happened: %s", what)
		}
		time.Sleep(200 * time.Millisecond)
	}
}
```

Add `headerKIDOf` (same body as `headerKID` in Step 1, with imports `encoding/base64` and `strings`) to this file.

Correct `e2e/parity/multinode/registry.go:1-10`: the cluster-capable backend running these scenarios is postgres; cassandra's multi-node fixture does not exist yet.

- [ ] **Step 3: Run**

Run: `go test ./e2e/parity/postgres/ -run 'TestMultiNode/SigningKeyPairFollowsTheCluster|TestSigningKeys_OwnCluster'`
Expected: PASS. On Task 7's commit (before the switch-over) `SigningKeyPairFollowsTheCluster` fails at "B accepts a token signed with the key issued on A".

- [ ] **Step 4: Commit**

```bash
git add e2e/parity/multinode e2e/parity/postgres/signing_keys_cluster_test.go
git commit -m "test(multinode): key pairs follow the cluster; bootstrap revocation and staleness on an own cluster

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 12: Cassandra — `List` never returns success with rows missing

Repository `../cyoda-go-cassandra` (private; the "commercial backend"). Work on a branch there; PR into its `main` with the title only as the body convention for courtesy PRs (`fix(kv): List fails instead of returning a partial result`), referencing cyoda-go-cassandra#102.

**Files:**
- Modify: `internal/store/data_store.go:126-162` (`get`), `:210-238` (`list`)
- Test: `internal/integration/data_store_test.go`

- [ ] **Step 1: Write the failing test** — a key whose meta points at a version with no data row is corrupt, not absent:

```go
func TestKVStore_ListFailsOnUnreadableKey(t *testing.T) {
	store, session, tid := newKVTestStore(t) // use the file's existing setup helper
	ctx := tenantCtx(tid)
	if err := store.Put(ctx, "ns-list-fail", "k", []byte("v")); err != nil {
		t.Fatal(err)
	}
	// Point meta at a version that has no data row.
	if err := session.Query(`UPDATE data_store_meta SET current_version = 999 WHERE tenant_id = ? AND store_type = ? AND store_key = ?`,
		string(tid), "kv:ns-list-fail", "k").Exec(); err != nil {
		t.Fatal(err)
	}
	if _, err := store.List(ctx, "ns-list-fail"); err == nil {
		t.Fatal("List returned success with an unreadable key")
	}
	if _, err := store.Get(ctx, "ns-list-fail", "k"); err == nil || errors.Is(err, spi.ErrNotFound) {
		t.Fatalf("Get on a corrupt key: err = %v, want a non-NotFound error", err)
	}
}
```

Adapt the setup lines to the helpers already used by `TestKVStore_List` in the same file (session, tenant id, store construction).

- [ ] **Step 2: Run and see it fail**

Run (in `../cyoda-go-cassandra`): `go test ./internal/integration/ -run TestKVStore_ListFailsOnUnreadableKey`
Expected: FAIL — `List returned success with an unreadable key`.

- [ ] **Step 3: Implement**
- `get`: a missing data row at the meta's current version returns `fmt.Errorf("key %s/%s: data row missing at version %d", …)` — not wrapping `spi.ErrNotFound`.
- `list`: skip a key only when `get` returns an error wrapping `spi.ErrNotFound` (deleted between the listing scan and the read); any other error returns `fmt.Errorf("failed to read %s/%s during list: %w", storeType, storeKey, err)`.

- [ ] **Step 4: Run the cassandra test suite**

Run: the repository's documented full test target (see its `CONTRIBUTING.md`/`Makefile`).
Expected: PASS.

- [ ] **Step 5: Commit, push, open the PR** (in the cassandra repository; follow `reference_github_mechanics`: inline-credential push, never an empty `--body-file`).

---

### Task 13: Documentation (Gate 4 / Gate 7)

**Files:**
- Modify: `cmd/cyoda/help/content/config/auth.md` (`:51-52`, `:180-185`, `:187-197`, `:224-252`), `cmd/cyoda/help/content/auth/tokens.md:121,125`, `cmd/cyoda/help/content/errors.md:84`, `cmd/cyoda/help/content/errors/KEYPAIR_NOT_FOUND.md`, `cmd/cyoda/help/content/errors/NOT_FOUND.md:16,24`, `cmd/cyoda/help/content/helm.md:313`, `cmd/cyoda/help/content/quickstart.md:105`, `cmd/cyoda/help/config_registry.go:87,97,99`, `README.md:169`, `docs/ARCHITECTURE.md` §7.2 (`:1846-1873`), `docs/cloud-parity/signing-key-window.md:30`, `COMPATIBILITY.md`, `CHANGELOG.md`
- Create: `docs/cloud-parity/signing-key-pairs.md`

Compact prose: the actionable core plus a pointer. No issue numbers.

- [ ] **Step 1: `config/auth.md`**
  - `:51-52` (`CYODA_JWT_SIGNING_KEY`): add — "Also derives the key that encrypts stored signing key pairs, so treat it as the root secret. Replacing it retires every issued key pair (see *JWT signing keypair rotation*)."
  - `:180-185`: "The bootstrap signing key has no window unless the key-pair API invalidates or reactivates it; that state is stored and shared by the cluster."
  - §*Auth cache reconciliation* (`:187-197`): "three per-node caches (trusted keys, signing key pairs, OIDC providers)"; add — "A stale signing-key cache refuses first-party tokens (401) and answers JWKS with 503."
  - §*JWT signing keypair rotation* (`:224-252`): replace the *Limitations* list with:
    - **Shared and persisted.** Key pairs, and changes to the bootstrap key's state, are stored and apply on every node; they survive restarts (not on the memory backend). The node that takes the call applies the change before answering; other nodes apply it when the change message arrives (normally under a second), at the latest after one reconcile interval; a node that cannot read its database keeps its last copy until it is stale (10 intervals), then refuses all keys.
    - **Rotating without refusals in a cluster.** Issue the new key pair with `validFrom` a few seconds ahead, then invalidate the old one once the new window has opened; a token signed with a brand-new key can otherwise be refused by a node that has not yet received the change.
    - **Replacing `CYODA_JWT_SIGNING_KEY`** retires every issued key pair: they stop signing, verifying and appearing in JWKS, and the new bootstrap key signs. Restoring the old key brings them back.
    - **If `CYODA_JWT_SIGNING_KEY` may be exposed:** generate a new key; update the secret for every node; restart every node. Tokens signed by the old key or by issued key pairs stop verifying; clients fetch new tokens. Issue new key pairs if you use API rotation. Invalidating or deleting the bootstrap key through the API does not protect stored key pairs: the key still decrypts them.
    - **Deleting the bootstrap key is permanent** for that key: it cannot be reactivated; replace `CYODA_JWT_SIGNING_KEY` to recover.
    - **If `/oauth/token` answers 500** because the selected key pair cannot be used (the log names it): invalidate it with an unexpired admin token or an admin from a federated OIDC provider, or replace `CYODA_JWT_SIGNING_KEY`.
    - **No exportable signing key:** a KMS-backed key vault for issued key pairs, with the bootstrap key deleted once an issued key signs, is supported by the design; no KMS vault ships yet.
- [ ] **Step 2: Other help topics**
  - `auth/tokens.md:121`: "…The `kid` header points at the signing key pair, shared by every node of the cluster (`/oauth/keys/*`)…"; `:125` unchanged except: "…or `kid` not a usable key of this node". Add after ERRORS a line: "`GET /.well-known/jwks.json` answers `503` with `Retry-After` while the node's key copy is stale."
  - `errors.md:84` and `errors/KEYPAIR_NOT_FOUND.md`: causes also include "a key pair owned by another bootstrap key (retired after `CYODA_JWT_SIGNING_KEY` was replaced)" and "the bootstrap key after it was deleted".
  - `errors/NOT_FOUND.md:16,24`: remove "key pair lifecycle, trusted-key lifecycle" from the list of emitters; add SEE ALSO `errors.KEYPAIR_NOT_FOUND`, `errors.TRUSTED_KEY_NOT_FOUND` (they are the codes those endpoints return).
  - `helm.md:313` and `quickstart.md:105`: one line each — "This key is also the root secret for issued signing key pairs; see `config.auth` before replacing it."
  - `config_registry.go:87` (`CYODA_JWT_SIGNING_KEY` description): append "; also encrypts stored signing key pairs". `:97`: replace "The bootstrap signing key has no validity window." with "The bootstrap signing key has no window unless the key-pair API gave it one." `:99`: "…for the trusted-key, signing-key and OIDC-provider caches…".
- [ ] **Step 3: README, ARCHITECTURE, parity, compatibility, changelog**
  - `README.md:169`: same wording as `config_registry.go:99`.
  - `docs/ARCHITECTURE.md` §7.2: describe `KVKeyStore` (records in `signing-keys`, wrapped vault, classification, node copy with change message + re-read + fail-closed bound, admin writes read the store, compensated rotation) and the shared replicated component; replace `:1863`'s "First-party JWT validation is unaffected (… no KV dependency)" with the signing-key store's fail-closed behaviour. Present tense only; audit the whole section.
  - `docs/cloud-parity/signing-key-window.md:30`: "has no window unless the key-pair API gave it one".
  - `docs/cloud-parity/signing-key-pairs.md`: the contract Cloud must mirror — key pairs shared and persisted; bootstrap revocation persisted and deleted-is-terminal; retire-on-replacement (no Cloud equivalent: Cloud's configured key is not stored); 404 for retired key pairs; 503 `STORAGE_UNAVAILABLE` on the five endpoints and on JWKS; at-rest sealing is an implementation property, not contract (Cloud stores PKCS#8 unencrypted). End with the CaaS ticket body (title `[CaaS] Signing key pairs: persisted bootstrap revocation, 503 on key-pair endpoints and JWKS, 404 for retired key pairs`) for the product owner to file.
  - `COMPATIBILITY.md`: out-of-tree plugin note — the cassandra plugin must include the fix for KV `List` returning a partial result (cyoda-go-cassandra#102) to run this version.
  - `CHANGELOG.md` under the v0.9.0 section, `### Breaking`: key-pair store failures answer 500/503 instead of 404; first-party token verification depends on the KV store (fails closed when a node cannot read it for 10 reconcile intervals); a restart no longer restores a revoked bootstrap key; replacing `CYODA_JWT_SIGNING_KEY` retires issued key pairs. `### Fixed`: key pairs shared across the cluster and persisted; trusted-key admin writes no longer act on a stale copy, no longer leave a rotation half applied, and no longer block verification.
- [ ] **Step 4: Verify the help tree**

Run: `go test ./cmd/cyoda/...` (help-topic and registry tests, `TestErrCode_Parity`).
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add -A cmd/cyoda/help README.md docs/ARCHITECTURE.md docs/cloud-parity COMPATIBILITY.md CHANGELOG.md
git commit -m "docs: signing key pairs shared by the cluster — help, architecture, parity, changelog

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 14: Verification, reviews, PR

- [ ] **Step 1: Exit checks** (non-test files):

```bash
grep -rn "InMemoryKeyStore\|InMemoryTrustedKeyStore" --include='*.go' .
grep -rn "rsa.PrivateKey" internal --include='*.go' | grep -v _test
grep -rn "rsa.GenerateKey" internal --include='*.go' | grep -v _test
grep -rnE "#(285|624|286)\b" --include='*.go' --include='*.md' --include='*.yaml' internal app api cmd e2e | grep -v docs/superpowers
```

Expected: first empty; second lists only `jwt.go`, `signer.go`, `key_vault.go`, `kv_key_store.go`; third only `key_vault.go`; fourth empty.

- [ ] **Step 2: `make preflight` then `make test-full`** — root + every plugin submodule + E2E. Expected: green. `go vet ./...` clean; vet each plugin module as CI's `per-module-hygiene` job does.
- [ ] **Step 3: `make race`** — once. Expected: green.
- [ ] **Step 4: Whole-branch code review** — `superpowers:requesting-code-review` with a fresh-context reviewer (standing request; dispatch it, do not run it inline). Fix every finding via TDD or record a reasoned reply.
- [ ] **Step 5: Security review** — `antigravity-bundle-security-engineer:security-auditor` against Gate 3: no key material, sealed bytes, PEMs or tokens logged; SYSTEM-tenant KV use and tenant isolation on the trusted-key paths; input validation at the adapters; 5xx bodies generic with a ticket. Fix findings.
- [ ] **Step 6: Open the PR** into `release/v0.9.0` (check the base before `gh pr create`): title `feat(auth)!: signing key pairs shared by the cluster and persisted`; body with the summary, `### Breaking`, the verification evidence, the cassandra#102 dependency, and `Closes #285`. End with `🤖 Generated with [Claude Code](https://claude.com/claude-code)`.
