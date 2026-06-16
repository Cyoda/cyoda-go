package auth_test

import (
	"crypto/rand"
	"crypto/rsa"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/auth"
	"github.com/cyoda-platform/cyoda-go/internal/common"
)

// --- KeyStore Tests ---

func TestKeyStore_SaveGetGetActiveListInvalidateReactivateDelete(t *testing.T) {
	store := auth.NewInMemoryKeyStore()

	key1, _ := rsa.GenerateKey(rand.Reader, 2048)
	key2, _ := rsa.GenerateKey(rand.Reader, 2048)

	kp1 := &auth.KeyPair{
		KID:        "kid-1",
		Audience:   "client",
		Algorithm:  "RS256",
		PublicKey:  &key1.PublicKey,
		PrivateKey: key1,
		Active:     true,
		ValidFrom:  time.Now(),
	}
	kp2 := &auth.KeyPair{
		KID:        "kid-2",
		Audience:   "client",
		Algorithm:  "RS256",
		PublicKey:  &key2.PublicKey,
		PrivateKey: key2,
		Active:     false,
		ValidFrom:  time.Now(),
	}

	// Save
	if err := store.Save(kp1, auth.RotateOptions{}); err != nil {
		t.Fatalf("Save kp1 failed: %v", err)
	}
	if err := store.Save(kp2, auth.RotateOptions{}); err != nil {
		t.Fatalf("Save kp2 failed: %v", err)
	}

	// Get
	got, err := store.Get("kid-1")
	if err != nil {
		t.Fatalf("Get kid-1 failed: %v", err)
	}
	if got.KID != "kid-1" || !got.Active {
		t.Errorf("unexpected key pair: KID=%s Active=%v", got.KID, got.Active)
	}

	// Get not found
	_, err = store.Get("kid-999")
	if err == nil {
		t.Fatal("expected error for missing key, got nil")
	}

	// GetActive
	active, err := store.GetActive("client")
	if err != nil {
		t.Fatalf("GetActive failed: %v", err)
	}
	if active.KID != "kid-1" {
		t.Errorf("expected active kid-1, got %s", active.KID)
	}

	// List
	all := store.List()
	if len(all) != 2 {
		t.Errorf("expected 2 keys, got %d", len(all))
	}

	// Invalidate
	if err := store.Invalidate("kid-1", 0); err != nil {
		t.Fatalf("Invalidate failed: %v", err)
	}
	got, _ = store.Get("kid-1")
	if got.Active {
		t.Error("expected kid-1 to be inactive after Invalidate")
	}

	// GetActive should fail now (both inactive)
	_, err = store.GetActive("client")
	if err == nil {
		t.Fatal("expected error when no active keys, got nil")
	}

	// Reactivate
	now := time.Now()
	if err := store.Reactivate("kid-1", now, now.Add(24*time.Hour)); err != nil {
		t.Fatalf("Reactivate failed: %v", err)
	}
	got, _ = store.Get("kid-1")
	if !got.Active {
		t.Error("expected kid-1 to be active after Reactivate")
	}

	// Delete
	if err := store.Delete("kid-1"); err != nil {
		t.Fatalf("Delete failed: %v", err)
	}
	_, err = store.Get("kid-1")
	if err == nil {
		t.Fatal("expected error after Delete, got nil")
	}
	all = store.List()
	if len(all) != 1 {
		t.Errorf("expected 1 key after delete, got %d", len(all))
	}

	// Delete not found
	if err := store.Delete("kid-1"); err == nil {
		t.Fatal("expected error deleting non-existent key, got nil")
	}

	// Invalidate not found
	if err := store.Invalidate("kid-999", 0); err == nil {
		t.Fatal("expected error invalidating non-existent key, got nil")
	}

	// Reactivate not found
	futureFrom := time.Now()
	futureTo := futureFrom.Add(24 * time.Hour)
	if err := store.Reactivate("kid-999", futureFrom, futureTo); err == nil {
		t.Fatal("expected error reactivating non-existent key, got nil")
	}
}

// --- TrustedKeyStore Tests ---

