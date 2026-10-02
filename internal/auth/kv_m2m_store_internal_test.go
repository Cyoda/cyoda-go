package auth

import (
	"context"
	"sync/atomic"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

type countingKV struct {
	spi.KeyValueStore
	gets atomic.Int32
}

func (c *countingKV) Get(ctx context.Context, ns, key string) ([]byte, error) {
	c.gets.Add(1)
	return c.KeyValueStore.Get(ctx, ns, key)
}

// Every Authenticate that reaches a decision makes two KV reads, so the
// number of reads does not reveal whether an id exists.
func TestKVM2M_AuthenticateReadShape(t *testing.T) {
	mem := newReplicaKV(t)
	ckv := &countingKV{KeyValueStore: mem}
	s := NewKVM2MClientStore(ckv, 0)
	sec, _ := s.Create(replicaSystemCtx(), "acme", "C1", "C1", []string{"ROLE_M2M"}, false)
	_, _ = s.Create(replicaSystemCtx(), "acme", "C2", "C2", []string{"ROLE_M2M"}, false)
	_ = mem.Delete(replicaSystemCtx(), m2mClientIndexNamespace, "C2") // C2: record without index
	for name, call := range map[string]func(){
		"unknown id":           func() { _, _ = s.Authenticate(replicaSystemCtx(), "NOPE", "x") },
		"record without index": func() { _, _ = s.Authenticate(replicaSystemCtx(), "C2", "x") },
		"wrong secret":         func() { _, _ = s.Authenticate(replicaSystemCtx(), "C1", "x") },
		"right secret":         func() { _, _ = s.Authenticate(replicaSystemCtx(), "C1", sec) },
	} {
		ckv.gets.Store(0)
		call()
		if n := ckv.gets.Load(); n != 2 {
			t.Errorf("%s: %d reads, want 2", name, n)
		}
	}
}
