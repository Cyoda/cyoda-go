package auth_test

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"errors"
	"fmt"
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/auth"
)

func newTrustedKey(t *testing.T, tenant spi.TenantID, kid string, from time.Time) *auth.TrustedKey {
	t.Helper()
	priv, _ := rsa.GenerateKey(rand.Reader, 2048)
	return &auth.TrustedKey{KID: kid, TenantID: tenant, PublicKey: &priv.PublicKey,
		JWK: map[string]any{"kty": "RSA", "kid": kid}, Audience: "human", Active: true, ValidFrom: from}
}

// Node B has not seen A's delete; its invalidate must not write the key back.
func TestKVTrustedKeyStore_StaleCopyCannotResurrectDeleted(t *testing.T) {
	ctx := systemCtx()
	kv := mustNewMemoryKV(t, ctx)
	a, _ := auth.NewKVTrustedKeyStore(ctx, kv)
	b, _ := auth.NewKVTrustedKeyStore(ctx, kv)
	tID := spi.TenantID("t")
	if err := a.Register(ctx, newTrustedKey(t, tID, "k", time.Now()), auth.RotateOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := b.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if err := a.Delete(ctx, tID, "k"); err != nil {
		t.Fatal(err)
	}
	err := b.Invalidate(ctx, tID, "k", 0)
	if !errors.Is(err, auth.ErrTrustedKeyNotFound) {
		t.Fatalf("invalidate on the stale node: err = %v, want ErrTrustedKeyNotFound", err)
	}
	if _, err := kv.Get(ctx, "trusted-keys", auth.TrustedKeyKVKeyForTesting(tID, "k")); !errors.Is(err, spi.ErrNotFound) {
		t.Fatalf("deleted key is back in the store: %v", err)
	}
}

// A rotation on B must invalidate a sibling A registered that B has not seen.
func TestKVTrustedKeyStore_RotationSeesSiblingRegisteredElsewhere(t *testing.T) {
	ctx := systemCtx()
	kv := mustNewMemoryKV(t, ctx)
	a, _ := auth.NewKVTrustedKeyStore(ctx, kv)
	b, _ := auth.NewKVTrustedKeyStore(ctx, kv)
	tID := spi.TenantID("t")
	_ = a.Register(ctx, newTrustedKey(t, tID, "k1", time.Now()), auth.RotateOptions{})
	if err := b.Register(ctx, newTrustedKey(t, tID, "k2", time.Now().Add(time.Second)), auth.RotateOptions{Invalidate: true}); err != nil {
		t.Fatal(err)
	}
	if err := a.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	k1, err := a.Get(ctx, tID, "k1")
	if err != nil {
		t.Fatal(err)
	}
	if k1.Active {
		t.Fatal("sibling registered on another node stayed active after rotation")
	}
}

type slowPutKV struct {
	spi.KeyValueStore
	entered chan struct{}
	release chan struct{}
}

func (s *slowPutKV) Put(ctx context.Context, ns, key string, v []byte) error {
	s.entered <- struct{}{}
	<-s.release
	return s.KeyValueStore.Put(ctx, ns, key, v)
}

// A slow admin write never delays verification.
func TestKVTrustedKeyStore_VerificationNotBlockedBySlowWrite(t *testing.T) {
	ctx := systemCtx()
	mem := mustNewMemoryKV(t, ctx)
	tID := spi.TenantID("t")
	pre, _ := auth.NewKVTrustedKeyStore(ctx, mem)
	_ = pre.Register(ctx, newTrustedKey(t, tID, "live", time.Now()), auth.RotateOptions{})
	kv := &slowPutKV{KeyValueStore: mem, entered: make(chan struct{}), release: make(chan struct{})}
	s, _ := auth.NewKVTrustedKeyStore(ctx, kv)
	go func() { _ = s.Register(ctx, newTrustedKey(t, tID, "new", time.Now()), auth.RotateOptions{}) }()
	<-kv.entered
	done := make(chan error)
	go func() { _, err := s.GetForVerification(tID, "live"); done <- err }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("verification waited for an admin store write")
	}
	close(kv.release)
}

// A failed sibling write undoes the whole rotation.
func TestKVTrustedKeyStore_RotationCompensatesOnSiblingFailure(t *testing.T) {
	ctx := systemCtx()
	mem := mustNewMemoryKV(t, ctx)
	tID := spi.TenantID("t")
	pre, _ := auth.NewKVTrustedKeyStore(ctx, mem)
	_ = pre.Register(ctx, newTrustedKey(t, tID, "a", time.Now()), auth.RotateOptions{})
	kv := &failingKV{KeyValueStore: mem, failOn: auth.TrustedKeyKVKeyForTesting(tID, "a")}
	s, _ := auth.NewKVTrustedKeyStore(ctx, kv)
	err := s.Register(ctx, newTrustedKey(t, tID, "b", time.Now().Add(time.Second)), auth.RotateOptions{Invalidate: true, GracePeriodSec: 60})
	if err == nil {
		t.Fatal("expected the sibling failure")
	}
	if _, err := mem.Get(ctx, "trusted-keys", auth.TrustedKeyKVKeyForTesting(tID, "b")); !errors.Is(err, spi.ErrNotFound) {
		t.Fatalf("new key left in the store after a failed rotation: %v", err)
	}
	if _, err := s.Get(ctx, tID, "b"); !errors.Is(err, auth.ErrTrustedKeyNotFound) {
		t.Fatalf("new key left in the copy: %v", err)
	}
	a, _ := s.Get(ctx, tID, "a")
	if a == nil || !a.Active {
		t.Fatal("sibling changed by a failed rotation")
	}
}

type getFailKV struct{ spi.KeyValueStore }

func (g getFailKV) Get(context.Context, string, string) ([]byte, error) {
	return nil, fmt.Errorf("store down")
}

// A store failure is not "not found".
func TestKVTrustedKeyStore_GetStoreFailureIsNotNotFound(t *testing.T) {
	ctx := systemCtx()
	s, _ := auth.NewKVTrustedKeyStore(ctx, getFailKV{mustNewMemoryKV(t, ctx)})
	_, err := s.Get(ctx, "t", "missing")
	if err == nil || errors.Is(err, auth.ErrTrustedKeyNotFound) {
		t.Fatalf("err = %v, want a store error", err)
	}
}
