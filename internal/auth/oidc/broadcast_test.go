package oidc

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/google/uuid"
)

// recordingMetrics counts calls to each Metrics method for assertion.
type recordingMetrics struct {
	hits              int64
	misses            int64
	evicts            int64
	panics            int64
	unknownBroadcasts int64
	jwksErrors        int64
	registryGauge     int64
	observeCount      int64

	dropsMu sync.Mutex
	drops   map[string]int64
}

func (m *recordingMetrics) IncKidCacheHit()                        { atomic.AddInt64(&m.hits, 1) }
func (m *recordingMetrics) IncKidCacheMiss()                       { atomic.AddInt64(&m.misses, 1) }
func (m *recordingMetrics) IncKidCacheEvict()                      { atomic.AddInt64(&m.evicts, 1) }
func (m *recordingMetrics) IncJWKSFetchError(outcome string)       { atomic.AddInt64(&m.jwksErrors, 1) }
func (m *recordingMetrics) IncBroadcastPanic()                     { atomic.AddInt64(&m.panics, 1) }
func (m *recordingMetrics) IncBroadcastDrop(reason string) {
	m.dropsMu.Lock()
	defer m.dropsMu.Unlock()
	if m.drops == nil {
		m.drops = make(map[string]int64)
	}
	m.drops[reason]++
}
func (m *recordingMetrics) IncUnknownProviderBroadcast()           { atomic.AddInt64(&m.unknownBroadcasts, 1) }
func (m *recordingMetrics) ObserveBroadcastReceive(seconds float64) { atomic.AddInt64(&m.observeCount, 1) }
func (m *recordingMetrics) SetRegistryProviders(n int)             { atomic.StoreInt64(&m.registryGauge, int64(n)) }

// DropsForReason returns the number of times IncBroadcastDrop was called with
// the given reason label.
func (m *recordingMetrics) DropsForReason(reason string) int64 {
	m.dropsMu.Lock()
	defer m.dropsMu.Unlock()
	return m.drops[reason]
}

// compile-time guard: recordingMetrics must satisfy Metrics.
var _ Metrics = (*recordingMetrics)(nil)

// panicDiscovery is a Discovery that always panics — used to test handleBroadcast's recover().
type panicDiscovery struct{}

func (panicDiscovery) Fetch(_ context.Context, _ string) (*DiscoveryDoc, error) {
	panic("simulated discovery panic")
}

// countingDiscovery records how many times Fetch was called for a given URI.
type countingDiscovery struct {
	calls int64
	doc   *DiscoveryDoc
}

func (c *countingDiscovery) Fetch(_ context.Context, _ string) (*DiscoveryDoc, error) {
	atomic.AddInt64(&c.calls, 1)
	return c.doc, nil
}

func newTestRegistryWithMetrics(t *testing.T, disc Discovery, metrics Metrics) *Registry {
	t.Helper()
	return NewRegistry(newTestStore(t), disc, nil, metrics, nil,
		RegistryConfig{AllowPrivateNetworks: true}) // tests bind to httptest.Server on 127.0.0.1
}

// pollUntil waits up to 1 s for cond to return true, polling every 10 ms.
// Returns true if cond became true before the deadline.
func pollUntil(cond func() bool) bool {
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return false
}

// ---------- Tests ----------

