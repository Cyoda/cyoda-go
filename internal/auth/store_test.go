package auth_test

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"errors"
	"sync"
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/auth"
	"github.com/cyoda-platform/cyoda-go/internal/common"
)

// --- KeyStore Tests ---

// newIssuedOnlyStore is a key store whose bootstrap key is invalidated right
// away, so only the key pairs a test issues can ever be chosen as signer.
func newIssuedOnlyStore(t *testing.T) *auth.KVKeyStore {
	t.Helper()
	boot := newBootstrap(t)
	s := newKeyStore(t, mustNewMemoryKV(t, systemCtx()), boot)
	if err := s.Invalidate(systemCtx(), bootKID(t, boot), 0); err != nil {
		t.Fatalf("invalidate bootstrap: %v", err)
	}
	return s
}

// published reports whether kid is in the store's JWKS set.
func published(t *testing.T, s auth.KeyStore, kid string) (*auth.KeyPair, bool) {
	t.Helper()
	all, err := s.Published()
	if err != nil {
		t.Fatalf("Published: %v", err)
	}
	for _, kp := range all {
		if kp.KID == kid {
			return kp, true
		}
	}
	return nil, false
}

func TestKeyStore_IssueCurrentInvalidateReactivateDelete(t *testing.T) {
	ctx := systemCtx()
	store := newIssuedOnlyStore(t)
	kp1 := issueWindow(t, store, time.Now(), time.Now().Add(time.Hour))

	// Current
	got, err := store.Current()
	if err != nil {
		t.Fatalf("Current failed: %v", err)
	}
	if got.KID != kp1.KID || !got.Active {
		t.Errorf("unexpected key pair: KID=%s Active=%v", got.KID, got.Active)
	}

	// Invalidate
	if err := store.Invalidate(ctx, kp1.KID, 0); err != nil {
		t.Fatalf("Invalidate failed: %v", err)
	}
	if _, err := store.VerificationKey(kp1.KID); !errors.Is(err, auth.ErrKeyPairNotFound) {
		t.Errorf("invalidated key pair still verifies: %v", err)
	}
	// Current fails now (no active key pair at all: newIssuedOnlyStore
	// invalidated the bootstrap key too)
	if _, err := store.Current(); !errors.Is(err, auth.ErrKeyPairNotFound) {
		t.Fatalf("Current with no active key pair: err = %v, want ErrKeyPairNotFound", err)
	}

	// Reactivate
	now := time.Now()
	re, err := store.Reactivate(ctx, kp1.KID, now, now.Add(24*time.Hour))
	if err != nil {
		t.Fatalf("Reactivate failed: %v", err)
	}
	if !re.Active {
		t.Error("expected the key pair to be active after Reactivate")
	}
	if got, err := store.Current(); err != nil || got.KID != kp1.KID {
		t.Fatalf("Current after Reactivate = %v, %v; want %s", got, err, kp1.KID)
	}

	// Delete
	if err := store.Delete(ctx, kp1.KID); err != nil {
		t.Fatalf("Delete failed: %v", err)
	}
	if _, ok := published(t, store, kp1.KID); ok {
		t.Fatal("deleted key pair is still published")
	}
	if _, err := store.VerificationKey(kp1.KID); !errors.Is(err, auth.ErrKeyPairNotFound) {
		t.Fatalf("deleted key pair still verifies: %v", err)
	}

	// Delete, Invalidate and Reactivate of an unknown key pair: not found.
	if err := store.Delete(ctx, kp1.KID); !errors.Is(err, auth.ErrKeyPairNotFound) {
		t.Fatalf("Delete of a deleted key pair: err = %v, want ErrKeyPairNotFound", err)
	}
	if err := store.Invalidate(ctx, "kid-999", 0); !errors.Is(err, auth.ErrKeyPairNotFound) {
		t.Fatalf("Invalidate unknown: err = %v, want ErrKeyPairNotFound", err)
	}
	if _, err := store.Reactivate(ctx, "kid-999", now, now.Add(24*time.Hour)); !errors.Is(err, auth.ErrKeyPairNotFound) {
		t.Fatalf("Reactivate unknown: err = %v, want ErrKeyPairNotFound", err)
	}
}

// --- TrustedKeyStore Tests ---

