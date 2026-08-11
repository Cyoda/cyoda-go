package postgres

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
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
// token (e.g. `"d00"`), and fold = sorted union.
//
// The persistence layer stores delta and savepoint payloads as JSONB, so
// both deltas and folded results must be valid JSON — hence the choice
// of JSON array over a newline-joined byte blob. With this apply, any
// interleaving/permutation of deltas yields the same fold, so any
// divergence observed by the tests is attributable to the persistence
// layer.
func setUnionApplyFunc(base []byte, delta spi.SchemaDelta) ([]byte, error) {
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
	var tok string
	if err := json.Unmarshal([]byte(delta), &tok); err != nil {
		return nil, fmt.Errorf("setUnionApplyFunc: decode delta %q: %w", delta, err)
	}
	m[tok] = struct{}{}
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
// need access to unexported methods (lastSavepointSeq, etc.) and to the
// concrete *modelStore value. Construction mirrors setupModelExtTest in
// the external test file but exposes the concrete types.
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
	dbURL := os.Getenv("CYODA_TEST_DB_URL")
	if dbURL == "" {
		t.Skip("CYODA_TEST_DB_URL not set — skipping PostgreSQL test")
	}
	poolCfg, err := pgxpool.ParseConfig(dbURL)
	if err != nil {
		t.Fatalf("parse pool config: %v", err)
	}
	poolCfg.MaxConns = 5
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

	factory := NewStoreFactory(pool)
	factory.SetApplyFunc(fixtureApplyFunc())

	tenantID := spi.TenantID("t1")
	uc := &spi.UserContext{
		UserID:   "test-user",
		UserName: "Test User",
		Tenant:   spi.Tenant{ID: tenantID, Name: string(tenantID)},
		Roles:    []string{"USER"},
	}
	ctx := spi.WithUserContext(context.Background(), uc)

	ms, err := factory.ModelStore(ctx)
	if err != nil {
		t.Fatalf("ModelStore: %v", err)
	}
	store, ok := ms.(*modelStore)
	if !ok {
		t.Fatalf("ModelStore did not return *modelStore; got %T", ms)
	}
	return &pgFixture{
		factory:  factory,
		pool:     pool,
		db:       pool,
		store:    store,
		ctx:      ctx,
		tenantID: tenantID,
		cfg:      store.cfg,
	}
}

// newPGFixtureWithInterval constructs a fixture like newPGFixture but
// with cfg.SchemaSavepointInterval overridden at factory-construction
// time — used by B-I2 to compare interval=64 vs interval=1_000_000.
func newPGFixtureWithInterval(t *testing.T, interval int) *pgFixture {
	t.Helper()
	dbURL := os.Getenv("CYODA_TEST_DB_URL")
	if dbURL == "" {
		t.Skip("CYODA_TEST_DB_URL not set — skipping PostgreSQL test")
	}
	poolCfg, err := pgxpool.ParseConfig(dbURL)
	if err != nil {
		t.Fatalf("parse pool config: %v", err)
	}
	poolCfg.MaxConns = 5
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
	factory := newStoreFactoryWithConfig(pool, cfg)
	factory.SetApplyFunc(fixtureApplyFunc())

	tenantID := spi.TenantID("t1")
	uc := &spi.UserContext{
		UserID:   "test-user",
		UserName: "Test User",
		Tenant:   spi.Tenant{ID: tenantID, Name: string(tenantID)},
		Roles:    []string{"USER"},
	}
	ctx := spi.WithUserContext(context.Background(), uc)

	ms, err := factory.ModelStore(ctx)
	if err != nil {
		t.Fatalf("ModelStore: %v", err)
	}
	store, ok := ms.(*modelStore)
	if !ok {
		t.Fatalf("ModelStore did not return *modelStore; got %T", ms)
	}
	return &pgFixture{
		factory:  factory,
		pool:     pool,
		db:       pool,
		store:    store,
		ctx:      ctx,
		tenantID: tenantID,
		cfg:      cfg,
	}
}

