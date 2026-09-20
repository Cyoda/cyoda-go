package dispatch

import (
	"context"
	"encoding/json"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/contract"
)

// stubNodeRegistry returns a fixed list of nodes.
type stubNodeRegistry struct {
	nodes []contract.NodeInfo
}

func (r *stubNodeRegistry) Register(_ context.Context, _ string, _ string) error { return nil }
func (r *stubNodeRegistry) Lookup(_ context.Context, _ string) (string, bool, error) {
	return "", false, nil
}
func (r *stubNodeRegistry) List(_ context.Context) ([]contract.NodeInfo, error) {
	return r.nodes, nil
}
func (r *stubNodeRegistry) Deregister(_ context.Context, _ string) error { return nil }
func (r *stubNodeRegistry) Changed() <-chan struct{}                     { return nil }

// testContext builds a context with UserContext set.
func testContext() context.Context {
	uc := &spi.UserContext{
		UserID: "user-1",
		Kind:   spi.PrincipalUser,
		Tenant: spi.Tenant{ID: "tenant-1", Name: "Test Tenant"},
		Roles:  []string{"ROLE_USER"},
	}
	return spi.WithUserContext(context.Background(), uc)
}

// testEntity builds a minimal entity for dispatch tests.
func testEntity() *spi.Entity {
	return &spi.Entity{
		Meta: spi.EntityMeta{
			ID:       "entity-1",
			TenantID: "tenant-1",
			ModelRef: spi.ModelRef{EntityName: "TestModel", ModelVersion: "1"},
			State:    "OPEN",
		},
		Data: []byte(`{"key":"value"}`),
	}
}

// testProcessor builds a processor with calculationNodesTags="python".
func testProcessor() spi.ProcessorDefinition {
	return spi.ProcessorDefinition{
		Type: "function",
		Name: "myProcessor",
		Config: spi.ProcessorConfig{
			AttachEntity:         true,
			CalculationNodesTags: "python",
		},
	}
}

// testCriterion builds criterion JSON with calculationNodesTags="python".
func testCriterion() json.RawMessage {
	return json.RawMessage(`{"type":"function","function":{"name":"myCriteria","config":{"calculationNodesTags":"python","attachEntity":true}}}`)
}

// testFunction builds a ScheduleFunction with calculationNodesTags="python".
func testFunction() spi.ScheduleFunction {
	return spi.ScheduleFunction{
		Name:                 "myScheduleFn",
		ResultKind:           "Schedule",
		CalculationNodesTags: "python",
		AttachEntity:         true,
	}
}
