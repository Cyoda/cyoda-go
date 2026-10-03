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

// testSecretLimit is the secret-check bound of the stores these tests build.
var testSecretLimit = auth.SecretCheckLimit{Slots: 4, Wait: time.Second}

func newM2M(t *testing.T, max int) (*auth.KVM2MClientStore, spi.KeyValueStore) {
	t.Helper()
	kv := mustNewMemoryKV(t, systemCtx())
	return auth.NewKVM2MClientStore(kv, max, testSecretLimit), kv
}

func TestKVM2M_Lifecycle(t *testing.T) {
	s, _ := newM2M(t, 0)
	ctx := systemCtx()
	sec, err := s.Create(ctx, "acme", "C1", "C1", []string{"ROLE_M2M"}, false)
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
	sec, err := s.Create(systemCtx(), "acme", "C1", "C1", []string{"ROLE_M2M"}, false)
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
	a, b := auth.NewKVM2MClientStore(kv, 0, testSecretLimit), auth.NewKVM2MClientStore(kv, 0, testSecretLimit)
	sec, _ := a.Create(systemCtx(), "acme", "C1", "C1", []string{"ROLE_M2M"}, false)
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
	sec, _ := s.Create(systemCtx(), "acme", "C1", "C1", []string{"ROLE_M2M"}, false)
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
	sec, _ := s.Create(systemCtx(), "acme", "C1", "C1", []string{"ROLE_M2M"}, false)
	// An orphan record of the same id in tenant "other" (as a crash could leave).
	if _, err := s.Create(systemCtx(), "other", "C2", "C2", []string{"ROLE_M2M"}, false); err != nil {
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
	sec, _ := s.Create(systemCtx(), "acme", "C1", "C1", []string{"ROLE_M2M"}, false)
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
	_, _ = s.Create(systemCtx(), "acme", "C1", "C1", []string{"ROLE_M2M"}, false)
	_, _ = s.Create(systemCtx(), "acme", "C2", "C2", []string{"ROLE_M2M"}, false)
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
// Authenticate returns a store error, and so does ResetSecret when the
// caller's namespace holds a record for the id (without one, see
// TestKVM2M_ResetSecretWithoutAnOwnRecordIsNotFoundBeforeTheIndexIsRead).
// Delete does too, unless the caller's own tenant namespace holds a record
// for the id — see TestKVM2M_DeleteRemovesADamagedIndexEntryOfTheCallersOwnClient.
func TestKVM2M_UndecodableIndexEntry(t *testing.T) {
	s, kv := newM2M(t, 0)
	sec, _ := s.Create(systemCtx(), "acme", "C1", "C1", []string{"ROLE_M2M"}, false)
	_ = kv.Put(systemCtx(), "m2m-client-ids", "C1", []byte("{"))
	if _, err := s.Authenticate(systemCtx(), "C1", sec); err == nil || errors.Is(err, auth.ErrInvalidClient) {
		t.Fatalf("authenticate: %v, want a store error", err)
	}
	if _, _, err := s.ResetSecret(systemCtx(), "acme", "C1"); err == nil || errors.Is(err, auth.ErrM2MClientNotFound) {
		t.Fatalf("reset: %v, want a store error", err)
	}
}

// A reset needs a record in the caller's own namespace, so without one it is
// "no such client" before the index entry is read: another tenant's damaged
// index entry is not visible to the caller as a store error.
func TestKVM2M_ResetSecretWithoutAnOwnRecordIsNotFoundBeforeTheIndexIsRead(t *testing.T) {
	s, kv := newM2M(t, 0)
	_ = kv.Put(systemCtx(), "m2m-client-ids", "C1", []byte("{"))
	if _, _, err := s.ResetSecret(systemCtx(), "acme", "C1"); !errors.Is(err, auth.ErrM2MClientNotFound) {
		t.Fatalf("reset: %v, want ErrM2MClientNotFound", err)
	}
	if got, err := kv.Get(systemCtx(), "m2m-client-ids", "C1"); err != nil || string(got) != "{" {
		t.Fatalf("index entry touched: %q %v", got, err)
	}
}

// The caller's own tenant namespace holding a record for the id proves
// ownership, the same rule Delete already applies to a damaged record: a
// damaged index entry is removed along with the record, and the id becomes
// creatable again.
func TestKVM2M_DeleteRemovesADamagedIndexEntryOfTheCallersOwnClient(t *testing.T) {
	s, kv := newM2M(t, 0)
	_, _ = s.Create(systemCtx(), "acme", "C1", "C1", []string{"ROLE_M2M"}, false)
	_ = kv.Put(systemCtx(), "m2m-client-ids", "C1", []byte("{"))
	if err := s.Delete(systemCtx(), "acme", "C1"); err != nil {
		t.Fatalf("delete: %v, want nil", err)
	}
	if _, err := kv.Get(systemCtx(), "m2m-client-ids", "C1"); !errors.Is(err, spi.ErrNotFound) {
		t.Fatal("damaged index entry left behind")
	}
	if _, err := kv.Get(systemCtx(), "m2m-clients:acme", "C1"); !errors.Is(err, spi.ErrNotFound) {
		t.Fatal("record left behind")
	}
	if _, err := s.Create(systemCtx(), "acme", "C1", "C1", []string{"ROLE_M2M"}, false); err != nil {
		t.Fatalf("create after delete: %v", err)
	}
}

// With no record in the caller's own tenant namespace, ownership cannot be
// proven: the damaged index entry may name another tenant, so it is never
// touched and Delete keeps returning the store error.
func TestKVM2M_DeleteKeepsAStoreErrorWhenOwnershipCannotBeProven(t *testing.T) {
	s, kv := newM2M(t, 0)
	_ = kv.Put(systemCtx(), "m2m-client-ids", "C1", []byte("{"))
	err := s.Delete(systemCtx(), "acme", "C1")
	if err == nil || errors.Is(err, auth.ErrM2MClientNotFound) {
		t.Fatalf("delete: %v, want a store error", err)
	}
	if _, err := kv.Get(systemCtx(), "m2m-client-ids", "C1"); err != nil {
		t.Fatalf("index entry touched though ownership could not be proven: %v", err)
	}
}

// A mere read failure on the index entry is not proof the entry is damaged:
// unlike an undecodable entry, it carries no evidence about what the entry
// names, so it is never treated as "the caller's own damaged entry" even
// with an own record present. Delete must keep returning the store error and
// touch neither the index entry nor the record.
func TestKVM2M_DeleteKeepsAStoreErrorOnAnIndexReadFailureEvenWithAnOwnRecord(t *testing.T) {
	mem := mustNewMemoryKV(t, systemCtx())
	good := auth.NewKVM2MClientStore(mem, 0, testSecretLimit)
	if _, err := good.Create(systemCtx(), "acme", "C1", "C1", []string{"ROLE_M2M"}, false); err != nil {
		t.Fatal(err)
	}
	s := auth.NewKVM2MClientStore(&failNSKV{KeyValueStore: mem, ns: "m2m-client-ids", failGet: true}, 0, testSecretLimit)
	err := s.Delete(systemCtx(), "acme", "C1")
	if err == nil || errors.Is(err, auth.ErrM2MClientNotFound) {
		t.Fatalf("delete: %v, want a store error", err)
	}
	if _, err := mem.Get(systemCtx(), "m2m-client-ids", "C1"); err != nil {
		t.Fatalf("index entry touched on a mere read failure: %v", err)
	}
	if _, err := mem.Get(systemCtx(), "m2m-clients:acme", "C1"); err != nil {
		t.Fatalf("record touched on a mere index read failure: %v", err)
	}
}

// A damaged record in the caller's own tenant namespace already proves
// ownership on its own (TestKVM2M_UndecodableRecord); with the index entry
// also damaged, both are removed and Delete returns nil.
func TestKVM2M_DeleteRemovesADamagedRecordAndDamagedIndexEntryOfTheCallersOwnClient(t *testing.T) {
	s, kv := newM2M(t, 0)
	_ = kv.Put(systemCtx(), "m2m-clients:acme", "C1", []byte("{"))
	_ = kv.Put(systemCtx(), "m2m-client-ids", "C1", []byte("{"))
	if err := s.Delete(systemCtx(), "acme", "C1"); err != nil {
		t.Fatalf("delete: %v, want nil", err)
	}
	if _, err := kv.Get(systemCtx(), "m2m-client-ids", "C1"); !errors.Is(err, spi.ErrNotFound) {
		t.Fatal("damaged index entry left behind")
	}
	if _, err := kv.Get(systemCtx(), "m2m-clients:acme", "C1"); !errors.Is(err, spi.ErrNotFound) {
		t.Fatal("damaged record left behind")
	}
}

func TestKVM2M_Cap(t *testing.T) {
	s, _ := newM2M(t, 2)
	_, _ = s.Create(systemCtx(), "acme", "C1", "C1", []string{"ROLE_M2M"}, false)
	_, _ = s.Create(systemCtx(), "acme", "C2", "C2", []string{"ROLE_M2M"}, false)
	if _, err := s.Create(systemCtx(), "acme", "C3", "C3", []string{"ROLE_M2M"}, false); !errors.Is(err, auth.ErrM2MClientCapReached) {
		t.Fatalf("third create: %v", err)
	}
	if _, err := s.Create(systemCtx(), "other", "C4", "C4", []string{"ROLE_M2M"}, false); err != nil {
		t.Fatal("cap is per tenant")
	}
	_ = s.Delete(systemCtx(), "acme", "C1")
	if _, err := s.Create(systemCtx(), "acme", "C3", "C3", []string{"ROLE_M2M"}, false); err != nil {
		t.Fatalf("after a delete: %v", err)
	}
}

func TestKVM2M_CapZeroIsUnbounded(t *testing.T) {
	s, _ := newM2M(t, 0)
	for i := 0; i < 3; i++ {
		if _, err := s.Create(systemCtx(), "acme", fmt.Sprintf("C%d", i), "u", []string{"ROLE_M2M"}, false); err != nil {
			t.Fatalf("create %d: %v", i, err)
		}
	}
}

// slowListKV makes List — the cap read — slow after it has read, so the
// count it returns is stale by the time the caller writes: concurrent creates
// overlap between the cap read and their writes unless something serialises
// them.
type slowListKV struct {
	spi.KeyValueStore
	delay time.Duration
}

func (k *slowListKV) List(ctx context.Context, ns string) (map[string][]byte, error) {
	out, err := k.KeyValueStore.List(ctx, ns)
	time.Sleep(k.delay)
	return out, err
}

func TestKVM2M_CapHoldsUnderConcurrentCreatesOnOneNode(t *testing.T) {
	s := auth.NewKVM2MClientStore(&slowListKV{KeyValueStore: mustNewMemoryKV(t, systemCtx()), delay: 30 * time.Millisecond}, 3, testSecretLimit)
	var wg sync.WaitGroup
	var ok atomic.Int32
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if _, err := s.Create(systemCtx(), "acme", fmt.Sprintf("C%d", i), fmt.Sprintf("C%d", i), []string{"ROLE_M2M"}, false); err == nil {
				ok.Add(1)
			}
		}(i)
	}
	wg.Wait()
	if ok.Load() != 3 {
		t.Fatalf("%d creates passed the cap of 3", ok.Load())
	}
	if l, err := s.List(systemCtx(), "acme"); err != nil || len(l) != 3 {
		t.Fatalf("tenant holds %d clients (%v), want 3", len(l), err)
	}
}

