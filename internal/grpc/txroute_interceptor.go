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

// txRouteInterceptor routes inbound EntityManage / EntityManageCollection
// callbacks by their tx-token: a token for the local node joins the referenced
// transaction onto the request context; a token for a live peer forwards the
// whole call to that peer (B→A). Routing/join failures are returned as the
// EntityManage error envelope (Success=false), never as a raw gRPC status, so
// clients read them the same way as any other EntityManage failure.
type txRouteInterceptor struct {
	signer        *token.Signer
	registry      contract.NodeRegistry
	selfNodeID    string
	txMgr         spi.TransactionManager
	pool          *proxy.ClientPool
	localGRPCPort int

	// forward seams — overridable in tests.
	forwardUnary  func(context.Context, *proxy.ClientPool, string, *cepb.CloudEvent) (*cepb.CloudEvent, error)
	forwardStream func(context.Context, *proxy.ClientPool, string, *cepb.CloudEvent) (googlegrpc.ServerStreamingClient[cepb.CloudEvent], error)
}

func newTxRouteInterceptor(signer *token.Signer, reg contract.NodeRegistry, selfNodeID string, txMgr spi.TransactionManager, localGRPCPort int) *txRouteInterceptor {
	return &txRouteInterceptor{
		signer:        signer,
		registry:      reg,
		selfNodeID:    selfNodeID,
		txMgr:         txMgr,
		pool:          proxy.NewClientPool(),
		localGRPCPort: localGRPCPort,
		forwardUnary:  proxy.ForwardEntityManage,
		forwardStream: proxy.ForwardEntityManageCollection,
	}
}

func isEntityManage(fullMethod string) bool {
	return fullMethod == cyodapb.CloudEventsService_EntityManage_FullMethodName
}

func isEntityManageCollection(fullMethod string) bool {
	return fullMethod == cyodapb.CloudEventsService_EntityManageCollection_FullMethodName
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
		if !isEntityManage(info.FullMethod) {
			return handler(ctx, req)
		}
		ce, _ := req.(*cepb.CloudEvent)

		tok := proxy.ExtractGRPCToken(ctx)
		ni, shouldProxy, err := proxy.ResolveNodeInfo(ctx, i.signer, i.registry, i.selfNodeID, tok)
		if err != nil {
			return i.unaryErr(ctx, ce, classifyRouteErr(err))
		}
		if shouldProxy {
			if ce == nil {
				return i.unaryErr(ctx, nil, fmt.Errorf("proxy path: request is not a CloudEvent"))
			}
			grpcAddr, addrErr := resolveGRPCAddr(ni, i.localGRPCPort)
			if addrErr != nil {
				return i.unaryErr(ctx, ce, fmt.Errorf("resolve peer gRPC addr: %w", addrErr))
			}
			resp, fwdErr := i.forwardUnary(ctx, i.pool, grpcAddr, ce)
			if fwdErr != nil {
				return i.unaryErr(ctx, ce, fwdErr)
			}
			return resp, nil
		}

		joinedCtx, jerr := txjoin.JoinFromToken(ctx, i.signer, i.txMgr, tok)
		if jerr != nil {
			return i.unaryErr(ctx, ce, jerr)
		}
		return handler(joinedCtx, req)
	}
}

// unaryErr renders err as an EntityManage error envelope. If the request could
// not be parsed as a CloudEvent the id is empty.
func (i *txRouteInterceptor) unaryErr(ctx context.Context, ce *cepb.CloudEvent, err error) (any, error) {
	id := ""
	if ce != nil {
		id = ce.Id
	}
	return entityTransactionError(common.WithDiagnostics(ctx), id, err)
}

// stream returns the stream interceptor for EntityManageCollection.
func (i *txRouteInterceptor) stream() googlegrpc.StreamServerInterceptor {
	return func(srv any, ss googlegrpc.ServerStream, info *googlegrpc.StreamServerInfo, handler googlegrpc.StreamHandler) error {
		if !isEntityManageCollection(info.FullMethod) {
			return handler(srv, ss)
		}
		ctx := ss.Context()

		tok := proxy.ExtractGRPCToken(ctx)
		ni, shouldProxy, err := proxy.ResolveNodeInfo(ctx, i.signer, i.registry, i.selfNodeID, tok)
		if err != nil {
			return i.streamErr(ss, "", classifyRouteErr(err))
		}
		if shouldProxy {
			grpcAddr, addrErr := resolveGRPCAddr(ni, i.localGRPCPort)
			if addrErr != nil {
				return i.streamErr(ss, "", fmt.Errorf("resolve peer gRPC addr: %w", addrErr))
			}
			return i.proxyStream(ctx, ss, grpcAddr)
		}

		joinedCtx, jerr := txjoin.JoinFromToken(ctx, i.signer, i.txMgr, tok)
		if jerr != nil {
			return i.streamErr(ss, "", jerr)
		}
		return handler(srv, &wrappedStream{ServerStream: ss, ctx: joinedCtx})
	}
}

// proxyStream consumes the inbound request message, re-issues the
// server-streaming call to the owner node, and copies every response frame back
// onto the inbound stream verbatim.
func (i *txRouteInterceptor) proxyStream(ctx context.Context, ss googlegrpc.ServerStream, addr string) error {
	var ce cepb.CloudEvent
	if err := ss.RecvMsg(&ce); err != nil {
		return err
	}
	cs, err := i.forwardStream(ctx, i.pool, addr, &ce)
	if err != nil {
		return i.streamErr(ss, ce.Id, err)
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

// streamErr sends err as an EntityManage error envelope on the stream. reqID
// is the already-consumed request id (empty string when no request has been
// read yet — pre-body token rejections legitimately have no request id).
func (i *txRouteInterceptor) streamErr(ss googlegrpc.ServerStream, reqID string, err error) error {
	respCE, buildErr := entityTransactionError(common.WithDiagnostics(ss.Context()), reqID, err)
	if buildErr != nil {
		slog.Error("failed to build EntityManage error envelope", "err", buildErr)
		return buildErr
	}
	return ss.SendMsg(respCE)
}
