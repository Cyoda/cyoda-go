package sqlite

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// withSavepointInterval is a test-only Option that overrides
// cfg.SchemaSavepointInterval on the factory. Production wiring reads
// this from CYODA_SCHEMA_SAVEPOINT_INTERVAL via parseConfig; tests use
// this option to exercise the savepoint trigger deterministically.
func withSavepointInterval(n int) Option {
	return func(f *StoreFactory) { f.cfg.SchemaSavepointInterval = n }
}

// setUnionApplyFunc is an associative-commutative-idempotent apply used
// by Sub-project B tests. The "schema" representation is a JSON array of
// unique string tokens, an ExtendSchema delta is a JSON-encoded string
// token (e.g. `"d00"`), and fold = sorted union.
//
// Copied verbatim from plugins/postgres/model_extensions_internal_test.go
// so the sqlite fold tests remain self-contained.
func setUnionApplyFunc(base []byte, deltas []spi.SchemaDelta) ([]byte, error) {
	m := map[string]struct{}{}
	if len(base) > 0 {
		var existing []string
		if err := json.Unmarshal(base, &existing); err != nil {
			return nil, fmt.Errorf("setUnionApplyFunc: decode base %q: %w", base, err)
		}
		for _, tok := range existing {
			m[tok] = struct{}{}
		}
	}
	for _, delta := range deltas {
		if len(delta) == 0 {
			continue
		}
		var tok string
		if err := json.Unmarshal([]byte(delta), &tok); err != nil {
			return nil, fmt.Errorf("setUnionApplyFunc: decode delta %q: %w", delta, err)
		}
		m[tok] = struct{}{}
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return json.Marshal(keys)
}

// sortNewlineTokens canonicalises a set-union schema (JSON array of
// strings, as produced by setUnionApplyFunc) so two folds produced under
// different orderings (or with/without interior savepoints) can be
// byte-compared. Decodes, sorts, and re-encodes.
//
// Copied verbatim from plugins/postgres/model_extensions_internal_test.go
// so the sqlite fold tests remain self-contained.
func sortNewlineTokens(b []byte) []byte {
	if len(b) == 0 {
		return []byte("[]")
	}
	var toks []string
	if err := json.Unmarshal(b, &toks); err != nil {
		// Fallback: plain newline-joined form (used in some error paths).
		parts := strings.Split(string(b), "\n")
		sort.Strings(parts)
		return []byte(strings.Join(parts, "\n"))
	}
	sort.Strings(toks)
	out, _ := json.Marshal(toks)
	return out
}

// sqliteFixture is the internal-package fixture for modelStore tests
// that need access to unexported methods (foldLocked, lastSavepointSeq,
// etc.) and to the concrete *modelStore value. Mirrors pgFixture in
// plugins/postgres/model_extensions_internal_test.go.
//
// Tests interact with the underlying *sql.DB via fx.store.db.ExecContext
// (the sqlite-correct form) rather than the pgx-style Exec(ctx, ...)
// used in the postgres fixture.
type sqliteFixture struct {
	factory  *StoreFactory
	store    *modelStore
	ctx      context.Context
	tenantID spi.TenantID
}

// newSQLiteFixture constructs a fixture with the default savepoint
// interval (64).
func newSQLiteFixture(t *testing.T) *sqliteFixture {
	return newSQLiteFixtureWithInterval(t, defaultSchemaSavepointInterval)
}

// newSQLiteFixtureWithInterval constructs a fixture like newSQLiteFixture
// but with cfg.SchemaSavepointInterval set at factory-construction time —
// used by tests that need a small interval to exercise the savepoint
// trigger deterministically.
func newSQLiteFixtureWithInterval(t *testing.T, interval int) *sqliteFixture {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "test.db")
	f, err := NewStoreFactoryForTest(context.Background(), dbPath, withSavepointInterval(interval))
	if err != nil {
		t.Fatalf("NewStoreFactoryForTest: %v", err)
	}
	t.Cleanup(func() { _ = f.Close() })
	f.SetApplyFunc(fixtureApplyFunc())

	tenantID := spi.TenantID("t1")
	uc := &spi.UserContext{
		UserID:   "test-user",
		UserName: "Test User",
		Tenant:   spi.Tenant{ID: tenantID, Name: string(tenantID)},
		Roles:    []string{"USER"},
	}
	ctx := spi.WithUserContext(context.Background(), uc)

	ms, err := f.ModelStore(ctx)
	if err != nil {
		t.Fatalf("ModelStore: %v", err)
	}
	store, ok := ms.(*modelStore)
	if !ok {
		t.Fatalf("ModelStore did not return *modelStore; got %T", ms)
	}
	return &sqliteFixture{
		factory:  f,
		store:    store,
		ctx:      ctx,
		tenantID: tenantID,
	}
}

