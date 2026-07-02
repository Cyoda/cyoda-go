package search

import spi "github.com/cyoda-platform/cyoda-go-spi"

// OrderKey is one parsed, pre-classification sort key from the request
// surface (HTTP grammar or gRPC orderBy). The service resolves it against the
// model schema into a fully-typed spi.OrderSpec.
type OrderKey struct {
	Path   string
	Source spi.FieldSource
	Desc   bool
}
