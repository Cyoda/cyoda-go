package events

import "github.com/cyoda-platform/cyoda-go-spi/strictjson"

// decodeWithUseNumber decodes a CloudEvent's data, or one member of it, with
// strictjson: a member name spells a schema property exactly, once, and
// anything else is refused with an error naming where. Numeric literals in
// freeform fields (map[string]interface{} / interface{}) are kept as
// json.Number instead of being coerced to float64 and losing precision above
// 2^53.
//
// This helper backs every generated UnmarshalJSON method in this package.
// The generator emits `json.Unmarshal(value, ...)` by default; a post-
// processing step in scripts/generate-events.sh rewrites those calls to
// reference this helper. Keep the helper in a hand-written file (not the
// generated types.go) so regeneration preserves it.
func decodeWithUseNumber(data []byte, v any) error {
	return strictjson.Decode(data, v, strictjson.PreserveNumbers)
}
