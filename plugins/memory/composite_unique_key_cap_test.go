package memory

import (
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

func TestFactory_SupportsCompositeUniqueKeys(t *testing.T) {
	var v any = &StoreFactory{}
	c, ok := v.(spi.CompositeUniqueKeyCapable)
	if !ok || !c.SupportsCompositeUniqueKeys() {
		t.Fatal("memory factory must advertise composite unique key support")
	}
}
