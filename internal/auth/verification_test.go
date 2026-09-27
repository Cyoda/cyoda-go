package auth_test

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"errors"
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/auth"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/plugins/memory"
)

func registerForVerification(t *testing.T, ctx context.Context, s auth.TrustedKeyStore, kid string, tenant spi.TenantID, validTo *time.Time) {
	t.Helper()
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	if err := s.Register(ctx, &auth.TrustedKey{
		KID: kid, TenantID: tenant, PublicKey: &priv.PublicKey, Audience: "human",
		Active: validTo == nil, ValidFrom: time.Now().Add(-2 * time.Hour), ValidTo: validTo,
	}, auth.RotateOptions{}); err != nil {
		t.Fatalf("register %s: %v", kid, err)
	}
}

// assertGetForVerification is the contract both stores share: a key is found
// only in the tenant that registered it, and only while within its validity
// window; anything else is ErrTrustedKeyNotFound.
func assertGetForVerification(t *testing.T, ctx context.Context, s auth.TrustedKeyStore) {
	t.Helper()
	past := time.Now().Add(-time.Hour)
	registerForVerification(t, ctx, s, "k1", "ta", nil)
	registerForVerification(t, ctx, s, "expired", "ta", &past)

	got, err := s.GetForVerification("ta", "k1")
	if err != nil || got.KID != "k1" || got.TenantID != "ta" {
		t.Fatalf("owner tenant: got=%+v err=%v", got, err)
	}

	// A key invalidated with a grace period is inactive but still within its
	// window: it is returned, and the caller decides what inactive means.
	registerForVerification(t, ctx, s, "grace", "ta", nil)
	if err := s.Invalidate(ctx, "ta", "grace", 3600); err != nil {
		t.Fatalf("invalidate: %v", err)
	}
	if got, err := s.GetForVerification("ta", "grace"); err != nil || got.Active {
		t.Errorf("grace-period key: got=%+v err=%v, want it returned with Active=false", got, err)
	}
	for name, c := range map[string]struct {
		tenant spi.TenantID
		kid    string
	}{
		"other-tenant": {"tb", "k1"},
		"expired":      {"ta", "expired"},
		"missing":      {"ta", "missing"},
	} {
		if _, err := s.GetForVerification(c.tenant, c.kid); !errors.Is(err, auth.ErrTrustedKeyNotFound) {
			t.Errorf("%s: err = %v, want ErrTrustedKeyNotFound", name, err)
		}
	}
}

// registerWindow registers an active key valid from two hours ago to validTo.
func registerWindow(t *testing.T, ctx context.Context, s auth.TrustedKeyStore, kid string, validTo time.Time, opts auth.RotateOptions) {
	t.Helper()
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	if err := s.Register(ctx, &auth.TrustedKey{
		KID: kid, TenantID: "ta", PublicKey: &priv.PublicKey, Audience: "human",
		Active: true, ValidFrom: time.Now().Add(-2 * time.Hour), ValidTo: &validTo,
	}, opts); err != nil {
		t.Fatalf("register %s: %v", kid, err)
	}
}

// assertInvalidateNeverExtends is the contract both stores share: a grace
// period keeps a key valid for at most that long, and never beyond the end of
// the window it already had. Invalidating — directly or by rotation — can
// only shorten a key's life, never bring back a key that had ended.
func assertInvalidateNeverExtends(t *testing.T, ctx context.Context, s auth.TrustedKeyStore) {
	t.Helper()
	verifiable := func(kid string) bool {
		_, err := s.GetForVerification("ta", kid)
		return err == nil
	}

	// A key already past its window stays dead.
	registerWindow(t, ctx, s, "expired", time.Now().Add(-time.Minute), auth.RotateOptions{})
	if err := s.Invalidate(ctx, "ta", "expired", 3600); err != nil {
		t.Fatalf("invalidate expired: %v", err)
	}
	if verifiable("expired") {
		t.Error("invalidating an expired key with a grace period revived it")
	}

	// An immediate revoke is not undone by a later invalidation with grace.
	registerWindow(t, ctx, s, "revoked", time.Now().Add(time.Hour), auth.RotateOptions{})
	if err := s.Invalidate(ctx, "ta", "revoked", 0); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if err := s.Invalidate(ctx, "ta", "revoked", 3600); err != nil {
		t.Fatalf("invalidate revoked: %v", err)
	}
	if verifiable("revoked") {
		t.Error("a second invalidation with a grace period revived a revoked key")
	}

	// A grace period longer than the remaining window does not lengthen it.
	end := time.Now().Add(10 * time.Minute)
	registerWindow(t, ctx, s, "short", end, auth.RotateOptions{})
	if err := s.Invalidate(ctx, "ta", "short", 3600); err != nil {
		t.Fatalf("invalidate short: %v", err)
	}
	if got := s.List("ta"); !validToAtMost(got, "short", end) {
		t.Error("a grace period extended a key beyond its window")
	}

	// Rotation does not revive an expired sibling.
	registerWindow(t, ctx, s, "old", time.Now().Add(-time.Minute), auth.RotateOptions{})
	registerWindow(t, ctx, s, "new", time.Now().Add(time.Hour), auth.RotateOptions{Invalidate: true, GracePeriodSec: 3600})
	if verifiable("old") {
		t.Error("rotation with a grace period revived an expired sibling")
	}

	// Rotation also shortens a sibling that is already in a grace period: a
	// rotation with no grace ends every other key of the tenant now.
	registerWindow(t, ctx, s, "graced", time.Now().Add(time.Hour), auth.RotateOptions{})
	if err := s.Invalidate(ctx, "ta", "graced", 3600); err != nil {
		t.Fatalf("invalidate graced: %v", err)
	}
	registerWindow(t, ctx, s, "newest", time.Now().Add(time.Hour), auth.RotateOptions{Invalidate: true, GracePeriodSec: 0})
	if verifiable("graced") {
		t.Error("rotation with no grace left a sibling in its grace period verifying")
	}
}

