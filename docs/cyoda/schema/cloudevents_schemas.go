// Package cyodaschemas embeds the CloudEvent JSON Schema tree shipped
// with cyoda-go. The tree is the canonical source of truth for event-
// payload validation and is consumed by:
//
//   - cmd/cyoda/help (the `cyoda help cloudevents json` action)
//   - scripts/generate-events.sh (Go-struct generation via go-jsonschema)
//   - downstream tooling that extracts the tree from the binary at
//     build time (e.g. cyoda-docs)
//
// The directory layout mirrors the URL scheme used in `$id` values:
// `common/…`, `entity/…`, `model/…`, `platform/…`, `processing/…`, `search/…`.
// `common/statemachine/` is a nested sub-category for state-machine
// metadata (WorkflowInfo, TransitionInfo, ProcessorInfo).
package cyodaschemas

import "embed"

// FS is the embedded filesystem rooted at this directory. The root has
// six top-level entries (common, entity, model, platform, processing, search);
// recursion into `common/statemachine/` is automatic per go:embed
// directory semantics.
//
//go:embed common entity model platform processing search
var FS embed.FS

// BaseID is the URL prefix used in every schema's `$id` field. Exposed
// so emitters can publish it in envelopes that downstream consumers
// use to construct absolute IDs.
const BaseID = "https://cyoda.com/cloud/event/"

// MetaSchemaURL identifies the JSON Schema meta-schema the payloads
// conform to (Draft 2020-12).
const MetaSchemaURL = "https://json-schema.org/draft/2020-12/schema"
