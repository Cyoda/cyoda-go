package auth_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/auth"
)

func newM2M(t *testing.T, max int) (*auth.KVM2MClientStore, spi.KeyValueStore) {
	t.Helper()
	kv := mustNewMemoryKV(t, systemCtx())
	return auth.NewKVM2MClientStore(kv, max), kv
}

func TestKVM2M_Lifecycle(t *testing.T) {
	s, _ := newM2M(t, 0)
	ctx := systemCtx()
	sec, err := s.Create(ctx, "acme", "C1", "C1", []string{"ROLE_M2M"})
	if err != nil {
		t.Fatal(err)
	}
	c, err := s.Authenticate(ctx, "C1", sec)
	if err != nil || c.TenantID != "acme" || c.ClientID != "C1" {
		t.Fatalf("authenticate: %v %v", c, err)
	}
	if _, err := s.Authenticate(ctx, "C1", "wrong"); !errors.Is(err, auth.ErrInvalidClient) {
		t.Fatalf("wrong secret: %v", err)
	}
	list, err := s.List(ctx, "acme")
	if err != nil || len(list) != 1 {
		t.Fatalf("list: %v %v", list, err)
	}
	sec2, c2, err := s.ResetSecret(ctx, "acme", "C1")
	if err != nil || c2.ClientID != "C1" || c2.UpdatedAt.Before(c.CreatedAt) || !c2.CreatedAt.Equal(c.CreatedAt) {
		t.Fatalf("reset: %v %v", c2, err)
	}
	if _, err := s.Authenticate(ctx, "C1", sec); !errors.Is(err, auth.ErrInvalidClient) {
		t.Fatal("old secret still works")
	}
	if _, err := s.Authenticate(ctx, "C1", sec2); err != nil {
		t.Fatal("new secret refused")
	}
	if err := s.Delete(ctx, "acme", "C1"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Authenticate(ctx, "C1", sec2); !errors.Is(err, auth.ErrInvalidClient) {
		t.Fatal("deleted client authenticates")
	}
	if err := s.Delete(ctx, "acme", "C1"); !errors.Is(err, auth.ErrM2MClientNotFound) {
		t.Fatalf("second delete: %v", err)
	}
}

func TestKVM2M_CreateStampsTimestampsAndResetAdvancesUpdatedAt(t *testing.T) {
	s, _ := newM2M(t, 0)
	before := time.Now()
	sec, err := s.Create(systemCtx(), "acme", "C1", "C1", []string{"ROLE_M2M"})
	if err != nil {
		t.Fatal(err)
	}
	after := time.Now()
	c0, err := s.Authenticate(systemCtx(), "C1", sec)
	if err != nil {
		t.Fatal(err)
	}
	if c0.CreatedAt.Before(before) || c0.CreatedAt.After(after) {
		t.Errorf("CreatedAt %v outside [%v, %v]", c0.CreatedAt, before, after)
	}
	if !c0.UpdatedAt.Equal(c0.CreatedAt) {
		t.Errorf("fresh create: UpdatedAt %v != CreatedAt %v", c0.UpdatedAt, c0.CreatedAt)
	}
	time.Sleep(2 * time.Millisecond) // guarantee monotonic distance
	sec2, c1, err := s.ResetSecret(systemCtx(), "acme", "C1")
	if err != nil {
		t.Fatal(err)
	}
	stored, err := s.Authenticate(systemCtx(), "C1", sec2)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []*auth.M2MClient{c1, stored} {
		if !c.UpdatedAt.After(c0.UpdatedAt) {
			t.Errorf("reset: UpdatedAt did not advance (%v -> %v)", c0.UpdatedAt, c.UpdatedAt)
		}
		if !c.CreatedAt.Equal(c0.CreatedAt) {
			t.Errorf("reset: CreatedAt changed (%v -> %v)", c0.CreatedAt, c.CreatedAt)
		}
	}
}

func TestKVM2M_SharedAcrossInstances(t *testing.T) {
	kv := mustNewMemoryKV(t, systemCtx())
	a, b := auth.NewKVM2MClientStore(kv, 0), auth.NewKVM2MClientStore(kv, 0)
	sec, _ := a.Create(systemCtx(), "acme", "C1", "C1", []string{"ROLE_M2M"})
	if _, err := b.Authenticate(systemCtx(), "C1", sec); err != nil {
		t.Fatal("node B does not see node A's client")
	}
	_ = a.Delete(systemCtx(), "acme", "C1")
	if _, err := b.Authenticate(systemCtx(), "C1", sec); !errors.Is(err, auth.ErrInvalidClient) {
		t.Fatal("node B still accepts a client node A deleted")
	}
}

func TestKVM2M_TenantIsolation(t *testing.T) {
	s, _ := newM2M(t, 0)
	sec, _ := s.Create(systemCtx(), "acme", "C1", "C1", []string{"ROLE_M2M"})
	if l, _ := s.List(systemCtx(), "other"); len(l) != 0 {
		t.Fatal("other tenant lists acme's client")
	}
	if err := s.Delete(systemCtx(), "other", "C1"); !errors.Is(err, auth.ErrM2MClientNotFound) {
		t.Fatalf("cross-tenant delete: %v", err)
	}
	if _, _, err := s.ResetSecret(systemCtx(), "other", "C1"); !errors.Is(err, auth.ErrM2MClientNotFound) {
		t.Fatalf("cross-tenant reset: %v", err)
	}
	if _, err := s.Authenticate(systemCtx(), "C1", sec); err != nil {
		t.Fatal("acme's client damaged by another tenant's calls")
	}
}

func TestKVM2M_DeleteNeverTouchesAnotherTenantsIndex(t *testing.T) {
	s, kv := newM2M(t, 0)
	sec, _ := s.Create(systemCtx(), "acme", "C1", "C1", []string{"ROLE_M2M"})
	// An orphan record of the same id in tenant "other" (as a crash could leave).
	if _, err := s.Create(systemCtx(), "other", "C2", "C2", []string{"ROLE_M2M"}); err != nil {
		t.Fatal(err)
	}
	raw, _ := kv.Get(systemCtx(), "m2m-clients:other", "C2")
	_ = kv.Put(systemCtx(), "m2m-clients:other", "C1", []byte(strings.Replace(string(raw), `"C2"`, `"C1"`, 1)))
	if err := s.Delete(systemCtx(), "other", "C1"); err != nil {
		t.Fatalf("delete of other's orphan: %v", err)
	}
	if _, err := s.Authenticate(systemCtx(), "C1", sec); err != nil {
		t.Fatal("acme's client lost its index entry")
	}
}

func TestKVM2M_RecordWithoutIndexNeverAuthenticatesAndIsRemovable(t *testing.T) {
	s, kv := newM2M(t, 0)
	sec, _ := s.Create(systemCtx(), "acme", "C1", "C1", []string{"ROLE_M2M"})
	_ = kv.Delete(systemCtx(), "m2m-client-ids", "C1")
	if _, err := s.Authenticate(systemCtx(), "C1", sec); !errors.Is(err, auth.ErrInvalidClient) {
		t.Fatal("record without index authenticates")
	}
	if _, _, err := s.ResetSecret(systemCtx(), "acme", "C1"); !errors.Is(err, auth.ErrM2MClientNotFound) {
		t.Fatalf("reset of unindexed record: %v", err)
	}
	if l, _ := s.List(systemCtx(), "acme"); len(l) != 1 {
		t.Fatal("unindexed record not listed")
	}
	if err := s.Delete(systemCtx(), "acme", "C1"); err != nil {
		t.Fatalf("delete of unindexed record: %v", err)
	}
	if l, _ := s.List(systemCtx(), "acme"); len(l) != 0 {
		t.Fatal("still listed after delete")
	}
}

func TestKVM2M_UndecodableRecord(t *testing.T) {
	s, kv := newM2M(t, 0)
	_, _ = s.Create(systemCtx(), "acme", "C1", "C1", []string{"ROLE_M2M"})
	_, _ = s.Create(systemCtx(), "acme", "C2", "C2", []string{"ROLE_M2M"})
	_ = kv.Put(systemCtx(), "m2m-clients:acme", "C1", []byte("{"))
	if _, err := s.Authenticate(systemCtx(), "C1", "x"); err == nil || errors.Is(err, auth.ErrInvalidClient) {
		t.Fatalf("authenticate on undecodable: %v, want a store error", err)
	}
	if l, err := s.List(systemCtx(), "acme"); err != nil || len(l) != 1 || l[0].ClientID != "C2" {
		t.Fatalf("list must skip the undecodable record: %v %v", l, err)
	}
	if _, _, err := s.ResetSecret(systemCtx(), "acme", "C1"); err == nil || errors.Is(err, auth.ErrM2MClientNotFound) {
		t.Fatalf("reset on undecodable: %v, want a store error", err)
	}
	if err := s.Delete(systemCtx(), "acme", "C1"); err != nil {
		t.Fatalf("delete must remove an undecodable record of the caller's tenant: %v", err)
	}
}

// An undecodable index entry is the store failing, never "no such client":
// Authenticate, Delete and ResetSecret return a store error.
func TestKVM2M_UndecodableIndexEntry(t *testing.T) {
	s, kv := newM2M(t, 0)
	sec, _ := s.Create(systemCtx(), "acme", "C1", "C1", []string{"ROLE_M2M"})
	_ = kv.Put(systemCtx(), "m2m-client-ids", "C1", []byte("{"))
	if _, err := s.Authenticate(systemCtx(), "C1", sec); err == nil || errors.Is(err, auth.ErrInvalidClient) {
		t.Fatalf("authenticate: %v, want a store error", err)
	}
	if err := s.Delete(systemCtx(), "acme", "C1"); err == nil || errors.Is(err, auth.ErrM2MClientNotFound) {
		t.Fatalf("delete: %v, want a store error", err)
	}
	if _, _, err := s.ResetSecret(systemCtx(), "acme", "C1"); err == nil || errors.Is(err, auth.ErrM2MClientNotFound) {
		t.Fatalf("reset: %v, want a store error", err)
	}
}

func TestKVM2M_Cap(t *testing.T) {
	s, _ := newM2M(t, 2)
	_, _ = s.Create(systemCtx(), "acme", "C1", "C1", []string{"ROLE_M2M"})
	_, _ = s.Create(systemCtx(), "acme", "C2", "C2", []string{"ROLE_M2M"})
	if _, err := s.Create(systemCtx(), "acme", "C3", "C3", []string{"ROLE_M2M"}); !errors.Is(err, auth.ErrM2MClientCapReached) {
		t.Fatalf("third create: %v", err)
	}
	if _, err := s.Create(systemCtx(), "other", "C4", "C4", []string{"ROLE_M2M"}); err != nil {
		t.Fatal("cap is per tenant")
	}
	_ = s.Delete(systemCtx(), "acme", "C1")
	if _, err := s.Create(systemCtx(), "acme", "C3", "C3", []string{"ROLE_M2M"}); err != nil {
		t.Fatalf("after a delete: %v", err)
	}
}

func TestKVM2M_CapZeroIsUnbounded(t *testing.T) {
	s, _ := newM2M(t, 0)
	for i := 0; i < 3; i++ {
		if _, err := s.Create(systemCtx(), "acme", fmt.Sprintf("C%d", i), "u", []string{"ROLE_M2M"}); err != nil {
			t.Fatalf("create %d: %v", i, err)
		}
	}
}

func TestKVM2M_CapHoldsUnderConcurrentCreatesOnOneNode(t *testing.T) {
	s, _ := newM2M(t, 3)
	var wg sync.WaitGroup
	var ok atomic.Int32
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if _, err := s.Create(systemCtx(), "acme", fmt.Sprintf("C%d", i), fmt.Sprintf("C%d", i), []string{"ROLE_M2M"}); err == nil {
				ok.Add(1)
			}
		}(i)
	}
	wg.Wait()
	if ok.Load() != 3 {
		t.Fatalf("%d creates passed the cap of 3", ok.Load())
	}
}