// SaveModel seeds the base model row for ref with the given base schema.
func (fx *sqliteFixture) SaveModel(t *testing.T, ref spi.ModelRef, base []byte) {
	t.Helper()
	desc := &spi.ModelDescriptor{
		Ref:         ref,
		State:       spi.ModelUnlocked,
		ChangeLevel: spi.ChangeLevelStructural,
		Schema:      base,
		UpdateDate:  time.Now().UTC().Truncate(time.Millisecond),
	}
	if err := fx.store.Save(fx.ctx, desc); err != nil {
		t.Fatalf("Save: %v", err)
	}
}

// fixtureApplyFunc is the default recording apply function for fixtures.
// It nests each delta under `$$applied` so observers can detect whether
// apply was invoked. Mirrors plugins/postgres's fixture helper.
func fixtureApplyFunc() ApplyFunc {
	return func(base []byte, deltas []spi.SchemaDelta) ([]byte, error) {
		var m map[string]json.RawMessage
		if len(base) == 0 {
			m = map[string]json.RawMessage{}
		} else if err := json.Unmarshal(base, &m); err != nil {
			m = map[string]json.RawMessage{"$$base": json.RawMessage(base)}
		}
		var applied []json.RawMessage
		if raw, ok := m["$$applied"]; ok {
			if err := json.Unmarshal(raw, &applied); err != nil {
				return nil, err
			}
		}
		for _, delta := range deltas {
			applied = append(applied, json.RawMessage(delta))
		}
		encoded, err := json.Marshal(applied)
		if err != nil {
			return nil, err
		}
		m["$$applied"] = encoded
		return json.Marshal(m)
	}
}

// TestSQLite_foldLocked_NoDeltas_ReturnsBase — sanity check for the
// new fold path. If no extension rows exist, fold returns the base
// schema verbatim (applyFunc not required).
func TestSQLite_foldLocked_NoDeltas_ReturnsBase(t *testing.T) {
	fx := newSQLiteFixture(t)
	ref := spi.ModelRef{EntityName: "E", ModelVersion: "1"}
	fx.SaveModel(t, ref, []byte(`{"base":true}`))

	got, err := foldViaTx(t, fx, ref, []byte(`{"base":true}`))
	if err != nil {
		t.Fatalf("foldLocked: %v", err)
	}
	if !bytes.Equal(got, []byte(`{"base":true}`)) {
		t.Errorf("foldLocked (no deltas) = %q, want base %q", got, `{"base":true}`)
	}
}

// TestSQLite_foldLocked_MultipleDeltas_AppliesInOrder — fold returns
// the forward-applied accumulation of delta payloads in seq order.
func TestSQLite_foldLocked_MultipleDeltas_AppliesInOrder(t *testing.T) {
	fx := newSQLiteFixture(t)
	fx.store.applyFunc = setUnionApplyFunc
	ref := spi.ModelRef{EntityName: "E", ModelVersion: "1"}
	fx.SaveModel(t, ref, []byte{})
	// Insert three delta rows directly (bypassing ExtendSchema to isolate the fold test).
	for i, d := range []string{`"d01"`, `"d02"`, `"d03"`} {
		if _, err := fx.store.db.ExecContext(fx.ctx,
			`INSERT INTO model_schema_extensions (tenant_id, model_name, model_version, seq, kind, payload)
			 VALUES (?, ?, ?, ?, 'delta', ?)`,
			string(fx.tenantID), ref.EntityName, ref.ModelVersion, i+1, []byte(d)); err != nil {
			t.Fatalf("insert delta %d: %v", i, err)
		}
	}

	got, err := foldViaTx(t, fx, ref, []byte{})
	if err != nil {
		t.Fatalf("foldLocked: %v", err)
	}
	expected, _ := setUnionApplyFunc([]byte{}, []spi.SchemaDelta{spi.SchemaDelta(`"d01"`)})
	expected, _ = setUnionApplyFunc(expected, []spi.SchemaDelta{spi.SchemaDelta(`"d02"`)})
	expected, _ = setUnionApplyFunc(expected, []spi.SchemaDelta{spi.SchemaDelta(`"d03"`)})
	if !bytes.Equal(got, expected) {
		t.Errorf("foldLocked = %q, want %q", got, expected)
	}
}

