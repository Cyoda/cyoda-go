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

	key1, _ := rsa.GenerateKey(rand.Reader, 2048)
	key2, _ := rsa.GenerateKey(rand.Reader, 2048)

	tk1 := &auth.TrustedKey{
		KID:       "tk-1",
		PublicKey: &key1.PublicKey,
		Audience:  "api://default",
		Active:    true,
		ValidFrom: time.Now(),
	}
	expiry := time.Now().Add(24 * time.Hour)
	tk2 := &auth.TrustedKey{
		KID:       "tk-2",
		PublicKey: &key2.PublicKey,
		Audience:  "api://other",
		Active:    true,
		ValidFrom: time.Now(),
		ValidTo:   &expiry,
	}

	// Register
	if err := store.Register(tk1); err != nil {
		t.Fatalf("Register tk1 failed: %v", err)
	}
	if err := store.Register(tk2); err != nil {
		t.Fatalf("Register tk2 failed: %v", err)
	}

	// Get
	got, err := store.Get("tk-1")
	if err != nil {
		t.Fatalf("Get tk-1 failed: %v", err)
	}
	if got.KID != "tk-1" || got.Audience != "api://default" || !got.Active {
		t.Errorf("unexpected trusted key: KID=%s Audience=%s Active=%v", got.KID, got.Audience, got.Active)
	}

	// Get not found
	_, err = store.Get("tk-999")
	if err == nil {
		t.Fatal("expected error for missing trusted key, got nil")
	}

	// List
	all := store.List()
	if len(all) != 2 {
		t.Errorf("expected 2 trusted keys, got %d", len(all))
	}

	// Invalidate
	if err := store.Invalidate("tk-1"); err != nil {
		t.Fatalf("Invalidate failed: %v", err)
	}
	got, _ = store.Get("tk-1")
	if got.Active {
		t.Error("expected tk-1 to be inactive after Invalidate")
	}

	// Reactivate
	if err := store.Reactivate("tk-1"); err != nil {
		t.Fatalf("Reactivate failed: %v", err)
	}
	got, _ = store.Get("tk-1")
	if !got.Active {
		t.Error("expected tk-1 to be active after Reactivate")
	}

	// Delete
	if err := store.Delete("tk-1"); err != nil {
		t.Fatalf("Delete failed: %v", err)
	}
	_, err = store.Get("tk-1")
	if err == nil {
		t.Fatal("expected error after Delete, got nil")
	}
	all = store.List()
	if len(all) != 1 {
		t.Errorf("expected 1 trusted key after delete, got %d", len(all))
	}

	// Delete not found
	if err := store.Delete("tk-1"); err == nil {
		t.Fatal("expected error deleting non-existent trusted key, got nil")
	}

	// Invalidate not found
	if err := store.Invalidate("tk-999"); err == nil {
		t.Fatal("expected error invalidating non-existent trusted key, got nil")
	}

	// Reactivate not found
	if err := store.Reactivate("tk-999"); err == nil {
		t.Fatal("expected error reactivating non-existent trusted key, got nil")
	}
}

// TestInMemoryTrustedKeyStore_RegisterEnforcesMaxKeys mirrors the KV-backed
// store's cap test: an admin must not be able to grow the in-memory map
// without bound. The two stores implement the same role and must agree on
// the bound; otherwise tests using the in-memory variant would silently
// permit what production rejects.
func TestInMemoryTrustedKeyStore_RegisterEnforcesMaxKeys(t *testing.T) {
	const cap = 3
	store := auth.NewInMemoryTrustedKeyStore(auth.WithInMemoryMaxTrustedKeys(cap))

	mkKey := func(i int) *auth.TrustedKey {
		k, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			t.Fatalf("GenerateKey: %v", err)
		}
		return &auth.TrustedKey{
			KID:       fmt.Sprintf("cap-key-%d", i),
			PublicKey: &k.PublicKey,
			Audience:  "svc",
			Active:    true,
			ValidFrom: time.Now().UTC(),
		}
	}

	for i := 0; i < cap; i++ {
		if err := store.Register(mkKey(i)); err != nil {
			t.Fatalf("Register #%d (under cap): %v", i, err)
		}
	}

	// (N+1)th must be rejected with a 409 Conflict AppError, matching the
	// KV-backed variant's behaviour.
	overflow := mkKey(cap)
	err := store.Register(overflow)
	if err == nil {
		t.Fatalf("Register beyond cap: expected error, got nil")
	}
	var appErr *common.AppError
	if !errors.As(err, &appErr) {
		t.Fatalf("expected *common.AppError, got %T: %v", err, err)
	}
	if appErr.Status != http.StatusConflict {
		t.Errorf("status = %d, want 409", appErr.Status)
	}
	if appErr.Code != common.ErrCodeConflict {
		t.Errorf("code = %q, want %q", appErr.Code, common.ErrCodeConflict)
	}

	// Existing keys are unaffected — overflow rejection must not corrupt state.
	if got := len(store.List()); got != cap {
		t.Errorf("expected list size %d after overflow rejection, got %d", cap, got)
	}
}

// TestInMemoryTrustedKeyStore_RegisterUpsertExistingDoesNotConsumeSlot
// mirrors the KV-store edge case: re-registering an existing KID at full
// capacity must remain permitted (rotation), only brand-new KIDs trip the cap.
func TestInMemoryTrustedKeyStore_RegisterUpsertExistingDoesNotConsumeSlot(t *testing.T) {
	store := auth.NewInMemoryTrustedKeyStore(auth.WithInMemoryMaxTrustedKeys(2))

	mk := func(kid string) *auth.TrustedKey {
		k, _ := rsa.GenerateKey(rand.Reader, 2048)
		return &auth.TrustedKey{
			KID: kid, PublicKey: &k.PublicKey, Audience: "svc",
			Active: true, ValidFrom: time.Now().UTC(),
		}
	}

	if err := store.Register(mk("k1")); err != nil {
		t.Fatalf("Register k1: %v", err)
	}
	if err := store.Register(mk("k2")); err != nil {
		t.Fatalf("Register k2: %v", err)
	}
	// Re-register existing kid — should succeed even at cap.
	if err := store.Register(mk("k1")); err != nil {
		t.Fatalf("re-Register k1 (upsert at cap): %v", err)
	}
	// New kid still rejected with 409.
	err := store.Register(mk("k3"))
	if err == nil {
		t.Fatalf("Register k3 beyond cap: expected error, got nil")
	}
	var appErr *common.AppError
	if !errors.As(err, &appErr) {
		t.Fatalf("expected *common.AppError, got %T: %v", err, err)
	}
	if appErr.Status != http.StatusConflict {
		t.Errorf("status = %d, want 409", appErr.Status)
	}
}

// --- M2MClientStore Tests ---

func TestM2MClientStore_CreateGetListVerifySecretResetSecretDelete(t *testing.T) {
	store := auth.NewInMemoryM2MClientStore()

	// Create
	secret, err := store.Create("client-1", "tenant-abc", "user-1", []string{"admin", "reader"})
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
	if client.ClientID != "client-1" || client.TenantID != "tenant-abc" || client.UserID != "user-1" {
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
	_, err = store.Create("client-2", "tenant-xyz", "user-2", []string{"reader"})
	if err != nil {
		t.Fatalf("Create client-2 failed: %v", err)
	}

	// List
	all := store.List()
	if len(all) != 2 {
		t.Errorf("expected 2 clients, got %d", len(all))
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
	all = store.List()
	if len(all) != 1 {
		t.Errorf("expected 1 client after delete, got %d", len(all))
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
