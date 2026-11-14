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

// TestSQLite_ExtendSchema_RacingUnlock_EveryReturnedDeltaKept — extenders
// and a real Unlock run at once, round after round. Every extension that
// returned nil is in the model's schema after Unlock, every other one
// returned spi.ErrModelNotLocked, and Unlock leaves the log empty.
func TestSQLite_ExtendSchema_RacingUnlock_EveryReturnedDeltaKept(t *testing.T) {
	const (
		rounds    = 30
		extenders = 4
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
			// Unlock starts with the extenders on even rounds and after a
			// delay growing with the round on odd ones, so the rounds cover
			// Unlock ahead of every extender, behind them, and between.
			if r%2 == 1 {
				time.Sleep(time.Duration(r) * 100 * time.Microsecond)
			}
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
	if len(kept) == 0 || refused == 0 {
		t.Errorf("the rounds never interleaved: %d kept, %d refused; want both", len(kept), refused)
	}
}
