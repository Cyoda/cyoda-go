package txjoin

import (
	"context"
	"errors"
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"

	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/cyoda-platform/cyoda-go/internal/cluster/token"
	"github.com/cyoda-platform/cyoda-go/internal/fence"
	"github.com/cyoda-platform/cyoda-go/internal/txgate"
	"github.com/cyoda-platform/cyoda-go/plugins/memory"
)

// joinerEnv is a Joiner wired over the memory backend's transaction manager, a
// real fence and gate, and a meter backed by a ManualReader so the counter can
// be observed through its public collection API — no test seam in production
// code.
type joinerEnv struct {
	factory   *memory.StoreFactory
	txMgr     spi.TransactionManager
	gate      *txgate.Registry
	fence     *fence.Fence
	signer    *token.Signer
	joiner    *Joiner
	reader    *sdkmetric.ManualReader
	txID      string
	tenantCtx context.Context
}

// newJoinerEnv opens a transaction on the memory backend, under a tenant
// context, with a fence and gate the Joiner shares.
func newJoinerEnv(t *testing.T) *joinerEnv {
	t.Helper()
	tenantCtx := spi.WithUserContext(context.Background(), &spi.UserContext{
		UserID: "u", Tenant: spi.Tenant{ID: "joiner-tenant", Name: "joiner"}, Roles: []string{"user"},
	})
	factory := memory.NewStoreFactory()
	t.Cleanup(func() { factory.Close() })
	txMgr, err := factory.TransactionManager(tenantCtx)
	if err != nil {
		t.Fatalf("TransactionManager: %v", err)
	}
	txID, _, err := txMgr.Begin(tenantCtx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	signer, err := token.NewSigner(make32(t))
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}
	gate := txgate.New()
	f := fence.New(gate)
	reader := sdkmetric.NewManualReader()
	meter := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)).Meter("txjoin-test")
	joiner, err := NewJoiner(signer, txMgr, f, gate, testResponseMax, testMaxWaiters, meter)
	if err != nil {
		t.Fatalf("NewJoiner: %v", err)
	}
	return &joinerEnv{
		factory: factory, txMgr: txMgr, gate: gate, fence: f, signer: signer,
		joiner: joiner, reader: reader, txID: txID, tenantCtx: tenantCtx,
	}
}

// passFor mints a pass naming calloutID at (major, minor) on env's transaction.
func (env *joinerEnv) passFor(calloutID string, major, minor uint32) string {
	tok, err := env.signer.Issue(token.Claims{
		NodeID: "local", TxRef: env.txID, ExpiresAt: time.Now().Add(time.Minute).Unix(),
		Callout: calloutID, Major: major, Minor: minor,
	})
	if err != nil {
		panic("passFor: Issue: " + err.Error())
	}
	return tok
}

// awaitRefused polls the fence with p, through its public Admit call, until it
// is refused — the signal that whatever state change p is waiting on has
// happened. A 2s ceiling, a 2ms poll: no production seam is needed since the
// fence's own API is the observation point.
func (env *joinerEnv) awaitRefused(t *testing.T, p fence.Pair) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, err := env.fence.Admit(context.Background(), []fence.Pair{p}); err != nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %+v to be refused", p)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// awaitAbsorbed waits until a pass at (callout, major, minor 0) is refused —
// true only once a pass at a higher minor has been admitted, absorbing 0.
func (env *joinerEnv) awaitAbsorbed(t *testing.T, callout string, major uint32) {
	t.Helper()
	env.awaitRefused(t, fence.Pair{Callout: callout, Major: major, Minor: 0})
}

// awaitEnded waits until the pair naming callout at (major, minor) is refused
// because the callout has been unregistered.
func (env *joinerEnv) awaitEnded(t *testing.T, callout string, major, minor uint32) {
	t.Helper()
	env.awaitRefused(t, fence.Pair{Callout: callout, Major: major, Minor: minor})
}

// assertCounter collects the meter's data through the ManualReader and checks
// that name{outcome=outcome} is want, AND that the metric's total across every
// outcome is also want — proving a single refused request is counted exactly
// once, under exactly one outcome, never split or duplicated across labels.
func (env *joinerEnv) assertCounter(t *testing.T, name, outcome string, want int64) {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := env.reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	var total, forOutcome int64
	seen := false
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != name {
				continue
			}
			sum, ok := m.Data.(metricdata.Sum[int64])
			if !ok {
				t.Fatalf("metric %s: data is %T, want metricdata.Sum[int64]", name, m.Data)
			}
			for _, dp := range sum.DataPoints {
				total += dp.Value
				if v, ok := dp.Attributes.Value(attribute.Key("outcome")); ok && v.AsString() == outcome {
					forOutcome += dp.Value
					seen = true
				}
			}
		}
	}
	if !seen || forOutcome != want {
		t.Fatalf("counter %s{outcome=%s} = %d (seen=%v), want %d", name, outcome, forOutcome, seen, want)
	}
	if total != want {
		t.Fatalf("counter %s total across all outcomes = %d, want %d: a refused request was counted more than once or under more than one outcome", name, total, want)
	}
}

func TestJoiner_CountsRefusalOnEntry(t *testing.T) {
	env := newJoinerEnv(t)
	pass := env.passFor("callout-1", 1, 0) // the callout was never begun
	err := env.joiner.Run(env.tenantCtx, pass, func(context.Context) { t.Fatal("handler must not run") })
	if !errors.Is(err, fence.ErrSuperseded) {
		t.Fatalf("got %v, want ErrSuperseded", err)
	}
	env.assertCounter(t, "cyoda.callout.superseded", "refused_on_entry", 1)
}

func TestJoiner_CountsRefusalAtLock(t *testing.T) {
	env := newJoinerEnv(t)
	_, end := env.fence.Begin(context.Background(), "callout-1", env.txID, nil)
	env.fence.Advance("callout-1", 1)
	pass := env.passFor("callout-1", 1, 1) // minor 1: Admit absorbs it, which the test can observe
	hold := env.gate.Acquire(env.txID)     // the test holds the lock, so Run queues behind it after Admit
	done := make(chan error, 1)
	go func() {
		done <- env.joiner.Run(env.tenantCtx, pass, func(context.Context) { t.Error("handler must not run") })
	}()
	env.awaitAbsorbed(t, "callout-1", 1) // Run has passed Admit: a pass with minor 0 is now refused
	endDone := make(chan struct{})
	go func() { end(); close(endDone) }() // the callout ends; its wait queues behind Run
	env.awaitEnded(t, "callout-1", 1, 1)
	hold()
	if err := <-done; !errors.Is(err, fence.ErrSuperseded) {
		t.Fatalf("got %v, want ErrSuperseded", err)
	}
	<-endDone
	env.assertCounter(t, "cyoda.callout.superseded", "refused_at_lock", 1)
}

func TestJoiner_CountsSupersededInProgress(t *testing.T) {
	env := newJoinerEnv(t)
	_, end := env.fence.Begin(context.Background(), "callout-1", env.txID, nil)
	env.fence.Advance("callout-1", 1)
	pass := env.passFor("callout-1", 1, 0)
	endDone := make(chan struct{})
	err := env.joiner.Run(env.tenantCtx, pass, func(context.Context) {
		go func() { end(); close(endDone) }() // ends while the handler holds the lock; its wait blocks until Run releases
		env.awaitEnded(t, "callout-1", 1, 0)  // end has unregistered the callout and reached its wait
	})
	if err != nil {
		t.Fatalf("the handler ran, Run must return nil, got %v", err)
	}
	<-endDone
	env.assertCounter(t, "cyoda.callout.superseded", "superseded_in_progress", 1)
}
