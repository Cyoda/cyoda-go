package auth

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// testSecretLimit is the secret-check bound of the stores these tests build.
var testSecretLimit = SecretCheckLimit{Slots: 4, Wait: time.Second}

type countingKV struct {
	spi.KeyValueStore
	gets atomic.Int32
}

func (c *countingKV) Get(ctx context.Context, ns, key string) ([]byte, error) {
	c.gets.Add(1)
	return c.KeyValueStore.Get(ctx, ns, key)
}

// Every Authenticate that reaches a decision makes exactly one KV read — of
// (tenant, id) — whether the id exists or not and whether the secret is
// right or wrong; an id outside the grammar makes none.
func TestKVM2M_AuthenticateReadShape(t *testing.T) {
	mem := newReplicaKV(t)
	ckv := &countingKV{KeyValueStore: mem}
	s := NewKVM2MClientStore(ckv, 0, testSecretLimit)
	sec, _ := s.Create(replicaSystemCtx(), "acme", "C1", []string{"ROLE_M2M"}, false)
	for name, tc := range map[string]struct {
		call  func()
		reads int32
	}{
		"unknown id":             {func() { _, _ = s.Authenticate(replicaSystemCtx(), "acme", "NOPE", "x") }, 1},
		"another tenant's id":    {func() { _, _ = s.Authenticate(replicaSystemCtx(), "other", "C1", sec) }, 1},
		"wrong secret":           {func() { _, _ = s.Authenticate(replicaSystemCtx(), "acme", "C1", "x") }, 1},
		"right secret":           {func() { _, _ = s.Authenticate(replicaSystemCtx(), "acme", "C1", sec) }, 1},
		"id outside the grammar": {func() { _, _ = s.Authenticate(replicaSystemCtx(), "acme", "a:b", "x") }, 0},
	} {
		ckv.gets.Store(0)
		tc.call()
		if n := ckv.gets.Load(); n != tc.reads {
			t.Errorf("%s: %d reads, want %d", name, n, tc.reads)
		}
	}
}
