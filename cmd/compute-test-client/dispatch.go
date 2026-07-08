package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"

	cepb "github.com/cyoda-platform/cyoda-go/api/grpc/cloudevents"
	cyodapb "github.com/cyoda-platform/cyoda-go/api/grpc/cyoda"
)

// CloudEvent type constants — duplicated here so the compute-test-client
// binary does not import internal/grpc (keeps it a standalone binary).
const (
	ceTypeJoin              = "CalculationMemberJoinEvent"
	ceTypeGreet             = "CalculationMemberGreetEvent"
	ceTypeKeepAlive         = "CalculationMemberKeepAliveEvent"
	ceTypeProcessorRequest  = "EntityProcessorCalculationRequest"
	ceTypeProcessorResponse = "EntityProcessorCalculationResponse"
	ceTypeCriteriaRequest   = "EntityCriteriaCalculationRequest"
	ceTypeCriteriaResponse  = "EntityCriteriaCalculationResponse"
)

// dispatcher manages the gRPC connection to cyoda and the dispatch loop.
type dispatcher struct {
	endpoint string
	token    string
	cat      *catalog
	conn     *grpc.ClientConn
	memberID string
}

// newDispatcher creates a dispatcher targeting the given cyoda gRPC endpoint.
func newDispatcher(endpoint, token string, cat *catalog) *dispatcher {
	return &dispatcher{
		endpoint: endpoint,
		token:    token,
		cat:      cat,
	}
}

// connect dials the gRPC endpoint, opens the StartStreaming bidi stream,
// sends the join event, and waits for the greet response. On success it
// stores the assigned memberID.
func (d *dispatcher) connect(ctx context.Context) (grpc.BidiStreamingClient[cepb.CloudEvent, cepb.CloudEvent], error) {
	conn, err := grpc.NewClient(d.endpoint,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to dial gRPC endpoint %s: %w", d.endpoint, err)
	}
	d.conn = conn

	client := cyodapb.NewCloudEventsServiceClient(conn)

	// Attach the JWT as per-call metadata.
	md := metadata.Pairs("authorization", "Bearer "+d.token)
	streamCtx := metadata.NewOutgoingContext(ctx, md)

	stream, err := client.StartStreaming(streamCtx)
	if err != nil {
		return nil, fmt.Errorf("failed to open StartStreaming: %w", err)
	}

	// Send join event.
	joinPayload := map[string]any{
		"id":                  uuid.NewString(),
		"tags":                []string{"compute-test-client"},
		"joinedLegalEntityId": "",
		"success":             true,
	}
	joinCE, err := newCloudEvent(ceTypeJoin, joinPayload)
	if err != nil {
		return nil, fmt.Errorf("failed to create join event: %w", err)
	}
	if err := stream.Send(joinCE); err != nil {
		return nil, fmt.Errorf("failed to send join event: %w", err)
	}
	slog.Info("join event sent", "pkg", "compute-test-client")

	// Wait for greet event.
	greetCE, err := stream.Recv()
	if err != nil {
		return nil, fmt.Errorf("failed to receive greet event: %w", err)
	}
	if greetCE.Type != ceTypeGreet {
		return nil, fmt.Errorf("expected %s, got %s", ceTypeGreet, greetCE.Type)
	}
	payload, err := extractTextData(greetCE)
	if err != nil {
		return nil, fmt.Errorf("failed to parse greet payload: %w", err)
	}
	var greet struct {
		MemberID string `json:"memberId"`
	}
	if err := json.Unmarshal(payload, &greet); err != nil {
		return nil, fmt.Errorf("failed to unmarshal greet: %w", err)
	}
	d.memberID = greet.MemberID
	slog.Info("greet received", "pkg", "compute-test-client", "memberId", d.memberID)

	return stream, nil
}

