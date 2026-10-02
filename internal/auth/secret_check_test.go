package auth_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/cyoda-platform/cyoda-go/internal/auth"
)

// A cached secret is honoured only while the stored record still carries the
// hash it was verified against: a reset or a delete takes effect on the next
// request.
func TestSecretCache_ResetAndDeleteTakeEffectAtOnce(t *testing.T) {
	s := auth.NewKVM2MClientStore(mustNewMemoryKV(t, systemCtx()), 0, auth.SecretCheckLimit{Slots: 4, Wait: time.Second})
	ctx := systemCtx()
	sec, _ := s.Create(ctx, "acme", "C1", "C1", []string{"ROLE_M2M"}, false)
	for i := 0; i < 2; i++ { // second call is a cache hit
		if _, err := s.Authenticate(ctx, "C1", sec); err != nil {
			t.Fatalf("authenticate %d: %v", i, err)
		}
	}
	sec2, _, err := s.ResetSecret(ctx, "acme", "C1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Authenticate(ctx, "C1", sec); !errors.Is(err, auth.ErrInvalidClient) {
		t.Fatalf("old secret after reset: %v", err)
	}
	if _, err := s.Authenticate(ctx, "C1", sec2); err != nil {
		t.Fatalf("new secret: %v", err)
	}
	if err := s.Delete(ctx, "acme", "C1"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Authenticate(ctx, "C1", sec2); !errors.Is(err, auth.ErrInvalidClient) {
		t.Fatalf("after delete: %v", err)
	}
}

// The cache is per node; the record is read from the shared store on every
// request, so a reset made through another node's store is honoured at once.
func TestSecretCache_ResetOnAnotherNodeTakesEffect(t *testing.T) {
	kv := mustNewMemoryKV(t, systemCtx())
	a := auth.NewKVM2MClientStore(kv, 0, auth.SecretCheckLimit{Slots: 4, Wait: time.Second})
	b := auth.NewKVM2MClientStore(kv, 0, auth.SecretCheckLimit{Slots: 4, Wait: time.Second})
	sec, _ := a.Create(systemCtx(), "acme", "C1", "C1", []string{"ROLE_M2M"}, false)
	if _, err := a.Authenticate(systemCtx(), "C1", sec); err != nil { // cached on a
		t.Fatal(err)
	}
	if _, _, err := b.ResetSecret(systemCtx(), "acme", "C1"); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Authenticate(systemCtx(), "C1", sec); !errors.Is(err, auth.ErrInvalidClient) {
		t.Fatalf("node a accepted the old secret after a reset on node b: %v", err)
	}
}

// A wrong secret is never a cache hit: after a cached success it is still
// refused, and the right secret is still accepted after it.
func TestSecretCache_WrongSecretAfterHitIsRefused(t *testing.T) {
	s := auth.NewKVM2MClientStore(mustNewMemoryKV(t, systemCtx()), 0, testSecretLimit)
	sec, _ := s.Create(systemCtx(), "acme", "C1", "C1", []string{"ROLE_M2M"}, false)
	if _, err := s.Authenticate(systemCtx(), "C1", sec); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Authenticate(systemCtx(), "C1", sec+"x"); !errors.Is(err, auth.ErrInvalidClient) {
		t.Fatalf("wrong secret after a cached success: %v", err)
	}
	if _, err := s.Authenticate(systemCtx(), "C1", sec); err != nil {
		t.Fatalf("right secret after a refused one: %v", err)
	}
}

func TestSecretCheck_BusyWhenSlotsExhausted(t *testing.T) {
	s := auth.NewKVM2MClientStore(mustNewMemoryKV(t, systemCtx()), 0, auth.SecretCheckLimit{Slots: 1, Wait: time.Millisecond})
	_, _ = s.Create(systemCtx(), "acme", "C1", "C1", []string{"ROLE_M2M"}, false)
	var busy atomic.Int32
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if _, err := s.Authenticate(systemCtx(), "C1", "wrong"); errors.Is(err, auth.ErrSecretCheckBusy) {
				busy.Add(1)
			}
		}()
	}
	close(start)
	wg.Wait()
	if busy.Load() == 0 {
		t.Fatal("8 concurrent bcrypt checks through 1 slot with a 1 ms wait: none was refused")
	}
}

// Every refusal for lack of a slot is counted, so busy refusals show on
// /metrics without a log line per refusal.
func TestSecretCheck_BusyRefusalsAreCounted(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })
	m, err := auth.NewOTelSecretCheckMetrics(mp.Meter("test"))
	if err != nil {
		t.Fatal(err)
	}
	s := auth.NewKVM2MClientStore(mustNewMemoryKV(t, systemCtx()), 0, auth.SecretCheckLimit{Slots: 1, Wait: time.Millisecond, Metrics: m})
	_, _ = s.Create(systemCtx(), "acme", "C1", "C1", []string{"ROLE_M2M"}, false)
	var busy atomic.Int64
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if _, err := s.Authenticate(systemCtx(), "C1", "wrong"); errors.Is(err, auth.ErrSecretCheckBusy) {
				busy.Add(1)
			}
		}()
	}
	close(start)
	wg.Wait()
	if busy.Load() == 0 {
		t.Fatal("no refusal to count")
	}
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	var counted int64
	found := false
	for _, sm := range rm.ScopeMetrics {
		for _, mt := range sm.Metrics {
			if mt.Name != "cyoda.auth.secret_checks.refused" {
				continue
			}
			found = true
			sum, ok := mt.Data.(metricdata.Sum[int64])
			if !ok {
				t.Fatalf("%s is %T, want an int64 counter", mt.Name, mt.Data)
			}
			for _, dp := range sum.DataPoints {
				counted += dp.Value
			}
		}
	}
	if !found || counted != busy.Load() {
		t.Fatalf("counter found=%v value=%d, want %d refusals", found, counted, busy.Load())
	}
}

// Unknown and malformed ids burn a bcrypt like a wrong secret does, so they
// take a slot too: with the only slot held, they are refused as busy.
func TestSecretCheck_BurnedComparisonsTakeASlot(t *testing.T) {
	s := auth.NewKVM2MClientStore(mustNewMemoryKV(t, systemCtx()), 0, auth.SecretCheckLimit{Slots: 1, Wait: time.Millisecond})
	_, _ = s.Create(systemCtx(), "acme", "C1", "C1", []string{"ROLE_M2M"}, false)
	for name, id := range map[string]string{"unknown id": "NOPE", "invalid id": "a:b"} {
		t.Run(name, func(t *testing.T) {
			var busy atomic.Int32
			var wg sync.WaitGroup
			start := make(chan struct{})
			for i := 0; i < 8; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					<-start
					if _, err := s.Authenticate(systemCtx(), id, "wrong"); errors.Is(err, auth.ErrSecretCheckBusy) {
						busy.Add(1)
					}
				}()
			}
			close(start)
			wg.Wait()
			if busy.Load() == 0 {
				t.Fatalf("8 concurrent %s checks through 1 slot with a 1 ms wait: none was refused", name)
			}
		})
	}
}