func TestTrustedKeyStore_RegisterGetListInvalidateReactivateDelete(t *testing.T) {
	store := auth.NewInMemoryTrustedKeyStore()
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
	if err := store.Register(tk1, auth.RotateOptions{}); err != nil {
		t.Fatalf("Register tk1 failed: %v", err)
	}
	if err := store.Register(tk2, auth.RotateOptions{}); err != nil {
		t.Fatalf("Register tk2 failed: %v", err)
	}

	// Get
	got, err := store.Get(tID, "tk-1")
	if err != nil {
		t.Fatalf("Get tk-1 failed: %v", err)
	}
	if got.KID != "tk-1" || got.Audience != "api://default" || !got.Active {
		t.Errorf("unexpected trusted key: KID=%s Audience=%s Active=%v", got.KID, got.Audience, got.Active)
	}

	// Get not found
	_, err = store.Get(tID, "tk-999")
	if err == nil {
		t.Fatal("expected error for missing trusted key, got nil")
	}

	// List
	all := store.List(tID)
	if len(all) != 2 {
		t.Errorf("expected 2 trusted keys, got %d", len(all))
	}

	// Invalidate (with 0 grace period — ValidTo = now)
	if err := store.Invalidate(tID, "tk-1", 0); err != nil {
		t.Fatalf("Invalidate failed: %v", err)
	}
	got, _ = store.Get(tID, "tk-1")
	if got.Active {
		t.Error("expected tk-1 to be inactive after Invalidate")
	}

	// Reactivate with a fresh validity window
	now := time.Now()
	if err := store.Reactivate(tID, "tk-1", now, now.Add(24*time.Hour)); err != nil {
		t.Fatalf("Reactivate failed: %v", err)
	}
	got, _ = store.Get(tID, "tk-1")
	if !got.Active {
		t.Error("expected tk-1 to be active after Reactivate")
	}

	// Delete
	if err := store.Delete(tID, "tk-1"); err != nil {
		t.Fatalf("Delete failed: %v", err)
	}
	_, err = store.Get(tID, "tk-1")
	if err == nil {
		t.Fatal("expected error after Delete, got nil")
	}
	all = store.List(tID)
	if len(all) != 1 {
		t.Errorf("expected 1 trusted key after delete, got %d", len(all))
	}

	// Delete not found
	if err := store.Delete(tID, "tk-1"); err == nil {
		t.Fatal("expected error deleting non-existent trusted key, got nil")
	}

	// Invalidate not found
	if err := store.Invalidate(tID, "tk-999", 0); err == nil {
		t.Fatal("expected error invalidating non-existent trusted key, got nil")
	}

	// Reactivate not found
	if err := store.Reactivate(tID, "tk-999", now, now.Add(24*time.Hour)); err == nil {
		t.Fatal("expected error reactivating non-existent trusted key, got nil")
	}
}

// TestInMemoryTrustedKeyStore_RegisterEnforcesMaxKeys verifies that the cap
// is enforced per-tenant on currently-valid keys. The new implementation
// returns 400 TRUSTED_KEY_CAP_REACHED (not 409) for cap violations.
func TestInMemoryTrustedKeyStore_RegisterEnforcesMaxKeys(t *testing.T) {
	const capVal = 3
	store := auth.NewInMemoryTrustedKeyStoreWithCap(capVal)
	tID := spi.TenantID("tenant-cap")

	mkKey := func(i int) *auth.TrustedKey {
		k, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			t.Fatalf("GenerateKey: %v", err)
		}
		return &auth.TrustedKey{
			KID:       fmt.Sprintf("cap-key-%d", i),
			TenantID:  tID,
			PublicKey: &k.PublicKey,
			Audience:  "svc",
			Active:    true,
			ValidFrom: time.Now().UTC(),
		}
	}

	for i := 0; i < capVal; i++ {
		if err := store.Register(mkKey(i), auth.RotateOptions{}); err != nil {
			t.Fatalf("Register #%d (under cap): %v", i, err)
		}
	}

	// (N+1)th must be rejected with 400 TRUSTED_KEY_CAP_REACHED.
	overflow := mkKey(capVal)
	err := store.Register(overflow, auth.RotateOptions{})
	if err == nil {
		t.Fatalf("Register beyond cap: expected error, got nil")
	}
	var appErr *common.AppError
	if !errors.As(err, &appErr) {
		t.Fatalf("expected *common.AppError, got %T: %v", err, err)
	}
	if appErr.Status != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", appErr.Status)
	}
	if appErr.Code != common.ErrCodeTrustedKeyCapReached {
		t.Errorf("code = %q, want %q", appErr.Code, common.ErrCodeTrustedKeyCapReached)
	}

	// Existing keys are unaffected — overflow rejection must not corrupt state.
	if got := len(store.List(tID)); got != capVal {
		t.Errorf("expected list size %d after overflow rejection, got %d", capVal, got)
	}
}

