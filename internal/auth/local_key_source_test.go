package auth_test

import (
	"errors"
	"testing"
	"time"

	"github.com/cyoda-platform/cyoda-go/internal/auth"
)

func TestLocalKeySource_ReturnsPublicKeyForRegisteredKID(t *testing.T) {
	ks := newTestKeyStore(t, newBootstrap(t))
	kp := issueWindow(t, ks, "client", time.Now(), time.Now().Add(time.Hour))

	src := auth.NewLocalKeySource(ks)
	got, err := src.GetKey(kp.KID)
	if err != nil {
		t.Fatalf("GetKey returned unexpected error: %v", err)
	}
	if got == nil {
		t.Fatal("GetKey returned nil public key")
	}
	if !got.Equal(kp.PublicKey) {
		t.Fatal("GetKey returned a different public key than issued")
	}
}

func TestLocalKeySource_ReturnsBootstrapPublicKey(t *testing.T) {
	boot := newBootstrap(t)
	ks := newTestKeyStore(t, boot)

	got, err := auth.NewLocalKeySource(ks).GetKey(ks.BootstrapKID())
	if err != nil {
		t.Fatalf("GetKey(bootstrap) returned unexpected error: %v", err)
	}
	if !got.Equal(&boot.PublicKey) {
		t.Fatal("GetKey returned a different public key than the bootstrap key")
	}
}

func TestLocalKeySource_ReturnsErrorForUnknownKID(t *testing.T) {
	src := auth.NewLocalKeySource(newTestKeyStore(t, newBootstrap(t)))

	got, err := src.GetKey("nonexistent-kid")
	if !errors.Is(err, auth.ErrKeyNotFound) {
		t.Fatalf("expected ErrKeyNotFound for unknown kid, got %v", err)
	}
	if got != nil {
		t.Fatalf("expected nil key on error, got %v", got)
	}
}

// The returned error matches both ErrKeyNotFound (what callers classify on)
// and the store's own error (the diagnostic).
func TestLocalKeySource_ErrorWrapsBothSentinels(t *testing.T) {
	src := auth.NewLocalKeySource(newTestKeyStore(t, newBootstrap(t)))

	_, err := src.GetKey("nope")
	if !errors.Is(err, auth.ErrKeyNotFound) || !errors.Is(err, auth.ErrKeyPairNotFound) {
		t.Fatalf("err = %v, want both ErrKeyNotFound and ErrKeyPairNotFound", err)
	}
}

// An invalidated signing key (Active=false) must NOT be returned by the key
// source: otherwise the JWT validator would still accept tokens signed by the
// just-invalidated kid — defeating the entire point of Invalidate.
func TestLocalKeySource_RejectsInvalidatedKey(t *testing.T) {
	ks := newTestKeyStore(t, newBootstrap(t))
	kp := issueWindow(t, ks, "client", time.Now(), time.Now().Add(time.Hour))
	src := auth.NewLocalKeySource(ks)

	// Pre-condition: while Active, the key is returned successfully.
	if _, err := src.GetKey(kp.KID); err != nil {
		t.Fatalf("GetKey while active: unexpected error: %v", err)
	}

	// Invalidate the key with a grace period: it stays published for
	// external verifiers, but cyoda itself stops accepting it at once.
	if err := ks.Invalidate(systemCtx(), kp.KID, 3600); err != nil {
		t.Fatalf("Invalidate: %v", err)
	}

	got, err := src.GetKey(kp.KID)
	if !errors.Is(err, auth.ErrKeyNotFound) {
		t.Fatalf("GetKey after Invalidate: expected errors.Is(err, ErrKeyNotFound), got %v", err)
	}
	if got != nil {
		t.Fatalf("GetKey after Invalidate: expected nil key, got %v", got)
	}
}

// A signing key verifies only inside its window [ValidFrom, ValidTo). An
// active key past its ValidTo, or not yet at its ValidFrom, is not returned,
// so tokens it signed stop verifying when its window ends.
func TestLocalKeySource_RejectsKeyOutsideItsWindow(t *testing.T) {
	now := time.Now()
	past, future := now.Add(-time.Hour), now.Add(time.Hour)
	ks := newTestKeyStore(t, newBootstrap(t))
	expired := issueWindow(t, ks, "client", now.Add(-2*time.Hour), past)
	notYet := issueWindow(t, ks, "client", future, future.Add(time.Hour))
	inWindow := issueWindow(t, ks, "client", past, future)

	src := auth.NewLocalKeySource(ks)
	for name, kid := range map[string]string{"expired": expired.KID, "not-yet": notYet.KID} {
		if _, err := src.GetKey(kid); !errors.Is(err, auth.ErrKeyNotFound) {
			t.Errorf("GetKey(%s) = %v, want ErrKeyNotFound", name, err)
		}
	}
	if _, err := src.GetKey(inWindow.KID); err != nil {
		t.Errorf("GetKey(in-window) = %v, want the key", err)
	}
}
