package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
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
	ceTypeFunctionRequest   = "EntityFunctionCalculationRequest"
	ceTypeFunctionResponse  = "EntityFunctionCalculationResponse"
)

// dispatcher manages the gRPC connection to cyoda and the dispatch loop.
type dispatcher struct {
	endpoint  string
	token     string
	cat       *catalog
	gcb       *grpcCallbackClient
	tags      []string
	behaviour behaviour
	rec       *recorder
	conn      *grpc.ClientConn
	memberID  string

	// sendMu serialises every write to the stream: the request loop and the
	// keep-alive ticker both send, and grpc-go forbids concurrent SendMsg on
	// one stream. Compute-node implementations must do the same.
	sendMu sync.Mutex

	// held are the callouts a late-callback client took and has not yet
	// called back for.
	heldMu sync.Mutex
	held   []heldCallout
}

// heldCallout is a callout whose pass is kept, in memory only, for a late callback.
type heldCallout struct {
	seq    int
	cfg    cbConfig
	cfgErr string
	pass   string
}

// send is the only place this client writes to its stream.
func (d *dispatcher) send(stream grpc.BidiStreamingClient[cepb.CloudEvent, cepb.CloudEvent], ce *cepb.CloudEvent) error {
	d.sendMu.Lock()
	defer d.sendMu.Unlock()
	return stream.Send(ce)
}

// newDispatcher creates a dispatcher targeting the given cyoda gRPC endpoint.
func newDispatcher(endpoint, token string, cat *catalog, gcb *grpcCallbackClient, tags []string, beh behaviour, rec *recorder) *dispatcher {
	return &dispatcher{endpoint: endpoint, token: token, cat: cat, gcb: gcb, tags: tags, behaviour: beh, rec: rec}
}

func (d *dispatcher) hold(hc heldCallout) {
	d.heldMu.Lock()
	defer d.heldMu.Unlock()
	d.held = append(d.held, hc)
}

// takeHeld returns the held callouts and forgets them.
func (d *dispatcher) takeHeld() []heldCallout {
	d.heldMu.Lock()
	defer d.heldMu.Unlock()
	held := d.held
	d.held = nil
	return held
}

// joinPayload is the join event's payload.
func (d *dispatcher) joinPayload() map[string]any {
	return map[string]any{
		"id":                  uuid.NewString(),
		"tags":                d.tags,
		"joinedLegalEntityId": "",
		"success":             true,
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
	joinCE, err := newCloudEvent(ceTypeJoin, d.joinPayload())
	if err != nil {
		return nil, fmt.Errorf("failed to create join event: %w", err)
	}
	if err := d.send(stream, joinCE); err != nil {
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
	d.rec.setMemberID(d.memberID)
	slog.Info("greet received", "pkg", "compute-test-client", "memberId", d.memberID)

	return stream, nil
}

// run enters the dispatch loop, reading events from the stream and routing
// them to the catalog. It also starts a keep-alive goroutine. This method
// blocks until the context is cancelled or the stream is closed.
func (d *dispatcher) run(ctx context.Context, stream grpc.BidiStreamingClient[cepb.CloudEvent, cepb.CloudEvent]) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

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

		switch msg.Type {
		case ceTypeProcessorRequest, ceTypeCriteriaRequest, ceTypeFunctionRequest:
			reply, drop, err := d.handleCallout(ctx, msg, payload)
			if err != nil {
				slog.Error("calculation request failed", "pkg", "compute-test-client", "type", msg.Type, "error", err)
				continue
			}
			if drop {
				slog.Info("closing the stream on receiving work", "pkg", "compute-test-client", "type", msg.Type)
				d.close()
				return nil
			}
			if reply == nil {
				continue
			}
			if err := d.send(stream, reply); err != nil {
				slog.Error("failed to send calculation response", "pkg", "compute-test-client", "type", msg.Type, "error", err)
			}

		case ceTypeKeepAlive:
			slog.Debug("keep-alive received from server", "pkg", "compute-test-client")

		default:
			slog.Debug("ignoring unknown event type", "pkg", "compute-test-client", "type", msg.Type)
		}
	}
}

