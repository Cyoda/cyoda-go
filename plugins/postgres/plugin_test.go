package postgres_test

import (
	"testing"

	spi "github.com/cyoda/cyoda-go-spi"

	_ "github.com/cyoda/cyoda-go/plugins/postgres"
)

func TestPluginRegistered(t *testing.T) {
	p, ok := spi.GetPlugin("postgres")
	if !ok {
		t.Fatal(`spi.GetPlugin("postgres") returned ok=false after blank import`)
	}
	if p.Name() != "postgres" {
		t.Errorf(`Name() = %q, want "postgres"`, p.Name())
	}
	dp, ok := p.(spi.DescribablePlugin)
	if !ok {
		t.Fatal("postgres plugin should implement DescribablePlugin")
	}
	vars := dp.ConfigVars()
	if len(vars) < 1 {
		t.Fatal("expected at least one ConfigVar")
	}
	var found bool
	for _, v := range vars {
		if v.Name == "CYODA_POSTGRES_URL" {
			found = true
			if !v.Required {
				t.Error("CYODA_POSTGRES_URL should be Required")
			}
			break
		}
	}
	if !found {
		t.Error("CYODA_POSTGRES_URL missing from ConfigVars")
	}
}