// reopenWithInterval rebuilds the modelStore on the same database with
// cfg.SchemaSavepointInterval overridden — simulates operator reconfig.
func (f *pgFixture) reopenWithInterval(t *testing.T, interval int) {
	t.Helper()
	newCfg := f.cfg
	newCfg.SchemaSavepointInterval = interval
	f.cfg = newCfg
	f.factory = newStoreFactoryWithConfig(f.pool, newCfg)
	f.factory.SetApplyFunc(fixtureApplyFunc())
	ms, err := f.factory.ModelStore(f.ctx)
	if err != nil {
		t.Fatalf("reopen ModelStore: %v", err)
	}
	store, ok := ms.(*modelStore)
	if !ok {
		t.Fatalf("reopen: ModelStore did not return *modelStore; got %T", ms)
	}
	f.store = store
}

// SaveModel seeds the base model row for ref with the given base schema.
func (f *pgFixture) SaveModel(t *testing.T, ref spi.ModelRef, base []byte) {
	t.Helper()
	desc := &spi.ModelDescriptor{
		Ref:         ref,
		State:       spi.ModelUnlocked,
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
	return func(base []byte, delta spi.SchemaDelta) ([]byte, error) {
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
		applied = append(applied, json.RawMessage(delta))
		encoded, err := json.Marshal(applied)
		if err != nil {
			return nil, err
		}
		m["$$applied"] = encoded
		return json.Marshal(m)
	}
}

// TestLastSavepointSeq_NoSavepoint — new helper function
// lastSavepointSeq(ctx, ref) (int64, error) returns the seq of
// the most-recent savepoint row for ref, or 0 if none exists.
// Task 10 refactor uses this to drive the savepoint trigger.
func TestLastSavepointSeq_NoSavepoint(t *testing.T) {
	fx := newPGFixture(t)
	ref := spi.ModelRef{EntityName: "E", ModelVersion: "1"}
	fx.SaveModel(t, ref, []byte(`{"base":true}`))

	seq, err := fx.store.lastSavepointSeq(fx.ctx, ref)
	if err != nil {
		t.Fatalf("lastSavepointSeq: %v", err)
	}
	if seq != 0 {
		t.Errorf("lastSavepointSeq on empty log = %d, want 0", seq)
	}
}

// TestExtendSchema_SavepointTriggerRespectsIntervalChange — B-I4.
// Start with interval 64, commit past the first savepoint. Change
// interval to 128, commit more deltas. The next savepoint only fires
// once (newSeq - lastSavepointSeq) >= 128 — i.e., 128 deltas past the
// most recent savepoint, not at an arbitrary global-seq multiple.
//
// NOTE: seq values are not equal to delta-call counts because the
// shared BIGSERIAL is also consumed by savepoint rows themselves.
// Assertions here are framed in terms of "did a new savepoint land or
// not" rather than exact seq arithmetic, but we still cross-check the
// last-savepoint seq against a direct query.
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
	lastSPBefore, err := fx.store.lastSavepointSeq(fx.ctx, ref)
	if err != nil {
		t.Fatalf("lastSavepointSeq: %v", err)
	}
	if lastSPBefore == 0 {
		t.Fatalf("expected a savepoint row after 64 deltas; lastSavepointSeq = 0")
	}

	// Reconfigure to interval=128. Commit 127 more deltas.
	// Under the new "since last savepoint" rule, no new savepoint fires
	// until (newSeq - lastSPBefore) >= 128. A delta-call doesn't increment
	// newSeq by 1 reliably (the savepoint row above consumed one seq), but
	// after 64 deltas + 1 savepoint, the next delta's newSeq is ~66. We
	// need newSeq >= lastSPBefore + 128 to see a new savepoint. 127 more
	// deltas cannot reach that distance: the max achievable newSeq is
	// (lastSPBefore + 127 deltas) which is lastSPBefore + 127 < lastSPBefore + 128.
	fx.reopenWithInterval(t, 128)
	for i := 0; i < 127; i++ {
		extend(fmt.Sprintf("d64-%d", i))
	}
	if got := spCount(); got != 1 {
		t.Errorf("interval=128, 127 deltas since last SP: savepoint count = %d, want still 1", got)
	}

	// One more delta — crosses the interval boundary.
	// After 128 deltas since the last savepoint, newSeq - lastSPBefore >= 128
	// is satisfied (exactly =128 after accounting for how many extra seqs
	// intervening savepoints consumed — here none since no new SP has
	// fired yet post-reconfig).
	extend("d128-trigger")
	if got := spCount(); got != 2 {
		t.Errorf("after 128 deltas since last SP with interval=128: savepoint count = %d, want 2", got)
	}
	lastSPAfter, err := fx.store.lastSavepointSeq(fx.ctx, ref)
	if err != nil {
		t.Fatalf("lastSavepointSeq: %v", err)
	}
	if lastSPAfter <= lastSPBefore {
		t.Errorf("expected lastSavepointSeq to advance: before=%d after=%d", lastSPBefore, lastSPAfter)
	}
	// The new savepoint must satisfy the trigger rule: it fires when the
	// triggering delta's newSeq is at least lastSPBefore + interval(128).
	// The SP row itself takes the next seq after that delta, so:
	//   lastSPAfter == triggering_delta_seq + 1, where triggering_delta_seq >= lastSPBefore + 128.
	if lastSPAfter < lastSPBefore+128 {
		t.Errorf("new savepoint seq %d < lastSPBefore(%d)+128; trigger fired too early", lastSPAfter, lastSPBefore)
	}
}

