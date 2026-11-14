package postgres

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// setUnionApplyFunc is an associative-commutative-idempotent apply used
// by B-I7 and B-I2 tests. The "schema" representation is a JSON array of
// unique string tokens, an ExtendSchema delta is a JSON-encoded string
// token (e.g. `"d00"`), and fold = sorted union. An empty delta re-encodes
// the base, as the production apply does.
//
// The persistence layer stores delta and savepoint payloads as JSONB, so
// both deltas and folded results must be valid JSON — hence the choice
// of JSON array over a newline-joined byte blob. With this apply, any
// interleaving/permutation of deltas yields the same fold, so any
// divergence observed by the tests is attributable to the persistence
// layer.
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

// pgFixture is the internal-package fixture for modelStore tests that
// need access to unexported methods and to the concrete *modelStore value.
// Construction mirrors setupModelExtTest in the external test file but
// exposes the concrete types.
type pgFixture struct {
	factory  *StoreFactory
	pool     *pgxpool.Pool
	db       *pgxpool.Pool // alias of pool for test-readability per plan
	store    *modelStore
	ctx      context.Context
	tenantID spi.TenantID
	cfg      config
}

func newPGFixture(t *testing.T) *pgFixture {
	t.Helper()
	return newPGFixtureSized(t, defaultStoreConfig().SchemaSavepointInterval, 5)
}

// newPGFixtureWithInterval constructs a fixture like newPGFixture but
// with cfg.SchemaSavepointInterval overridden at factory-construction
// time.
func newPGFixtureWithInterval(t *testing.T, interval int) *pgFixture {
	t.Helper()
	return newPGFixtureSized(t, interval, 5)
}

