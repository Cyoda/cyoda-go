package grpc

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/google/uuid"
	googlegrpc "google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	cepb "github.com/cyoda-platform/cyoda-go/api/grpc/cloudevents"
	events "github.com/cyoda-platform/cyoda-go/api/grpc/events"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/logging"
)

// StartStreaming implements the bidirectional streaming RPC for calculation
// member lifecycle management. It expects a ROLE_M2M-authorized user, a
// CalculationMemberJoinEvent as the first message, and then handles
// keep-alive and response routing for the connected member.
func (s *CloudEventsServiceImpl) StartStreaming(stream googlegrpc.BidiStreamingServer[cepb.CloudEvent, cepb.CloudEvent]) error {
	ctx := stream.Context()

	// 1. Check ROLE_M2M authorization.
	uc := spi.GetUserContext(ctx)
	if uc == nil {
		return status.Errorf(codes.Unauthenticated, "no user context")
	}
	if !spi.HasRole(uc.Roles, "ROLE_M2M") {
		return status.Errorf(codes.PermissionDenied, "ROLE_M2M required for streaming")
	}

	// 2. Read first message — must be CalculationMemberJoinEvent.
	firstMsg, err := stream.Recv()
	if err != nil {
		return err
	}
	eventType, payload, err := ParseCloudEvent(firstMsg)
	if err != nil {
		return status.Errorf(codes.InvalidArgument, "failed to parse first message: %v", err)
	}
	if eventType != CalculationMemberJoinEvent {
		return status.Errorf(codes.InvalidArgument, "first message must be %s, got %s", CalculationMemberJoinEvent, eventType)
	}

	// 3. Extract tags and joinedLegalEntityId.
	var joinEvent events.CalculationMemberJoinEventJson
	if err := json.Unmarshal(payload, &joinEvent); err != nil {
		return status.Errorf(codes.InvalidArgument, "invalid join event payload: %v", err)
	}

	// Also check for joinedLegalEntityId (not in the generated schema but
	// may be present as an extra field from the client).
	var extra struct {
		JoinedLegalEntityID string `json:"joinedLegalEntityId"`
	}
	_ = json.Unmarshal(payload, &extra)

	// 4. Validate tenant.
	tenantID := uc.Tenant.ID
	if extra.JoinedLegalEntityID != "" && spi.TenantID(extra.JoinedLegalEntityID) != tenantID {
		return status.Errorf(codes.PermissionDenied, "tenant mismatch")
	}

	// 5. Build the greet and register. Register publishes the member and
	// then starts its writer with the greet as the first event on the wire,
	// so the member is already visible when the client holds the greet, and
	// a dispatch routed the instant the member is visible still queues
	// behind the greet. The raw stream.Send closure below is the ONLY raw
	// write on this stream, and only the writer ever calls it.
	memberID := uuid.NewString()
	greetPayload := events.CalculationMemberGreetEventJson{
		ID:                  memberID,
		MemberID:            memberID,
		JoinedLegalEntityID: string(tenantID),
		Success:             true,
	}
	greetCE, err := NewCloudEvent(CalculationMemberGreetEvent, greetPayload)
	if err != nil {
		return status.Errorf(codes.Internal, "failed to create greet event: %v", err)
	}
	member := s.registry.Register(memberID, tenantID, joinEvent.Tags, func(ce *cepb.CloudEvent) error {
		return stream.Send(ce)
	}, greetCE)
	defer s.registry.Unregister(member)
	slog.Info("member joined", "pkg", "grpc", "memberId", memberID, "tenantId", string(tenantID), "tags", joinEvent.Tags)

	// 6. Keep-alive loop and receive goroutine. Both evict the member to end
	// the stream; neither ever blocks on it.
	kaCtx, kaCancel := context.WithCancel(ctx)
	defer kaCancel()
	go s.keepAliveLoop(kaCtx, member)
	recvCh := make(chan *cepb.CloudEvent)
	go s.receiveLoop(stream, member, recvCh)

	// 7. Main loop. Eviction — by keep-alive timeout, write stall, send
	// failure, client close, or a contained panic — is the only exit.
	// Returning is what makes grpc-go cancel the stream and unblock a raw
	// send stuck in the HTTP/2 write window.
	for {
		select {
		case <-member.Evicted():
			err := member.EvictErr()
			slog.Info("member stream ended", "pkg", "grpc", "memberId", memberID, "reason", err)
			return err
		case msg := <-recvCh:
			evtType, evtPayload, err := ParseCloudEvent(msg)
			if err != nil {
				slog.Warn("malformed CloudEvent from member", "pkg", "grpc", "memberId", memberID, "error", err)
				continue
			}
			slog.Debug("CloudEvent received from member", "pkg", "grpc", "memberId", memberID, "type", evtType, "ceId", msg.Id, "payload", logging.PayloadPreview(evtPayload, 200))

			switch evtType {
			case CalculationMemberKeepAliveEvent:
				// Liveness-only: an inbound keep-alive refreshes the member's
				// LastSeen and nothing more. The server pings on its own ticker
				// (keepAliveLoop); it must NOT echo a keep-alive back. Echoing
				// against a client that also echoes inbound keep-alives produces
				// a zero-delay, unbounded ping-pong storm pinning both processes
				// at 100% CPU.
				member.UpdateLastSeen()
			case EntityProcessorCalculationResponse:
				member.UpdateLastSeen()
				handleProcessorResponse(member, evtPayload)
			case EntityCriteriaCalculationResponse:
				member.UpdateLastSeen()
				handleCriteriaResponse(member, evtPayload)
			case EntityFunctionCalculationResponse:
				member.UpdateLastSeen()
				handleFunctionResponse(member, evtPayload)
			case EventAckResponse:
				member.UpdateLastSeen()
			default:
				slog.Warn("unknown event type from member", "pkg", "grpc", "memberId", memberID, "type", evtType)
			}
		}
	}
}

