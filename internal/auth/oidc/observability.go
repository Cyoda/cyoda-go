package oidc

// Metrics is the OIDC subsystem's instrumentation interface per spec D22.
// The production implementation is NewOTelMetrics (metrics_otel.go); NopMetrics
// is the no-op used by tests and in non-JWT IAM modes. Registry, broadcast
// handler, and resolver all call methods on Metrics at the D22 call sites.
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

// NopMetrics is a no-op Metrics implementation. Used by tests and as the
// fallback in non-JWT IAM modes (where the OIDC subsystem is inactive).
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
