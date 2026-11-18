package memory_test

import (
	"context"
	"testing"

	spi "github.com/cyoda/cyoda-go-spi"

	_ "github.com/cyoda/cyoda-go/plugins/memory"
)

func TestPluginRegistered(t *testing.T) {
	p, ok := spi.GetPlugin("memory")
	if !ok {
		t.Fatal(`spi.GetPlugin("memory") returned ok=false after blank import`)
	}
	if p.Name() != "memory" {
		t.Errorf(`Name() = %q, want "memory"`, p.Name())
	}
}

func TestNewFactory_ReturnsReadyFactory(t *testing.T) {
	p, _ := spi.GetPlugin("memory")
	factory, err := p.NewFactory(context.Background(), nilGetenv)
	if err != nil {
		t.Fatalf("NewFactory: %v", err)
	}
	tm, err := factory.TransactionManager(context.Background())
	if err != nil {
		t.Fatalf("TransactionManager: %v", err)
	}
	if tm == nil {
		t.Fatal("expected non-nil TransactionManager")
	}
	if err := factory.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
}

func nilGetenv(string) string { return "" }
