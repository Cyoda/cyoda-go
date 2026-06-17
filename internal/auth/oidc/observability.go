package oidc

// Metrics is the OIDC subsystem's instrumentation interface per spec D22.
// Production instrumentation lands in a follow-up; NopMetrics is the only
// implementation today. Registry, broadcast handler, and resolver all call
// methods on Metrics at the D22 call sites; switching to a real impl is a
// one-line constructor change.
type Metrics interface {
	IncKidCacheHit()
	IncKidCacheMiss()
	IncKidCacheEvict()
	IncJWKSFetchError(outcome string)
	IncBroadcastPanic()
	IncBroadcastDrop(reason string)
	IncUnknownProviderBroadcast()
	ObserveBroadcastReceive(seconds float64)
	SetRegistryProviders(n int)
}

// NopMetrics is the default implementation — no-ops. Used by tests and as
// the placeholder until production instrumentation is wired.
type NopMetrics struct{}

func (NopMetrics) IncKidCacheHit()                        {}
func (NopMetrics) IncKidCacheMiss()                       {}
func (NopMetrics) IncKidCacheEvict()                      {}
func (NopMetrics) IncJWKSFetchError(outcome string)       {}
func (NopMetrics) IncBroadcastPanic()                     {}
func (NopMetrics) IncBroadcastDrop(reason string)         {}
func (NopMetrics) IncUnknownProviderBroadcast()           {}
func (NopMetrics) ObserveBroadcastReceive(seconds float64) {}
func (NopMetrics) SetRegistryProviders(n int)             {}

// Compile-time guard.
var _ Metrics = NopMetrics{}