func TestLastSavepointSeq_ReturnsMostRecent(t *testing.T) {
	fx := newPGFixture(t)
	ref := spi.ModelRef{EntityName: "E", ModelVersion: "1"}
	fx.SaveModel(t, ref, []byte(`{"base":true}`))
	// Drive enough deltas to trigger two savepoints (default interval=64;
	// savepoint rows themselves consume a seq from the shared BIGSERIAL,
	// so hardcoding expected seq numbers is fragile — compare against a
	// direct query of the most-recent savepoint seq instead).
	for i := 0; i < 128; i++ {
		if err := fx.store.ExtendSchema(fx.ctx, ref, spi.SchemaDelta(fmt.Sprintf(`[{"kind":"broaden_type","path":"d%d","payload":["NULL"]}]`, i))); err != nil {
			t.Fatalf("ExtendSchema %d: %v", i, err)
		}
	}

	var want int64
	if err := fx.db.QueryRow(fx.ctx, `
		SELECT seq FROM model_schema_extensions
		WHERE tenant_id=$1 AND model_name=$2 AND model_version=$3 AND kind='savepoint'
		ORDER BY seq DESC LIMIT 1`,
		string(fx.tenantID), ref.EntityName, ref.ModelVersion).Scan(&want); err != nil {
		t.Fatalf("direct SP query: %v", err)
	}
	if want == 0 {
		t.Fatalf("expected at least one savepoint row after 128 deltas (interval=64); saw none")
	}

	got, err := fx.store.lastSavepointSeq(fx.ctx, ref)
	if err != nil {
		t.Fatalf("lastSavepointSeq: %v", err)
	}
	if got != want {
		t.Errorf("lastSavepointSeq = %d, want %d (most-recent SP row per direct query)", got, want)
	}

	// Sanity: two savepoint rows after 128 deltas at interval=64.
	var spCount int
	if err := fx.db.QueryRow(fx.ctx, `
		SELECT COUNT(*) FROM model_schema_extensions
		WHERE tenant_id=$1 AND model_name=$2 AND model_version=$3 AND kind='savepoint'`,
		string(fx.tenantID), ref.EntityName, ref.ModelVersion).Scan(&spCount); err != nil {
		t.Fatalf("SP count: %v", err)
	}
	if spCount != 2 {
		t.Errorf("savepoint count after 128 deltas (interval=64) = %d, want 2", spCount)
	}
}

