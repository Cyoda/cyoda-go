package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"

	cyodapb "github.com/cyoda-platform/cyoda-go/api/grpc/cyoda"
)

// grpc_callback.go — feature #287 gRPC-transport callback for the
// compute-test-client. A processor may issue its joined callback as a gRPC
// EntityManage(EntityCreateRequest) instead of an HTTP POST, presenting the
// signed cyodatxtoken as the "tx-token" gRPC metadata key. When that call lands
// on a NON-owner node the txRouteInterceptor forwards it to the owner node
// (B→A EntityManage forward) where it joins the originating transaction T.
//
// The token value is never logged (Gate 3) — only echoed as metadata.

// ceTypeEntityCreateRequest / ceTypeEntityTransactionResponse duplicate the
// internal/grpc CloudEvent type constants so this binary stays free of internal/
// imports.
const (
	ceTypeEntityCreateRequest = "EntityCreateRequest"
	ceTypeEntitySearchRequest = "EntitySearchRequest"
	grpcTxTokenKey            = "tx-token"
)

// grpcCallbackClient issues EntityManage callbacks over gRPC, joining T via the
// tx-token metadata. It dials a (possibly non-owner) cyoda-go node.
type grpcCallbackClient struct {
	conn   *grpc.ClientConn
	client cyodapb.CloudEventsServiceClient
	bearer string
}

// newGRPCCallbackClient dials endpoint for EntityManage callbacks, or returns nil
// when endpoint is empty (the gRPC callback processors then fail loudly).
func newGRPCCallbackClient(endpoint, bearer string) (*grpcCallbackClient, error) {
	if endpoint == "" {
		return nil, nil
	}
	conn, err := grpc.NewClient(endpoint, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, fmt.Errorf("dial gRPC callback endpoint %s: %w", endpoint, err)
	}
	return &grpcCallbackClient{
		conn:   conn,
		client: cyodapb.NewCloudEventsServiceClient(conn),
		bearer: bearer,
	}, nil
}

// close tears down the callback connection.
func (g *grpcCallbackClient) close() {
	if g != nil && g.conn != nil {
		g.conn.Close()
	}
}

// grpcCBResult is the parsed EntityTransactionResponse envelope of a gRPC callback.
type grpcCBResult struct {
	Success   bool
	EntityID  string
	TxID      string
	ErrorCode string
	ErrorMsg  string
}

// createSecondary issues an EntityManage(EntityCreateRequest) callback that
// creates a secondary entity within the joined transaction T. The tx-token rides
// as gRPC "tx-token" metadata; when txToken names a remote owner the receiving
// node forwards the whole EntityManage call there (B→A).
func (g *grpcCallbackClient) createSecondary(ctx context.Context, cfg cbConfig, txToken, status string) (grpcCBResult, error) {
	version := cfg.SecondaryVersion
	if version == 0 {
		version = 1
	}
	reqPayload := map[string]any{
		"id":         uuid.NewString(),
		"dataFormat": "JSON",
		"payload": map[string]any{
			"model": map[string]any{"name": cfg.SecondaryModel, "version": version},
			"data":  map[string]any{"name": "child", "amount": 1, "status": status},
		},
	}
	ce, err := newCloudEvent(ceTypeEntityCreateRequest, reqPayload)
	if err != nil {
		return grpcCBResult{}, fmt.Errorf("build EntityCreateRequest: %w", err)
	}

	pairs := []string{"authorization", "Bearer " + g.bearer}
	if txToken != "" {
		pairs = append(pairs, grpcTxTokenKey, txToken)
	}
	md := metadata.Pairs(pairs...)
	callCtx, cancel := context.WithTimeout(metadata.NewOutgoingContext(ctx, md), 15*time.Second)
	defer cancel()

	resp, err := g.client.EntityManage(callCtx, ce)
	if err != nil {
		return grpcCBResult{}, fmt.Errorf("EntityManage callback: %w", err)
	}

	payload, err := extractTextData(resp)
	if err != nil {
		return grpcCBResult{}, fmt.Errorf("extract EntityManage response: %w", err)
	}
	var env struct {
		Success         bool `json:"success"`
		TransactionInfo struct {
			EntityIds     []string `json:"entityIds"`
			TransactionID *string  `json:"transactionId"`
		} `json:"transactionInfo"`
		Error *struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(payload, &env); err != nil {
		return grpcCBResult{}, fmt.Errorf("decode EntityTransactionResponse: %w", err)
	}
	res := grpcCBResult{Success: env.Success}
	if len(env.TransactionInfo.EntityIds) > 0 {
		res.EntityID = env.TransactionInfo.EntityIds[0]
	}
	if env.TransactionInfo.TransactionID != nil {
		res.TxID = *env.TransactionInfo.TransactionID
	}
	if env.Error != nil {
		res.ErrorCode = env.Error.Code
		res.ErrorMsg = env.Error.Message
	}
	return res, nil
}

// searchSecondary issues a streaming EntitySearchCollection(EntitySearchRequest)
// callback matching data.status == status over (cfg.SecondaryModel, version),
// presenting the tx-token as gRPC "tx-token" metadata. When txToken is set the
// search joins T (via the txRouteInterceptor's search routing) and observes T's
// uncommitted writes; without it the search runs standalone against committed
// state. Returns the number of matched entities.
func (g *grpcCallbackClient) searchSecondary(ctx context.Context, cfg cbConfig, txToken, status string) (count int, err error) {
	version := cfg.SecondaryVersion
	if version == 0 {
		version = 1
	}
	reqPayload := map[string]any{
		"id":    uuid.NewString(),
		"model": map[string]any{"name": cfg.SecondaryModel, "version": version},
		"condition": map[string]any{
			"type":         "simple",
			"jsonPath":     "$.status",
			"operatorType": "EQUALS",
			"value":        status,
		},
	}
	ce, err := newCloudEvent(ceTypeEntitySearchRequest, reqPayload)
	if err != nil {
		return 0, fmt.Errorf("build EntitySearchRequest: %w", err)
	}

	pairs := []string{"authorization", "Bearer " + g.bearer}
	if txToken != "" {
		pairs = append(pairs, grpcTxTokenKey, txToken)
	}
	callCtx, cancel := context.WithTimeout(metadata.NewOutgoingContext(ctx, metadata.Pairs(pairs...)), 15*time.Second)
	defer cancel()

	stream, err := g.client.EntitySearchCollection(callCtx, ce)
	if err != nil {
		return 0, fmt.Errorf("EntitySearchCollection callback: %w", err)
	}
	for {
		frame, rerr := stream.Recv()
		if errors.Is(rerr, io.EOF) {
			break
		}
		if rerr != nil {
			return count, fmt.Errorf("search stream recv: %w", rerr)
		}
		payload, xerr := extractTextData(frame)
		if xerr != nil {
			return count, fmt.Errorf("extract search frame: %w", xerr)
		}
		var env struct {
			Success bool `json:"success"`
			Error   *struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if uerr := json.Unmarshal(payload, &env); uerr != nil {
			return count, fmt.Errorf("decode search frame: %w", uerr)
		}
		if !env.Success {
			msg := ""
			if env.Error != nil {
				msg = env.Error.Code + ": " + env.Error.Message
			}
			return count, fmt.Errorf("search returned error frame: %s", msg)
		}
		count++
	}
	return count, nil
}
