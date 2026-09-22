package grpc

import (
	"time"

	"github.com/cyoda-platform/cyoda-go/internal/contract"
	"github.com/cyoda-platform/cyoda-go/internal/fence"
)

// OwnerTestConfig is what an envelope test may set on the owner's loop.
type OwnerTestConfig struct {
	FixedNumRetries int
	Patience        time.Duration
}

// NewOwnerForTest builds the owner's loop (internal/callout.Coordinator) over a
// dispatcher and its registry. internal/callout imports this package, so the
// in-package tests cannot import it; owner_wiring_test.go, an external test
// file of this directory, sets this variable from its init.
var NewOwnerForTest func(local *ProcessorDispatcher, members *MemberRegistry, f *fence.Fence, cfg OwnerTestConfig) contract.ExternalProcessingService
