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

// withExtendMaxRetries is a test-only Option that overrides
// cfg.SchemaExtendMaxRetries on the factory. Production wiring reads
// this from CYODA_SCHEMA_EXTEND_MAX_RETRIES via parseConfig; tests use
// this option to squeeze the retry budget down to a handful of attempts
// so the exhaustion path surfaces deterministically under forced
// SQLITE_BUSY contention.
func withExtendMaxRetries(n int) Option {
	return func(f *StoreFactory) { f.cfg.SchemaExtendMaxRetries = n }
}

// setUnionApplyFunc is an associative-commutative-idempotent apply used
// by Sub-project B tests. The "schema" representation is a JSON array of
// unique string tokens, an ExtendSchema delta is a JSON-encoded string
// token (e.g. `"d00"`), and fold = sorted union.
//
// Copied verbatim from plugins/postgres/model_extensions_internal_test.go
// so the sqlite fold tests remain self-contained.
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

func newSQLiteFixture(t *testing.T) *sqliteFixture {
	return newSQLiteFixtureWithInterval(t, 0)
}

// newSQLiteFixtureWithInterval constructs a fixture like newSQLiteFixture
// but with cfg.SchemaSavepointInterval overridden at factory-construction
// time — used by B-I3/B-I4 tests that need a small interval to exercise
// the savepoint trigger deterministically. Passing interval=0 means "use
// the factory default" (64).
func newSQLiteFixtureWithInterval(t *testing.T, interval int) *sqliteFixture {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "test.db")
	var opts []Option
	if interval > 0 {
		opts = append(opts, withSavepointInterval(interval))
	}
	f, err := NewStoreFactoryForTest(context.Background(), dbPath, opts...)
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

// TestSQLite_foldLocked_NoDeltas_ReturnsBase — sanity check for the
// new fold path. If no extension rows exist, fold returns the base
// schema verbatim (applyFunc not required).
func TestSQLite_foldLocked_NoDeltas_ReturnsBase(t *testing.T) {
	fx := newSQLiteFixture(t)
	ref := spi.ModelRef{EntityName: "E", ModelVersion: "1"}
	fx.SaveModel(t, ref, []byte(`{"base":true}`))

	got, err := fx.store.foldLocked(fx.ctx, ref, []byte(`{"base":true}`))
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
			`INSERT INTO model_schema_extensions (tenant_id, model_name, model_version, seq, kind, payload, tx_id)
			 VALUES (?, ?, ?, ?, 'delta', ?, '')`,
			string(fx.tenantID), ref.EntityName, ref.ModelVersion, i+1, []byte(d)); err != nil {
			t.Fatalf("insert delta %d: %v", i, err)
		}
	}

	got, err := fx.store.foldLocked(fx.ctx, ref, []byte{})
	if err != nil {
		t.Fatalf("foldLocked: %v", err)
	}
	expected, _ := setUnionApplyFunc([]byte{}, spi.SchemaDelta(`"d01"`))
	expected, _ = setUnionApplyFunc(expected, spi.SchemaDelta(`"d02"`))
	expected, _ = setUnionApplyFunc(expected, spi.SchemaDelta(`"d03"`))
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

	for i := 0; i < 10; i++ {
		if err := fx.store.ExtendSchema(fx.ctx, ref, spi.SchemaDelta(fmt.Sprintf(`"d%d"`, i))); err != nil {
			t.Fatalf("ExtendSchema %d: %v", i, err)
		}
	}

	seq, err := fx.store.lastSavepointSeq(fx.ctx, ref)
	if err != nil {
		t.Fatalf("lastSavepointSeq: %v", err)
	}
	if seq == 0 {
		t.Errorf("savepoint seq after 10 deltas with interval=10 = 0, want nonzero")
	}
}

