package grpc

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"

	googlegrpc "google.golang.org/grpc"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	cepb "github.com/cyoda-platform/cyoda-go/api/grpc/cloudevents"
	cyodapb "github.com/cyoda-platform/cyoda-go/api/grpc/cyoda"
	"github.com/cyoda-platform/cyoda-go/internal/cluster/proxy"
	"github.com/cyoda-platform/cyoda-go/internal/cluster/token"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/contract"
	"github.com/cyoda-platform/cyoda-go/internal/domain/txjoin"
)

// forwardUnaryFn re-issues a unary CloudEvent RPC to a peer node.
type forwardUnaryFn func(context.Context, *proxy.ClientPool, string, *cepb.CloudEvent) (*cepb.CloudEvent, error)

// forwardStreamFn re-issues a server-streaming CloudEvent RPC to a peer node.
type forwardStreamFn func(context.Context, *proxy.ClientPool, string, *cepb.CloudEvent) (googlegrpc.ServerStreamingClient[cepb.CloudEvent], error)

// envelopeFn renders a routing/join failure as a schema-valid error CloudEvent
// for a given RPC family, so the client parses it like any other failure of
// that RPC rather than a raw gRPC status.
type envelopeFn func(ctx context.Context, ceID string, err error) (*cepb.CloudEvent, error)

// txRouteInterceptor routes inbound entity callbacks by their tx-token: a token
// for the local node joins the referenced transaction onto the request context;
// a token for a live peer forwards the whole call to that peer (B→A). It covers
// both the write RPCs (EntityManage / EntityManageCollection) and the read RPCs
// (EntitySearch / EntitySearchCollection) — a callback that presents a valid
// token joins T for reads too, so reads-your-own-writes is symmetric with writes.
//
// Routing/join failures are returned as that RPC's error envelope
// (Success=false) — the transaction envelope for the write RPCs, the entity
// response envelope for the search RPCs — never as a raw gRPC status, so clients
// read them the same way as any other failure of that RPC.
type txRouteInterceptor struct {
	signer        *token.Signer
	registry      contract.NodeRegistry
	selfNodeID    string
	joiner        *txjoin.Joiner
	pool          *proxy.ClientPool
	localGRPCPort int

	// forward seams — overridable in tests.
	forwardUnary        forwardUnaryFn
	forwardStream       forwardStreamFn
	forwardSearchUnary  forwardUnaryFn
	forwardSearchStream forwardStreamFn
}

func newTxRouteInterceptor(signer *token.Signer, reg contract.NodeRegistry, selfNodeID string, j *txjoin.Joiner, localGRPCPort int, allowLoopback bool) *txRouteInterceptor {
	return &txRouteInterceptor{
		signer:              signer,
		registry:            reg,
		selfNodeID:          selfNodeID,
		joiner:              j,
		pool:                proxy.NewClientPool(allowLoopback),
		localGRPCPort:       localGRPCPort,
		forwardUnary:        proxy.ForwardEntityManage,
		forwardStream:       proxy.ForwardEntityManageCollection,
		forwardSearchUnary:  proxy.ForwardEntitySearch,
		forwardSearchStream: proxy.ForwardEntitySearchCollection,
	}
}

// unaryRoute reports whether a unary method is tx-token-routed and, if so,
// returns the peer-forward function and the error-envelope builder for its RPC
// family. A non-routed method (routed=false) passes through untouched.
func (i *txRouteInterceptor) unaryRoute(fullMethod string) (forward forwardUnaryFn, envelope envelopeFn, routed bool) {
	switch fullMethod {
	case cyodapb.CloudEventsService_EntityManage_FullMethodName:
		return i.forwardUnary, entityTransactionError, true
	case cyodapb.CloudEventsService_EntitySearch_FullMethodName:
		return i.forwardSearchUnary, entityResponseError, true
	}
	return nil, nil, false
}

