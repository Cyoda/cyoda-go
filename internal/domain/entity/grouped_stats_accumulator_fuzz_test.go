package entity

import (
	"testing"
)

// FuzzBuildGroupKey verifies that two encodings collide only when the
// inputs (as a []any slice with nil-or-string entries) are equal. The
// fuzzer generates pairs of inputs; we assert input-equality ⟺
// output-equality.
//
// Seed corpus pins the known adversarial cases plus identity cases.
// Failure means D18's collision-free contract is broken — either an
// equal-input pair produced different keys (non-determinism) or a
// distinct-input pair collided (real D18 regression).
func FuzzBuildGroupKey(f *testing.F) {
	f.Add([]byte("a|b"), []byte("c"), []byte("a"), []byte("b|c"))
	f.Add([]byte(""), []byte(""), []byte(""), []byte(""))
	f.Add([]byte{0x00}, []byte{0x01}, []byte{0x01}, []byte{0x00})
	f.Add([]byte("alpha"), []byte("beta"), []byte("alpha"), []byte("beta"))
	f.Add([]byte{0x00, 0x00, 0x00, 0x08, 'a'}, []byte(""), []byte("a"), []byte(""))

	f.Fuzz(func(t *testing.T, a1, a2, b1, b2 []byte) {
		keyA := buildGroupKey([]any{string(a1), string(a2)})
		keyB := buildGroupKey([]any{string(b1), string(b2)})

		inputsEqual := string(a1) == string(b1) && string(a2) == string(b2)
		outputsEqual := keyA == keyB

		if inputsEqual && !outputsEqual {
			t.Fatalf("equal inputs produced different keys: a=%q,%q b=%q,%q keyA=%x keyB=%x",
				a1, a2, b1, b2, keyA, keyB)
		}
		if !inputsEqual && outputsEqual {
			t.Fatalf("distinct inputs collided: a=%q,%q b=%q,%q both=%x",
				a1, a2, b1, b2, keyA)
		}
	})
}

// FuzzBuildGroupKey_NullAndStrings exercises the nil sentinel path that
// FuzzBuildGroupKey can't generate (the fuzzer only produces []byte → string
// — never typed-nil entries). Verifies the nil-vs-empty-string distinction
// promised by D18 holds for any string input the fuzzer can find.
func FuzzBuildGroupKey_NullAndStrings(f *testing.F) {
	f.Add([]byte(""), false)
	f.Add([]byte("alpha"), true)
	f.Add([]byte{0x00, 0x00, 0x00, 0x08, 'a'}, false)
	f.Add([]byte{0x01}, true)

	f.Fuzz(func(t *testing.T, val []byte, useNil bool) {
		var entry any
		if useNil {
			entry = nil
		} else {
			entry = string(val)
		}

		keyA := buildGroupKey([]any{entry})
		keyB := buildGroupKey([]any{entry})

		if keyA != keyB {
			t.Fatalf("non-deterministic encoding: %q (useNil=%v) → %x vs %x", val, useNil, keyA, keyB)
		}

		// The nil-string distinction: nil never equals any string encoding.
		if useNil {
			withEmpty := buildGroupKey([]any{""})
			if keyA == withEmpty {
				t.Fatalf("nil collided with empty string")
			}
			withVal := buildGroupKey([]any{string(val)})
			if keyA == withVal {
				t.Fatalf("nil collided with string %q", val)
			}
		}
	})
}
