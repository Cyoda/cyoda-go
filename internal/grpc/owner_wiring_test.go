package grpc_test

import (
	"time"

	"github.com/cyoda-platform/cyoda-go/internal/callout"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/contract"
	"github.com/cyoda-platform/cyoda-go/internal/fence"
	internalgrpc "github.com/cyoda-platform/cyoda-go/internal/grpc"
)

func init() {
	internalgrpc.NewOwnerForTest = func(local *internalgrpc.ProcessorDispatcher, members *internalgrpc.MemberRegistry, f *fence.Fence, cfg internalgrpc.OwnerTestConfig) contract.ExternalProcessingService {
		return callout.New(local, members, nil, f, common.NewDefaultUUIDGenerator(), callout.Config{
			SelfNodeID:        "node-test",
			FixedNumRetries:   cfg.FixedNumRetries,
			Patience:          cfg.Patience,
			HandoverAllowance: 30 * time.Second,
		})
	}
}
