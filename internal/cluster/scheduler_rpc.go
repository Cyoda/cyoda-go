package cluster

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/cluster/dispatch"
	"github.com/cyoda-platform/cyoda-go/internal/cluster/peeraddr"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/contract"
	"github.com/cyoda-platform/cyoda-go/internal/domain/workflow"
	"github.com/cyoda-platform/cyoda-go/internal/scheduler"
)

// schedulerTaskPath is the peer-authenticated route a coordinator posts to
// so another node fires a ScheduledTask on its behalf. Namespaced alongside
// the generic callout dispatch route
// (internal/cluster/dispatch/handler.go: /internal/dispatch/callout) even
// though it is served by this package's own handler rather than
// dispatch.DispatchHandler — the scheduled-task payload and its engine seam
// are scheduler-specific, not callout-specific, so it gets its own small
// handler rather than widening DispatchHandler's contract.
const schedulerTaskPath = "/internal/dispatch/scheduled-task"

// SchedulerTaskRequest is the cross-node payload for ExecuteScheduledTask.
type SchedulerTaskRequest struct {
	Task spi.ScheduledTask `json:"task"`
}

// SchedulerTaskResponse acks a peer-delegated fire. Error is sanitized, never
// the raw underlying error text. It travels sealed for the request it answers,
// so the coordinator either reads what the peer wrote or knows the answer was
// lost — there is no third case in which something else speaks for the peer.
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
// dispatch channel — the same AEAD-wrapped transport callout dispatch uses
// (internal/cluster/dispatch/forwarder.go) — when the target is a peer.
// There is no unauthenticated forwarding path.
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

	if err := c.client.ExecuteScheduledTask(ctx, target, addr, task); err != nil {
		slog.Warn("scheduled task peer forward failed, dropping (next scan redispatches)",
			"pkg", "cluster", "taskId", task.ID, "target", target, "err", err)
	}
}

// SchedulerRPCClient forwards ExecuteScheduledTask calls to a peer over the
// PeerAuth-authenticated channel. Mirrors
// dispatch.HTTPForwarder.ForwardCallout's sign/POST/open shape
// (internal/cluster/dispatch/forwarder.go) so it reuses the exact same auth
// implementation instance the app wires for callout dispatch; it is a
// separate ~small type rather than a method on HTTPForwarder because its
// request/response payload is scheduler-specific and DispatchForwarder's
// interface is scoped to callout dispatch.
type SchedulerRPCClient struct {
	auth dispatch.PeerAuth
	// timeout is the whole-call budget (CYODA_DISPATCH_FORWARD_TIMEOUT). The
	// HTTP client applies it to the request; it bounds the name lookup that
	// precedes the request too, so a resolver that is down cannot hold the
	// scan loop's goroutine for the resolver's own timeout instead.
	timeout       time.Duration
	httpClient    *http.Client
	allowLoopback bool
}

