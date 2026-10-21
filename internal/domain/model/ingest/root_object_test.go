package ingest

import (
	"context"
	"encoding/json"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// An entity's data is a JSON object at every change level. Without a change
// level the model's object root refuses anything else; with one, a write that
// does not fit proposes a schema change instead — so a null root at TYPE made
// the model's root nullable and stored an entity with no data, and a scalar or
// array root at STRUCTURAL replaced the document with a value. Each is refused
// with the wording the unextended model gives, and the model is not extended.
func TestValidateOrExtend_RootIsAnObjectAtEveryChangeLevel(t *testing.T) {
	roots := map[string]any{
		"null":    nil,
		"number":  json.Number("5"),
		"string":  "x",
		"boolean": true,
		"array":   []any{map[string]any{"a": "b"}},
	}
	for _, level := range []spi.ChangeLevel{"", spi.ChangeLevelArrayLength, spi.ChangeLevelArrayElements, spi.ChangeLevelType, spi.ChangeLevelStructural} {
		for kind, root := range roots {
			desc := descWithChangeLevel(t)
			desc.ChangeLevel = level
			store := &recordingModelStore{}
			err := ValidateOrExtend(context.Background(), store, desc, root)
			if err == nil {
				t.Errorf("level %q, %s root: accepted", level, kind)
				continue
			}
			if want := "validation failed: expected object, got " + kind; err.Error() != want {
				t.Errorf("level %q, %s root: error %q; want %q", level, kind, err, want)
			}
			if len(store.deltas) != 0 {
				t.Errorf("level %q, %s root: the model was extended: %v", level, kind, store.deltas)
			}
		}
	}
}