// receiveLoop is the one goroutine that reads the member's stream. Every
// Recv error, including a clean close, evicts the member with that error so
// the stream handler returns it. A panic here is contained by ticket and
// evicts; it does not latch the node, because this goroutine does no engine
// or store work and the member simply reconnects.
func (s *CloudEventsServiceImpl) receiveLoop(stream googlegrpc.BidiStreamingServer[cepb.CloudEvent, cepb.CloudEvent], member *Member, recvCh chan<- *cepb.CloudEvent) {
	defer func() {
		if rec := recover(); rec != nil {
			member.Evict(panicStatus(rec, "StartStreaming.receive"))
		}
	}()
	for {
		msg, err := stream.Recv()
		if err != nil {
			member.Evict(err)
			return
		}
		select {
		case recvCh <- msg:
		case <-member.Evicted():
			return
		}
	}
}

// keepAliveLoop pings the member every interval and evicts it when it has
// shown no inbound activity for timeout, OR when one write has been in
// flight for longer than timeout — a member whose own keep-alive goroutine
// keeps pinging while its application has stopped reading is still frozen,
// and write progress is the signal that catches it. The loop never blocks on
// the stream: a ping the writer cannot take right now is skipped.
func (s *CloudEventsServiceImpl) keepAliveLoop(ctx context.Context, member *Member) {
	defer func() {
		if rec := recover(); rec != nil {
			member.Evict(panicStatus(rec, "keepAliveLoop"))
		}
	}()
	ticker := time.NewTicker(s.keepAliveInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-member.Evicted():
			return
		case <-ticker.C:
			if time.Since(member.LastSeen()) > s.keepAliveTimeout {
				slog.Info("member timed out", "pkg", "grpc", "memberId", member.ID)
				member.Evict(status.Error(codes.DeadlineExceeded, "keep-alive timeout"))
				return
			}
			if since := member.WriteInFlightSince(); !since.IsZero() && time.Since(since) > s.keepAliveTimeout {
				slog.Warn("member not draining", "pkg", "grpc", "memberId", member.ID, "stalledFor", time.Since(since))
				member.Evict(status.Error(codes.DeadlineExceeded, "member not draining"))
				return
			}
			kaCE, err := NewCloudEvent(CalculationMemberKeepAliveEvent, events.CalculationMemberKeepAliveEventJson{
				ID: member.ID, MemberID: member.ID, Success: true,
			})
			if err != nil {
				slog.Error("failed to create keep-alive event", "pkg", "grpc", "memberId", member.ID, "error", err)
				continue
			}
			if member.TrySend(kaCE) {
				slog.Debug("keep-alive sent", "pkg", "grpc", "memberId", member.ID)
			} else {
				slog.Debug("writer busy, ping skipped", "pkg", "grpc", "memberId", member.ID)
			}
		}
	}
}

// reportedSuccess is the `success` flag of a calculation response, decoded so
// that the three states of the key stay apart: absent, present and null, and
// present and boolean.
//
// Absent is the schema's default. docs/cyoda/schema/common/BaseEvent.json
// declares the field optional with the default `true`, so a member that omits
// it has reported success; a member reporting a failure sends `success: false`.
// The literal null is neither: `null` is not a boolean, so it is not the
// default and not a flag, and an answer carrying it cannot be read at all.
//
// A `*bool` cannot express that — encoding/json leaves it nil for an absent key
// and for an explicit null alike. A type with an UnmarshalJSON method is
// instead handed the literal `null` to decode, which is what tells the two
// apart here. The three decoders resolve the flag through this one type, so
// ProcessingResponse.Success stays a plain bool that every reader downstream
// can trust, beside the NullSuccess that marks the answer unreadable.
//
// The default stands in for a flag, never for a verdict: `matches` on a
// criteria response is kept absent (see handleCriteriaResponse) and an answer
// that cannot be read is still refused.
type reportedSuccess struct {
	present bool // the key was in the response at all
	null    bool // ... and carried the literal null rather than a boolean
	value   bool // ... and, when not null, the boolean it carried
}

