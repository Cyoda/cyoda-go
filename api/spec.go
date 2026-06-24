package api

import (
	_ "embed"

	"github.com/getkin/kin-openapi/openapi3"
)

// specYAML is the OpenAPI document embedded verbatim from the source file.
//
//go:embed openapi.yaml
var specYAML []byte

// GetSwagger returns the OpenAPI specification for this service.
//
// The spec is embedded as the raw openapi.yaml source rather than
// re-serialized by oapi-codegen's embedded-spec output. oapi-codegen rewrites
// operationIds to PascalCase and does not round-trip OpenAPI 3.1 faithfully
// (it only supports 3.0.x), so its embedded spec diverges from the source.
// Embedding the source file directly keeps the served document — /openapi.json,
// the Scalar UI, and `cyoda help openapi` — byte-faithful to openapi.yaml.
//
// A fresh document is parsed on each call so callers may mutate the result
// (e.g. rewriting servers for the live host) without affecting others.
func GetSwagger() (*openapi3.T, error) {
	loader := openapi3.NewLoader()
	loader.IsExternalRefsAllowed = true
	return loader.LoadFromData(specYAML)
}
