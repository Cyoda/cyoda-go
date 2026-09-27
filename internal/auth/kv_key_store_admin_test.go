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
		PublicKey string                         `json:"publicKey"`
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
		"reactivate": func() error {
			_, err := r.Reactivate(ctx, r.BootstrapKID(), time.Now(), time.Now().Add(time.Hour))
			return err
		},
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
	_ = a.Invalidate(ctx, a.BootstrapKID(), 60)        // writes a bootstrap-state record
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
