package cluster

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/cluster/dispatch"
	"github.com/cyoda-platform/cyoda-go/internal/cluster/peeraddr"
	"github.com/cyoda-platform/cyoda-go/internal/contract"
	"github.com/cyoda-platform/cyoda-go/internal/domain/workflow"
	"github.com/cyoda-platform/cyoda-go/internal/scheduler"
)

// schedulerTaskPath is the peer-authenticated route a coordinator posts to
// so another node fires a ScheduledTask on its behalf. Namespaced alongside
// the processor/criteria dispatch routes
// (internal/cluster/dispatch/handler.go: /internal/dispatch/processor,
// /internal/dispatch/criteria) even though it is served by this package's
// own handler rather than dispatch.DispatchHandler — the scheduled-task
// payload and its engine seam are scheduler-specific, not processor/
// criteria-specific, so it gets its own small handler rather than widening
// DispatchHandler's contract.
const schedulerTaskPath = "/internal/dispatch/scheduled-task"

// SchedulerTaskRequest is the cross-node payload for ExecuteScheduledTask.
type SchedulerTaskRequest struct {
	Task spi.ScheduledTask `json:"task"`
}

// SchedulerTaskResponse acks a peer-delegated fire. The coordinator does
// not depend on its content — Execute is fire-and-forget (design doc §6.2)
// — but Success/Error are populated for diagnostics; Error is sanitized,
// never the raw underlying error text.
type SchedulerTaskResponse struct {
	Success bool   `json:"success"`
	Error   string `json:"error,omitempty"`
}

// schedulerEngineAdapter adapts *workflow.Engine's FireScheduledTransition
// (which returns workflow.ScheduledOutcome) to scheduler.Engine (which
// returns a plain string) — the seam that lets the real engine satisfy
// scheduler.Engine without internal/scheduler importing
// internal/domain/workflow.
type schedulerEngineAdapter struct {
	engine *workflow.Engine
}

func (a schedulerEngineAdapter) FireScheduledTransition(ctx context.Context, task spi.ScheduledTask) (string, error) {
	outcome, err := a.engine.FireScheduledTransition(ctx, task)
	return string(outcome), err
}

// NewSchedulerEngine adapts a real *workflow.Engine to scheduler.Engine for
// use with LocalExecutor, ClusterExecutor, and SchedulerRPCHandler.
func NewSchedulerEngine(engine *workflow.Engine) scheduler.Engine {
	return schedulerEngineAdapter{engine: engine}
}

// ClusterExecutor implements scheduler.Executor: it fires a ScheduledTask
// locally when the coordinator picked this node as the dispatch target, or
// forwards it to the target peer over the existing PeerAuth-authenticated
// dispatch channel — the same AEAD-wrapped transport processor/criteria
// dispatch uses (internal/cluster/dispatch/forwarder.go) — when the target
// is a peer. There is no unauthenticated forwarding path.
//
// A peer-forward failure (unresolvable target, unreachable peer, auth
// rejected) is logged and dropped — it never falls back to firing locally,
// which would silently defeat the distribution strategy the coordinator
// chose and could concentrate load back onto the coordinator node during a
// partition. The scheduler's at-least-once redispatch (RedispatchBackoff,
// internal/scheduler/service.go) covers it: the row is still due on a
// later scan and may then land on a different, live target.
type ClusterExecutor struct {
	local    *scheduler.LocalExecutor
	registry contract.NodeRegistry
	client   *SchedulerRPCClient
	selfID   string
}

// NewClusterExecutor constructs a ClusterExecutor. registry and client may
// be nil for a single-node/cluster-disabled deployment where every
// dispatch target is always selfID (see scheduler.Self distribution) —
// Execute never reaches the forward branch in that configuration, but nil
// is handled defensively regardless.
func NewClusterExecutor(engine scheduler.Engine, selfID string, registry contract.NodeRegistry, client *SchedulerRPCClient) *ClusterExecutor {
	return &ClusterExecutor{
		local:    scheduler.NewLocalExecutor(engine),
		registry: registry,
		client:   client,
		selfID:   selfID,
	}
}

