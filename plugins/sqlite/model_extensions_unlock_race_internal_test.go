package sqlite

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// TestSQLite_ExtendSchema_RacingUnlock_EveryReturnedDeltaKept — round after
// round, a first wave of extensions returns before Unlock starts, then a
// second wave runs at once with a real Unlock. Every extension that returned
// nil is in the model's schema after Unlock (the first wave always is, so
// every Unlock folds a non-empty log), every other one returned
// spi.ErrModelNotLocked, and Unlock leaves the log empty. Which of the second
// wave win is not asserted.
func TestSQLite_ExtendSchema_RacingUnlock_EveryReturnedDeltaKept(t *testing.T) {
	const (
		rounds    = 30
		extenders = 4
		firstWave = 2
	)
	fx := newSQLiteFixtureWithInterval(t, 2)
	fx.store.applyFunc = setUnionApplyFunc
	ref := spi.ModelRef{EntityName: "E", ModelVersion: "1"}
	fx.SaveModel(t, ref, []byte(`[]`))

	var kept []string
	refused := 0
	for r := 0; r < rounds; r++ {
		if err := fx.store.Lock(fx.ctx, ref); err != nil {
			t.Fatalf("round %d: Lock: %v", r, err)
		}
		// The first wave returns before Unlock starts: its deltas are in the
		// log when Unlock folds it, and must be in the schema afterwards.
		for i := 0; i < firstWave; i++ {
			tok := fmt.Sprintf("r%02dfirst%d", r, i)
			if err := fx.store.ExtendSchema(fx.ctx, ref, spi.SchemaDelta(`"`+tok+`"`)); err != nil {
				t.Fatalf("round %d: first-wave extension %d: %v", r, i, err)
			}
			kept = append(kept, tok)
		}
		start := make(chan struct{})
		errs := make([]error, extenders)
		var unlockErr error
		var wg sync.WaitGroup
		for i := 0; i < extenders; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				ctx, cancel := context.WithTimeout(fx.ctx, 30*time.Second)
				defer cancel()
				errs[i] = fx.store.ExtendSchema(ctx, ref, spi.SchemaDelta(fmt.Sprintf(`"r%02di%d"`, r, i)))
			}()
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			ctx, cancel := context.WithTimeout(fx.ctx, 30*time.Second)
			defer cancel()
			unlockErr = fx.store.Unlock(ctx, ref)
		}()
		close(start)
		wg.Wait()

		if unlockErr != nil {
			t.Fatalf("round %d: Unlock: %v", r, unlockErr)
		}
		for i, err := range errs {
			switch {
			case err == nil:
				kept = append(kept, fmt.Sprintf("r%02di%d", r, i))
			case errors.Is(err, spi.ErrModelNotLocked):
				refused++
			default:
				t.Fatalf("round %d: extender %d: %v", r, i, err)
			}
		}
		if rows := countExtensionRows(t, fx, ref, ""); rows != 0 {
			t.Fatalf("round %d: extension log after Unlock has %d rows, want 0", r, rows)
		}
		got, err := fx.store.Get(fx.ctx, ref)
		if err != nil {
			t.Fatalf("round %d: Get: %v", r, err)
		}
		var toks []string
		if err := json.Unmarshal(got.Schema, &toks); err != nil {
			t.Fatalf("round %d: decode %s: %v", r, got.Schema, err)
		}
		have := make(map[string]bool, len(toks))
		for _, tok := range toks {
			have[tok] = true
		}
		for _, tok := range kept {
			if !have[tok] {
				t.Fatalf("round %d: extension %q returned nil but is not in the schema %s", r, tok, got.Schema)
			}
		}
	}
	t.Logf("%d extensions kept, %d refused over %d rounds", len(kept), refused, rounds)
}
