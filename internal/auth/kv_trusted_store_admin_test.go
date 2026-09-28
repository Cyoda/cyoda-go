package auth_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
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
	if err := b.ReconcileForTest(ctx); err != nil {
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
	if err := a.ReconcileForTest(ctx); err != nil {
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
	aKey := auth.TrustedKeyKVKeyForTesting(tID, "a")
	beforeA, err := mem.Get(ctx, "trusted-keys", aKey)
	if err != nil {
		t.Fatal(err)
	}
	kv := &failingKV{KeyValueStore: mem, failOn: aKey}
	s, _ := auth.NewKVTrustedKeyStore(ctx, kv)
	err = s.Register(ctx, newTrustedKey(t, tID, "b", time.Now().Add(time.Second)), auth.RotateOptions{Invalidate: true, GracePeriodSec: 60})
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
	afterA, err := mem.Get(ctx, "trusted-keys", aKey)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(beforeA, afterA) {
		t.Fatalf("sibling's stored bytes changed by a failed rotation: before=%s after=%s", beforeA, afterA)
	}
}

// Cross-tenant refusal on the KV store itself: tenant B's Delete, Invalidate
// and Reactivate on tenant A's key each answer ErrTrustedKeyNotFound, and A's
// stored bytes are untouched — read via kv.Get, never through the node's
// copy.
func TestKVTrustedKeyStore_CrossTenantRefusal_DeleteInvalidateReactivate(t *testing.T) {
	ctx := systemCtx()
	kv := mustNewMemoryKV(t, ctx)
	s, _ := auth.NewKVTrustedKeyStore(ctx, kv)
	tA := spi.TenantID("tenant-a")
	tB := spi.TenantID("tenant-b")
	if err := s.Register(ctx, newTrustedKey(t, tA, "k", time.Now()), auth.RotateOptions{}); err != nil {
		t.Fatal(err)
	}
	kvKey := auth.TrustedKeyKVKeyForTesting(tA, "k")
	assertUnchanged := func(t *testing.T, before []byte) {
		t.Helper()
		after, err := kv.Get(ctx, "trusted-keys", kvKey)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(before, after) {
			t.Errorf("tenant A's stored key changed by tenant B's call: before=%s after=%s", before, after)
		}
	}
	snapshot := func(t *testing.T) []byte {
		t.Helper()
		b, err := kv.Get(ctx, "trusted-keys", kvKey)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}

	before := snapshot(t)
	if err := s.Delete(ctx, tB, "k"); !errors.Is(err, auth.ErrTrustedKeyNotFound) {
		t.Errorf("B.Delete(k): err = %v, want ErrTrustedKeyNotFound", err)
	}
	assertUnchanged(t, before)

	before = snapshot(t)
	if err := s.Invalidate(ctx, tB, "k", 0); !errors.Is(err, auth.ErrTrustedKeyNotFound) {
		t.Errorf("B.Invalidate(k): err = %v, want ErrTrustedKeyNotFound", err)
	}
	assertUnchanged(t, before)

	before = snapshot(t)
	future := time.Now().Add(time.Hour)
	if err := s.Reactivate(ctx, tB, "k", time.Now(), future); !errors.Is(err, auth.ErrTrustedKeyNotFound) {
		t.Errorf("B.Reactivate(k): err = %v, want ErrTrustedKeyNotFound", err)
	}
	assertUnchanged(t, before)
}

type getFailKV struct{ spi.KeyValueStore }

func (g getFailKV) Get(context.Context, string, string) ([]byte, error) {
	return nil, fmt.Errorf("store down")
}

// A store failure is not "not found" — but a genuinely absent key, read
// through a working store, still is.
func TestKVTrustedKeyStore_GetStoreFailureIsNotNotFound(t *testing.T) {
	ctx := systemCtx()
	s, _ := auth.NewKVTrustedKeyStore(ctx, getFailKV{mustNewMemoryKV(t, ctx)})
	_, err := s.Get(ctx, "t", "missing")
	if err == nil || errors.Is(err, auth.ErrTrustedKeyNotFound) {
		t.Fatalf("err = %v, want a store error", err)
	}

	working, _ := auth.NewKVTrustedKeyStore(ctx, mustNewMemoryKV(t, ctx))
	if _, err := working.Get(ctx, "t", "missing"); !errors.Is(err, auth.ErrTrustedKeyNotFound) {
		t.Fatalf("working store, absent key: err = %v, want ErrTrustedKeyNotFound", err)
	}
}

func trustedRecords(t *testing.T, kv spi.KeyValueStore) map[string][]byte {
	t.Helper()
	all, err := kv.List(systemCtx(), "trusted-keys")
	if err != nil {
		t.Fatal(err)
	}
	return all
}

// A time the stored record could not be read back with is refused, and
// nothing is written: an unreadable record would otherwise be served by no
// node, and survive in the store.
func TestKVTrustedKeyStore_RefusesUnstorableTime(t *testing.T) {
	ctx := systemCtx()
	kv := mustNewMemoryKV(t, ctx)
	s, err := auth.NewKVTrustedKeyStore(ctx, kv)
	if err != nil {
		t.Fatal(err)
	}
	tID := spi.TenantID("t")
	far := newTrustedKey(t, tID, "far", time.Now())
	vt := year10000
	far.ValidTo = &vt
	if err := s.Register(ctx, far, auth.RotateOptions{}); err == nil {
		t.Fatal("registered a key with validTo in year 10000")
	}
	if err := s.Register(ctx, newTrustedKey(t, tID, "early", yearMinus), auth.RotateOptions{}); err == nil {
		t.Fatal("registered a key with validFrom in year -1")
	}
	if got := trustedRecords(t, kv); len(got) != 0 {
		t.Fatalf("records written: %v", mapKeys(got))
	}
	if err := s.Register(ctx, newTrustedKey(t, tID, "k", time.Now()), auth.RotateOptions{}); err != nil {
		t.Fatal(err)
	}
	before := trustedRecords(t, kv)
	if err := s.Reactivate(ctx, tID, "k", time.Now(), year10000); err == nil {
		t.Fatal("reactivated with validTo in year 10000")
	}
	if err := s.Reactivate(ctx, tID, "k", yearMinus, time.Now().Add(time.Hour)); err == nil {
		t.Fatal("reactivated with validFrom in year -1")
	}
	sameRecords(t, before, trustedRecords(t, kv))
	if _, err := auth.NewKVTrustedKeyStore(ctx, kv); err != nil {
		t.Fatalf("restart: %v", err)
	}
}

// seedUndecodable stores a registered key "good" and an undecodable record
// "bad", both of tenant t.
func seedUndecodable(t *testing.T) (spi.KeyValueStore, spi.TenantID) {
	t.Helper()
	ctx := systemCtx()
	kv := mustNewMemoryKV(t, ctx)
	seed, err := auth.NewKVTrustedKeyStore(ctx, kv)
	if err != nil {
		t.Fatal(err)
	}
	tID := spi.TenantID("t")
	if err := seed.Register(ctx, newTrustedKey(t, tID, "good", time.Now()), auth.RotateOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := kv.Put(ctx, "trusted-keys", auth.TrustedKeyKVKeyForTesting(tID, "bad"), []byte("{")); err != nil {
		t.Fatal(err)
	}
	return kv, tID
}

// An undecodable record never stops a node starting: it is skipped with an
// ERROR, as on a re-read, and only that key is refused (fail closed).
func TestKVTrustedKeyStore_StartupSkipsUndecodable(t *testing.T) {
	kv, tID := seedUndecodable(t)
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	defer slog.SetDefault(prev)
	s, err := auth.NewKVTrustedKeyStore(systemCtx(), kv)
	if err != nil {
		t.Fatalf("startup failed on an undecodable record: %v", err)
	}
	if !strings.Contains(buf.String(), "level=ERROR") || !strings.Contains(buf.String(), "t:bad") {
		t.Fatalf("no ERROR naming the record: %s", buf.String())
	}
	if _, err := s.GetForVerification(tID, "good"); err != nil {
		t.Fatalf("good key not loaded: %v", err)
	}
	if _, err := s.GetForVerification(tID, "bad"); !errors.Is(err, auth.ErrTrustedKeyNotFound) {
		t.Fatalf("undecodable key: err = %v, want ErrTrustedKeyNotFound", err)
	}
}

// The admin can delete an undecodable record: its KV key carries the tenant,
// so the delete stays in the caller's tenant. Invalidate and reactivate must
// rewrite the record and keep failing, but not as not-found.
func TestKVTrustedKeyStore_DeleteUndecodable(t *testing.T) {
	kv, tID := seedUndecodable(t)
	ctx := systemCtx()
	s, err := auth.NewKVTrustedKeyStore(ctx, kv)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Invalidate(ctx, tID, "bad", 0); err == nil || errors.Is(err, auth.ErrTrustedKeyNotFound) {
		t.Fatalf("invalidate: err = %v, want a store error", err)
	}
	if err := s.Reactivate(ctx, tID, "bad", time.Now(), time.Now().Add(time.Hour)); err == nil || errors.Is(err, auth.ErrTrustedKeyNotFound) {
		t.Fatalf("reactivate: err = %v, want a store error", err)
	}
	if err := s.Delete(ctx, "other", "bad"); !errors.Is(err, auth.ErrTrustedKeyNotFound) {
		t.Fatalf("delete from another tenant: err = %v, want ErrTrustedKeyNotFound", err)
	}
	if err := s.Delete(ctx, tID, "bad"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := kv.Get(ctx, "trusted-keys", auth.TrustedKeyKVKeyForTesting(tID, "bad")); !errors.Is(err, spi.ErrNotFound) {
		t.Fatalf("record still stored: %v", err)
	}
	if _, err := s.GetForVerification(tID, "good"); err != nil {
		t.Fatalf("good key lost: %v", err)
	}
}

// Deleting tenant A's undecodable record at KID k must not drop tenant B's
// key k from the node copy: the copy is keyed by bare KID, and Register's
// cross-tenant check cannot see a record that does not decode.
func TestKVTrustedKeyStore_DeleteUndecodableKeepsOtherTenantsKey(t *testing.T) {
	ctx := systemCtx()
	kv := mustNewMemoryKV(t, ctx)
	if err := kv.Put(ctx, "trusted-keys", auth.TrustedKeyKVKeyForTesting("A", "k"), []byte("{")); err != nil {
		t.Fatal(err)
	}
	s, err := auth.NewKVTrustedKeyStore(ctx, kv)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Register(ctx, newTrustedKey(t, "B", "k", time.Now()), auth.RotateOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(ctx, "A", "k"); err != nil {
		t.Fatalf("delete A's record: %v", err)
	}
	if _, err := s.GetForVerification("B", "k"); err != nil {
		t.Fatalf("B's key dropped by A's delete: %v", err)
	}
}

// Construction fails, and never serves an empty copy, when the initial List
// fails.
func TestKVTrustedKeyStore_ConstructionFailsWhenListFails(t *testing.T) {
	kv := &toggleListKV{KeyValueStore: mustNewMemoryKV(t, systemCtx())}
	kv.fail.Store(true)
	if s, err := auth.NewKVTrustedKeyStore(systemCtx(), kv); err == nil || s != nil {
		t.Fatalf("store = %v, err = %v; want a construction error", s, err)
	}
}

// Decode accepts exactly the range encode writes: a stored timestamp whose UTC
// year is outside 1..9999 does not decode, even where RFC 3339 can spell it
// (year 0).
func TestKVTrustedKeyStore_DecodeRefusesUnstorableTime(t *testing.T) {
	ctx := systemCtx()
	kv := mustNewMemoryKV(t, ctx)
	s, err := auth.NewKVTrustedKeyStore(ctx, kv)
	if err != nil {
		t.Fatal(err)
	}
	tID := spi.TenantID("t")
	if err := s.Register(ctx, newTrustedKey(t, tID, "k", time.Now()), auth.RotateOptions{}); err != nil {
		t.Fatal(err)
	}
	key := auth.TrustedKeyKVKeyForTesting(tID, "k")
	good, err := kv.Get(ctx, "trusted-keys", key)
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"validFrom", "validTo"} {
		var rec map[string]any
		if err := json.Unmarshal(good, &rec); err != nil {
			t.Fatal(err)
		}
		rec[field] = "0000-06-01T00:00:00Z"
		b, _ := json.Marshal(rec)
		if err := kv.Put(ctx, "trusted-keys", key, b); err != nil {
			t.Fatal(err)
		}
		r, err := auth.NewKVTrustedKeyStore(ctx, kv)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := r.GetForVerification(tID, "k"); !errors.Is(err, auth.ErrTrustedKeyNotFound) {
			t.Fatalf("%s in year 0 loaded: err = %v", field, err)
		}
		if _, err := r.Get(ctx, tID, "k"); err == nil || errors.Is(err, auth.ErrTrustedKeyNotFound) {
			t.Fatalf("%s in year 0: Get err = %v, want a decode error", field, err)
		}
	}
}