// handleCallout records one calculation request and decides what this client
// does with it: a reply to send (nil = stay silent) and whether to close the
// stream. The pass is read here and goes no further than the callback that
// presents it; it is never logged or recorded.
func (d *dispatcher) handleCallout(ctx context.Context, msg *cepb.CloudEvent, payload json.RawMessage) (*cepb.CloudEvent, bool, error) {
	var head struct {
		RequestID     string          `json:"requestId"`
		EntityID      string          `json:"entityId"`
		ProcessorName string          `json:"processorName"`
		ProcessorID   string          `json:"processorId"`
		CriteriaName  string          `json:"criteriaName"`
		CriteriaID    string          `json:"criteriaId"`
		FunctionName  string          `json:"functionName"`
		FunctionID    string          `json:"functionId"`
		Parameters    json.RawMessage `json:"parameters"`
	}
	if err := json.Unmarshal(payload, &head); err != nil {
		return nil, false, fmt.Errorf("failed to unmarshal calculation request: %w", err)
	}
	kind, name := "processor", head.ProcessorName
	if name == "" {
		name = head.ProcessorID
	}
	switch msg.Type {
	case ceTypeCriteriaRequest:
		kind, name = "criterion", head.CriteriaName
		if name == "" {
			name = head.CriteriaID
		}
	case ceTypeFunctionRequest:
		kind, name = "function", head.FunctionName
		if name == "" {
			name = head.FunctionID
		}
	}

	pass := txTokenFromCloudEvent(msg)
	seq := d.rec.add(calloutRecord{
		Kind: kind, Name: name, RequestID: head.RequestID, EventID: msg.Id,
		EntityID: head.EntityID, PassPresent: pass != "",
	})

	switch d.behaviour {
	case behaviourStall:
		return nil, false, nil
	case behaviourDrop:
		return nil, true, nil
	case behaviourLateCallback:
		hc := heldCallout{seq: seq, pass: pass}
		cfg, err := parseCallbackConfig(head.Parameters)
		if err != nil {
			hc.cfgErr = err.Error()
		}
		hc.cfg = cfg
		d.hold(hc)
		return nil, false, nil
	case behaviourFail, behaviourFailRetryable:
		var verdict *bool
		if d.behaviour == behaviourFailRetryable {
			v := true
			verdict = &v
		}
		msgText := "scripted failure: " + string(d.behaviour)
		var reply *cepb.CloudEvent
		var err error
		switch kind {
		case "criterion":
			reply, err = d.buildCriteriaResponse(head.RequestID, head.EntityID, false, false, msgText, verdict)
		case "function":
			reply, err = d.buildFunctionResponse(head.RequestID, head.EntityID, "", nil, false, msgText, verdict)
		default:
			reply, err = d.buildProcessorResponse(head.RequestID, head.EntityID, nil, false, msgText, verdict)
		}
		return reply, false, err
	}

	var reply *cepb.CloudEvent
	var err error
	switch msg.Type {
	case ceTypeCriteriaRequest:
		reply, err = d.handleCriteriaRequest(ctx, payload, pass)
	case ceTypeFunctionRequest:
		reply, err = d.handleFunctionRequest(ctx, payload, pass)
	default:
		// authtype carries the executor's principal kind; processors see it as
		// Entity.AuthType.
		reply, err = d.handleProcessorRequest(ctx, payload, pass, authTypeFromCloudEvent(msg))
	}
	return reply, false, err
}

// release makes the late callback for every held callout and records what
// each door answered.
func (d *dispatcher) release(ctx context.Context) {
	for _, hc := range d.takeHeld() {
		d.rec.setCallback(hc.seq, d.lateCallback(ctx, hc))
	}
}

