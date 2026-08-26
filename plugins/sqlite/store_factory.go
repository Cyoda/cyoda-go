package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
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
	readDB    *sql.DB
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

	dsn := fmt.Sprintf("file:%s?_txlock=immediate", cfg.Path)
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

	// readDB is a second, dedicated *sql.DB against the same file, used
	// exclusively for reads (GetPage, GetVersionMetadata,
	// GetVersionByTransaction, non-tx Iterate). It never opens a
	// transaction, so _txlock never applies to it.
	//
	// Why a separate pool at all: a long-lived, undrained Iterate cursor
	// checks out its connection for the life of the iterator. Sharing db's
	// sole connection would starve every concurrent write (e.g. a streamed
	// async-search SaveResults chunk) until the iterator is closed.
	//
	// Why more than one connection: WAL journal mode (a database-level
	// property set on db above) allows readers to run concurrently with
	// each other and with the writer. Capping the reader at one connection
	// would just move the starvation inside the reader pool — the async
	// search pool alone runs CYODA_SEARCH_ASYNC_WORKERS (8 by default)
	// scans, and every interactive GetPage would queue behind whichever
	// long scan currently held the connection.
	//
	// No migration run — db above already owns schema setup, and readDB is
	// opened after it succeeds.
	//
	// _txlock=deferred, not the writer DSN's immediate: a transaction opened
	// here must never take the write lock. GetResultIDs asks for a read-only
	// transaction, which the driver already begins DEFERRED, but a future
	// caller passing nil TxOptions would inherit immediate from a shared DSN
	// and start contending with the writer from a pool sized for readers.
	// query_only(1) in readerPragmaParams makes the same guarantee at the
	// statement level.
	readerDSN := fmt.Sprintf("file:%s?_txlock=deferred", cfg.Path) + readerPragmaParams(cfg)
	readDB, err := sql.Open("sqlite3", readerDSN)
	if err != nil {
		db.Close()
		_ = fl.Unlock()
		return nil, fmt.Errorf("open sqlite reader connection: %w", err)
	}
	readers := cfg.readerPoolSize()
	readDB.SetMaxOpenConns(readers)
	// Idle == open so a burst of concurrent scans does not reopen (and
	// re-PRAGMA) connections on every wave; ConnMaxIdleTime returns the
	// per-connection page cache after a quiet period.
	readDB.SetMaxIdleConns(readers)
	readDB.SetConnMaxIdleTime(5 * time.Minute)
	// Fail fast on a bad DSN/pragma here rather than at the first read:
	// the reader's PRAGMAs travel in the DSN (see readerPragmaParams), so
	// they are applied by the driver at connect time, not by applyPragmas.
	if err := readDB.PingContext(ctx); err != nil {
		db.Close()
		readDB.Close()
		_ = fl.Unlock()
		return nil, fmt.Errorf("open sqlite reader connection: %w", err)
	}

	f := &StoreFactory{
		db:       db,
		readDB:   readDB,
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

// defaultReaderPoolSize returns how many connections readDB holds open when
// CYODA_SQLITE_READER_POOL_SIZE is not set.
//
// Sized by GOMAXPROCS: SQLite reads execute in-process (the driver is a
// wasm build of SQLite, not a network client), so reader concurrency past
// the number of runnable threads buys no throughput.
//
// Floored at 4 so a one- or two-CPU container still keeps an interactive
// GetPage off the back of a long streaming scan — the starvation this pool
// exists to prevent.
//
// Ceilinged at 8 because PRAGMA cache_size is per-connection: each pooled
// reader owns its own page cache (cfg.CacheSizeKiB, 64 MiB by default), so
// an uncapped pool would scale resident memory with core count. 8 is also
// the default async-search worker count (CYODA_SEARCH_ASYNC_WORKERS) — the
// largest number of simultaneous long scans the engine issues out of the box.
//
// GOMAXPROCS follows the CPU quota and is blind to the memory cgroup, so this
// derivation alone cannot serve a container that is generous on cores and
// tight on memory — a 512 MiB limit with 8 CPUs gets a ~562 MiB page-cache
// ceiling (writer + 8 readers × 64 MiB). CYODA_SQLITE_READER_POOL_SIZE is the
// lever for that case; CYODA_SQLITE_CACHE_SIZE is not, because it shrinks the
// writer's cache along with the readers'.
func defaultReaderPoolSize() int {
	n := runtime.GOMAXPROCS(0)
	if n < 4 {
		return 4
	}
	if n > 8 {
		return 8
	}
	return n
}

// readerPoolSize returns the reader-pool size this config asks for, falling
// back to the derived default. The fallback covers a config struct built in
// code without the field (an internal caller, a test helper): database/sql
// reads SetMaxOpenConns(0) as UNLIMITED, which would uncap the page-cache
// ceiling this knob exists to bound. parseConfig already floors env input at 1.
func (c config) readerPoolSize() int {
	if c.ReaderPoolSize < 1 {
		return defaultReaderPoolSize()
	}
	return c.ReaderPoolSize
}

// readerPragmaParams returns the "&_pragma=..." DSN suffix that configures
// every connection readDB opens.
//
// applyPragmas (below) issues its statements over the pool, so with more
// than one connection it would configure only whichever connection happened
// to serve them; the rest would silently run on SQLite's defaults (2 MiB
// page cache, foreign keys off, the driver's 1-minute busy timeout). The
// driver applies "_pragma" DSN parameters on every connection it opens, in
// order, which is the only per-connection hook available here.
//
// Only the per-connection settings are repeated. journal_mode and
// auto_vacuum are persistent database-level properties already established
// on db, and re-issuing journal_mode on a reader would contend with an
// active writer for no gain.
//
// query_only(1) turns "readDB is only ever read from" from a convention three
// comments in this package assert into something SQLite enforces: every write
// statement on a reader connection fails outright. A write here would contend
// with the writer for the database lock from a pool whose busy-timeout budget
// was never sized for it, so the next read path added to readDB must not be
// able to acquire write rights by accident.
func readerPragmaParams(cfg config) string {
	// busy_timeout first, per the driver's documented PRAGMA ordering.
	pragmas := []string{
		fmt.Sprintf("busy_timeout(%d)", cfg.BusyTimeout.Milliseconds()),
		"synchronous(NORMAL)",
		fmt.Sprintf("cache_size(-%d)", cfg.CacheSizeKiB),
		"foreign_keys(ON)",
		"mmap_size(268435456)",
		"journal_size_limit(67108864)",
		"query_only(1)",
	}
	var sb strings.Builder
	for _, p := range pragmas {
		sb.WriteString("&_pragma=")
		sb.WriteString(url.QueryEscape(p))
	}
	return sb.String()
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
	return &entityStore{db: f.db, readDB: f.readDB, tenantID: tid, tm: f.tm, clock: f.clock, cfg: f.cfg}, nil
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
	return &asyncSearchStore{db: f.db, readDB: f.readDB, clock: f.clock}, nil
}

// ScheduledTaskStore returns the durable ScheduledTask store. Unlike the
// per-tenant accessors above, it does not resolve a tenant from ctx: ScanDue
// is cross-tenant by design and Upsert/Delete/Reconcile carry the tenant on
// the task/request itself (see spi.ScheduledTaskStore godoc).
func (f *StoreFactory) ScheduledTaskStore(_ context.Context) (spi.ScheduledTaskStore, error) {
	return &scheduledTaskStore{db: f.db, tm: f.tm}, nil
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
	if err := f.readDB.Close(); err != nil && firstErr == nil {
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
		ReaderPoolSize:         defaultReaderPoolSize(),
		SchemaExtendMaxRetries: 8,
	}
	f, err := newStoreFactory(ctx, cfg, opts...)
	if err != nil {
		return nil, err
	}
	f.initTransactionManager(&defaultUUIDGenerator{})
	return f, nil
}
