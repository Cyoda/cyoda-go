package importer

import (
	"io"

	"github.com/cyoda/cyoda-go/internal/domain/model/schema"
)

// Importer converts raw data from a reader into a ModelNode schema tree.
type Importer interface {
	Import(r io.Reader, dataFormat string) (*schema.ModelNode, error)
}
