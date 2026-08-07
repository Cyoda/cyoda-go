package search

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go-spi/predicate"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/domain/model/schema"
)

// ValidateConditionValueTypes walks a condition tree and checks that each
// simple clause's operand PARSES into at least one of the field's declared
// types — the same type-directed parse the leaf-comparison kernel
// (spi.ExpandLeaf) performs at evaluation time. This replaces the older
// JSON-kind-vs-DataType assignability check and its operator-class matrix:
// there is no operator-vs-field-type rejection anymore. CONTAINS on a numeric
// field, GREATER_THAN "true" on a boolean, a numeric-looking string on a
// [INTEGER, STRING] field — all parse and are ACCEPTED; the kernel evaluates
// them to a (non-)match, never a type error (spec §6).
//
// The model's FieldsMap provides a lookup from JSONPath (e.g. "$.price") to
// a FieldDescriptor carrying the observed DataType(s). Conditions referencing
// unknown paths are accepted (the condition may traverse a path not yet seen
// in training data); a field with no declared types carries no constraint.
//
// Returns a non-nil error only when an operand parses into NONE of the
// field's declared types (errConditionTypeMismatch), or a lifecycle field is
// unknown (errInvalidFieldPath). Operand shape/arity — an object operand
// (never valid for any operator), null on a binary op, a range op's
// 2-element bounds shape — is enforced separately by
// ValidateCondition/validateOperandShape/validateBetweenArity, upstream of
// this type check.
func ValidateConditionValueTypes(model *schema.ModelNode, cond predicate.Condition) error {
	if cond == nil {
		return nil
	}
	// fm stays nil when model is nil. walkConditionTypes/validateSimpleConditionType
	// gracefully skip the data-field-vs-schema check on a nil map (an
	// unknown-path lookup returns ok=false, the "accept" branch) — so the
	// only checks that still run without a model are the model-independent
	// ones: operator/BETWEEN-arity (via the caller's ValidateCondition) and
	// lifecycle/temporal type-soundness (validateLifecycleType below). This
	// lets callers with no schema plumbing (e.g. grouped-stats) reuse this
	// function for temporal/lifecycle validation by passing model=nil.
	var fm map[string]schema.FieldDescriptor
	if model != nil {
		fm = model.FieldsMap()
	}
	return walkConditionTypes(fm, cond, 0)
}

func walkConditionTypes(fm map[string]schema.FieldDescriptor, cond predicate.Condition, depth int) error {
	if cond == nil {
		return nil
	}
	if depth >= MaxConditionDepth {
		return fmt.Errorf("condition depth exceeded (max %d)", MaxConditionDepth)
	}
	switch c := cond.(type) {
	case *predicate.SimpleCondition:
		return validateSimpleConditionType(fm, c)
	case *predicate.GroupCondition:
		for _, child := range c.Conditions {
			if err := walkConditionTypes(fm, child, depth+1); err != nil {
				return err
			}
		}
		return nil
	case *predicate.LifecycleCondition:
		return validateLifecycleType(c)
	case *predicate.ArrayCondition, *predicate.FunctionCondition:
		return nil
	default:
		return nil
	}
}

