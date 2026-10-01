package auth_test

import (
	"context"
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/auth"
)

// txProbeKV (records every call whose context carries a transaction) and its
// calls() accessor are defined in kv_m2m_store_test.go, package-shared here.
// A REPEATABLE READ caller transaction can serve a decision read (siblings,
// cap check, prev value for undo) a stale snapshot even though the eventual
// write goes through unjoined, so both the reads and the writes need
// covering, not only the write.

func withCallerTx(ctx context.Context) context.Context {
	return spi.WithTransaction(ctx, &spi.TransactionState{ID: "caller-tx"})
}

// TestKVKeyStore_IgnoresCallerTransaction proves that no KV call KVKeyStore
// makes on behalf of an admin request — including the decision reads
// (changeable/siblingWrites) that run before a write is decided, not only
// the write itself — carries a transaction found in the caller's context.
// Issue with Invalidate exercises siblingWrites; Reactivate/Invalidate
// exercise changeable; Delete exercises its own direct kv.Get.
func TestKVKeyStore_IgnoresCallerTransaction(t *testing.T) {
	boot := newBootstrap(t)
	probe := &txProbeKV{KeyValueStore: mustNewMemoryKV(t, systemCtx())}
	s := newKeyStore(t, probe, boot)

	txCtx := withCallerTx(systemCtx())

	kp, err := s.Issue(txCtx, auth.IssueRequest{
		ValidFrom:  time.Now(),
		ValidTo:    time.Now().Add(time.Hour),
		Invalidate: true, // exercises siblingWrites' store List
	})
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if _, err := s.Reactivate(txCtx, kp.KID, time.Now(), time.Now().Add(2*time.Hour)); err != nil {
		t.Fatalf("Reactivate: %v", err)
	}
	if err := s.Invalidate(txCtx, kp.KID, 0); err != nil {
		t.Fatalf("Invalidate: %v", err)
	}
	if err := s.Delete(txCtx, kp.KID); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	if seen := probe.calls(); len(seen) != 0 {
		t.Fatalf("KV calls made under the caller's transaction: %v", seen)
	}
}

// TestKVTrustedKeyStore_IgnoresCallerTransaction mirrors
// TestKVKeyStore_IgnoresCallerTransaction for the trusted-key store: Register
// with Invalidate exercises storedKeys (the sibling-read decision), Get reads
// the node's copy that Register just populated (a copy hit; it reaches no
// KV), Reactivate/Invalidate exercise storedKey via update, and Delete
// exercises its own storedKey read.
func TestKVTrustedKeyStore_IgnoresCallerTransaction(t *testing.T) {
	probe := &txProbeKV{KeyValueStore: mustNewMemoryKV(t, systemCtx())}
	s, err := auth.NewKVTrustedKeyStore(systemCtx(), probe)
	if err != nil {
		t.Fatalf("NewKVTrustedKeyStore: %v", err)
	}

	tenant := spi.TenantID("acme")
	tk1 := newTrustedKey(t, tenant, "kid-1", time.Now())
	tk2 := newTrustedKey(t, tenant, "kid-2", time.Now())

	txCtx := withCallerTx(systemCtx())

	if err := s.Register(txCtx, tk1, auth.RotateOptions{}); err != nil {
		t.Fatalf("Register tk1: %v", err)
	}
	if err := s.Register(txCtx, tk2, auth.RotateOptions{Invalidate: true}); err != nil {
		t.Fatalf("Register tk2 (invalidate): %v", err)
	}
	if _, err := s.Get(txCtx, tenant, tk1.KID); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if err := s.Reactivate(txCtx, tenant, tk1.KID, time.Now(), time.Now().Add(48*time.Hour)); err != nil {
		t.Fatalf("Reactivate: %v", err)
	}
	if err := s.Invalidate(txCtx, tenant, tk1.KID, 0); err != nil {
		t.Fatalf("Invalidate: %v", err)
	}
	if err := s.Delete(txCtx, tenant, tk2.KID); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	if seen := probe.calls(); len(seen) != 0 {
		t.Fatalf("KV calls made under the caller's transaction: %v", seen)
	}
}