// TestInMemoryTrustedKeyStore_RegisterUpsertExistingDoesNotConsumeSlot
// mirrors the KV-store edge case: re-registering an existing KID at full
// capacity must remain permitted (rotation), only brand-new active KIDs trip the cap.
func TestInMemoryTrustedKeyStore_RegisterUpsertExistingDoesNotConsumeSlot(t *testing.T) {
	store := auth.NewInMemoryTrustedKeyStoreWithCap(2)
	tID := spi.TenantID("tenant-upsert")

	mk := func(kid string) *auth.TrustedKey {
		k, _ := rsa.GenerateKey(rand.Reader, 2048)
		return &auth.TrustedKey{
			KID: kid, TenantID: tID, PublicKey: &k.PublicKey, Audience: "svc",
			Active: true, ValidFrom: time.Now().UTC(),
		}
	}

	if err := store.Register(mk("k1"), auth.RotateOptions{}); err != nil {
		t.Fatalf("Register k1: %v", err)
	}
	if err := store.Register(mk("k2"), auth.RotateOptions{}); err != nil {
		t.Fatalf("Register k2: %v", err)
	}
	// Re-register existing kid — should succeed even at cap (upsert, not insert).
	if err := store.Register(mk("k1"), auth.RotateOptions{}); err != nil {
		t.Fatalf("re-Register k1 (upsert at cap): %v", err)
	}
	// New kid still rejected with 400.
	err := store.Register(mk("k3"), auth.RotateOptions{})
	if err == nil {
		t.Fatalf("Register k3 beyond cap: expected error, got nil")
	}
	var appErr *common.AppError
	if !errors.As(err, &appErr) {
		t.Fatalf("expected *common.AppError, got %T: %v", err, err)
	}
	if appErr.Status != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", appErr.Status)
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

func TestKeyStore_GetActive_AudiencePartition(t *testing.T) {
	s := auth.NewInMemoryKeyStore()
	priv := testRSAPriv(t)
	now := time.Now()
	human := &auth.KeyPair{KID: "h1", Audience: "human", Algorithm: "RS256", PublicKey: &priv.PublicKey, PrivateKey: priv, Active: true, ValidFrom: now}
	client := &auth.KeyPair{KID: "c1", Audience: "client", Algorithm: "RS256", PublicKey: &priv.PublicKey, PrivateKey: priv, Active: true, ValidFrom: now}
	if err := s.Save(human, auth.RotateOptions{}); err != nil {
		t.Fatalf("save human: %v", err)
	}
	if err := s.Save(client, auth.RotateOptions{}); err != nil {
		t.Fatalf("save client: %v", err)
	}
	got, err := s.GetActive("human")
	if err != nil || got.KID != "h1" {
		t.Fatalf("GetActive(human): got=%+v err=%v", got, err)
	}
	if _, err := s.GetActive("robot"); err == nil {
		t.Fatal("GetActive(robot) should error")
	}
}

func TestKeyStore_GetActive_MaxValidFrom(t *testing.T) {
	s := auth.NewInMemoryKeyStore()
	priv := testRSAPriv(t)
	older := &auth.KeyPair{KID: "old", Audience: "client", Algorithm: "RS256", PublicKey: &priv.PublicKey, PrivateKey: priv, Active: true, ValidFrom: time.Now().Add(-1 * time.Hour)}
	newer := &auth.KeyPair{KID: "new", Audience: "client", Algorithm: "RS256", PublicKey: &priv.PublicKey, PrivateKey: priv, Active: true, ValidFrom: time.Now()}
	_ = s.Save(older, auth.RotateOptions{})
	_ = s.Save(newer, auth.RotateOptions{})
	got, _ := s.GetActive("client")
	if got.KID != "new" {
		t.Errorf("expected newer ValidFrom selected, got %s", got.KID)
	}
}

func TestKeyStore_Save_RotateInvalidatesSiblings(t *testing.T) {
	s := auth.NewInMemoryKeyStore()
	priv := testRSAPriv(t)
	now := time.Now()
	existing := &auth.KeyPair{KID: "e1", Audience: "client", Algorithm: "RS256", PublicKey: &priv.PublicKey, PrivateKey: priv, Active: true, ValidFrom: now}
	_ = s.Save(existing, auth.RotateOptions{})
	fresh := &auth.KeyPair{KID: "f1", Audience: "client", Algorithm: "RS256", PublicKey: &priv.PublicKey, PrivateKey: priv, Active: true, ValidFrom: now.Add(1 * time.Second)}
	if err := s.Save(fresh, auth.RotateOptions{Invalidate: true, GracePeriodSec: 60}); err != nil {
		t.Fatalf("save: %v", err)
	}
	old, _ := s.Get("e1")
	if old.Active {
		t.Error("expected e1.Active=false")
	}
	if old.ValidTo == nil {
		t.Error("expected e1.ValidTo set")
	}
}

func TestKeyStore_Save_RotateNoOp(t *testing.T) {
	s := auth.NewInMemoryKeyStore()
	priv := testRSAPriv(t)
	fresh := &auth.KeyPair{KID: "alone", Audience: "client", Algorithm: "RS256", PublicKey: &priv.PublicKey, PrivateKey: priv, Active: true, ValidFrom: time.Now()}
	if err := s.Save(fresh, auth.RotateOptions{Invalidate: true, GracePeriodSec: 60}); err != nil {
		t.Fatalf("save: %v", err)
	}
	got, _ := s.Get("alone")
	if !got.Active {
		t.Error("solo Save with Invalidate=true should still leave new key active")
	}
}

func TestKeyStore_Save_ConcurrentRotateExactlyOneActive(t *testing.T) {
	s := auth.NewInMemoryKeyStore()
	priv := testRSAPriv(t)
	baseline := &auth.KeyPair{KID: "base", Audience: "client", Algorithm: "RS256", PublicKey: &priv.PublicKey, PrivateKey: priv, Active: true, ValidFrom: time.Now()}
	_ = s.Save(baseline, auth.RotateOptions{})
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			kp := &auth.KeyPair{KID: fmt.Sprintf("c%d", i), Audience: "client", Algorithm: "RS256", PublicKey: &priv.PublicKey, PrivateKey: priv, Active: true, ValidFrom: time.Now().Add(time.Duration(i+1) * time.Millisecond)}
			_ = s.Save(kp, auth.RotateOptions{Invalidate: true, GracePeriodSec: 1})
		}()
	}
	wg.Wait()
	active := 0
	for _, kid := range []string{"base", "c0", "c1"} {
		if kp, err := s.Get(kid); err == nil && kp.Active {
			active++
		}
	}
	if active != 1 {
		t.Errorf("expected exactly 1 active client-audience key, got %d", active)
	}
}

