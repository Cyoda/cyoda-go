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
)

// registerWindow registers an active key of tenant "ta" with the window
// [from, validTo).
func registerWindow(t *testing.T, ctx context.Context, s auth.TrustedKeyStore, kid string, from time.Time, validTo *time.Time, invalidatePrevious bool) {
	t.Helper()
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	if err := s.Register(ctx, &auth.TrustedKey{
		KID: kid, TenantID: "ta", PublicKey: &priv.PublicKey,
		Active: true, ValidFrom: from, ValidTo: validTo,
	}, invalidatePrevious); err != nil {
		t.Fatalf("register %s: %v", kid, err)
	}
}

// A key is found for verification only in the tenant that registered it,
// only while active, and only inside its window [ValidFrom, ValidTo);
// anything else is ErrTrustedKeyNotFound. An invalidated key is refused at
// once: trusted keys have no grace period.
func TestKVTrustedKeyStore_GetForVerification(t *testing.T) {
	ctx := systemCtx()
	s := newTestTrustedStore(t)
	past, future := time.Now().Add(-time.Hour), time.Now().Add(time.Hour)
	registerWindow(t, ctx, s, "k1", past.Add(-time.Hour), nil, false)
	registerWindow(t, ctx, s, "expired", past.Add(-time.Hour), &past, false)
	registerWindow(t, ctx, s, "ahead", future, nil, false)
	registerWindow(t, ctx, s, "revoked", past, &future, false)
	if err := s.Invalidate(ctx, "ta", "revoked"); err != nil {
		t.Fatalf("invalidate: %v", err)
	}

	got, err := s.GetForVerification(ctx, "ta", "k1")
	if err != nil || got.KID != "k1" || got.TenantID != "ta" {
		t.Fatalf("owner tenant: got=%+v err=%v", got, err)
	}
	for name, c := range map[string]struct {
		tenant spi.TenantID
		kid    string
	}{
		"other-tenant":  {"tb", "k1"},
		"expired":       {"ta", "expired"},
		"not-yet-valid": {"ta", "ahead"},
		"invalidated":   {"ta", "revoked"},
		"missing":       {"ta", "missing"},
	} {
		if _, err := s.GetForVerification(ctx, c.tenant, c.kid); !errors.Is(err, auth.ErrTrustedKeyNotFound) {
			t.Errorf("%s: err = %v, want ErrTrustedKeyNotFound", name, err)
		}
	}
}

// Invalidating — directly or by rotation — only ever shortens a key's
// window: it never lengthens one, and never brings back a key that ended.
func TestKVTrustedKeyStore_InvalidateNeverExtends(t *testing.T) {
	ctx := systemCtx()
	s := newTestTrustedStore(t)
	validTo := func(kid string) time.Time {
		t.Helper()
		k, err := s.Get(ctx, "ta", kid)
		if err != nil || k.ValidTo == nil {
			t.Fatalf("get %s: %+v, %v", kid, k, err)
		}
		return *k.ValidTo
	}

	// A key already past its window keeps its end.
	ended := time.Now().Add(-time.Minute)
	registerWindow(t, ctx, s, "expired", time.Now().Add(-time.Hour), &ended, false)
	if err := s.Invalidate(ctx, "ta", "expired"); err != nil {
		t.Fatalf("invalidate expired: %v", err)
	}
	if got := validTo("expired"); !got.Equal(ended) {
		t.Errorf("invalidating an expired key moved its end: %v, want %v", got, ended)
	}

	// A live key ends now.
	end := time.Now().Add(10 * time.Minute)
	registerWindow(t, ctx, s, "live", time.Now().Add(-time.Hour), &end, false)
	before := time.Now()
	if err := s.Invalidate(ctx, "ta", "live"); err != nil {
		t.Fatalf("invalidate live: %v", err)
	}
	if got := validTo("live"); got.After(time.Now()) || got.Before(before) {
		t.Errorf("invalidated key ends at %v, want now", got)
	}

	// Rotation does not move an expired sibling's end.
	old := time.Now().Add(-time.Minute)
	registerWindow(t, ctx, s, "old", time.Now().Add(-time.Hour), &old, false)
	far := time.Now().Add(time.Hour)
	registerWindow(t, ctx, s, "new", time.Now().Add(-time.Minute), &far, true)
	if got := validTo("old"); !got.Equal(old) {
		t.Errorf("rotation moved an expired sibling's end: %v, want %v", got, old)
	}
}

// The per-tenant cap bounds the keys that can verify a subject token. An
// invalidated key frees its slot at once; reactivating a key makes it verify
// again, so it is held to the same cap.
func TestKVTrustedKeyStore_CapCountsVerifyingKeys(t *testing.T) {
	ctx := systemCtx()
	s := auth.NewKVTrustedKeyStore(mustNewMemoryKV(t, ctx), 2)
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
			KID: kid, TenantID: "ta", PublicKey: &priv.PublicKey,
			Active: true, ValidFrom: time.Now().Add(-time.Hour), ValidTo: &vt,
		}, false)
	}
	for _, kid := range []string{"a", "b"} {
		if err := register(kid); err != nil {
			t.Fatalf("register %s: %v", kid, err)
		}
	}
	if err := register("c"); !capReached(err) {
		t.Fatalf("register c at the cap: err = %v, want TRUSTED_KEY_CAP_REACHED", err)
	}
	if err := s.Invalidate(ctx, "ta", "a"); err != nil {
		t.Fatalf("invalidate a: %v", err)
	}
	if err := register("c"); err != nil {
		t.Fatalf("register c after a was invalidated: %v", err)
	}

	vt := time.Now().Add(time.Hour)
	if err := s.Reactivate(ctx, "ta", "a", time.Now().Add(-time.Minute), vt); !capReached(err) {
		t.Fatalf("reactivate a at the cap: err = %v, want TRUSTED_KEY_CAP_REACHED", err)
	}
	if err := s.Invalidate(ctx, "ta", "c"); err != nil {
		t.Fatalf("invalidate c: %v", err)
	}
	if err := s.Reactivate(ctx, "ta", "a", time.Now().Add(-time.Minute), vt); err != nil {
		t.Fatalf("reactivate a below the cap: %v", err)
	}
}
