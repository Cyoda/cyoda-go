package goroutinesafety

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"
)

// TestChecker_Fixtures pins the analyser's own behaviour: the guard over the
// E2E suite is only meaningful if it actually fires. Each case is a complete
// source file; wantViolations lists a distinguishing substring per expected
// finding, in sorted order.
func TestChecker_Fixtures(t *testing.T) {
	cases := []struct {
		name           string
		src            string
		wantViolations []string
	}{
		{
			name: "direct t.Fatalf inside go func",
			src: `package p
import "testing"
func TestX(t *testing.T) {
	go func() { t.Fatalf("boom") }()
}`,
			wantViolations: []string{"calls t.Fatalf from a goroutine"},
		},
		{
			name: "t handed to a helper inside go func",
			src: `package p
import "testing"
func helper(t *testing.T) {}
func TestX(t *testing.T) {
	go func() { helper(t) }()
}`,
			wantViolations: []string{"passes t to helper(…) from a goroutine"},
		},
		{
			name: "t handed to a method inside go func",
			src: `package p
import "testing"
type h struct{}
func TestX(t *testing.T) {
	var x h
	go func() { x.Create(t, 1) }()
}`,
			wantViolations: []string{"passes t to x.Create(…) from a goroutine"},
		},
		{
			name: "go f(t) passes t to a concurrent body",
			src: `package p
import "testing"
func worker(t *testing.T) {}
func TestX(t *testing.T) {
	go worker(t)
}`,
			wantViolations: []string{"passes t to worker(…) from a goroutine"},
		},
		{
			name: "go t.Fatal directly",
			src: `package p
import "testing"
func TestX(t *testing.T) {
	go t.Fatal("boom")
}`,
			wantViolations: []string{"calls t.Fatal from a goroutine"},
		},
		{
			name: "aliased t used in a goroutine",
			src: `package p
import "testing"
func TestX(t *testing.T) {
	tt := t
	go func() { tt.Fatalf("boom") }()
}`,
			wantViolations: []string{"calls tt.Fatalf from a goroutine"},
		},
		{
			name: "aliased t handed to a helper in a goroutine",
			src: `package p
import "testing"
func helper(t *testing.T) {}
func TestX(t *testing.T) {
	tt := t
	go func() { helper(tt) }()
}`,
			wantViolations: []string{"passes tt to helper(…) from a goroutine"},
		},
		{
			name: "violation nested in a closure inside the goroutine",
			src: `package p
import "testing"
func TestX(t *testing.T) {
	go func() {
		func() { t.FailNow() }()
	}()
}`,
			wantViolations: []string{"calls t.FailNow from a goroutine"},
		},
		{
			name: "subtest t shadows the outer t",
			src: `package p
import "testing"
func TestX(t *testing.T) {
	t.Run("sub", func(t *testing.T) {
		go func() { t.Skip("nope") }()
	})
}`,
			wantViolations: []string{"calls t.Skip from a goroutine"},
		},
		{
			name: "testing.TB parameter is covered too",
			src: `package p
import "testing"
func withTB(tb testing.TB) {
	go func() { tb.Fatal("boom") }()
}`,
			wantViolations: []string{"calls tb.Fatal from a goroutine"},
		},
		{
			name: "goroutine-safe methods are allowed",
			src: `package p
import "testing"
func TestX(t *testing.T) {
	go func() {
		t.Errorf("bad")
		t.Logf("note")
		t.Error("bad")
		t.Log("note")
		_ = t.Name()
		_ = t.Failed()
	}()
}`,
		},
		{
			name: "helpers that do not take t are allowed",
			src: `package p
import "testing"
func rawHelper(path string) (int, error) { return 0, nil }
func TestX(t *testing.T) {
	done := make(chan int, 1)
	go func() {
		status, _ := rawHelper("/x")
		done <- status
	}()
	if <-done != 200 {
		t.Fatal("bad status")
	}
}`,
		},
		{
			name: "Fatal on the test goroutine is allowed",
			src: `package p
import "testing"
func helper(t *testing.T) { t.Fatal("boom") }
func TestX(t *testing.T) {
	helper(t)
	t.Fatalf("done")
}`,
		},
		{
			name: "arguments of a go statement evaluate on the test goroutine",
			src: `package p
import "testing"
func value(t *testing.T) int { return 1 }
func worker(v int) {}
func TestX(t *testing.T) {
	go worker(value(t))
}`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fset := token.NewFileSet()
			file, err := parser.ParseFile(fset, "fixture.go", tc.src, 0)
			if err != nil {
				t.Fatalf("parse fixture: %v", err)
			}

			got := analyze(fset, []*ast.File{file})
			if len(got) != len(tc.wantViolations) {
				t.Fatalf("got %d violation(s), want %d\ngot:  %v\nwant: %v",
					len(got), len(tc.wantViolations), got, tc.wantViolations)
			}
			for i, want := range tc.wantViolations {
				if !strings.Contains(got[i], want) {
					t.Errorf("violation %d = %q; want it to contain %q", i, got[i], want)
				}
			}
		})
	}
}