func TestTrustedKeyStore_RegisterGetListInvalidateReactivateDelete(t *testing.T) {
	store := newTestTrustedStore(t)
	tID := spi.TenantID("tenant-test")

	key1, _ := rsa.GenerateKey(rand.Reader, 2048)
	key2, _ := rsa.GenerateKey(rand.Reader, 2048)

	tk1 := &auth.TrustedKey{
		KID:       "tk-1",
		TenantID:  tID,
		PublicKey: &key1.PublicKey,
		Active:    true,
		ValidFrom: time.Now(),
	}
	expiry := time.Now().Add(24 * time.Hour)
	tk2 := &auth.TrustedKey{
		KID:       "tk-2",
		TenantID:  tID,
		PublicKey: &key2.PublicKey,
		Active:    true,
		ValidFrom: time.Now(),
		ValidTo:   &expiry,
	}

	// Register
	if err := store.Register(context.Background(), tk1, false); err != nil {
		t.Fatalf("Register tk1 failed: %v", err)
	}
	if err := store.Register(context.Background(), tk2, false); err != nil {
		t.Fatalf("Register tk2 failed: %v", err)
	}

	// Get
	got, err := store.Get(context.Background(), tID, "tk-1")
	if err != nil {
		t.Fatalf("Get tk-1 failed: %v", err)
	}
	if got.KID != "tk-1" || !got.Active {
		t.Errorf("unexpected trusted key: KID=%s Active=%v", got.KID, got.Active)
	}

	// Get not found
	_, err = store.Get(context.Background(), tID, "tk-999")
	if err == nil {
		t.Fatal("expected error for missing trusted key, got nil")
	}

	// List
	all, _ := store.List(context.Background(), tID)
	if len(all) != 2 {
		t.Errorf("expected 2 trusted keys, got %d", len(all))
	}

	// Invalidate: ends the key at once (ValidTo = now)
	if err := store.Invalidate(context.Background(), tID, "tk-1"); err != nil {
		t.Fatalf("Invalidate failed: %v", err)
	}
	got, _ = store.Get(context.Background(), tID, "tk-1")
	if got.Active {
		t.Error("expected tk-1 to be inactive after Invalidate")
	}

	// Reactivate with a fresh validity window
	now := time.Now()
	if err := store.Reactivate(context.Background(), tID, "tk-1", now, now.Add(24*time.Hour)); err != nil {
		t.Fatalf("Reactivate failed: %v", err)
	}
	got, _ = store.Get(context.Background(), tID, "tk-1")
	if !got.Active {
		t.Error("expected tk-1 to be active after Reactivate")
	}

	// Delete
	if err := store.Delete(context.Background(), tID, "tk-1"); err != nil {
		t.Fatalf("Delete failed: %v", err)
	}
	_, err = store.Get(context.Background(), tID, "tk-1")
	if err == nil {
		t.Fatal("expected error after Delete, got nil")
	}
	all, _ = store.List(context.Background(), tID)
	if len(all) != 1 {
		t.Errorf("expected 1 trusted key after delete, got %d", len(all))
	}

	// Delete not found
	if err := store.Delete(context.Background(), tID, "tk-1"); err == nil {
		t.Fatal("expected error deleting non-existent trusted key, got nil")
	}

	// Invalidate not found
	if err := store.Invalidate(context.Background(), tID, "tk-999"); err == nil {
		t.Fatal("expected error invalidating non-existent trusted key, got nil")
	}

	// Reactivate not found
	if err := store.Reactivate(context.Background(), tID, "tk-999", now, now.Add(24*time.Hour)); err == nil {
		t.Fatal("expected error reactivating non-existent trusted key, got nil")
	}
}

// A key pair issued ahead of time (ValidFrom in the future) is not used to
// sign until its window opens: the current key keeps signing.
func TestKeyStore_Signer_SkipsKeyNotYetValid(t *testing.T) {
	s := newIssuedOnlyStore(t)
	now := time.Now()
	current := issueWindow(t, s, now.Add(-time.Hour), now.Add(2*time.Hour))
	issueWindow(t, s, now.Add(time.Hour), now.Add(2*time.Hour))
	got, _, err := s.Signer()
	if err != nil || got.KID != current.KID {
		t.Fatalf("Signer = %+v, %v; want %s", got, err, current.KID)
	}
}

// Two key pairs with the same ValidFrom (a client may send any validFrom)
// resolve the same way every time: the greater KID signs.
func TestKeyStore_Signer_TieBreakIsDeterministic(t *testing.T) {
	s := newIssuedOnlyStore(t)
	from := time.Now().Add(-time.Hour)
	want := ""
	for i := 0; i < 5; i++ {
		kp := issueWindow(t, s, from, from.Add(2*time.Hour))
		if kp.KID > want {
			want = kp.KID
		}
	}
	for i := 0; i < 50; i++ {
		got, _, err := s.Signer()
		if err != nil || got.KID != want {
			t.Fatalf("Signer = %+v, %v; want %s every time", got, err, want)
		}
	}
}

