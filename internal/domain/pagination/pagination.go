// Package pagination holds the validation rules shared by paginated HTTP and
// gRPC endpoints, so every entry point that computes
// `offset = pageNumber * pageSize` enforces the same caps and catches the same
// int64 overflow before reaching storage.
package pagination

import (
	"fmt"
	"math"
	"math/bits"
	"net/http"

	"github.com/cyoda-platform/cyoda-go/internal/common"
)

const (
	// MaxPageSize caps sync and async pagination limits. Attacker-supplied
	// values above this would let a single request pull an unreasonable
	// volume of data.
	MaxPageSize = 10000
	// MaxPageNumber caps pageNumber. Even when the offset multiplication
	// fits in int64, an absurd pageNumber by itself is a sign of misuse:
	// with the maximum allowed pageSize (10000), a result set that fills
	// MaxInt32 pages would contain ~2.1e13 entities — orders of magnitude
	// beyond any realistic snapshot. Capping pageNumber independently
	// catches this earlier than the overflow guard alone.
	MaxPageNumber = math.MaxInt32 / MaxPageSize
)

// ValidateOffset rejects pagination parameters that are negative, exceed
// the per-request caps, or whose product overflows int64. Inputs are int64
// because the offending entry points come from int32 (HTTP/OpenAPI) or
// untyped JSON-int (gRPC); widening to int64 lets callers pass the raw
// value without first narrowing it and losing the overflow information.
//
// Returns a 400 *common.AppError on violation, nil otherwise.
func ValidateOffset(pageNumber, pageSize int64) error {
	if pageNumber < 0 {
		return common.Operational(http.StatusBadRequest, common.ErrCodeBadRequest, "invalid pageNumber")
	}
	if pageSize < 0 {
		return common.Operational(http.StatusBadRequest, common.ErrCodeBadRequest, "invalid pageSize")
	}
	if pageSize > MaxPageSize {
		return common.Operational(http.StatusBadRequest, common.ErrCodeBadRequest,
			fmt.Sprintf("pageSize exceeds maximum %d", MaxPageSize))
	}
	if pageNumber > MaxPageNumber {
		return common.Operational(http.StatusBadRequest, common.ErrCodeBadRequest,
			fmt.Sprintf("pageNumber exceeds maximum %d", MaxPageNumber))
	}
	// Defense-in-depth: even with both caps enforced, prove the offset
	// fits in int64 before any caller multiplies. bits.Mul64 is the
	// platform-independent way to detect overflow: hi != 0 means the
	// product does not fit in uint64; lo > MaxInt64 means it fits in
	// uint64 but overflows int64.
	hi, lo := bits.Mul64(uint64(pageNumber), uint64(pageSize))
	if hi != 0 || lo > math.MaxInt64 {
		return common.Operational(http.StatusBadRequest, common.ErrCodeBadRequest, "pageNumber*pageSize overflows int64")
	}
	return nil
}