// Execute implements scheduler.Executor.
func (c *ClusterExecutor) Execute(ctx context.Context, task spi.ScheduledTask, target string) {
	if target == c.selfID || target == "" || c.registry == nil || c.client == nil {
		// A nil registry/client for a genuinely non-self target means this
		// ClusterExecutor was constructed without cluster wiring (mis-wire)
		// even though the coordinator picked a peer — the distribution
		// strategy is being silently defeated (every task lands on this
		// node regardless of Pick's choice). Fire locally anyway
		// (fail-toward-runs: a due task must never be dropped), but make
		// the mis-wire loud so it's observable rather than quietly
		// concentrating load onto whichever node happens to scan.
		if (c.registry == nil || c.client == nil) && target != c.selfID && target != "" {
			slog.Warn("scheduled task: cluster executor has no registry/client wired, firing locally instead of on picked target",
				"pkg", "scheduler", "taskId", task.ID, "target", target, "selfID", c.selfID)
		}
		c.local.Execute(ctx, task, target)
		return
	}

	addr, alive, err := c.registry.Lookup(ctx, target)
	if err != nil || !alive || addr == "" {
		slog.Warn("scheduled task: target peer unresolved, dropping (next scan redispatches)",
			"pkg", "cluster", "taskId", task.ID, "target", target, "err", err)
		return
	}

	if err := c.client.ExecuteScheduledTask(ctx, addr, task); err != nil {
		slog.Warn("scheduled task peer forward failed, dropping (next scan redispatches)",
			"pkg", "cluster", "taskId", task.ID, "target", target, "err", err)
	}
}

// SchedulerRPCClient forwards ExecuteScheduledTask calls to a peer over the
// PeerAuth-authenticated channel. Mirrors
// dispatch.HTTPForwarder.ForwardProcessor/ForwardCriteria's sign/POST/
// decode shape (internal/cluster/dispatch/forwarder.go) so it reuses the
// exact same auth implementation instance the app wires for processor/
// criteria dispatch; it is a separate ~small type rather than a method on
// HTTPForwarder because its request/response payload is scheduler-specific
// and DispatchForwarder's interface is scoped to processor/criteria.
type SchedulerRPCClient struct {
	auth          dispatch.PeerAuth
	httpClient    *http.Client
	allowLoopback bool
}

// NewSchedulerRPCClient constructs a SchedulerRPCClient. auth must be the
// same PeerAuth instance (shared secret) the peer's SchedulerRPCHandler
// verifies against — in production, the identical instance passed to
// dispatch.NewDispatchHandler for processor/criteria dispatch.
func NewSchedulerRPCClient(auth dispatch.PeerAuth, timeout time.Duration) *SchedulerRPCClient {
	return &SchedulerRPCClient{
		auth:       auth,
		httpClient: &http.Client{Timeout: timeout},
	}
}

// AllowLoopbackForTesting opts the client out of the loopback SSRF guard —
// see dispatch.HTTPForwarder.AllowLoopbackForTesting. Never call this in
// production. Returns the receiver for fluent construction.
func (c *SchedulerRPCClient) AllowLoopbackForTesting() *SchedulerRPCClient {
	c.allowLoopback = true
	return c
}