// TestSQLite_ExtendSchema_SavepointAtConfigInterval — B-I4 for sqlite.
// With interval=10, the 10th ExtendSchema MUST write a savepoint row.
// (The savepoint is written at nextSeq+1 so its seq = 11 in that run.)
func TestSQLite_ExtendSchema_SavepointAtConfigInterval(t *testing.T) {
	fx := newSQLiteFixtureWithInterval(t, 10)
	fx.store.applyFunc = setUnionApplyFunc
	ref := spi.ModelRef{EntityName: "E", ModelVersion: "1"}
	fx.SaveModel(t, ref, []byte{})
	if err := fx.store.Lock(fx.ctx, ref); err != nil {
		t.Fatalf("Lock: %v", err)
	}

	for i := 0; i < 10; i++ {
		if err := fx.store.ExtendSchema(fx.ctx, ref, spi.SchemaDelta(fmt.Sprintf(`"d%d"`, i))); err != nil {
			t.Fatalf("ExtendSchema %d: %v", i, err)
		}
	}

	seq, err := savepointSeqViaTx(t, fx, ref)
	if err != nil {
		t.Fatalf("lastSavepointSeq: %v", err)
	}
	if seq == 0 {
		t.Errorf("savepoint seq after 10 deltas with interval=10 = 0, want nonzero")
	}
}

// TestSQLite_TestFactory_DefaultSavepointInterval — the test factory runs
// with the shipped default interval (64), not savepoints off: the 63rd
// delta writes no savepoint, the 64th writes one.
func TestSQLite_TestFactory_DefaultSavepointInterval(t *testing.T) {
	fx := newSQLiteFixture(t)
	fx.store.applyFunc = setUnionApplyFunc
	ref := spi.ModelRef{EntityName: "E", ModelVersion: "1"}
	fx.SaveModel(t, ref, []byte{})
	if err := fx.store.Lock(fx.ctx, ref); err != nil {
		t.Fatalf("Lock: %v", err)
	}
	extend := func(i int) {
		t.Helper()
		if err := fx.store.ExtendSchema(fx.ctx, ref, spi.SchemaDelta(fmt.Sprintf(`"d%02d"`, i))); err != nil {
			t.Fatalf("ExtendSchema %d: %v", i, err)
		}
	}
	for i := 1; i < 64; i++ {
		extend(i)
	}
	if seq, err := savepointSeqViaTx(t, fx, ref); err != nil || seq != 0 {
		t.Fatalf("savepoint seq after 63 deltas = %d (err %v), want 0", seq, err)
	}
	extend(64)
	if seq, err := savepointSeqViaTx(t, fx, ref); err != nil || seq == 0 {
		t.Fatalf("savepoint seq after 64 deltas = %d (err %v), want nonzero", seq, err)
	}
}

// TestSQLite_ExtendSchema_UpgradeFromPreBDeployment — asserts that
// an existing sqlite database with a populated models.doc.schema and
// an empty model_schema_extensions table opens cleanly under the
// new log-based plugin. The first Get returns the base schema
// verbatim (zero deltas ⇒ identity fold).
//
// The "pre-B deployment" scenario is simulated by calling Save() (which
// writes a proper models row via the canonical marshal path) and NOT
// calling ExtendSchema, so the model_schema_extensions table stays empty.
func TestSQLite_ExtendSchema_UpgradeFromPreBDeployment(t *testing.T) {
	fx := newSQLiteFixture(t)
	ref := spi.ModelRef{EntityName: "E", ModelVersion: "1"}
	baseSchema := []byte(`{"type":"object","pre_b":true}`)

	// Seed a pre-B model row: populated doc.schema, no extension rows.
	fx.SaveModel(t, ref, baseSchema)

	// Confirm the extension log is empty for this (tenant, model, version).
	var count int
	if err := fx.store.db.QueryRowContext(fx.ctx,
		`SELECT COUNT(*) FROM model_schema_extensions
		 WHERE tenant_id = ? AND model_name = ? AND model_version = ?`,
		string(fx.tenantID), ref.EntityName, ref.ModelVersion).Scan(&count); err != nil {
		t.Fatalf("count extension rows: %v", err)
	}
	if count != 0 {
		t.Fatalf("pre-B fixture has %d extension rows, want 0", count)
	}

	// Get returns the base schema verbatim (zero deltas ⇒ identity fold).
	desc, err := fx.store.Get(fx.ctx, ref)
	if err != nil {
		t.Fatalf("Get on pre-B model: %v", err)
	}
	if !bytes.Equal(desc.Schema, baseSchema) {
		t.Errorf("pre-B Get returned %q, want verbatim %q", desc.Schema, baseSchema)
	}
}