// streamRoute is the server-streaming counterpart of unaryRoute.
func (i *txRouteInterceptor) streamRoute(fullMethod string) (forward forwardStreamFn, envelope envelopeFn, routed bool) {
	switch fullMethod {
	case cyodapb.CloudEventsService_EntityManageCollection_FullMethodName:
		return i.forwardStream, entityTransactionError, true
	case cyodapb.CloudEventsService_EntitySearchCollection_FullMethodName:
		return i.forwardSearchStream, entityResponseError, true
	}
	return nil, nil, false
}

// classifyRouteErr maps a proxy.ResolveNodeInfo error onto the canonical
// operational codes (mirroring the join layer's own), so the envelope carries a
// client-facing code rather than a generic server error. Registry-lookup and
// unknown failures fall through unchanged and surface as SERVER_ERROR.
func classifyRouteErr(err error) error {
	switch {
	case errors.Is(err, token.ErrTokenExpired):
		return common.Operational(http.StatusGone, common.ErrCodeTransactionExpired, "transaction token has expired")
	case errors.Is(err, token.ErrTokenTampered), errors.Is(err, token.ErrTokenInvalid):
		return common.Operational(http.StatusUnauthorized, common.ErrCodeUnauthorized, "invalid transaction token")
	case errors.Is(err, proxy.ErrNodeUnavailable):
		return common.Operational(http.StatusServiceUnavailable, common.ErrCodeTransactionNodeUnavailable, "transaction node is not available")
	default:
		return err
	}
}

// unary returns the unary interceptor. It runs after the auth interceptor, so
// the authenticated UserContext is already on ctx for the join layer's tenant
// check.
func (i *txRouteInterceptor) unary() googlegrpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *googlegrpc.UnaryServerInfo, handler googlegrpc.UnaryHandler) (any, error) {
		forward, envelope, routed := i.unaryRoute(info.FullMethod)
		if !routed {
			return handler(ctx, req)
		}
		ce, _ := req.(*cepb.CloudEvent)

		tok := proxy.ExtractGRPCToken(ctx)
		ni, shouldProxy, err := proxy.ResolveNodeInfo(ctx, i.signer, i.registry, i.selfNodeID, tok)
		if err != nil {
			return i.unaryErr(ctx, ce, envelope, classifyRouteErr(err))
		}
		if shouldProxy {
			if ce == nil {
				return i.unaryErr(ctx, nil, envelope, fmt.Errorf("proxy path: request is not a CloudEvent"))
			}
			grpcAddr, addrErr := resolveGRPCAddr(ni, i.localGRPCPort)
			if addrErr != nil {
				return i.unaryErr(ctx, ce, envelope, fmt.Errorf("resolve peer gRPC addr: %w", addrErr))
			}
			resp, fwdErr := forward(ctx, i.pool, grpcAddr, ce)
			if fwdErr != nil {
				return i.unaryErr(ctx, ce, envelope, fwdErr)
			}
			return resp, nil
		}

		// The message is complete before the interceptor runs, so a unary call
		// needs nothing read ahead of the lock.
		var resp any
		var herr error
		if jerr := i.joiner.Run(ctx, tok, func(joined context.Context) { resp, herr = handler(joined, req) }); jerr != nil {
			if clientGone(jerr) {
				return nil, status.FromContextError(jerr).Err()
			}
			return i.unaryErr(ctx, ce, envelope, jerr)
		}
		// The answer is complete by the time the handler returns — as it is on
		// the HTTP door, where the encoder has materialised it before the
		// buffered writer sees it — so what the ceiling bounds here is what is
		// SENT, not the peak this node held while building it. Past it the call
		// fails with the JOINED_RESPONSE_TOO_LARGE the other two doors give: the
		// three doors answer one contract, and a truncated answer is not one of
		// its answers. An unjoined call holds no transaction and is not governed.
		if tok != "" && overJoinedCeiling(resp, i.joiner.MaxResponseBytes()) {
			return i.unaryErr(ctx, ce, envelope, i.joiner.ResponseTooLargeError())
		}
		return resp, herr
	}
}