// NewSchedulerRPCClient constructs a SchedulerRPCClient. auth must be the
// same PeerAuth instance (shared secret) the peer's SchedulerRPCHandler
// verifies against — in production, the identical instance passed to
// dispatch.NewDispatchHandler for processor/criteria dispatch.
func NewSchedulerRPCClient(auth dispatch.PeerAuth, timeout time.Duration) *SchedulerRPCClient {
	return &SchedulerRPCClient{
		auth:    auth,
		timeout: timeout,
		httpClient: &http.Client{
			Timeout:       timeout,
			CheckRedirect: peeraddr.RefuseRedirects,
			// A transport of its own, never http.DefaultTransport: that one
			// takes a proxy from the environment, and through it the sealed
			// fire goes to an address peeraddr.Validate never saw — the same
			// pivot refusing redirects closes, reached from the environment
			// instead of from the network. Keep-alives are kept: unlike a
			// hand-over, nothing here turns on telling a peer that is down
			// from one that took the work.
			Transport: &http.Transport{Proxy: nil},
		},
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
// route, authenticated via the wrapped PeerAuth and sealed for the node named
// target — the node whose address the registry gave, and the only one that can
// open it. The call is fire-and-forget from the coordinator's point of view — a
// non-nil error means the peer could not be reached, rejected the request, or
// answered something this node could not open under its request's binding; the
// caller (ClusterExecutor) logs and drops it rather than retrying inline,
// relying on the scan loop's at-least-once redispatch.
func (c *SchedulerRPCClient) ExecuteScheduledTask(ctx context.Context, target, addr string, task spi.ScheduledTask) error {
	lookupCtx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	if err := peeraddr.Validate(lookupCtx, addr, c.allowLoopback); err != nil {
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

	wire, binding, err := c.auth.Sign(httpReq, target, plain)
	if err != nil {
		return fmt.Errorf("scheduler rpc: sign body: %w", err)
	}
	if len(wire) > dispatch.MaxEnvelopeSize {
		// The peer reads at most the ceiling, so these bytes would be truncated
		// and refused there. Refused here instead, before a connection is
		// opened: the coordinator logs it and the next scan redispatches.
		return fmt.Errorf("scheduler rpc: the request is %d bytes sealed and the envelope holds %d", len(wire), dispatch.MaxEnvelopeSize)
	}
	httpReq.Body = io.NopCloser(bytes.NewReader(wire))
	httpReq.ContentLength = int64(len(wire))

	httpResp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return fmt.Errorf("scheduler rpc: POST %s: %w", url, err)
	}
	defer httpResp.Body.Close()

	// A refusal is decided by its status alone. The body that comes with it is
	// not sealed — anyone on the path can write one — so it is never parsed,
	// only excerpted into the error for an operator to read.
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(httpResp.Body, 512))
		return fmt.Errorf("scheduler rpc: peer returned %d: %s", httpResp.StatusCode, raw)
	}

	// The answer comes back sealed for this request and is opened under the
	// binding Sign returned. An answer that does not open is a lost answer —
	// there is no plaintext fallback, or a forged success would tell this node
	// a task fired that never ran.
	// One byte past the ceiling is read so that an answer above it can be told
	// apart from one at it; without the check that follows, an oversized answer
	// arrives truncated and is blamed on the cipher.
	sealed, err := io.ReadAll(io.LimitReader(httpResp.Body, dispatch.MaxEnvelopeSize+1))
	if err != nil {
		return fmt.Errorf("scheduler rpc: read response from %s: %w", url, err)
	}
	if len(sealed) > dispatch.MaxEnvelopeSize {
		return fmt.Errorf("scheduler rpc: the answer from %s is too large for the envelope, which holds %d bytes", url, dispatch.MaxEnvelopeSize)
	}
	opened, err := c.auth.OpenResponse(httpResp.Header, binding, sealed)
	if err != nil {
		return fmt.Errorf("failed to open scheduler response: %w", err)
	}

	var resp SchedulerTaskResponse
	if err := json.Unmarshal(opened, &resp); err != nil {
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
// route. It keeps dispatch.DispatchHandler's auth pattern over the same
// PeerAuth: the same Verify-or-403 gate, a full replay cache answered under
// seal while a replayed nonce gets the bare status, every answer to a request
// that opened sealed for that request, and the same "never log the task payload
// beyond ids, nor a decode error's text" discipline — so the scheduled-task
// peer surface carries the same security posture as processor/criteria dispatch
// (Gate 3: no new unauthenticated cluster surface). What it does not share is
// the answer itself: a fire is acked, not classified into the callout
// taxonomy.
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
	body, identity, binding, err := h.auth.Verify(r)
	switch {
	case errors.Is(err, dispatch.ErrReplayCacheFull):
		// Opened and authenticated, then refused by the replay cache's
		// capacity: nothing fired, and the coordinator is told so under seal —
		// the same answer dispatch.DispatchHandler gives for the same refusal. A
		// replayed nonce is a different matter and keeps the bare status below:
		// there is no request to bind that answer to but the one the replay
		// copies.
		slog.Warn("scheduled task refused: the replay cache is full",
			"pkg", "cluster", "remoteAddr", r.RemoteAddr)
		h.writeSealed(w, binding, SchedulerTaskResponse{Success: false, Error: "the node could not take the scheduled task"})
		return
	case err != nil:
		slog.Warn("scheduled task dispatch auth failed",
			"pkg", "cluster", "remoteAddr", r.RemoteAddr, "err", err)
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	var req SchedulerTaskRequest
	if err := json.Unmarshal(body, &req); err != nil {
		// Sealed, like every other answer to a request that opened: a bare
		// status is indistinguishable from one written by whoever is on the path
		// between the nodes, so it would reach the coordinator as a lost answer
		// rather than as what this node decided. Logged by the error's shape —
		// a decode error quotes the literal it failed on, and the body is
		// another node's.
		slog.Error("a scheduled task request could not be read",
			"pkg", "cluster", "error", common.JSONErrorShape(err))
		h.writeSealed(w, binding, SchedulerTaskResponse{Success: false, Error: "the scheduled task request could not be read"})
		return
	}

	// The system identity — not the caller's — drives the fire; the peer
	// identity is attached alongside it purely for audit/tracing parity
	// with dispatch.DispatchHandler.buildContext.
	sysCtx := dispatch.WithPeerIdentity(common.SystemUserContext(req.Task.TenantID), identity)

	outcome, fireErr := h.engine.FireScheduledTransition(sysCtx, req.Task)
	if fireErr != nil {
		slog.Error("scheduled task peer fire failed",
			"pkg", "cluster", "taskId", req.Task.ID, "err", fireErr)
		h.writeSealed(w, binding, SchedulerTaskResponse{
			Success: false,
			Error:   "scheduled task fire failed",
		})
		return
	}

	slog.Debug("scheduled task peer fire resolved",
		"pkg", "cluster", "taskId", req.Task.ID, "outcome", outcome)
	h.writeSealed(w, binding, SchedulerTaskResponse{Success: true})
}

// writeSealed answers the request binding names, under seal — the same
// discipline dispatch.DispatchHandler.writeSealed keeps. A coordinator trusts
// nothing else: a status line, or a body it cannot open, tells it only that the
// answer was lost, never that the task fired.
func (h *SchedulerRPCHandler) writeSealed(w http.ResponseWriter, binding dispatch.ResponseBinding, resp SchedulerTaskResponse) {
	plain, err := json.Marshal(resp)
	if err != nil {
		slog.Error("failed to marshal scheduled task answer", "pkg", "cluster", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	wire, err := h.auth.SealResponse(w.Header(), binding, plain)
	if err != nil {
		slog.Error("failed to seal scheduled task answer", "pkg", "cluster", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
	if _, err := w.Write(wire); err != nil {
		slog.Warn("failed to write scheduled task answer", "pkg", "cluster", "err", err)
	}
}
