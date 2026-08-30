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
// operand parsing, type bucketing, regex compilation, and the filter-path
// parse (spi.ParseFilterPath) — into an immutable tree. Match then walks that
// tree per row, resolving each leaf's parsed hops against the row's own data
// through spi.ResolvePath — the one resolver the SPI kernel's PreparedFilter
// also calls (docs/cloud-parity/path-grammar.md section 10).
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
	prepLeaf         // data leaf, addressed by a parsed filter path
	prepMetaString   // lifecycle leaf on a string-valued meta field
	prepMetaTemporal // lifecycle leaf on creationDate / lastUpdateTime
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
	hops []spi.PathHop

	// prepMetaString / prepMetaTemporal
	metaField string

	// prepLeaf / prepMetaString / prepMetaTemporal
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

// prepare desugars cond before dispatching on its shape, so no evaluator
// below this point ever sees a *predicate.ArrayCondition — spi.DesugarCondition
// rewrites it into an AND of positional EQUALS leaves (see that function's
// doc). This is the same call spi.ConditionToFilter makes at its own entry
// point; one implementation, two entry points, so a bracket-indexed array
// clause means the same thing under a pushdown and under this evaluator.
func prepare(cond predicate.Condition, fieldTypes FieldTypes) (prepNode, error) {
	switch c := spi.DesugarCondition(cond).(type) {
	case *predicate.SimpleCondition:
		return prepareSimple(c, fieldTypes)
	case *predicate.LifecycleCondition:
		return prepareLifecycle(c)
	case *predicate.GroupCondition:
		return prepareGroup(c, fieldTypes)
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
		hops, err := spi.ParseFilterPath(stripLeader(c.JsonPath))
		if err != nil {
			// A path outside the grammar never resolves to anything — the
			// same "never matches" a leaf whose expansion failed already
			// produces, and the same answer the SPI kernel gives a malformed
			// SourceData path (prepareNode in prepared_filter.go). Do not
			// promote this to a Prepare error: only a structural property of
			// the CONDITION does that, and the path is validated at the
			// boundary before a condition ever reaches this evaluator.
			return prepNode{kind: prepNever}, nil
		}
		n.hops = hops
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
		//
		// KNOWN DIVERGENCE, deliberately not resolved here. The Filter-side
		// evaluator has no such guard: a text or pattern operator on a temporal
		// meta field reaches its string branch there and matches against the
		// RFC3339 rendering, so the same request answers differently depending
		// on whether the query pushes down.
		//
		// Do NOT resolve this by aligning either evaluator. A text or pattern
		// operator on a temporal field is not a supported predicate, and the
		// resolution is to refuse it at the shared validation boundary, which
		// makes both evaluators' behaviour unreachable. Aligning here would
		// specify semantics for a predicate that is being withdrawn.
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
		// The path's SYNTAX decides what it addresses, never the stored
		// value's shape (docs/cloud-parity/path-grammar.md section 3): a bare
		// hop resolves to the value there, whatever its shape, unwrapped
		// never; a "[*]" hop resolves to that array's elements, and to
		// nothing when the value there is not an array. ResolvePath is the
		// one resolver that encodes this — the same one the SPI kernel's
		// PreparedFilter.Match calls — so this evaluator cannot answer a
		// bare-vs-wildcard path differently than a pushed-down query would.
		// A leaf holds when SOME addressed value satisfies it (existential).
		for _, r := range spi.ResolvePath(data, n.hops) {
			if spi.EvalLeaf(n.exp, r) {
				return true
			}
		}
		return false

	case prepMetaString:
		return spi.EvalLeaf(n.exp, metaStringResult(n.metaField, meta))

	case prepMetaTemporal:
		return spi.EvalLeaf(n.exp, metaTemporalResult(n.metaField, meta))
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
