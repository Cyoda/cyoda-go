package sqlite

import (
	"context"
	"fmt"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

func init() { spi.Register(&plugin{}) }

type plugin struct{}

func (p *plugin) Name() string { return "sqlite" }

func (p *plugin) ConfigVars() []spi.ConfigVar {
	return []spi.ConfigVar{
		{Name: "CYODA_SQLITE_PATH", Description: "Database file path", Default: "$XDG_DATA_HOME/cyoda/cyoda.db (Windows: %LocalAppData%\\cyoda\\cyoda.db)"},
		{Name: "CYODA_SQLITE_AUTO_MIGRATE", Description: "Run embedded SQL migrations on startup", Default: "true"},
		{Name: "CYODA_SQLITE_BUSY_TIMEOUT", Description: "Wait time for write lock", Default: "5s"},
		{Name: "CYODA_SQLITE_CACHE_SIZE", Description: "Page cache in KiB", Default: "64000"},
		{Name: "CYODA_SQLITE_SEARCH_SCAN_LIMIT", Description: "Max rows examined per search with residual filter", Default: "100000"},
		{Name: "CYODA_SCHEMA_SAVEPOINT_INTERVAL", Description: "Rows per savepoint during schema extension", Default: "64"},
		{Name: "CYODA_SCHEMA_EXTEND_MAX_RETRIES", Description: "Max retries on concurrent schema extension", Default: "8"},
	}
}

func (p *plugin) NewFactory(
	ctx context.Context,
	getenv func(string) string,
	opts ...spi.FactoryOption,
) (spi.StoreFactory, error) {
	cfg, err := parseConfig(getenv)
	if err != nil {
		return nil, fmt.Errorf("sqlite: %w", err)
	}

	factory, err := newStoreFactory(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("sqlite: %w", err)
	}

	factory.initTransactionManager(&defaultUUIDGenerator{})
	return factory, nil
}
