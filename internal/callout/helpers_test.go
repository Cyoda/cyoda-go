package callout

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	cepb "github.com/cyoda-platform/cyoda-go/api/grpc/cloudevents"
	"github.com/cyoda-platform/cyoda-go/internal/cluster/token"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/fence"
	internalgrpc "github.com/cyoda-platform/cyoda-go/internal/grpc"
	"github.com/cyoda-platform/cyoda-go/internal/txgate"
)

const (
	tenantA = spi.TenantID("tenant-a")
	tenantB = spi.TenantID("tenant-b")

	// limitMs is the answer limit the tests give a cnode that stays silent.
	limitMs = 60
)

func userCtx(tenant spi.TenantID) context.Context {
	return spi.WithUserContext(context.Background(), &spi.UserContext{
		UserID:   "user-1",
		UserName: "test-user",
		Kind:     spi.PrincipalUser,
		Tenant:   spi.Tenant{ID: tenant, Name: "Test Tenant"},
	})
}

func secret() []byte {
	b := make([]byte, 32)
	for i := range b {
		b[i] = byte(i + 1)
	}
	return b
}

// env is an owner over a real dispatcher, a real member registry and a real
// fence, the way app.go builds them.
type env struct {
	reg    *internalgrpc.MemberRegistry
	gate   *txgate.Registry
	fence  *fence.Fence
	signer *token.Signer
	owner  *Coordinator
}

func newEnv(t *testing.T, cfg Config) *env {
	t.Helper()
	signer, err := token.NewSigner(secret())
	if err != nil {
		t.Fatalf("token.NewSigner: %v", err)
	}
	reg := internalgrpc.NewMemberRegistry()
	gate := txgate.New()
	f := fence.New(gate)
	local := internalgrpc.NewProcessorDispatcher(reg, internalgrpc.NewRoundRobinSelector(reg),
		signer, 30*time.Second, 60*time.Second, 30*time.Second)
	if cfg.SelfNodeID == "" {
		cfg.SelfNodeID = "node-owner"
	}
	return &env{reg: reg, gate: gate, fence: f, signer: signer, owner: New(local, reg, nil, f, common.NewTestUUIDGenerator(), cfg)}
}

// cnode records what one scripted cnode was sent.
type cnode struct {
	mu         sync.Mutex
	requestIDs []string
	passes     []string
}

func (n *cnode) count() int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return len(n.requestIDs)
}

func (n *cnode) seen() (requestIDs, passes []string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]string(nil), n.requestIDs...), append([]string(nil), n.passes...)
}

// script is what a scripted cnode does with a request. A nil script takes the
// work and never answers.
type script func(reg *internalgrpc.MemberRegistry, m *internalgrpc.Member, requestID string)

// answers completes the request the way a cnode of any kind would: a processor
// result, a criterion result and a function result in one response.
func answers(by string) script {
	return func(_ *internalgrpc.MemberRegistry, m *internalgrpc.Member, requestID string) {
		yes := true
		m.CompleteRequest(requestID, &internalgrpc.ProcessingResponse{
			Success:    true,
			Payload:    json.RawMessage(`{"data":{"by":"` + by + `"}}`),
			Matches:    &yes,
			Reason:     by,
			ResultKind: "Schedule",
			Result:     json.RawMessage(`{"by":"` + by + `"}`),
		})
	}
}

// fails answers "I failed", with the cnode's own message and verdict.
func fails(message string, verdict *bool) script {
	return func(_ *internalgrpc.MemberRegistry, m *internalgrpc.Member, requestID string) {
		m.CompleteRequest(requestID, &internalgrpc.ProcessingResponse{Success: false, Error: message, Retryable: verdict})
	}
}

// detaches takes the work and goes away, as a cnode that crashed does: its
// stream handler unregisters it, which fails the request as disconnected.
func detaches() script {
	return func(reg *internalgrpc.MemberRegistry, m *internalgrpc.Member, _ string) { reg.Unregister(m) }
}