// overJoinedCeiling reports whether a unary answer passes the joined-answer
// ceiling. Every answer of these RPCs is a CloudEvent; its encoded size is what
// the node sends once the transaction's lock has been given back, the same
// measure heldStream takes of each frame it holds.
func overJoinedCeiling(resp any, limit int) bool {
	pm, ok := resp.(proto.Message)
	return ok && proto.Size(pm) > limit
}

// clientGone reports whether err is the call's own context ending, as the join
// layer marks it: the compute node went away while its request was queued for
// the transaction's lock, so nothing was touched and there is nobody to answer
// with an envelope. A join that FAILED carrying an unrelated cancellation is
// not this — its client is still there, waiting for an answer, and gets the
// envelope and the ticket that name the fault.
func clientGone(err error) bool {
	return errors.Is(err, common.ErrClientGone)
}

// unaryErr renders err as the routed RPC's error envelope. If the request could
// not be parsed as a CloudEvent the id is empty.
func (i *txRouteInterceptor) unaryErr(ctx context.Context, ce *cepb.CloudEvent, envelope envelopeFn, err error) (any, error) {
	id := ""
	if ce != nil {
		id = ce.Id
	}
	return envelope(common.WithDiagnostics(ctx), id, err)
}

// stream returns the stream interceptor for the routed server-streaming RPCs
// (EntityManageCollection and EntitySearchCollection).
func (i *txRouteInterceptor) stream() googlegrpc.StreamServerInterceptor {
	return func(srv any, ss googlegrpc.ServerStream, info *googlegrpc.StreamServerInfo, handler googlegrpc.StreamHandler) error {
		forward, envelope, routed := i.streamRoute(info.FullMethod)
		if !routed {
			return handler(srv, ss)
		}
		ctx := ss.Context()

		tok := proxy.ExtractGRPCToken(ctx)
		ni, shouldProxy, err := proxy.ResolveNodeInfo(ctx, i.signer, i.registry, i.selfNodeID, tok)
		if err != nil {
			return i.streamErr(ss, "", envelope, classifyRouteErr(err))
		}
		if shouldProxy {
			grpcAddr, addrErr := resolveGRPCAddr(ni, i.localGRPCPort)
			if addrErr != nil {
				return i.streamErr(ss, "", envelope, fmt.Errorf("resolve peer gRPC addr: %w", addrErr))
			}
			return i.proxyStream(ctx, ss, forward, envelope, grpcAddr)
		}

		if tok == "" {
			return handler(srv, ss)
		}
		// The pass itself has been checked before this line: ResolveNodeInfo
		// verifies it — signature, shape, expiry — to decide which node serves
		// the call, and classifyRouteErr answers a bad one with the join
		// layer's own 401 / 410 above, before any message is received. The
		// joiner verifies it again to hold it as a Pass; what is left after
		// that needs the request's identity and the fence.
		pass, err := i.joiner.Verify(tok)
		if err != nil {
			return i.streamErr(ss, "", envelope, err)
		}
		// How many callbacks may queue for one transaction is bounded, and the
		// bound is read before the request message is taken off the stream: a
		// refusal costs this node no buffer. The gate applies the same bound
		// again when the lock is taken, and that answer is the binding one.
		if err := i.joiner.CheckRoom(pass); err != nil {
			return i.streamErr(ss, "", envelope, err)
		}
		// Receive the request before the lock is taken (see heldStream).
		var first cepb.CloudEvent
		if err := ss.RecvMsg(&first); err != nil {
			return err
		}
		held := &heldStream{ServerStream: ss, ctx: ctx, first: &first, limit: i.joiner.MaxResponseBytes()}
		var herr error
		if jerr := i.joiner.RunVerified(ctx, pass, func(joined context.Context) {
			held.ctx = joined
			herr = handler(srv, held)
		}); jerr != nil {
			if clientGone(jerr) {
				return status.FromContextError(jerr).Err()
			}
			return i.streamErr(ss, first.Id, envelope, jerr)
		}
		if held.tooLarge() {
			// Fail closed: nothing of an over-size answer is sent. The lock has
			// already been given back.
			return i.streamErr(ss, first.Id, envelope, i.joiner.ResponseTooLargeError())
		}
		// What the handler wrote is delivered even when it then failed: a joined
		// chunked collection that fails at chunk n still answers chunks 1…n-1,
		// as an unheld stream does.
		if err := held.flush(); err != nil {
			return err
		}
		return herr
	}
}