// Create refuses a taken id and an id whose index entry does not decode. An
// id outside the grammar fails in the encoder, before anything is written.
func TestKVM2M_CreateRefusesExistingAndInvalidIDs(t *testing.T) {
	s, kv := newM2M(t, 0)
	_, _ = s.Create(systemCtx(), "acme", "C1", "C1", []string{"ROLE_M2M"}, false)
	if _, err := s.Create(systemCtx(), "other", "C1", "C1", []string{"ROLE_M2M"}, false); !errors.Is(err, auth.ErrM2MClientExists) {
		t.Fatalf("existing id in another tenant: %v", err)
	}
	_ = kv.Put(systemCtx(), "m2m-client-ids", "C9", []byte("{"))
	if _, err := s.Create(systemCtx(), "acme", "C9", "C9", []string{"ROLE_M2M"}, false); !errors.Is(err, auth.ErrM2MClientExists) {
		t.Fatalf("undecodable index entry's id: %v", err)
	}
	if _, err := s.Create(systemCtx(), "acme", "a-b", "a-b", []string{"ROLE_M2M"}, false); err == nil {
		t.Fatal("id outside the grammar: created")
	}
	if _, err := kv.Get(systemCtx(), "m2m-client-ids", "a-b"); !errors.Is(err, spi.ErrNotFound) {
		t.Fatalf("id outside the grammar: index entry written (%v)", err)
	}
	if _, err := kv.Get(systemCtx(), "m2m-clients:acme", "a-b"); !errors.Is(err, spi.ErrNotFound) {
		t.Fatalf("id outside the grammar: record written (%v)", err)
	}
}