func TestKeyStore_Signer_MaxValidFrom(t *testing.T) {
	s := newIssuedOnlyStore(t)
	now := time.Now()
	issueWindow(t, s, now.Add(-time.Hour), now.Add(time.Hour))
	newer := issueWindow(t, s, now, now.Add(time.Hour))
	got, _, err := s.Signer()
	if err != nil || got.KID != newer.KID {
		t.Errorf("expected newer ValidFrom selected, got %+v, %v", got, err)
	}
}

func TestKeyStore_Issue_RotateInvalidatesSiblings(t *testing.T) {
	ctx := systemCtx()
	s := newIssuedOnlyStore(t)
	now := time.Now()
	existing := issueWindow(t, s, now, now.Add(time.Hour))
	fresh, err := s.Issue(ctx, auth.IssueRequest{ValidFrom: now.Add(time.Second), ValidTo: now.Add(time.Hour), Invalidate: true, GracePeriodSec: 60})
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	if !fresh.Active {
		t.Error("the new key pair must stay active")
	}
	// The sibling stops signing at once (Active flips, checked via
	// `published` below), but keeps verifying through its 60s grace period.
	if _, err := s.VerificationKey(existing.KID); err != nil {
		t.Errorf("expected the sibling still verifying inside its grace period, VerificationKey err = %v", err)
	}
	old, ok := published(t, s, existing.KID)
	if !ok || old.Active || old.ValidTo == nil || old.ValidTo.After(time.Now().Add(61*time.Second)) {
		t.Errorf("expected the sibling published in a 60s grace period, got %+v (published %v)", old, ok)
	}
}

func TestKeyStore_Issue_RotateNoOp(t *testing.T) {
	s := newIssuedOnlyStore(t)
	now := time.Now()
	fresh, err := s.Issue(systemCtx(), auth.IssueRequest{ValidFrom: now, ValidTo: now.Add(time.Hour), Invalidate: true, GracePeriodSec: 60})
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	if got, err := s.Current(); err != nil || got.KID != fresh.KID || !got.Active {
		t.Errorf("solo Issue with Invalidate=true should still leave the new key active: %+v, %v", got, err)
	}
}

func TestKeyStore_Issue_ConcurrentRotateExactlyOneActive(t *testing.T) {
	ctx := systemCtx()
	s := newIssuedOnlyStore(t)
	base := issueWindow(t, s, time.Now(), time.Now().Add(time.Hour))
	kids := []string{base.KID, "", ""}
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			now := time.Now()
			kp, err := s.Issue(ctx, auth.IssueRequest{ValidFrom: now.Add(time.Duration(i+1) * time.Millisecond), ValidTo: now.Add(time.Hour), Invalidate: true, GracePeriodSec: 1})
			if err == nil {
				kids[i+1] = kp.KID
			}
		}(i)
	}
	wg.Wait()
	// Active (not VerificationKey) is the right check here: with a grace
	// period a just-invalidated sibling still verifies, so more than one key
	// can legitimately answer VerificationKey at once. Active never does —
	// exactly one key pair may sign at a time.
	active := 0
	for _, kid := range kids {
		if kid == "" {
			t.Fatal("a concurrent issue failed")
		}
		if kp, ok := published(t, s, kid); ok && kp.Active {
			active++
		}
	}
	if active != 1 {
		t.Errorf("expected exactly 1 active key, got %d", active)
	}
}

func TestKeyStore_Published_LazyFilter(t *testing.T) {
	s := newIssuedOnlyStore(t)
	now := time.Now()
	past := now.Add(-time.Hour)
	active := issueWindow(t, s, now, now.Add(time.Hour))
	expired := issueWindow(t, s, past.Add(-time.Hour), past)
	if _, ok := published(t, s, active.KID); !ok {
		t.Error("expected the active key pair published")
	}
	if _, ok := published(t, s, expired.KID); ok {
		t.Error("expected the expired key pair not published")
	}
}

func TestKeyStore_Reactivate_FreshWindow(t *testing.T) {
	s := newIssuedOnlyStore(t)
	now := time.Now()
	past := now.Add(-time.Hour)
	expired := issueWindow(t, s, past.Add(-time.Hour), past)
	if _, err := s.VerificationKey(expired.KID); err == nil {
		t.Fatal("an expired key pair verifies")
	}
	got, err := s.Reactivate(systemCtx(), expired.KID, now, now.Add(24*time.Hour))
	if err != nil {
		t.Fatalf("reactivate: %v", err)
	}
	if !got.Active {
		t.Error("expected Active=true after reactivate")
	}
	if _, err := s.VerificationKey(expired.KID); err != nil {
		t.Errorf("a reactivated key pair does not verify: %v", err)
	}
}

