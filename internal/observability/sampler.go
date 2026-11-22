package observability

import (
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"sync/atomic"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// SamplerConfig is the high-level, operator-facing sampler configuration.
// It is the JSON shape of both POST and GET /admin/trace-sampler.
//
// The Ratio field is only meaningful when Sampler == "ratio"; it is omitted
// from JSON when zero (valid because "ratio" with Ratio=0 is equivalent to
// "never" and the validation path rejects that combination anyway).
type SamplerConfig struct {
	Sampler     string  `json:"sampler"`         // "always" | "never" | "ratio"
	Ratio       float64 `json:"ratio,omitempty"` // required iff Sampler == "ratio"
	ParentBased bool    `json:"parent_based"`    // default true
}

// samplerState bundles an OTel sampler with the config that produced it.
// We store both so the admin handler can return a round-trippable config
// from GET without reverse-engineering OTel's sampler description string.
type samplerState struct {
	sampler sdktrace.Sampler
	config  SamplerConfig
}

// DynamicSampler is an sdktrace.Sampler whose inner sampler can be replaced
// at runtime. Reads and writes are lock-free via atomic.Pointer, so the
// sampling hot path pays no synchronization cost.
//
// The exported package-global Sampler is the instance installed on the
// TracerProvider at Init time. The admin handler calls its SetSampler
// method to replace the inner sampler.
type DynamicSampler struct {
	current atomic.Pointer[samplerState]
}

// Sampler is the package-global dynamic sampler installed on the
// TracerProvider. Mirrors internal/logging.Level.
var Sampler = NewDynamicSampler()

// NewDynamicSampler returns a DynamicSampler with a safe initial state
// (ParentBased(AlwaysSample)). Init() replaces this immediately with the
// env-configured config, but having a valid initial value means
// pre-Init spans do not panic.
func NewDynamicSampler() *DynamicSampler {
	ds := &DynamicSampler{}
	s, _ := BuildSampler(SamplerConfig{Sampler: "always", ParentBased: true})
	ds.current.Store(&samplerState{
		sampler: s,
		config:  SamplerConfig{Sampler: "always", ParentBased: true},
	})
	return ds
}

// ShouldSample delegates to the current inner sampler.
func (d *DynamicSampler) ShouldSample(p sdktrace.SamplingParameters) sdktrace.SamplingResult {
	return d.current.Load().sampler.ShouldSample(p)
}

// Description returns the current inner sampler's description.
func (d *DynamicSampler) Description() string {
	return d.current.Load().sampler.Description()
}

// SetSampler atomically replaces the inner sampler from a SamplerConfig.
// Returns an error if cfg is invalid.
func (d *DynamicSampler) SetSampler(cfg SamplerConfig) error {
	s, err := BuildSampler(cfg)
	if err != nil {
		return err
	}
	d.current.Store(&samplerState{sampler: s, config: cfg})
	return nil
}

// Config returns the last-set SamplerConfig. Round-trippable with POST.
func (d *DynamicSampler) Config() SamplerConfig {
	return d.current.Load().config
}

// Inner returns the current inner sdktrace.Sampler. Used by tests and
// by the Init path that passes this sampler to sdktrace.WithSampler.
func (d *DynamicSampler) Inner() sdktrace.Sampler {
	return d.current.Load().sampler
}

// BuildSampler constructs an sdktrace.Sampler from a SamplerConfig.
// Used by NewDynamicSampler and SetSampler — one place for all
// construction logic.
func BuildSampler(cfg SamplerConfig) (sdktrace.Sampler, error) {
	var inner sdktrace.Sampler
	switch cfg.Sampler {
	case "always":
		if cfg.Ratio != 0 {
			return nil, fmt.Errorf("ratio not allowed for sampler=%q", cfg.Sampler)
		}
		inner = sdktrace.AlwaysSample()
	case "never":
		if cfg.Ratio != 0 {
			return nil, fmt.Errorf("ratio not allowed for sampler=%q", cfg.Sampler)
		}
		inner = sdktrace.NeverSample()
	case "ratio":
		if cfg.Ratio <= 0 || cfg.Ratio > 1 {
			return nil, fmt.Errorf("ratio must be in (0, 1] for sampler=ratio (use sampler=never for zero sampling), got %g", cfg.Ratio)
		}
		inner = sdktrace.TraceIDRatioBased(cfg.Ratio)
	default:
		return nil, fmt.Errorf("unknown sampler type: accepted values are always, never, ratio")
	}
	if cfg.ParentBased {
		inner = sdktrace.ParentBased(inner)
	}
	return inner, nil
}

// SamplerConfigFromEnv parses standard OTel env vars into a SamplerConfig.
// Supported OTEL_TRACES_SAMPLER values:
//   - (unset) → default (always, parent_based)
//   - always_on → AlwaysSample root
//   - always_off → NeverSample root
//   - traceidratio → TraceIDRatioBased(OTEL_TRACES_SAMPLER_ARG) root
//   - parentbased_always_on → ParentBased(AlwaysSample)
//   - parentbased_always_off → ParentBased(NeverSample)
//   - parentbased_traceidratio → ParentBased(TraceIDRatioBased(arg))
//
// On unknown values, parse failures, or out-of-range ratios, logs WARN
// and falls back to a sensible default. Never fails startup.
func SamplerConfigFromEnv() SamplerConfig {
	samplerEnv := os.Getenv("OTEL_TRACES_SAMPLER")
	argEnv := os.Getenv("OTEL_TRACES_SAMPLER_ARG")

	switch samplerEnv {
	case "":
		return SamplerConfig{Sampler: "always", ParentBased: true}
	case "always_on":
		return SamplerConfig{Sampler: "always", ParentBased: false}
	case "always_off":
		return SamplerConfig{Sampler: "never", ParentBased: false}
	case "traceidratio":
		return SamplerConfig{Sampler: "ratio", Ratio: parseRatioOrDefault(argEnv, 1.0), ParentBased: false}
	case "parentbased_always_on":
		return SamplerConfig{Sampler: "always", ParentBased: true}
	case "parentbased_always_off":
		return SamplerConfig{Sampler: "never", ParentBased: true}
	case "parentbased_traceidratio":
		return SamplerConfig{Sampler: "ratio", Ratio: parseRatioOrDefault(argEnv, 1.0), ParentBased: true}
	default:
		slog.Warn("unknown OTEL_TRACES_SAMPLER value, using default",
			"pkg", "observability", "value", samplerEnv)
		return SamplerConfig{Sampler: "always", ParentBased: true}
	}
}

// parseRatioOrDefault parses s as a float in (0, 1]. On parse failure or
// out-of-range, logs WARN and returns fallback.
func parseRatioOrDefault(s string, fallback float64) float64 {
	if s == "" {
		return fallback
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		slog.Warn("invalid OTEL_TRACES_SAMPLER_ARG, using fallback",
			"pkg", "observability", "value", s, "fallback", fallback, "error", err)
		return fallback
	}
	if v <= 0 || v > 1 {
		slog.Warn("OTEL_TRACES_SAMPLER_ARG out of range (0, 1], using fallback",
			"pkg", "observability", "value", v, "fallback", fallback)
		return fallback
	}
	return v
}
