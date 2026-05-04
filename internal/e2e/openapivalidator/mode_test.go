package openapivalidator

import "testing"

// TestModeIsEnforce pins the validator's Mode constant. Any future PR that
// flips back to ModeRecord must also remove or change this test — visible
// in code review, not silent.
func TestModeIsEnforce(t *testing.T) {
	if Mode != ModeEnforce {
		t.Fatalf("Mode = %v; expected ModeEnforce on main. Re-flipping to ModeRecord requires explicit PR review.", Mode)
	}
}