func validateSimpleConditionType(fm map[string]schema.FieldDescriptor, c *predicate.SimpleCondition) error {
	// FieldsMap keys carry the "$." prefix; a condition may legitimately omit it
	// and still name a known field. Looking it up raw made the type check silently
	// skip such a leaf, so an operand that should be rejected 400
	// CONDITION_TYPE_MISMATCH was accepted and evaluated to an empty page instead.
	key := normalisePath(c.JsonPath)
	fd, ok := fm[key]
	if !ok {
		// Not a leaf. In a schema'd model (non-empty FieldsMap), a path that is
		// a KNOWN CONTAINER — a strict prefix of one or more leaf paths, but not
		// itself a leaf — has substructure and cannot be compared to a scalar:
		// you must navigate to a leaf sub-path. Reject a scalar-operand
		// comparison on such a path as INVALID_FIELD_PATH. Unary presence tests
		// (IS_NULL/NOT_NULL) carry no scalar operand — they test presence, not a
		// value — so they are NOT rejected. A mixed object-or-scalar node is a
		// leaf after schema field-collection (it carries its scalar types), so it
		// never reaches this branch. A genuinely-unknown (non-container) path
		// carries no type constraint here; the separate field-path validation
		// pass classifies it.
		if len(fm) > 0 && carriesScalarOperand(mapOperator(c.OperatorType)) && isKnownContainerPath(key, fm) {
			return fmt.Errorf("field %q is a container with substructure and cannot be compared to a scalar; navigate to a leaf sub-path: %w",
				c.JsonPath, errInvalidFieldPath)
		}
		// Unknown path — no type constraint here (INVALID_FIELD_PATH for data
		// leaves is raised by the separate field-path validation pass).
		return nil
	}
	if len(fd.Types) == 0 {
		// No declared types recorded — no constraint; accept.
		return nil
	}

	// An object operand is rejected upstream, at the model-independent shape
	// layer (validateOperandShape in operators.go, wired into ValidateCondition
	// — the single boundary every transport funnels through before this
	// type check runs) as INVALID_CONDITION, not here: it is a shape/arity
	// error (spec §6/§8), not a field-type mismatch.

	// Only the comparison/range family constrains the operand's type. String
	// operators and the null-presence tests parse any operand — they evaluate
	// to a (non-)match, never a type error (spec §6, parse-based).
	if !isParseConstrainedOp(mapOperator(c.OperatorType)) {
		return nil
	}

	switch v := c.Value.(type) {
	case nil:
		// Null operand carries no type to mismatch; arity is enforced elsewhere.
		return nil
	case []any:
		// Array operand — BETWEEN's [lo, hi] bounds or a legacy positional
		// (IN-style) set. Every non-null element must parse into a declared
		// type; a null element is compatible with any type; an empty array has
		// nothing to mismatch. Range-op arity (exactly two bounds) is enforced
		// by validateBetweenArity, not here.
		for i, elem := range v {
			if elem == nil {
				continue
			}
			if !operandParsesDeclared(fd.Types, elem) {
				return fmt.Errorf("value[%d] %v parses into none of field %q's declared types %v: %w",
					i, elem, c.JsonPath, fd.Types, errConditionTypeMismatch)
			}
		}
		return nil
	default:
		if !operandParsesDeclared(fd.Types, v) {
			return fmt.Errorf("operand %v parses into none of field %q's declared types %v: %w",
				v, c.JsonPath, fd.Types, errConditionTypeMismatch)
		}
		return nil
	}
}

// isParseConstrainedOp reports whether op's operand must parse into a declared
// type for the condition to be valid. Only the six comparison operators and the
// two range operators are constrained; string operators (CONTAINS, LIKE, the
// case-insensitive/negated variants, ...) and the null-presence tests (IS_NULL,
// NOT_NULL) parse any operand and are always accepted — mirroring the kernel,
// where ExpandLeaf only reports a "parses into no declared type" error for the
// compare (expandCompare) and range (expandBetween) families.
func isParseConstrainedOp(op spi.FilterOp) bool {
	switch op {
	case spi.FilterEq, spi.FilterNe, spi.FilterGt, spi.FilterGte, spi.FilterLt, spi.FilterLte,
		spi.FilterBetween, spi.FilterBetweenInclusive:
		return true
	}
	return false
}

// carriesScalarOperand reports whether op compares the field against a scalar
// operand (and therefore cannot address a container path). Every operator does
// EXCEPT the unary null-presence tests (IS_NULL, NOT_NULL), which test presence
// rather than a value and so remain valid on a container path.
func carriesScalarOperand(op spi.FilterOp) bool {
	switch op {
	case spi.FilterIsNull, spi.FilterNotNull:
		return false
	}
	return true
}

// isKnownContainerPath reports whether p names a KNOWN CONTAINER in fm — a
// strict prefix of one or more leaf paths, without being a leaf itself. It
// mirrors the prefix probe in path_validate.go's isPathKnown, but is used to
// REJECT a scalar comparison on the interior node rather than to accept the
// path. p is assumed absent from fm as a direct leaf (the caller checks that
// first). Both dot- and wildcard-delimited descents count as substructure.
func isKnownContainerPath(p string, fm map[string]schema.FieldDescriptor) bool {
	dotPrefix := p + "."
	arrPrefix := p + "["
	for known := range fm {
		if strings.HasPrefix(known, dotPrefix) || strings.HasPrefix(known, arrPrefix) {
			return true
		}
	}
	return false
}

