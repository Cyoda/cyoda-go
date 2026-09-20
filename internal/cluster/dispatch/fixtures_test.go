package dispatch

import (
	"context"
	"encoding/json"
	"sync"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	cepb "github.com/cyoda-platform/cyoda-go/api/grpc/cloudevents"
	"github.com/cyoda-platform/cyoda-go/internal/contract"
	internalgrpc "github.com/cyoda-platform/cyoda-go/internal/grpc"
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

// scriptedCnode is a compute member of a real MemberRegistry: it records the
// pass of every request it is sent and answers each one with success, so a test
// can run the real local procedure over it and read what the pass says.
type scriptedCnode struct {
	mu     sync.Mutex
	passes []string
}

func (c *scriptedCnode) onlyPass(t *testing.T) string {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.passes) != 1 {
		t.Fatalf("the cnode was sent %d requests, want 1", len(c.passes))
	}
	return c.passes[0]
}

// attachedCnode registers one scripted cnode for tenantID under tag, on a
// registry of its own.
func attachedCnode(t *testing.T, tenantID spi.TenantID, tag string) (*internalgrpc.MemberRegistry, *scriptedCnode) {
	t.Helper()
	const id = "cnode-1"
	reg := internalgrpc.NewMemberRegistry()
	cnode := &scriptedCnode{}
	member := reg.Register(id, tenantID, []string{tag}, func(ce *cepb.CloudEvent) error {
		_, payload, err := internalgrpc.ParseCloudEvent(ce)
		if err != nil {
			t.Errorf("ParseCloudEvent: %v", err)
			return nil
		}
		var body struct {
			RequestID string `json:"requestId"`
		}
		if err := json.Unmarshal(payload, &body); err != nil {
			t.Errorf("request payload: %v", err)
			return nil
		}
		func() {
			cnode.mu.Lock()
			defer cnode.mu.Unlock()
			cnode.passes = append(cnode.passes, internalgrpc.TxTokenFromCloudEvent(ce))
		}()
		if m := reg.Get(id); m != nil {
			m.CompleteRequest(body.RequestID, &internalgrpc.ProcessingResponse{
				Success: true, Payload: json.RawMessage(`{"data":{"by":"` + id + `"}}`)})
		}
		return nil
	}, nil)
	t.Cleanup(func() { reg.Unregister(member) })
	return reg, cnode
}
