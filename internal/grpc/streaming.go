package grpc

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"time"

	"github.com/google/uuid"
	googlegrpc "google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	cepb "github.com/cyoda-platform/cyoda-go/api/grpc/cloudevents"
	events "github.com/cyoda-platform/cyoda-go/api/grpc/events"
	"github.com/cyoda-platform/cyoda-go/internal/auth"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/contract"
	"github.com/cyoda-platform/cyoda-go/internal/logging"
)

// clientRecheckInterval is how often a stream re-reads its client from the
// store, and the deadline of each read. A stream outlives the token that
// opened it, so this bounds how long a deleted client, or one whose secret
// was reset, keeps a stream open.
const clientRecheckInterval = 60 * time.Second

// StartStreaming implements the bidirectional streaming RPC for calculation
// member lifecycle management. It expects a compute node's own client token
// (see streamPrincipal), a CalculationMemberJoinEvent as the first message,
// and then handles keep-alive and response routing for the connected member.
func (s *CloudEventsServiceImpl) StartStreaming(stream googlegrpc.BidiStreamingServer[cepb.CloudEvent, cepb.CloudEvent]) error {
	ctx := stream.Context()

	// 1. Only a compute node's own client token opens a stream.
	uc, ct, err := streamPrincipal(ctx)
	if err != nil {
		return err
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

	// 5. Check the client once before the member exists: a client deleted or
	// reset since its token was issued never joins. Mock mode has no client
	// store and no check.
	if s.m2mStore != nil {
		if err := s.recheckClient(ctx, ct, tenantID); err != nil {
			slog.Info("member stream refused by client check", "pkg", "grpc", "tenantId", string(tenantID), "clientId", ct.ClientID, "reason", err)
			return err
		}
	}

	// 6. Build the greet and register. Register publishes the member and
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

	// 7. Keep-alive loop and receive goroutine. Both evict the member to end
	// the stream; neither ever blocks on it.
	kaCtx, kaCancel := context.WithCancel(ctx)
	defer kaCancel()
	go s.keepAliveLoop(kaCtx, member)
	if s.m2mStore != nil {
		go s.clientRecheckLoop(kaCtx, member, ct, tenantID)
	}
	recvCh := make(chan *cepb.CloudEvent)
	go s.receiveLoop(stream, member, recvCh)

	// 8. Main loop. Eviction — by keep-alive timeout, write stall, send
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

// streamPrincipal returns the caller and its client-token marker when the
// caller may open a stream: a client-credentials token of kind service with
// no executor. A UserContext's presence and ROLE_M2M are not checked here:
// the server's stream interceptors (StreamAuthInterceptor, StreamRequireM2M)
// refuse a caller without either before any stream handler runs. An
// on-behalf-of token states a user, not a compute node, and never opens one.
// Every refusal here is PermissionDenied.
func streamPrincipal(ctx context.Context) (*spi.UserContext, contract.ClientToken, error) {
	uc := spi.GetUserContext(ctx)
	ct, marked := contract.ClientTokenFrom(ctx)
	if uc.Kind != spi.PrincipalService || uc.Executor != nil || !marked {
		return nil, contract.ClientToken{}, status.Error(codes.PermissionDenied, "a compute node must connect with its own client's token")
	}
	return uc, ct, nil
}

// clientRecheckLoop re-reads the stream's client every clientRecheckInterval
// after the check at open, and evicts the member the first time
// recheckClient refuses it. It checks the client only: the opening token's
// expiry does not end a stream.
func (s *CloudEventsServiceImpl) clientRecheckLoop(ctx context.Context, member *Member, ct contract.ClientToken, tenant spi.TenantID) {
	defer func() {
		if rec := recover(); rec != nil {
			member.Evict(panicStatus(rec, "clientRecheckLoop"))
		}
	}()
	ticker := time.NewTicker(clientRecheckInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-member.Evicted():
			return
		case <-ticker.C:
			if err := s.recheckClient(ctx, ct, tenant); err != nil {
				if ctx.Err() != nil {
					return // the stream ended during the read: nothing to close
				}
				slog.Info("member stream closed by client re-check", "pkg", "grpc", "memberId", member.ID, "tenantId", string(tenant), "clientId", ct.ClientID, "reason", err)
				member.Evict(err)
				return
			}
		}
	}
}

// recheckClient reports whether the stream's client still stands: nil if so,
// status Unauthenticated if the client is gone from the stream's tenant or
// its secret was reset, status Unavailable if the store cannot be read. The
// client is looked up in the stream's tenant, so a client of another tenant
// is gone. The read is bounded by clientRecheckInterval: a read that does not
// answer in time is a store that cannot be read.
func (s *CloudEventsServiceImpl) recheckClient(ctx context.Context, ct contract.ClientToken, tenant spi.TenantID) error {
	readCtx, cancel := context.WithTimeout(ctx, clientRecheckInterval)
	defer cancel()
	c, err := s.m2mStore.Lookup(readCtx, tenant, ct.ClientID)
	if errors.Is(err, auth.ErrM2MClientNotFound) {
		return status.Error(codes.Unauthenticated, "the client was deleted")
	}
	if err != nil {
		if ctx.Err() == nil {
			slog.Warn("client re-check could not read the client store", "pkg", "grpc", "tenantId", string(tenant), "clientId", ct.ClientID, "error", err)
		}
		return status.Error(codes.Unavailable, "the client could not be re-checked")
	}
	if c.SecretGen != ct.Gen {
		return status.Error(codes.Unauthenticated, "the client's secret was reset")
	}
	return nil
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
// can trust, beside the Unreadable reason that marks the answer refused.
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

// The client-safe reasons an answer is unreadable. Both are this node's own
// fixed text: nothing a member sent is quoted back, and each says as much
// about what could not be read as it honestly can — the key, where one key is
// at fault — which is what a member's author needs to hear in the 400 the
// callout ends with. The decode error's shape goes to the log, never to the
// caller.
const (
	unreadableNullSuccess = "success was null"
	unreadableUndecodable = "the answer did not decode"
)

// completeUndecodable ends the pending request an answer names when the answer
// itself does not decode into the shape its event type promises.
//
// Returning without completing anything would leave the callout waiting for an
// answer that has already arrived, until its answer limit runs out: the try is
// spent, the operation is told a retryable "no answer came", and a member that
// did answer is blamed for silence. So the request id is recovered on its own,
// from a decode narrow enough to survive whatever made the full one fail, and
// the callout is ended at once.
//
// A payload that is not JSON at all has no request id to recover, and then
// there is genuinely nothing to complete — the answer limit is the only
// remaining bound, which is correct, because nothing identifies what it was an
// answer to.
func completeUndecodable(member *Member, kind string, payload json.RawMessage, cause error) {
	var ident struct {
		RequestID string `json:"requestId"`
	}
	if err := json.Unmarshal(payload, &ident); err != nil || ident.RequestID == "" {
		slog.Warn("failed to unmarshal "+kind+" response, and it names no request",
			"pkg", "grpc", "memberId", member.ID, "error", common.JSONErrorShape(cause))
		return
	}
	slog.Warn("failed to unmarshal "+kind+" response",
		"pkg", "grpc", "memberId", member.ID, "requestId", ident.RequestID, "error", common.JSONErrorShape(cause))
	member.CompleteRequest(ident.RequestID, &ProcessingResponse{Unreadable: unreadableUndecodable})
}

// unreadableReason is the reason a decoded answer still cannot be read, or "".
func unreadableReason(s reportedSuccess) string {
	if s.unreadable() {
		return unreadableNullSuccess
	}
	return ""
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
		completeUndecodable(member, "processor", payload, err)
		return
	}

	errMsg := ""
	var retryable *bool
	if resp.Error != nil {
		errMsg = resp.Error.Message
		retryable = resp.Error.Retryable
	}
	member.CompleteRequest(resp.RequestID, &ProcessingResponse{
		Payload:    resp.Payload,
		Success:    resp.Success.succeeded(),
		Unreadable: unreadableReason(resp.Success),
		Error:      errMsg,
		Warnings:   resp.Warnings,
		Retryable:  retryable,
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
		completeUndecodable(member, "criteria", payload, err)
		return
	}

	errMsg := ""
	var retryable *bool
	if resp.Error != nil {
		errMsg = resp.Error.Message
		retryable = resp.Error.Retryable
	}
	member.CompleteRequest(resp.RequestID, &ProcessingResponse{
		Success:    resp.Success.succeeded(),
		Unreadable: unreadableReason(resp.Success),
		Error:      errMsg,
		Matches:    resp.Matches,
		Reason:     resp.Reason,
		Warnings:   resp.Warnings,
		Retryable:  retryable,
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
		completeUndecodable(member, "function", payload, err)
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
		Success:    resp.Success.succeeded(),
		Unreadable: unreadableReason(resp.Success),
		Error:      errMsg,
		Result:     result,
		ResultKind: resultKind,
		Warnings:   resp.Warnings,
		Retryable:  retryable,
	})
}