func TestKeyStore_Reactivate_IdempotentOnActive(t *testing.T) {
	s := newIssuedOnlyStore(t)
	kp := issueWindow(t, s, time.Now(), time.Now().Add(time.Hour))
	newWindow := time.Now().Add(48 * time.Hour)
	got, err := s.Reactivate(systemCtx(), kp.KID, time.Now(), newWindow)
	if err != nil {
		t.Fatalf("reactivate on already-active: %v", err)
	}
	if !got.Active {
		t.Error("expected Active=true (idempotent)")
	}
	if got.ValidTo == nil || !got.ValidTo.Equal(newWindow) {
		t.Errorf("expected ValidTo updated to %v, got %v", newWindow, got.ValidTo)
	}
}

func TestKeyStore_Published_IncludesGracePeriodKey(t *testing.T) {
	s := newIssuedOnlyStore(t)
	kp := issueWindow(t, s, time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
	if err := s.Invalidate(systemCtx(), kp.KID, 3600); err != nil {
		t.Fatal(err)
	}
	if got, ok := published(t, s, kp.KID); !ok || got.Active {
		t.Fatalf("expected the grace-period key published and inactive, got %+v (published %v)", got, ok)
	}
}

// A grace period keeps a key pair verifiable for at most that long, and never
// beyond the end of the window it already had: invalidating, directly or by
// rotation, can only shorten a key pair's life.
func TestKeyStore_InvalidateNeverExtends(t *testing.T) {
	ctx := systemCtx()
	s := newIssuedOnlyStore(t)
	issue := func(validTo time.Time, invalidate bool, grace int64) *auth.KeyPair {
		t.Helper()
		kp, err := s.Issue(ctx, auth.IssueRequest{
			ValidFrom: time.Now().Add(-2 * time.Hour), ValidTo: validTo,
			Invalidate: invalidate, GracePeriodSec: grace,
		})
		if err != nil {
			t.Fatalf("issue: %v", err)
		}
		return kp
	}
	verifiable := func(kid string) bool {
		_, ok := published(t, s, kid)
		return ok
	}

	expired := issue(time.Now().Add(-time.Minute), false, 0)
	if err := s.Invalidate(ctx, expired.KID, 3600); err != nil {
		t.Fatalf("invalidate expired: %v", err)
	}
	if verifiable(expired.KID) {
		t.Error("invalidating an expired key pair with a grace period revived it")
	}

	revoked := issue(time.Now().Add(time.Hour), false, 0)
	if err := s.Invalidate(ctx, revoked.KID, 0); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if err := s.Invalidate(ctx, revoked.KID, 3600); err != nil {
		t.Fatalf("invalidate revoked: %v", err)
	}
	if verifiable(revoked.KID) {
		t.Error("a second invalidation with a grace period revived a revoked key pair")
	}

	end := time.Now().Add(10 * time.Minute)
	short := issue(end, false, 0)
	if err := s.Invalidate(ctx, short.KID, 3600); err != nil {
		t.Fatalf("invalidate short: %v", err)
	}
	if kp, ok := published(t, s, short.KID); !ok || kp.ValidTo == nil || kp.ValidTo.After(end) {
		t.Errorf("a grace period extended a key pair beyond its window: %+v (published %v)", kp, ok)
	}

	old := issue(time.Now().Add(-time.Minute), false, 0)
	issue(time.Now().Add(time.Hour), true, 3600)
	if verifiable(old.KID) {
		t.Error("rotation with a grace period revived an expired key pair")
	}

	graced := issue(time.Now().Add(time.Hour), false, 0)
	if err := s.Invalidate(ctx, graced.KID, 3600); err != nil {
		t.Fatalf("invalidate graced: %v", err)
	}
	issue(time.Now().Add(time.Hour), true, 0)
	if verifiable(graced.KID) {
		t.Error("rotation with no grace left a key pair in its grace period published")
	}
}

func testRSAPriv(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("rsa.GenerateKey: %v", err)
	}
	return priv
}

