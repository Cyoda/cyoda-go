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
		JWK: map[string]any{"kty": "RSA", "kid": kid}, Active: true, ValidFrom: from}
}

// A failed write of a previous key stops the rotation before the new key is
// written: the new key is absent and the failed key unchanged.
func TestKVTrustedKeyStore_RotationSiblingFailureLeavesNewKeyAbsent(t *testing.T) {
	ctx := systemCtx()
	mem := mustNewMemoryKV(t, ctx)
	tID := spi.TenantID("t")
	if err := auth.NewKVTrustedKeyStore(mem, 0).Register(ctx, newTrustedKey(t, tID, "a", time.Now()), false); err != nil {
		t.Fatal(err)
	}
	beforeA, err := mem.Get(ctx, "trusted-keys:t", "a")
	if err != nil {
		t.Fatal(err)
	}
	s := auth.NewKVTrustedKeyStore(&failingKV{KeyValueStore: mem, failOn: "a"}, 0)
	if err := s.Register(ctx, newTrustedKey(t, tID, "b", time.Now().Add(time.Second)), true); err == nil {
		t.Fatal("expected the sibling failure")
	}
	if _, err := mem.Get(ctx, "trusted-keys:t", "b"); !errors.Is(err, spi.ErrNotFound) {
		t.Fatalf("new key left in the store after a failed rotation: %v", err)
	}
	afterA, err := mem.Get(ctx, "trusted-keys:t", "a")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(beforeA, afterA) {
		t.Fatalf("sibling's stored bytes changed by a failed write: before=%s after=%s", beforeA, afterA)
	}
}