// newPGFixtureSized constructs a fixture with the given savepoint interval
// and main-pool size, for tests that hold many entity transactions open.
func newPGFixtureSized(t *testing.T, interval int, maxConns int32) *pgFixture {
	t.Helper()
	dbURL := testDBURL(t)
	poolCfg, err := pgxpool.ParseConfig(dbURL)
	if err != nil {
		t.Fatalf("parse pool config: %v", err)
	}
	poolCfg.MaxConns = maxConns
	poolCfg.MinConns = 0
	poolCfg.MaxConnIdleTime = 60 * time.Second
	poolCfg.HealthCheckPeriod = 24 * time.Hour

	pool, err := pgxpool.NewWithConfig(context.Background(), poolCfg)
	if err != nil {
		t.Fatalf("create pool: %v", err)
	}
	t.Cleanup(pool.Close)

	if err := dropSchema(pool); err != nil {
		t.Fatalf("reset schema: %v", err)
	}
	if err := Migrate(pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	t.Cleanup(func() { _ = dropSchema(pool) })

	cfg := defaultStoreConfig()
	cfg.SchemaSavepointInterval = interval
	f := &pgFixture{
		pool:     pool,
		db:       pool,
		tenantID: spi.TenantID("t1"),
		cfg:      cfg,
	}
	f.ctx = spi.WithUserContext(context.Background(), &spi.UserContext{
		UserID:   "test-user",
		UserName: "Test User",
		Tenant:   spi.Tenant{ID: f.tenantID, Name: string(f.tenantID)},
		Roles:    []string{"USER"},
	})
	f.open(t)
	return f
}

// open builds the factory and the model store from f.cfg on f.pool.
func (f *pgFixture) open(t *testing.T) {
	t.Helper()
	f.factory = newStoreFactoryWithConfig(f.pool, f.cfg)
	f.factory.SetApplyFunc(fixtureApplyFunc())
	t.Cleanup(f.factory.closeModelPool)
	ms, err := f.factory.ModelStore(f.ctx)
	if err != nil {
		t.Fatalf("ModelStore: %v", err)
	}
	store, ok := ms.(*modelStore)
	if !ok {
		t.Fatalf("ModelStore did not return *modelStore; got %T", ms)
	}
	f.store = store
}

// reopenWithInterval rebuilds the modelStore on the same database with
// cfg.SchemaSavepointInterval overridden — simulates operator reconfig.
func (f *pgFixture) reopenWithInterval(t *testing.T, interval int) {
	t.Helper()
	f.cfg.SchemaSavepointInterval = interval
	f.open(t)
}

// txManager wires a transaction manager onto the fixture's factory.
func (f *pgFixture) txManager(t *testing.T) spi.TransactionManager {
	t.Helper()
	f.factory.InitTransactionManager(newTestUUIDGenerator())
	tm, err := f.factory.TransactionManager(f.ctx)
	if err != nil {
		t.Fatalf("TransactionManager: %v", err)
	}
	return tm
}

// pinSnapshot runs a statement in the entity transaction on txCtx, so its
// REPEATABLE READ snapshot is taken now: nothing committed afterwards is
// visible to it.
func (f *pgFixture) pinSnapshot(t *testing.T, txCtx context.Context) {
	t.Helper()
	if _, err := f.factory.resolveRaw(txCtx).Exec(txCtx, "SELECT 1"); err != nil {
		t.Fatalf("pin snapshot: %v", err)
	}
}

// countRows counts ref's extension rows of the given kind ("" = any).
func (f *pgFixture) countRows(t *testing.T, ref spi.ModelRef, kind string) int {
	t.Helper()
	var n int
	if err := f.db.QueryRow(context.Background(), `
		SELECT COUNT(*) FROM model_schema_extensions
		WHERE tenant_id = $1 AND model_name = $2 AND model_version = $3
		  AND ($4 = '' OR kind = $4)`,
		string(f.tenantID), ref.EntityName, ref.ModelVersion, kind).Scan(&n); err != nil {
		t.Fatalf("count extension rows: %v", err)
	}
	return n
}

// lastSavepointSeq returns the seq of ref's most recent savepoint row, or 0.
func (f *pgFixture) lastSavepointSeq(t *testing.T, ref spi.ModelRef) int64 {
	t.Helper()
	var seq int64
	if err := f.db.QueryRow(context.Background(), `
		SELECT COALESCE(MAX(seq), 0) FROM model_schema_extensions
		WHERE tenant_id = $1 AND model_name = $2 AND model_version = $3 AND kind = 'savepoint'`,
		string(f.tenantID), ref.EntityName, ref.ModelVersion).Scan(&seq); err != nil {
		t.Fatalf("last savepoint seq: %v", err)
	}
	return seq
}

// SaveModel seeds a LOCKED model row for ref with the given base schema:
// only a locked model accepts extensions.
func (f *pgFixture) SaveModel(t *testing.T, ref spi.ModelRef, base []byte) {
	t.Helper()
	f.saveModel(t, ref, base, spi.ModelLocked)
}

// saveModel seeds the model row for ref in the given state.
func (f *pgFixture) saveModel(t *testing.T, ref spi.ModelRef, base []byte, state spi.ModelState) {
	t.Helper()
	desc := &spi.ModelDescriptor{
		Ref:         ref,
		State:       state,
		ChangeLevel: spi.ChangeLevelStructural,
		Schema:      base,
		UpdateDate:  time.Now().UTC().Truncate(time.Millisecond),
	}
	if err := f.store.Save(f.ctx, desc); err != nil {
		t.Fatalf("Save: %v", err)
	}
}

// fixtureApplyFunc is the same recording apply function used by the
// external test file; duplicated here because it's needed for the
// internal-package fixture too.
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

// TestExtendSchema_SavepointTriggerRespectsIntervalChange — B-I4.
// Start with interval 64, commit past the first savepoint. Change
// interval to 128, commit more deltas. The next savepoint only fires
// once 128 of the model's deltas follow the most recent savepoint.
//
// NOTE: seq values are not equal to delta-call counts because the
// shared BIGSERIAL is also consumed by savepoint rows themselves.
// Assertions here are framed in terms of "did a new savepoint land or
// not" rather than exact seq arithmetic.
func TestExtendSchema_SavepointTriggerRespectsIntervalChange(t *testing.T) {
	fx := newPGFixture(t)
	ref := spi.ModelRef{EntityName: "E", ModelVersion: "1"}
	fx.SaveModel(t, ref, []byte(`{"base":true}`))

	extend := func(tag string) {
		t.Helper()
		if err := fx.store.ExtendSchema(fx.ctx, ref,
			spi.SchemaDelta(fmt.Sprintf(`[{"kind":"broaden_type","path":"%s","payload":["NULL"]}]`, tag))); err != nil {
			t.Fatalf("ExtendSchema %s: %v", tag, err)
		}
	}
	spCount := func() int {
		t.Helper()
		var n int
		if err := fx.db.QueryRow(fx.ctx, `
			SELECT COUNT(*) FROM model_schema_extensions
			WHERE tenant_id=$1 AND model_name=$2 AND model_version=$3 AND kind='savepoint'`,
			string(fx.tenantID), ref.EntityName, ref.ModelVersion).Scan(&n); err != nil {
			t.Fatalf("SP count: %v", err)
		}
		return n
	}

	// Interval = 64 (default). 64 deltas → exactly one savepoint fires at
	// newSeq=64 (lastSP was 0, 64-0 >= 64).
	for i := 0; i < 64; i++ {
		extend(fmt.Sprintf("d%d", i))
	}
	if got := spCount(); got != 1 {
		t.Fatalf("after 64 deltas with interval=64, savepoint count = %d, want 1", got)
	}
	lastSPBefore := fx.lastSavepointSeq(t, ref)
	if lastSPBefore == 0 {
		t.Fatalf("expected a savepoint row after 64 deltas; lastSavepointSeq = 0")
	}

	// Reconfigure to interval=128. Commit 127 more deltas: fewer than 128
	// deltas follow the savepoint, so no new savepoint fires.
	fx.reopenWithInterval(t, 128)
	for i := 0; i < 127; i++ {
		extend(fmt.Sprintf("d64-%d", i))
	}
	if got := spCount(); got != 1 {
		t.Errorf("interval=128, 127 deltas since last SP: savepoint count = %d, want still 1", got)
	}

	// One more delta — the 128th since the last savepoint.
	extend("d128-trigger")
	if got := spCount(); got != 2 {
		t.Errorf("after 128 deltas since last SP with interval=128: savepoint count = %d, want 2", got)
	}
	lastSPAfter := fx.lastSavepointSeq(t, ref)
	if lastSPAfter <= lastSPBefore {
		t.Errorf("expected lastSavepointSeq to advance: before=%d after=%d", lastSPBefore, lastSPAfter)
	}
	// 128 deltas, each taking one seq, lie between the two savepoints.
	if lastSPAfter < lastSPBefore+128 {
		t.Errorf("new savepoint seq %d < lastSPBefore(%d)+128; trigger fired too early", lastSPAfter, lastSPBefore)
	}
}

// TestExtendSchema_CommutativeAppend_ConvergesUnderConcurrency — B-I7.
// N goroutines extend the same model concurrently. Each extension is a
// short transaction that waits on the model's row lock, so all commit
// without a conflict or retry. The set-union apply is associative,
// commutative, and idempotent — so the fold must equal the fold of any
// serial replay regardless of interleaving. The test also asserts that
// exactly N delta rows exist: no writer was silently dropped.
//
// The fixture wires its default applyFunc at construction and the factory's
// SetApplyFunc panics on re-entry, so we swap the store-level applyFunc
// directly. This is the same effective wiring the production path
// resolves: modelStore holds the applyFunc that ExtendSchema and Get use.
func TestExtendSchema_CommutativeAppend_ConvergesUnderConcurrency(t *testing.T) {
	const N = 8
	fx := newPGFixture(t)
	fx.store.applyFunc = setUnionApplyFunc
	ref := spi.ModelRef{EntityName: "E", ModelVersion: "1"}
	fx.SaveModel(t, ref, []byte{})

	deltas := make([]string, N)
	for i := 0; i < N; i++ {
		deltas[i] = fmt.Sprintf(`"d%02d"`, i)
	}

	var wg sync.WaitGroup
	wg.Add(N)
	for i := 0; i < N; i++ {
		i := i
		go func() {
			defer wg.Done()
			err := fx.store.ExtendSchema(fx.ctx, ref, spi.SchemaDelta(deltas[i]))
			if err != nil {
				t.Errorf("ExtendSchema #%d: %v", i, err)
			}
		}()
	}
	wg.Wait()

	expected := []byte{}
	for _, d := range deltas {
		expected, _ = setUnionApplyFunc(expected, []spi.SchemaDelta{spi.SchemaDelta(d)})
	}
	expectedSorted := sortNewlineTokens(expected)

	got, err := fx.store.Get(fx.ctx, ref)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	gotSorted := sortNewlineTokens(got.Schema)
	if !bytes.Equal(gotSorted, expectedSorted) {
		t.Errorf("concurrent fold != serial-replay fold\n  got:  %q\n  want: %q", gotSorted, expectedSorted)
	}

	var count int
	if err := fx.db.QueryRow(fx.ctx,
		`SELECT COUNT(*) FROM model_schema_extensions
		 WHERE tenant_id=$1 AND model_name=$2 AND model_version=$3 AND kind='delta'`,
		string(fx.tenantID), ref.EntityName, ref.ModelVersion).Scan(&count); err != nil {
		t.Fatalf("delta count query: %v", err)
	}
	if count != N {
		t.Errorf("delta row count = %d, want %d (all writers must commit)", count, N)
	}
}

// TestExtendSchema_ContextCancellation_ReturnsCtxErr — ExtendSchema has no
// retry budget that could turn a cancellation into ErrRetryExhausted: a
// cancelled ctx surfaces the context error, the same observable contract
// every plugin honours.
func TestExtendSchema_ContextCancellation_ReturnsCtxErr(t *testing.T) {
	fx := newPGFixture(t)
	ref := spi.ModelRef{EntityName: "E", ModelVersion: "1"}
	fx.SaveModel(t, ref, []byte(`{"base":true}`))

	ctx, cancel := context.WithCancel(fx.ctx)
	cancel() // cancel before calling

	err := fx.store.ExtendSchema(ctx, ref, spi.SchemaDelta(`"d1"`))
	if err == nil {
		t.Fatal("ExtendSchema with cancelled ctx must fail")
	}
	if errors.Is(err, spi.ErrRetryExhausted) {
		t.Errorf("cancelled ctx → ErrRetryExhausted (want ctx.Err()); err = %v", err)
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled ctx → non-ctx error; err = %v", err)
	}
}

// TestExtendSchema_FoldAcrossSavepointBoundary_ByteIdentical — B-I2 local.
// Run the same 150-delta sequence twice: once with interval=64 (multiple
// savepoints fire mid-log) and once with interval=1_000_000 (no savepoint
// ever fires, pure delta replay). The savepoint row is an internal
// optimisation; the fold observed by callers must be byte-identical
// regardless.
//
// The two sub-runs share one PostgreSQL database. They run sequentially
// and `newPGFixtureWithInterval` drops + re-migrates the schema on
// construction, so the interval=64 fold is read before the second
// fixture is built.
func TestExtendSchema_FoldAcrossSavepointBoundary_ByteIdentical(t *testing.T) {
	a := newPGFixture(t) // default interval 64
	a.store.applyFunc = setUnionApplyFunc
	refA := spi.ModelRef{EntityName: "A", ModelVersion: "1"}
	a.SaveModel(t, refA, []byte{})
	for i := 0; i < 150; i++ {
		_ = a.store.ExtendSchema(a.ctx, refA, spi.SchemaDelta(fmt.Sprintf(`"d%03d"`, i)))
	}
	got, _ := a.store.Get(a.ctx, refA)
	gotSorted := sortNewlineTokens(got.Schema)

	b := newPGFixtureWithInterval(t, 1_000_000)
	b.store.applyFunc = setUnionApplyFunc
	refB := spi.ModelRef{EntityName: "B", ModelVersion: "1"}
	b.SaveModel(t, refB, []byte{})
	for i := 0; i < 150; i++ {
		_ = b.store.ExtendSchema(b.ctx, refB, spi.SchemaDelta(fmt.Sprintf(`"d%03d"`, i)))
	}
	want, _ := b.store.Get(b.ctx, refB)
	wantSorted := sortNewlineTokens(want.Schema)

	if !bytes.Equal(gotSorted, wantSorted) {
		t.Errorf("fold with savepoints != fold without\n  with savepoints:    %q\n  without savepoints: %q", gotSorted, wantSorted)
	}
}

// TestExtendSchema_RejectionLeavesNoSavepointOrOp — B-I6. At the
// savepoint-interval boundary, a delta that applyFunc rejects (e.g. a
// simulated ChangeLevel violation) leaves neither a delta row nor a
// savepoint row.
func TestExtendSchema_RejectionLeavesNoSavepointOrOp(t *testing.T) {
	fx := newPGFixture(t)
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

	// Commit 63 valid deltas — just below the 64 interval.
	for i := 0; i < 63; i++ {
		_ = fx.store.ExtendSchema(fx.ctx, ref, spi.SchemaDelta(fmt.Sprintf(`"d%d"`, i)))
	}

	var preDeltaCount, preSPCount int
	if err := fx.db.QueryRow(fx.ctx, `SELECT COUNT(*) FROM model_schema_extensions WHERE kind='delta'`).Scan(&preDeltaCount); err != nil {
		t.Fatalf("pre delta count: %v", err)
	}
	if err := fx.db.QueryRow(fx.ctx, `SELECT COUNT(*) FROM model_schema_extensions WHERE kind='savepoint'`).Scan(&preSPCount); err != nil {
		t.Fatalf("pre savepoint count: %v", err)
	}

	// Attempt the 64th delta — applyFunc rejects the RESTRICT delta at the boundary.
	err := fx.store.ExtendSchema(fx.ctx, ref, spi.SchemaDelta(`"RESTRICT-d63"`))
	if err == nil {
		t.Fatal("ExtendSchema with rejecting applyFunc on savepoint path must fail")
	}

	var postDeltaCount, postSPCount int
	if err := fx.db.QueryRow(fx.ctx, `SELECT COUNT(*) FROM model_schema_extensions WHERE kind='delta'`).Scan(&postDeltaCount); err != nil {
		t.Fatalf("post delta count: %v", err)
	}
	if err := fx.db.QueryRow(fx.ctx, `SELECT COUNT(*) FROM model_schema_extensions WHERE kind='savepoint'`).Scan(&postSPCount); err != nil {
		t.Fatalf("post savepoint count: %v", err)
	}

	if postDeltaCount != preDeltaCount {
		t.Errorf("rejected extension added a delta row: before=%d after=%d", preDeltaCount, postDeltaCount)
	}
	if postSPCount != preSPCount {
		t.Errorf("rejected extension added a savepoint row: before=%d after=%d", preSPCount, postSPCount)
	}
}

// TestExtendSchema_RejectsDeltaThatApplyRefuses — persist-time Apply check.
// A delta that applyFunc rejects must not reach model_schema_extensions.
// Exercises the FIRST-delta code path, away from the savepoint boundary.
// Matches memory's apply-inline behavior so all three plugins give
// the same timing/contract: reject at ExtendSchema, not at later fold-on-Get.
func TestExtendSchema_RejectsDeltaThatApplyRefuses(t *testing.T) {
	fx := newPGFixture(t)
	// ApplyFunc that rejects any delta containing the sentinel.
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

	var preCount int
	if err := fx.db.QueryRow(fx.ctx,
		`SELECT COUNT(*) FROM model_schema_extensions
		 WHERE tenant_id = $1 AND model_name = $2 AND model_version = $3`,
		string(fx.tenantID), ref.EntityName, ref.ModelVersion).Scan(&preCount); err != nil {
		t.Fatalf("pre count: %v", err)
	}

	err := fx.store.ExtendSchema(fx.ctx, ref, spi.SchemaDelta(`"POISON-d1"`))
	if err == nil {
		t.Fatal("ExtendSchema with rejecting applyFunc must fail")
	}

	var postCount int
	if err := fx.db.QueryRow(fx.ctx,
		`SELECT COUNT(*) FROM model_schema_extensions
		 WHERE tenant_id = $1 AND model_name = $2 AND model_version = $3`,
		string(fx.tenantID), ref.EntityName, ref.ModelVersion).Scan(&postCount); err != nil {
		t.Fatalf("post count: %v", err)
	}
	if postCount != preCount {
		t.Errorf("rejected delta persisted: before=%d after=%d", preCount, postCount)
	}
}

// TestExtendSchema_NoOpWritesNoRow — a delta the schema already holds is a
// successful no-op that writes nothing.
func TestExtendSchema_NoOpWritesNoRow(t *testing.T) {
	fx := newPGFixture(t)
	fx.store.applyFunc = setUnionApplyFunc
	ref := spi.ModelRef{EntityName: "E", ModelVersion: "1"}
	fx.SaveModel(t, ref, []byte(`[]`))

	for i := 0; i < 2; i++ {
		if err := fx.store.ExtendSchema(fx.ctx, ref, spi.SchemaDelta(`"f1"`)); err != nil {
			t.Fatalf("ExtendSchema #%d: %v", i, err)
		}
	}
	if n := fx.countRows(t, ref, "delta"); n != 1 {
		t.Errorf("delta rows after the same delta twice = %d, want 1", n)
	}
}

// TestExtendSchema_NilApplyFuncRefused — without an apply function the
// backend cannot check, de-duplicate or fold a delta, so it refuses to
// extend and writes nothing.
func TestExtendSchema_NilApplyFuncRefused(t *testing.T) {
	fx := newPGFixture(t)
	ref := spi.ModelRef{EntityName: "E", ModelVersion: "1"}
	fx.SaveModel(t, ref, []byte(`{"base":true}`))
	fx.store.applyFunc = nil

	if err := fx.store.ExtendSchema(fx.ctx, ref, spi.SchemaDelta(`"d0"`)); err == nil {
		t.Fatal("ExtendSchema without an apply function succeeded, want an error")
	}
	if n := fx.countRows(t, ref, ""); n != 0 {
		t.Errorf("extension rows = %d, want 0", n)
	}
}

// TestExtendSchema_NotLocked_ErrModelNotLocked — an UNLOCKED model refuses
// an extension with spi.ErrModelNotLocked and writes nothing.
func TestExtendSchema_NotLocked_ErrModelNotLocked(t *testing.T) {
	fx := newPGFixture(t)
	fx.store.applyFunc = setUnionApplyFunc
	ref := spi.ModelRef{EntityName: "E", ModelVersion: "1"}
	fx.saveModel(t, ref, []byte(`[]`), spi.ModelUnlocked)

	err := fx.store.ExtendSchema(fx.ctx, ref, spi.SchemaDelta(`"f1"`))
	if !errors.Is(err, spi.ErrModelNotLocked) {
		t.Fatalf("ExtendSchema on an unlocked model = %v, want spi.ErrModelNotLocked", err)
	}
	if n := fx.countRows(t, ref, ""); n != 0 {
		t.Errorf("extension rows = %d, want 0", n)
	}
}

// TestExtendSchema_IntervalCountsModelDeltas — the savepoint interval
// counts the model's own deltas since its latest savepoint, not distance in
// the sequence all models share.
func TestExtendSchema_IntervalCountsModelDeltas(t *testing.T) {
	fx := newPGFixtureWithInterval(t, 4)
	fx.store.applyFunc = setUnionApplyFunc
	refA := spi.ModelRef{EntityName: "A", ModelVersion: "1"}
	refB := spi.ModelRef{EntityName: "B", ModelVersion: "1"}
	fx.SaveModel(t, refA, []byte(`[]`))
	fx.SaveModel(t, refB, []byte(`[]`))
	extend := func(ref spi.ModelRef, tok string) {
		t.Helper()
		if err := fx.store.ExtendSchema(fx.ctx, ref, spi.SchemaDelta(`"`+tok+`"`)); err != nil {
			t.Fatalf("ExtendSchema %s %s: %v", ref.EntityName, tok, err)
		}
	}

	extend(refA, "a0")
	for i := 0; i < 10; i++ {
		extend(refB, fmt.Sprintf("b%d", i))
	}
	extend(refA, "a1")
	extend(refA, "a2")
	if n := fx.countRows(t, refA, "savepoint"); n != 0 {
		t.Fatalf("model A savepoints after 3 of its deltas (interval 4) = %d, want 0", n)
	}
	extend(refA, "a3")
	if n := fx.countRows(t, refA, "savepoint"); n != 1 {
		t.Errorf("model A savepoints after its 4th delta = %d, want 1", n)
	}
	if n := fx.countRows(t, refB, "savepoint"); n != 2 {
		t.Errorf("model B savepoints after 10 deltas (interval 4) = %d, want 2", n)
	}
}

// TestLock_WritesNoExtensionRow — Lock is the state change alone: it
// writes no savepoint (or any other extension row).
func TestLock_WritesNoExtensionRow(t *testing.T) {
	fx := newPGFixture(t)
	ref := spi.ModelRef{EntityName: "E", ModelVersion: "1"}
	fx.saveModel(t, ref, []byte(`{"base":true}`), spi.ModelUnlocked)

	if err := fx.store.Lock(fx.ctx, ref); err != nil {
		t.Fatalf("Lock: %v", err)
	}
	if n := fx.countRows(t, ref, ""); n != 0 {
		t.Errorf("extension rows after Lock = %d, want 0", n)
	}
	locked, err := fx.store.IsLocked(fx.ctx, ref)
	if err != nil {
		t.Fatalf("IsLocked: %v", err)
	}
	if !locked {
		t.Error("model not locked after Lock")
	}
}

// TestGet_OneStatement_NoTornReadWithUnlock — Get reads the doc and the log
// in one snapshot: against extend/Unlock/Lock cycles it never returns the
// base doc from before an Unlock with the log that Unlock cleared. Tokens
// are added in order; a reader's successive Gets never lose a token.
func TestGet_OneStatement_NoTornReadWithUnlock(t *testing.T) {
	fx := newPGFixture(t)
	fx.store.applyFunc = setUnionApplyFunc
	ref := spi.ModelRef{EntityName: "E", ModelVersion: "1"}
	fx.SaveModel(t, ref, []byte(`[]`))
	const cycles, readers = 60, 4

	stop := make(chan struct{})
	var stopOnce sync.Once
	stopReaders := func() { stopOnce.Do(func() { close(stop) }) }
	defer stopReaders()
	var wg sync.WaitGroup
	errc := make(chan error, readers)
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

// TestGet_IgnoresCallerTransaction — Get reads the latest committed model,
// not the snapshot of the entity transaction on ctx.
func TestGet_IgnoresCallerTransaction(t *testing.T) {
	fx := newPGFixture(t)
	fx.store.applyFunc = setUnionApplyFunc
	tm := fx.txManager(t)
	ref := spi.ModelRef{EntityName: "E", ModelVersion: "1"}
	fx.SaveModel(t, ref, []byte(`[]`))

	_, txCtx := beginGuarded(t, tm, fx.ctx)
	if _, err := fx.store.Get(txCtx, ref); err != nil {
		t.Fatalf("Get in tx: %v", err)
	}
	fx.pinSnapshot(t, txCtx)

	if err := fx.store.ExtendSchema(fx.ctx, ref, spi.SchemaDelta(`"late"`)); err != nil {
		t.Fatalf("ExtendSchema: %v", err)
	}
	got, err := fx.store.Get(txCtx, ref)
	if err != nil {
		t.Fatalf("Get in tx after the extension: %v", err)
	}
	if !bytes.Contains(got.Schema, []byte(`"late"`)) {
		t.Errorf("Get in a transaction whose snapshot predates the extension = %s, want it to include \"late\"", got.Schema)
	}
}

// nestedApplyFunc is a toy schema fold with nesting. A schema is a JSON
// object tree (an object node is a JSON object, a leaf is true); a delta
// {"parent":[...],"name":n,"children":[...]} adds n under the object at
// parent, as an object with the given leaf children when children is set,
// else as a leaf. A parent that does not exist is refused, as the real
// apply refuses an add_property under a missing parent. The deltas apply in
// order; an empty list re-encodes the base.
func nestedApplyFunc(base []byte, deltas []spi.SchemaDelta) ([]byte, error) {
	root := map[string]any{}
	if len(base) > 0 {
		if err := json.Unmarshal(base, &root); err != nil {
			return nil, fmt.Errorf("nestedApplyFunc: decode base %q: %w", base, err)
		}
	}
	for _, delta := range deltas {
		if len(delta) == 0 {
			continue
		}
		var d struct {
			Parent   []string `json:"parent"`
			Name     string   `json:"name"`
			Children []string `json:"children"`
		}
		if err := json.Unmarshal(delta, &d); err != nil {
			return nil, fmt.Errorf("nestedApplyFunc: decode delta %q: %w", delta, err)
		}
		node := root
		for _, p := range d.Parent {
			next, ok := node[p].(map[string]any)
			if !ok {
				return nil, fmt.Errorf("nestedApplyFunc: parent %v not found", d.Parent)
			}
			node = next
		}
		if d.Children == nil {
			node[d.Name] = true
		} else {
			obj := map[string]any{}
			for _, c := range d.Children {
				obj[c] = true
			}
			node[d.Name] = obj
		}
	}
	return json.Marshal(root)
}

// TestExtendSchema_NestedUnderParentAddedAfterSnapshot — an extension made
// from an entity transaction folds and checks against the latest committed
// schema, not the transaction's snapshot: a field under an object another
// extension added after the snapshot is accepted, and both survive.
func TestExtendSchema_NestedUnderParentAddedAfterSnapshot(t *testing.T) {
	fx := newPGFixture(t)
	fx.store.applyFunc = nestedApplyFunc
	tm := fx.txManager(t)
	ref := spi.ModelRef{EntityName: "E", ModelVersion: "1"}
	fx.SaveModel(t, ref, []byte(`{}`))

	_, txCtx := beginGuarded(t, tm, fx.ctx)
	fx.pinSnapshot(t, txCtx)

	if err := fx.store.ExtendSchema(fx.ctx, ref,
		spi.SchemaDelta(`{"parent":[],"name":"a","children":["x"]}`)); err != nil {
		t.Fatalf("ExtendSchema a{x}: %v", err)
	}
	if err := fx.store.ExtendSchema(txCtx, ref,
		spi.SchemaDelta(`{"parent":["a"],"name":"y"}`)); err != nil {
		t.Fatalf("ExtendSchema a.y from the entity transaction: %v", err)
	}

	got, err := fx.store.Get(fx.ctx, ref)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	var tree map[string]map[string]bool
	if err := json.Unmarshal(got.Schema, &tree); err != nil {
		t.Fatalf("decode %s: %v", got.Schema, err)
	}
	if !tree["a"]["x"] || !tree["a"]["y"] {
		t.Errorf("folded schema = %s, want a.x and a.y", got.Schema)
	}
}
