package app

import (
	"strings"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"

	memoryplugin "github.com/cyoda-platform/cyoda-go/plugins/memory"
	pgplugin "github.com/cyoda-platform/cyoda-go/plugins/postgres"
	sqliteplugin "github.com/cyoda-platform/cyoda-go/plugins/sqlite"
)

// Run wires the schema replay function into the storage factory through
// wireSchemaApply, which refuses a factory without SetApplyFunc of exactly
// applyFuncSetter's signature, and Run then exits. These compile-time
// assertions pin all in-tree plugin factories to that declaration, so a drift
// on either side surfaces here as a build failure rather than as a refused
// startup.
var (
	_ applyFuncSetter = (*memoryplugin.StoreFactory)(nil)
	_ applyFuncSetter = (*pgplugin.StoreFactory)(nil)
	_ applyFuncSetter = (*sqliteplugin.StoreFactory)(nil)
)

// noSetterFactory is a storage factory that offers no SetApplyFunc.
type noSetterFactory struct{ spi.StoreFactory }

// oldSetterFactory offers SetApplyFunc with the per-delta signature.
type oldSetterFactory struct{ spi.StoreFactory }

func (oldSetterFactory) SetApplyFunc(func(base []byte, delta spi.SchemaDelta) ([]byte, error)) {}

// recordingSetterFactory offers SetApplyFunc with the list signature.
type recordingSetterFactory struct {
	spi.StoreFactory
	fn func(base []byte, deltas []spi.SchemaDelta) ([]byte, error)
}

func (f *recordingSetterFactory) SetApplyFunc(fn func(base []byte, deltas []spi.SchemaDelta) ([]byte, error)) {
	f.fn = fn
}

// TestWireSchemaApply_RefusesFactoryWithoutSetter: a factory that cannot take
// the apply function cannot fold a model's extension log, so startup refuses
// it instead of running with every extended model unreadable.
func TestWireSchemaApply_RefusesFactoryWithoutSetter(t *testing.T) {
	for name, f := range map[string]spi.StoreFactory{
		"no SetApplyFunc":        noSetterFactory{},
		"per-delta SetApplyFunc": oldSetterFactory{},
	} {
		t.Run(name, func(t *testing.T) {
			err := wireSchemaApply(f)
			if err == nil {
				t.Fatal("wireSchemaApply accepted a factory without SetApplyFunc of the list signature")
			}
			if !strings.Contains(err.Error(), "SetApplyFunc(func(base []byte, deltas []spi.SchemaDelta) ([]byte, error))") {
				t.Errorf("error does not name the required method: %v", err)
			}
		})
	}
}

// TestWireSchemaApply_InstallsApplyMany: a factory with the setter gets the
// list-form replay function.
func TestWireSchemaApply_InstallsApplyMany(t *testing.T) {
	f := &recordingSetterFactory{}
	if err := wireSchemaApply(f); err != nil {
		t.Fatalf("wireSchemaApply: %v", err)
	}
	if f.fn == nil {
		t.Fatal("no apply function installed")
	}
	got, err := f.fn([]byte(`{"kind":"OBJECT"}`), nil)
	if err != nil {
		t.Fatalf("installed apply function: %v", err)
	}
	if string(got) != `{"kind":"OBJECT"}` {
		t.Errorf("installed apply function re-encoded the base as %s", got)
	}
}
