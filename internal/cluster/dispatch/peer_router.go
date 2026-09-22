package dispatch

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"syscall"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/metric/noop"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/contract"
	internalgrpc "github.com/cyoda-platform/cyoda-go/internal/grpc"
)

// outcomeNotConnected is the hand-over outcome of a peer that was never asked.
// It is not a contract.CalloutFailureKind: the answer the owner reads is
// NoHandOff, and the counter keeps the two apart because they mean different
// things to an operator — no compute member anywhere, or a peer this pnode
// cannot reach.
const outcomeNotConnected = "not_connected"

const (
	// maxPeerDiagnostics bounds how many of an answering pnode's diagnostic
	// entries this pnode relays. A sealed answer proves only that some holder
	// of the cluster key wrote it, and its warnings, errors and attempts go on
	// to the client: an envelope may carry MaxEnvelopeSize of them, and one
	// pnode does not fill another's request diagnostics without a bound. The
	// bound is well above what a genuine answer holds — one entry per try.
	maxPeerDiagnostics = 32
	// maxPeerDiagnosticRunes bounds one entry's length. The cut is marked, in
	// the tree's convention for a shortened client-facing text.
	maxPeerDiagnosticRunes = 512
	// maxPeerCriterionReasonRunes bounds a relayed criterion's reason alone.
	// It is pinned to internalgrpc.MaxCriterionReasonRunes, not to
	// maxPeerDiagnosticRunes: the reason is the business explanation a
	// criterion gives for refusing a transition rather than a diagnostic, and
	// a reason relayed from a peer must not be cut shorter than one answered
	// locally — that would be exactly the backend-divergence-shaped defect
	// this tree treats as a bug, in the cluster dimension. See
	// TestCriterionReasonBound_AgreesWithTheMemberBound.
	maxPeerCriterionReasonRunes = internalgrpc.MaxCriterionReasonRunes
)

// peerDiagnosticsOmittedWarning stands in for the entries past the bound, so
// that a reader of the diagnostics is never left thinking they are complete.
const peerDiagnosticsOmittedWarning = "further diagnostics from the peer node were omitted"

// PeerRouter is the owner's view of the other pnodes: which of them advertise
// a tag, and the hand-over of a callout to one of them. It holds no state of
// its own.
type PeerRouter struct {
	registry   contract.NodeRegistry
	selfNodeID string
	selector   PeerSelector
	forwarder  DispatchForwarder
	handovers  metric.Int64Counter
}

// NewPeerRouter constructs a PeerRouter. A nil meter records nothing.
func NewPeerRouter(registry contract.NodeRegistry, selfNodeID string, selector PeerSelector, forwarder DispatchForwarder, meter metric.Meter) (*PeerRouter, error) {
	if meter == nil {
		meter = noop.NewMeterProvider().Meter("")
	}
	handovers, err := meter.Int64Counter("cyoda.callout.handovers",
		metric.WithDescription("Callouts handed over to another node, by outcome"))
	if err != nil {
		return nil, fmt.Errorf("failed to create the hand-over counter: %w", err)
	}
	return &PeerRouter{registry: registry, selfNodeID: selfNodeID, selector: selector, forwarder: forwarder, handovers: handovers}, nil
}

// Peers returns the alive pnodes other than self that advertise any of tagsCSV
// for tenantID, in selector order: the selector picks one of those left, again
// and again. A registry that cannot be read has no peers.
func (r *PeerRouter) Peers(tenantID, tagsCSV string) []contract.NodeInfo {
	nodes, err := r.registry.List(context.Background())
	if err != nil {
		slog.Debug("failed to list cluster nodes", "pkg", "dispatch", "err", err)
		return nil
	}
	var left []contract.NodeInfo
	for _, n := range nodes {
		if n.NodeID != r.selfNodeID && n.Alive && common.TagsOverlap(n.Tags[tenantID], tagsCSV) {
			left = append(left, n)
		}
	}
	ordered := make([]contract.NodeInfo, 0, len(left))
	for len(left) > 0 {
		pick, err := r.selector.Select(left)
		if err != nil {
			slog.Debug("peer selection failed", "pkg", "dispatch", "err", err)
			break
		}
		ordered = append(ordered, pick)
		for i, n := range left {
			if n.NodeID == pick.NodeID {
				left = append(left[:i:i], left[i+1:]...)
				break
			}
		}
	}
	return ordered
}