// TestSQLite_ExtendSchema_UnlockDoesNotWriteSavepoint — §5.3 asymmetry.
// Unlock changes state back to UNLOCKED but MUST NOT write a savepoint.
// Unlock folds the extension log into the stored schema and clears it,
// so no savepoint survives it.
func TestSQLite_ExtendSchema_UnlockDoesNotWriteSavepoint(t *testing.T) {
	fx := newSQLiteFixture(t)
	fx.store.applyFunc = setUnionApplyFunc
	ref := spi.ModelRef{EntityName: "E", ModelVersion: "1"}
	fx.SaveModel(t, ref, []byte(`["base"]`))
	if err := fx.store.Lock(fx.ctx, ref); err != nil {
		t.Fatalf("Lock: %v", err)
	}
	beforeUnlock, err := savepointSeqViaTx(t, fx, ref)
	if err != nil {
		t.Fatalf("lastSavepointSeq pre-unlock: %v", err)
	}
	if err := fx.store.Unlock(fx.ctx, ref); err != nil {
		t.Fatalf("Unlock: %v", err)
	}
	afterUnlock, err := savepointSeqViaTx(t, fx, ref)
	if err != nil {
		t.Fatalf("lastSavepointSeq post-unlock: %v", err)
	}
	if beforeUnlock != afterUnlock {
		t.Errorf("Unlock mutated lastSavepointSeq: before=%d after=%d", beforeUnlock, afterUnlock)
	}
}

// TestSQLite_ExtendSchema_ContextCancellation_ReturnsCtxErr — §4.2.
// Cancelling ctx before ExtendSchema must return context.Canceled
// (wrapped) since the retry loop checks ctx.Err() at the top of each attempt.
func TestSQLite_ExtendSchema_ContextCancellation_ReturnsCtxErr(t *testing.T) {
	fx := newSQLiteFixture(t)
	fx.store.applyFunc = setUnionApplyFunc
	ref := spi.ModelRef{EntityName: "E", ModelVersion: "1"}
	fx.SaveModel(t, ref, []byte(`[]`))
	if err := fx.store.Lock(fx.ctx, ref); err != nil {
		t.Fatalf("Lock: %v", err)
	}

	ctx, cancel := context.WithCancel(fx.ctx)
	cancel()

	err := fx.store.ExtendSchema(ctx, ref, spi.SchemaDelta(`"d1"`))
	if err == nil {
		t.Fatal("ExtendSchema with cancelled ctx must fail")
	}
	if errors.Is(err, spi.ErrRetryExhausted) {
		t.Errorf("cancelled ctx → ErrRetryExhausted (want ctx.Err()); err = %v", err)
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled ctx → non-context error; err = %v", err)
	}
}

