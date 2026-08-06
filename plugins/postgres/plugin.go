package postgres

import (
	"context"
	"fmt"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

func init() { spi.Register(&plugin{}) }

type plugin struct{}

func (p *plugin) Name() string { return "postgres" }

func (p *plugin) ConfigVars() []spi.ConfigVar {
	return []spi.ConfigVar{
		{Name: "CYODA_POSTGRES_URL", Description: "PostgreSQL connection string", Required: true},
		{Name: "CYODA_POSTGRES_MAX_CONNS", Description: "Max pool connections", Default: "25"},
		{Name: "CYODA_POSTGRES_MIN_CONNS", Description: "Min pool connections", Default: "5"},
		{Name: "CYODA_POSTGRES_MAX_CONN_IDLE_TIME", Description: "Max idle time before closing connection", Default: "5m"},
		{Name: "CYODA_POSTGRES_AUTO_MIGRATE", Description: "Run embedded SQL migrations on startup", Default: "true"},
		{Name: "CYODA_POSTGRES_STATEMENT_TIMEOUT", Description: "Maximum run time for a single SQL statement; 0 disables", Default: "5m"},
		{Name: "CYODA_POSTGRES_IDLE_IN_TX_TIMEOUT", Description: "Maximum time a connection may sit idle inside an open transaction; 0 disables", Default: "5m"},
		{Name: "CYODA_POSTGRES_ACQUIRE_TIMEOUT", Description: "Maximum wait for a free pooled connection before failing with 503; 0 disables", Default: "10s"},
		{Name: "CYODA_POSTGRES_MIGRATE_LOCK_TIMEOUT", Description: "Maximum lock wait during schema migration; 0 disables", Default: "5m"},
		{Name: "CYODA_POSTGRES_SEARCH_STATEMENT_TIMEOUT", Description: "Statement ceiling for async search scans; 0 disables", Default: "30m"},
		{Name: "CYODA_SCHEMA_SAVEPOINT_INTERVAL", Description: "Rows per savepoint during schema extension", Default: "64"},
	}
}

func (p *plugin) NewFactory(
	ctx context.Context,
	getenv func(string) string,
	opts ...spi.FactoryOption,
) (spi.StoreFactory, error) {
	cfg, err := parseConfig(getenv)
	if err != nil {
		return nil, fmt.Errorf("postgres: %w", err)
	}

	pool, err := newPool(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("postgres: %w", err)
	}

	compatDB := openDB(pool)
	compatErr := checkSchemaCompat(ctx, compatDB, cfg.AutoMigrate)
	_ = compatDB.Close()
	if compatErr != nil {
		pool.Close()
		return nil, compatErr
	}
	if cfg.AutoMigrate {
		if err := runMigrations(ctx, pool); err != nil {
			pool.Close()
			return nil, fmt.Errorf("postgres migrate: %w", err)
		}
	}

	factory := newStoreFactory(pool, cfg)
	factory.initTransactionManager(&defaultUUIDGenerator{})
	return factory, nil
}
