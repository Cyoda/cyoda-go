package postgres_test

import (
	"github.com/cyoda-platform/cyoda-go/plugins/postgres"
)

// beginGuarded is the in-package guard (tx_guard_test.go), reached through the
// export_test.go idiom. It is an alias, not a second copy: every transaction
// this suite opens — in either test package — goes through the same Begin +
// rollback-on-cleanup, so a new test cannot reintroduce the leak by copying an
// unguarded neighbour.
var beginGuarded = postgres.BeginGuardedForTest