// lateCallback presents a held pass on the HTTP door and, when a gRPC callback
// client exists, on the gRPC door, with an entity create on each.
func (d *dispatcher) lateCallback(ctx context.Context, hc heldCallout) callbackOutcome {
	var out callbackOutcome
	switch {
	case hc.cfgErr != "":
		out.Error = hc.cfgErr
		return out
	case hc.cfg.SecondaryModel == "":
		out.Error = "late-callback needs secondaryModel in the callout's context"
		return out
	case d.cat.cb == nil:
		out.Error = "callback client unavailable (CYODA_COMPUTE_HTTP_BASE unset)"
		return out
	}
	res, _, _, err := d.cat.cb.createSecondary(ctx, hc.cfg, hc.pass, hc.cfg.Marker)
	if err != nil {
		out.Error = "http callback: " + err.Error()
		return out
	}
	out.HTTPStatus = res.Status
	out.HTTPErrorCode = problemErrorCode(res.Body)

	if d.gcb == nil {
		return out
	}
	out.GRPCAttempted = true
	g, err := d.gcb.createSecondary(ctx, hc.cfg, hc.pass, hc.cfg.Marker)
	if err != nil {
		out.Error = "grpc callback: " + err.Error()
		return out
	}
	out.GRPCSuccess = g.Success
	out.GRPCErrorCode = g.ErrorCode
	out.GRPCErrorMessage = g.ErrorMsg
	return out
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
			if err := d.send(stream, ce); err != nil {
				slog.Error("failed to send keep-alive", "pkg", "compute-test-client", "error", err)
				return
			}
			slog.Debug("keep-alive sent", "pkg", "compute-test-client")
		}
	}
}

// handleProcessorRequest dispatches a processor request to the catalog and
// returns the response CloudEvent.
func (d *dispatcher) handleProcessorRequest(ctx context.Context, payload json.RawMessage, txToken, authType string) (*cepb.CloudEvent, error) {
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
		ID:       req.EntityID,
		AuthType: authType,
	}
	if req.Payload != nil && req.Payload.Data != nil {
		entity.Data = req.Payload.Data
	}

	// Callback-capable processors take precedence: they receive
	// the tx-token and the callback client to issue joined callbacks.
	if cbFn, ok := d.cat.callbackProcessor(name); ok {
		cfg, err := parseCallbackConfig(req.Parameters)
		if err != nil {
			return d.buildProcessorResponse(req.RequestID, req.EntityID, nil, false, err.Error(), nil)
		}
		result, err := cbFn(ctx, entity, cfg, txToken, d.cat.cb)
		if err != nil {
			return d.buildProcessorResponse(req.RequestID, req.EntityID, nil, false, err.Error(), verdictOf(err))
		}
		return d.buildProcessorResponse(req.RequestID, req.EntityID, result.Data, true, "", nil)
	}

	procFn, ok := d.cat.processor(name)
	if !ok {
		return d.buildProcessorResponse(req.RequestID, req.EntityID, nil, false, fmt.Sprintf("unknown processor: %s", name), nil)
	}

	result, err := procFn(ctx, entity, req.Parameters)
	if err != nil {
		return d.buildProcessorResponse(req.RequestID, req.EntityID, nil, false, err.Error(), verdictOf(err))
	}

	return d.buildProcessorResponse(req.RequestID, req.EntityID, result.Data, true, "", nil)
}