// operandParsesDeclared reports whether a single scalar operand parses into at
// least one of the declared types. It calls the kernel's own comparison-parse
// (spi.ExpandLeaf with FilterEq): the parse decision — "does this operand
// denote a value of a declared type" — is operator-independent across the
// comparison family (expandCompare's engaged check depends only on the operand
// and declared set, not the operator), so FilterEq is a faithful oracle that
// also applies per array element, where a range operator cannot be expanded in
// isolation. The operand is normalised with spi.OperandString — the single
// shared operand→string form the evaluators (internal/match, spi.MatchFilter)
// feed the kernel — so validation and evaluation agree. A Void expansion
// (parses but every bucket dropped, e.g. EQUALS 12.5 on [INTEGER]) is NOT a
// mismatch — it is accepted and evaluates to non-match.
func operandParsesDeclared(declared []schema.DataType, v any) bool {
	_, err := spi.ExpandLeaf(spi.FilterEq, spi.OperandString(v), nil, declared)
	return err == nil
}

// errConditionTypeMismatch is the sentinel error for condition type mismatch.
// Handlers check errors.Is(err, errConditionTypeMismatch) to emit HTTP 400
// with ErrCodeConditionTypeMismatch.
var errConditionTypeMismatch = fmt.Errorf("condition type mismatch")

// errInvalidFieldPath is the sentinel error for a condition referencing a
// meta field path the vocabulary does not recognize. Handlers check
// errors.Is(err, errInvalidFieldPath) to emit HTTP 400 with
// ErrCodeInvalidFieldPath (distinct from errConditionTypeMismatch's
// CONDITION_TYPE_MISMATCH: the field itself is unknown, not merely
// type-incompatible with its operator/operand).
var errInvalidFieldPath = fmt.Errorf("invalid field path")

// ErrConditionTypeMismatch and ErrInvalidFieldPath are exported aliases of
// the sentinels above, letting other domain packages (e.g. entity's
// grouped-stats validation, which calls ValidateConditionValueTypes(nil, ...)
// for its own model-independent temporal/lifecycle type-soundness check)
// classify the returned error via errors.Is without duplicating the sentinel
// or depending on package-internal identifiers.
var (
	ErrConditionTypeMismatch = errConditionTypeMismatch
	ErrInvalidFieldPath      = errInvalidFieldPath
)

// metaTemporalDeclared is the declared type set for temporal meta fields
// (creationDate, lastUpdateTime): a single ZonedDateTime, matching
// lifecycleToFilter's stamping. A coarser operand (e.g. "2024", or an
// offset-less "2021-01-01T00:00:00") parses as its own natural subtype and
// upscales to ZonedDateTime (spec §4), so it parses into this set and is
// accepted.
var metaTemporalDeclared = []spi.DataType{spi.ZonedDateTime}

// validateLifecycleType enforces type-soundness for LifecycleCondition
// (meta) clauses, parse-based (spec §6):
//   - the field must be a known meta filter field (sortableMetaFields key,
//     or the previousTransition alias) — otherwise errInvalidFieldPath.
//   - for fields the meta vocabulary classifies as temporal (creationDate,
//     lastUpdateTime), a comparison/range operand must parse into a temporal
//     type — otherwise errConditionTypeMismatch. There is NO operator-class
//     rejection: a string operator (CONTAINS, ...) on a temporal field parses
//     and is accepted (the kernel evaluates it to a non-match), and a coarse
//     operand upscales rather than being rejected.
//
// Non-temporal meta fields (state, transitionForLatestSave, transactionId,
// id) carry no further constraint here: they compare as their stored
// text/string form regardless of operator.

// ValidateLifecycleCondition checks a lifecycle/meta condition for type
// soundness (known meta field; a comparison/range operand that parses into a
// temporal type on temporal fields). Shared by the search API boundary and
// workflow-criterion import so both reject the same malformed conditions.
// Returns a descriptive error; callers map it to their own 4xx code.
func ValidateLifecycleCondition(c *predicate.LifecycleCondition) error {
	return validateLifecycleType(c)
}