// ExecuteScheduledTask POSTs task to the peer at addr's scheduled-task
// route, authenticated via the wrapped PeerAuth. The call is
// fire-and-forget from the coordinator's point of view — a non-nil error
// means the peer could not be reached or rejected the request; the caller
// (ClusterExecutor) logs and drops it rather than retrying inline, relying
// on the scan loop's at-least-once redispatch.
func (c *SchedulerRPCClient) ExecuteScheduledTask(ctx context.Context, addr string, task spi.ScheduledTask) error {
	if err := peeraddr.Validate(addr, c.allowLoopback); err != nil {
		return err
	}

	plain, err := json.Marshal(SchedulerTaskRequest{Task: task})
	if err != nil {
		return fmt.Errorf("scheduler rpc: marshal request: %w", err)
	}

	url := ensureScheme(addr) + schedulerTaskPath
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, nil)
	if err != nil {
		return fmt.Errorf("scheduler rpc: build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	wire, err := c.auth.Sign(httpReq, plain)
	if err != nil {
		return fmt.Errorf("scheduler rpc: sign body: %w", err)
	}
	httpReq.Body = io.NopCloser(bytes.NewReader(wire))
	httpReq.ContentLength = int64(len(wire))

	httpResp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return fmt.Errorf("scheduler rpc: POST %s: %w", url, err)
	}
	defer httpResp.Body.Close()

	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(httpResp.Body, 512))
		return fmt.Errorf("scheduler rpc: peer returned %d: %s", httpResp.StatusCode, raw)
	}

	var resp SchedulerTaskResponse
	if err := json.NewDecoder(httpResp.Body).Decode(&resp); err != nil {
		return fmt.Errorf("scheduler rpc: decode response from %s: %w", url, err)
	}
	if !resp.Success {
		return fmt.Errorf("scheduler rpc: peer reported failure: %s", resp.Error)
	}
	return nil
}

// ensureScheme prepends http:// if addr has no scheme. Duplicates
// dispatch's unexported helper of the same name and behavior
// (internal/cluster/dispatch/forwarder.go) rather than exporting it across
// the package boundary for a two-line function.
func ensureScheme(addr string) string {
	if !strings.Contains(addr, "://") {
		return "http://" + addr
	}
	return addr
}

// SchedulerRPCHandler serves the peer-authenticated ExecuteScheduledTask
// route. Mirrors dispatch.DispatchHandler's auth pattern exactly — the same
// PeerAuth.Verify-or-403 gate, the same "never log the task payload beyond
// ids" discipline — so the scheduled-task peer surface carries the
// identical security posture as processor/criteria dispatch (Gate 3: no
// new unauthenticated cluster surface).
type SchedulerRPCHandler struct {
	engine scheduler.Engine
	auth   dispatch.PeerAuth
}

// NewSchedulerRPCHandler constructs a SchedulerRPCHandler. auth must be the
// same PeerAuth instance peers sign with via SchedulerRPCClient.
func NewSchedulerRPCHandler(engine scheduler.Engine, auth dispatch.PeerAuth) *SchedulerRPCHandler {
	return &SchedulerRPCHandler{engine: engine, auth: auth}
}

// Register registers the scheduled-task route on mux.
func (h *SchedulerRPCHandler) Register(mux *http.ServeMux) {
	mux.HandleFunc("POST "+schedulerTaskPath, h.handle)
}

// handle authenticates the request, unmarshals the task, builds a system
// UserContext scoped to the task's tenant, and fires it — the worker side
// of design doc §6.2.
func (h *SchedulerRPCHandler) handle(w http.ResponseWriter, r *http.Request) {
	body, identity, err := h.auth.Verify(r)
	if err != nil {
		slog.Warn("scheduled task dispatch auth failed",
			"pkg", "cluster", "remoteAddr", r.RemoteAddr, "err", err)
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	var req SchedulerTaskRequest
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}

	// The system identity — not the caller's — drives the fire; the peer
	// identity is attached alongside it purely for audit/tracing parity
	// with dispatch.DispatchHandler.buildContext.
	sysCtx := dispatch.WithPeerIdentity(scheduler.SystemUserContext(req.Task.TenantID), identity)

	outcome, fireErr := h.engine.FireScheduledTransition(sysCtx, req.Task)
	if fireErr != nil {
		slog.Error("scheduled task peer fire failed",
			"pkg", "cluster", "taskId", req.Task.ID, "err", fireErr)
		writeSchedulerJSON(w, http.StatusOK, SchedulerTaskResponse{
			Success: false,
			Error:   "scheduled task fire failed",
		})
		return
	}

	slog.Debug("scheduled task peer fire resolved",
		"pkg", "cluster", "taskId", req.Task.ID, "outcome", outcome)
	writeSchedulerJSON(w, http.StatusOK, SchedulerTaskResponse{Success: true})
}

func writeSchedulerJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Error("scheduler rpc handler: failed to write JSON response", "pkg", "cluster", "err", err)
	}
}
