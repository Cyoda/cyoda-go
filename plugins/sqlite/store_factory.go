package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/gofrs/flock"

	_ "github.com/ncruces/go-sqlite3/driver"
)

// Option is a functional option for newStoreFactory.
type Option func(*StoreFactory)

// WithClock injects a custom Clock into the factory.
// Used by conformance tests to advance time deterministically.
func WithClock(c Clock) Option {
	return func(f *StoreFactory) { f.clock = c }
}

// ApplyFunc replays an opaque SchemaDelta onto a base schema and
// returns the new schema bytes. Production wiring uses schema.Apply
// from internal/domain/model/schema; the SPI keeps deltas opaque so
// the catalog stays out of the plugin.
type ApplyFunc func(base []byte, delta spi.SchemaDelta) ([]byte, error)

// WithApplyFunc installs the replay function used by ExtendSchema.
// Must be called when the caller intends to use ExtendSchema; until
// then, ExtendSchema returns an informative error.
func WithApplyFunc(fn ApplyFunc) Option {
	return func(f *StoreFactory) { f.applyFunc = fn }
}

// SetApplyFunc installs the replay function used by ExtendSchema.
// May be called at most once — typically immediately after
// Plugin.NewFactory in app/app.go. Panics on double-call
// (programmer error).
//
// The parameter is the unnamed function type (not sqlite.ApplyFunc)
// so that an interface type-assertion in app/app.go can satisfy the
// setter uniformly across plugins.
func (f *StoreFactory) SetApplyFunc(fn func(base []byte, delta spi.SchemaDelta) ([]byte, error)) {
	if f.applyFunc != nil {
		panic("sqlite: SetApplyFunc called twice")
	}
	f.applyFunc = ApplyFunc(fn)
}

// StoreFactory implements spi.StoreFactory backed by SQLite.
type StoreFactory struct {
	db        *sql.DB
	fileLock  *flock.Flock
	clock     Clock
	cfg       config
	tm        *transactionManager
	applyFunc ApplyFunc

	closeMu sync.Mutex
	closed  bool

	walTicker *time.Ticker
	walDone   chan struct{}
}

func newStoreFactory(ctx context.Context, cfg config, opts ...Option) (*StoreFactory, error) {
	if err := os.MkdirAll(filepath.Dir(cfg.Path), 0o755); err != nil {
		return nil, fmt.Errorf("create db directory: %w", err)
	}

	fl := flock.New(cfg.Path + ".lock")
	locked, err := fl.TryLock()
	if err != nil {
		return nil, fmt.Errorf("acquire file lock on %s: %w", cfg.Path, err)
	}
	if !locked {
		return nil, fmt.Errorf("another cyoda-go instance is using %s", cfg.Path)
	}

	dsn := fmt.Sprintf("file:%s?_txlock=immediate&_busy_timeout=%d",
		cfg.Path, cfg.BusyTimeout.Milliseconds())
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		_ = fl.Unlock()
		return nil, fmt.Errorf("open sqlite: %w", err)
	}

	// Limit to a single connection — SQLite is single-writer and the
	// golang-migrate sqlite driver holds the db open during migrations.
	// Multiple connections against a file-based SQLite can cause locking
	// issues. We serialize access via Go-level concurrency.
	db.SetMaxOpenConns(1)

	if err := applyPragmas(db, cfg); err != nil {
		db.Close()
		_ = fl.Unlock()
		return nil, fmt.Errorf("apply pragmas: %w", err)
	}

	if err := assertMinVersion(db); err != nil {
		db.Close()
		_ = fl.Unlock()
		return nil, err
	}

	if err := checkSchemaCompat(ctx, db, cfg.AutoMigrate); err != nil {
		db.Close()
		_ = fl.Unlock()
		return nil, err
	}
	if cfg.AutoMigrate {
		if err := runMigrations(ctx, db); err != nil {
			db.Close()
			_ = fl.Unlock()
			return nil, fmt.Errorf("sqlite migrate: %w", err)
		}
	}

	f := &StoreFactory{
		db:       db,
		fileLock: fl,
		clock:    wallClock{},
		cfg:      cfg,
	}
	for _, o := range opts {
		o(f)
	}
	f.startWALMaintenance()
	return f, nil
}

func applyPragmas(db *sql.DB, cfg config) error {
	pragmas := []string{
		"PRAGMA journal_mode = WAL",
		"PRAGMA synchronous = NORMAL",
		fmt.Sprintf("PRAGMA busy_timeout = %d", cfg.BusyTimeout.Milliseconds()),
		fmt.Sprintf("PRAGMA cache_size = -%d", cfg.CacheSizeKiB),
		"PRAGMA foreign_keys = ON",
		"PRAGMA mmap_size = 268435456",
		"PRAGMA journal_size_limit = 67108864",
		"PRAGMA auto_vacuum = INCREMENTAL",
	}
	for _, p := range pragmas {
		if _, err := db.Exec(p); err != nil {
			return fmt.Errorf("%s: %w", p, err)
		}
	}
	return nil
}

func assertMinVersion(db *sql.DB) error {
	var ver string
	if err := db.QueryRow("SELECT sqlite_version()").Scan(&ver); err != nil {
		return fmt.Errorf("query sqlite version: %w", err)
	}
	slog.Info("sqlite version", "pkg", "sqlite", "version", ver)
	// ncruces/go-sqlite3 bundles SQLite 3.46+, so this assertion is
	// primarily a safety net for unexpected downgrades.
	if ver < "3.45.0" {
		return fmt.Errorf("sqlite version %s is below required minimum 3.45.0 (JSONB support)", ver)
	}
	return nil
}