func TestKVM2M_CreateRefusesExistingAndInvalidIDs(t *testing.T) {
	s, kv := newM2M(t, 0)
	_, _ = s.Create(systemCtx(), "acme", "C1", "C1", []string{"ROLE_M2M"})
	if _, err := s.Create(systemCtx(), "other", "C1", "C1", []string{"ROLE_M2M"}); !errors.Is(err, auth.ErrM2MClientExists) {
		t.Fatalf("existing id in another tenant: %v", err)
	}
	_ = kv.Put(systemCtx(), "m2m-client-ids", "C9", []byte("{"))
	if _, err := s.Create(systemCtx(), "acme", "C9", "C9", []string{"ROLE_M2M"}); !errors.Is(err, auth.ErrM2MClientExists) {
		t.Fatalf("undecodable index entry's id: %v", err)
	}
	if _, err := s.Create(systemCtx(), "acme", "a-b", "a-b", []string{"ROLE_M2M"}); !errors.Is(err, auth.ErrInvalidClient) {
		t.Fatalf("id outside the grammar: %v, want ErrInvalidClient", err)
	}
}

// No stored value carries a plaintext secret: not after a create, not after
// a reset.
func TestKVM2M_NoPlaintextSecretStored(t *testing.T) {
	s, kv := newM2M(t, 0)
	sec, err := s.Create(systemCtx(), "acme", "C1", "C1", []string{"ROLE_M2M"})
	if err != nil {
		t.Fatal(err)
	}
	sec2, _, err := s.ResetSecret(systemCtx(), "acme", "C1")
	if err != nil {
		t.Fatal(err)
	}
	for _, ns := range []string{"m2m-clients:acme", "m2m-client-ids"} {
		entries, err := kv.List(systemCtx(), ns)
		if err != nil || len(entries) == 0 {
			t.Fatalf("%s: %v %v", ns, entries, err)
		}
		for k, v := range entries {
			if strings.Contains(string(v), sec) || strings.Contains(string(v), sec2) {
				t.Fatalf("%s/%s holds a plaintext secret", ns, k)
			}
		}
	}
}

