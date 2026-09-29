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

	got, err := auth.NewLocalKeySource(ks).GetKey(bootKID(t, boot))
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

// An invalidated key pair keeps verifying until the end of its grace period
// (its validTo), and never after. Grace 0 ends it at once.
func TestLocalKeySource_InvalidatedKeyVerifiesThroughGrace(t *testing.T) {
	ks := newTestKeyStore(t, newBootstrap(t))
	src := auth.NewLocalKeySource(ks)

	withGrace := issueWindow(t, ks, "human", time.Now(), time.Now().Add(time.Hour))
	if err := ks.Invalidate(systemCtx(), withGrace.KID, 3600); err != nil {
		t.Fatal(err)
	}
	if _, err := src.GetKey(withGrace.KID); err != nil {
		t.Fatalf("invalidated with grace: still inside the grace period, got %v", err)
	}

	noGrace := issueWindow(t, ks, "human", time.Now(), time.Now().Add(time.Hour))
	if err := ks.Invalidate(systemCtx(), noGrace.KID, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := src.GetKey(noGrace.KID); !errors.Is(err, auth.ErrKeyNotFound) {
		t.Fatalf("invalidated with grace 0: want ErrKeyNotFound, got %v", err)
	}

	// Cutting a running grace period short: invalidate again with 0.
	if err := ks.Invalidate(systemCtx(), withGrace.KID, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := src.GetKey(withGrace.KID); !errors.Is(err, auth.ErrKeyNotFound) {
		t.Fatalf("re-invalidated with 0: want ErrKeyNotFound, got %v", err)
	}
}

func TestLocalKeySource_DeleteDuringGraceEndsVerification(t *testing.T) {
	ks := newTestKeyStore(t, newBootstrap(t))
	src := auth.NewLocalKeySource(ks)
	kp := issueWindow(t, ks, "human", time.Now(), time.Now().Add(time.Hour))
	_ = ks.Invalidate(systemCtx(), kp.KID, 3600)
	if err := ks.Delete(systemCtx(), kp.KID); err != nil {
		t.Fatal(err)
	}
	if _, err := src.GetKey(kp.KID); !errors.Is(err, auth.ErrKeyNotFound) {
		t.Fatalf("deleted during grace: want ErrKeyNotFound, got %v", err)
	}
}

func TestLocalKeySource_ReactivateDuringGrace(t *testing.T) {
	ks := newTestKeyStore(t, newBootstrap(t))
	kp := issueWindow(t, ks, "human", time.Now(), time.Now().Add(time.Hour))
	_ = ks.Invalidate(systemCtx(), kp.KID, 3600)
	got, err := ks.Reactivate(systemCtx(), kp.KID, time.Now(), time.Now().Add(2*time.Hour))
	if err != nil || !got.Active {
		t.Fatalf("reactivate during grace: %v %v", got, err)
	}
}

// An invalidated key pair never signs, even inside its grace period.
func TestKVKeyStore_InvalidatedKeyInGraceNeverSigns(t *testing.T) {
	ks := newTestKeyStore(t, newBootstrap(t))
	kp := issueWindow(t, ks, "human", time.Now(), time.Now().Add(time.Hour))
	_ = ks.Invalidate(systemCtx(), kp.KID, 3600)
	if _, _, err := ks.Signer("human"); !errors.Is(err, auth.ErrKeyPairNotFound) {
		t.Fatalf("signer = %v, want none: the only key pair of the audience is invalidated", err)
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
