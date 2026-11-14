package schema_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"

	"github.com/cyoda-platform/cyoda-go/internal/domain/model/schema"
	"github.com/cyoda-platform/cyoda-go/internal/domain/model/schema/gentree"
)

// applyOne replays one delta onto the stored bytes base in its own ApplyMany
// call.
func applyOne(base []byte, delta spi.SchemaDelta) ([]byte, error) {
	return schema.ApplyMany(base, []spi.SchemaDelta{delta})
}

// applyDelta replays one delta onto base through ApplyMany, the apply function
// the plugins use: base is encoded, the delta applied, and the result decoded.
func applyDelta(base *schema.ModelNode, delta spi.SchemaDelta) (*schema.ModelNode, error) {
	raw, err := schema.Marshal(base)
	if err != nil {
		return nil, err
	}
	out, err := applyOne(raw, delta)
	if err != nil {
		return nil, err
	}
	return schema.Unmarshal(out)
}

// TestApplyMany_EqualsOneCallPerDelta: replaying a list of deltas in one call
// gives the bytes that replaying them one call each gives, and fails exactly
// when that does. The deltas mix the two shapes a log holds: one diffed from
// the schema as folded so far (a writer that saw every earlier extension) and
// one diffed from the original base (a writer on a stale descriptor), with an
// empty delta and, in some draws, a delta that cannot apply.
func TestApplyMany_EqualsOneCallPerDelta(t *testing.T) {
	cfg := gentree.DefaultConfig()
	cfg.TargetLevel = spi.ChangeLevelStructural
	cfg.KindMutationRate = 0.3
	const N = 500
	var ran, skipped int
	for i := 0; i < N; i++ {
		seed := int64(i + 70_000)
		t.Run(fmt.Sprintf("seed=%d", seed), func(t *testing.T) {
			r := gentree.NewRNG(seed)
			base := gentree.GenModelNode(r, cfg.MaxDepth, cfg.MaxWidth, cfg)
			baseBytes, err := schema.Marshal(base)
			if err != nil {
				t.Fatal(err)
			}

			var deltas []spi.SchemaDelta
			folded := baseBytes
			count := 2 + r.IntN(7)
			for j := 0; j < count; j++ {
				refBytes := folded
				if r.IntN(3) == 0 {
					refBytes = baseBytes
				}
				ref, err := schema.Unmarshal(refBytes)
				if err != nil {
					t.Fatal(err)
				}
				incoming := gentree.GenExtensionPair(r, ref, cfg.TargetLevel, cfg)
				refAgain, err := schema.Unmarshal(refBytes)
				if err != nil {
					t.Fatal(err)
				}
				extended, err := schema.Extend(refAgain, incoming, cfg.TargetLevel)
				if err != nil {
					continue
				}
				d, err := schema.Diff(ref, extended)
				if err != nil {
					t.Fatalf("Diff: %v", err)
				}
				deltas = append(deltas, d)
				if next, err := applyOne(folded, d); err == nil {
					folded = next
				}
			}
			if len(deltas) == 0 {
				skipped++
				t.Skip("no delta drawn")
			}
			ran++
			// An empty delta somewhere in the list.
			at := r.IntN(len(deltas) + 1)
			deltas = append(deltas[:at], append([]spi.SchemaDelta{nil}, deltas[at:]...)...)
			// A delta that cannot apply, in one draw out of four.
			if r.IntN(4) == 0 {
				bad, err := schema.MarshalDelta([]schema.SchemaOp{
					schema.NewAddProperty("no/such/parent", "x", mustLeafPayload(t)),
				})
				if err != nil {
					t.Fatal(err)
				}
				at := r.IntN(len(deltas) + 1)
				deltas = append(deltas[:at], append([]spi.SchemaDelta{bad}, deltas[at:]...)...)
			}

			want := baseBytes
			var wantErr error
			for _, d := range deltas {
				if want, wantErr = applyOne(want, d); wantErr != nil {
					break
				}
			}
			got, gotErr := schema.ApplyMany(baseBytes, deltas)
			if (wantErr == nil) != (gotErr == nil) {
				t.Fatalf("one call per delta failed=%v (%v), one call failed=%v (%v)", wantErr != nil, wantErr, gotErr != nil, gotErr)
			}
			if wantErr != nil {
				return
			}
			if !bytes.Equal(got, want) {
				t.Fatalf("ApplyMany differs from one call per delta\n  base=%s\n  one call per delta=%s\n  one call=%s", baseBytes, want, got)
			}
		})
	}
	assertSkipRatio(t, ran, skipped, "TestApplyMany_EqualsOneCallPerDelta")
}

