package app

import (
	memoryplugin "github.com/cyoda-platform/cyoda-go/plugins/memory"
	pgplugin "github.com/cyoda-platform/cyoda-go/plugins/postgres"
	sqliteplugin "github.com/cyoda-platform/cyoda-go/plugins/sqlite"
)

// Run wires the schema-apply replay function into the storage factory
// through a soft type-assertion on applyFuncSetter (app.go): a factory
// that does not implement SetApplyFunc is silently skipped, and every
// later fold-on-read of a model with pending schema deltas fails. These
// compile-time assertions pin all in-tree plugin factories to the SAME
// declaration Run asserts on, so a drift on either side surfaces here
// as a build failure instead of at runtime as an unfoldable model.
var (
	_ applyFuncSetter = (*memoryplugin.StoreFactory)(nil)
	_ applyFuncSetter = (*pgplugin.StoreFactory)(nil)
	_ applyFuncSetter = (*sqliteplugin.StoreFactory)(nil)
)