// No stored value carries a plaintext secret: not after a create, not after
// a reset.
func TestKVM2M_NoPlaintextSecretStored(t *testing.T) {
	s, kv := newM2M(t, 0)
	sec, err := s.Create(systemCtx(), "acme", "C1", "C1", []string{"ROLE_M2M"}, false)
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

// failNSKV fails every Put into one namespace, writing nothing. With failGet
// set, it fails every Get from that namespace too, reading nothing.
type failNSKV struct {
	spi.KeyValueStore
	ns      string
	failGet bool
}

func (k *failNSKV) Put(ctx context.Context, ns, key string, v []byte) error {
	if ns == k.ns {
		return errors.New("injected: write failed")
	}
	return k.KeyValueStore.Put(ctx, ns, key, v)
}

func (k *failNSKV) Get(ctx context.Context, ns, key string) ([]byte, error) {
	if k.failGet && ns == k.ns {
		return nil, errors.New("injected: read failed")
	}
	return k.KeyValueStore.Get(ctx, ns, key)
}

func TestKVM2M_CreateUndoesARecordWhenTheIndexWriteFails(t *testing.T) {
	mem := mustNewMemoryKV(t, systemCtx())
	s := auth.NewKVM2MClientStore(&failNSKV{KeyValueStore: mem, ns: "m2m-client-ids"}, 0, testSecretLimit)
	if _, err := s.Create(systemCtx(), "acme", "C1", "C1", []string{"ROLE_M2M"}, false); err == nil {
		t.Fatal("want error")
	}
	if _, err := mem.Get(systemCtx(), "m2m-clients:acme", "C1"); !errors.Is(err, spi.ErrNotFound) {
		t.Fatal("record left behind")
	}
}

// nsCommitThenFailKV commits every Put into one namespace and then reports
// an error (a timeout after commit). If then is set, it runs between the
// commit and the error, keyed by the written key.
type nsCommitThenFailKV struct {
	spi.KeyValueStore
	ns   string
	then func(key string)
}

func (k *nsCommitThenFailKV) Put(ctx context.Context, ns, key string, v []byte) error {
	if err := k.KeyValueStore.Put(ctx, ns, key, v); err != nil {
		return err
	}
	if ns == k.ns {
		if k.then != nil {
			k.then(key)
		}
		return errors.New("injected: committed, then failed")
	}
	return nil
}

// An ambiguous record write — committed, then reported as failed — leaves
// nothing behind: the record is removed. The index entry is not this call's
// to remove: it never wrote one, and an entry written meanwhile by another
// node's create of the same id in another tenant must survive.
func TestKVM2M_CreateUndoesAnAmbiguousRecordWrite(t *testing.T) {
	mem := mustNewMemoryKV(t, systemCtx())
	otherIdx := []byte(`{"tenantId":"other"}`)
	s := auth.NewKVM2MClientStore(&nsCommitThenFailKV{KeyValueStore: mem, ns: "m2m-clients:acme", then: func(key string) {
		if err := mem.Put(systemCtx(), "m2m-client-ids", key, otherIdx); err != nil {
			t.Error(err)
		}
	}}, 0, testSecretLimit)
	if _, err := s.Create(systemCtx(), "acme", "C1", "C1", []string{"ROLE_M2M"}, false); err == nil {
		t.Fatal("want error")
	}
	if _, err := mem.Get(systemCtx(), "m2m-clients:acme", "C1"); !errors.Is(err, spi.ErrNotFound) {
		t.Error("record left behind")
	}
	if got, err := mem.Get(systemCtx(), "m2m-client-ids", "C1"); err != nil || string(got) != string(otherIdx) {
		t.Errorf("index entry touched: %q %v", got, err)
	}
}

func TestKVM2M_CreateUndoesAnAmbiguousIndexWrite(t *testing.T) {
	mem := mustNewMemoryKV(t, systemCtx())
	s := auth.NewKVM2MClientStore(&nsCommitThenFailKV{KeyValueStore: mem, ns: "m2m-client-ids"}, 0, testSecretLimit)
	if _, err := s.Create(systemCtx(), "acme", "C1", "C1", []string{"ROLE_M2M"}, false); err == nil {
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
	good := auth.NewKVM2MClientStore(mem, 0, testSecretLimit)
	sec, _ := good.Create(systemCtx(), "acme", "C1", "C1", []string{"ROLE_M2M"}, false)
	s := auth.NewKVM2MClientStore(brokenKV{}, 0, testSecretLimit)
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
	if _, err := s.Create(systemCtx(), "acme", "C2", "C2", []string{"ROLE_M2M"}, false); err == nil || errors.Is(err, auth.ErrM2MClientExists) {
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
	s := auth.NewKVM2MClientStore(brokenKV{}, 0, testSecretLimit) // any read would error
	for _, id := range []string{"a\x00b", "\xff", strings.Repeat("A", 101), "a:b", ""} {
		if _, err := s.Authenticate(systemCtx(), id, "x"); !errors.Is(err, auth.ErrInvalidClient) {
			t.Fatalf("%q: %v, want ErrInvalidClient without a store read", id, err)
		}
	}
}

// txProbeKV records every call whose context carries a transaction: the
// postgres KV store would join it.
type txProbeKV struct {
	spi.KeyValueStore
	mu   sync.Mutex
	seen []string
}

func (k *txProbeKV) probe(ctx context.Context, op, ns string) {
	if spi.GetTransaction(ctx) == nil {
		return
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	k.seen = append(k.seen, op+" "+ns)
}

func (k *txProbeKV) calls() []string {
	k.mu.Lock()
	defer k.mu.Unlock()
	return append([]string(nil), k.seen...)
}

func (k *txProbeKV) Put(ctx context.Context, ns, key string, v []byte) error {
	k.probe(ctx, "Put", ns)
	return k.KeyValueStore.Put(ctx, ns, key, v)
}

func (k *txProbeKV) Get(ctx context.Context, ns, key string) ([]byte, error) {
	k.probe(ctx, "Get", ns)
	return k.KeyValueStore.Get(ctx, ns, key)
}

func (k *txProbeKV) Delete(ctx context.Context, ns, key string) error {
	k.probe(ctx, "Delete", ns)
	return k.KeyValueStore.Delete(ctx, ns, key)
}

func (k *txProbeKV) List(ctx context.Context, ns string) (map[string][]byte, error) {
	k.probe(ctx, "List", ns)
	return k.KeyValueStore.List(ctx, ns)
}

// No KV call made by the store carries the caller's transaction: not from
// any method, and not from the undo of a failed create.
func TestKVM2M_IgnoresCallerTransaction(t *testing.T) {
	txCtx := spi.WithTransaction(systemCtx(), &spi.TransactionState{ID: "caller-tx"})

	probe := &txProbeKV{KeyValueStore: mustNewMemoryKV(t, systemCtx())}
	s := auth.NewKVM2MClientStore(probe, 10, testSecretLimit) // a cap, so Create lists too
	sec, err := s.Create(txCtx, "acme", "C1", "C1", []string{"ROLE_M2M"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Authenticate(txCtx, "C1", sec); err != nil {
		t.Fatal(err)
	}
	_, _ = s.Authenticate(txCtx, "NOPE", "x") // the decoy read
	if _, err := s.List(txCtx, "acme"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.ResetSecret(txCtx, "acme", "C1"); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(txCtx, "acme", "C1"); err != nil {
		t.Fatal(err)
	}

	undoProbe := &txProbeKV{KeyValueStore: &failNSKV{KeyValueStore: mustNewMemoryKV(t, systemCtx()), ns: "m2m-client-ids"}}
	if _, err := auth.NewKVM2MClientStore(undoProbe, 0, testSecretLimit).Create(txCtx, "acme", "C2", "C2", []string{"ROLE_M2M"}, false); err == nil {
		t.Fatal("create with a failing index write: want error")
	}

	if seen := append(probe.calls(), undoProbe.calls()...); len(seen) != 0 {
		t.Fatalf("KV calls made under the caller's transaction: %v", seen)
	}
}

// A client created with onBehalfOf=true carries it and starts at SecretGen 1;
// a reset increments SecretGen without touching OnBehalfOf; Lookup reads the
// current record without a secret, and an absent id is ErrM2MClientNotFound.
func TestKVM2MClientStore_OnBehalfOfAndSecretGen(t *testing.T) {
	s := auth.NewKVM2MClientStore(mustNewMemoryKV(t, systemCtx()), 0, testSecretLimit)
	ctx := systemCtx()
	sec, err := s.Create(ctx, "acme", "C1", "C1", []string{"ROLE_M2M"}, true)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	c, err := s.Authenticate(ctx, "C1", sec)
	if err != nil || !c.OnBehalfOf || c.SecretGen != 1 {
		t.Fatalf("after create: %+v, %v", c, err)
	}
	_, c, err = s.ResetSecret(ctx, "acme", "C1")
	if err != nil || c.SecretGen != 2 || !c.OnBehalfOf {
		t.Fatalf("after reset: %+v, %v", c, err)
	}
	got, err := s.Lookup(ctx, "C1")
	if err != nil || got.SecretGen != 2 || got.TenantID != "acme" {
		t.Fatalf("lookup: %+v, %v", got, err)
	}
	if _, err := s.Lookup(ctx, "NOPE"); !errors.Is(err, auth.ErrM2MClientNotFound) {
		t.Fatalf("lookup absent: %v", err)
	}
}

// A store failure on Lookup keeps its storage-unavailable marker, so a
// caller (the compute-stream re-check) can tell it apart from "gone".
func TestKVM2MClientStore_LookupKeepsUnavailableMarker(t *testing.T) {
	s := auth.NewKVM2MClientStore(brokenKV{}, 0, testSecretLimit)
	_, err := s.Lookup(systemCtx(), "C1")
	var su interface{ StorageUnavailable() bool }
	if !errors.As(err, &su) || !su.StorageUnavailable() {
		t.Fatalf("Lookup error lost its storage-unavailable marker: %v", err)
	}
}

// cancelThenFailKV commits the first Put into ns, then cancels the caller's
// context and reports an error — a caller that gave up while the write
// landed. Its Put and Delete fail, writing nothing, once the context they are
// given is done.
type cancelThenFailKV struct {
	spi.KeyValueStore
	ns     string
	cancel context.CancelFunc
	fired  bool
}

func (k *cancelThenFailKV) Put(ctx context.Context, ns, key string, v []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := k.KeyValueStore.Put(ctx, ns, key, v); err != nil {
		return err
	}
	if ns == k.ns && !k.fired {
		k.fired = true
		k.cancel()
		return errors.New("injected: committed, then the caller went away")
	}
	return nil
}

func (k *cancelThenFailKV) Delete(ctx context.Context, ns, key string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return k.KeyValueStore.Delete(ctx, ns, key)
}

// The undo of a failed create runs on a context the caller cannot cancel: a
// caller that went away does not leave the index entry or the record behind.
func TestKVM2M_CreateUndoSurvivesCallerCancel(t *testing.T) {
	mem := mustNewMemoryKV(t, systemCtx())
	ctx, cancel := context.WithCancel(systemCtx())
	defer cancel()
	s := auth.NewKVM2MClientStore(&cancelThenFailKV{KeyValueStore: mem, ns: "m2m-client-ids", cancel: cancel}, 0, testSecretLimit)
	if _, err := s.Create(ctx, "acme", "C1", "C1", []string{"ROLE_M2M"}, false); err == nil {
		t.Fatal("want error")
	}
	for _, ns := range []string{"m2m-client-ids", "m2m-clients:acme"} {
		if _, err := mem.Get(systemCtx(), ns, "C1"); !errors.Is(err, spi.ErrNotFound) {
			t.Errorf("%s/C1 left behind", ns)
		}
	}
}

// The undo of an ambiguous record write runs on a context the caller cannot
// cancel: a caller that went away does not leave the record behind.
func TestKVM2M_CreateRecordUndoSurvivesCallerCancel(t *testing.T) {
	mem := mustNewMemoryKV(t, systemCtx())
	ctx, cancel := context.WithCancel(systemCtx())
	defer cancel()
	s := auth.NewKVM2MClientStore(&cancelThenFailKV{KeyValueStore: mem, ns: "m2m-clients:acme", cancel: cancel}, 0, testSecretLimit)
	if _, err := s.Create(ctx, "acme", "C1", "C1", []string{"ROLE_M2M"}, false); err == nil {
		t.Fatal("want error")
	}
	for _, ns := range []string{"m2m-clients:acme", "m2m-client-ids"} {
		if _, err := mem.Get(systemCtx(), ns, "C1"); !errors.Is(err, spi.ErrNotFound) {
			t.Errorf("%s/C1 left behind", ns)
		}
	}
}

// An ambiguous reset write — committed, then reported as failed — does not
// end the old secret: the record read before the change is restored, and the
// caller still gets the error.
func TestKVM2M_ResetSecretRestoresTheRecordAfterAnAmbiguousWrite(t *testing.T) {
	mem := mustNewMemoryKV(t, systemCtx())
	good := auth.NewKVM2MClientStore(mem, 0, testSecretLimit)
	sec, err := good.Create(systemCtx(), "acme", "C1", "C1", []string{"ROLE_M2M"}, false)
	if err != nil {
		t.Fatal(err)
	}
	s := auth.NewKVM2MClientStore(&nsCommitThenFailKV{KeyValueStore: mem, ns: "m2m-clients:acme"}, 0, testSecretLimit)
	if _, _, err := s.ResetSecret(systemCtx(), "acme", "C1"); err == nil {
		t.Fatal("want error")
	}
	if _, err := good.Authenticate(systemCtx(), "C1", sec); err != nil {
		t.Fatalf("old secret refused after a failed reset: %v", err)
	}
}

// The restore runs on a context the caller cannot cancel: a caller that went
// away does not leave the client without a usable secret.
func TestKVM2M_ResetSecretRestoreSurvivesCallerCancel(t *testing.T) {
	mem := mustNewMemoryKV(t, systemCtx())
	good := auth.NewKVM2MClientStore(mem, 0, testSecretLimit)
	sec, err := good.Create(systemCtx(), "acme", "C1", "C1", []string{"ROLE_M2M"}, false)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(systemCtx())
	defer cancel()
	s := auth.NewKVM2MClientStore(&cancelThenFailKV{KeyValueStore: mem, ns: "m2m-clients:acme", cancel: cancel}, 0, testSecretLimit)
	if _, _, err := s.ResetSecret(ctx, "acme", "C1"); err == nil {
		t.Fatal("want error")
	}
	if _, err := good.Authenticate(systemCtx(), "C1", sec); err != nil {
		t.Fatalf("old secret refused after a failed reset: %v", err)
	}
}