// failNSKV fails every Put into one namespace, writing nothing.
type failNSKV struct {
	spi.KeyValueStore
	ns string
}

func (k *failNSKV) Put(ctx context.Context, ns, key string, v []byte) error {
	if ns == k.ns {
		return errors.New("injected: write failed")
	}
	return k.KeyValueStore.Put(ctx, ns, key, v)
}

func TestKVM2M_CreateUndoesARecordWhenTheIndexWriteFails(t *testing.T) {
	mem := mustNewMemoryKV(t, systemCtx())
	s := auth.NewKVM2MClientStore(&failNSKV{KeyValueStore: mem, ns: "m2m-client-ids"}, 0)
	if _, err := s.Create(systemCtx(), "acme", "C1", "C1", []string{"ROLE_M2M"}); err == nil {
		t.Fatal("want error")
	}
	if _, err := mem.Get(systemCtx(), "m2m-clients:acme", "C1"); !errors.Is(err, spi.ErrNotFound) {
		t.Fatal("record left behind")
	}
}

// nsCommitThenFailKV commits every Put into one namespace and then reports
// an error (a timeout after commit).
type nsCommitThenFailKV struct {
	spi.KeyValueStore
	ns string
}

func (k *nsCommitThenFailKV) Put(ctx context.Context, ns, key string, v []byte) error {
	if err := k.KeyValueStore.Put(ctx, ns, key, v); err != nil {
		return err
	}
	if ns == k.ns {
		return errors.New("injected: committed, then failed")
	}
	return nil
}