func (f *StoreFactory) startWALMaintenance() {
	f.walTicker = time.NewTicker(5 * time.Minute)
	f.walDone = make(chan struct{})
	go func() {
		for {
			select {
			case <-f.walTicker.C:
				if _, err := f.db.Exec("PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
					slog.Warn("wal checkpoint failed", "pkg", "sqlite", "error", err)
				}
				if _, err := f.db.Exec("PRAGMA incremental_vacuum(1000)"); err != nil {
					slog.Warn("incremental vacuum failed", "pkg", "sqlite", "error", err)
				}
			case <-f.walDone:
				return
			}
		}
	}()
}

func resolveTenant(ctx context.Context) (spi.TenantID, error) {
	uc := spi.GetUserContext(ctx)
	if uc == nil {
		return "", fmt.Errorf("no user context in request — tenant cannot be resolved")
	}
	if uc.Tenant.ID == "" {
		return "", fmt.Errorf("user context has no tenant")
	}
	return uc.Tenant.ID, nil
}

func (f *StoreFactory) EntityStore(ctx context.Context) (spi.EntityStore, error) {
	tid, err := resolveTenant(ctx)
	if err != nil {
		return nil, err
	}
	return &entityStore{db: f.db, tenantID: tid, tm: f.tm, clock: f.clock, cfg: f.cfg}, nil
}

func (f *StoreFactory) ModelStore(ctx context.Context) (spi.ModelStore, error) {
	tid, err := resolveTenant(ctx)
	if err != nil {
		return nil, err
	}
	return &modelStore{db: f.db, tenantID: tid, applyFunc: f.applyFunc, cfg: f.cfg}, nil
}

func (f *StoreFactory) KeyValueStore(ctx context.Context) (spi.KeyValueStore, error) {
	tid, err := resolveTenant(ctx)
	if err != nil {
		return nil, err
	}
	return &kvStore{db: f.db, tenantID: tid}, nil
}

func (f *StoreFactory) MessageStore(ctx context.Context) (spi.MessageStore, error) {
	tid, err := resolveTenant(ctx)
	if err != nil {
		return nil, err
	}
	return &messageStore{db: f.db, tenantID: tid}, nil
}

func (f *StoreFactory) WorkflowStore(ctx context.Context) (spi.WorkflowStore, error) {
	tid, err := resolveTenant(ctx)
	if err != nil {
		return nil, err
	}
	kv := &kvStore{db: f.db, tenantID: tid}
	return &workflowStore{kv: kv}, nil
}

func (f *StoreFactory) StateMachineAuditStore(ctx context.Context) (spi.StateMachineAuditStore, error) {
	tid, err := resolveTenant(ctx)
	if err != nil {
		return nil, err
	}
	return &smAuditStore{db: f.db, tenantID: tid}, nil
}

func (f *StoreFactory) AsyncSearchStore(_ context.Context) (spi.AsyncSearchStore, error) {
	return &asyncSearchStore{db: f.db, clock: f.clock}, nil
}

// TransactionManager implements spi.StoreFactory.
// Returns the TM registered via initTransactionManager. Errors if none is set.
func (f *StoreFactory) TransactionManager(_ context.Context) (spi.TransactionManager, error) {
	if f.tm == nil {
		return nil, fmt.Errorf("sqlite: TransactionManager not initialized")
	}
	return f.tm, nil
}

func (f *StoreFactory) Close() error {
	f.closeMu.Lock()
	defer f.closeMu.Unlock()
	if f.closed {
		return nil
	}
	f.closed = true
	if f.walTicker != nil {
		f.walTicker.Stop()
		close(f.walDone)
	}
	var firstErr error
	if err := f.db.Close(); err != nil && firstErr == nil {
		firstErr = err
	}
	if err := f.fileLock.Unlock(); err != nil && firstErr == nil {
		firstErr = err
	}
	return firstErr
}

// SupportsCompositeUniqueKeys advertises composite-unique-key enforcement.
func (f *StoreFactory) SupportsCompositeUniqueKeys() bool { return true }

// initTransactionManager installs the SI+FCW transaction manager on the factory.
// Called by Plugin.NewFactory after the factory is created.
// Seeds lastSubmitTime from the database to maintain monotonicity across restarts.
func (f *StoreFactory) initTransactionManager(uuids spi.UUIDGenerator) {
	f.tm = newTransactionManager(f, uuids)
	f.tm.seedLastSubmitTime()
}

// NewStoreFactoryForTest creates a factory with auto-migrate enabled and the
// given path. Intended for test use only.
func NewStoreFactoryForTest(ctx context.Context, dbPath string, opts ...Option) (*StoreFactory, error) {
	cfg := config{
		Path:                   dbPath,
		AutoMigrate:            true,
		BusyTimeout:            5 * time.Second,
		CacheSizeKiB:           64000,
		SearchScanLimit:        100_000,
		SchemaExtendMaxRetries: 8,
	}
	f, err := newStoreFactory(ctx, cfg, opts...)
	if err != nil {
		return nil, err
	}
	f.initTransactionManager(&defaultUUIDGenerator{})
	return f, nil
}

// NewStoreFactoryForTestWithScanLimit creates a factory with a custom scan
// limit for testing scan budget exhaustion. Intended for test use only.
func NewStoreFactoryForTestWithScanLimit(ctx context.Context, dbPath string, scanLimit int, opts ...Option) (*StoreFactory, error) {
	cfg := config{
		Path:                   dbPath,
		AutoMigrate:            true,
		BusyTimeout:            5 * time.Second,
		CacheSizeKiB:           64000,
		SearchScanLimit:        scanLimit,
		SchemaExtendMaxRetries: 8,
	}
	f, err := newStoreFactory(ctx, cfg, opts...)
	if err != nil {
		return nil, err
	}
	f.initTransactionManager(&defaultUUIDGenerator{})
	return f, nil
}
