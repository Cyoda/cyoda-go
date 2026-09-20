package registry

import (
	"context"
	"testing"
	"time"

	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

func startMeteredGossip(t *testing.T, id string, port int) (*Gossip, *sdkmetric.ManualReader) {
	t.Helper()
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })
	g, err := NewGossip(GossipConfig{
		NodeID:           id,
		NodeAddr:         "http://" + id + ".test:8080",
		BindAddr:         "127.0.0.1",
		BindPort:         port,
		StabilityWindow:  200 * time.Millisecond,
		ListScanInterval: 200 * time.Millisecond,
		Meter:            mp.Meter("test"),
	})
	if err != nil {
		t.Fatalf("NewGossip: %v", err)
	}
	t.Cleanup(func() { _ = g.Deregister(context.Background(), id) })
	return g, reader
}

// int64Value returns the sum of every data point of the named instrument.
func int64Value(t *testing.T, reader *sdkmetric.ManualReader, name string) (int64, bool) {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect: %v", err)
	}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != name {
				continue
			}
			var total int64
			switch data := m.Data.(type) {
			case metricdata.Sum[int64]:
				for _, dp := range data.DataPoints {
					total += dp.Value
				}
			case metricdata.Gauge[int64]:
				for _, dp := range data.DataPoints {
					total += dp.Value
				}
			default:
				t.Fatalf("%s has unexpected data type %T", name, m.Data)
			}
			return total, true
		}
	}
	return 0, false
}

func TestGossipMetrics_ListsOutstanding(t *testing.T) {
	g, reader := startMeteredGossip(t, "metric-out-1", 27946)
	v1 := listVersion{Epoch: 5, Seq: 1}
	p := startRawPeer(t, "metric-out-peer", 27947, rawMeta(t, "metric-out-peer", v1), "127.0.0.1:27946")

	waitFor(t, 5*time.Second, "one peer's list is outstanding", func() bool {
		v, ok := int64Value(t, reader, "cyoda.cluster.tags.lists_outstanding")
		return ok && v == 1
	})

	waitFor(t, 5*time.Second, "the peer is asked", func() bool { return p.requestCount() >= 1 })
	p.send(t, "metric-out-1", 27946, topicTags, tagListMsg{NodeID: "metric-out-peer", Version: v1, Tags: map[string][]string{"t": {"x"}}})
	waitFor(t, 5*time.Second, "nothing is outstanding once the list is held", func() bool {
		v, ok := int64Value(t, reader, "cyoda.cluster.tags.lists_outstanding")
		return ok && v == 0
	})
	_ = g
}

func TestGossipMetrics_SendFailures(t *testing.T) {
	g, reader := startMeteredGossip(t, "metric-fail-1", 27948)
	p := startRawPeer(t, "metric-fail-peer", 27949, rawMeta(t, "metric-fail-peer", listVersion{Epoch: 5}), "127.0.0.1:27948")
	waitFor(t, 5*time.Second, "metric-fail-1 sees the peer", func() bool {
		_, ok := g.member("metric-fail-peer")
		return ok
	})

	// The peer dies without leaving. Until memberlist notices, it is still an
	// alive member whose port refuses connections.
	if err := p.list.Shutdown(); err != nil {
		t.Fatal(err)
	}
	if err := g.UpdateTags(map[string][]string{"tenant-a": {"python"}}); err != nil {
		t.Fatal(err)
	}

	waitFor(t, 5*time.Second, "a failed send is counted", func() bool {
		v, ok := int64Value(t, reader, "cyoda.cluster.tags.send_failures")
		return ok && v >= 1
	})
}
