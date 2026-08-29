package importer

import (
	"encoding/json"
	"fmt"
	"math/big"

	"github.com/cyoda-platform/cyoda-go/internal/domain/model/schema"
)

// WalkConfig is retained as an empty struct for backward compatibility
// across the refactor. Scope fields (IntScope, DecimalScope) were
// removed in A.1 Task 13 along with the BYTE/SHORT/FLOAT DataTypes.
type WalkConfig struct{}

// DefaultWalkConfig returns an empty WalkConfig.
func DefaultWalkConfig() WalkConfig { return WalkConfig{} }

// Walk converts a generic parsed data tree into a ModelNode schema tree.
func Walk(data any) (*schema.ModelNode, error) {
	return WalkWithConfig(data, DefaultWalkConfig())
}

// WalkWithConfig applies the default walk.
func WalkWithConfig(data any, cfg WalkConfig) (*schema.ModelNode, error) {
	w := &walker{cfg: cfg}
	return w.walkValue(data, rootPath)
}

// rootPath is the canonical path of the document root — the same "$" prefix
// ModelNode.FieldsMap keys its entries by, so a walk-time diagnostic names a
// location in the vocabulary the rest of the stack uses.
const rootPath = "$"

type walker struct {
	cfg WalkConfig
}

func (w *walker) walkValue(v any, path string) (*schema.ModelNode, error) {
	switch val := v.(type) {
	case map[string]any:
		return w.walkObject(val, path)
	case []any:
		return w.walkArray(val, path)
	case string:
		// Delegate string classification to the shared inference used by
		// validation so discovery and validation never diverge. This
		// content-sniffs ISO-8601 strings into a temporal subtype (LocalDate,
		// Year, …) and leaves every other string as String.
		return schema.NewLeafNode(schema.InferDataType(val)), nil
	case json.Number:
		return classifyNumber(val)
	case float64:
		return nil, fmt.Errorf("%s: walker received float64 value; callers must use json.UseNumber() decoding", path)
	case bool:
		return schema.NewLeafNode(schema.Boolean), nil
	case nil:
		return schema.NewLeafNode(schema.Null), nil
	default:
		return nil, fmt.Errorf("%s: unsupported type: %T", path, v)
	}
}

func (w *walker) walkObject(m map[string]any, path string) (*schema.ModelNode, error) {
	node := schema.NewObjectNode()
	for k, v := range m {
		// Refuse the key before it can become a schema field. This is the one
		// point both field-set-establishing ingresses share, so enforcing here
		// covers the explicit model import and the ChangeLevel-driven
		// extension on an entity write alike — see ErrInvalidFieldName.
		if err := validateFieldName(path, k); err != nil {
			return nil, err
		}
		child, err := w.walkValue(v, path+"."+k)
		if err != nil {
			return nil, err
		}
		node.SetChild(k, child)
	}
	return node, nil
}

func classifyNumber(n json.Number) (*schema.ModelNode, error) {
	d, err := schema.ParseDecimal(n.String())
	if err != nil {
		return nil, fmt.Errorf("classify number %q: %w", n.String(), err)
	}
	stripped := d.StripTrailingZeros()
	// Value-based classification (spec §2.3): any whole-number value routes
	// to the integer branch regardless of source syntax. After
	// StripTrailingZeros, scale <= 0 means the value is a whole number:
	//   scale == 0 → unscaled itself (e.g. 42, "1.0" → 1)
	//   scale <  0 → unscaled × 10^(-scale) (e.g. "100" → (1,-2) → 100;
	//                "1e400" → (1,-400) → 10^400)
	// Only a positive scale after stripping indicates a genuine fractional
	// component that must go through ClassifyDecimal.
	if stripped.Scale() <= 0 {
		unscaled := stripped.Unscaled()
		if s := stripped.Scale(); s < 0 {
			mult := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(-s)), nil)
			unscaled = new(big.Int).Mul(unscaled, mult)
		}
		return schema.NewLeafNode(schema.ClassifyInteger(unscaled)), nil
	}
	return schema.NewLeafNode(schema.ClassifyDecimal(stripped)), nil
}

func (w *walker) walkArray(arr []any, path string) (*schema.ModelNode, error) {
	if len(arr) == 0 {
		return schema.NewArrayNode(schema.NewLeafNode(schema.Null)), nil
	}
	// One "[*]" hop per array level, matching the FieldsMap key spelling.
	elemPath := path + "[*]"
	var element *schema.ModelNode
	for _, item := range arr {
		child, err := w.walkValue(item, elemPath)
		if err != nil {
			return nil, err
		}
		if element == nil {
			element = child
			continue
		}
		element = schema.Merge(element, child)
	}
	node := schema.NewArrayNode(element)
	node.ObserveArrayWidth(len(arr))
	return node, nil
}