func TestBroadcastEnvelope_Roundtrip(t *testing.T) {
	env := broadcastEnvelope{Op: "reload", TenantID: "tenantA", URI: "https://idp.example"}
	blob, err := json.Marshal(&env)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var back broadcastEnvelope
	if err := json.Unmarshal(blob, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if back != env {
		t.Errorf("round trip lost data: got %+v, want %+v", back, env)
	}

	// reload_all omits tenant + uri (omitempty).
	envAll := broadcastEnvelope{Op: "reload_all"}
	blobAll, err := json.Marshal(&envAll)
	if err != nil {
		t.Fatalf("marshal reload_all: %v", err)
	}
	s := string(blobAll)
	if !strings.Contains(s, `"op":"reload_all"`) {
		t.Errorf("expected op:reload_all, got %s", s)
	}
	if strings.Contains(s, `"t"`) {
		t.Errorf("reload_all envelope must omit t field: %s", s)
	}
	if strings.Contains(s, `"u"`) {
		t.Errorf("reload_all envelope must omit u field: %s", s)
	}
}

func TestHandleBroadcast_MalformedEnvelopeIgnored(t *testing.T) {
	metrics := &recordingMetrics{}
	r := newTestRegistryWithMetrics(t, &fakeDiscovery{}, metrics)

	// must not panic; ObserveBroadcastReceive is always called (defer fires).
	r.handleBroadcast([]byte("not-json"))

	if got := atomic.LoadInt64(&metrics.observeCount); got != 1 {
		t.Errorf("ObserveBroadcastReceive count = %d, want 1", got)
	}
	if got := atomic.LoadInt64(&metrics.panics); got != 0 {
		t.Errorf("BroadcastPanic count = %d, want 0", got)
	}
	if got := metrics.DropsForReason("malformed_envelope"); got != 1 {
		t.Errorf("IncBroadcastDrop(malformed_envelope) = %d, want 1", got)
	}
}

func TestHandleBroadcast_DispatchesReload(t *testing.T) {
	// countingDiscovery tracks how many Fetch calls arrive.
	disc := &countingDiscovery{doc: &DiscoveryDoc{Issuer: "https://idp.example", JWKSURI: "https://idp.example/jwks"}}
	metrics := &recordingMetrics{}
	r := newTestRegistryWithMetrics(t, disc, metrics)

	uri := "https://idp.example"
	tenant := uuid.New()
	p := &OidcProvider{
		ID:                 uuid.New(),
		WellKnownConfigURI: uri,
		CreatedAt:          time.Now(),
		OwnerLegalEntityID: tenant,
	}
	r.installForTest(p, nil, &DiscoveryDoc{Issuer: "https://idp.example", JWKSURI: "https://idp.example/jwks"})

	payload, _ := json.Marshal(broadcastEnvelope{Op: "reload", TenantID: tenant.String(), URI: uri})
	r.handleBroadcast(payload)

	if !pollUntil(func() bool { return atomic.LoadInt64(&disc.calls) >= 1 }) {
		t.Fatalf("reloadOne did not trigger a discovery Fetch within 1 s")
	}

	// Second identical broadcast: the singleflight key is "<tenant>:<uri>".
	// If the first goroutine is still in flight, the second is dropped.
	// Whether or not it's dropped is implementation-dependent timing, so we
	// only assert the first one fired; we do NOT assert a fixed total count
	// because the first may have finished by now.
	if atomic.LoadInt64(&disc.calls) < 1 {
		t.Errorf("expected at least 1 Fetch call, got 0")
	}
}

func TestHandleBroadcast_DispatchesReload_SingleflightDropsDuplicate(t *testing.T) {
	// Verify the singleflight key is "<tenantID>:<uri>" by holding the key in
	// flight (via a blocking discovery) while sending a second broadcast.
	gate := make(chan struct{})
	release := make(chan struct{})
	var fetchCount int64

	blockingDisc := discoveryFunc(func(_ context.Context, _ string) (*DiscoveryDoc, error) {
		atomic.AddInt64(&fetchCount, 1)
		gate <- struct{}{} // signal: I'm in flight
		<-release          // wait until the test releases me
		return &DiscoveryDoc{Issuer: "x", JWKSURI: "y"}, nil
	})

	metrics := &recordingMetrics{}
	r := newTestRegistryWithMetrics(t, blockingDisc, metrics)

	uri := "https://idp.example"
	tenant := uuid.New()
	p := &OidcProvider{
		ID:                 uuid.New(),
		WellKnownConfigURI: uri,
		CreatedAt:          time.Now(),
		OwnerLegalEntityID: tenant,
	}
	r.installForTest(p, nil, &DiscoveryDoc{Issuer: "x", JWKSURI: "y"})

	payload, _ := json.Marshal(broadcastEnvelope{Op: "reload", TenantID: tenant.String(), URI: uri})

	// First broadcast: fires the goroutine, which blocks in Fetch.
	r.handleBroadcast(payload)

	// Wait until the goroutine is actually inside Fetch.
	select {
	case <-gate:
	case <-time.After(time.Second):
		t.Fatal("first dispatch did not reach Fetch within 1 s")
	}

	// Second broadcast with same key: singleflight must drop it.
	dispatched := r.singleflight.Dispatch(tenant.String()+":"+uri, func() {})
	if dispatched {
		t.Error("singleflight should have dropped the duplicate dispatch (same key still in flight)")
	}

	// Unblock the first goroutine.
	close(release)

	if !pollUntil(func() bool { return atomic.LoadInt64(&fetchCount) == 1 }) {
		t.Errorf("fetchCount = %d, want exactly 1", atomic.LoadInt64(&fetchCount))
	}
}

func TestHandleBroadcast_DispatchesInvalidate(t *testing.T) {
	metrics := &recordingMetrics{}
	r := newTestRegistryWithMetrics(t, &fakeDiscovery{}, metrics)

	uri := "https://idp.example"
	tenant := uuid.New()
	p := &OidcProvider{
		ID:                 uuid.New(),
		WellKnownConfigURI: uri,
		CreatedAt:          time.Now(),
		OwnerLegalEntityID: tenant,
	}
	r.installForTest(p, nil, &DiscoveryDoc{Issuer: "https://idp.example", JWKSURI: "https://idp.example/jwks"})

	// Verify provider is present before broadcast.
	func() {
		r.mu.RLock()
		defer r.mu.RUnlock()
		if _, ok := r.providers[spi.TenantID(tenant.String())][uri]; !ok {
			t.Fatal("provider not installed before invalidate broadcast")
		}
	}()

	payload, _ := json.Marshal(broadcastEnvelope{Op: "invalidate", TenantID: tenant.String(), URI: uri})
	r.handleBroadcast(payload)

	// Wait for the singleflight goroutine to complete invalidateOne.
	providerGone := pollUntil(func() bool {
		r.mu.RLock()
		defer r.mu.RUnlock()
		_, present := r.providers[spi.TenantID(tenant.String())][uri]
		return !present
	})
	if !providerGone {
		t.Error("provider still present after invalidate broadcast")
	}
}

func TestHandleBroadcast_DispatchesReloadAll(t *testing.T) {
	metrics := &recordingMetrics{}
	r := newTestRegistryWithMetrics(t, &fakeDiscovery{}, metrics)

	// Plant a provider in KV — ReloadAll will pick it up from the store.
	ctx := context.Background()
	tenant := uuid.New()
	p := sampleProvider(t, spi.TenantID(tenant.String()), "https://idp-all.example")
	if err := r.store.Register(ctx, p); err != nil {
		t.Fatalf("store.Register: %v", err)
	}

	// The in-memory map is empty at this point.
	func() {
		r.mu.RLock()
		defer r.mu.RUnlock()
		if byURI := r.providers[spi.TenantID(tenant.String())]; byURI != nil {
			t.Fatal("expected empty in-memory map before reload_all")
		}
	}()

	payload, _ := json.Marshal(broadcastEnvelope{Op: "reload_all"})
	r.handleBroadcast(payload)

	// Wait for ReloadAll to complete and populate the map.
	loaded := pollUntil(func() bool {
		r.mu.RLock()
		defer r.mu.RUnlock()
		byURI := r.providers[spi.TenantID(tenant.String())]
		if byURI == nil {
			return false
		}
		_, ok := byURI[p.WellKnownConfigURI]
		return ok
	})
	if !loaded {
		t.Error("in-memory registry does not reflect KV state after reload_all broadcast")
	}
}

func TestHandleBroadcast_UnknownOpIsNoop(t *testing.T) {
	metrics := &recordingMetrics{}
	r := newTestRegistryWithMetrics(t, &fakeDiscovery{}, metrics)

	payload, _ := json.Marshal(broadcastEnvelope{Op: "bogus", TenantID: "x", URI: "y"})
	r.handleBroadcast(payload)

	if got := atomic.LoadInt64(&metrics.observeCount); got != 1 {
		t.Errorf("ObserveBroadcastReceive count = %d, want 1", got)
	}
	if got := atomic.LoadInt64(&metrics.panics); got != 0 {
		t.Errorf("BroadcastPanic count = %d, want 0", got)
	}
}

func TestHandleBroadcast_PanicRecovered(t *testing.T) {
	// Two panic paths exist in handleBroadcast per broadcast.go:
	//   1. Synchronous path (json.Unmarshal + switch) — guarded by the recover()
	//      in handleBroadcast's own defer.
	//   2. Goroutine dispatched by safeDispatch — guarded by safeDispatch's
	//      own recover() which also calls IncBroadcastPanic.
	//
	// This test covers both layers.

	uri := "https://idp.example"
	tenant := uuid.New()

	// --- Layer 1: synchronous panic via nil singleflight ---
	// A nil r.singleflight causes a nil-deref inside handleBroadcast's switch
	// (synchronous path) before any goroutine is spawned. The recover() in
	// handleBroadcast's defer catches it and increments IncBroadcastPanic.
	m1 := &recordingMetrics{}
	r1 := &Registry{
		providers:    map[spi.TenantID]map[string]*OidcProvider{},
		sources:      map[spi.TenantID]map[string]*providerSource{},
		kidIndex:     map[string][]providerRef{},
		store:        newTestStore(t),
		discovery:    &fakeDiscovery{},
		broadcast:    nil,
		singleflight: nil, // nil → Dispatch call panics synchronously
		metrics:      m1,
		logger:       newTestRegistry(t).logger,
	}
	p1, _ := json.Marshal(broadcastEnvelope{Op: "reload", TenantID: tenant.String(), URI: uri})
	r1.handleBroadcast(p1) // must not crash

	if got := atomic.LoadInt64(&m1.panics); got != 1 {
		t.Errorf("layer-1 BroadcastPanic count = %d, want 1", got)
	}
	if got := atomic.LoadInt64(&m1.observeCount); got != 1 {
		t.Errorf("layer-1 ObserveBroadcastReceive count = %d, want 1", got)
	}

	// --- Layer 2: goroutine-level panic via panicDiscovery ---
	// panicDiscovery.Fetch panics inside reloadOne, which runs inside the
	// goroutine spawned by safeDispatch. safeDispatch's own recover() catches
	// the panic and increments IncBroadcastPanic.
	m2 := &recordingMetrics{}
	r2 := newTestRegistryWithMetrics(t, panicDiscovery{}, m2)
	p2 := &OidcProvider{
		ID:                 uuid.New(),
		WellKnownConfigURI: uri,
		CreatedAt:          time.Now(),
		OwnerLegalEntityID: tenant,
	}
	// Plant provider so reloadOne passes the I9 existence check before calling
	// discovery.Fetch — which panics.
	r2.installForTest(p2, nil, &DiscoveryDoc{Issuer: "x", JWKSURI: "y"})

	payload2, _ := json.Marshal(broadcastEnvelope{Op: "reload", TenantID: tenant.String(), URI: uri})
	r2.handleBroadcast(payload2) // must not crash; safeDispatch catches the panic

	if !pollUntil(func() bool { return atomic.LoadInt64(&m2.panics) > 0 }) {
		t.Errorf("layer-2 BroadcastPanic count = 0, want >0 (goroutine panic must be caught by safeDispatch)")
	}
}

// discoveryFunc is a function-valued Discovery — lets tests provide inline Fetch logic.
type discoveryFunc func(ctx context.Context, uri string) (*DiscoveryDoc, error)

func (f discoveryFunc) Fetch(ctx context.Context, uri string) (*DiscoveryDoc, error) {
	return f(ctx, uri)
}

// ---------- Audit C1: field-length cap tests ----------

// TestHandleBroadcast_DropsOversizedOp verifies that an envelope whose Op
// field exceeds maxBroadcastOpLen is silently dropped — no work dispatched.
func TestHandleBroadcast_DropsOversizedOp(t *testing.T) {
	metrics := &recordingMetrics{}
	r := newTestRegistryWithMetrics(t, &fakeDiscovery{}, metrics)

	payload, _ := json.Marshal(broadcastEnvelope{
		Op:       strings.Repeat("X", maxBroadcastOpLen+1),
		TenantID: "00000000-0000-0000-0000-000000000001",
		URI:      "https://idp.example",
	})
	r.handleBroadcast(payload)

	if got := r.singleflight.inFlightCount(); got != 0 {
		t.Errorf("inFlightCount = %d, want 0 (oversized Op must not dispatch)", got)
	}
	if got := atomic.LoadInt64(&metrics.panics); got != 0 {
		t.Errorf("BroadcastPanic = %d, want 0", got)
	}
	if got := metrics.DropsForReason("oversized_op"); got != 1 {
		t.Errorf("IncBroadcastDrop(oversized_op) = %d, want 1", got)
	}
}

// TestHandleBroadcast_DropsOversizedTenantID verifies that an envelope whose
// TenantID field exceeds maxBroadcastTenantIDLen is silently dropped.
func TestHandleBroadcast_DropsOversizedTenantID(t *testing.T) {
	metrics := &recordingMetrics{}
	r := newTestRegistryWithMetrics(t, &fakeDiscovery{}, metrics)

	payload, _ := json.Marshal(broadcastEnvelope{
		Op:       "reload",
		TenantID: strings.Repeat("0", maxBroadcastTenantIDLen+1),
		URI:      "https://idp.example",
	})
	r.handleBroadcast(payload)

	if got := r.singleflight.inFlightCount(); got != 0 {
		t.Errorf("inFlightCount = %d, want 0 (oversized TenantID must not dispatch)", got)
	}
	if got := atomic.LoadInt64(&metrics.panics); got != 0 {
		t.Errorf("BroadcastPanic = %d, want 0", got)
	}
	if got := metrics.DropsForReason("oversized_tenantid"); got != 1 {
		t.Errorf("IncBroadcastDrop(oversized_tenantid) = %d, want 1", got)
	}
}

// TestHandleBroadcast_DropsOversizedURI verifies that an envelope whose URI
// field exceeds maxBroadcastURILen is silently dropped.
func TestHandleBroadcast_DropsOversizedURI(t *testing.T) {
	metrics := &recordingMetrics{}
	r := newTestRegistryWithMetrics(t, &fakeDiscovery{}, metrics)

	payload, _ := json.Marshal(broadcastEnvelope{
		Op:       "reload",
		TenantID: "00000000-0000-0000-0000-000000000001",
		URI:      "https://idp.example/" + strings.Repeat("x", maxBroadcastURILen),
	})
	r.handleBroadcast(payload)

	if got := r.singleflight.inFlightCount(); got != 0 {
		t.Errorf("inFlightCount = %d, want 0 (oversized URI must not dispatch)", got)
	}
	if got := atomic.LoadInt64(&metrics.panics); got != 0 {
		t.Errorf("BroadcastPanic = %d, want 0", got)
	}
	if got := metrics.DropsForReason("oversized_uri"); got != 1 {
		t.Errorf("IncBroadcastDrop(oversized_uri) = %d, want 1", got)
	}
}

// TestHandleBroadcast_AcceptsBoundaryLengths verifies that envelopes at exactly
// the field length limits are accepted (not dropped). We use an unknown op so
// the envelope flows through the length check but dispatches no goroutine —
// the assertion is simply that no panic fired.
func TestHandleBroadcast_AcceptsBoundaryLengths(t *testing.T) {
	metrics := &recordingMetrics{}
	r := newTestRegistryWithMetrics(t, &fakeDiscovery{}, metrics)

	// Op exactly at limit: fill to maxBroadcastOpLen chars. Using a non-recognised
	// op routes to the unknown-op DEBUG log path, which does no dispatching.
	payload, _ := json.Marshal(broadcastEnvelope{
		Op:       strings.Repeat("Z", maxBroadcastOpLen),
		TenantID: strings.Repeat("0", maxBroadcastTenantIDLen),
		URI:      strings.Repeat("u", maxBroadcastURILen),
	})
	r.handleBroadcast(payload)

	if got := atomic.LoadInt64(&metrics.panics); got != 0 {
		t.Errorf("BroadcastPanic = %d, want 0 (boundary-length envelope must not panic)", got)
	}
	if got := atomic.LoadInt64(&metrics.observeCount); got != 1 {
		t.Errorf("ObserveBroadcastReceive = %d, want 1", got)
	}
}