// TestSQLite_ExtendSchema_RejectionLeavesNoPersistedTrace — B-I6 for sqlite.
// At the savepoint-interval boundary, where the extension would also write a
// savepoint, a delta that applyFunc rejects (e.g. a simulated ChangeLevel
// violation) leaves neither a delta row nor a savepoint row.
//
// Construction uses newSQLiteFixtureWithInterval(t, 10) directly so that
// the 10th ExtendSchema call reaches the boundary.
func TestSQLite_ExtendSchema_RejectionLeavesNoPersistedTrace(t *testing.T) {
	fx := newSQLiteFixtureWithInterval(t, 10)
	// The fixture already installed fixtureApplyFunc on the factory;
	// SetApplyFunc would panic on re-entry. Override the store-level
	// applyFunc directly — the same pattern Tasks 15-19 use.
	fx.store.applyFunc = func(base []byte, deltas []spi.SchemaDelta) ([]byte, error) {
		for _, delta := range deltas {
			if bytes.Contains([]byte(delta), []byte("RESTRICT")) {
				return nil, fmt.Errorf("simulated ChangeLevel violation")
			}
		}
		return setUnionApplyFunc(base, deltas)
	}
	ref := spi.ModelRef{EntityName: "E", ModelVersion: "1"}
	fx.SaveModel(t, ref, []byte{})
	if err := fx.store.Lock(fx.ctx, ref); err != nil {
		t.Fatalf("Lock: %v", err)
	}

	// Warm the log to one row below the interval boundary (9 deltas at
	// interval=10). The 10th ExtendSchema reaches the boundary.
	for i := 0; i < 9; i++ {
		if err := fx.store.ExtendSchema(fx.ctx, ref, spi.SchemaDelta(fmt.Sprintf(`"d%d"`, i))); err != nil {
			t.Fatalf("warmup ExtendSchema #%d: %v", i, err)
		}
	}

	var preDeltaCount, preSPCount int
	if err := fx.store.db.QueryRowContext(fx.ctx,
		`SELECT COUNT(*) FROM model_schema_extensions
		 WHERE tenant_id = ? AND model_name = ? AND model_version = ? AND kind = 'delta'`,
		string(fx.tenantID), ref.EntityName, ref.ModelVersion).Scan(&preDeltaCount); err != nil {
		t.Fatalf("pre delta count: %v", err)
	}
	if err := fx.store.db.QueryRowContext(fx.ctx,
		`SELECT COUNT(*) FROM model_schema_extensions
		 WHERE tenant_id = ? AND model_name = ? AND model_version = ? AND kind = 'savepoint'`,
		string(fx.tenantID), ref.EntityName, ref.ModelVersion).Scan(&preSPCount); err != nil {
		t.Fatalf("pre savepoint count: %v", err)
	}

	// Attempt the 10th delta — applyFunc rejects the RESTRICT delta.
	err := fx.store.ExtendSchema(fx.ctx, ref, spi.SchemaDelta(`"RESTRICT-d9"`))
	if err == nil {
		t.Fatal("ExtendSchema with rejecting applyFunc on savepoint path must fail")
	}

	var postDeltaCount, postSPCount int
	if err := fx.store.db.QueryRowContext(fx.ctx,
		`SELECT COUNT(*) FROM model_schema_extensions
		 WHERE tenant_id = ? AND model_name = ? AND model_version = ? AND kind = 'delta'`,
		string(fx.tenantID), ref.EntityName, ref.ModelVersion).Scan(&postDeltaCount); err != nil {
		t.Fatalf("post delta count: %v", err)
	}
	if err := fx.store.db.QueryRowContext(fx.ctx,
		`SELECT COUNT(*) FROM model_schema_extensions
		 WHERE tenant_id = ? AND model_name = ? AND model_version = ? AND kind = 'savepoint'`,
		string(fx.tenantID), ref.EntityName, ref.ModelVersion).Scan(&postSPCount); err != nil {
		t.Fatalf("post savepoint count: %v", err)
	}

	if postDeltaCount != preDeltaCount {
		t.Errorf("rejected extension added a delta row: before=%d after=%d", preDeltaCount, postDeltaCount)
	}
	if postSPCount != preSPCount {
		t.Errorf("rejected extension added a savepoint row: before=%d after=%d", preSPCount, postSPCount)
	}
}

// TestSQLite_ExtendSchema_FoldAcrossSavepointBoundary_ByteIdentical — B-I2
// local. Run the same 30-delta sequence twice: once with interval=10
// (multiple savepoints fire mid-log) and once with interval=1_000_000 (no
// savepoint ever fires, pure delta replay). The savepoint row is an
// internal optimisation; the fold observed by callers must be
// byte-identical regardless.
//
// Each fixture constructs its own sqlite file under t.TempDir(), so the
// two runs are fully isolated — no schema-reset gymnastics required.
func TestSQLite_ExtendSchema_FoldAcrossSavepointBoundary_ByteIdentical(t *testing.T) {
	a := newSQLiteFixtureWithInterval(t, 10)
	a.store.applyFunc = setUnionApplyFunc
	refA := spi.ModelRef{EntityName: "A", ModelVersion: "1"}
	a.SaveModel(t, refA, []byte{})
	if err := a.store.Lock(a.ctx, refA); err != nil {
		t.Fatalf("Lock: %v", err)
	}
	for i := 0; i < 30; i++ {
		if err := a.store.ExtendSchema(a.ctx, refA, spi.SchemaDelta(fmt.Sprintf(`"d%03d"`, i))); err != nil {
			t.Fatalf("with-savepoints ExtendSchema #%d: %v", i, err)
		}
	}
	got, err := a.store.Get(a.ctx, refA)
	if err != nil {
		t.Fatalf("with-savepoints Get: %v", err)
	}

	b := newSQLiteFixtureWithInterval(t, 1_000_000)
	b.store.applyFunc = setUnionApplyFunc
	refB := spi.ModelRef{EntityName: "B", ModelVersion: "1"}
	b.SaveModel(t, refB, []byte{})
	if err := b.store.Lock(b.ctx, refB); err != nil {
		t.Fatalf("Lock: %v", err)
	}
	for i := 0; i < 30; i++ {
		if err := b.store.ExtendSchema(b.ctx, refB, spi.SchemaDelta(fmt.Sprintf(`"d%03d"`, i))); err != nil {
			t.Fatalf("no-savepoints ExtendSchema #%d: %v", i, err)
		}
	}
	want, err := b.store.Get(b.ctx, refB)
	if err != nil {
		t.Fatalf("no-savepoints Get: %v", err)
	}

	// setUnionApplyFunc emits a canonical sorted JSON array, so both folds
	// should already be byte-identical. Route through sortNewlineTokens
	// for defensive parity with the postgres counterpart (B-I2) — any
	// future apply-func swap that produces unsorted output still passes
	// this test as long as the multiset of tokens matches.
	gotSorted := sortNewlineTokens(got.Schema)
	wantSorted := sortNewlineTokens(want.Schema)
	if !bytes.Equal(gotSorted, wantSorted) {
		t.Errorf("fold with savepoints != fold without\n  with savepoints:    %q\n  without savepoints: %q", gotSorted, wantSorted)
	}
}

