package grpc_test

import (
	"time"

	"github.com/cyoda-platform/cyoda-go/internal/callout"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/contract"
	internalgrpc "github.com/cyoda-platform/cyoda-go/internal/grpc"
	"github.com/cyoda-platform/cyoda-go/internal/platformclock"
	"github.com/cyoda-platform/cyoda-go/internal/txcoord"
)

func init() {
	internalgrpc.NewOwnerForTest = func(local *internalgrpc.ProcessorDispatcher, members *internalgrpc.MemberRegistry, c txcoord.Coordination, cfg internalgrpc.OwnerTestConfig) contract.ExternalProcessingService {
		return callout.New(local, members, nil, c, common.NewDefaultUUIDGenerator(), platformclock.Real(), callout.Config{
			SelfNodeID:        "node-test",
			FixedNumRetries:   cfg.FixedNumRetries,
			Patience:          cfg.Patience,
			HandoverAllowance: 30 * time.Second,
		})
	}
}
