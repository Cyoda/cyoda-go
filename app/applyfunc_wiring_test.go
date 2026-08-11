package app

import (
	spi "github.com/cyoda-platform/cyoda-go-spi"

	memoryplugin "github.com/cyoda-platform/cyoda-go/plugins/memory"
	pgplugin "github.com/cyoda-platform/cyoda-go/plugins/postgres"
	sqliteplugin "github.com/cyoda-platform/cyoda-go/plugins/sqlite"
)

// Run wires the schema-apply replay function into the storage factory
// through a soft type-assertion (see the applyFuncSetter interface in
// app.go): a factory that does not implement SetApplyFunc is silently
// skipped, and every later fold-on-read of a model with pending schema
// deltas fails. These compile-time assertions pin all in-tree plugin
// factories to the exact signature app.go asserts on, so a signature
// drift surfaces here as a build failure instead of at runtime as an
// unfoldable model.
type wiredApplyFuncSetter interface {
	SetApplyFunc(fn func(base []byte, delta spi.SchemaDelta) ([]byte, error))
}

var (
	_ wiredApplyFuncSetter = (*memoryplugin.StoreFactory)(nil)
	_ wiredApplyFuncSetter = (*pgplugin.StoreFactory)(nil)
	_ wiredApplyFuncSetter = (*sqliteplugin.StoreFactory)(nil)
)
