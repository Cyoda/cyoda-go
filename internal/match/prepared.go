package match

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/tidwall/gjson"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go-spi/predicate"
)

// prepared.go is the prepare/execute split of the predicate-tree evaluator.
// Prepare resolves everything that depends only on the query — declared types,
// operand parsing, type bucketing, regex compilation, and the gjson path
// conversion — into an immutable tree. Match then walks that tree per row.
//
// Prepare returns an error where the Filter-side spi.Prepare does not, because
// the two consume different input types: spi.FilterOp is a closed enum, while
// predicate.Condition carries free-text operator and field names that can name
// nothing.
//
// Errors are structural properties of the CONDITION, never of the row. Every
// row-dependent failure stays a non-match, exactly as before.

// errUnsupportedOperator marks an operator NAME with no kernel op — a
// structural fault that fails Prepare. It is deliberately distinct from an
// expansion failure (an operand that parses into no declared type), which is a
// leaf that never matches. Collapsing the two would reject conditions that
// evaluate cleanly today.
var errUnsupportedOperator = errors.New("unsupported operator")

// prepKind discriminates the prepared node shapes.
type prepKind int

const (
	// prepNever is the ZERO VALUE on purpose: an unpopulated node, and the
	// zero Prepared that Prepare returns alongside an error, must fail closed.
	prepNever prepKind = iota
	prepGroup
	prepLeaf         // data leaf, addressed by a gjson path
	prepMetaString   // lifecycle leaf on a string-valued meta field
	prepMetaTemporal // lifecycle leaf on creationDate / lastUpdateTime
	prepArray
)

// Prepared is a predicate.Condition compiled for repeated evaluation. Build it
// once per query with Prepare, then call Match once per candidate row. It is
// immutable after Prepare returns and safe to share across goroutines.
//
// The zero Prepared never matches.
type Prepared struct {
	root prepNode
}

type prepNode struct {
	kind prepKind

	// prepGroup
	or       bool
	children []prepNode

	// prepLeaf
	gjsonPath string

	// prepMetaString / prepMetaTemporal
	metaField string

	// prepLeaf / prepMetaString / prepMetaTemporal
	exp spi.Expansion

	// prepArray
	arrayBase string
	positions []arrayPos
}

// arrayPos is one positional EQUALS of an ArrayCondition. Each position has
// its own operand and therefore its own expansion — one expansion per leaf
// would be wrong here.
type arrayPos struct {
	idx int
	exp spi.Expansion
}

// Prepare compiles cond against the declared types fieldTypes supplies.
//
// fieldTypes is consumed during preparation and never retained. Callers whose
// closure mutates captured state (the workflow engine's does) rely on this:
// calling it once, on one goroutine, is what makes the result safe to share.
//
// Declared types are resolved for exactly the leaf set the pre-split evaluator
// resolved — every SimpleCondition and every ArrayCondition, whatever the
// operator, not only the comparison and range leaves that consume them.
// Narrowing the set would stop resolving types for leaves that resolve them
// today, and those are precisely the leaves whose lookup failure currently
// fails a criterion closed. Resolving fewer would be a fail-open movement on
// the write path.
func Prepare(cond predicate.Condition, fieldTypes FieldTypes) (Prepared, error) {
	n, err := prepare(cond, fieldTypes)
	if err != nil {
		return Prepared{}, err
	}
	return Prepared{root: n}, nil
}

func prepare(cond predicate.Condition, fieldTypes FieldTypes) (prepNode, error) {
	switch c := cond.(type) {
	case *predicate.SimpleCondition:
		return prepareSimple(c, fieldTypes)
	case *predicate.LifecycleCondition:
		return prepareLifecycle(c)
	case *predicate.GroupCondition:
		return prepareGroup(c, fieldTypes)
	case *predicate.ArrayCondition:
		return prepareArray(c, fieldTypes)
	case *predicate.FunctionCondition:
		return prepNode{}, fmt.Errorf("function conditions not implemented")
	default:
		return prepNode{}, fmt.Errorf("unknown condition type: %T", cond)
	}
}