func TestKeyStore_ListForVerification_LazyFilter(t *testing.T) {
	s := auth.NewInMemoryKeyStore()
	priv := testRSAPriv(t)
	now := time.Now()
	past := now.Add(-1 * time.Hour)
	active := &auth.KeyPair{KID: "active", Audience: "client", Algorithm: "RS256", PublicKey: &priv.PublicKey, PrivateKey: priv, Active: true, ValidFrom: now}
	expired := &auth.KeyPair{KID: "expired", Audience: "client", Algorithm: "RS256", PublicKey: &priv.PublicKey, PrivateKey: priv, Active: false, ValidFrom: past, ValidTo: &past}
	_ = s.Save(active, auth.RotateOptions{})
	_ = s.Save(expired, auth.RotateOptions{})
	got := s.ListForVerification()
	if len(got) != 1 || got[0].KID != "active" {
		t.Fatalf("expected only active, got %+v", got)
	}
}

func TestKeyStore_Reactivate_FreshWindow(t *testing.T) {
	s := auth.NewInMemoryKeyStore()
	priv := testRSAPriv(t)
	now := time.Now()
	past := now.Add(-1 * time.Hour)
	expired := &auth.KeyPair{KID: "e", Audience: "client", Algorithm: "RS256", PublicKey: &priv.PublicKey, PrivateKey: priv, Active: false, ValidFrom: past, ValidTo: &past}
	_ = s.Save(expired, auth.RotateOptions{})
	if err := s.Reactivate("e", now, past); err == nil {
		t.Error("expected past-validTo to reject")
	}
	if err := s.Reactivate("e", now, now.Add(24*time.Hour)); err != nil {
		t.Fatalf("reactivate: %v", err)
	}
	got, _ := s.Get("e")
	if !got.Active {
		t.Error("expected Active=true after reactivate")
	}
}