// Changed is the node registry's change signal: closed when a peer's list
// arrives with different tags, a peer joins, or a peer leaves. Take it before
// calling Peers, so that no change is missed.
func (r *PeerRouter) Changed() <-chan struct{} { return r.registry.Changed() }

// HandOver passes call to peer with triesLeft tries under fencing number major.
// ctx bounds the wait for the answer: the caller puts the deadline on it
// (triesLeft × answer limit + the hand-over allowance, never past the callout's
// deadline); the transport has no timeout of its own beyond opening the
// connection.
//
// The peer's warnings are returned for the caller to add; the diagnostics the
// tries raised as errors on the peer are added to ctx here. The caller's own
// context ending is not reported: the caller checks its ctx, and context.Cause,
// after the call.
func (r *PeerRouter) HandOver(ctx context.Context, peer contract.NodeInfo, call internalgrpc.Callout, triesLeft int, major uint32) HandOverAnswer {
	ans, outcome := r.handOver(ctx, peer, call, triesLeft, major)
	r.handovers.Add(ctx, 1, metric.WithAttributes(attribute.String("outcome", outcome)))
	for _, e := range ans.peerErrors {
		common.AddError(ctx, e)
	}
	slog.Debug("hand-over", "pkg", "dispatch", "kind", call.Kind.String(), "name", call.Name, "requestId", call.RequestID,
		"peer", peer.NodeID, "triesLeft", triesLeft, "major", major, "outcome", outcome, "triesUsed", ans.TriesUsed)
	return ans
}

func (r *PeerRouter) handOver(ctx context.Context, peer contract.NodeInfo, call internalgrpc.Callout, triesLeft int, major uint32) (HandOverAnswer, string) {
	// Built afresh for every try: the entity travels as it stands now, never as
	// an earlier try serialised it.
	req, err := newHandOverRequest(spi.GetUserContext(ctx), r.selfNodeID, call, triesLeft, major)
	if err != nil {
		slog.Warn("callout cannot be handed over", "pkg", "dispatch", "kind", call.Kind.String(), "name", call.Name, "requestId", call.RequestID, "err", err)
		return provedBeforeConnecting(err), contract.Terminal.String()
	}

	resp, err := r.forwarder.ForwardCallout(ctx, peer.NodeID, peer.Addr, req)
	if err != nil {
		stage := StageAfterConnect
		var fe *ForwardError
		if errors.As(err, &fe) {
			stage = fe.Stage
		}
		switch stage {
		case StageBeforeConnect:
			// The error can embed the URL the request was built for, so only the
			// stage is logged. The error itself stays in the returned failure's
			// wrapped cause, which no response renderer reads.
			slog.Warn("hand-over could not be built or signed; nothing was sent",
				"pkg", "dispatch", "peer", peer.NodeID, "requestId", call.RequestID)
			return provedBeforeConnecting(err), contract.Terminal.String()
		case StageNotConnected:
			// The cause names the address — a refused address in its own words,
			// a dial error in the socket's — so the peer is named by its node id
			// and the cause travels as its class, never as its text.
			slog.Warn("peer was not asked; no try used", "pkg", "dispatch", "peer", peer.NodeID,
				"requestId", call.RequestID, "reason", notConnectedReason(err))
			return notConnected(), outcomeNotConnected
		default:
			// The address and route are in err: logged here, never returned.
			slog.Warn("hand-over answer lost; counted as one try", "pkg", "dispatch", "peer", peer.NodeID, "requestId", call.RequestID, "err", err)
			return lostAnswer(), contract.NoAnswer.String()
		}
	}

	omitted := r.boundPeerText(resp, peer, call.RequestID)
	ans := readAnswer(call, resp, triesLeft)
	// Only an answer that was read relays the peer's text; a lost one relays
	// none of it, and a note about what was left out would refer to nothing.
	if omitted && !ans.lost {
		ans.Warnings = append(ans.Warnings, peerDiagnosticsOmittedWarning)
	}
	if ans.Failure == nil {
		return ans, OutcomeOK
	}
	return ans, ans.Failure.Kind.String()
}

// notConnectedReason classifies, in a closed vocabulary, why a peer could not
// be connected to. The vocabulary is closed on purpose: the error's own text
// names the peer's address and the route, and an operator needs the class —
// a refused address is a misconfiguration, a timeout is the network, a refused
// port is a peer that is gone. "other" is the honest answer where the socket
// gave nothing this node recognises.
func notConnectedReason(err error) string {
	if errors.Is(err, ErrForbiddenPeerAddress) {
		return "address_refused"
	}
	// Before the timeout check: a name lookup that timed out is still a name
	// lookup, which is the more useful class of the two.
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return "dns"
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return "timeout"
	}
	switch {
	case errors.Is(err, syscall.ECONNREFUSED):
		return "refused"
	case errors.Is(err, syscall.EHOSTUNREACH), errors.Is(err, syscall.ENETUNREACH):
		return "unreachable"
	}
	return "other"
}