// run enters the dispatch loop, reading events from the stream and routing
// them to the catalog. It also starts a keep-alive goroutine. This method
// blocks until the context is cancelled or the stream is closed.
func (d *dispatcher) run(ctx context.Context, stream grpc.BidiStreamingClient[cepb.CloudEvent, cepb.CloudEvent]) error {
	// Start keep-alive goroutine.
	go d.keepAliveLoop(ctx, stream)

	for {
		msg, err := stream.Recv()
		if err != nil {
			select {
			case <-ctx.Done():
				return ctx.Err()
			default:
			}
			return fmt.Errorf("stream recv: %w", err)
		}

		payload, err := extractTextData(msg)
		if err != nil {
			slog.Warn("malformed CloudEvent", "pkg", "compute-test-client", "error", err)
			continue
		}

		// The signed tx-token (feature #287) rides as a CloudEvent extension
		// attribute; it is echoed on joined callbacks. Empty when the dispatch
		// carries no transaction context. Never logged (Gate 3).
		txToken := txTokenFromCloudEvent(msg)

		switch msg.Type {
		case ceTypeProcessorRequest:
			resp, err := d.handleProcessorRequest(ctx, payload, txToken)
			if err != nil {
				slog.Error("processor dispatch failed", "pkg", "compute-test-client", "error", err)
				continue
			}
			if err := stream.Send(resp); err != nil {
				slog.Error("failed to send processor response", "pkg", "compute-test-client", "error", err)
			}

		case ceTypeCriteriaRequest:
			resp, err := d.handleCriteriaRequest(ctx, payload, txToken)
			if err != nil {
				slog.Error("criteria dispatch failed", "pkg", "compute-test-client", "error", err)
				continue
			}
			if err := stream.Send(resp); err != nil {
				slog.Error("failed to send criteria response", "pkg", "compute-test-client", "error", err)
			}

		case ceTypeKeepAlive:
			slog.Debug("keep-alive received from server", "pkg", "compute-test-client")

		default:
			slog.Debug("ignoring unknown event type", "pkg", "compute-test-client", "type", msg.Type)
		}
	}
}

// close tears down the gRPC connection.
func (d *dispatcher) close() {
	if d.conn != nil {
		d.conn.Close()
	}
}

// keepAliveLoop sends keep-alive events every 10 seconds.
func (d *dispatcher) keepAliveLoop(ctx context.Context, stream grpc.BidiStreamingClient[cepb.CloudEvent, cepb.CloudEvent]) {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			ka := map[string]any{
				"id":       uuid.NewString(),
				"memberId": d.memberID,
				"success":  true,
			}
			ce, err := newCloudEvent(ceTypeKeepAlive, ka)
			if err != nil {
				slog.Error("failed to create keep-alive event", "pkg", "compute-test-client", "error", err)
				continue
			}
			if err := stream.Send(ce); err != nil {
				slog.Error("failed to send keep-alive", "pkg", "compute-test-client", "error", err)
				return
			}
			slog.Debug("keep-alive sent", "pkg", "compute-test-client")
		}
	}
}

// handleProcessorRequest dispatches a processor request to the catalog and
// returns the response CloudEvent.
func (d *dispatcher) handleProcessorRequest(ctx context.Context, payload json.RawMessage, txToken string) (*cepb.CloudEvent, error) {
	var req struct {
		RequestID     string          `json:"requestId"`
		ProcessorID   string          `json:"processorId"`
		ProcessorName string          `json:"processorName"`
		EntityID      string          `json:"entityId"`
		Parameters    json.RawMessage `json:"parameters"`
		Payload       *struct {
			Data json.RawMessage `json:"data"`
			Meta json.RawMessage `json:"meta"`
		} `json:"payload"`
	}
	if err := json.Unmarshal(payload, &req); err != nil {
		return nil, fmt.Errorf("failed to unmarshal processor request: %w", err)
	}

	name := req.ProcessorName
	if name == "" {
		name = req.ProcessorID
	}
	slog.Info("processor request", "pkg", "compute-test-client", "requestId", req.RequestID, "processor", name, "entityId", req.EntityID)

	// Build entity from payload data.
	entity := &Entity{
		ID: req.EntityID,
	}
	if req.Payload != nil && req.Payload.Data != nil {
		entity.Data = req.Payload.Data
	}

	// Callback-capable processors (feature #287) take precedence: they receive
	// the tx-token and the callback client to issue joined callbacks.
	if cbFn, ok := d.cat.callbackProcessor(name); ok {
		cfg, err := parseCallbackConfig(req.Parameters)
		if err != nil {
			return d.buildProcessorResponse(req.RequestID, req.EntityID, nil, false, err.Error())
		}
		result, err := cbFn(ctx, entity, cfg, txToken, d.cat.cb)
		if err != nil {
			return d.buildProcessorResponse(req.RequestID, req.EntityID, nil, false, err.Error())
		}
		return d.buildProcessorResponse(req.RequestID, req.EntityID, result.Data, true, "")
	}

	procFn, ok := d.cat.processor(name)
	if !ok {
		return d.buildProcessorResponse(req.RequestID, req.EntityID, nil, false, fmt.Sprintf("unknown processor: %s", name))
	}

	result, err := procFn(ctx, entity, req.Parameters)
	if err != nil {
		return d.buildProcessorResponse(req.RequestID, req.EntityID, nil, false, err.Error())
	}

	return d.buildProcessorResponse(req.RequestID, req.EntityID, result.Data, true, "")
}

