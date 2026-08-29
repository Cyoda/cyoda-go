package schema

import spi "github.com/cyoda-platform/cyoda-go-spi"

// The precise type-comparison core (DataType, Decimal, numeric classifiers,
// TypeSet, assignability) now lives in the SPI so the search leaf-comparison
// kernel can reach it (the SPI module cannot import this internal package).
// These aliases preserve the schema.X surface for the model-tree/discovery
// code that still lives here.

// ModelNode and the branch types are aliases for the same reason: the SPI
// carries the READ side of a model's schema — the node, its wire codec and the
// flattening into a fields map — so a storage backend that self-executes a
// search can go from the bytes it holds to declared types without importing
// the engine. Two structurally identical copies drift, and these two had:
// the SPI's decoder read one kind label and dropped the other branches a
// union's payload carried, so a predicate on a dropped path declared no type
// and matched nothing.
//
// What stays here is the WRITE side — Merge, Extend, Diff, Apply, Validate,
// the op catalog, unique-key derivation. Only the engine decides what a
// model's schema becomes; the SPI's mutators exist so the engine can build one.
type ModelNode = spi.ModelNode

// NodeKind names one branch a [ModelNode] can carry.
type NodeKind = spi.NodeKind

// Branch and the three branch types: a node holds the SET of kinds a path has
// been observed as, and each branch records what that observation carried.
type (
	Branch       = spi.Branch
	ScalarBranch = spi.ScalarBranch
	ObjectBranch = spi.ObjectBranch
	ArrayBranch  = spi.ArrayBranch
)

// The NodeKind members.
const (
	KindLeaf   = spi.KindLeaf
	KindObject = spi.KindObject
	KindArray  = spi.KindArray
)

// Node constructors and the wire codec.
var (
	NewObjectNode = spi.NewObjectNode
	NewLeafNode   = spi.NewLeafNode
	NewArrayNode  = spi.NewArrayNode

	// Marshal serializes a ModelNode tree to the persisted JSON bytes.
	Marshal = spi.MarshalModelNode
	// Unmarshal deserializes persisted JSON bytes into a ModelNode tree.
	Unmarshal = spi.UnmarshalModelNode
)

// DataType and its members.
type DataType = spi.DataType

// FieldDescriptor is an alias for [spi.FieldDescriptor], for the same reason
// DataType is: the search leaf-comparison kernel and spi.ConditionToFilter both
// consume it, and a storage plugin that self-executes a search must be able to
// build one. Aliasing rather than redeclaring means there is one definition —
// two structurally identical copies drift, and this one already did.
type FieldDescriptor = spi.FieldDescriptor

const (
	Integer        = spi.Integer
	Long           = spi.Long
	BigInteger     = spi.BigInteger
	UnboundInteger = spi.UnboundInteger
	Double         = spi.Double
	BigDecimal     = spi.BigDecimal
	UnboundDecimal = spi.UnboundDecimal
	String         = spi.String
	Character      = spi.Character
	LocalDate      = spi.LocalDate
	LocalDateTime  = spi.LocalDateTime
	LocalTime      = spi.LocalTime
	ZonedDateTime  = spi.ZonedDateTime
	Year           = spi.Year
	YearMonth      = spi.YearMonth
	UUIDType       = spi.UUIDType
	TimeUUIDType   = spi.TimeUUIDType
	ByteArray      = spi.ByteArray
	Boolean        = spi.Boolean
	Null           = spi.Null
)

// ParseDataType resolves a DataType by name.
var ParseDataType = spi.ParseDataType

// TypeSet is a sorted, deduplicated set of DataTypes.
type TypeSet = spi.TypeSet

// NewTypeSet, NumericFamily, NumericRank, and Union operate on TypeSet /
// DataType values.
var (
	NewTypeSet    = spi.NewTypeSet
	NumericFamily = spi.NumericFamily
	NumericRank   = spi.NumericRank
	Union         = spi.Union
)

// Decimal and its constructor.
type Decimal = spi.Decimal

var ParseDecimal = spi.ParseDecimal

// Numeric classification and assignability.
var (
	IsNumeric       = spi.IsNumeric
	ClassifyInteger = spi.ClassifyInteger
	ClassifyDecimal = spi.ClassifyDecimal
	IsAssignableTo  = spi.IsAssignableTo
	CollapseNumeric = spi.CollapseNumeric
)

// ClassifyTemporalString reports the most specific temporal DataType an
// ISO-8601 string parses as (or false if it is not temporal). Model discovery
// and leaf-value validation use it so a data field whose sample values are
// date-shaped strings is classified as a temporal subtype — the same
// classification the search leaf kernel applies to stored temporal values.
var ClassifyTemporalString = spi.ClassifyTemporalString