func validToAtMost(keys []*auth.TrustedKey, kid string, limit time.Time) bool {
	for _, k := range keys {
		if k.KID == kid {
			return k.ValidTo != nil && !k.ValidTo.After(limit)
		}
	}
	return false
}

// assertCapCountsVerifyingKeys: the per-tenant cap bounds the keys that can
// verify a subject token. A key in its grace period after invalidation still
// verifies, so it still takes a slot; a key invalidated with no grace does
// not. The store must have a cap of 2.
func assertCapCountsVerifyingKeys(t *testing.T, ctx context.Context, s auth.TrustedKeyStore) {
	t.Helper()
	capReached := func(err error) bool {
		var ae *common.AppError
		return errors.As(err, &ae) && ae.Code == common.ErrCodeTrustedKeyCapReached
	}
	register := func(kid string) error {
		priv, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			t.Fatalf("generate key: %v", err)
		}
		vt := time.Now().Add(time.Hour)
		return s.Register(ctx, &auth.TrustedKey{
			KID: kid, TenantID: "ta", PublicKey: &priv.PublicKey, Audience: "human",
			Active: true, ValidFrom: time.Now().Add(-time.Hour), ValidTo: &vt,
		}, auth.RotateOptions{})
	}
	for _, kid := range []string{"a", "b"} {
		if err := register(kid); err != nil {
			t.Fatalf("register %s: %v", kid, err)
		}
	}
	if err := s.Invalidate(ctx, "ta", "a", 3600); err != nil {
		t.Fatalf("invalidate a with grace: %v", err)
	}
	if err := register("c"); !capReached(err) {
		t.Fatalf("register c with a still verifying in its grace period: err = %v, want TRUSTED_KEY_CAP_REACHED", err)
	}
	if err := s.Invalidate(ctx, "ta", "a", 0); err != nil {
		t.Fatalf("invalidate a at once: %v", err)
	}
	if err := register("c"); err != nil {
		t.Fatalf("register c after a stopped verifying: %v", err)
	}

	// Reactivating a key makes it verify again, so it is held to the same
	// cap: with b and c verifying, a cannot come back.
	vt := time.Now().Add(time.Hour)
	if err := s.Reactivate(ctx, "ta", "a", time.Now().Add(-time.Minute), vt); !capReached(err) {
		t.Fatalf("reactivate a at the cap: err = %v, want TRUSTED_KEY_CAP_REACHED", err)
	}
	if err := s.Invalidate(ctx, "ta", "c", 0); err != nil {
		t.Fatalf("invalidate c at once: %v", err)
	}
	if err := s.Reactivate(ctx, "ta", "a", time.Now().Add(-time.Minute), vt); err != nil {
		t.Fatalf("reactivate a below the cap: %v", err)
	}
}

func TestInMemoryTrustedKeyStore_CapCountsVerifyingKeys(t *testing.T) {
	assertCapCountsVerifyingKeys(t, context.Background(), auth.NewInMemoryTrustedKeyStoreWithCap(2))
}

func TestKVTrustedKeyStore_CapCountsVerifyingKeys(t *testing.T) {
	ctx := systemCtx()
	kv, err := memory.NewStoreFactory().KeyValueStore(ctx)
	if err != nil {
		t.Fatalf("KeyValueStore: %v", err)
	}
	s, err := auth.NewKVTrustedKeyStore(ctx, kv, auth.WithMaxTrustedKeys(2))
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	assertCapCountsVerifyingKeys(t, ctx, s)
}

func TestInMemoryTrustedKeyStore_GetForVerification(t *testing.T) {
	assertGetForVerification(t, context.Background(), auth.NewInMemoryTrustedKeyStore())
}

func TestInMemoryTrustedKeyStore_InvalidateNeverExtends(t *testing.T) {
	assertInvalidateNeverExtends(t, context.Background(), auth.NewInMemoryTrustedKeyStore())
}

func TestKVTrustedKeyStore_InvalidateNeverExtends(t *testing.T) {
	ctx := systemCtx()
	kv, err := memory.NewStoreFactory().KeyValueStore(ctx)
	if err != nil {
		t.Fatalf("KeyValueStore: %v", err)
	}
	s, err := auth.NewKVTrustedKeyStore(ctx, kv)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	assertInvalidateNeverExtends(t, ctx, s)
}

func TestKVTrustedKeyStore_GetForVerification(t *testing.T) {
	ctx := systemCtx()
	kv, err := memory.NewStoreFactory().KeyValueStore(ctx)
	if err != nil {
		t.Fatalf("KeyValueStore: %v", err)
	}
	s, err := auth.NewKVTrustedKeyStore(ctx, kv)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	assertGetForVerification(t, ctx, s)
}
