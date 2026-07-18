package grpc

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	googlegrpc "google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	cepb "github.com/cyoda-platform/cyoda-go/api/grpc/cloudevents"
	events "github.com/cyoda-platform/cyoda-go/api/grpc/events"
	"github.com/cyoda-platform/cyoda-go/internal/logging"
)

// Default keep-alive configuration. Override with SetKeepAliveConfig.
var (
	DefaultKeepAliveInterval = 10 * time.Second
	DefaultKeepAliveTimeout  = 30 * time.Second
)

// SetKeepAliveConfig overrides the default keep-alive interval and timeout for
// this service instance.
func (s *CloudEventsServiceImpl) SetKeepAliveConfig(interval, timeout time.Duration) {
	s.keepAliveInterval = interval
	s.keepAliveTimeout = timeout
}

func (s *CloudEventsServiceImpl) keepAliveInterval_() time.Duration {
	if s.keepAliveInterval > 0 {
		return s.keepAliveInterval
	}
	return DefaultKeepAliveInterval
}

func (s *CloudEventsServiceImpl) keepAliveTimeout_() time.Duration {
	if s.keepAliveTimeout > 0 {
		return s.keepAliveTimeout
	}
	return DefaultKeepAliveTimeout
}

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

	// 5. Register member.
	sendFn := func(ce *cepb.CloudEvent) error {
		return stream.Send(ce)
	}
	memberID := s.registry.Register(tenantID, joinEvent.Tags, sendFn)
	defer s.registry.Unregister(memberID)
	slog.Info("member joined", "pkg", "grpc", "memberId", memberID, "tenantId", string(tenantID), "tags", joinEvent.Tags)

	// 6. Send GreetEvent.
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
	if err := stream.Send(greetCE); err != nil {
		slog.Error("failed to send greet", "pkg", "grpc", "memberId", memberID, "error", err)
		return err
	}
	slog.Info("member greeted", "pkg", "grpc", "memberId", memberID)

	// 7. Start keep-alive goroutine. The timeoutCh is closed when the member
	// exceeds the keep-alive timeout, which terminates the receive loop.
	timeoutCh := make(chan struct{})
	keepAliveCtx, keepAliveCancel := context.WithCancel(ctx)
	defer keepAliveCancel()
	go s.keepAliveLoop(keepAliveCtx, memberID, timeoutCh)

	// 8. Receive loop. We run Recv in a goroutine so we can also select on
	// the timeout channel.
	type recvResult struct {
		msg *cepb.CloudEvent
		err error
	}

	for {
		recvCh := make(chan recvResult, 1)
		go func() {
			msg, err := stream.Recv()
			recvCh <- recvResult{msg, err}
		}()

		select {
		case <-timeoutCh:
			slog.Info("member timed out", "pkg", "grpc", "memberId", memberID)
			return status.Errorf(codes.DeadlineExceeded, "keep-alive timeout")
		case res := <-recvCh:
			if res.err != nil {
				slog.Info("member disconnected", "pkg", "grpc", "memberId", memberID, "reason", "stream closed")
				return res.err
			}

			evtType, evtPayload, err := ParseCloudEvent(res.msg)
			if err != nil {
				slog.Warn("malformed CloudEvent from member", "pkg", "grpc", "memberId", memberID, "error", err)
				continue // skip malformed messages
			}
			slog.Debug("CloudEvent received from member", "pkg", "grpc", "memberId", memberID, "type", evtType, "ceId", res.msg.Id, "payload", logging.PayloadPreview(evtPayload, 200))

			switch evtType {
			case CalculationMemberKeepAliveEvent:
				slog.Debug("keep-alive received", "pkg", "grpc", "memberId", memberID)
				member := s.registry.Get(memberID)
				if member != nil {
					member.UpdateLastSeen()
				}
				// Send keep-alive response.
				kaResp := events.CalculationMemberKeepAliveEventJson{
					ID:       res.msg.Id,
					MemberID: memberID,
					Success:  true,
				}
				kaCE, err := NewCloudEvent(CalculationMemberKeepAliveEvent, kaResp)
				if err == nil {
					if err := stream.Send(kaCE); err != nil {
						slog.Warn("failed to send keep-alive response", "pkg", "grpc", "memberId", memberID, "error", err)
					}
				}

			case EntityProcessorCalculationResponse:
				s.handleProcessorResponse(memberID, evtPayload)
				slog.Debug("processor response routed", "pkg", "grpc", "memberId", memberID)

			case EntityCriteriaCalculationResponse:
				s.handleCriteriaResponse(memberID, evtPayload)
				slog.Debug("criteria response routed", "pkg", "grpc", "memberId", memberID)

			case EntityFunctionCalculationResponse:
				s.handleFunctionResponse(memberID, evtPayload)
				slog.Debug("function response routed", "pkg", "grpc", "memberId", memberID)

			case EventAckResponse:
				// Client acknowledged a server event — proves liveness.
				member := s.registry.Get(memberID)
				if member != nil {
					member.UpdateLastSeen()
				}

			default:
				slog.Warn("unknown event type from member", "pkg", "grpc", "memberId", memberID, "type", evtType)
			}
		}
	}
}

