package postgres

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// lockWaitersOnModelRow counts the backends waiting on a lock in the
// extension's state statement.
func lockWaitersOnModelRow(t *testing.T, fx *pgFixture) int {
	t.Helper()
	var n int
	if err := fx.db.QueryRow(context.Background(), `
		SELECT COUNT(*) FROM pg_stat_activity
		WHERE datname = current_database() AND wait_event_type = 'Lock'
		  AND query LIKE '%FOR NO KEY UPDATE%'`).Scan(&n); err != nil {
		t.Fatalf("read pg_stat_activity: %v", err)
	}
	return n
}

// TestExtendSchema_WaitingBehindUnlock_RefusedAndNothingWritten — Unlock's
// state update holds the model row, uncommitted, while extenders queue on
// the row lock. When it commits, every extender re-reads the row Unlock
// wrote, sees UNLOCKED and returns spi.ErrModelNotLocked: none writes a
// delta to a model whose log Unlock has already folded and cleared, so no
// extension that returned nil is lost, and the schema is unchanged.
func TestExtendSchema_WaitingBehindUnlock_RefusedAndNothingWritten(t *testing.T) {
	fx := newPGFixture(t)
	fx.store.applyFunc = setUnionApplyFunc
	n := cap(fx.store.models.extSlots)
	ref := spi.ModelRef{EntityName: "E", ModelVersion: "1"}
	fx.SaveModel(t, ref, []byte(`["base"]`))
	before, err := fx.store.Get(fx.ctx, ref)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}

	// Unlock's state update, held open.
	unlock, err := fx.pool.Begin(context.Background())
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = unlock.Rollback(context.Background()) }()
	if _, err := unlock.Exec(context.Background(), `
		UPDATE models SET doc = jsonb_set(doc, '{state}', to_jsonb($4::text))
		WHERE tenant_id = $1 AND model_name = $2 AND model_version = $3`,
		string(fx.tenantID), ref.EntityName, ref.ModelVersion, string(spi.ModelUnlocked)); err != nil {
		t.Fatalf("state update: %v", err)
	}

	errs := make([]error, n)
	var wg sync.WaitGroup
	done := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(fx.ctx, 30*time.Second)
			defer cancel()
			errs[i] = fx.store.ExtendSchema(ctx, ref, spi.SchemaDelta(fmt.Sprintf(`"d%d"`, i)))
		}()
	}
	go func() { wg.Wait(); close(done) }()

	// Every extender waits on the row lock, or (if the state were read
	// without the lock) has already returned.
	deadline := time.Now().Add(20 * time.Second)
wait:
	for lockWaitersOnModelRow(t, fx) < n {
		select {
		case <-done:
			break wait
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("only %d of %d extenders reached the row lock", lockWaitersOnModelRow(t, fx), n)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := unlock.Commit(context.Background()); err != nil {
		t.Fatalf("commit the state update: %v", err)
	}
	<-done

	for i, err := range errs {
		if !errors.Is(err, spi.ErrModelNotLocked) {
			t.Errorf("extender %d = %v, want spi.ErrModelNotLocked", i, err)
		}
	}
	if rows := fx.countRows(t, ref, ""); rows != 0 {
		t.Errorf("extension log after the race has %d rows, want 0", rows)
	}
	after, err := fx.store.Get(fx.ctx, ref)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !bytes.Equal(after.Schema, before.Schema) {
		t.Errorf("schema = %s, want it unchanged at %s", after.Schema, before.Schema)
	}
}

// TestExtendSchema_RacingUnlock_EveryReturnedDeltaKept — round after round,
// a first wave of extensions returns before Unlock starts, then a second wave
// runs at once with a real Unlock. Every extension that returned nil is in
// the model's schema after Unlock (the first wave always is, so every Unlock
// folds a non-empty log), every other one returned spi.ErrModelNotLocked, and
// Unlock leaves the log empty. Which of the second wave win is not asserted.
func TestExtendSchema_RacingUnlock_EveryReturnedDeltaKept(t *testing.T) {
	const (
		rounds    = 30
		firstWave = 2
	)
	fx := newPGFixtureWithInterval(t, 2)
	fx.store.applyFunc = setUnionApplyFunc
	n := cap(fx.store.models.extSlots)
	ref := spi.ModelRef{EntityName: "E", ModelVersion: "1"}
	fx.SaveModel(t, ref, []byte(`[]`))

	var kept []string
	refused := 0
	for r := 0; r < rounds; r++ {
		if r > 0 {
			if err := fx.store.Lock(fx.ctx, ref); err != nil {
				t.Fatalf("round %d: Lock: %v", r, err)
			}
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
		errs := make([]error, n)
		var unlockErr error
		var wg sync.WaitGroup
		for i := 0; i < n; i++ {
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
		if rows := fx.countRows(t, ref, ""); rows != 0 {
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