// attach registers a scripted cnode. On a fresh registry cnodes are tried in
// the order they were attached.
func (e *env) attach(t *testing.T, id string, tenant spi.TenantID, tag string, s script) *cnode {
	t.Helper()
	n := &cnode{}
	m := e.reg.Register(id, tenant, []string{tag}, func(ce *cepb.CloudEvent) error {
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
			n.mu.Lock()
			defer n.mu.Unlock()
			n.requestIDs = append(n.requestIDs, body.RequestID)
			n.passes = append(n.passes, internalgrpc.TxTokenFromCloudEvent(ce))
		}()
		if s != nil {
			if m := e.reg.Get(id); m != nil {
				s(e.reg, m, body.RequestID)
			}
		}
		return nil
	}, nil)
	t.Cleanup(func() { e.reg.Unregister(m) })
	return n
}

func testEntity() *spi.Entity {
	return &spi.Entity{Meta: spi.EntityMeta{ID: "entity-1", TenantID: tenantA}, Data: []byte(`{"foo":"bar"}`)}
}

func processorDef(tag, retryPolicy string, idempotent bool) spi.ProcessorDefinition {
	return spi.ProcessorDefinition{Name: "charge", Config: spi.ProcessorConfig{
		CalculationNodesTags: tag, ResponseTimeoutMs: limitMs, RetryPolicy: retryPolicy, Idempotent: idempotent,
	}}
}

func criterionJSON(tag, retryPolicy string) json.RawMessage {
	return json.RawMessage(fmt.Sprintf(
		`{"type":"function","function":{"name":"isEligible","config":{"calculationNodesTags":%q,"responseTimeoutMs":%d,"retryPolicy":%q}}}`,
		tag, limitMs, retryPolicy))
}

func functionDef(tag, retryPolicy string) spi.ScheduleFunction {
	return spi.ScheduleFunction{Name: "calcFire", ResultKind: "Schedule", CalculationNodesTags: tag, ResponseTimeoutMs: limitMs, RetryPolicy: retryPolicy}
}

// patientLimitMs is the answer limit for a test whose try must be ended by
// something other than the limit — a caller that goes away, the fence releasing
// the callout. A cnode has half a minute to answer, so the thing under test is
// always what ends the try, whatever the machine is doing meanwhile.
const patientLimitMs = 30_000

// dispatchPatientFunction makes the callout of dispatchFunction with an answer
// limit no test waits out.
func (e *env) dispatchPatientFunction(ctx context.Context, tag string) error {
	fn := functionDef(tag, "")
	fn.ResponseTimeoutMs = patientLimitMs
	_, err := e.owner.DispatchFunction(ctx, testEntity(), fn, "wf1", "t1", "tx-1")
	return err
}

// dispatchFunction is the callout most tests make: a function is repeat-safe by
// rule, so every kind of failure that permits another cnode can be shown on it.
func (e *env) dispatchFunction(ctx context.Context, tag, retryPolicy string) (string, error) {
	res, err := e.owner.DispatchFunction(ctx, testEntity(), functionDef(tag, retryPolicy), "wf1", "t1", "tx-1")
	if err != nil {
		return "", err
	}
	var by struct {
		By string `json:"by"`
	}
	if err := json.Unmarshal(res.Value, &by); err != nil {
		return "", fmt.Errorf("function result: %w", err)
	}
	return by.By, nil
}

func appErrOf(t *testing.T, err error) *common.AppError {
	t.Helper()
	var appErr *common.AppError
	if !errors.As(err, &appErr) {
		t.Fatalf("no *AppError behind %v", err)
	}
	return appErr
}

func mustFinish(t *testing.T, what string, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("%s did not finish", what)
	}
}

// claimsOf reads the claims of a pass a cnode was given.
func claimsOf(t *testing.T, e *env, pass string) *token.Claims {
	t.Helper()
	claims, err := e.signer.Verify(pass)
	if err != nil {
		t.Fatalf("verify pass: %v", err)
	}
	return claims
}