// TestSQLite_ExtendSchema_RejectsDeltaThatApplyRefuses — persist-time
// Apply check. A delta that applyFunc rejects must not reach
// model_schema_extensions. Exercises the FIRST-delta code path, away from
// the savepoint boundary. This matches the memory plugin, which runs
// applyFunc inline on every ExtendSchema.
func TestSQLite_ExtendSchema_RejectsDeltaThatApplyRefuses(t *testing.T) {
	fx := newSQLiteFixture(t)
	fx.store.applyFunc = func(base []byte, deltas []spi.SchemaDelta) ([]byte, error) {
		for _, delta := range deltas {
			if bytes.Contains([]byte(delta), []byte("POISON")) {
				return nil, fmt.Errorf("applyFunc rejected delta: poison")
			}
		}
		return setUnionApplyFunc(base, deltas)
	}
	ref := spi.ModelRef{EntityName: "E", ModelVersion: "1"}
	fx.SaveModel(t, ref, []byte{})
	if err := fx.store.Lock(fx.ctx, ref); err != nil {
		t.Fatalf("Lock: %v", err)
	}

	var preCount int
	if err := fx.store.db.QueryRowContext(fx.ctx,
		`SELECT COUNT(*) FROM model_schema_extensions
		 WHERE tenant_id = ? AND model_name = ? AND model_version = ?`,
		string(fx.tenantID), ref.EntityName, ref.ModelVersion).Scan(&preCount); err != nil {
		t.Fatalf("pre count: %v", err)
	}

	err := fx.store.ExtendSchema(fx.ctx, ref, spi.SchemaDelta(`"POISON-d1"`))
	if err == nil {
		t.Fatal("ExtendSchema with rejecting applyFunc must fail")
	}

	var postCount int
	if err := fx.store.db.QueryRowContext(fx.ctx,
		`SELECT COUNT(*) FROM model_schema_extensions
		 WHERE tenant_id = ? AND model_name = ? AND model_version = ?`,
		string(fx.tenantID), ref.EntityName, ref.ModelVersion).Scan(&postCount); err != nil {
		t.Fatalf("post count: %v", err)
	}
	if postCount != preCount {
		t.Errorf("rejected delta persisted: before=%d after=%d", preCount, postCount)
	}
}

// TestSQLite_ExtendSchema_ConcurrentSavepointCrossing_NoLostDelta —
// cross-backend regression for the schema-fold delta-loss corruption
// path fixed in the postgres plugin (see plugins/postgres
// TestExtendSchema_ConcurrentSelfWrap_SavepointCrossing_NoLostDelta):
// a savepoint fold that runs without seeing a concurrent writer's
// lower-seq delta excludes that delta from every future fold. SQLite's
// single-writer transaction model makes each ExtendSchema attempt
// (fold + append in one internal tx, retried wholesale on BUSY)
// naturally immune; this test locks that property in. N concurrent
// writers each add a distinct token with a savepoint interval small
// enough that many folds fire mid-storm; every committed delta must
// survive into the final fold.
func TestSQLite_ExtendSchema_ConcurrentSavepointCrossing_NoLostDelta(t *testing.T) {
	const (
		n        = 24
		interval = 3
	)
	fx := newSQLiteFixtureWithInterval(t, interval)
	fx.store.applyFunc = setUnionApplyFunc
	ref := spi.ModelRef{EntityName: "E", ModelVersion: "1"}
	fx.SaveModel(t, ref, []byte{})
	if err := fx.store.Lock(fx.ctx, ref); err != nil {
		t.Fatalf("Lock: %v", err)
	}

	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		i := i
		go func() {
			defer wg.Done()
			delta := spi.SchemaDelta(fmt.Sprintf(`"d%02d"`, i))
			if err := fx.store.ExtendSchema(fx.ctx, ref, delta); err != nil {
				t.Errorf("ExtendSchema #%d: %v", i, err)
			}
		}()
	}
	wg.Wait()

	got, err := fx.store.Get(fx.ctx, ref)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	for i := 0; i < n; i++ {
		token := fmt.Sprintf("d%02d", i)
		if !bytes.Contains(got.Schema, []byte(token)) {
			t.Errorf("committed delta %q lost from fold; folded schema = %s", token, got.Schema)
		}
	}
}

