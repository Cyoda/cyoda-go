package entity

import (
	"math"
	"math/rand"
	"testing"
)

func TestBuildGroupKey_DistinguishesNullFromEmpty(t *testing.T) {
	k1 := buildGroupKey([]any{nil})
	k2 := buildGroupKey([]any{""})
	if k1 == k2 {
		t.Fatalf("null and empty string should encode differently")
	}
}

func TestBuildGroupKey_CollisionAdversarial(t *testing.T) {
	// ["a|b","c"] vs ["a","b|c"] would collide under naive strings.Join.
	k1 := buildGroupKey([]any{"a|b", "c"})
	k2 := buildGroupKey([]any{"a", "b|c"})
	if k1 == k2 {
		t.Fatalf("collision on pipe-separated values")
	}
}

func TestAccumulator_WelfordStdev(t *testing.T) {
	a := newAccumulator(AggregationExprValidated{
		Op: AggStdev, Field: "x", Alias: "stdev_x",
	})
	samples := []float64{50.0, 50.5, 49.5, 50.2, 49.8}
	for _, x := range samples {
		a.observeFloat(x)
	}
	got, ok := a.result()
	if !ok {
		t.Fatalf("expected ok result")
	}
	// Hand-computation: mean = 50.0; squared deviations 0, 0.25, 0.25, 0.04,
	// 0.04 sum to 0.58; sample variance = 0.58/(5-1) = 0.145; sample stdev =
	// sqrt(0.145) = 0.380788655293...
	want := 0.38078865529319543
	if math.Abs(got-want)/want > 1e-9 {
		t.Fatalf("stdev got %.12f, want ~%.12f", got, want)
	}
}

func TestAccumulator_StdevNBelow2IsNil(t *testing.T) {
	a := newAccumulator(AggregationExprValidated{Op: AggStdev, Alias: "s"})
	if _, ok := a.result(); ok {
		t.Fatalf("n=0 should not produce a stdev result")
	}
	a.observeFloat(42.0)
	if _, ok := a.result(); ok {
		t.Fatalf("n=1 should not produce a stdev result")
	}
}

// TestAccumulator_WelfordStability_OneMillionSamples verifies that the
// Welford recurrence remains numerically stable at 1M samples. Existing
// unit tests pin small-N parity (5 samples vs hand-computed reference);
// the cross-backend parity test pins Welford ↔ STDDEV_SAMP at small
// scale. This scale test complements both by pinning the in-process
// accumulator at production-scale sample counts: 1M draws from N(50, 4)
// must converge to stdev=2.0 within statistical noise.
//
// Tolerance: 5e-3 relative error is ~5σ on the sample-stdev estimator at
// this N — generous to avoid flakes, tight enough to catch a regression
// where the Welford update grows catastrophic cancellation. The seed is
// pinned so the test is deterministic.
func TestAccumulator_WelfordStability_OneMillionSamples(t *testing.T) {
	if testing.Short() {
		t.Skip("scale test; -short skips")
	}
	a := newAccumulator(AggregationExprValidated{Op: AggStdev, Field: "x", Alias: "s"})
	const (
		n        = 1_000_000
		wantMean = 50.0
		wantStd  = 2.0
		seed     = int64(20260614)
	)
	rng := rand.New(rand.NewSource(seed))
	for i := 0; i < n; i++ {
		x := rng.NormFloat64()*wantStd + wantMean
		a.observeFloat(x)
	}
	got, ok := a.result()
	if !ok {
		t.Fatalf("expected ok result")
	}
	relErr := math.Abs(got-wantStd) / wantStd
	if relErr > 0.005 {
		t.Fatalf("stdev=%.6f want %.4f (rel err %.4e > 5e-3)", got, wantStd, relErr)
	}
	t.Logf("n=%d stdev=%.6f rel_err=%.4e", n, got, relErr)
}