// buildProcessorResponse constructs an EntityProcessorCalculationResponse CloudEvent.
func (d *dispatcher) buildProcessorResponse(requestID, entityID string, data json.RawMessage, success bool, errMsg string, retryable *bool) (*cepb.CloudEvent, error) {
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
		resp["error"] = errorNode("PROCESSOR_ERROR", errMsg, retryable)
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

	// Callback-capable criteria take precedence.
	if cbFn, ok := d.cat.callbackCriterion(name); ok {
		cfg, err := parseCallbackConfig(req.Parameters)
		if err != nil {
			return d.buildCriteriaResponse(req.RequestID, req.EntityID, false, false, err.Error(), nil)
		}
		matches, err := cbFn(ctx, entity, cfg, txToken, d.cat.cb)
		if err != nil {
			return d.buildCriteriaResponse(req.RequestID, req.EntityID, false, false, err.Error(), verdictOf(err))
		}
		return d.buildCriteriaResponse(req.RequestID, req.EntityID, matches, true, "", nil)
	}

	critFn, ok := d.cat.criterion(name)
	if !ok {
		return d.buildCriteriaResponse(req.RequestID, req.EntityID, false, false, fmt.Sprintf("unknown criterion: %s", name), nil)
	}

	matches, err := critFn(ctx, entity, req.Parameters)
	if err != nil {
		return d.buildCriteriaResponse(req.RequestID, req.EntityID, false, false, err.Error(), verdictOf(err))
	}

	return d.buildCriteriaResponse(req.RequestID, req.EntityID, matches, true, "", nil)
}

// handleFunctionRequest dispatches a generic Function calculation request
// (spi.ScheduleFunction) to the catalog and returns the
// response CloudEvent.
func (d *dispatcher) handleFunctionRequest(ctx context.Context, payload json.RawMessage, txToken string) (*cepb.CloudEvent, error) {
	var req struct {
		RequestID    string          `json:"requestId"`
		FunctionID   string          `json:"functionId"`
		FunctionName string          `json:"functionName"`
		EntityID     string          `json:"entityId"`
		Parameters   json.RawMessage `json:"parameters"`
		Payload      *struct {
			Data json.RawMessage `json:"data"`
			Meta json.RawMessage `json:"meta"`
		} `json:"payload"`
	}
	if err := json.Unmarshal(payload, &req); err != nil {
		return nil, fmt.Errorf("failed to unmarshal function request: %w", err)
	}

	name := req.FunctionName
	if name == "" {
		name = req.FunctionID
	}
	slog.Info("function request", "pkg", "compute-test-client", "requestId", req.RequestID, "function", name, "entityId", req.EntityID)

	entity := &Entity{
		ID: req.EntityID,
	}
	if req.Payload != nil && req.Payload.Data != nil {
		entity.Data = req.Payload.Data
	}

	fn, ok := d.cat.function(name)
	if !ok {
		return d.buildFunctionResponse(req.RequestID, req.EntityID, "", nil, false, fmt.Sprintf("unknown function: %s", name), nil)
	}

	resultKind, result, err := fn(ctx, entity, req.Parameters)
	if err != nil {
		return d.buildFunctionResponse(req.RequestID, req.EntityID, "", nil, false, err.Error(), verdictOf(err))
	}

	return d.buildFunctionResponse(req.RequestID, req.EntityID, resultKind, result, true, "", nil)
}

// buildFunctionResponse constructs an EntityFunctionCalculationResponse CloudEvent.
func (d *dispatcher) buildFunctionResponse(requestID, entityID, resultKind string, result map[string]any, success bool, errMsg string, retryable *bool) (*cepb.CloudEvent, error) {
	resp := map[string]any{
		"id":        uuid.NewString(),
		"requestId": requestID,
		"entityId":  entityID,
		"success":   success,
	}
	if resultKind != "" {
		resp["resultKind"] = resultKind
	}
	if result != nil {
		resp["result"] = result
	}
	if errMsg != "" {
		resp["error"] = errorNode("FUNCTION_ERROR", errMsg, retryable)
	}
	return newCloudEvent(ceTypeFunctionResponse, resp)
}

// buildCriteriaResponse constructs an EntityCriteriaCalculationResponse CloudEvent.
func (d *dispatcher) buildCriteriaResponse(requestID, entityID string, matches, success bool, errMsg string, retryable *bool) (*cepb.CloudEvent, error) {
	resp := map[string]any{
		"id":        uuid.NewString(),
		"requestId": requestID,
		"entityId":  entityID,
		"success":   success,
		"matches":   matches,
	}
	if errMsg != "" {
		resp["error"] = errorNode("CRITERIA_ERROR", errMsg, retryable)
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
