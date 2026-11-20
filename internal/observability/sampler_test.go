package observability_test

import (
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	"github.com/cyoda/cyoda-go/internal/observability"
)

func TestDynamicSampler_InitialDefault(t *testing.T) {
	cfg := observability.SamplerConfig{Sampler: "always", ParentBased: true}
	ds := observability.NewDynamicSampler()
	if err := ds.SetSampler(cfg); err != nil {
		t.Fatalf("SetSampler: %v", err)
	}

	got := ds.Config()
	if got != cfg {
		t.Errorf("Config = %+v, want %+v", got, cfg)
	}

	res := ds.ShouldSample(sdktrace.SamplingParameters{Name: "test"})
	if res.Decision != sdktrace.RecordAndSample {
		t.Errorf("decision = %v, want RecordAndSample", res.Decision)
	}
}

func TestDynamicSampler_SetSampler_Ratio(t *testing.T) {
	ds := observability.NewDynamicSampler()
	cfg := observability.SamplerConfig{Sampler: "ratio", Ratio: 0.1, ParentBased: true}
	if err := ds.SetSampler(cfg); err != nil {
		t.Fatalf("SetSampler: %v", err)
	}

	got := ds.Config()
	if got != cfg {
		t.Errorf("Config = %+v, want %+v", got, cfg)
	}

	desc := ds.Description()
	if !strings.Contains(desc, "ParentBased") {
		t.Errorf("Description = %q, want to contain ParentBased", desc)
	}
	if !strings.Contains(desc, "TraceIDRatioBased") {
		t.Errorf("Description = %q, want to contain TraceIDRatioBased", desc)
	}
}

func TestDynamicSampler_SetSampler_Never(t *testing.T) {
	ds := observability.NewDynamicSampler()
	cfg := observability.SamplerConfig{Sampler: "never", ParentBased: false}
	if err := ds.SetSampler(cfg); err != nil {
		t.Fatalf("SetSampler: %v", err)
	}

	res := ds.ShouldSample(sdktrace.SamplingParameters{Name: "test"})
	if res.Decision != sdktrace.Drop {
		t.Errorf("decision = %v, want Drop", res.Decision)
	}
}

func TestDynamicSampler_ConcurrentReadDuringSet(t *testing.T) {
	ds := observability.NewDynamicSampler()
	if err := ds.SetSampler(observability.SamplerConfig{Sampler: "always", ParentBased: true}); err != nil {
		t.Fatalf("SetSampler: %v", err)
	}

	var wg sync.WaitGroup
	stop := atomic.Bool{}
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for !stop.Load() {
				_ = ds.ShouldSample(sdktrace.SamplingParameters{Name: "test"})
			}
		}()
	}

	configs := []observability.SamplerConfig{
		{Sampler: "always", ParentBased: true},
		{Sampler: "never", ParentBased: true},
		{Sampler: "ratio", Ratio: 0.5, ParentBased: true},
		{Sampler: "ratio", Ratio: 0.01, ParentBased: false},
	}
	deadline := time.Now().Add(1 * time.Second)
	for time.Now().Before(deadline) {
		for _, c := range configs {
			if err := ds.SetSampler(c); err != nil {
				t.Errorf("SetSampler: %v", err)
			}
		}
	}
	stop.Store(true)
	wg.Wait()
}

func TestDynamicSampler_Inner(t *testing.T) {
	ds := observability.NewDynamicSampler()
	if err := ds.SetSampler(observability.SamplerConfig{Sampler: "always", ParentBased: true}); err != nil {
		t.Fatalf("SetSampler: %v", err)
	}

	var inner sdktrace.Sampler = ds.Inner()
	if inner == nil {
		t.Fatal("Inner returned nil")
	}
}

func TestBuildSampler_Valid(t *testing.T) {
	cases := []struct {
		name     string
		cfg      observability.SamplerConfig
		wantDesc string
	}{
		{"always root", observability.SamplerConfig{Sampler: "always", ParentBased: false}, "AlwaysOnSampler"},
		{"always parent-based", observability.SamplerConfig{Sampler: "always", ParentBased: true}, "ParentBased"},
		{"never root", observability.SamplerConfig{Sampler: "never", ParentBased: false}, "AlwaysOffSampler"},
		{"never parent-based", observability.SamplerConfig{Sampler: "never", ParentBased: true}, "ParentBased"},
		{"ratio root", observability.SamplerConfig{Sampler: "ratio", Ratio: 0.1, ParentBased: false}, "TraceIDRatioBased"},
		{"ratio parent-based", observability.SamplerConfig{Sampler: "ratio", Ratio: 0.1, ParentBased: true}, "ParentBased"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s, err := observability.BuildSampler(c.cfg)
			if err != nil {
				t.Fatalf("BuildSampler: %v", err)
			}
			if s == nil {
				t.Fatal("sampler is nil")
			}
			if !strings.Contains(s.Description(), c.wantDesc) {
				t.Errorf("Description = %q, want to contain %q", s.Description(), c.wantDesc)
			}
		})
	}
}

