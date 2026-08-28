package importer

import (
	"fmt"
	"io"

	"github.com/cyoda-platform/cyoda-go/internal/domain/model/schema"
)

// SampleDataImporter imports sample data by parsing it according to the given
// format and walking the result into a ModelNode tree.
type SampleDataImporter struct{}

// NewSampleDataImporter returns a new SampleDataImporter.
func NewSampleDataImporter() *SampleDataImporter {
	return &SampleDataImporter{}
}

// Import reads data from r, parses it according to dataFormat ("JSON", "XML"),
// and returns the inferred ModelNode schema tree.
func (i *SampleDataImporter) Import(r io.Reader, dataFormat string) (*schema.ModelNode, error) {
	var parsed any
	var err error
	switch dataFormat {
	case "JSON":
		parsed, err = ParseJSON(r)
	case "XML":
		parsed, err = ParseXML(r)
	default:
		return nil, fmt.Errorf("unsupported data format: %s", dataFormat)
	}
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", dataFormat, err)
	}
	return walkSampleData(parsed)
}