// TestSQLite_ExtendSchema_SaveOnLock — B-I3 for sqlite.
// Lock on a model with pending (unfolded) deltas MUST write a savepoint.
func TestSQLite_ExtendSchema_SaveOnLock(t *testing.T) {
	fx := newSQLiteFixture(t)
	fx.store.applyFunc = setUnionApplyFunc
	ref := spi.ModelRef{EntityName: "E", ModelVersion: "1"}
	fx.SaveModel(t, ref, []byte{})
	if err := fx.store.ExtendSchema(fx.ctx, ref, spi.SchemaDelta(`"d1"`)); err != nil {
		t.Fatalf("ExtendSchema: %v", err)
	}

	before, err := fx.store.lastSavepointSeq(fx.ctx, ref)
	if err != nil {
		t.Fatalf("lastSavepointSeq pre-lock: %v", err)
	}
	if before != 0 {
		t.Fatalf("pre-lock lastSavepointSeq = %d, want 0", before)
	}
	if err := fx.store.Lock(fx.ctx, ref); err != nil {
		t.Fatalf("Lock: %v", err)
	}
	after, err := fx.store.lastSavepointSeq(fx.ctx, ref)
	if err != nil {
		t.Fatalf("lastSavepointSeq post-lock: %v", err)
	}
	if after == 0 {
		t.Error("post-lock lastSavepointSeq = 0, want nonzero")
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
// The extension log is not drained on Unlock; subsequent Lock calls
// with no new deltas are correctly de-duped via (maxSeq == lastSP).
func TestSQLite_ExtendSchema_UnlockDoesNotWriteSavepoint(t *testing.T) {
	fx := newSQLiteFixture(t)
	fx.store.applyFunc = setUnionApplyFunc
	ref := spi.ModelRef{EntityName: "E", ModelVersion: "1"}
	fx.SaveModel(t, ref, []byte(`["base"]`))
	if err := fx.store.Lock(fx.ctx, ref); err != nil {
		t.Fatalf("Lock: %v", err)
	}
	beforeUnlock, err := fx.store.lastSavepointSeq(fx.ctx, ref)
	if err != nil {
		t.Fatalf("lastSavepointSeq pre-unlock: %v", err)
	}
	if err := fx.store.Unlock(fx.ctx, ref); err != nil {
		t.Fatalf("Unlock: %v", err)
	}
	afterUnlock, err := fx.store.lastSavepointSeq(fx.ctx, ref)
	if err != nil {
		t.Fatalf("lastSavepointSeq post-unlock: %v", err)
	}
	if beforeUnlock != afterUnlock {
		t.Errorf("Unlock mutated lastSavepointSeq: before=%d after=%d", beforeUnlock, afterUnlock)
	}
}

// TestSQLite_ExtendSchema_TransparentRetry_ConvergesUnderBusy — B-I7 for
// sqlite. N goroutines extending concurrently all commit within the retry
// budget. Depending on SQLite's busy_timeout absorbing contention, the
// retry wrapper may or may not fire; either way all N deltas must land.
func TestSQLite_ExtendSchema_TransparentRetry_ConvergesUnderBusy(t *testing.T) {
	const N = 8
	fx := newSQLiteFixture(t)
	// NOTE: fx.factory.SetApplyFunc would panic (fixture already installed
	// fixtureApplyFunc); override directly on the store instead.
	fx.store.applyFunc = setUnionApplyFunc
	ref := spi.ModelRef{EntityName: "E", ModelVersion: "1"}
	fx.SaveModel(t, ref, []byte(`[]`))

	var wg sync.WaitGroup
	errs := make([]error, N)
	wg.Add(N)
	for i := 0; i < N; i++ {
		i := i
		go func() {
			defer wg.Done()
			errs[i] = fx.store.ExtendSchema(fx.ctx, ref, spi.SchemaDelta(fmt.Sprintf(`"d%d"`, i)))
		}()
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("ExtendSchema #%d failed: %v", i, err)
		}
	}

	// All N deltas landed. Scope to this tenant + model for defensive parity
	// with the rest of the extension-table assertions in this file.
	var count int
	if err := fx.store.db.QueryRowContext(fx.ctx,
		`SELECT COUNT(*) FROM model_schema_extensions
		 WHERE tenant_id = ? AND model_name = ? AND model_version = ? AND kind = 'delta'`,
		string(fx.tenantID), ref.EntityName, ref.ModelVersion,
	).Scan(&count); err != nil {
		t.Fatalf("count delta rows: %v", err)
	}
	if count != N {
		t.Errorf("delta row count = %d, want %d", count, N)
	}
}

// TestSQLite_ExtendSchema_RetryExhaustion_ReturnsErrRetryExhausted — B-I7.
// Forces deterministic SQLITE_BUSY contention by holding an uncommitted
// write transaction on a second *sql.DB handle to the same database file,
// then invokes ExtendSchema with a tight retry budget and a tight
// busy_timeout. The retry loop MUST exhaust and return
// spi.ErrRetryExhausted wrapping the last spi.ErrConflict.
//
// This is the counterpart to TransparentRetry_ConvergesUnderBusy: that
// test proves the happy path (retries converge under the default 5s
// busy_timeout), this one proves the exhaustion path is actually wired
// — i.e. that the classifier + retry loop + ErrRetryExhausted wrap all
// work when contention genuinely overflows the budget.
//
// Implementation notes:
//   - The factory-level flock guards against a second StoreFactory on
//     the same file, but it does NOT prevent a raw *sql.DB handle from
//     opening the file. That's what we exploit here: a bare lock-holder
//     DB with its own connection pool, independent of fx.store.db.
//   - fx.store.db.SetMaxOpenConns(1) means the ExtendSchema attempt's
//     BeginTx grabs the sole pool connection and then tries to acquire
//     the write (RESERVED) lock inside SQLite — which the holder's
//     uncommitted IMMEDIATE tx is keeping. After busy_timeout, the
//     driver returns SQLITE_BUSY, which classifyError maps to
//     spi.ErrConflict, which the retry loop catches.
//   - busy_timeout=10ms keeps the whole test snappy; three attempts
//     bound total wait at ~30ms per test run before exhaustion.
func TestSQLite_ExtendSchema_RetryExhaustion_ReturnsErrRetryExhausted(t *testing.T) {
	const maxRetries = 3
	dbPath := filepath.Join(t.TempDir(), "test.db")

	f, err := NewStoreFactoryForTest(
		context.Background(),
		dbPath,
		withExtendMaxRetries(maxRetries),
	)
	if err != nil {
		t.Fatalf("NewStoreFactoryForTest: %v", err)
	}
	t.Cleanup(func() { _ = f.Close() })
	f.SetApplyFunc(fixtureApplyFunc())

	// Shrink busy_timeout on the shared connection so each retry
	// attempt fails fast under contention. SQLite PRAGMAs are
	// per-connection; with MaxOpenConns=1 this covers all writes.
	if _, err := f.db.ExecContext(context.Background(),
		"PRAGMA busy_timeout = 10"); err != nil {
		t.Fatalf("override busy_timeout: %v", err)
	}

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
	store := ms.(*modelStore)
	store.applyFunc = setUnionApplyFunc
	ref := spi.ModelRef{EntityName: "E", ModelVersion: "1"}

	// Seed a base model row using the main factory BEFORE opening the
	// lock-holder — saves us from interleaving writes against the held
	// write lock during setup.
	desc := &spi.ModelDescriptor{
		Ref:         ref,
		State:       spi.ModelUnlocked,
		ChangeLevel: spi.ChangeLevelStructural,
		Schema:      []byte(`[]`),
		UpdateDate:  time.Now().UTC().Truncate(time.Millisecond),
	}
	if err := store.Save(ctx, desc); err != nil {
		t.Fatalf("Save seed model: %v", err)
	}

	// Open a SECOND *sql.DB handle to the same file. This handle has
	// its own connection pool, independent of the factory's pool, so
	// its write lock is visible to — and blocks — fx.store.db.
	// Use _txlock=immediate so BeginTx acquires the RESERVED lock
	// straight away (matching production factory wiring).
	holderDSN := fmt.Sprintf("file:%s?_txlock=immediate&_busy_timeout=10", dbPath)
	holderDB, err := sql.Open("sqlite3", holderDSN)
	if err != nil {
		t.Fatalf("open holder DB: %v", err)
	}
	t.Cleanup(func() { _ = holderDB.Close() })

	// BEGIN IMMEDIATE (via _txlock=immediate default) acquires the
	// RESERVED write lock. Do NOT commit — the lock is held until
	// Rollback in cleanup.
	holderTx, err := holderDB.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatalf("holder BeginTx: %v", err)
	}
	defer func() { _ = holderTx.Rollback() }()

	// Sanity: write something inside the holder tx to ensure the
	// lock is actually RESERVED (BEGIN IMMEDIATE acquires RESERVED
	// by default, but an explicit write is a belt-and-braces guarantee
	// across driver implementations).
	if _, err := holderTx.ExecContext(context.Background(),
		`INSERT INTO model_schema_extensions
		    (tenant_id, model_name, model_version, seq, kind, payload, tx_id)
		 VALUES (?, ?, ?, ?, 'delta', ?, '')`,
		"holder-tenant", "holder-model", "1", 1, []byte(`"holder"`)); err != nil {
		t.Fatalf("holder insert: %v", err)
	}

	// Now invoke ExtendSchema on the main factory. Each attempt's
	// BeginTx → BEGIN IMMEDIATE must wait for the RESERVED lock,
	// time out at ~10ms, return SQLITE_BUSY → ErrConflict. After
	// maxRetries attempts the loop exhausts.
	//
	// Use a generous ctx deadline so ctx cancellation doesn't
	// pre-empt the retry loop before exhaustion.
	extendCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	err = store.ExtendSchema(extendCtx, ref, spi.SchemaDelta(`"d1"`))
	if err == nil {
		t.Fatal("ExtendSchema under held write lock: got nil, want ErrRetryExhausted")
	}
	if !errors.Is(err, spi.ErrRetryExhausted) {
		t.Errorf("ExtendSchema error = %v, want errors.Is(_, ErrRetryExhausted)", err)
	}
	// Multi-%w wrap preserves the last conflict in the chain, so
	// ErrConflict must also be observable. This guards against a
	// regression where someone "cleans up" the wrap and loses the
	// retryable-class signal.
	if !errors.Is(err, spi.ErrConflict) {
		t.Errorf("ExtendSchema error = %v, want errors.Is(_, ErrConflict)", err)
	}

	// Cleanup: rollback the holder so the extensions table has no
	// stale rows. (t.Cleanup on holderDB.Close also covers this.)
	_ = holderTx.Rollback()
}