func TestBuildSampler_Invalid(t *testing.T) {
	cases := []struct {
		name string
		cfg  observability.SamplerConfig
	}{
		{"unknown type", observability.SamplerConfig{Sampler: "foo"}},
		{"ratio negative", observability.SamplerConfig{Sampler: "ratio", Ratio: -0.1}},
		{"ratio zero", observability.SamplerConfig{Sampler: "ratio", Ratio: 0}},
		{"ratio too high", observability.SamplerConfig{Sampler: "ratio", Ratio: 1.5}},
		{"ratio on always", observability.SamplerConfig{Sampler: "always", Ratio: 0.5}},
		{"ratio on never", observability.SamplerConfig{Sampler: "never", Ratio: 0.5}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := observability.BuildSampler(c.cfg)
			if err == nil {
				t.Errorf("BuildSampler(%+v) = nil, want error", c.cfg)
			}
		})
	}
}

func TestSamplerConfigFromEnv_Table(t *testing.T) {
	cases := []struct {
		name       string
		envSampler string
		envArg     string
		want       observability.SamplerConfig
	}{
		{"unset", "", "", observability.SamplerConfig{Sampler: "always", ParentBased: true}},
		{"always_on", "always_on", "", observability.SamplerConfig{Sampler: "always", ParentBased: false}},
		{"always_off", "always_off", "", observability.SamplerConfig{Sampler: "never", ParentBased: false}},
		{"traceidratio", "traceidratio", "0.25", observability.SamplerConfig{Sampler: "ratio", Ratio: 0.25, ParentBased: false}},
		{"parentbased_always_on", "parentbased_always_on", "", observability.SamplerConfig{Sampler: "always", ParentBased: true}},
		{"parentbased_always_off", "parentbased_always_off", "", observability.SamplerConfig{Sampler: "never", ParentBased: true}},
		{"parentbased_traceidratio", "parentbased_traceidratio", "0.5", observability.SamplerConfig{Sampler: "ratio", Ratio: 0.5, ParentBased: true}},
		{"unknown", "wat", "", observability.SamplerConfig{Sampler: "always", ParentBased: true}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("OTEL_TRACES_SAMPLER", c.envSampler)
			t.Setenv("OTEL_TRACES_SAMPLER_ARG", c.envArg)
			got := observability.SamplerConfigFromEnv()
			if got != c.want {
				t.Errorf("SamplerConfigFromEnv = %+v, want %+v", got, c.want)
			}
		})
	}
}

func TestSamplerConfigFromEnv_RatioParseFailure(t *testing.T) {
	t.Setenv("OTEL_TRACES_SAMPLER", "traceidratio")
	t.Setenv("OTEL_TRACES_SAMPLER_ARG", "not-a-number")
	got := observability.SamplerConfigFromEnv()
	want := observability.SamplerConfig{Sampler: "ratio", Ratio: 1.0, ParentBased: false}
	if got != want {
		t.Errorf("SamplerConfigFromEnv = %+v, want %+v (fallback to 1.0)", got, want)
	}
}

func TestSamplerConfigFromEnv_RatioOutOfRange(t *testing.T) {
	t.Setenv("OTEL_TRACES_SAMPLER", "parentbased_traceidratio")
	t.Setenv("OTEL_TRACES_SAMPLER_ARG", "1.5")
	got := observability.SamplerConfigFromEnv()
	want := observability.SamplerConfig{Sampler: "ratio", Ratio: 1.0, ParentBased: true}
	if got != want {
		t.Errorf("SamplerConfigFromEnv = %+v, want %+v (fallback to 1.0)", got, want)
	}
}

func TestSamplerConfigFromEnv_RatioZero(t *testing.T) {
	t.Setenv("OTEL_TRACES_SAMPLER", "traceidratio")
	t.Setenv("OTEL_TRACES_SAMPLER_ARG", "0")
	got := observability.SamplerConfigFromEnv()
	want := observability.SamplerConfig{Sampler: "ratio", Ratio: 1.0, ParentBased: false}
	if got != want {
		t.Errorf("SamplerConfigFromEnv = %+v, want %+v (fallback to 1.0 since ratio=0 is invalid)", got, want)
	}
}