// expandNamed maps an operator NAME to its kernel op and expands the operand.
// A name with no kernel op is a structural fault; anything else the kernel
// rejects is an expansion failure the caller turns into a never-match leaf.
func expandNamed(operatorType string, value any, declared []spi.DataType) (spi.Expansion, error) {
	op, ok := opNameToFilterOp(operatorType)
	if !ok {
		return spi.Expansion{}, fmt.Errorf("%w: %s", errUnsupportedOperator, operatorType)
	}
	var values []string
	if op == spi.FilterBetween || op == spi.FilterBetweenInclusive {
		values = betweenBounds(value)
	}
	return spi.ExpandLeaf(op, spi.OperandString(value), values, declared)
}

// leafNode builds a prepared leaf of the given kind, or a never-match node when
// expansion fails. It propagates only the structural fault.
func leafNode(kind prepKind, operatorType string, value any, declared []spi.DataType) (prepNode, error) {
	exp, err := expandNamed(operatorType, value, declared)
	if err != nil {
		if errors.Is(err, errUnsupportedOperator) {
			return prepNode{}, err
		}
		// An operand that parses into no declared type, or a malformed range
		// arity, is a leaf that never matches — the swallowed expansion error
		// the per-row evaluator produced, made explicit.
		return prepNode{kind: prepNever}, nil
	}
	return prepNode{kind: kind, exp: exp}, nil
}

func prepareSimple(c *predicate.SimpleCondition, fieldTypes FieldTypes) (prepNode, error) {
	var declared []spi.DataType
	if fieldTypes != nil {
		declared = fieldTypes(fieldMapKey(c.JsonPath))
	}
	n, err := leafNode(prepLeaf, c.OperatorType, c.Value, declared)
	if err != nil {
		return prepNode{}, err
	}
	if n.kind == prepLeaf {
		n.gjsonPath = convertJSONPath(c.JsonPath)
	}
	return n, nil
}

func prepareLifecycle(c *predicate.LifecycleCondition) (prepNode, error) {
	// Canonicalise BEFORE the field check, or previousTransition — a working
	// field name — starts erroring.
	field := c.Field
	if field == "previousTransition" {
		field = "transitionForLatestSave"
	}

	switch field {
	case "creationDate", "lastUpdateTime":
		// Field-identity guard, sitting in FRONT of the operator check: a
		// temporal field admits only comparison, range and null operators, and
		// anything else is a never-match leaf rather than an error. It must
		// never lexically substring-match the formatted RFC3339 rendering.
		if !isTemporalOperator(c.OperatorType) {
			return prepNode{kind: prepNever}, nil
		}
		n, err := leafNode(prepMetaTemporal, c.OperatorType, c.Value, []spi.DataType{spi.ZonedDateTime})
		if err != nil {
			return prepNode{}, err
		}
		if n.kind == prepMetaTemporal {
			n.metaField = field
		}
		return n, nil

	case "state", "transitionForLatestSave", "transactionId", "id":
		n, err := leafNode(prepMetaString, c.OperatorType, c.Value, []spi.DataType{spi.String})
		if err != nil {
			return prepNode{}, err
		}
		if n.kind == prepMetaString {
			n.metaField = field
		}
		return n, nil

	default:
		// Reported with the ORIGINAL field name, not the canonicalised one.
		return prepNode{}, fmt.Errorf("unknown lifecycle field: %s", c.Field)
	}
}

func prepareGroup(c *predicate.GroupCondition, fieldTypes FieldTypes) (prepNode, error) {
	var or bool
	switch c.Operator {
	case "AND":
	case "OR":
		or = true
	default:
		return prepNode{}, fmt.Errorf("unknown group operator: %s", c.Operator)
	}

	n := prepNode{kind: prepGroup, or: or}
	if len(c.Conditions) > 0 {
		n.children = make([]prepNode, 0, len(c.Conditions))
		for _, child := range c.Conditions {
			cn, err := prepare(child, fieldTypes)
			if err != nil {
				return prepNode{}, err
			}
			n.children = append(n.children, cn)
		}
	}
	return n, nil
}

