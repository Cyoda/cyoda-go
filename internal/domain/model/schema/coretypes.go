package schema

import spi "github.com/cyoda-platform/cyoda-go-spi"

// The precise type-comparison core (DataType, Decimal, numeric classifiers,
// TypeSet, assignability) now lives in the SPI so the search leaf-comparison
// kernel can reach it (the SPI module cannot import this internal package).
// These aliases preserve the schema.X surface for the model-tree/discovery
// code that still lives here.

// DataType and its members.
type DataType = spi.DataType

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