// lockedUnionFixture returns a fixture whose model ref is saved with base
// `[]`, set-union apply, and LOCKED (ExtendSchema refuses an unlocked model).
func lockedUnionFixture(t *testing.T) (*sqliteFixture, spi.ModelRef) {
	t.Helper()
	fx := newSQLiteFixture(t)
	fx.store.applyFunc = setUnionApplyFunc
	ref := spi.ModelRef{EntityName: "E", ModelVersion: "1"}
	fx.SaveModel(t, ref, []byte(`[]`))
	if err := fx.store.Lock(fx.ctx, ref); err != nil {
		t.Fatalf("Lock: %v", err)
	}
	return fx, ref
}

func countExtensionRows(t *testing.T, fx *sqliteFixture, ref spi.ModelRef, kind string) int {
	t.Helper()
	q := `SELECT COUNT(*) FROM model_schema_extensions
		 WHERE tenant_id = ? AND model_name = ? AND model_version = ?`
	args := []any{string(fx.tenantID), ref.EntityName, ref.ModelVersion}
	if kind != "" {
		q += ` AND kind = ?`
		args = append(args, kind)
	}
	var n int
	if err := fx.store.db.QueryRowContext(fx.ctx, q, args...).Scan(&n); err != nil {
		t.Fatalf("count extension rows: %v", err)
	}
	return n
}

// An extension that changes nothing writes no row (design 4.3).
func TestSQLite_ExtendSchema_NoOpWritesNoRow(t *testing.T) {
	fx, ref := lockedUnionFixture(t)
	for i := 0; i < 2; i++ {
		if err := fx.store.ExtendSchema(fx.ctx, ref, spi.SchemaDelta(`"d1"`)); err != nil {
			t.Fatalf("ExtendSchema #%d: %v", i, err)
		}
	}
	if n := countExtensionRows(t, fx, ref, "delta"); n != 1 {
		t.Errorf("delta rows after extending the same delta twice = %d, want 1", n)
	}
}

// SQLITE_BUSY is retried until the context ends: no retry budget (design 4.13).
func TestSQLite_ExtendSchema_BusyRetriedUntilContextEnds(t *testing.T) {
	fx, ref := lockedUnionFixture(t)
	if _, err := fx.factory.db.ExecContext(context.Background(), "PRAGMA busy_timeout = 10"); err != nil {
		t.Fatalf("override busy_timeout: %v", err)
	}
	dbPath := fx.factory.cfg.Path

	hold := func() (*sql.Tx, func()) {
		holderDB, err := sql.Open("sqlite3", fmt.Sprintf("file:%s?_txlock=immediate&_pragma=busy_timeout(10)", dbPath))
		if err != nil {
			t.Fatalf("open holder DB: %v", err)
		}
		tx, err := holderDB.BeginTx(context.Background(), nil)
		if err != nil {
			t.Fatalf("holder BeginTx: %v", err)
		}
		return tx, func() { _ = tx.Rollback(); _ = holderDB.Close() }
	}

	// Lock held for far longer than the old 8-attempt budget (~80ms at a
	// 10ms busy_timeout); released after 600ms: ExtendSchema must succeed.
	_, release := hold()
	go func() { time.Sleep(600 * time.Millisecond); release() }()
	ctx, cancel := context.WithTimeout(fx.ctx, 20*time.Second)
	defer cancel()
	if err := fx.store.ExtendSchema(ctx, ref, spi.SchemaDelta(`"d1"`)); err != nil {
		t.Fatalf("ExtendSchema across a transient lock: %v", err)
	}

	// Lock held past the context deadline: the context error, never a budget error.
	_, release2 := hold()
	defer release2()
	ctx2, cancel2 := context.WithTimeout(fx.ctx, 300*time.Millisecond)
	defer cancel2()
	err := fx.store.ExtendSchema(ctx2, ref, spi.SchemaDelta(`"d2"`))
	if err == nil {
		t.Fatal("ExtendSchema under a held lock with an expiring ctx: got nil")
	}
	if errors.Is(err, spi.ErrRetryExhausted) {
		t.Errorf("got ErrRetryExhausted, want a context error: %v", err)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want errors.Is(_, context.DeadlineExceeded)", err)
	}
	// Contention is not a client-retryable conflict (design 3.4).
	if errors.Is(err, spi.ErrConflict) {
		t.Errorf("err = %v, must not satisfy errors.Is(_, spi.ErrConflict)", err)
	}
}