func prepareArray(c *predicate.ArrayCondition, fieldTypes FieldTypes) (prepNode, error) {
	var declared []spi.DataType
	if fieldTypes != nil {
		declared = fieldTypes(arrayElementFieldPath(c.JsonPath))
	}

	n := prepNode{kind: prepArray, arrayBase: convertJSONPath(c.JsonPath)}
	for i, expected := range c.Values {
		if expected == nil {
			continue // nil positions are skipped
		}
		// EQUALS always maps to a kernel op, so expandNamed can only fail here
		// with an expansion error — which made the whole array condition false
		// for every row, since matchArray returned on the first failing
		// position regardless of data.
		exp, err := expandNamed("EQUALS", expected, declared)
		if err != nil {
			return prepNode{kind: prepNever}, nil
		}
		n.positions = append(n.positions, arrayPos{idx: i, exp: exp})
	}
	return n, nil
}

// Match reports whether the entity satisfies the prepared condition. It cannot
// fail: every way this evaluation could error is a structural property of the
// condition and was already reported by Prepare.
func (p Prepared) Match(data []byte, meta spi.EntityMeta) bool {
	return p.root.match(data, meta)
}

func (n *prepNode) match(data []byte, meta spi.EntityMeta) bool {
	switch n.kind {
	case prepGroup:
		if n.or {
			for i := range n.children {
				if n.children[i].match(data, meta) {
					return true
				}
			}
			return false
		}
		for i := range n.children {
			if !n.children[i].match(data, meta) {
				return false
			}
		}
		return true

	case prepLeaf:
		result := gjson.GetBytes(data, n.gjsonPath)
		// Routing on the DATA's shape, not the condition's, stays per row: an
		// array-wildcard path yields an array for one entity and nothing for
		// the next. Both branches consume the same expansion, which is why
		// hoisting the expansion is safe while the routing is not.
		if result.IsArray() {
			matched := false
			result.ForEach(func(_, v gjson.Result) bool {
				if spi.EvalLeaf(n.exp, v) {
					matched = true
					return false // short-circuit
				}
				return true
			})
			return matched
		}
		return spi.EvalLeaf(n.exp, result)

	case prepMetaString:
		return spi.EvalLeaf(n.exp, metaStringResult(n.metaField, meta))

	case prepMetaTemporal:
		return spi.EvalLeaf(n.exp, metaTemporalResult(n.metaField, meta))

	case prepArray:
		for _, pos := range n.positions {
			r := gjson.GetBytes(data, fmt.Sprintf("%s.%d", n.arrayBase, pos.idx))
			if !spi.EvalLeaf(pos.exp, r) {
				return false
			}
		}
		return true
	}

	// prepNever, and any unpopulated node.
	return false
}

// metaStringResult wraps a string-valued meta field in a one-key document and
// reads it back, so meta string comparison goes through the same kernel as
// data leaves with a declared String type.
func metaStringResult(field string, meta spi.EntityMeta) gjson.Result {
	var v string
	switch field {
	case "state":
		v = meta.State
	case "transitionForLatestSave":
		v = meta.TransitionForLatestSave
	case "transactionId":
		v = meta.TransactionID
	case "id":
		v = meta.ID
	}
	return gjson.Get(fmt.Sprintf(`{"v":%q}`, v), "v")
}

// metaTemporalResult bridges a stored meta instant to a gjson.Result. A zero
// time (unset) bridges to an ABSENT result, not to a comparable ~year-1
// instant: IS_NULL matches it and every binary op, negatives included,
// non-matches under the kernel's null uniformity.
func metaTemporalResult(field string, meta spi.EntityMeta) gjson.Result {
	var t time.Time
	switch field {
	case "creationDate":
		t = meta.CreationDate
	case "lastUpdateTime":
		t = meta.LastModifiedDate
	}
	if t.IsZero() {
		return gjson.Result{}
	}
	b, err := json.Marshal(t)
	if err != nil {
		return gjson.Result{}
	}
	return gjson.ParseBytes(b)
}
