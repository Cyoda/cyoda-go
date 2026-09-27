package sqlite

import (
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// zeroStagedMarks must never mutate the caller's own op.after: ClaimDue
// builds its returned claims from the same *spi.ScheduledTask it staged,
// and needs UnsafeMarked as it read it during the candidate scan (under the
// same commit gate as the claim itself), not zeroed out from under it by a
// hygiene pass meant only for what gets staged/committed internally.
func TestZeroStagedMarks_DoesNotMutateTheCallersPostImage(t *testing.T) {
	orig := &spi.ScheduledTask{UnsafeMarked: true}
	ops := []scheduledTaskOp{{key: taskKey{tenant: "t", id: "i"}, after: orig}}

	zeroStagedMarks(ops)

	if !orig.UnsafeMarked {
		t.Fatal("zeroStagedMarks mutated the caller's own op.after in place; it must operate on a copy")
	}
}