func TestTrustedKeyStore_TenantIsolation(t *testing.T) {
	s := newTestTrustedStore(t)
	priv := testRSAPriv(t)
	tA := spi.TenantID("tenant-a")
	tB := spi.TenantID("tenant-b")
	tk := &auth.TrustedKey{KID: "k1", TenantID: tA, PublicKey: &priv.PublicKey, Active: true, ValidFrom: time.Now()}
	if err := s.Register(context.Background(), tk, false); err != nil {
		t.Fatalf("register: %v", err)
	}
	if _, err := s.Get(context.Background(), tB, "k1"); err == nil {
		t.Error("B.Get(k1) leaked")
	}
	if err := s.Delete(context.Background(), tB, "k1"); err == nil {
		t.Error("B.Delete(k1) leaked")
	}
	if err := s.Invalidate(context.Background(), tB, "k1"); err == nil {
		t.Error("B.Invalidate(k1) leaked")
	}
}

func TestTrustedKeyStore_CapReached(t *testing.T) {
	s := auth.NewKVTrustedKeyStore(mustNewMemoryKV(t, systemCtx()), 2)
	priv := testRSAPriv(t)
	tID := spi.TenantID("t")
	mk := func(kid string) *auth.TrustedKey {
		return &auth.TrustedKey{KID: kid, TenantID: tID, PublicKey: &priv.PublicKey, Active: true, ValidFrom: time.Now()}
	}
	_ = s.Register(context.Background(), mk("k1"), false)
	_ = s.Register(context.Background(), mk("k2"), false)
	err := s.Register(context.Background(), mk("k3"), false)
	if err == nil {
		t.Fatal("expected cap-reached error")
	}
	var ae *common.AppError
	if !errors.As(err, &ae) || ae.Code != common.ErrCodeTrustedKeyCapReached {
		t.Errorf("expected TRUSTED_KEY_CAP_REACHED, got %v", err)
	}
}

func TestTrustedKeyStore_CapCountsValidOnly(t *testing.T) {
	s := auth.NewKVTrustedKeyStore(mustNewMemoryKV(t, systemCtx()), 2)
	priv := testRSAPriv(t)
	tID := spi.TenantID("t")
	past := time.Now().Add(-1 * time.Hour)
	expired := &auth.TrustedKey{KID: "old", TenantID: tID, PublicKey: &priv.PublicKey, Active: false, ValidFrom: past, ValidTo: &past}
	active := &auth.TrustedKey{KID: "new", TenantID: tID, PublicKey: &priv.PublicKey, Active: true, ValidFrom: time.Now()}
	_ = s.Register(context.Background(), expired, false)
	_ = s.Register(context.Background(), active, false)
	third := &auth.TrustedKey{KID: "third", TenantID: tID, PublicKey: &priv.PublicKey, Active: true, ValidFrom: time.Now()}
	if err := s.Register(context.Background(), third, false); err != nil {
		t.Fatalf("expected accept; expired excluded from count; got %v", err)
	}
}

func TestTrustedKeyStore_RotateInvalidatesSameTenant(t *testing.T) {
	s := newTestTrustedStore(t)
	priv := testRSAPriv(t)
	tID := spi.TenantID("t")
	a := &auth.TrustedKey{KID: "a", TenantID: tID, PublicKey: &priv.PublicKey, Active: true, ValidFrom: time.Now()}
	_ = s.Register(context.Background(), a, false)
	b := &auth.TrustedKey{KID: "b", TenantID: tID, PublicKey: &priv.PublicKey, Active: true, ValidFrom: time.Now().Add(1 * time.Second)}
	if err := s.Register(context.Background(), b, true); err != nil {
		t.Fatalf("register: %v", err)
	}
	gotA, _ := s.Get(context.Background(), tID, "a")
	if gotA.Active || gotA.ValidTo == nil {
		t.Errorf("expected a invalidated with ValidTo; got %+v", gotA)
	}
}

func TestTrustedKeyStore_Reactivate_RequiresFreshWindow(t *testing.T) {
	s := newTestTrustedStore(t)
	priv := testRSAPriv(t)
	tID := spi.TenantID("t")
	now := time.Now()
	past := now.Add(-1 * time.Hour)
	expired := &auth.TrustedKey{KID: "e", TenantID: tID, PublicKey: &priv.PublicKey, Active: false, ValidFrom: past, ValidTo: &past}
	_ = s.Register(context.Background(), expired, false)
	if err := s.Reactivate(context.Background(), tID, "e", now, past); err == nil {
		t.Error("expected past validTo rejected")
	}
	if err := s.Reactivate(context.Background(), tID, "e", now, now.Add(24*time.Hour)); err != nil {
		t.Fatalf("reactivate: %v", err)
	}
}

func TestM2MClient_TenantIDIsSpiType(t *testing.T) {
	var c auth.M2MClient
	var _ spi.TenantID = c.TenantID // compile-time check: field must be assignable from spi.TenantID
}