// buildProcessorResponse constructs an EntityProcessorCalculationResponse CloudEvent.
func (d *dispatcher) buildProcessorResponse(requestID, entityID string, data json.RawMessage, success bool, errMsg string) (*cepb.CloudEvent, error) {
	resp := map[string]any{
		"id":        uuid.NewString(),
		"requestId": requestID,
		"entityId":  entityID,
		"success":   success,
	}
	if data != nil {
		resp["payload"] = map[string]any{
			"type": "JSON",
			"data": json.RawMessage(data),
		}
	}
	if errMsg != "" {
		resp["error"] = map[string]any{
			"code":    "PROCESSOR_ERROR",
			"message": errMsg,
		}
	}
	return newCloudEvent(ceTypeProcessorResponse, resp)
}

// handleCriteriaRequest dispatches a criteria request to the catalog and
// returns the response CloudEvent.
func (d *dispatcher) handleCriteriaRequest(ctx context.Context, payload json.RawMessage, txToken string) (*cepb.CloudEvent, error) {
	var req struct {
		RequestID    string          `json:"requestId"`
		CriteriaID   string          `json:"criteriaId"`
		CriteriaName string          `json:"criteriaName"`
		EntityID     string          `json:"entityId"`
		Parameters   json.RawMessage `json:"parameters"`
		Payload      *struct {
			Data json.RawMessage `json:"data"`
			Meta json.RawMessage `json:"meta"`
		} `json:"payload"`
	}
	if err := json.Unmarshal(payload, &req); err != nil {
		return nil, fmt.Errorf("failed to unmarshal criteria request: %w", err)
	}

	name := req.CriteriaName
	if name == "" {
		name = req.CriteriaID
	}
	slog.Info("criteria request", "pkg", "compute-test-client", "requestId", req.RequestID, "criteria", name, "entityId", req.EntityID)

	entity := &Entity{
		ID: req.EntityID,
	}
	if req.Payload != nil && req.Payload.Data != nil {
		entity.Data = req.Payload.Data
	}

	// Callback-capable criteria (feature #287) take precedence.
	if cbFn, ok := d.cat.callbackCriterion(name); ok {
		cfg, err := parseCallbackConfig(req.Parameters)
		if err != nil {
			return d.buildCriteriaResponse(req.RequestID, req.EntityID, false, false, err.Error())
		}
		matches, err := cbFn(ctx, entity, cfg, txToken, d.cat.cb)
		if err != nil {
			return d.buildCriteriaResponse(req.RequestID, req.EntityID, false, false, err.Error())
		}
		return d.buildCriteriaResponse(req.RequestID, req.EntityID, matches, true, "")
	}

	critFn, ok := d.cat.criterion(name)
	if !ok {
		return d.buildCriteriaResponse(req.RequestID, req.EntityID, false, false, fmt.Sprintf("unknown criterion: %s", name))
	}

	matches, err := critFn(ctx, entity, req.Parameters)
	if err != nil {
		return d.buildCriteriaResponse(req.RequestID, req.EntityID, false, false, err.Error())
	}

	return d.buildCriteriaResponse(req.RequestID, req.EntityID, matches, true, "")
}

// buildCriteriaResponse constructs an EntityCriteriaCalculationResponse CloudEvent.
func (d *dispatcher) buildCriteriaResponse(requestID, entityID string, matches, success bool, errMsg string) (*cepb.CloudEvent, error) {
	resp := map[string]any{
		"id":        uuid.NewString(),
		"requestId": requestID,
		"entityId":  entityID,
		"success":   success,
		"matches":   matches,
	}
	if errMsg != "" {
		resp["error"] = map[string]any{
			"code":    "CRITERIA_ERROR",
			"message": errMsg,
		}
	}
	return newCloudEvent(ceTypeCriteriaResponse, resp)
}

// --- CloudEvent helpers ---

// newCloudEvent creates a CloudEvent with the given type and JSON-marshalled payload.
func newCloudEvent(eventType string, payload any) (*cepb.CloudEvent, error) {
	data, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal CloudEvent payload: %w", err)
	}
	return &cepb.CloudEvent{
		Id:          uuid.NewString(),
		Source:      "compute-test-client",
		SpecVersion: "1.0",
		Type:        eventType,
		Data:        &cepb.CloudEvent_TextData{TextData: string(data)},
	}, nil
}

// extractTextData extracts the text data payload from a CloudEvent.
func extractTextData(ce *cepb.CloudEvent) (json.RawMessage, error) {
	switch d := ce.Data.(type) {
	case *cepb.CloudEvent_TextData:
		return json.RawMessage(d.TextData), nil
	case *cepb.CloudEvent_BinaryData:
		return json.RawMessage(d.BinaryData), nil
	default:
		return nil, fmt.Errorf("unsupported CloudEvent data variant: %T", ce.Data)
	}
}