// boundPeerText makes an answer's peer-written text fit to relay, and reports
// whether entries were left out. It touches nothing the answer decides — the
// outcome, the tries used, the result and the cnode's own verdict stand as they
// came; what it bounds is untrusted text on its way to the client's
// diagnostics, and an error code on its way to becoming a client-facing one.
func (r *PeerRouter) boundPeerText(resp *DispatchCalloutResponse, peer contract.NodeInfo, requestID string) bool {
	if resp == nil {
		return false
	}
	omitted := len(resp.Warnings) > maxPeerDiagnostics || len(resp.Errors) > maxPeerDiagnostics || len(resp.Attempts) > maxPeerDiagnostics
	resp.Warnings = boundLines(resp.Warnings)
	resp.Errors = boundLines(resp.Errors)
	// The cnode's own message becomes the MemberFailed failure's, and from
	// there the client's 400 body — the same text a local try bounds where the
	// member speaks it (boundMemberText, internal/grpc/dispatch.go).
	resp.MemberError = boundLine(resp.MemberError)
	// A criterion's reason reaches the client where a transition is refused,
	// and a function's resultKind reaches the error the engine raises for a
	// result it cannot use. Both are the answering node's free text, but the
	// reason is the business explanation a criterion gives for refusing a
	// transition rather than a diagnostic, so it keeps its own wider
	// allowance instead of the general diagnostic bound the rest of this text
	// gets. The vocabulary resultKind belongs to is not judged here: which
	// kinds exist is the caller's question, and the one use site
	// (workflow.armScheduled) refuses a kind it cannot use for a hand-over
	// exactly as it does for a local callout.
	resp.Reason = boundCriterionReason(resp.Reason)
	resp.ResultKind = boundLine(resp.ResultKind)
	if len(resp.Attempts) > maxPeerDiagnostics {
		resp.Attempts = resp.Attempts[:maxPeerDiagnostics:maxPeerDiagnostics]
	}
	for i := range resp.Attempts {
		resp.Attempts[i].MemberID = boundLine(resp.Attempts[i].MemberID)
		resp.Attempts[i].Cause = boundLine(resp.Attempts[i].Cause)
	}
	if code := resp.ErrorCode; code != "" && !common.KnownErrorCode(code) {
		// An error code is part of what the client reads. One this build does
		// not define is dropped rather than minted: the answer is then
		// classified exactly as an answer that carried no code at all, which
		// gives the hand-over's own code for that outcome.
		// The code is a peer's text like any other: bounded before it is logged.
		slog.Warn("peer node classified a failure with an error code this node does not define",
			"pkg", "dispatch", "peer", peer.NodeID, "requestId", requestID, "peerErrorCode", boundLine(code))
		resp.ErrorCode, resp.ErrorStatus, resp.ErrorRetryable = "", 0, false
	}
	return omitted
}

// boundLines keeps the first maxPeerDiagnostics entries, each bounded.
func boundLines(in []string) []string {
	if len(in) > maxPeerDiagnostics {
		in = in[:maxPeerDiagnostics:maxPeerDiagnostics]
	}
	for i, s := range in {
		in[i] = boundLine(s)
	}
	return in
}

// boundLine keeps the first maxPeerDiagnosticRunes runes, marking the cut so a
// reader can tell a shortened text from a short one.
func boundLine(s string) string {
	if len(s) <= maxPeerDiagnosticRunes {
		return s // runes never outnumber bytes: nothing to cut
	}
	r := []rune(s)
	if len(r) <= maxPeerDiagnosticRunes {
		return s
	}
	return string(r[:maxPeerDiagnosticRunes]) + "…"
}

// boundCriterionReason keeps the first maxPeerCriterionReasonRunes runes,
// marking the cut the same way boundLine does, at the reason's own wider
// allowance instead of the general diagnostic bound.
func boundCriterionReason(s string) string {
	if len(s) <= maxPeerCriterionReasonRunes {
		return s // runes never outnumber bytes: nothing to cut
	}
	r := []rune(s)
	if len(r) <= maxPeerCriterionReasonRunes {
		return s
	}
	return string(r[:maxPeerCriterionReasonRunes]) + "…"
}
