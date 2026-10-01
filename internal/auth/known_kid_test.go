package auth

import (
	"crypto/rand"
	"crypto/rsa"
	"errors"
	"testing"
	"time"
)

// A kid that names a cyoda-go key pair is never resolved by a later
// validator in the chain, even while that key pair cannot verify (ahead of
// its window, or invalidated past its grace period). Otherwise a tenant's
// OIDC provider could publish a key under a cyoda-go kid and have its
// tokens accepted under that kid.
func TestChainedValidator_KnownKIDThatCannotVerifyDoesNotFallThrough(t *testing.T) {
	boot, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	ks := newTestKeyStore(t, boot)
	now := time.Now()

	ahead, err := ks.Issue(replicaSystemCtx(), IssueRequest{Audience: "client", ValidFrom: now.Add(time.Hour), ValidTo: now.Add(2 * time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	ended, err := ks.Issue(replicaSystemCtx(), IssueRequest{Audience: "client", ValidFrom: now, ValidTo: now.Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	if err := ks.Invalidate(replicaSystemCtx(), ended.KID, 0); err != nil {
		t.Fatal(err)
	}
	bootKID, err := DeriveKID(&boot.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	if err := ks.Invalidate(replicaSystemCtx(), bootKID, 0); err != nil {
		t.Fatal(err)
	}

	for name, kid := range map[string]string{
		"ahead of its window":   ahead.KID,
		"invalidated":           ended.KID,
		"invalidated bootstrap": bootKID,
	} {
		t.Run(name, func(t *testing.T) {
			next := &mockValidator{result: makeUC("idp-user")}
			chain := NewChainedValidator(NewValidatorFromSource(NewLocalKeySource(ks), "cyoda"), next)

			tok := signTokenWithEphemeralKey(t, kid, "cyoda", "u1", "org1", 60)
			if _, err := chain.Validate(tok); err == nil {
				t.Fatal("token under a cyoda-go kid that cannot verify was accepted")
			} else if errors.Is(err, ErrUnknownKID) {
				t.Fatalf("err = %v, want a hard failure, not ErrUnknownKID", err)
			}
			if next.calls != 0 {
				t.Fatalf("next validator called %d times, want 0", next.calls)
			}
		})
	}

	t.Run("unknown kid still falls through", func(t *testing.T) {
		next := &mockValidator{result: makeUC("idp-user")}
		chain := NewChainedValidator(NewValidatorFromSource(NewLocalKeySource(ks), "cyoda"), next)
		if _, err := chain.Validate(signTokenWithEphemeralKey(t, "idp-kid", "cyoda", "u1", "org1", 60)); err != nil {
			t.Fatalf("unknown kid: err = %v, want the next validator's success", err)
		}
		if next.calls != 1 {
			t.Fatalf("next validator called %d times, want 1", next.calls)
		}
	})
}