// UnmarshalJSON records which of the three states the key was in.
func (s *reportedSuccess) UnmarshalJSON(data []byte) error {
	s.present = true
	if string(data) == "null" {
		s.null = true
		return nil
	}
	return json.Unmarshal(data, &s.value)
}

// succeeded reports whether the member reported success, the schema's default
// standing in for an absent key. An unreadable flag reports no success, so a
// reader that consults this alone fails closed.
func (s reportedSuccess) succeeded() bool {
	return !s.present || s.value
}

// unreadable reports whether the key carried the literal null.
func (s reportedSuccess) unreadable() bool {
	return s.null
}

// handleProcessorResponse routes a processor calculation response to the
// pending request on the given member.
func handleProcessorResponse(member *Member, payload json.RawMessage) {
	var resp struct {
		RequestID string          `json:"requestId"`
		Success   reportedSuccess `json:"success"`
		Error     *struct {
			Message   string `json:"message"`
			Retryable *bool  `json:"retryable"`
		} `json:"error"`
		Warnings []string        `json:"warnings"`
		Payload  json.RawMessage `json:"payload"`
	}
	if err := json.Unmarshal(payload, &resp); err != nil {
		slog.Warn("failed to unmarshal processor response", "pkg", "grpc", "memberId", member.ID, "error", common.JSONErrorShape(err))
		return
	}

	errMsg := ""
	var retryable *bool
	if resp.Error != nil {
		errMsg = resp.Error.Message
		retryable = resp.Error.Retryable
	}
	member.CompleteRequest(resp.RequestID, &ProcessingResponse{
		Payload:     resp.Payload,
		Success:     resp.Success.succeeded(),
		NullSuccess: resp.Success.unreadable(),
		Error:       errMsg,
		Warnings:    resp.Warnings,
		Retryable:   retryable,
	})
}

// handleCriteriaResponse routes a criteria calculation response to the
// pending request on the given member.
func handleCriteriaResponse(member *Member, payload json.RawMessage) {
	var resp struct {
		RequestID string          `json:"requestId"`
		Success   reportedSuccess `json:"success"`
		// A pointer: a response that says nothing about matches must arrive
		// at the callout saying nothing. Decoded into a bool it would arrive
		// as "does not match" — a verdict on a criterion that decides a
		// transition, invented here, which the callout could no longer refuse.
		Matches *bool  `json:"matches"`
		Reason  string `json:"reason"`
		Error   *struct {
			Message   string `json:"message"`
			Retryable *bool  `json:"retryable"`
		} `json:"error"`
		Warnings []string `json:"warnings"`
	}
	if err := json.Unmarshal(payload, &resp); err != nil {
		slog.Warn("failed to unmarshal criteria response", "pkg", "grpc", "memberId", member.ID, "error", common.JSONErrorShape(err))
		return
	}

	errMsg := ""
	var retryable *bool
	if resp.Error != nil {
		errMsg = resp.Error.Message
		retryable = resp.Error.Retryable
	}
	member.CompleteRequest(resp.RequestID, &ProcessingResponse{
		Success:     resp.Success.succeeded(),
		NullSuccess: resp.Success.unreadable(),
		Error:       errMsg,
		Matches:     resp.Matches,
		Reason:      resp.Reason,
		Warnings:    resp.Warnings,
		Retryable:   retryable,
	})
}

// handleFunctionResponse routes a function calculation response to the
// pending request on the given member.
func handleFunctionResponse(member *Member, payload json.RawMessage) {
	var resp struct {
		RequestID  string           `json:"requestId"`
		Success    reportedSuccess  `json:"success"`
		Result     *json.RawMessage `json:"result"`
		ResultKind *string          `json:"resultKind"`
		Error      *struct {
			Message   string `json:"message"`
			Retryable *bool  `json:"retryable"`
		} `json:"error"`
		Warnings []string `json:"warnings"`
	}
	if err := json.Unmarshal(payload, &resp); err != nil {
		slog.Warn("failed to unmarshal function response", "pkg", "grpc", "memberId", member.ID, "error", common.JSONErrorShape(err))
		return
	}

	errMsg := ""
	var retryable *bool
	if resp.Error != nil {
		errMsg = resp.Error.Message
		retryable = resp.Error.Retryable
	}
	var result json.RawMessage
	if resp.Result != nil {
		result = *resp.Result
	}
	resultKind := ""
	if resp.ResultKind != nil {
		resultKind = *resp.ResultKind
	}
	member.CompleteRequest(resp.RequestID, &ProcessingResponse{
		Success:     resp.Success.succeeded(),
		NullSuccess: resp.Success.unreadable(),
		Error:       errMsg,
		Result:      result,
		ResultKind:  resultKind,
		Warnings:    resp.Warnings,
		Retryable:   retryable,
	})
}
