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
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/txcoord"
	"github.com/cyoda-platform/cyoda-go/plugins/memory"
)

// joinerEnv is a Joiner wired over the memory backend's transaction manager, a
// real transaction coordination, and a meter backed by a ManualReader so the
// counter can be observed through its public collection API — no test seam in
// production code.
type joinerEnv struct {
	factory   *memory.StoreFactory
	txMgr     spi.TransactionManager
	coord     txcoord.Coordination
	signer    *token.Signer
	joiner    *Joiner
	reader    *sdkmetric.ManualReader
	txID      string
	txCtx     context.Context
	tenantCtx context.Context
}

// newJoinerEnv opens a transaction on the memory backend, under a tenant
// context, with the coordination the Joiner opens its scopes in.
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
	txID, txCtx, err := txMgr.Begin(tenantCtx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	signer, err := token.NewSigner(make32(t))
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}
	coord := txcoord.New(txMgr)
	reader := sdkmetric.NewManualReader()
	meter := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)).Meter("txjoin-test")
	joiner, err := NewJoiner(signer, txMgr, coord, testResponseMax, testMaxWaiters, meter)
	if err != nil {
		t.Fatalf("NewJoiner: %v", err)
	}
	return &joinerEnv{
		factory: factory, txMgr: txMgr, coord: coord, signer: signer,
		joiner: joiner, reader: reader, txID: txID, txCtx: txCtx, tenantCtx: tenantCtx,
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

// assertOutcome checks name's count under each outcome in want, and that the
// total across every outcome is their sum.
func (env *joinerEnv) assertOutcome(t *testing.T, name string, want map[string]int64) {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := env.reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	got := map[string]int64{}
	var total, wantTotal int64
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
				if v, ok := dp.Attributes.Value(attribute.Key("outcome")); ok {
					got[v.AsString()] += dp.Value
				}
			}
		}
	}
	for outcome, n := range want {
		wantTotal += n
		if got[outcome] != n {
			t.Errorf("counter %s{outcome=%s} = %d, want %d", name, outcome, got[outcome], n)
		}
	}
	if total != wantTotal {
		t.Errorf("counter %s total = %d, want %d", name, total, wantTotal)
	}
}

func TestJoiner_CountsRefusalOnEntry(t *testing.T) {
	env := newJoinerEnv(t)
	pass := env.passFor("callout-1", 1, 0) // the callout was never begun
	_, err := env.joiner.Run(env.tenantCtx, pass, txcoord.Read, func(context.Context) *common.Outcome {
		t.Fatal("handler must not run")
		return nil
	})
	if !errors.Is(err, txcoord.ErrSuperseded) {
		t.Fatalf("got %v, want ErrSuperseded", err)
	}
	env.assertCounter(t, "cyoda.callout.superseded", "refused_on_entry", 1)
}

// A request admitted on entry and queued for the gate when its callout ends is
// refused when it takes the gate, and counted as refused before it ran.
func TestJoiner_CountsRefusalAtTheGate(t *testing.T) {
	env := newJoinerEnv(t)
	_, end := env.coord.BeginCallout(env.txCtx, env.txID, "callout-1")
	env.coord.Advance("callout-1", 1)
	pass := env.passFor("callout-1", 1, 0)

	holding := make(chan context.Context, 1)
	release := make(chan struct{})
	holderDone := make(chan struct{})
	go func() {
		defer close(holderDone)
		_, _ = env.joiner.Run(env.tenantCtx, pass, txcoord.Read, func(ctx context.Context) *common.Outcome {
			holding <- ctx
			<-release
			return nil
		})
	}()
	holderCtx := <-holding
	done := make(chan error, 1)
	go func() {
		_, err := env.joiner.Run(env.tenantCtx, pass, txcoord.Read, func(context.Context) *common.Outcome {
			t.Error("handler must not run")
			return nil
		})
		done <- err
	}()
	deadline := time.Now().Add(2 * time.Second)
	for !env.coord.AtCapacity(env.txID, 1) {
		if time.Now().After(deadline) {
			t.Fatal("the second request never queued")
		}
		time.Sleep(time.Millisecond)
	}
	endDone := make(chan struct{})
	go func() { end(); close(endDone) }()                             // the callout ends; it waits for the holder
	for !errors.Is(txcoord.Check(holderCtx), txcoord.ErrSuperseded) { // end has unregistered the callout
		if time.Now().After(deadline) {
			t.Fatal("the callout never ended")
		}
		time.Sleep(time.Millisecond)
	}
	close(release)
	if err := <-done; !errors.Is(err, txcoord.ErrSuperseded) {
		t.Fatalf("got %v, want ErrSuperseded", err)
	}
	<-holderDone
	<-endDone
	// The holder was in its handler when the callout ended: superseded in
	// progress. The queued one was refused before it ran.
	env.assertOutcome(t, "cyoda.callout.superseded", map[string]int64{"refused_on_entry": 1, "superseded_in_progress": 1})
}

// A request still in its handler when its callout ends is discarded and
// answered 410, and counted as superseded in progress.
func TestJoiner_CountsSupersededInProgress(t *testing.T) {
	env := newJoinerEnv(t)
	_, end := env.coord.BeginCallout(env.txCtx, env.txID, "callout-1")
	env.coord.Advance("callout-1", 1)
	pass := env.passFor("callout-1", 1, 0)
	endDone := make(chan struct{})
	rewrite, err := env.joiner.Run(env.tenantCtx, pass, txcoord.Write, func(ctx context.Context) *common.Outcome {
		go func() { end(); close(endDone) }() // ends while the handler holds the gate; its wait blocks until the scope ends
		deadline := time.Now().Add(2 * time.Second)
		for !errors.Is(txcoord.Check(ctx), txcoord.ErrSuperseded) { // end has unregistered the callout
			if time.Now().After(deadline) {
				t.Error("the callout never ended")
				break
			}
			time.Sleep(time.Millisecond)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("the handler ran, Run must not refuse, got %v", err)
	}
	if rewrite == nil || !errors.Is(rewrite, txcoord.ErrSuperseded) {
		t.Fatalf("rewrite = %v, want 410 CALLOUT_SUPERSEDED", rewrite)
	}
	<-endDone
	env.assertCounter(t, "cyoda.callout.superseded", "superseded_in_progress", 1)
}
