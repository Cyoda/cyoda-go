package entity

import (
	"math"
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