func TestKVM2M_CreateUndoesAnAmbiguousIndexWrite(t *testing.T) {
	mem := mustNewMemoryKV(t, systemCtx())
	s := auth.NewKVM2MClientStore(&nsCommitThenFailKV{KeyValueStore: mem, ns: "m2m-client-ids"}, 0)
	if _, err := s.Create(systemCtx(), "acme", "C1", "C1", []string{"ROLE_M2M"}); err == nil {
		t.Fatal("want error")
	}
	for _, ns := range []string{"m2m-client-ids", "m2m-clients:acme"} {
		if _, err := mem.Get(systemCtx(), ns, "C1"); !errors.Is(err, spi.ErrNotFound) {
			t.Fatalf("%s/C1 left behind", ns)
		}
	}
}

func TestKVM2M_StoreFailureIsNeverNotFoundOrInvalid(t *testing.T) {
	mem := mustNewMemoryKV(t, systemCtx())
	good := auth.NewKVM2MClientStore(mem, 0)
	sec, _ := good.Create(systemCtx(), "acme", "C1", "C1", []string{"ROLE_M2M"})
	s := auth.NewKVM2MClientStore(brokenKV{}, 0)
	if _, err := s.Authenticate(systemCtx(), "C1", sec); err == nil || errors.Is(err, auth.ErrInvalidClient) {
		t.Fatalf("authenticate: %v", err)
	}
	if _, err := s.List(systemCtx(), "acme"); err == nil {
		t.Fatal("list: want error")
	}
	if err := s.Delete(systemCtx(), "acme", "C1"); err == nil || errors.Is(err, auth.ErrM2MClientNotFound) {
		t.Fatalf("delete: %v", err)
	}
	if _, _, err := s.ResetSecret(systemCtx(), "acme", "C1"); err == nil || errors.Is(err, auth.ErrM2MClientNotFound) {
		t.Fatalf("reset: %v", err)
	}
	if _, err := s.Create(systemCtx(), "acme", "C2", "C2", []string{"ROLE_M2M"}); err == nil || errors.Is(err, auth.ErrM2MClientExists) {
		t.Fatalf("create: %v", err)
	}
	// The store error keeps its storage-unavailable marker, so the adapter
	// can answer 503.
	var su interface{ StorageUnavailable() bool }
	if _, err := s.List(systemCtx(), "acme"); !errors.As(err, &su) || !su.StorageUnavailable() {
		t.Fatalf("list error lost its storage-unavailable marker: %v", err)
	}
}

