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

// newIssuedOnlyStore is a key store whose bootstrap key signs for "human",
// so the "client" audience has only the key pairs a test issues.
func newIssuedOnlyStore(t *testing.T) *auth.KVKeyStore {
	t.Helper()
	return newKeyStore(t, mustNewMemoryKV(t, systemCtx()), newBootstrap(t), "human")
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
	kp1 := issueWindow(t, store, "client", time.Now(), time.Now().Add(time.Hour))

	// Current
	got, err := store.Current("client")
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
	// Current fails now (no active key pair for the audience)
	if _, err := store.Current("client"); !errors.Is(err, auth.ErrKeyPairNotFound) {
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
	if got, err := store.Current("client"); err != nil || got.KID != kp1.KID {
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
		Audience:  "api://default",
		Active:    true,
		ValidFrom: time.Now(),
	}
	expiry := time.Now().Add(24 * time.Hour)
	tk2 := &auth.TrustedKey{
		KID:       "tk-2",
		TenantID:  tID,
		PublicKey: &key2.PublicKey,
		Audience:  "api://other",
		Active:    true,
		ValidFrom: time.Now(),
		ValidTo:   &expiry,
	}

	// Register
	if err := store.Register(context.Background(), tk1, auth.RotateOptions{}); err != nil {
		t.Fatalf("Register tk1 failed: %v", err)
	}
	if err := store.Register(context.Background(), tk2, auth.RotateOptions{}); err != nil {
		t.Fatalf("Register tk2 failed: %v", err)
	}

	// Get
	got, err := store.Get(context.Background(), tID, "tk-1")
	if err != nil {
		t.Fatalf("Get tk-1 failed: %v", err)
	}
	if got.KID != "tk-1" || got.Audience != "api://default" || !got.Active {
		t.Errorf("unexpected trusted key: KID=%s Audience=%s Active=%v", got.KID, got.Audience, got.Active)
	}

	// Get not found
	_, err = store.Get(context.Background(), tID, "tk-999")
	if err == nil {
		t.Fatal("expected error for missing trusted key, got nil")
	}

	// List
	all := store.List(tID)
	if len(all) != 2 {
		t.Errorf("expected 2 trusted keys, got %d", len(all))
	}

	// Invalidate (with 0 grace period — ValidTo = now)
	if err := store.Invalidate(context.Background(), tID, "tk-1", 0); err != nil {
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
	all = store.List(tID)
	if len(all) != 1 {
		t.Errorf("expected 1 trusted key after delete, got %d", len(all))
	}

	// Delete not found
	if err := store.Delete(context.Background(), tID, "tk-1"); err == nil {
		t.Fatal("expected error deleting non-existent trusted key, got nil")
	}

	// Invalidate not found
	if err := store.Invalidate(context.Background(), tID, "tk-999", 0); err == nil {
		t.Fatal("expected error invalidating non-existent trusted key, got nil")
	}

	// Reactivate not found
	if err := store.Reactivate(context.Background(), tID, "tk-999", now, now.Add(24*time.Hour)); err == nil {
		t.Fatal("expected error reactivating non-existent trusted key, got nil")
	}
}

// --- M2MClientStore Tests ---

func TestM2MClientStore_CreateGetListVerifySecretResetSecretDelete(t *testing.T) {
	store := auth.NewInMemoryM2MClientStore()

	// Create
	secret, err := store.Create("client-1", spi.TenantID("tenant-abc"), "user-1", []string{"admin", "reader"})
	if err != nil {
		t.Fatalf("Create failed: %v", err)
	}
	if len(secret) != 64 { // 32 bytes = 64 hex chars
		t.Errorf("expected secret length 64, got %d", len(secret))
	}

	// Get
	client, err := store.Get("client-1")
	if err != nil {
		t.Fatalf("Get failed: %v", err)
	}
	if client.ClientID != "client-1" || client.TenantID != spi.TenantID("tenant-abc") || client.UserID != "user-1" {
		t.Errorf("unexpected client: %+v", client)
	}
	if len(client.Roles) != 2 || client.Roles[0] != "admin" || client.Roles[1] != "reader" {
		t.Errorf("unexpected roles: %v", client.Roles)
	}

	// Get not found
	_, err = store.Get("client-999")
	if err == nil {
		t.Fatal("expected error for missing client, got nil")
	}

	// Create second client
	_, err = store.Create("client-2", spi.TenantID("tenant-xyz"), "user-2", []string{"reader"})
	if err != nil {
		t.Fatalf("Create client-2 failed: %v", err)
	}

	// List — scoped per tenant; each tenant sees only its own client.
	listA := store.List(spi.TenantID("tenant-abc"))
	if len(listA) != 1 || listA[0].ClientID != "client-1" {
		t.Errorf("tenant-abc: expected [client-1], got %v", listA)
	}
	listXYZ := store.List(spi.TenantID("tenant-xyz"))
	if len(listXYZ) != 1 || listXYZ[0].ClientID != "client-2" {
		t.Errorf("tenant-xyz: expected [client-2], got %v", listXYZ)
	}

	// VerifySecret — correct
	ok, err := store.VerifySecret("client-1", secret)
	if err != nil {
		t.Fatalf("VerifySecret failed: %v", err)
	}
	if !ok {
		t.Error("expected VerifySecret to return true for correct secret")
	}

	// VerifySecret — wrong
	ok, err = store.VerifySecret("client-1", "wrong-secret")
	if err != nil {
		t.Fatalf("VerifySecret failed: %v", err)
	}
	if ok {
		t.Error("expected VerifySecret to return false for wrong secret")
	}

	// VerifySecret — not found
	_, err = store.VerifySecret("client-999", secret)
	if err == nil {
		t.Fatal("expected error for missing client in VerifySecret, got nil")
	}

	// ResetSecret
	newSecret, err := store.ResetSecret("client-1")
	if err != nil {
		t.Fatalf("ResetSecret failed: %v", err)
	}
	if newSecret == secret {
		t.Error("expected new secret to differ from original")
	}

	// Old secret should no longer work
	ok, _ = store.VerifySecret("client-1", secret)
	if ok {
		t.Error("expected old secret to fail after ResetSecret")
	}

	// New secret should work
	ok, _ = store.VerifySecret("client-1", newSecret)
	if !ok {
		t.Error("expected new secret to work after ResetSecret")
	}

	// ResetSecret not found
	_, err = store.ResetSecret("client-999")
	if err == nil {
		t.Fatal("expected error for missing client in ResetSecret, got nil")
	}

	// Delete
	if err := store.Delete("client-1"); err != nil {
		t.Fatalf("Delete failed: %v", err)
	}
	_, err = store.Get("client-1")
	if err == nil {
		t.Fatal("expected error after Delete, got nil")
	}
	// After deleting client-1 (tenant-abc), that tenant is empty; tenant-xyz is unchanged.
	if got := store.List(spi.TenantID("tenant-abc")); len(got) != 0 {
		t.Errorf("tenant-abc: expected 0 clients after delete, got %d", len(got))
	}
	if got := store.List(spi.TenantID("tenant-xyz")); len(got) != 1 {
		t.Errorf("tenant-xyz: expected 1 client after delete, got %d", len(got))
	}

	// Delete not found
	if err := store.Delete("client-1"); err == nil {
		t.Fatal("expected error deleting non-existent client, got nil")
	}
}

// --- KeyStore (audience-partitioned) Tests ---

func TestKeyStore_Current_AudiencePartition(t *testing.T) {
	boot := newBootstrap(t)
	s := newTestKeyStore(t, boot) // bootstrap signs for "client"
	human := issueWindow(t, s, "human", time.Now(), time.Now().Add(time.Hour))
	got, err := s.Current("human")
	if err != nil || got.KID != human.KID {
		t.Fatalf("Current(human): got=%+v err=%v", got, err)
	}
	if got, err := s.Current("client"); err != nil || got.KID != bootKID(t, boot) {
		t.Fatalf("Current(client): got=%+v err=%v; want the bootstrap key", got, err)
	}
	if _, err := s.Current("robot"); !errors.Is(err, auth.ErrKeyPairNotFound) {
		t.Fatalf("Current(robot): err = %v, want ErrKeyPairNotFound", err)
	}
}

// A key pair issued ahead of time (ValidFrom in the future) is not used to
// sign until its window opens: the current key keeps signing.
func TestKeyStore_Signer_SkipsKeyNotYetValid(t *testing.T) {
	s := newIssuedOnlyStore(t)
	now := time.Now()
	current := issueWindow(t, s, "client", now.Add(-time.Hour), now.Add(2*time.Hour))
	issueWindow(t, s, "client", now.Add(time.Hour), now.Add(2*time.Hour))
	got, _, err := s.Signer("client")
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
		kp := issueWindow(t, s, "client", from, from.Add(2*time.Hour))
		if kp.KID > want {
			want = kp.KID
		}
	}
	for i := 0; i < 50; i++ {
		got, _, err := s.Signer("client")
		if err != nil || got.KID != want {
			t.Fatalf("Signer = %+v, %v; want %s every time", got, err, want)
		}
	}
}

func TestKeyStore_Signer_MaxValidFrom(t *testing.T) {
	s := newIssuedOnlyStore(t)
	now := time.Now()
	issueWindow(t, s, "client", now.Add(-time.Hour), now.Add(time.Hour))
	newer := issueWindow(t, s, "client", now, now.Add(time.Hour))
	got, _, err := s.Signer("client")
	if err != nil || got.KID != newer.KID {
		t.Errorf("expected newer ValidFrom selected, got %+v, %v", got, err)
	}
}

func TestKeyStore_Issue_RotateInvalidatesSiblings(t *testing.T) {
	ctx := systemCtx()
	s := newIssuedOnlyStore(t)
	now := time.Now()
	existing := issueWindow(t, s, "client", now, now.Add(time.Hour))
	fresh, err := s.Issue(ctx, auth.IssueRequest{Audience: "client", ValidFrom: now.Add(time.Second), ValidTo: now.Add(time.Hour), Invalidate: true, GracePeriodSec: 60})
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	if !fresh.Active {
		t.Error("the new key pair must stay active")
	}
	if _, err := s.VerificationKey(existing.KID); !errors.Is(err, auth.ErrKeyPairNotFound) {
		t.Errorf("expected the sibling inactive, VerificationKey err = %v", err)
	}
	old, ok := published(t, s, existing.KID)
	if !ok || old.Active || old.ValidTo == nil || old.ValidTo.After(time.Now().Add(61*time.Second)) {
		t.Errorf("expected the sibling published in a 60s grace period, got %+v (published %v)", old, ok)
	}
}

func TestKeyStore_Issue_RotateNoOp(t *testing.T) {
	s := newIssuedOnlyStore(t)
	now := time.Now()
	fresh, err := s.Issue(systemCtx(), auth.IssueRequest{Audience: "client", ValidFrom: now, ValidTo: now.Add(time.Hour), Invalidate: true, GracePeriodSec: 60})
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	if got, err := s.Current("client"); err != nil || got.KID != fresh.KID || !got.Active {
		t.Errorf("solo Issue with Invalidate=true should still leave the new key active: %+v, %v", got, err)
	}
}

func TestKeyStore_Issue_ConcurrentRotateExactlyOneActive(t *testing.T) {
	ctx := systemCtx()
	s := newIssuedOnlyStore(t)
	base := issueWindow(t, s, "client", time.Now(), time.Now().Add(time.Hour))
	kids := []string{base.KID, "", ""}
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			now := time.Now()
			kp, err := s.Issue(ctx, auth.IssueRequest{Audience: "client", ValidFrom: now.Add(time.Duration(i+1) * time.Millisecond), ValidTo: now.Add(time.Hour), Invalidate: true, GracePeriodSec: 1})
			if err == nil {
				kids[i+1] = kp.KID
			}
		}(i)
	}
	wg.Wait()
	active := 0
	for _, kid := range kids {
		if kid == "" {
			t.Fatal("a concurrent issue failed")
		}
		if _, err := s.VerificationKey(kid); err == nil {
			active++
		}
	}
	if active != 1 {
		t.Errorf("expected exactly 1 active client-audience key, got %d", active)
	}
}