// keepAliveLoop periodically checks that the member is still alive and sends
// keep-alive events. If the member times out, it closes timeoutCh to signal
// the receive loop to terminate.
func (s *CloudEventsServiceImpl) keepAliveLoop(ctx context.Context, memberID string, timeoutCh chan struct{}) {
	interval := s.keepAliveInterval_()
	timeout := s.keepAliveTimeout_()

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			member := s.registry.Get(memberID)
			if member == nil {
				return
			}
			if time.Since(member.LastSeen()) > timeout {
				close(timeoutCh)
				return
			}
			kaPayload := events.CalculationMemberKeepAliveEventJson{
				ID:       memberID,
				MemberID: memberID,
				Success:  true,
			}
			kaCE, err := NewCloudEvent(CalculationMemberKeepAliveEvent, kaPayload)
			if err != nil {
				slog.Error("failed to create keep-alive event", "pkg", "grpc", "memberId", memberID, "error", err)
				continue
			}
			if err := member.Send(kaCE); err != nil {
				slog.Error("failed to send keep-alive", "pkg", "grpc", "memberId", memberID, "error", err)
			} else {
				slog.Debug("keep-alive sent", "pkg", "grpc", "memberId", memberID)
			}
		}
	}
}

// handleProcessorResponse routes a processor calculation response to the
// pending request on the given member.
func (s *CloudEventsServiceImpl) handleProcessorResponse(memberID string, payload json.RawMessage) {
	var resp struct {
		RequestID string `json:"requestId"`
		Success   bool   `json:"success"`
		Error     *struct {
			Message   string `json:"message"`
			Retryable *bool  `json:"retryable"`
		} `json:"error"`
		Warnings []string        `json:"warnings"`
		Payload  json.RawMessage `json:"payload"`
	}
	if err := json.Unmarshal(payload, &resp); err != nil {
		slog.Warn("failed to unmarshal processor response", "pkg", "grpc", "memberId", memberID, "error", err)
		return
	}

	member := s.registry.Get(memberID)
	if member == nil {
		return
	}

	errMsg := ""
	var retryable *bool
	if resp.Error != nil {
		errMsg = resp.Error.Message
		retryable = resp.Error.Retryable
	}
	member.CompleteRequest(resp.RequestID, &ProcessingResponse{
		Payload:   resp.Payload,
		Success:   resp.Success,
		Error:     errMsg,
		Warnings:  resp.Warnings,
		Retryable: retryable,
	})
}

// handleCriteriaResponse routes a criteria calculation response to the
// pending request on the given member.
func (s *CloudEventsServiceImpl) handleCriteriaResponse(memberID string, payload json.RawMessage) {
	var resp struct {
		RequestID string `json:"requestId"`
		Success   bool   `json:"success"`
		Matches   bool   `json:"matches"`
		Reason    string `json:"reason"`
		Error     *struct {
			Message   string `json:"message"`
			Retryable *bool  `json:"retryable"`
		} `json:"error"`
		Warnings []string `json:"warnings"`
	}
	if err := json.Unmarshal(payload, &resp); err != nil {
		slog.Warn("failed to unmarshal criteria response", "pkg", "grpc", "memberId", memberID, "error", err)
		return
	}

	member := s.registry.Get(memberID)
	if member == nil {
		return
	}

	errMsg := ""
	var retryable *bool
	if resp.Error != nil {
		errMsg = resp.Error.Message
		retryable = resp.Error.Retryable
	}
	matches := resp.Matches
	member.CompleteRequest(resp.RequestID, &ProcessingResponse{
		Success:   resp.Success,
		Error:     errMsg,
		Matches:   &matches,
		Reason:    resp.Reason,
		Warnings:  resp.Warnings,
		Retryable: retryable,
	})
}

// handleFunctionResponse routes a function calculation response to the
// pending request on the given member.
func (s *CloudEventsServiceImpl) handleFunctionResponse(memberID string, payload json.RawMessage) {
	var resp struct {
		RequestID  string           `json:"requestId"`
		Success    bool             `json:"success"`
		Result     *json.RawMessage `json:"result"`
		ResultKind *string          `json:"resultKind"`
		Error      *struct {
			Message   string `json:"message"`
			Retryable *bool  `json:"retryable"`
		} `json:"error"`
		Warnings []string `json:"warnings"`
	}
	if err := json.Unmarshal(payload, &resp); err != nil {
		slog.Warn("failed to unmarshal function response", "pkg", "grpc", "memberId", memberID, "error", err)
		return
	}

	member := s.registry.Get(memberID)
	if member == nil {
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
		Success:    resp.Success,
		Error:      errMsg,
		Result:     result,
		ResultKind: resultKind,
		Warnings:   resp.Warnings,
		Retryable:  retryable,
	})
}