// TestSQLite_ExtendSchema_ContextCancellation_ReturnsCtxErr — §4.2.
// Cancelling ctx before ExtendSchema must return context.Canceled
// (wrapped) — not ErrRetryExhausted — since the retry loop checks
// ctx.Err() at the top of each attempt.
func TestSQLite_ExtendSchema_ContextCancellation_ReturnsCtxErr(t *testing.T) {
	fx := newSQLiteFixture(t)
	fx.store.applyFunc = setUnionApplyFunc
	ref := spi.ModelRef{EntityName: "E", ModelVersion: "1"}
	fx.SaveModel(t, ref, []byte(`[]`))

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
// At the savepoint-interval boundary, ExtendSchema is forced to fold the log
// and write a savepoint row. If that fold fails (applyFunc rejects the
// delta — e.g. a simulated ChangeLevel violation), the entire operation
// must roll back: neither the delta row that triggered the fold nor the
// would-be savepoint row may persist.
//
// Without an ambient caller-supplied tx, atomicity requires ExtendSchema
// to self-wrap — otherwise the initial delta INSERT auto-commits before
// the fold is attempted.
//
// Construction uses newSQLiteFixtureWithInterval(t, 10) directly so that
// the 10th ExtendSchema call triggers the savepoint fold path. This
// sidesteps the need for a mid-test reopen — the state reached after 9
// warmup deltas at interval=10 is identical whether or not the interval
// was changed at construction.
func TestSQLite_ExtendSchema_RejectionLeavesNoPersistedTrace(t *testing.T) {
	fx := newSQLiteFixtureWithInterval(t, 10)
	// The fixture already installed fixtureApplyFunc on the factory;
	// SetApplyFunc would panic on re-entry. Override the store-level
	// applyFunc directly — the same pattern Tasks 15-19 use.
	fx.store.applyFunc = func(base []byte, delta spi.SchemaDelta) ([]byte, error) {
		if bytes.Contains([]byte(delta), []byte("RESTRICT")) {
			return nil, fmt.Errorf("simulated ChangeLevel violation")
		}
		return setUnionApplyFunc(base, delta)
	}
	ref := spi.ModelRef{EntityName: "E", ModelVersion: "1"}
	fx.SaveModel(t, ref, []byte{})

	// Warm the log to one row below the interval boundary (9 deltas at
	// interval=10). The 10th ExtendSchema will drive the savepoint fold.
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

	// Attempt the 10th delta — the RESTRICT delta triggers the savepoint
	// path where applyFunc is called and rejects.
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
// model_schema_extensions. Exercises the FIRST-delta code path (no
// savepoint fold involved): rejection must come from the apply-check,
// not from the savepoint-fold boundary. This closes the asymmetry with
// the memory plugin, which runs applyFunc inline on every ExtendSchema.
func TestSQLite_ExtendSchema_RejectsDeltaThatApplyRefuses(t *testing.T) {
	fx := newSQLiteFixture(t)
	fx.store.applyFunc = func(base []byte, delta spi.SchemaDelta) ([]byte, error) {
		if bytes.Contains([]byte(delta), []byte("POISON")) {
			return nil, fmt.Errorf("applyFunc rejected delta: poison")
		}
		return setUnionApplyFunc(base, delta)
	}
	ref := spi.ModelRef{EntityName: "E", ModelVersion: "1"}
	fx.SaveModel(t, ref, []byte{})

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
