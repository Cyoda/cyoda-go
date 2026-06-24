package oidc

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

func TestOTelMetrics_EmitsAllNineInstruments(t *testing.T) {
	reader := metric.NewManualReader()
	mp := metric.NewMeterProvider(metric.WithReader(reader))
	m, err := NewOTelMetrics(mp.Meter("test"))
	if err != nil {
		t.Fatalf("NewOTelMetrics: %v", err)
	}
	m.IncKidCacheHit()
	m.IncKidCacheMiss()
	m.IncKidCacheEvict()
	m.IncJWKSFetchError("discovery")
	m.IncBroadcastPanic()
	m.IncBroadcastDrop("malformed_envelope")
	m.IncUnknownProviderBroadcast()
	m.ObserveBroadcastReceive(0.003)
	m.SetRegistryProviders(4)

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect: %v", err)
	}
	got := map[string]bool{}
	for _, sm := range rm.ScopeMetrics {
		for _, md := range sm.Metrics {
			got[md.Name] = true
		}
	}
	for _, name := range []string{
		"oidc.kid.cache.hit", "oidc.kid.cache.miss", "oidc.kid.cache.evict",
		"oidc.jwks.fetch.error", "oidc.broadcast.panic", "oidc.broadcast.drop",
		"oidc.unknown_provider.broadcast", "oidc.broadcast.receive", "oidc.registry.providers",
	} {
		if !got[name] {
			t.Errorf("instrument %q not emitted", name)
		}
	}
}

func TestOTelMetrics_RegistryProvidersHasNoAttributes(t *testing.T) {
	reader := metric.NewManualReader()
	mp := metric.NewMeterProvider(metric.WithReader(reader))
	m, _ := NewOTelMetrics(mp.Meter("test"))
	m.SetRegistryProviders(7)

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect: %v", err)
	}
	for _, sm := range rm.ScopeMetrics {
		for _, md := range sm.Metrics {
			if md.Name != "oidc.registry.providers" {
				continue
			}
			g, ok := md.Data.(metricdata.Gauge[int64])
			if !ok {
				t.Fatalf("oidc.registry.providers is %T, want Gauge[int64]", md.Data)
			}
			if len(g.DataPoints) != 1 {
				t.Fatalf("want 1 datapoint, got %d", len(g.DataPoints))
			}
			if g.DataPoints[0].Attributes.Len() != 0 {
				t.Errorf("oidc.registry.providers must have no attributes, got %v", g.DataPoints[0].Attributes)
			}
			if g.DataPoints[0].Value != 7 {
				t.Errorf("value=%d want 7", g.DataPoints[0].Value)
			}
			return
		}
	}
	t.Fatal("oidc.registry.providers not found")
}

func TestOTelMetrics_DropCarriesReasonAttribute(t *testing.T) {
	reader := metric.NewManualReader()
	mp := metric.NewMeterProvider(metric.WithReader(reader))
	m, _ := NewOTelMetrics(mp.Meter("test"))
	m.IncBroadcastDrop("oversized_uri")

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect: %v", err)
	}
	for _, sm := range rm.ScopeMetrics {
		for _, md := range sm.Metrics {
			if md.Name != "oidc.broadcast.drop" {
				continue
			}
			s := md.Data.(metricdata.Sum[int64])
			if len(s.DataPoints) != 1 {
				t.Fatalf("want 1 datapoint, got %d", len(s.DataPoints))
			}
			v, ok := s.DataPoints[0].Attributes.Value("reason")
			if !ok || v.AsString() != "oversized_uri" {
				t.Errorf("reason attribute = %q (present=%v), want oversized_uri", v.AsString(), ok)
			}
			return
		}
	}
	t.Fatal("oidc.broadcast.drop not found")
}