// TestExtendSchema_SaveOnLock — B-I3. Lock transition writes a
// savepoint row atomically with the lock state change.
func TestExtendSchema_SaveOnLock(t *testing.T) {
	fx := newPGFixture(t)
	ref := spi.ModelRef{EntityName: "E", ModelVersion: "1"}
	fx.SaveModel(t, ref, []byte(`{"base":true}`))

	// Pre-lock: no savepoint.
	before, err := fx.store.lastSavepointSeq(fx.ctx, ref)
	if err != nil {
		t.Fatalf("lastSavepointSeq pre-lock: %v", err)
	}
	if before != 0 {
		t.Fatalf("pre-lock lastSavepointSeq = %d, want 0", before)
	}

	// Lock.
	if err := fx.store.Lock(fx.ctx, ref); err != nil {
		t.Fatalf("Lock: %v", err)
	}

	// Post-lock: a savepoint exists.
	after, err := fx.store.lastSavepointSeq(fx.ctx, ref)
	if err != nil {
		t.Fatalf("lastSavepointSeq post-lock: %v", err)
	}
	if after == 0 {
		t.Error("post-lock lastSavepointSeq = 0, want nonzero (savepoint must be written on lock)")
	}

	// Savepoint payload equals the folded schema (which, with zero deltas,
	// equals the base schema verbatim). JSONB storage normalises whitespace
	// and may reorder keys, so compare semantically.
	var payload []byte
	if err := fx.db.QueryRow(fx.ctx,
		`SELECT payload FROM model_schema_extensions
		 WHERE tenant_id=$1 AND model_name=$2 AND model_version=$3 AND kind='savepoint'
		 ORDER BY seq DESC LIMIT 1`,
		string(fx.tenantID), ref.EntityName, ref.ModelVersion).Scan(&payload); err != nil {
		t.Fatalf("read savepoint payload: %v", err)
	}
	var got, want any
	if err := json.Unmarshal(payload, &got); err != nil {
		t.Fatalf("unmarshal savepoint payload %q: %v", payload, err)
	}
	if err := json.Unmarshal([]byte(`{"base":true}`), &want); err != nil {
		t.Fatalf("unmarshal want: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("savepoint payload = %q, want base schema %q (semantically)", payload, `{"base":true}`)
	}
	_ = bytes.Equal // keep bytes import live for future assertions

	// Lock state flipped.
	locked, err := fx.store.IsLocked(fx.ctx, ref)
	if err != nil {
		t.Fatalf("IsLocked: %v", err)
	}
	if !locked {
		t.Error("expected model to be locked after Lock()")
	}
}

// TestExtendSchema_UnlockDoesNotWriteSavepoint — §5.2 asymmetry.
// Unlock must not write a savepoint. The test is slightly awkward
// because Unlock clears the extension log entirely (operator-contract
// drain); we assert that after the lock→unlock sequence the savepoint
// log is no different from the "was never locked" state — i.e. empty.
func TestExtendSchema_UnlockDoesNotWriteSavepoint(t *testing.T) {
	fx := newPGFixture(t)
	ref := spi.ModelRef{EntityName: "E", ModelVersion: "1"}
	fx.SaveModel(t, ref, []byte(`{"base":true}`))

	if err := fx.store.Lock(fx.ctx, ref); err != nil {
		t.Fatalf("Lock: %v", err)
	}
	seqAfterLock, err := fx.store.lastSavepointSeq(fx.ctx, ref)
	if err != nil {
		t.Fatalf("lastSavepointSeq after lock: %v", err)
	}
	if seqAfterLock == 0 {
		t.Fatal("precondition: expected a savepoint row after Lock")
	}

	if err := fx.store.Unlock(fx.ctx, ref); err != nil {
		t.Fatalf("Unlock: %v", err)
	}
	// Unlock drains the extension log (including savepoints) per the
	// operator contract — so lastSavepointSeq drops to 0. The assertion
	// is that Unlock did NOT *add* a new savepoint: the post-unlock
	// savepoint seq is strictly not greater than the pre-unlock seq.
	seqAfterUnlock, err := fx.store.lastSavepointSeq(fx.ctx, ref)
	if err != nil {
		t.Fatalf("lastSavepointSeq after unlock: %v", err)
	}
	if seqAfterUnlock > seqAfterLock {
		t.Errorf("Unlock wrote a new savepoint: before=%d after=%d (save-on-unlock is deliberately omitted)",
			seqAfterLock, seqAfterUnlock)
	}
}

// TestExtendSchema_CommutativeAppend_ConvergesUnderConcurrency — B-I7.
// N goroutines extend the same model concurrently. These are self-wrap
// writers (no ambient transaction), which serialise by blocking on the
// per-(tenant, model) write-claim, so all are expected to commit without
// a conflict or retry. The set-union apply is associative,
// commutative, and idempotent — so the fold must equal the fold of any
// serial replay regardless of interleaving. The test also asserts that
// exactly N delta rows exist: no writer was silently dropped.
//
// The fixture wires its default applyFunc at construction and the factory's
// SetApplyFunc panics on re-entry, so we swap the store-level applyFunc
// directly. This is the same effective wiring the production path
// resolves: modelStore holds the applyFunc that foldLocked invokes.
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
		expected, _ = setUnionApplyFunc(expected, spi.SchemaDelta(d))
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

// TestExtendSchema_ContextCancellation_ReturnsCtxErr — §4.2 contract.
// Postgres's ExtendSchema has no transparent retry loop: a concurrent
// same-model writer surfaces spi.ErrConflict to the caller (first
// committer wins), so there is no retry-budget path
// that could turn a ctx cancellation into ErrRetryExhausted. Assert
// that the pgx-native cancellation behavior surfaces the context error
// — the same observable contract every plugin must honour.
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
// savepoint-interval boundary, ExtendSchema is forced to fold the log
// and write a savepoint row. If that fold fails (applyFunc rejects the
// delta — e.g., simulated ChangeLevel violation), the entire operation
// must roll back: neither the delta row that triggered the fold nor
// the would-be savepoint row may persist.
//
// Without an ambient caller-supplied tx, atomicity requires ExtendSchema
// to self-wrap — otherwise the initial delta INSERT auto-commits before
// the fold is attempted.
func TestExtendSchema_RejectionLeavesNoSavepointOrOp(t *testing.T) {
	fx := newPGFixture(t)
	fx.store.applyFunc = func(base []byte, delta spi.SchemaDelta) ([]byte, error) {
		if bytes.Contains([]byte(delta), []byte("RESTRICT")) {
			return nil, fmt.Errorf("simulated ChangeLevel violation")
		}
		return setUnionApplyFunc(base, delta)
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

	// Attempt the 64th delta — the RESTRICT delta triggers the savepoint path where applyFunc is called.
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
// Exercises the FIRST-delta code path (no savepoint fold involved): the
// rejection must come from the apply-check, not from the savepoint-fold
// check. Matches memory's apply-inline behavior so all three plugins give
// the same timing/contract: reject at ExtendSchema, not at later fold-on-Get.
func TestExtendSchema_RejectsDeltaThatApplyRefuses(t *testing.T) {
	fx := newPGFixture(t)
	// ApplyFunc that rejects any delta containing the sentinel.
	fx.store.applyFunc = func(base []byte, delta spi.SchemaDelta) ([]byte, error) {
		if bytes.Contains([]byte(delta), []byte("POISON")) {
			return nil, fmt.Errorf("applyFunc rejected delta: poison")
		}
		return setUnionApplyFunc(base, delta)
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
