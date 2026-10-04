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
	sec, err := s.Create(ctx, "acme", "C1", []string{"ROLE_M2M"}, false)
	if err != nil {
		t.Fatal(err)
	}
	c, err := s.Authenticate(ctx, "acme", "C1", sec)
	if err != nil || c.TenantID != "acme" || c.ClientID != "C1" || c.UserID != "C1" {
		t.Fatalf("authenticate: %v %v", c, err)
	}
	if _, err := s.Authenticate(ctx, "acme", "C1", "wrong"); !errors.Is(err, auth.ErrInvalidClient) {
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
	if _, err := s.Authenticate(ctx, "acme", "C1", sec); !errors.Is(err, auth.ErrInvalidClient) {
		t.Fatal("old secret still works")
	}
	if _, err := s.Authenticate(ctx, "acme", "C1", sec2); err != nil {
		t.Fatal("new secret refused")
	}
	if err := s.Delete(ctx, "acme", "C1"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Authenticate(ctx, "acme", "C1", sec2); !errors.Is(err, auth.ErrInvalidClient) {
		t.Fatal("deleted client authenticates")
	}
	if err := s.Delete(ctx, "acme", "C1"); !errors.Is(err, auth.ErrM2MClientNotFound) {
		t.Fatalf("second delete: %v", err)
	}
}

func TestKVM2M_CreateStampsTimestampsAndResetAdvancesUpdatedAt(t *testing.T) {
	s, _ := newM2M(t, 0)
	before := time.Now()
	sec, err := s.Create(systemCtx(), "acme", "C1", []string{"ROLE_M2M"}, false)
	if err != nil {
		t.Fatal(err)
	}
	after := time.Now()
	c0, err := s.Authenticate(systemCtx(), "acme", "C1", sec)
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
	stored, err := s.Authenticate(systemCtx(), "acme", "C1", sec2)
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
	sec, _ := a.Create(systemCtx(), "acme", "C1", []string{"ROLE_M2M"}, false)
	if _, err := b.Authenticate(systemCtx(), "acme", "C1", sec); err != nil {
		t.Fatal("node B does not see node A's client")
	}
	_ = a.Delete(systemCtx(), "acme", "C1")
	if _, err := b.Authenticate(systemCtx(), "acme", "C1", sec); !errors.Is(err, auth.ErrInvalidClient) {
		t.Fatal("node B still accepts a client node A deleted")
	}
}

// Another tenant neither sees nor changes acme's client, and acme's client
// does not authenticate in another tenant.
func TestKVM2M_TenantIsolation(t *testing.T) {
	s, _ := newM2M(t, 0)
	sec, _ := s.Create(systemCtx(), "acme", "C1", []string{"ROLE_M2M"}, false)
	if l, _ := s.List(systemCtx(), "other"); len(l) != 0 {
		t.Fatal("other tenant lists acme's client")
	}
	if err := s.Delete(systemCtx(), "other", "C1"); !errors.Is(err, auth.ErrM2MClientNotFound) {
		t.Fatalf("cross-tenant delete: %v", err)
	}
	if _, _, err := s.ResetSecret(systemCtx(), "other", "C1"); !errors.Is(err, auth.ErrM2MClientNotFound) {
		t.Fatalf("cross-tenant reset: %v", err)
	}
	if _, err := s.Lookup(systemCtx(), "other", "C1"); !errors.Is(err, auth.ErrM2MClientNotFound) {
		t.Fatalf("cross-tenant lookup: %v", err)
	}
	if _, err := s.Authenticate(systemCtx(), "other", "C1", sec); !errors.Is(err, auth.ErrInvalidClient) {
		t.Fatalf("acme's client authenticates in another tenant: %v", err)
	}
	if _, err := s.Authenticate(systemCtx(), "acme", "C1", sec); err != nil {
		t.Fatal("acme's client damaged by another tenant's calls")
	}
}

func TestKVM2M_SameIDInTwoTenants(t *testing.T) {
	s, _ := newM2M(t, 0)
	ctx := systemCtx()
	secA, err := s.Create(ctx, "a", "backend", []string{"ROLE_M2M"}, false)
	if err != nil {
		t.Fatal(err)
	}
	secB, err := s.Create(ctx, "b", "backend", []string{"ROLE_M2M"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if c, err := s.Authenticate(ctx, "a", "backend", secA); err != nil || c.TenantID != "a" {
		t.Fatalf("a: %v %v", c, err)
	}
	if _, err := s.Authenticate(ctx, "b", "backend", secA); !errors.Is(err, auth.ErrInvalidClient) {
		t.Fatalf("a's secret at b: %v", err)
	}
	if err := s.Delete(ctx, "a", "backend"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Authenticate(ctx, "b", "backend", secB); err != nil {
		t.Fatalf("b after a's delete: %v", err)
	}
	if _, _, err := s.ResetSecret(ctx, "a", "backend"); !errors.Is(err, auth.ErrM2MClientNotFound) {
		t.Fatalf("reset of a's deleted client: %v", err)
	}
}

func TestKVM2M_IDsDifferingInCaseAreDistinct(t *testing.T) {
	s, _ := newM2M(t, 0)
	ctx := systemCtx()
	upper, err := s.Create(ctx, "a", "Backend", []string{"ROLE_M2M"}, false)
	if err != nil {
		t.Fatal(err)
	}
	lower, err := s.Create(ctx, "a", "backend", []string{"ROLE_M2M"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Authenticate(ctx, "a", "Backend", lower); !errors.Is(err, auth.ErrInvalidClient) {
		t.Fatal("Backend accepts backend's secret")
	}
	if _, err := s.Authenticate(ctx, "a", "Backend", upper); err != nil {
		t.Fatal(err)
	}
}

func TestKVM2M_CreateRefusesATakenID(t *testing.T) {
	s, _ := newM2M(t, 0)
	ctx := systemCtx()
	if _, err := s.Create(ctx, "a", "backend", []string{"ROLE_M2M"}, false); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Create(ctx, "a", "backend", []string{"ROLE_M2M"}, false); !errors.Is(err, auth.ErrM2MClientExists) {
		t.Fatalf("second create: %v", err)
	}
}

// At the cap, a taken id is still 409, not the cap.
func TestKVM2M_ExistsBeforeCap(t *testing.T) {
	s, _ := newM2M(t, 1)
	ctx := systemCtx()
	if _, err := s.Create(ctx, "a", "backend", []string{"ROLE_M2M"}, false); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Create(ctx, "a", "backend", []string{"ROLE_M2M"}, false); !errors.Is(err, auth.ErrM2MClientExists) {
		t.Fatalf("taken id at the cap: %v", err)
	}
}

func TestKVM2M_RecreateAfterDelete(t *testing.T) {
	s, _ := newM2M(t, 0)
	ctx := systemCtx()
	old, _ := s.Create(ctx, "a", "backend", []string{"ROLE_M2M"}, false)
	oldC, _ := s.Authenticate(ctx, "a", "backend", old)
	if err := s.Delete(ctx, "a", "backend"); err != nil {
		t.Fatal(err)
	}
	fresh, err := s.Create(ctx, "a", "backend", []string{"ROLE_M2M"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Authenticate(ctx, "a", "backend", old); !errors.Is(err, auth.ErrInvalidClient) {
		t.Fatal("old secret works on the new client")
	}
	newC, err := s.Authenticate(ctx, "a", "backend", fresh)
	if err != nil {
		t.Fatal(err)
	}
	if newC.SecretGen == oldC.SecretGen {
		t.Fatal("new incarnation reuses the old secret generation")
	}
}

// slowGetKV makes Get slow after it has read, so the answer is stale by the
// time the caller writes: two creates of one id both read "absent" before
// either writes, unless the write itself is conditional.
type slowGetKV struct {
	spi.KeyValueStore
	delay time.Duration
}

func (k *slowGetKV) Get(ctx context.Context, ns, key string) ([]byte, error) {
	out, err := k.KeyValueStore.Get(ctx, ns, key)
	time.Sleep(k.delay)
	return out, err
}

// Two concurrent creates of one id: exactly one succeeds, and its secret works.
func TestKVM2M_ConcurrentCreatesOfOneID(t *testing.T) {
	kv := &slowGetKV{KeyValueStore: mustNewMemoryKV(t, systemCtx()), delay: 50 * time.Millisecond}
	s1 := auth.NewKVM2MClientStore(kv, 0, testSecretLimit)
	s2 := auth.NewKVM2MClientStore(kv, 0, testSecretLimit) // a second node: its own locks
	for round := 0; round < 10; round++ {
		id := fmt.Sprintf("race%d", round)
		var wg sync.WaitGroup
		secs, errs := make([]string, 2), make([]error, 2)
		for i, s := range []*auth.KVM2MClientStore{s1, s2} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				secs[i], errs[i] = s.Create(systemCtx(), "a", id, []string{"ROLE_M2M"}, false)
			}()
		}
		wg.Wait()
		won := 0
		for i := range 2 {
			switch {
			case errs[i] == nil:
				won++
				if _, err := s1.Authenticate(systemCtx(), "a", id, secs[i]); err != nil {
					t.Fatalf("round %d: winner's secret refused", round)
				}
			case !errors.Is(errs[i], auth.ErrM2MClientExists):
				t.Fatalf("round %d: %v", round, errs[i])
			}
		}
		if won != 1 {
			t.Fatalf("round %d: %d winners", round, won)
		}
	}
}

// A reset racing a delete never brings the client back.
func TestKVM2M_ResetRacingDeleteNeverResurrects(t *testing.T) {
	mem := mustNewMemoryKV(t, systemCtx())
	var s *auth.KVM2MClientStore
	// The delete lands between the reset's read and its write.
	kv := &beforeCASKV{KeyValueStore: mem, before: func() {
		if err := s.Delete(systemCtx(), "a", "backend"); err != nil {
			t.Error(err)
		}
	}}
	s = auth.NewKVM2MClientStore(kv, 0, testSecretLimit)
	if _, err := s.Create(systemCtx(), "a", "backend", []string{"ROLE_M2M"}, false); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.ResetSecret(systemCtx(), "a", "backend"); !errors.Is(err, auth.ErrM2MClientNotFound) {
		t.Fatalf("reset: %v", err)
	}
	if _, err := mem.Get(systemCtx(), "m2m-clients:a", "backend"); !errors.Is(err, spi.ErrNotFound) {
		t.Fatal("deleted client came back")
	}
}

// A reset that loses to another reset answers ErrM2MClientChanged and leaves
// the winner's record.
func TestKVM2M_ResetLosingARaceIsChanged(t *testing.T) {
	mem := mustNewMemoryKV(t, systemCtx())
	var s *auth.KVM2MClientStore
	var winner string
	kv := &beforeCASKV{KeyValueStore: mem}
	s = auth.NewKVM2MClientStore(kv, 0, testSecretLimit)
	if _, err := s.Create(systemCtx(), "a", "backend", []string{"ROLE_M2M"}, false); err != nil {
		t.Fatal(err)
	}
	kv.before = func() {
		kv.before = nil // the inner reset runs without interference
		sec, _, err := s.ResetSecret(systemCtx(), "a", "backend")
		if err != nil {
			t.Error(err)
		}
		winner = sec
	}
	if _, _, err := s.ResetSecret(systemCtx(), "a", "backend"); !errors.Is(err, auth.ErrM2MClientChanged) {
		t.Fatalf("losing reset: %v", err)
	}
	if _, err := s.Authenticate(systemCtx(), "a", "backend", winner); err != nil {
		t.Fatal("winner's secret refused")
	}
}

// beforeCASKV runs before (once set) ahead of every CompareAndPut.
type beforeCASKV struct {
	spi.KeyValueStore
	before func()
}

func (k *beforeCASKV) CompareAndPut(ctx context.Context, ns, key string, expected, value []byte) (bool, error) {
	if f := k.before; f != nil {
		f()
	}
	return k.KeyValueStore.CompareAndPut(ctx, ns, key, expected, value)
}

// casCommitThenFailKV applies every conditional write it overrides and then
// reports an error (a timeout after commit). then, if set, runs once, between
// the first such write and its error.
type casCommitThenFailKV struct {
	spi.KeyValueStore
	then func()
}

func (k *casCommitThenFailKV) runThen() {
	if f := k.then; f != nil {
		k.then = nil
		f()
	}
}

func (k *casCommitThenFailKV) PutIfAbsent(ctx context.Context, ns, key string, v []byte) (bool, error) {
	if _, err := k.KeyValueStore.PutIfAbsent(ctx, ns, key, v); err != nil {
		return false, err
	}
	k.runThen()
	return false, errors.New("injected: committed, then failed")
}

func (k *casCommitThenFailKV) CompareAndPut(ctx context.Context, ns, key string, expected, v []byte) (bool, error) {
	if _, err := k.KeyValueStore.CompareAndPut(ctx, ns, key, expected, v); err != nil {
		return false, err
	}
	k.runThen()
	return false, errors.New("injected: committed, then failed")
}

// An ambiguous create write is undone, and only this call's write.
func TestKVM2M_CreateUndoesItsOwnAmbiguousWrite(t *testing.T) {
	mem := mustNewMemoryKV(t, systemCtx())
	s := auth.NewKVM2MClientStore(&casCommitThenFailKV{KeyValueStore: mem}, 0, testSecretLimit)
	if _, err := s.Create(systemCtx(), "a", "backend", []string{"ROLE_M2M"}, false); err == nil {
		t.Fatal("want error")
	}
	if _, err := mem.Get(systemCtx(), "m2m-clients:a", "backend"); !errors.Is(err, spi.ErrNotFound) {
		t.Fatal("record left behind")
	}
}

// The undo of an ambiguous create never removes another create's winning
// record: here another node's record replaced this call's write before the
// error came back.
func TestKVM2M_CreateUndoSparesAnotherCallsRecord(t *testing.T) {
	mem := mustNewMemoryKV(t, systemCtx())
	other := auth.NewKVM2MClientStore(mem, 0, testSecretLimit)
	var otherSecret string
	s := auth.NewKVM2MClientStore(&casCommitThenFailKV{KeyValueStore: mem, then: func() {
		_ = mem.Delete(systemCtx(), "m2m-clients:a", "backend")
		var err error
		otherSecret, err = other.Create(systemCtx(), "a", "backend", []string{"ROLE_M2M"}, false)
		if err != nil {
			t.Error(err)
		}
	}}, 0, testSecretLimit)
	if _, err := s.Create(systemCtx(), "a", "backend", []string{"ROLE_M2M"}, false); err == nil {
		t.Fatal("want error")
	}
	if _, err := other.Authenticate(systemCtx(), "a", "backend", otherSecret); err != nil {
		t.Fatal("the other create's client was removed")
	}
}

// An ambiguous reset write is undone only if nothing changed since.
func TestKVM2M_ResetUndoesItsOwnAmbiguousWrite(t *testing.T) {
	mem := mustNewMemoryKV(t, systemCtx())
	plain := auth.NewKVM2MClientStore(mem, 0, testSecretLimit)
	old, err := plain.Create(systemCtx(), "a", "backend", []string{"ROLE_M2M"}, false)
	if err != nil {
		t.Fatal(err)
	}
	s := auth.NewKVM2MClientStore(&casCommitThenFailKV{KeyValueStore: mem}, 0, testSecretLimit)
	if _, _, err := s.ResetSecret(systemCtx(), "a", "backend"); err == nil {
		t.Fatal("want error")
	}
	if _, err := plain.Authenticate(systemCtx(), "a", "backend", old); err != nil {
		t.Fatal("old secret not restored")
	}
}

// The undo of an ambiguous reset never revives a client deleted meanwhile.
func TestKVM2M_ResetUndoNeverRevivesADeletedClient(t *testing.T) {
	mem := mustNewMemoryKV(t, systemCtx())
	plain := auth.NewKVM2MClientStore(mem, 0, testSecretLimit)
	if _, err := plain.Create(systemCtx(), "a", "backend", []string{"ROLE_M2M"}, false); err != nil {
		t.Fatal(err)
	}
	s := auth.NewKVM2MClientStore(&casCommitThenFailKV{KeyValueStore: mem, then: func() {
		if err := plain.Delete(systemCtx(), "a", "backend"); err != nil {
			t.Error(err)
		}
	}}, 0, testSecretLimit)
	if _, _, err := s.ResetSecret(systemCtx(), "a", "backend"); err == nil {
		t.Fatal("want error")
	}
	if _, err := mem.Get(systemCtx(), "m2m-clients:a", "backend"); !errors.Is(err, spi.ErrNotFound) {
		t.Fatal("deleted client came back")
	}
}

// A record that does not decode is the store failing: Authenticate, Lookup
// and ResetSecret return a store error, List skips it, and Delete removes it
// (the caller's namespace proves it is the caller's).
func TestKVM2M_UndecodableRecord(t *testing.T) {
	s, kv := newM2M(t, 0)
	_, _ = s.Create(systemCtx(), "acme", "C1", []string{"ROLE_M2M"}, false)
	_, _ = s.Create(systemCtx(), "acme", "C2", []string{"ROLE_M2M"}, false)
	_ = kv.Put(systemCtx(), "m2m-clients:acme", "C1", []byte("{"))
	if _, err := s.Authenticate(systemCtx(), "acme", "C1", "x"); err == nil || errors.Is(err, auth.ErrInvalidClient) {
		t.Fatalf("authenticate on undecodable: %v, want a store error", err)
	}
	if _, err := s.Lookup(systemCtx(), "acme", "C1"); err == nil || errors.Is(err, auth.ErrM2MClientNotFound) {
		t.Fatalf("lookup on undecodable: %v, want a store error", err)
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
	if _, err := kv.Get(systemCtx(), "m2m-clients:acme", "C1"); !errors.Is(err, spi.ErrNotFound) {
		t.Fatal("undecodable record left behind")
	}
}

func TestKVM2M_Cap(t *testing.T) {
	s, _ := newM2M(t, 2)
	_, _ = s.Create(systemCtx(), "acme", "C1", []string{"ROLE_M2M"}, false)
	_, _ = s.Create(systemCtx(), "acme", "C2", []string{"ROLE_M2M"}, false)
	if _, err := s.Create(systemCtx(), "acme", "C3", []string{"ROLE_M2M"}, false); !errors.Is(err, auth.ErrM2MClientCapReached) {
		t.Fatalf("third create: %v", err)
	}
	if _, err := s.Create(systemCtx(), "other", "C4", []string{"ROLE_M2M"}, false); err != nil {
		t.Fatal("cap is per tenant")
	}
	_ = s.Delete(systemCtx(), "acme", "C1")
	if _, err := s.Create(systemCtx(), "acme", "C3", []string{"ROLE_M2M"}, false); err != nil {
		t.Fatalf("after a delete: %v", err)
	}
}

func TestKVM2M_CapZeroIsUnbounded(t *testing.T) {
	s, _ := newM2M(t, 0)
	for i := 0; i < 3; i++ {
		if _, err := s.Create(systemCtx(), "acme", fmt.Sprintf("C%d", i), []string{"ROLE_M2M"}, false); err != nil {
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
			if _, err := s.Create(systemCtx(), "acme", fmt.Sprintf("C%d", i), []string{"ROLE_M2M"}, false); err == nil {
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

// A record in the tenant takes its id even when it does not decode. An id
// outside the grammar fails in the encoder, before anything is written.
func TestKVM2M_CreateRefusesExistingAndInvalidIDs(t *testing.T) {
	s, kv := newM2M(t, 0)
	_ = kv.Put(systemCtx(), "m2m-clients:acme", "C9", []byte("{"))
	if _, err := s.Create(systemCtx(), "acme", "C9", []string{"ROLE_M2M"}, false); !errors.Is(err, auth.ErrM2MClientExists) {
		t.Fatalf("id of an undecodable record: %v", err)
	}
	for _, id := range []string{"a:b", "system", "SYSTEM"} {
		if _, err := s.Create(systemCtx(), "acme", id, []string{"ROLE_M2M"}, false); err == nil {
			t.Fatalf("%q outside the grammar: created", id)
		}
		if _, err := kv.Get(systemCtx(), "m2m-clients:acme", id); !errors.Is(err, spi.ErrNotFound) {
			t.Fatalf("%q outside the grammar: record written (%v)", id, err)
		}
	}
}

// A client is stored once, in its tenant's namespace: no other namespace is
// written, and no stored value carries a plaintext secret, not after a
// create and not after a reset.
func TestKVM2M_OneRecordInTheTenantNamespaceAndNoPlaintextSecret(t *testing.T) {
	s, kv := newM2M(t, 0)
	sec, err := s.Create(systemCtx(), "acme", "C1", []string{"ROLE_M2M"}, false)
	if err != nil {
		t.Fatal(err)
	}
	sec2, _, err := s.ResetSecret(systemCtx(), "acme", "C1")
	if err != nil {
		t.Fatal(err)
	}
	entries, err := kv.List(systemCtx(), "m2m-clients:acme")
	if err != nil || len(entries) != 1 {
		t.Fatalf("tenant namespace: %v %v", entries, err)
	}
	for k, v := range entries {
		if strings.Contains(string(v), sec) || strings.Contains(string(v), sec2) {
			t.Fatalf("m2m-clients:acme/%s holds a plaintext secret", k)
		}
	}
	if idx, err := kv.List(systemCtx(), "m2m-client-ids"); err != nil || len(idx) != 0 {
		t.Fatalf("a global client-id index was written: %v %v", idx, err)
	}
}

func TestKVM2M_StoreFailureIsNeverNotFoundOrInvalid(t *testing.T) {
	mem := mustNewMemoryKV(t, systemCtx())
	good := auth.NewKVM2MClientStore(mem, 0, testSecretLimit)
	sec, _ := good.Create(systemCtx(), "acme", "C1", []string{"ROLE_M2M"}, false)
	s := auth.NewKVM2MClientStore(brokenKV{}, 0, testSecretLimit)
	if _, err := s.Authenticate(systemCtx(), "acme", "C1", sec); err == nil || errors.Is(err, auth.ErrInvalidClient) {
		t.Fatalf("authenticate: %v", err)
	}
	if _, err := s.Lookup(systemCtx(), "acme", "C1"); err == nil || errors.Is(err, auth.ErrM2MClientNotFound) {
		t.Fatalf("lookup: %v", err)
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
	if _, err := s.Create(systemCtx(), "acme", "C2", []string{"ROLE_M2M"}, false); err == nil || errors.Is(err, auth.ErrM2MClientExists) {
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
func (brokenKV) PutIfAbsent(context.Context, string, string, []byte) (bool, error) {
	return false, unavailable{}
}
func (brokenKV) CompareAndPut(context.Context, string, string, []byte, []byte) (bool, error) {
	return false, unavailable{}
}
func (brokenKV) DeleteIfEqual(context.Context, string, string, []byte) (bool, error) {
	return false, unavailable{}
}

// An id outside the client-id grammar is ErrInvalidClient without a store
// read: the store here fails every call, so a read would be a store error.
func TestKVM2M_AuthenticateRefusesMalformedIDsWithoutReading(t *testing.T) {
	s := auth.NewKVM2MClientStore(brokenKV{}, 0, testSecretLimit) // any read would error
	for _, id := range []string{"a\x00b", "\xff", strings.Repeat("A", 101), "a:b", "", "system", "System"} {
		if _, err := s.Authenticate(systemCtx(), "acme", id, "x"); !errors.Is(err, auth.ErrInvalidClient) {
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

func (k *txProbeKV) PutIfAbsent(ctx context.Context, ns, key string, v []byte) (bool, error) {
	k.probe(ctx, "PutIfAbsent", ns)
	return k.KeyValueStore.PutIfAbsent(ctx, ns, key, v)
}

func (k *txProbeKV) CompareAndPut(ctx context.Context, ns, key string, expected, v []byte) (bool, error) {
	k.probe(ctx, "CompareAndPut", ns)
	return k.KeyValueStore.CompareAndPut(ctx, ns, key, expected, v)
}

func (k *txProbeKV) DeleteIfEqual(ctx context.Context, ns, key string, expected []byte) (bool, error) {
	k.probe(ctx, "DeleteIfEqual", ns)
	return k.KeyValueStore.DeleteIfEqual(ctx, ns, key, expected)
}

// No KV call made by the store carries the caller's transaction: not from
// any method, and not from the undo of a failed create or reset.
func TestKVM2M_IgnoresCallerTransaction(t *testing.T) {
	txCtx := spi.WithTransaction(systemCtx(), &spi.TransactionState{ID: "caller-tx"})

	probe := &txProbeKV{KeyValueStore: mustNewMemoryKV(t, systemCtx())}
	s := auth.NewKVM2MClientStore(probe, 10, testSecretLimit) // a cap, so Create lists too
	sec, err := s.Create(txCtx, "acme", "C1", []string{"ROLE_M2M"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Authenticate(txCtx, "acme", "C1", sec); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Lookup(txCtx, "acme", "C1"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.List(txCtx, "acme"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.ResetSecret(txCtx, "acme", "C1"); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(txCtx, "acme", "C1"); err != nil {
		t.Fatal(err)
	}

	mem := mustNewMemoryKV(t, systemCtx())
	undoProbe := &txProbeKV{KeyValueStore: &casCommitThenFailKV{KeyValueStore: mem}}
	failing := auth.NewKVM2MClientStore(undoProbe, 0, testSecretLimit)
	if _, err := failing.Create(txCtx, "acme", "C2", []string{"ROLE_M2M"}, false); err == nil {
		t.Fatal("create with an ambiguous write: want error")
	}
	if _, err := auth.NewKVM2MClientStore(mem, 0, testSecretLimit).Create(systemCtx(), "acme", "C3", []string{"ROLE_M2M"}, false); err != nil {
		t.Fatal(err)
	}
	if _, _, err := failing.ResetSecret(txCtx, "acme", "C3"); err == nil {
		t.Fatal("reset with an ambiguous write: want error")
	}

	if seen := append(probe.calls(), undoProbe.calls()...); len(seen) != 0 {
		t.Fatalf("KV calls made under the caller's transaction: %v", seen)
	}
}

// A new client carries onBehalfOf and a secret generation drawn at random in
// [1, 2^52]; a reset increments it without touching OnBehalfOf; Lookup reads
// the current record without a secret, and an absent id is
// ErrM2MClientNotFound.
func TestKVM2MClientStore_OnBehalfOfAndSecretGen(t *testing.T) {
	s := auth.NewKVM2MClientStore(mustNewMemoryKV(t, systemCtx()), 0, testSecretLimit)
	ctx := systemCtx()
	sec, err := s.Create(ctx, "acme", "C1", []string{"ROLE_M2M"}, true)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	c, err := s.Authenticate(ctx, "acme", "C1", sec)
	if err != nil || !c.OnBehalfOf || c.SecretGen < 1 || c.SecretGen > 1<<52 {
		t.Fatalf("after create: %+v, %v", c, err)
	}
	gen := c.SecretGen
	_, c, err = s.ResetSecret(ctx, "acme", "C1")
	if err != nil || c.SecretGen != gen+1 || !c.OnBehalfOf {
		t.Fatalf("after reset: %+v, %v", c, err)
	}
	got, err := s.Lookup(ctx, "acme", "C1")
	if err != nil || got.SecretGen != gen+1 || got.TenantID != "acme" {
		t.Fatalf("lookup: %+v, %v", got, err)
	}
	if _, err := s.Lookup(ctx, "acme", "NOPE"); !errors.Is(err, auth.ErrM2MClientNotFound) {
		t.Fatalf("lookup absent: %v", err)
	}
}

// A new client's secret generation does not start at a fixed value: across
// creates, the generations differ.
func TestKVM2M_SecretGenerationStartsAtRandom(t *testing.T) {
	s, _ := newM2M(t, 0)
	seen := map[uint64]bool{}
	for i := 0; i < 4; i++ {
		id := fmt.Sprintf("C%d", i)
		sec, err := s.Create(systemCtx(), "acme", id, []string{"ROLE_M2M"}, false)
		if err != nil {
			t.Fatal(err)
		}
		c, err := s.Authenticate(systemCtx(), "acme", id, sec)
		if err != nil {
			t.Fatal(err)
		}
		seen[c.SecretGen] = true
	}
	if len(seen) != 4 {
		t.Fatalf("4 creates drew %d distinct generations", len(seen))
	}
}

// A store failure on Lookup keeps its storage-unavailable marker, so a
// caller (the compute-stream re-check) can tell it apart from "gone".
func TestKVM2MClientStore_LookupKeepsUnavailableMarker(t *testing.T) {
	s := auth.NewKVM2MClientStore(brokenKV{}, 0, testSecretLimit)
	_, err := s.Lookup(systemCtx(), "acme", "C1")
	var su interface{ StorageUnavailable() bool }
	if !errors.As(err, &su) || !su.StorageUnavailable() {
		t.Fatalf("Lookup error lost its storage-unavailable marker: %v", err)
	}
}

// cancelThenFailKV applies the first conditional write it overrides, then
// cancels the caller's context and reports an error — a caller that gave up
// while the write landed. Every conditional write it is given on a context
// that is done fails, writing nothing; so an undo that ran on the caller's
// context would write nothing.
type cancelThenFailKV struct {
	spi.KeyValueStore
	cancel context.CancelFunc
	fired  bool
}

func (k *cancelThenFailKV) fire() error {
	if k.fired {
		return nil
	}
	k.fired = true
	k.cancel()
	return errors.New("injected: committed, then the caller went away")
}

func (k *cancelThenFailKV) PutIfAbsent(ctx context.Context, ns, key string, v []byte) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	applied, err := k.KeyValueStore.PutIfAbsent(ctx, ns, key, v)
	if err != nil {
		return false, err
	}
	if err := k.fire(); err != nil {
		return false, err
	}
	return applied, nil
}

func (k *cancelThenFailKV) CompareAndPut(ctx context.Context, ns, key string, expected, v []byte) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	applied, err := k.KeyValueStore.CompareAndPut(ctx, ns, key, expected, v)
	if err != nil {
		return false, err
	}
	if err := k.fire(); err != nil {
		return false, err
	}
	return applied, nil
}

func (k *cancelThenFailKV) DeleteIfEqual(ctx context.Context, ns, key string, expected []byte) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	return k.KeyValueStore.DeleteIfEqual(ctx, ns, key, expected)
}

// The undo of an ambiguous create runs on a context the caller cannot
// cancel: a caller that went away does not leave the record behind.
func TestKVM2M_CreateUndoSurvivesCallerCancel(t *testing.T) {
	mem := mustNewMemoryKV(t, systemCtx())
	ctx, cancel := context.WithCancel(systemCtx())
	defer cancel()
	s := auth.NewKVM2MClientStore(&cancelThenFailKV{KeyValueStore: mem, cancel: cancel}, 0, testSecretLimit)
	if _, err := s.Create(ctx, "acme", "C1", []string{"ROLE_M2M"}, false); err == nil {
		t.Fatal("want error")
	}
	if _, err := mem.Get(systemCtx(), "m2m-clients:acme", "C1"); !errors.Is(err, spi.ErrNotFound) {
		t.Error("record left behind")
	}
}

// The undo of an ambiguous reset runs on a context the caller cannot cancel:
// a caller that went away does not leave the client without a usable
// secret.
func TestKVM2M_ResetSecretRestoreSurvivesCallerCancel(t *testing.T) {
	mem := mustNewMemoryKV(t, systemCtx())
	good := auth.NewKVM2MClientStore(mem, 0, testSecretLimit)
	sec, err := good.Create(systemCtx(), "acme", "C1", []string{"ROLE_M2M"}, false)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(systemCtx())
	defer cancel()
	s := auth.NewKVM2MClientStore(&cancelThenFailKV{KeyValueStore: mem, cancel: cancel}, 0, testSecretLimit)
	if _, _, err := s.ResetSecret(ctx, "acme", "C1"); err == nil {
		t.Fatal("want error")
	}
	if _, err := good.Authenticate(systemCtx(), "acme", "C1", sec); err != nil {
		t.Fatalf("old secret refused after a failed reset: %v", err)
	}
}