// heldStream is the stream a joined server-streaming handler sees. The handler
// runs under its transaction's lock, and neither end of the stream may make
// that lock wait on the compute node: the request message was received before
// the lock was taken and is replayed here, and every response frame is held
// until the handler has returned and the lock is released.
//
// What is held is bounded by the joiner's own ceiling
// (CYODA_CALLOUT_JOINED_RESPONSE_MAX_BYTES) — the owner's next move waits behind
// these bytes. Past the ceiling the frames are let go of and the call fails: a
// collection answered in part would be a wrong answer.
type heldStream struct {
	googlegrpc.ServerStream
	ctx   context.Context
	first *cepb.CloudEvent
	held  []any
	bytes int
	limit int
	over  bool
}

func (s *heldStream) Context() context.Context { return s.ctx }

func (s *heldStream) RecvMsg(m any) error {
	if s.first == nil {
		// The request of a server-streaming RPC is one message, and it has been
		// replayed: the stream is at its end. Delegating instead would wait on
		// the compute node — for its half-close — while the handler holds the
		// transaction's lock.
		return io.EOF
	}
	dst, ok := m.(proto.Message)
	if !ok {
		return fmt.Errorf("failed to replay request: %T is not a proto message", m)
	}
	proto.Merge(dst, s.first)
	s.first = nil
	return nil
}

func (s *heldStream) SendMsg(m any) error {
	// Every frame of these RPCs is a CloudEvent; its encoded size is what the
	// node holds until the lock is released.
	if pm, ok := m.(proto.Message); ok {
		s.bytes += proto.Size(pm)
	}
	if s.over || s.bytes > s.limit {
		s.over = true
		s.held = nil // held under the transaction's lock: let it go at once
		return txjoin.ErrHeldResponseTooLarge
	}
	s.held = append(s.held, m)
	return nil
}

// tooLarge reports whether the handler's frames passed the ceiling. The
// interceptor asks after the handler has returned: a handler that swallows the
// refusal must not have its collection answered in part either.
func (s *heldStream) tooLarge() bool { return s.over }

func (s *heldStream) flush() error {
	for _, m := range s.held {
		if err := s.ServerStream.SendMsg(m); err != nil {
			return err
		}
	}
	return nil
}

// proxyStream consumes the inbound request message, re-issues the
// server-streaming call to the owner node, and copies every response frame back
// onto the inbound stream verbatim.
func (i *txRouteInterceptor) proxyStream(ctx context.Context, ss googlegrpc.ServerStream, forward forwardStreamFn, envelope envelopeFn, addr string) error {
	var ce cepb.CloudEvent
	if err := ss.RecvMsg(&ce); err != nil {
		return err
	}
	cs, err := forward(ctx, i.pool, addr, &ce)
	if err != nil {
		return i.streamErr(ss, ce.Id, envelope, err)
	}
	for {
		frame, err := cs.Recv()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if err := ss.SendMsg(frame); err != nil {
			return err
		}
	}
}

// streamErr sends err as the routed RPC's error envelope on the stream. reqID
// is the already-consumed request id (empty string when no request has been
// read yet — pre-body token rejections legitimately have no request id).
func (i *txRouteInterceptor) streamErr(ss googlegrpc.ServerStream, reqID string, envelope envelopeFn, err error) error {
	respCE, buildErr := envelope(common.WithDiagnostics(ss.Context()), reqID, err)
	if buildErr != nil {
		slog.Error("failed to build tx-route error envelope", "err", buildErr)
		return buildErr
	}
	return ss.SendMsg(respCE)
}
