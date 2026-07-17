package sqlite

import (
	"flag"
	"os"
	"testing"

	"github.com/cyoda-platform/cyoda-go/e2e/parity"
	_ "github.com/cyoda-platform/cyoda-go/e2e/parity/externalapi"         // register ExternalAPI scenario suite
	_ "github.com/cyoda-platform/cyoda-go/e2e/parity/scheduledtransition" // register scheduled-transition scenario suite
)

var sharedFixture *sqliteFixture

func TestMain(m *testing.M) {
	flag.Parse()
	if testing.Short() {
		os.Exit(0)
	}

	fix, teardown, err := setup()
	if err != nil {
		// Cannot use t.Fatal in TestMain — print and exit.
		println("FATAL: fixture setup failed:", err.Error())
		os.Exit(1)
	}

	sharedFixture = fix
	// os.Exit skips deferred calls, so teardown must run before Exit —
	// otherwise the cyoda subprocess (which inherits the test binary's
	// stderr) survives, `go test` waits its WaitDelay for child I/O to
	// close, and the package is marked FAIL even after PASS is printed.
	code := m.Run()
	teardown()
	os.Exit(code)
}

func TestParity(t *testing.T) {
	for _, nt := range parity.AllTests() {
		t.Run(nt.Name, func(t *testing.T) {
			nt.Fn(t, sharedFixture)
		})
	}
}

func TestParity_SchemaExtensionPropertyBudget(t *testing.T) {
	parity.RunSchemaExtensionPropertyBudget(t, sharedFixture)
}