func TestKeyStore_Published_LazyFilter(t *testing.T) {
	s := newIssuedOnlyStore(t)
	now := time.Now()
	past := now.Add(-time.Hour)
	active := issueWindow(t, s, "client", now, now.Add(time.Hour))
	expired := issueWindow(t, s, "client", past.Add(-time.Hour), past)
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
	expired := issueWindow(t, s, "client", past.Add(-time.Hour), past)
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
	kp := issueWindow(t, s, "client", time.Now(), time.Now().Add(time.Hour))
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
	kp := issueWindow(t, s, "client", time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
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
			Audience: "client", ValidFrom: time.Now().Add(-2 * time.Hour), ValidTo: validTo,
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
	tk := &auth.TrustedKey{KID: "k1", TenantID: tA, PublicKey: &priv.PublicKey, Audience: "human", Active: true, ValidFrom: time.Now()}
	if err := s.Register(context.Background(), tk, auth.RotateOptions{}); err != nil {
		t.Fatalf("register: %v", err)
	}
	if _, err := s.Get(context.Background(), tB, "k1"); err == nil {
		t.Error("B.Get(k1) leaked")
	}
	if err := s.Delete(context.Background(), tB, "k1"); err == nil {
		t.Error("B.Delete(k1) leaked")
	}
	if err := s.Invalidate(context.Background(), tB, "k1", 0); err == nil {
		t.Error("B.Invalidate(k1) leaked")
	}
}

func TestTrustedKeyStore_CrossTenantCollision_409(t *testing.T) {
	s := newTestTrustedStore(t)
	priv := testRSAPriv(t)
	tA := spi.TenantID("tenant-a")
	tB := spi.TenantID("tenant-b")
	kA := &auth.TrustedKey{KID: "shared", TenantID: tA, PublicKey: &priv.PublicKey, Audience: "human", Active: true, ValidFrom: time.Now()}
	_ = s.Register(context.Background(), kA, auth.RotateOptions{})
	kB := &auth.TrustedKey{KID: "shared", TenantID: tB, PublicKey: &priv.PublicKey, Audience: "human", Active: true, ValidFrom: time.Now()}
	err := s.Register(context.Background(), kB, auth.RotateOptions{})
	if err == nil {
		t.Fatal("expected cross-tenant error")
	}
	var ae *common.AppError
	if !errors.As(err, &ae) || ae.Code != common.ErrCodeKeyOwnedByDifferentTenant {
		t.Errorf("expected KEY_OWNED_BY_DIFFERENT_TENANT, got %v", err)
	}
}

func TestTrustedKeyStore_CapReached(t *testing.T) {
	s := newTestTrustedStore(t, auth.WithMaxTrustedKeys(2))
	priv := testRSAPriv(t)
	tID := spi.TenantID("t")
	mk := func(kid string) *auth.TrustedKey {
		return &auth.TrustedKey{KID: kid, TenantID: tID, PublicKey: &priv.PublicKey, Audience: "human", Active: true, ValidFrom: time.Now()}
	}
	_ = s.Register(context.Background(), mk("k1"), auth.RotateOptions{})
	_ = s.Register(context.Background(), mk("k2"), auth.RotateOptions{})
	err := s.Register(context.Background(), mk("k3"), auth.RotateOptions{})
	if err == nil {
		t.Fatal("expected cap-reached error")
	}
	var ae *common.AppError
	if !errors.As(err, &ae) || ae.Code != common.ErrCodeTrustedKeyCapReached {
		t.Errorf("expected TRUSTED_KEY_CAP_REACHED, got %v", err)
	}
}

func TestTrustedKeyStore_CapCountsValidOnly(t *testing.T) {
	s := newTestTrustedStore(t, auth.WithMaxTrustedKeys(2))
	priv := testRSAPriv(t)
	tID := spi.TenantID("t")
	past := time.Now().Add(-1 * time.Hour)
	expired := &auth.TrustedKey{KID: "old", TenantID: tID, PublicKey: &priv.PublicKey, Audience: "human", Active: false, ValidFrom: past, ValidTo: &past}
	active := &auth.TrustedKey{KID: "new", TenantID: tID, PublicKey: &priv.PublicKey, Audience: "human", Active: true, ValidFrom: time.Now()}
	_ = s.Register(context.Background(), expired, auth.RotateOptions{})
	_ = s.Register(context.Background(), active, auth.RotateOptions{})
	third := &auth.TrustedKey{KID: "third", TenantID: tID, PublicKey: &priv.PublicKey, Audience: "human", Active: true, ValidFrom: time.Now()}
	if err := s.Register(context.Background(), third, auth.RotateOptions{}); err != nil {
		t.Fatalf("expected accept; expired excluded from count; got %v", err)
	}
}

func TestTrustedKeyStore_RotateInvalidatesSameTenant(t *testing.T) {
	s := newTestTrustedStore(t)
	priv := testRSAPriv(t)
	tID := spi.TenantID("t")
	a := &auth.TrustedKey{KID: "a", TenantID: tID, PublicKey: &priv.PublicKey, Audience: "human", Active: true, ValidFrom: time.Now()}
	_ = s.Register(context.Background(), a, auth.RotateOptions{})
	b := &auth.TrustedKey{KID: "b", TenantID: tID, PublicKey: &priv.PublicKey, Audience: "human", Active: true, ValidFrom: time.Now().Add(1 * time.Second)}
	if err := s.Register(context.Background(), b, auth.RotateOptions{Invalidate: true, GracePeriodSec: 60}); err != nil {
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
	expired := &auth.TrustedKey{KID: "e", TenantID: tID, PublicKey: &priv.PublicKey, Audience: "human", Active: false, ValidFrom: past, ValidTo: &past}
	_ = s.Register(context.Background(), expired, auth.RotateOptions{})
	if err := s.Reactivate(context.Background(), tID, "e", now, past); err == nil {
		t.Error("expected past validTo rejected")
	}
	if err := s.Reactivate(context.Background(), tID, "e", now, now.Add(24*time.Hour)); err != nil {
		t.Fatalf("reactivate: %v", err)
	}
}

// --- M2MClient timestamp + type tests ---

func TestInMemoryM2MClientStore_Create_StampsCreatedAndUpdatedAt(t *testing.T) {
	store := auth.NewInMemoryM2MClientStore()
	before := time.Now()
	_, err := store.Create("client-a", spi.TenantID("tenant-a"), "user-a", []string{"ROLE_M2M"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	after := time.Now()

	c, err := store.Get("client-a")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if c.CreatedAt.Before(before) || c.CreatedAt.After(after) {
		t.Errorf("CreatedAt %v outside [%v, %v]", c.CreatedAt, before, after)
	}
	if !c.UpdatedAt.Equal(c.CreatedAt) {
		t.Errorf("Create: UpdatedAt (%v) should equal CreatedAt (%v) on fresh create", c.UpdatedAt, c.CreatedAt)
	}
}

func TestInMemoryM2MClientStore_CreateWithSecret_StampsCreatedAndUpdatedAt(t *testing.T) {
	store := auth.NewInMemoryM2MClientStore()
	before := time.Now()
	err := store.CreateWithSecret("client-b", spi.TenantID("tenant-b"), "user-b", "secret-b", []string{"ROLE_M2M"})
	if err != nil {
		t.Fatalf("CreateWithSecret: %v", err)
	}
	after := time.Now()

	c, err := store.Get("client-b")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if c.CreatedAt.Before(before) || c.CreatedAt.After(after) {
		t.Errorf("CreatedAt %v outside [%v, %v]", c.CreatedAt, before, after)
	}
	if !c.UpdatedAt.Equal(c.CreatedAt) {
		t.Errorf("CreateWithSecret: UpdatedAt should equal CreatedAt on fresh create")
	}
}

func TestInMemoryM2MClientStore_ResetSecret_AdvancesUpdatedAt(t *testing.T) {
	store := auth.NewInMemoryM2MClientStore()
	if _, err := store.Create("client-c", spi.TenantID("tenant-c"), "user-c", []string{"ROLE_M2M"}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	c0, err := store.Get("client-c")
	if err != nil {
		t.Fatalf("Get c0: %v", err)
	}
	time.Sleep(2 * time.Millisecond) // guarantee monotonic distance
	if _, err := store.ResetSecret("client-c"); err != nil {
		t.Fatalf("ResetSecret: %v", err)
	}
	c1, err := store.Get("client-c")
	if err != nil {
		t.Fatalf("Get c1: %v", err)
	}

	if !c1.UpdatedAt.After(c0.UpdatedAt) {
		t.Errorf("ResetSecret: UpdatedAt did not advance (%v -> %v)", c0.UpdatedAt, c1.UpdatedAt)
	}
	if !c1.CreatedAt.Equal(c0.CreatedAt) {
		t.Errorf("ResetSecret: CreatedAt must not change (%v -> %v)", c0.CreatedAt, c1.CreatedAt)
	}
}

func TestM2MClient_TenantIDIsSpiType(t *testing.T) {
	var c auth.M2MClient
	var _ spi.TenantID = c.TenantID // compile-time check: field must be assignable from spi.TenantID
}