// brokenKV fails every call with a storage-unavailable error.
type brokenKV struct{}

type unavailable struct{}

func (unavailable) Error() string            { return "storage down" }
func (unavailable) StorageUnavailable() bool { return true }

func (brokenKV) Put(context.Context, string, string, []byte) error       { return unavailable{} }
func (brokenKV) Get(context.Context, string, string) ([]byte, error)     { return nil, unavailable{} }
func (brokenKV) Delete(context.Context, string, string) error            { return unavailable{} }
func (brokenKV) List(context.Context, string) (map[string][]byte, error) { return nil, unavailable{} }

func TestKVM2M_AuthenticateRefusesMalformedIDsWithoutReading(t *testing.T) {
	s := auth.NewKVM2MClientStore(brokenKV{}, 0) // any read would error
	for _, id := range []string{"a\x00b", "\xff", strings.Repeat("A", 101), "a:b", ""} {
		if _, err := s.Authenticate(systemCtx(), id, "x"); !errors.Is(err, auth.ErrInvalidClient) {
			t.Fatalf("%q: %v, want ErrInvalidClient without a store read", id, err)
		}
	}
}

func TestKVM2M_IgnoresCallerTransaction(t *testing.T) {
	s, _ := newM2M(t, 0)
	ctx := spi.WithTransaction(systemCtx(), &spi.TransactionState{}) // a caller's transaction
	sec, err := s.Create(ctx, "acme", "C1", "C1", []string{"ROLE_M2M"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Authenticate(systemCtx(), "C1", sec); err != nil {
		t.Fatal("a write made under a caller's transaction is not visible outside it")
	}
}
