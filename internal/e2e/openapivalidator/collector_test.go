package openapivalidator

import (
	"sync"
	"testing"
)

func TestCollector_AppendAndDrain(t *testing.T) {
	c := newCollector()
	c.append(Mismatch{Operation: "op1", Status: 200, Reason: "r1"})
	c.append(Mismatch{Operation: "op2", Status: 400, Reason: "r2"})

	out := c.drain()
	if len(out) != 2 {
		t.Fatalf("len = %d, want 2", len(out))
	}
	if out[0].Operation != "op1" || out[1].Operation != "op2" {
		t.Errorf("ordering mismatch: %+v", out)
	}
	if remaining := c.drain(); len(remaining) != 0 {
		t.Errorf("collector not drained: %d remaining", len(remaining))
	}
}

func TestCollector_RecordExercised(t *testing.T) {
	c := newCollector()
	c.recordExercised("opA")
	c.recordExercised("opB")
	c.recordExercised("opA")
	exercised := c.exerciseSet()
	if len(exercised) != 2 {
		t.Errorf("got %d unique ops, want 2", len(exercised))
	}
	if !exercised["opA"] || !exercised["opB"] {
		t.Errorf("missing keys: %v", exercised)
	}
}

func TestCollector_ConcurrentAppend(t *testing.T) {
	c := newCollector()
	var wg sync.WaitGroup
	const n = 1000
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c.append(Mismatch{Operation: "op"})
			c.recordExercised("op")
		}()
	}
	wg.Wait()
	if got := len(c.drain()); got != n {
		t.Errorf("got %d mismatches, want %d", got, n)
	}
}
