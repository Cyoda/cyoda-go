package grpc

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"

	googlegrpc "google.golang.org/grpc"

	spi "github.com/cyoda-platform/cyoda-go-spi"
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
	txMgr         spi.TransactionManager
	pool          *proxy.ClientPool
	localGRPCPort int

	// forward seams — overridable in tests.
	forwardUnary        forwardUnaryFn
	forwardStream       forwardStreamFn
	forwardSearchUnary  forwardUnaryFn
	forwardSearchStream forwardStreamFn
}

func newTxRouteInterceptor(signer *token.Signer, reg contract.NodeRegistry, selfNodeID string, txMgr spi.TransactionManager, localGRPCPort int, allowLoopback bool) *txRouteInterceptor {
	return &txRouteInterceptor{
		signer:              signer,
		registry:            reg,
		selfNodeID:          selfNodeID,
		txMgr:               txMgr,
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

// classifyRouteErr maps a proxy.ResolveTarget error onto the canonical
// operational codes (mirroring txjoin.JoinFromToken), so the envelope carries a
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
// the authenticated UserContext is already on ctx for JoinFromToken's tenant
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

		joinedCtx, jerr := txjoin.JoinFromToken(ctx, i.signer, i.txMgr, tok)
		if jerr != nil {
			return i.unaryErr(ctx, ce, envelope, jerr)
		}
		return handler(joinedCtx, req)
	}
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

		joinedCtx, jerr := txjoin.JoinFromToken(ctx, i.signer, i.txMgr, tok)
		if jerr != nil {
			return i.streamErr(ss, "", envelope, jerr)
		}
		return handler(srv, &wrappedStream{ServerStream: ss, ctx: joinedCtx})
	}
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