// TestApplyMany_EmptyListIsCanonicalRoundTrip: an empty list re-encodes base
// in the canonical form, whatever spelling base arrived in, and a canonical
// output comes back unchanged — so an extension can compare its fold with the
// fold plus the new delta directly, without a second re-encoding.
func TestApplyMany_EmptyListIsCanonicalRoundTrip(t *testing.T) {
	cfg := gentree.DefaultConfig()
	cfg.TargetLevel = spi.ChangeLevelStructural
	cfg.KindMutationRate = 0.3
	for i := 0; i < 300; i++ {
		seed := int64(i + 80_000)
		t.Run(fmt.Sprintf("seed=%d", seed), func(t *testing.T) {
			r := gentree.NewRNG(seed)
			node := gentree.GenModelNode(r, cfg.MaxDepth, cfg.MaxWidth, cfg)
			canonical, err := schema.Marshal(node)
			if err != nil {
				t.Fatal(err)
			}
			// The spelling a JSON column hands back: other whitespace, other
			// key order.
			var generic any
			if err := json.Unmarshal(canonical, &generic); err != nil {
				t.Fatal(err)
			}
			respelled, err := json.MarshalIndent(generic, "", "  ")
			if err != nil {
				t.Fatal(err)
			}
			for name, in := range map[string][]byte{"canonical": canonical, "respelled": respelled} {
				for _, empty := range [][]spi.SchemaDelta{nil, {}} {
					got, err := schema.ApplyMany(in, empty)
					if err != nil {
						t.Fatalf("%s: %v", name, err)
					}
					if !bytes.Equal(got, canonical) {
						t.Fatalf("%s: empty list gave %s, want %s", name, got, canonical)
					}
				}
			}
			// The fold's output is a fixed point of the empty list.
			folded, err := schema.ApplyMany(respelled, []spi.SchemaDelta{nil})
			if err != nil {
				t.Fatal(err)
			}
			again, err := schema.ApplyMany(folded, nil)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(again, folded) {
				t.Fatalf("re-encoding a fold changed it:\n  %s\n  %s", folded, again)
			}
		})
	}
}

// TestApplyMany_DoesNotTouchItsInputs: the list is read, never written.
func TestApplyMany_DoesNotTouchItsInputs(t *testing.T) {
	root := schema.NewObjectNode()
	root.SetChild("a", schema.NewLeafNode(schema.String))
	base, err := schema.Marshal(root)
	if err != nil {
		t.Fatal(err)
	}
	d, err := schema.MarshalDelta([]schema.SchemaOp{schema.NewAddProperty("", "b", mustLeafPayload(t))})
	if err != nil {
		t.Fatal(err)
	}
	baseCopy := append([]byte(nil), base...)
	dCopy := append(spi.SchemaDelta(nil), d...)
	if _, err := schema.ApplyMany(base, []spi.SchemaDelta{d}); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(base, baseCopy) || !bytes.Equal(d, dCopy) {
		t.Fatal("ApplyMany wrote to its inputs")
	}
}

func mustLeafPayload(t *testing.T) []byte {
	t.Helper()
	b, err := schema.Marshal(schema.NewLeafNode(schema.String))
	if err != nil {
		t.Fatal(err)
	}
	return b
}
