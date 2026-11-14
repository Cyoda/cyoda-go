package postgres

import (
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// TestFold_OneApplyCall: the apply function decodes and re-encodes the whole
// schema on every call, so the fold makes one call for all the deltas after
// the savepoint, and an extension makes two under the model's lock — the fold
// and the fold plus the new delta — however long the log is, a savepoint
// included (it stores the second result).
func TestFold_OneApplyCall(t *testing.T) {
	setup := func(t *testing.T, interval int) (*pgFixture, *atomic.Int64, spi.ModelRef) {
		fx := newPGFixtureWithInterval(t, interval)
		calls := &atomic.Int64{}
		fx.store.applyFunc = func(base []byte, deltas []spi.SchemaDelta) ([]byte, error) {
			calls.Add(1)
			return setUnionApplyFunc(base, deltas)
		}
		ref := spi.ModelRef{EntityName: "fold-calls", ModelVersion: "1"}
		fx.SaveModel(t, ref, []byte(`[]`))
		return fx, calls, ref
	}
	extend := func(t *testing.T, fx *pgFixture, ref spi.ModelRef, ds ...string) {
		t.Helper()
		for _, d := range ds {
			if err := fx.store.ExtendSchema(fx.ctx, ref, spi.SchemaDelta(d)); err != nil {
				t.Fatalf("ExtendSchema %s: %v", d, err)
			}
		}
	}

	t.Run("log", func(t *testing.T) {
		fx, calls, ref := setup(t, 1000)
		extend(t, fx, ref, `"d1"`, `"d2"`, `"d3"`, `"d4"`, `"d5"`)
		if got := fx.countRows(t, ref, "delta"); got != 5 {
			t.Fatalf("delta rows = %d, want 5", got)
		}

		calls.Store(0)
		desc, err := fx.store.Get(fx.ctx, ref)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if got := calls.Load(); got != 1 {
			t.Errorf("Get over 5 deltas made %d apply calls, want 1", got)
		}
		if string(desc.Schema) != `["d1","d2","d3","d4","d5"]` {
			t.Errorf("folded schema = %s", desc.Schema)
		}

		calls.Store(0)
		extend(t, fx, ref, `"d6"`)
		if got := calls.Load(); got != 2 {
			t.Errorf("an extension over 5 deltas made %d apply calls, want 2", got)
		}

		calls.Store(0)
		extend(t, fx, ref, `"d3"`)
		if got := calls.Load(); got != 2 {
			t.Errorf("a no-op extension over 6 deltas made %d apply calls, want 2", got)
		}
		if got := fx.countRows(t, ref, "delta"); got != 6 {
			t.Errorf("delta rows = %d, want 6 (the held delta writes nothing)", got)
		}
	})

	t.Run("savepoint", func(t *testing.T) {
		fx, calls, ref := setup(t, 3)
		extend(t, fx, ref, `"d1"`, `"d2"`)
		calls.Store(0)
		extend(t, fx, ref, `"d3"`)
		if got := fx.countRows(t, ref, "savepoint"); got != 1 {
			t.Fatalf("savepoint rows = %d, want 1", got)
		}
		if got := calls.Load(); got != 2 {
			t.Errorf("an extension writing a savepoint made %d apply calls, want 2", got)
		}
		desc, err := fx.store.Get(fx.ctx, ref)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		// The savepoint comes back from a JSONB column, respelled.
		if string(sortNewlineTokens(desc.Schema)) != `["d1","d2","d3"]` {
			t.Errorf("schema from the savepoint = %s", desc.Schema)
		}
	})
}

// TestFold_ApplyErrorNamesLogPosition: a fold the apply function refuses names
// how many deltas it applied and the seq they follow (the savepoint's, 0 for
// the doc), as sqlite's fold does.
func TestFold_ApplyErrorNamesLogPosition(t *testing.T) {
	fx := newPGFixtureWithInterval(t, 2)
	fx.store.applyFunc = setUnionApplyFunc
	ref := spi.ModelRef{EntityName: "fold-error", ModelVersion: "1"}
	fx.SaveModel(t, ref, []byte(`[]`))
	for _, d := range []string{`"d1"`, `"d2"`, `"d3"`} {
		if err := fx.store.ExtendSchema(fx.ctx, ref, spi.SchemaDelta(d)); err != nil {
			t.Fatalf("ExtendSchema %s: %v", d, err)
		}
	}
	sp := fx.lastSavepointSeq(t, ref)
	if sp == 0 {
		t.Fatal("precondition: a savepoint was written")
	}
	fx.store.applyFunc = func([]byte, []spi.SchemaDelta) ([]byte, error) {
		return nil, errors.New("refused")
	}
	_, err := fx.store.Get(fx.ctx, ref)
	if err == nil {
		t.Fatal("Get folded a log its apply function refuses")
	}
	if want := fmt.Sprintf("apply 1 deltas after seq %d", sp); !strings.Contains(err.Error(), want) {
		t.Errorf("error %q does not contain %q", err, want)
	}
}