// Cross-tenant refusal on the KV store itself: tenant B's Delete, Invalidate
// and Reactivate on tenant A's key each answer ErrTrustedKeyNotFound, and A's
// stored bytes are untouched.
func TestKVTrustedKeyStore_CrossTenantRefusal_DeleteInvalidateReactivate(t *testing.T) {
	ctx := systemCtx()
	kv := mustNewMemoryKV(t, ctx)
	s := auth.NewKVTrustedKeyStore(kv, 0)
	tA := spi.TenantID("tenant-a")
	tB := spi.TenantID("tenant-b")
	if err := s.Register(ctx, newTrustedKey(t, tA, "k", time.Now()), false); err != nil {
		t.Fatal(err)
	}
	snapshot := func(t *testing.T) []byte {
		t.Helper()
		b, err := kv.Get(ctx, "trusted-keys:tenant-a", "k")
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	assertUnchanged := func(t *testing.T, before []byte) {
		t.Helper()
		if after := snapshot(t); !bytes.Equal(before, after) {
			t.Errorf("tenant A's stored key changed by tenant B's call: before=%s after=%s", before, after)
		}
	}

	before := snapshot(t)
	if err := s.Delete(ctx, tB, "k"); !errors.Is(err, auth.ErrTrustedKeyNotFound) {
		t.Errorf("B.Delete(k): err = %v, want ErrTrustedKeyNotFound", err)
	}
	assertUnchanged(t, before)

	if err := s.Invalidate(ctx, tB, "k"); !errors.Is(err, auth.ErrTrustedKeyNotFound) {
		t.Errorf("B.Invalidate(k): err = %v, want ErrTrustedKeyNotFound", err)
	}
	assertUnchanged(t, before)

	if err := s.Reactivate(ctx, tB, "k", time.Now(), time.Now().Add(time.Hour)); !errors.Is(err, auth.ErrTrustedKeyNotFound) {
		t.Errorf("B.Reactivate(k): err = %v, want ErrTrustedKeyNotFound", err)
	}
	assertUnchanged(t, before)

	if _, err := s.GetForVerification(ctx, tB, "k"); !errors.Is(err, auth.ErrTrustedKeyNotFound) {
		t.Errorf("B.GetForVerification(k): err = %v, want ErrTrustedKeyNotFound", err)
	}
}

type getFailKV struct{ spi.KeyValueStore }

func (g getFailKV) Get(context.Context, string, string) ([]byte, error) {
	return nil, fmt.Errorf("store down")
}

// A store failure is not "not found" — but a genuinely absent key, read
// through a working store, still is.
func TestKVTrustedKeyStore_GetStoreFailureIsNotNotFound(t *testing.T) {
	ctx := systemCtx()
	s := auth.NewKVTrustedKeyStore(getFailKV{mustNewMemoryKV(t, ctx)}, 0)
	if _, err := s.Get(ctx, "t", "missing"); err == nil || errors.Is(err, auth.ErrTrustedKeyNotFound) {
		t.Fatalf("Get: err = %v, want a store error", err)
	}
	if _, err := s.GetForVerification(ctx, "t", "missing"); err == nil || errors.Is(err, auth.ErrTrustedKeyNotFound) {
		t.Fatalf("GetForVerification: err = %v, want a store error", err)
	}

	working := newTestTrustedStore(t)
	if _, err := working.Get(ctx, "t", "missing"); !errors.Is(err, auth.ErrTrustedKeyNotFound) {
		t.Fatalf("working store, absent key: err = %v, want ErrTrustedKeyNotFound", err)
	}
}

func trustedRecords(t *testing.T, kv spi.KeyValueStore, tenant spi.TenantID) map[string][]byte {
	t.Helper()
	all, err := kv.List(systemCtx(), "trusted-keys:"+string(tenant))
	if err != nil {
		t.Fatal(err)
	}
	return all
}

// A time the stored record could not be read back with is refused, and
// nothing is written: an unreadable record would otherwise survive in the
// store and refuse every exchange that names it.
func TestKVTrustedKeyStore_RefusesUnstorableTime(t *testing.T) {
	ctx := systemCtx()
	kv := mustNewMemoryKV(t, ctx)
	s := auth.NewKVTrustedKeyStore(kv, 0)
	tID := spi.TenantID("t")
	far := newTrustedKey(t, tID, "far", time.Now())
	vt := year10000
	far.ValidTo = &vt
	if err := s.Register(ctx, far, false); err == nil {
		t.Fatal("registered a key with validTo in year 10000")
	}
	if err := s.Register(ctx, newTrustedKey(t, tID, "early", yearMinus), false); err == nil {
		t.Fatal("registered a key with validFrom in year -1")
	}
	if got := trustedRecords(t, kv, tID); len(got) != 0 {
		t.Fatalf("records written: %v", mapKeys(got))
	}
	if err := s.Register(ctx, newTrustedKey(t, tID, "k", time.Now()), false); err != nil {
		t.Fatal(err)
	}
	before := trustedRecords(t, kv, tID)
	if err := s.Reactivate(ctx, tID, "k", time.Now(), year10000); err == nil {
		t.Fatal("reactivated with validTo in year 10000")
	}
	if err := s.Reactivate(ctx, tID, "k", yearMinus, time.Now().Add(time.Hour)); err == nil {
		t.Fatal("reactivated with validFrom in year -1")
	}
	sameRecords(t, before, trustedRecords(t, kv, tID))
	if _, err := s.Get(ctx, tID, "k"); err != nil {
		t.Fatalf("key unreadable after refused changes: %v", err)
	}
}

// seedUndecodable stores a registered key "good" and an undecodable record
// "bad", both of tenant t.
func seedUndecodable(t *testing.T) (spi.KeyValueStore, spi.TenantID) {
	t.Helper()
	ctx := systemCtx()
	kv := mustNewMemoryKV(t, ctx)
	tID := spi.TenantID("t")
	if err := auth.NewKVTrustedKeyStore(kv, 0).Register(ctx, newTrustedKey(t, tID, "good", time.Now().Add(-time.Minute)), false); err != nil {
		t.Fatal(err)
	}
	if err := kv.Put(ctx, "trusted-keys:t", "bad", []byte("{")); err != nil {
		t.Fatal(err)
	}
	return kv, tID
}

// An undecodable record is skipped by List with an ERROR naming it, and
// refuses any exchange that names it with a store error (fail closed) — it
// is never read as a key, and never as "no such key". The other keys of the
// tenant are unaffected.
func TestKVTrustedKeyStore_UndecodableRecordFailsClosed(t *testing.T) {
	kv, tID := seedUndecodable(t)
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	defer slog.SetDefault(prev)
	s := auth.NewKVTrustedKeyStore(kv, 0)
	list, err := s.List(systemCtx(), tID)
	if err != nil || len(list) != 1 || list[0].KID != "good" {
		t.Fatalf("List = %v, %v; want only good", list, err)
	}
	if !strings.Contains(buf.String(), "level=ERROR") || !strings.Contains(buf.String(), "bad") {
		t.Fatalf("no ERROR naming the record: %s", buf.String())
	}
	if _, err := s.GetForVerification(systemCtx(), tID, "good"); err != nil {
		t.Fatalf("good key: %v", err)
	}
	if _, err := s.GetForVerification(systemCtx(), tID, "bad"); err == nil || errors.Is(err, auth.ErrTrustedKeyNotFound) {
		t.Fatalf("undecodable key: err = %v, want a store error", err)
	}
}

// The admin can delete an undecodable record: it is in the caller's
// tenant's namespace, so the delete stays in that tenant. Invalidate and
// reactivate must rewrite the record and keep failing, but not as not-found.
func TestKVTrustedKeyStore_DeleteUndecodable(t *testing.T) {
	kv, tID := seedUndecodable(t)
	ctx := systemCtx()
	s := auth.NewKVTrustedKeyStore(kv, 0)
	if err := s.Invalidate(ctx, tID, "bad"); err == nil || errors.Is(err, auth.ErrTrustedKeyNotFound) {
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
	if _, err := kv.Get(ctx, "trusted-keys:t", "bad"); !errors.Is(err, spi.ErrNotFound) {
		t.Fatalf("record still stored: %v", err)
	}
	if _, err := s.GetForVerification(ctx, tID, "good"); err != nil {
		t.Fatalf("good key lost: %v", err)
	}
}

// Deleting tenant A's undecodable record at KID k leaves tenant B's key k
// alone: the two are in different namespaces.
func TestKVTrustedKeyStore_DeleteUndecodableKeepsOtherTenantsKey(t *testing.T) {
	ctx := systemCtx()
	kv := mustNewMemoryKV(t, ctx)
	if err := kv.Put(ctx, "trusted-keys:A", "k", []byte("{")); err != nil {
		t.Fatal(err)
	}
	s := auth.NewKVTrustedKeyStore(kv, 0)
	if err := s.Register(ctx, newTrustedKey(t, "B", "k", time.Now().Add(-time.Minute)), false); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(ctx, "A", "k"); err != nil {
		t.Fatalf("delete A's record: %v", err)
	}
	if _, err := s.GetForVerification(ctx, "B", "k"); err != nil {
		t.Fatalf("B's key dropped by A's delete: %v", err)
	}
}

// Decode accepts exactly the range encode writes: a stored timestamp whose UTC
// year is outside 1..9999 does not decode, even where RFC 3339 can spell it
// (year 0). Such a record is a store error on every read, never a key.
func TestKVTrustedKeyStore_DecodeRefusesUnstorableTime(t *testing.T) {
	ctx := systemCtx()
	kv := mustNewMemoryKV(t, ctx)
	s := auth.NewKVTrustedKeyStore(kv, 0)
	tID := spi.TenantID("t")
	if err := s.Register(ctx, newTrustedKey(t, tID, "k", time.Now()), false); err != nil {
		t.Fatal(err)
	}
	good, err := kv.Get(ctx, "trusted-keys:t", "k")
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
		if err := kv.Put(ctx, "trusted-keys:t", "k", b); err != nil {
			t.Fatal(err)
		}
		if _, err := s.GetForVerification(ctx, tID, "k"); err == nil || errors.Is(err, auth.ErrTrustedKeyNotFound) {
			t.Fatalf("%s in year 0: GetForVerification err = %v, want a decode error", field, err)
		}
		if _, err := s.Get(ctx, tID, "k"); err == nil || errors.Is(err, auth.ErrTrustedKeyNotFound) {
			t.Fatalf("%s in year 0: Get err = %v, want a decode error", field, err)
		}
	}
}