func validateLifecycleType(c *predicate.LifecycleCondition) error {
	if !isKnownMetaFilterField(c.Field) {
		return fmt.Errorf("unknown meta filter field %q: %w", c.Field, errInvalidFieldPath)
	}
	field := c.Field
	if field == "previousTransition" {
		field = "transitionForLatestSave"
	}
	if !isTemporalMetaField(field) {
		return nil
	}
	// String operators and null-presence tests on a temporal meta field parse
	// any operand and are accepted (eval decides a non-match) — spec §6.
	if !isParseConstrainedOp(mapOperator(c.OperatorType)) {
		return nil
	}
	for i, elem := range operandElements(c.Value) {
		if elem == nil {
			continue
		}
		if !operandParsesDeclared(metaTemporalDeclared, elem) {
			return fmt.Errorf("operand[%d] %v parses into no temporal type for field %q: %w",
				i, elem, c.Field, errConditionTypeMismatch)
		}
	}
	return nil
}

// operandElements normalises a condition value into its operand elements: a
// scalar becomes a single-element slice; a []any (BETWEEN's [lo, hi] pair, or a
// positional set) becomes one element per member. Callers skip nil elements.
func operandElements(v any) []any {
	if arr, ok := v.([]any); ok {
		return arr
	}
	return []any{v}
}

// loadModelNode fetches and parses the model schema for ref, returning the
// *schema.ModelNode used for condition-type validation. Returns nil when
// the store lookup fails, the descriptor has no schema bound, or the schema
// fails to parse — callers treat this as "no type constraints available"
// rather than failing the search on a schema-load hiccup. EnsureModelRegistered
// has already confirmed the model exists by the time this runs, so in the
// normal case the node is present.
func loadModelNode(ctx context.Context, store spi.ModelStore, ref spi.ModelRef) *schema.ModelNode {
	// Reuse the store's cached parse when it has one; see loadFieldsMap.
	if p, ok := store.(schemaNodeProvider); ok {
		node, err := p.SchemaNode(ctx, ref)
		if err != nil {
			return nil
		}
		return node
	}
	desc, err := store.Get(ctx, ref)
	if err != nil || desc == nil || len(desc.Schema) == 0 {
		return nil
	}
	node, err := schema.Unmarshal(desc.Schema)
	if err != nil {
		return nil
	}
	return node
}

// validateConditionTypes is the single boundary enforcing condition
// type-soundness for every SearchService entry point (HTTP, gRPC, and any
// future transport funnel through Search/SubmitAsync). It loads the model
// schema and delegates to ValidateConditionValueTypes, mapping the returned
// sentinel error to the appropriate 400-classified *common.AppError:
// errInvalidFieldPath → INVALID_FIELD_PATH (the field itself is unknown),
// anything else → CONDITION_TYPE_MISMATCH (the value is type-incompatible
// with a known field/operator).
//
// A schema-load hiccup (see loadModelNode) returns nil — the search proceeds
// without pre-rejecting type-unsound conditions rather than 5xx-ing on an infra
// flake. This is safe, not a wrong-but-available result: with no model the eval
// path stamps empty Declared too, so comparison leaves degrade to non-match
// (empty results, never a wrong match), and existence is already gated upstream
// by EnsureModelRegistered. It is deliberately more lenient here than the
// workflow engine (which fails closed on the same load error) — a search
// prefers empty-on-flake to a 5xx.
func (s *SearchService) validateConditionTypes(ctx context.Context, modelStore spi.ModelStore, modelRef spi.ModelRef, cond predicate.Condition) *common.AppError {
	node := loadModelNode(ctx, modelStore, modelRef)
	if node == nil {
		return nil
	}
	if err := ValidateConditionValueTypes(node, cond); err != nil {
		code := common.ErrCodeConditionTypeMismatch
		if errors.Is(err, errInvalidFieldPath) {
			code = common.ErrCodeInvalidFieldPath
		}
		return common.Operational(http.StatusBadRequest, code, err.Error())
	}
	return nil
}