// Lock is the state update only; it writes no savepoint (design 4.14).
func TestSQLite_Lock_WritesNoExtensionRow(t *testing.T) {
	fx := newSQLiteFixture(t)
	fx.store.applyFunc = setUnionApplyFunc
	ref := spi.ModelRef{EntityName: "E", ModelVersion: "1"}
	fx.SaveModel(t, ref, []byte(`["base"]`))
	// A pending delta on an UNLOCKED model (written around the state machine)
	// is what made Lock write a savepoint.
	if _, err := fx.store.db.ExecContext(fx.ctx,
		`INSERT INTO model_schema_extensions (tenant_id, model_name, model_version, seq, kind, payload)
		 VALUES (?, ?, ?, 1, 'delta', ?)`,
		string(fx.tenantID), ref.EntityName, ref.ModelVersion, []byte(`"d1"`)); err != nil {
		t.Fatalf("seed delta: %v", err)
	}
	if err := fx.store.Lock(fx.ctx, ref); err != nil {
		t.Fatalf("Lock: %v", err)
	}
	if n := countExtensionRows(t, fx, ref, "savepoint"); n != 0 {
		t.Errorf("savepoint rows after Lock = %d, want 0", n)
	}
	if n := countExtensionRows(t, fx, ref, ""); n != 1 {
		t.Errorf("extension rows after Lock = %d, want the 1 seeded delta", n)
	}
}

// Get reads the doc and folds the log in one read transaction: against
// extend/Unlock/Lock cycles it never returns the base doc with the log
// already cleared (design 4.8). Tokens are added in order d0,d1,...; a
// goroutine's successive Gets must never lose a token it already saw.
func TestSQLite_Get_OneReadTransaction_NoTornReadWithUnlock(t *testing.T) {
	fx, ref := lockedUnionFixture(t)
	const cycles, readers = 60, 6

	stop := make(chan struct{})
	var stopOnce sync.Once
	stopReaders := func() { stopOnce.Do(func() { close(stop) }) }
	defer stopReaders()
	var wg sync.WaitGroup
	errc := make(chan error, readers+1)
	for r := 0; r < readers; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			seen := 0
			for {
				select {
				case <-stop:
					return
				default:
				}
				d, err := fx.store.Get(fx.ctx, ref)
				if err != nil {
					errc <- fmt.Errorf("Get: %w", err)
					return
				}
				var toks []string
				if err := json.Unmarshal(d.Schema, &toks); err != nil {
					errc <- fmt.Errorf("decode %q: %w", d.Schema, err)
					return
				}
				if len(toks) < seen {
					errc <- fmt.Errorf("torn read: saw %d tokens, then %d (%q)", seen, len(toks), d.Schema)
					return
				}
				seen = len(toks)
			}
		}()
	}
	for i := 0; i < cycles; i++ {
		if err := fx.store.ExtendSchema(fx.ctx, ref, spi.SchemaDelta(fmt.Sprintf(`"d%03d"`, i))); err != nil {
			t.Fatalf("ExtendSchema %d: %v", i, err)
		}
		if err := fx.store.Unlock(fx.ctx, ref); err != nil {
			t.Fatalf("Unlock %d: %v", i, err)
		}
		if err := fx.store.Lock(fx.ctx, ref); err != nil {
			t.Fatalf("Lock %d: %v", i, err)
		}
	}
	stopReaders()
	wg.Wait()
	close(errc)
	for err := range errc {
		t.Error(err)
	}
}

// foldViaTx folds ref's log over base inside a read transaction, as Get does.
func foldViaTx(t *testing.T, fx *sqliteFixture, ref spi.ModelRef, base []byte) ([]byte, error) {
	t.Helper()
	tx, err := fx.store.readDB.BeginTx(fx.ctx, nil)
	if err != nil {
		t.Fatalf("begin read tx: %v", err)
	}
	defer func() { _ = tx.Rollback() }()
	return fx.store.foldLockedInTx(fx.ctx, tx, ref, base)
}

func savepointSeqViaTx(t *testing.T, fx *sqliteFixture, ref spi.ModelRef) (int64, error) {
	t.Helper()
	tx, err := fx.store.readDB.BeginTx(fx.ctx, nil)
	if err != nil {
		t.Fatalf("begin read tx: %v", err)
	}
	defer func() { _ = tx.Rollback() }()
	return fx.store.lastSavepointSeqInTx(fx.ctx, tx, ref)
}
