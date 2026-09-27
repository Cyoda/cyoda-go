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
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
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
	if err := b.ReconcileForTest(ctx); err != nil {
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
	priv := mustOpenPrivate(t, boot, bootKID(t, boot), kp.KID, rec.Vault.Owner, spki, sealed)
	for _, der := range [][]byte{x509.MarshalPKCS1PrivateKey(priv), mustPKCS8(t, priv)} {
		head := der
		if len(head) > 48 {
			head = head[:48] // one PEM line's worth: base64 of 48 bytes never crosses a line break
		}
		forms := [][]byte{
			der,
			[]byte(base64.StdEncoding.EncodeToString(der)),
			[]byte(base64.URLEncoding.EncodeToString(der)),
			[]byte(base64.RawURLEncoding.EncodeToString(der)),
			[]byte(hex.EncodeToString(der)),
			[]byte(base64.StdEncoding.EncodeToString(head)),
		}
		for _, form := range forms {
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
	mu    sync.Mutex
	keys  map[string]*rsa.PrivateKey // stands in for keys held by a KMS
	signs int                        // counts Sign calls, so a test can prove signing went through this vault
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
	r.v.signs++
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
	kv := mustNewMemoryKV(t, ctx)
	rv := &remoteVault{keys: map[string]*rsa.PrivateKey{}}
	s, err := auth.NewKVKeyStore(ctx, kv, auth.KVKeyStoreConfig{Bootstrap: newBootstrap(t), BootstrapAudience: "human", Vault: rv})
	if err != nil {
		t.Fatal(err)
	}
	kp := issue(t, s, "client", false)
	if _, ok := rv.keys[kp.KID]; !ok {
		t.Fatal("the configured vault never generated the key: cfg.Vault was ignored")
	}
	raw, _ := kv.Get(ctx, "signing-keys", kp.KID)
	var rec struct {
		Vault struct{ Kind string } `json:"vault"`
	}
	_ = json.Unmarshal(raw, &rec)
	if rec.Vault.Kind != "remote" {
		t.Fatalf("stored vault kind = %q, want %q: cfg.Vault was ignored", rec.Vault.Kind, "remote")
	}
	got, signer, err := s.Signer("client")
	if err != nil || got.KID != kp.KID {
		t.Fatalf("%v %v", got, err)
	}
	tok, err := auth.Sign(ctx, map[string]any{"sub": "x"}, signer, got.KID)
	if err != nil {
		t.Fatal(err)
	}
	rv.mu.Lock()
	signs := rv.signs
	rv.mu.Unlock()
	if signs == 0 {
		t.Fatal("the remote vault's signer was never called: signing did not go through cfg.Vault")
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
	boot := newBootstrap(t)
	s := newKeyStore(t, kv, boot, "client")
	old := issue(t, s, "client", false)
	humanKey := issue(t, s, "human", false)
	nw := issue(t, s, "client", true)
	if _, err := s.VerificationKey(old.KID); !errors.Is(err, auth.ErrKeyPairNotFound) {
		t.Fatal("old client key still verifies after rotation")
	}
	if _, err := s.VerificationKey(bootKID(t, boot)); !errors.Is(err, auth.ErrKeyPairNotFound) {
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
	boot := newBootstrap(t)
	s := newKeyStore(t, mustNewMemoryKV(t, systemCtx()), boot, "human")
	issue(t, s, "client", true)
	if _, err := s.VerificationKey(bootKID(t, boot)); err != nil {
		t.Fatalf("human bootstrap key invalidated by a client rotation: %v", err)
	}
}

// commitThenFailKV commits the Put to the underlying store and only then
// reports failure — the case writeAll's own doc names ("the failing write
// can itself have partially or fully committed before reporting failure").
// A KV that rejects the write before it ever lands (failPutKV, the earlier
// shape of this test) makes "restore the bootstrap key to absent" vacuous:
// deleting a key that was never written is a no-op regardless of whether the
// restore loop is even correct. Only a KV that actually commits first can
// prove the restore loop undoes real, already-stored state.
type commitThenFailKV struct {
	spi.KeyValueStore
	failKey string
}

func (f *commitThenFailKV) Put(ctx context.Context, ns, key string, v []byte) error {
	if err := f.KeyValueStore.Put(ctx, ns, key, v); err != nil {
		return err
	}
	if key == f.failKey {
		return errors.New("injected failure after commit")
	}
	return nil
}

// Two siblings are ended by this rotation: the old client key (an existing
// stored record) and the bootstrap key itself (bootstrap audience "client",
// never before touched — absent state). siblingWrites always appends the
// bootstrap sibling last, so failing its Put fails the SECOND sibling: the
// restore must both put the old key's original bytes back (a write that had
// already landed) and remove the bootstrap sibling's record entirely (a
// write that took it from absent to written and must go back to absent, not
// to some placeholder value) — and that removal must undo a write that
// genuinely committed, not one commitThenFailKV merely rejected up front.
func TestKVKeyStore_RotationCompensatesOnSiblingFailure(t *testing.T) {
	ctx := systemCtx()
	mem := mustNewMemoryKV(t, ctx)
	boot := newBootstrap(t)
	pre := newKeyStore(t, mem, boot, "client")
	old := issue(t, pre, "client", false)
	before, _ := mem.Get(ctx, "signing-keys", old.KID)
	kid := bootKID(t, boot)
	if _, err := mem.Get(ctx, "signing-keys", kid); !errors.Is(err, spi.ErrNotFound) {
		t.Fatal("bootstrap key already has a stored record before the rotation")
	}
	bc := newFakeBroadcaster()
	var pings int
	bc.Subscribe("auth.signingkeys", func([]byte) { pings++ })
	s, _ := auth.NewKVKeyStore(ctx, &commitThenFailKV{KeyValueStore: mem, failKey: kid},
		auth.KVKeyStoreConfig{Bootstrap: boot, BootstrapAudience: "client", Broadcaster: bc})
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
	if _, err := mem.Get(ctx, "signing-keys", kid); !errors.Is(err, spi.ErrNotFound) {
		t.Fatal("bootstrap sibling left behind: its committed write was not restored to absent")
	}
	if pings == 0 {
		t.Fatal("no change message after a failed rotation that wrote the store")
	}
}

// putOrderKV records the order Put is called in, so a test can assert on
// write ordering rather than only on the final state.
type putOrderKV struct {
	spi.KeyValueStore
	mu    sync.Mutex
	order []string
}

func (k *putOrderKV) Put(ctx context.Context, ns, key string, v []byte) error {
	k.mu.Lock()
	k.order = append(k.order, key)
	k.mu.Unlock()
	return k.KeyValueStore.Put(ctx, ns, key, v)
}

// The new key must be written before any sibling: a reader that lists the
// store mid-rotation must never see a sibling already ended with no
// replacement key active yet.
func TestKVKeyStore_RotationWritesNewKeyBeforeSiblings(t *testing.T) {
	ctx := systemCtx()
	mem := mustNewMemoryKV(t, ctx)
	boot := newBootstrap(t)
	pre := newKeyStore(t, mem, boot, "client")
	old := issue(t, pre, "client", false)
	rec := &putOrderKV{KeyValueStore: mem}
	s, err := auth.NewKVKeyStore(ctx, rec, auth.KVKeyStoreConfig{Bootstrap: boot, BootstrapAudience: "client"})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	nw, err := s.Issue(ctx, auth.IssueRequest{Audience: "client", ValidFrom: now, ValidTo: now.Add(time.Hour), Invalidate: true})
	if err != nil {
		t.Fatal(err)
	}
	rec.mu.Lock()
	order := append([]string(nil), rec.order...)
	rec.mu.Unlock()
	if len(order) < 2 {
		t.Fatalf("expected at least 2 Put calls (new key + siblings), got %v", order)
	}
	if order[0] != nw.KID {
		t.Fatalf("Put order = %v, want the new KID %s first", order, nw.KID)
	}
	found := false
	for _, k := range order[1:] {
		if k == old.KID {
			found = true
		}
	}
	if !found {
		t.Fatalf("sibling %s never written; order = %v", old.KID, order)
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
	if err := s.Delete(ctx, bootKID(t, boot)); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "still unseals") || !strings.Contains(buf.String(), "level=WARN") {
		t.Fatalf("missing WARN about the PEM; log: %s", buf.String())
	}
	r := newKeyStore(t, kv, boot, "client") // restart
	if _, err := r.VerificationKey(bootKID(t, boot)); !errors.Is(err, auth.ErrKeyPairNotFound) {
		t.Fatal("deleted bootstrap key verifies after restart")
	}
	for name, call := range map[string]func() error{
		"reactivate": func() error {
			_, err := r.Reactivate(ctx, bootKID(t, boot), time.Now(), time.Now().Add(time.Hour))
			return err
		},
		"invalidate": func() error { return r.Invalidate(ctx, bootKID(t, boot), 0) },
		"delete":     func() error { return r.Delete(ctx, bootKID(t, boot)) },
	} {
		if err := call(); !errors.Is(err, auth.ErrKeyPairNotFound) {
			t.Fatalf("%s after delete: err = %v, want ErrKeyPairNotFound", name, err)
		}
	}
	// A rotation never touches a deleted bootstrap key: it is never a
	// sibling, so its stored bytes must come out byte-identical, not merely
	// still containing the deleted flag.
	before, err := kv.Get(ctx, "signing-keys", bootKID(t, boot))
	if err != nil {
		t.Fatal(err)
	}
	issue(t, r, "client", true)
	after, err := kv.Get(ctx, "signing-keys", bootKID(t, boot))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatalf("a deleted bootstrap key was touched by a rotation: before=%s after=%s", before, after)
	}
}

func TestKVKeyStore_NoWarnWithoutOwnedPairs(t *testing.T) {
	ctx := systemCtx()
	boot := newBootstrap(t)
	s := newKeyStore(t, mustNewMemoryKV(t, ctx), boot, "client")
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	defer slog.SetDefault(prev)
	if err := s.Invalidate(ctx, bootKID(t, boot), 0); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(buf.String(), "still unseals") {
		t.Fatal("WARN logged with no owned key pairs")
	}
}

// The mirror of TestKVKeyStore_NoWarnWithoutOwnedPairs: an owned pair exists,
// so invalidating the bootstrap key directly must warn.
func TestKVKeyStore_InvalidateBootstrapWithOwnedPairWarns(t *testing.T) {
	ctx := systemCtx()
	boot := newBootstrap(t)
	s := newKeyStore(t, mustNewMemoryKV(t, ctx), boot, "client")
	issue(t, s, "client", false)
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	defer slog.SetDefault(prev)
	if err := s.Invalidate(ctx, bootKID(t, boot), 0); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "still unseals") || !strings.Contains(buf.String(), "level=WARN") {
		t.Fatalf("missing WARN on bootstrap invalidate with an owned pair; log: %s", buf.String())
	}
}

// A client rotation that also ends the bootstrap key (bootstrap audience
// "client") must warn, exactly as a direct Invalidate of the bootstrap key
// does.
func TestKVKeyStore_RotationEndingBootstrapWarns(t *testing.T) {
	ctx := systemCtx()
	s := newKeyStore(t, mustNewMemoryKV(t, ctx), newBootstrap(t), "client")
	issue(t, s, "client", false) // an owned pair, so the WARN has something to report
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	defer slog.SetDefault(prev)
	issue(t, s, "client", true) // rotation ends the bootstrap key too
	if !strings.Contains(buf.String(), "still unseals") || !strings.Contains(buf.String(), "level=WARN") {
		t.Fatalf("missing WARN on a rotation that ends the bootstrap key; log: %s", buf.String())
	}
}

// Invalidating an issued key (not the bootstrap key) never logs the
// bootstrap-revocation WARN. The bootstrap key is revoked FIRST, before the
// log capture starts: logRevokedBootstrap's own "is the bootstrap key
// actually revoked" check would otherwise suppress the WARN regardless of
// whether Invalidate's own `kid == s.boot.kid` guard is even present, making
// the assertion pass for the wrong reason.
func TestKVKeyStore_IssuedKeyInvalidateNeverWarns(t *testing.T) {
	ctx := systemCtx()
	boot := newBootstrap(t)
	s := newKeyStore(t, mustNewMemoryKV(t, ctx), boot, "client")
	kp := issue(t, s, "client", false)
	if err := s.Invalidate(ctx, bootKID(t, boot), 0); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	defer slog.SetDefault(prev)
	if err := s.Invalidate(ctx, kp.KID, 0); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(buf.String(), "still unseals") {
		t.Fatal("WARN logged for an issued-key invalidate")
	}
}

func TestKVKeyStore_BootstrapInvalidateReactivateAcrossRestart(t *testing.T) {
	ctx := systemCtx()
	kv := mustNewMemoryKV(t, ctx)
	boot := newBootstrap(t)
	s := newKeyStore(t, kv, boot, "client")
	_ = s.Invalidate(ctx, bootKID(t, boot), 0)
	r := newKeyStore(t, kv, boot, "client")
	if _, err := r.VerificationKey(bootKID(t, boot)); !errors.Is(err, auth.ErrKeyPairNotFound) {
		t.Fatal("invalidated bootstrap key back after restart")
	}
	kp, err := r.Reactivate(ctx, bootKID(t, boot), time.Now().Add(-time.Second), time.Now().Add(time.Hour))
	if err != nil || !kp.Active || kp.ValidTo == nil {
		t.Fatalf("reactivate: %+v %v", kp, err)
	}
	if _, err := r.VerificationKey(bootKID(t, boot)); err != nil {
		t.Fatal(err)
	}
}

func TestKVKeyStore_RetiredAndForeignAreNotFound(t *testing.T) {
	ctx := systemCtx()
	kv := mustNewMemoryKV(t, ctx)
	bootA, bootB := newBootstrap(t), newBootstrap(t) // two different bootstrap keys
	a := newKeyStore(t, kv, bootA, "client")
	kp := issue(t, a, "client", false)
	_ = a.Invalidate(ctx, bootKID(t, bootA), 60) // writes a bootstrap-state record
	b := newKeyStore(t, kv, bootB, "client")
	for _, kid := range []string{kp.KID, bootKID(t, bootA)} {
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
	if got, _, _ := b.Signer("client"); got.KID != bootKID(t, bootB) {
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
	if err := a.ReconcileForTest(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := a.VerificationKey(k1.KID); !errors.Is(err, auth.ErrKeyPairNotFound) {
		t.Fatal("sibling issued on another node stayed active")
	}
}

// Deleting an undecodable record away from the bootstrap KID replaces it with
// a deleted bootstrap-state record (spec §5.5) rather than removing it: the
// KID could be some other node's bootstrap key, and fail-closed means it
// stays revoked rather than silently reappearing usable if that key is ever
// reintroduced.
func TestKVKeyStore_UndecodableCanBeDeleted(t *testing.T) {
	ctx := systemCtx()
	kv := mustNewMemoryKV(t, ctx)
	_ = kv.Put(ctx, "signing-keys", undecodableKID, []byte("{"))
	s := newKeyStore(t, kv, newBootstrap(t), "client")
	if err := s.Delete(ctx, undecodableKID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Signer("client"); err != nil {
		t.Fatalf("signing still blocked after deleting the undecodable record: %v", err)
	}
	raw, err := kv.Get(ctx, "signing-keys", undecodableKID)
	if err != nil {
		t.Fatalf("undecodable record removed instead of replaced: %v", err)
	}
	var rec struct {
		Kind    string `json:"kind"`
		Active  bool   `json:"active"`
		Deleted bool   `json:"deleted"`
	}
	_ = json.Unmarshal(raw, &rec)
	if rec.Kind != "bootstrap" || rec.Active || !rec.Deleted {
		t.Fatalf("stored record after delete = %+v, want a deleted bootstrap state", rec)
	}
}

// The same rule at the bootstrap KID itself: deleting an undecodable record
// there leaves this node's own bootstrap key refused, and terminal like any
// other bootstrap deletion.
func TestKVKeyStore_UndecodableAtBootstrapKIDCanBeDeleted(t *testing.T) {
	ctx := systemCtx()
	kv := mustNewMemoryKV(t, ctx)
	boot := newBootstrap(t)
	kid, _ := auth.DeriveKID(&boot.PublicKey)
	_ = kv.Put(ctx, "signing-keys", kid, []byte("{"))
	s := newKeyStore(t, kv, boot, "client")
	if err := s.Delete(ctx, kid); err != nil {
		t.Fatal(err)
	}
	if _, err := s.VerificationKey(kid); !errors.Is(err, auth.ErrKeyPairNotFound) {
		t.Fatal("bootstrap key verifies after deleting its undecodable record")
	}
	if _, err := s.Reactivate(ctx, kid, time.Now(), time.Now().Add(time.Hour)); !errors.Is(err, auth.ErrKeyPairNotFound) {
		t.Fatal("a deleted bootstrap key was reactivated")
	}
	raw, _ := kv.Get(ctx, "signing-keys", kid)
	var rec struct {
		Kind    string `json:"kind"`
		Active  bool   `json:"active"`
		Deleted bool   `json:"deleted"`
	}
	_ = json.Unmarshal(raw, &rec)
	if rec.Kind != "bootstrap" || rec.Active || !rec.Deleted {
		t.Fatalf("stored record after delete = %+v, want a deleted bootstrap state", rec)
	}
}

// Review focus: two issues at once on one node. t.Fatal must run only on the
// test's own goroutine, so failures from the two issuing goroutines are
// collected on a channel instead of reported inline.
func TestKVKeyStore_ConcurrentIssueOnOneNode(t *testing.T) {
	ctx := systemCtx()
	s := newKeyStore(t, mustNewMemoryKV(t, ctx), newBootstrap(t), "human")
	var wg sync.WaitGroup
	kids := make([]string, 2)
	errs := make(chan error, len(kids))
	for i := range kids {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			now := time.Now()
			kp, err := s.Issue(ctx, auth.IssueRequest{Audience: "client", ValidFrom: now, ValidTo: now.Add(time.Hour)})
			if err != nil {
				errs <- err
				return
			}
			kids[i] = kp.KID
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	if kids[0] == kids[1] {
		t.Fatal("duplicate KID")
	}
	first, _, _ := s.Signer("client")
	second, _, _ := s.Signer("client")
	if first.KID != second.KID {
		t.Fatal("signer choice not deterministic")
	}
}

// toggleGetListKV fails every Get and List call once fail is set, simulating
// the store becoming unavailable for admin reads: a real storage failure,
// never to be confused with "record not found".
type toggleGetListKV struct {
	spi.KeyValueStore
	fail atomic.Bool
}

func (k *toggleGetListKV) Get(ctx context.Context, ns, key string) ([]byte, error) {
	if k.fail.Load() {
		return nil, errors.New("store unavailable")
	}
	return k.KeyValueStore.Get(ctx, ns, key)
}

func (k *toggleGetListKV) List(ctx context.Context, ns string) (map[string][]byte, error) {
	if k.fail.Load() {
		return nil, errors.New("store unavailable")
	}
	return k.KeyValueStore.List(ctx, ns)
}

// A storage failure must never be reported as ErrKeyPairNotFound: a caller
// that maps 404 vs 500 on that distinction would otherwise tell a client the
// key does not exist when the truth is the store could not be read.
func TestKVKeyStore_StoreFailureIsNotNotFound(t *testing.T) {
	ctx := systemCtx()
	mem := mustNewMemoryKV(t, ctx)
	boot := newBootstrap(t)
	kv := &toggleGetListKV{KeyValueStore: mem}
	s, err := auth.NewKVKeyStore(ctx, kv, auth.KVKeyStoreConfig{Bootstrap: boot, BootstrapAudience: "client"})
	if err != nil {
		t.Fatal(err)
	}
	kp := issue(t, s, "client", false)
	kv.fail.Store(true)
	now := time.Now()
	checks := map[string]func() error{
		"issue with invalidate": func() error {
			_, err := s.Issue(ctx, auth.IssueRequest{Audience: "client", ValidFrom: now, ValidTo: now.Add(time.Hour), Invalidate: true})
			return err
		},
		"invalidate":       func() error { return s.Invalidate(ctx, kp.KID, 0) },
		"reactivate":       func() error { _, err := s.Reactivate(ctx, kp.KID, now, now.Add(time.Hour)); return err },
		"delete issued":    func() error { return s.Delete(ctx, kp.KID) },
		"delete bootstrap": func() error { return s.Delete(ctx, bootKID(t, boot)) },
	}
	for name, call := range checks {
		if err := call(); err == nil || errors.Is(err, auth.ErrKeyPairNotFound) {
			t.Fatalf("%s: err = %v, want a non-not-found store error", name, err)
		}
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

// Times whose UTC form has no four-digit year: Go reads the offset forms, but
// a record formatted from them could never be parsed back.
var (
	year10000 = time.Date(9999, 12, 31, 23, 59, 59, 0, time.FixedZone("", -5*3600))
	yearMinus = time.Date(0, 1, 1, 0, 0, 0, 0, time.FixedZone("", 5*3600))
)

func signingRecords(t *testing.T, kv spi.KeyValueStore) map[string][]byte {
	t.Helper()
	all, err := kv.List(systemCtx(), "signing-keys")
	if err != nil {
		t.Fatal(err)
	}
	return all
}

func sameRecords(t *testing.T, before, after map[string][]byte) {
	t.Helper()
	if len(after) != len(before) {
		t.Fatalf("records = %d, want %d", len(after), len(before))
	}
	for k, v := range before {
		if !bytes.Equal(after[k], v) {
			t.Fatalf("record %s changed", k)
		}
	}
}

func TestKVKeyStore_IssueRefusesUnstorableTime(t *testing.T) {
	ctx := systemCtx()
	kv := mustNewMemoryKV(t, ctx)
	boot := newBootstrap(t)
	s := newKeyStore(t, kv, boot, "client")
	now := time.Now()
	for name, req := range map[string]auth.IssueRequest{
		"validTo year 10000": {Audience: "client", ValidFrom: now, ValidTo: year10000},
		"validFrom year -1":  {Audience: "client", ValidFrom: yearMinus, ValidTo: now.Add(time.Hour)},
	} {
		if _, err := s.Issue(ctx, req); err == nil {
			t.Fatalf("%s: issued", name)
		}
	}
	if got := signingRecords(t, kv); len(got) != 0 {
		t.Fatalf("%d records written", len(got))
	}
	if kp, _, err := s.Signer("client"); err != nil || kp.KID != bootKID(t, boot) {
		t.Fatalf("signing changed: %v %v", kp, err)
	}
	if _, err := newKeyStore(t, kv, boot, "client").VerificationKey(bootKID(t, boot)); err != nil {
		t.Fatalf("restart: %v", err)
	}
}

func TestKVKeyStore_ReactivateRefusesUnstorableTime(t *testing.T) {
	ctx := systemCtx()
	kv := mustNewMemoryKV(t, ctx)
	boot := newBootstrap(t)
	s := newKeyStore(t, kv, boot, "human")
	kp := issue(t, s, "client", false)
	if err := s.Invalidate(ctx, bootKID(t, boot), 60); err != nil {
		t.Fatal(err)
	}
	before := signingRecords(t, kv)
	for _, kid := range []string{kp.KID, bootKID(t, boot)} {
		if _, err := s.Reactivate(ctx, kid, time.Now().Add(-time.Second), year10000); err == nil {
			t.Fatalf("%s: reactivated with validTo in year 10000", kid)
		}
		if _, err := s.Reactivate(ctx, kid, yearMinus, time.Now().Add(time.Hour)); err == nil {
			t.Fatalf("%s: reactivated with validFrom in year -1", kid)
		}
	}
	sameRecords(t, before, signingRecords(t, kv))
	if _, err := newKeyStore(t, kv, boot, "human").VerificationKey(kp.KID); err != nil {
		t.Fatalf("restart: %v", err)
	}
}

// The bootstrap key in its never-changed state has no record: a refused
// reactivation must not write one.
func TestKVKeyStore_ReactivateAbsentBootstrapRefusesUnstorableTime(t *testing.T) {
	ctx := systemCtx()
	kv := mustNewMemoryKV(t, ctx)
	boot := newBootstrap(t)
	s := newKeyStore(t, kv, boot, "client")
	if _, err := s.Reactivate(ctx, bootKID(t, boot), time.Now().Add(-time.Second), year10000); err == nil {
		t.Fatal("reactivated with validTo in year 10000")
	}
	if got := signingRecords(t, kv); len(got) != 0 {
		t.Fatalf("%d records written", len(got))
	}
	if kp, _, err := s.Signer("client"); err != nil || kp.KID != bootKID(t, boot) {
		t.Fatalf("signing changed: %v %v", kp, err)
	}
}

func TestStorableTime(t *testing.T) {
	for _, c := range []struct {
		t    time.Time
		want bool
	}{
		{time.Time{}, true},
		{time.Date(9999, 12, 31, 23, 59, 59, 999999999, time.UTC), true},
		{year10000, false},
		{yearMinus, false},
		{time.Date(0, 6, 1, 0, 0, 0, 0, time.UTC), false},
		{time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC), false},
	} {
		if got := auth.StorableTime(c.t); got != c.want {
			t.Errorf("StorableTime(%v) = %v, want %v", c.t, got, c.want)
		}
	}
}

// Every KID a store gives a key pair — issued or bootstrap — has the form the
// key-pair endpoints accept.
func TestMatchesKeyPairIDPattern(t *testing.T) {
	boot := newBootstrap(t)
	s := newKeyStore(t, mustNewMemoryKV(t, systemCtx()), boot, "client")
	for _, kid := range []string{issue(t, s, "human", false).KID, bootKID(t, boot)} {
		if !auth.MatchesKeyPairIDPattern(kid) {
			t.Errorf("store KID %q refused", kid)
		}
	}
	hex32 := "0123456789abcdef0123456789abcdef"
	for _, kid := range []string{"", "not-a-kid", strings.ToUpper(hex32), hex32[1:], hex32 + "0", "../" + hex32[3:], hex32[:31] + "\n"} {
		if auth.MatchesKeyPairIDPattern(kid) {
			t.Errorf("%q accepted", kid)
		}
	}
}
