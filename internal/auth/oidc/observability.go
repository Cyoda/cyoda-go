package oidc

// Metrics is the OIDC subsystem's instrumentation interface per spec D22.
// Production wiring (OpenTelemetry / Prometheus) lands in issue #313 — for
// this PR, NopMetrics is the only implementation. Registry, broadcast
// handler, and resolver all call methods on Metrics at the D22 call sites;
// switching to a real impl is a one-line constructor change.
type Metrics interface {
	IncKidCacheHit()
	IncKidCacheMiss()
	IncKidCacheEvict()
	IncJWKSFetchError(outcome string)
	IncBroadcastPanic()
	IncUnknownProviderBroadcast()
	ObserveBroadcastReceive(seconds float64)
	SetRegistryProviders(n int)
}

// NopMetrics is the default implementation — no-ops. Used by tests and as
// the placeholder until issue #313 wires real instrumentation.
type NopMetrics struct{}

func (NopMetrics) IncKidCacheHit()                        {}
func (NopMetrics) IncKidCacheMiss()                       {}
func (NopMetrics) IncKidCacheEvict()                      {}
func (NopMetrics) IncJWKSFetchError(outcome string)       {}
func (NopMetrics) IncBroadcastPanic()                     {}
func (NopMetrics) IncUnknownProviderBroadcast()           {}
func (NopMetrics) ObserveBroadcastReceive(seconds float64) {}
func (NopMetrics) SetRegistryProviders(n int)             {}

// Compile-time guard.
var _ Metrics = NopMetrics{}