func TestKeyStore_Reactivate_IdempotentOnActive(t *testing.T) {
	s := auth.NewInMemoryKeyStore()
	priv := testRSAPriv(t)
	kp := &auth.KeyPair{KID: "k", Audience: "client", Algorithm: "RS256", PublicKey: &priv.PublicKey, PrivateKey: priv, Active: true, ValidFrom: time.Now()}
	_ = s.Save(kp, auth.RotateOptions{})
	newWindow := time.Now().Add(48 * time.Hour)
	if err := s.Reactivate("k", time.Now(), newWindow); err != nil {
		t.Fatalf("reactivate on already-active: %v", err)
	}
	got, _ := s.Get("k")
	if !got.Active {
		t.Error("expected Active=true (idempotent)")
	}
	if got.ValidTo == nil || !got.ValidTo.Equal(newWindow) {
		t.Errorf("expected ValidTo updated to %v, got %v", newWindow, got.ValidTo)
	}
}

func TestKeyStore_ListForVerification_IncludesGracePeriodKey(t *testing.T) {
	s := auth.NewInMemoryKeyStore()
	priv := testRSAPriv(t)
	now := time.Now()
	future := now.Add(1 * time.Hour)
	grace := &auth.KeyPair{
		KID: "grace", Audience: "client", Algorithm: "RS256",
		PublicKey: &priv.PublicKey, PrivateKey: priv,
		Active: false, ValidFrom: now.Add(-1 * time.Hour), ValidTo: &future,
	}
	_ = s.Save(grace, auth.RotateOptions{})
	got := s.ListForVerification()
	if len(got) != 1 || got[0].KID != "grace" {
		t.Fatalf("expected grace-period key included, got %+v", got)
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
	s := auth.NewInMemoryTrustedKeyStore()
	priv := testRSAPriv(t)
	tA := spi.TenantID("tenant-a")
	tB := spi.TenantID("tenant-b")
	tk := &auth.TrustedKey{KID: "k1", TenantID: tA, PublicKey: &priv.PublicKey, Audience: "human", Active: true, ValidFrom: time.Now()}
	if err := s.Register(tk, auth.RotateOptions{}); err != nil {
		t.Fatalf("register: %v", err)
	}
	if _, err := s.Get(tB, "k1"); err == nil {
		t.Error("B.Get(k1) leaked")
	}
	if err := s.Delete(tB, "k1"); err == nil {
		t.Error("B.Delete(k1) leaked")
	}
	if err := s.Invalidate(tB, "k1", 0); err == nil {
		t.Error("B.Invalidate(k1) leaked")
	}
}

func TestTrustedKeyStore_CrossTenantCollision_409(t *testing.T) {
	s := auth.NewInMemoryTrustedKeyStore()
	priv := testRSAPriv(t)
	tA := spi.TenantID("tenant-a")
	tB := spi.TenantID("tenant-b")
	kA := &auth.TrustedKey{KID: "shared", TenantID: tA, PublicKey: &priv.PublicKey, Audience: "human", Active: true, ValidFrom: time.Now()}
	_ = s.Register(kA, auth.RotateOptions{})
	kB := &auth.TrustedKey{KID: "shared", TenantID: tB, PublicKey: &priv.PublicKey, Audience: "human", Active: true, ValidFrom: time.Now()}
	err := s.Register(kB, auth.RotateOptions{})
	if err == nil {
		t.Fatal("expected cross-tenant error")
	}
	var ae *common.AppError
	if !errors.As(err, &ae) || ae.Code != common.ErrCodeKeyOwnedByDifferentTenant {
		t.Errorf("expected KEY_OWNED_BY_DIFFERENT_TENANT, got %v", err)
	}
}

func TestTrustedKeyStore_CapReached(t *testing.T) {
	s := auth.NewInMemoryTrustedKeyStoreWithCap(2)
	priv := testRSAPriv(t)
	tID := spi.TenantID("t")
	mk := func(kid string) *auth.TrustedKey {
		return &auth.TrustedKey{KID: kid, TenantID: tID, PublicKey: &priv.PublicKey, Audience: "human", Active: true, ValidFrom: time.Now()}
	}
	_ = s.Register(mk("k1"), auth.RotateOptions{})
	_ = s.Register(mk("k2"), auth.RotateOptions{})
	err := s.Register(mk("k3"), auth.RotateOptions{})
	if err == nil {
		t.Fatal("expected cap-reached error")
	}
	var ae *common.AppError
	if !errors.As(err, &ae) || ae.Code != common.ErrCodeTrustedKeyCapReached {
		t.Errorf("expected TRUSTED_KEY_CAP_REACHED, got %v", err)
	}
}

func TestTrustedKeyStore_CapCountsValidOnly(t *testing.T) {
	s := auth.NewInMemoryTrustedKeyStoreWithCap(2)
	priv := testRSAPriv(t)
	tID := spi.TenantID("t")
	past := time.Now().Add(-1 * time.Hour)
	expired := &auth.TrustedKey{KID: "old", TenantID: tID, PublicKey: &priv.PublicKey, Audience: "human", Active: false, ValidFrom: past, ValidTo: &past}
	active := &auth.TrustedKey{KID: "new", TenantID: tID, PublicKey: &priv.PublicKey, Audience: "human", Active: true, ValidFrom: time.Now()}
	_ = s.Register(expired, auth.RotateOptions{})
	_ = s.Register(active, auth.RotateOptions{})
	third := &auth.TrustedKey{KID: "third", TenantID: tID, PublicKey: &priv.PublicKey, Audience: "human", Active: true, ValidFrom: time.Now()}
	if err := s.Register(third, auth.RotateOptions{}); err != nil {
		t.Fatalf("expected accept; expired excluded from count; got %v", err)
	}
}

func TestTrustedKeyStore_RotateInvalidatesSameTenant(t *testing.T) {
	s := auth.NewInMemoryTrustedKeyStore()
	priv := testRSAPriv(t)
	tID := spi.TenantID("t")
	a := &auth.TrustedKey{KID: "a", TenantID: tID, PublicKey: &priv.PublicKey, Audience: "human", Active: true, ValidFrom: time.Now()}
	_ = s.Register(a, auth.RotateOptions{})
	b := &auth.TrustedKey{KID: "b", TenantID: tID, PublicKey: &priv.PublicKey, Audience: "human", Active: true, ValidFrom: time.Now().Add(1 * time.Second)}
	if err := s.Register(b, auth.RotateOptions{Invalidate: true, GracePeriodSec: 60}); err != nil {
		t.Fatalf("register: %v", err)
	}
	gotA, _ := s.Get(tID, "a")
	if gotA.Active || gotA.ValidTo == nil {
		t.Errorf("expected a invalidated with ValidTo; got %+v", gotA)
	}
}

func TestTrustedKeyStore_Reactivate_RequiresFreshWindow(t *testing.T) {
	s := auth.NewInMemoryTrustedKeyStore()
	priv := testRSAPriv(t)
	tID := spi.TenantID("t")
	now := time.Now()
	past := now.Add(-1 * time.Hour)
	expired := &auth.TrustedKey{KID: "e", TenantID: tID, PublicKey: &priv.PublicKey, Audience: "human", Active: false, ValidFrom: past, ValidTo: &past}
	_ = s.Register(expired, auth.RotateOptions{})
	if err := s.Reactivate(tID, "e", now, past); err == nil {
		t.Error("expected past validTo rejected")
	}
	if err := s.Reactivate(tID, "e", now, now.Add(24*time.Hour)); err != nil {
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
