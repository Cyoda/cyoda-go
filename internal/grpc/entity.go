package grpc

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	cepb "github.com/cyoda-platform/cyoda-go/api/grpc/cloudevents"
	cyodapb "github.com/cyoda-platform/cyoda-go/api/grpc/cyoda"
	events "github.com/cyoda-platform/cyoda-go/api/grpc/events"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/domain/entity"
	"github.com/cyoda-platform/cyoda-go/internal/logging"
)

// EntityManage handles unary entity management CloudEvents (create, update,
// delete, transition) by calling entity service methods directly.
func (s *CloudEventsServiceImpl) EntityManage(ctx context.Context, ce *cepb.CloudEvent) (*cepb.CloudEvent, error) {
	ctx = common.WithDiagnostics(ctx)

	eventType, payload, err := ParseCloudEvent(ce)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid CloudEvent: %v", err)
	}

	slog.Debug("CloudEvent received", "pkg", "grpc", "rpc", "entityManage", "type", eventType, "ceId", ce.Id, "payload", logging.PayloadPreview(payload, 200))

	switch eventType {
	case EntityCreateRequest:
		dec := json.NewDecoder(bytes.NewReader(payload))
		dec.UseNumber()
		var req events.EntityCreateRequestJson
		if err := dec.Decode(&req); err != nil {
			return nil, status.Errorf(codes.InvalidArgument, "invalid payload: %v", err)
		}

		opCtx, cancelTimeout, terr := resolveEventTimeout(ctx, req.TransactionTimeoutMs, "transactionTimeoutMs")
		if terr != nil {
			return entityTransactionError(ctx, ce.Id, terr)
		}
		defer cancelTimeout()

		format := string(req.DataFormat)
		if format == "" {
			format = "JSON"
		}
		// The payload bytes go to the entity service verbatim — no
		// decode→re-marshal round-trip, which would rewrite unpaired
		// surrogates and invalid UTF-8 to U+FFFD and collapse duplicate
		// keys before the unstorable-payload guard could see them.
		result, err := s.entityHandler.CreateEntity(opCtx, entity.CreateEntityInput{
			EntityName:   req.Payload.Model.Name,
			ModelVersion: fmt.Sprintf("%d", req.Payload.Model.Version),
			Format:       format,
			Data:         req.Payload.Data,
		})
		if err != nil {
			if appErr := common.ClassifyRequestTimeout(opCtx, err, common.ErrCodeTransactionTimeout); appErr != nil {
				err = appErr
			}
			slog.Error("operation failed", "pkg", "grpc", "rpc", "entityManage", "type", eventType, "ceId", ce.Id, "error", err.Error())
			return entityTransactionError(ctx, ce.Id, err)
		}

		diag := common.GetDiagnostics(ctx)
		resp := events.EntityTransactionResponseJson{
			ID:        ce.Id,
			Success:   true,
			Warnings:  diag.GetWarnings(),
			RequestID: ce.Id,
			TransactionInfo: events.EntityTransactionInfoJson{
				EntityIds: result.EntityIDs,
			},
		}
		if result.TransactionID != "" {
			resp.TransactionInfo.TransactionID = &result.TransactionID
		}
		slog.Debug("CloudEvent response", "pkg", "grpc", "rpc", "entityManage", "type", EntityTransactionResponse, "ceId", ce.Id, "success", true)
		return NewCloudEvent(EntityTransactionResponse, resp)

	case EntityUpdateRequest:
		dec := json.NewDecoder(bytes.NewReader(payload))
		dec.UseNumber()
		var req events.EntityUpdateRequestJson
		if err := dec.Decode(&req); err != nil {
			return nil, status.Errorf(codes.InvalidArgument, "invalid payload: %v", err)
		}

		opCtx, cancelTimeout, terr := resolveEventTimeout(ctx, req.TransactionTimeoutMs, "transactionTimeoutMs")
		if terr != nil {
			return entityTransactionError(ctx, ce.Id, terr)
		}
		defer cancelTimeout()

		format := string(req.DataFormat)
		if format == "" {
			format = "JSON"
		}
		transition := ""
		if req.Payload.Transition != nil {
			transition = *req.Payload.Transition
		}

		result, err := s.entityHandler.UpdateEntity(opCtx, entity.UpdateEntityInput{
			EntityID:   req.Payload.EntityID,
			Format:     format,
			Data:       req.Payload.Data,
			Transition: transition,
		})
		if err != nil {
			if appErr := common.ClassifyRequestTimeout(opCtx, err, common.ErrCodeTransactionTimeout); appErr != nil {
				err = appErr
			}
			slog.Error("operation failed", "pkg", "grpc", "rpc", "entityManage", "type", eventType, "ceId", ce.Id, "error", err.Error())
			return entityTransactionError(ctx, ce.Id, err)
		}

		diag := common.GetDiagnostics(ctx)
		resp := events.EntityTransactionResponseJson{
			ID:        ce.Id,
			Success:   true,
			Warnings:  diag.GetWarnings(),
			RequestID: ce.Id,
			TransactionInfo: events.EntityTransactionInfoJson{
				EntityIds: result.EntityIDs,
			},
		}
		if result.TransactionID != "" {
			resp.TransactionInfo.TransactionID = &result.TransactionID
		}
		slog.Debug("CloudEvent response", "pkg", "grpc", "rpc", "entityManage", "type", EntityTransactionResponse, "ceId", ce.Id, "success", true)
		return NewCloudEvent(EntityTransactionResponse, resp)

	case EntityPatchRequest:
		dec := json.NewDecoder(bytes.NewReader(payload))
		dec.UseNumber()
		var req events.EntityPatchRequestJson
		if err := dec.Decode(&req); err != nil {
			return nil, status.Errorf(codes.InvalidArgument, "invalid payload: %v", err)
		}

		opCtx, cancelTimeout, terr := resolveEventTimeout(ctx, req.TransactionTimeoutMs, "transactionTimeoutMs")
		if terr != nil {
			return entityTransactionError(ctx, ce.Id, terr)
		}
		defer cancelTimeout()

		if req.Payload.IfMatch == nil || *req.Payload.IfMatch == "" {
			return entityTransactionError(ctx, ce.Id, common.Operational(
				http.StatusPreconditionRequired, common.ErrCodePreconditionRequired,
				"missing ifMatch: provide the transactionId from your last read, or \"*\" to accept last-writer-wins"))
		}
		transition := ""
		if req.Payload.Transition != nil {
			transition = *req.Payload.Transition
		}
		result, err := s.entityHandler.PatchEntity(opCtx, entity.PatchEntityInput{
			EntityID:    req.Payload.EntityID,
			Patch:       req.Payload.Patch,
			PatchFormat: string(req.PatchFormat),
			Transition:  transition,
			IfMatch:     *req.Payload.IfMatch,
		})
		if err != nil {
			if appErr := common.ClassifyRequestTimeout(opCtx, err, common.ErrCodeTransactionTimeout); appErr != nil {
				err = appErr
			}
			slog.Error("operation failed", "pkg", "grpc", "rpc", "entityManage", "type", eventType, "ceId", ce.Id, "error", err.Error())
			return entityTransactionError(ctx, ce.Id, err)
		}
		diag := common.GetDiagnostics(ctx)
		resp := events.EntityTransactionResponseJson{
			ID:              ce.Id,
			Success:         true,
			Warnings:        diag.GetWarnings(),
			RequestID:       ce.Id,
			TransactionInfo: events.EntityTransactionInfoJson{EntityIds: result.EntityIDs},
		}
		if result.TransactionID != "" {
			resp.TransactionInfo.TransactionID = &result.TransactionID
		}
		slog.Debug("CloudEvent response", "pkg", "grpc", "rpc", "entityManage", "type", EntityTransactionResponse, "ceId", ce.Id, "success", true)
		return NewCloudEvent(EntityTransactionResponse, resp)

	case EntityDeleteRequest:
		dec := json.NewDecoder(bytes.NewReader(payload))
		dec.UseNumber()
		var req events.EntityDeleteRequestJson
		if err := dec.Decode(&req); err != nil {
			return nil, status.Errorf(codes.InvalidArgument, "invalid payload: %v", err)
		}

		result, err := s.entityHandler.DeleteEntity(ctx, req.EntityID)
		if err != nil {
			slog.Error("operation failed", "pkg", "grpc", "rpc", "entityManage", "type", eventType, "ceId", ce.Id, "error", err.Error())
			return entityDeleteError(ctx, ce.Id, req.EntityID, err)
		}

		diag := common.GetDiagnostics(ctx)
		resp := events.EntityDeleteResponseJson{
			ID:        ce.Id,
			Success:   true,
			Warnings:  diag.GetWarnings(),
			RequestID: ce.Id,
			EntityID:  result.EntityID,
			Model: events.ModelSpecJson{
				Name:    result.ModelName,
				Version: result.ModelVersion,
			},
			TransactionID: result.TransactionID,
		}
		slog.Debug("CloudEvent response", "pkg", "grpc", "rpc", "entityManage", "type", EntityDeleteResponse, "ceId", ce.Id, "success", true)
		return NewCloudEvent(EntityDeleteResponse, resp)

	case EntityTransitionRequest:
		dec := json.NewDecoder(bytes.NewReader(payload))
		dec.UseNumber()
		var req events.EntityTransitionRequestJson
		if err := dec.Decode(&req); err != nil {
			return nil, status.Errorf(codes.InvalidArgument, "invalid payload: %v", err)
		}

		// Fetch the entity's current data to pass through with the transition.
		envelope, err := s.entityHandler.GetEntity(ctx, entity.GetOneEntityInput{
			EntityID: req.EntityID,
		})
		if err != nil {
			slog.Error("operation failed", "pkg", "grpc", "rpc", "entityManage", "type", eventType, "ceId", ce.Id, "error", err.Error())
			return entityTransitionError(ctx, ce.Id, err)
		}

		dataBytes, err := json.Marshal(envelope.Data)
		if err != nil {
			slog.Error("operation failed", "pkg", "grpc", "rpc", "entityManage", "type", eventType, "ceId", ce.Id, "error", err.Error())
			return entityTransitionError(ctx, ce.Id, fmt.Errorf("failed to marshal entity data: %v", err))
		}

		_, err = s.entityHandler.UpdateEntity(ctx, entity.UpdateEntityInput{
			EntityID:   req.EntityID,
			Format:     "JSON",
			Data:       dataBytes,
			Transition: req.Transition,
		})
		if err != nil {
			slog.Error("operation failed", "pkg", "grpc", "rpc", "entityManage", "type", eventType, "ceId", ce.Id, "error", err.Error())
			return entityTransitionError(ctx, ce.Id, err)
		}

		diag := common.GetDiagnostics(ctx)
		resp := events.EntityTransitionResponseJson{
			ID:       ce.Id,
			Success:  true,
			Warnings: diag.GetWarnings(),
		}
		slog.Debug("CloudEvent response", "pkg", "grpc", "rpc", "entityManage", "type", EntityTransitionResponse, "ceId", ce.Id, "success", true)
		return NewCloudEvent(EntityTransitionResponse, resp)

	default:
		slog.Warn("unsupported event type", "pkg", "grpc", "rpc", "entityManage", "type", eventType)
		return nil, status.Errorf(codes.InvalidArgument, "unsupported entity event type: %s", eventType)
	}
}

// EntityManageCollection handles server-streaming entity collection operations
// (batch create, update collection, delete all) by calling entity service methods directly.
func (s *CloudEventsServiceImpl) EntityManageCollection(ce *cepb.CloudEvent, stream cyodapb.CloudEventsService_EntityManageCollectionServer) error {
	ctx := stream.Context()
	ctx = common.WithDiagnostics(ctx)

	eventType, payload, err := ParseCloudEvent(ce)
	if err != nil {
		return status.Errorf(codes.InvalidArgument, "invalid CloudEvent: %v", err)
	}

	slog.Debug("CloudEvent received", "pkg", "grpc", "rpc", "entityManageCollection", "type", eventType, "ceId", ce.Id, "payload", logging.PayloadPreview(payload, 200))

	switch eventType {
	case EntityCreateCollectionRequest:
		dec := json.NewDecoder(bytes.NewReader(payload))
		dec.UseNumber()
		var req events.EntityCreateCollectionRequestJson
		if err := dec.Decode(&req); err != nil {
			return status.Errorf(codes.InvalidArgument, "invalid payload: %v", err)
		}

		opCtx, cancelTimeout, terr := resolveEventTimeout(ctx, req.TransactionTimeoutMs, "transactionTimeoutMs")
		if terr != nil {
			respCE, ceErr := entityTransactionError(ctx, ce.Id, terr)
			if ceErr != nil {
				return status.Errorf(codes.Internal, "failed to build error response: %v", ceErr)
			}
			return stream.Send(respCE)
		}
		defer cancelTimeout()

		if len(req.Payloads) == 0 {
			return status.Errorf(codes.InvalidArgument, "payloads array is empty")
		}

		items := make([]entity.CollectionItem, len(req.Payloads))
		for i, p := range req.Payloads {
			items[i] = entity.CollectionItem{
				ModelName:    p.Model.Name,
				ModelVersion: int32(p.Model.Version),
				Payload:      p.Data,
			}
		}

		result, err := s.entityHandler.CreateEntityCollection(opCtx, items)
		if err != nil {
			if appErr := common.ClassifyRequestTimeout(opCtx, err, common.ErrCodeTransactionTimeout); appErr != nil {
				err = appErr
			}
			slog.Error("operation failed", "pkg", "grpc", "rpc", "entityManageCollection", "type", eventType, "ceId", ce.Id, "error", err.Error())
			respCE, ceErr := entityTransactionError(ctx, ce.Id, err)
			if ceErr != nil {
				return status.Errorf(codes.Internal, "failed to build error response: %v", ceErr)
			}
			return stream.Send(respCE)
		}

		diag := common.GetDiagnostics(ctx)
		resp := events.EntityTransactionResponseJson{
			ID:        ce.Id,
			Success:   true,
			Warnings:  diag.GetWarnings(),
			RequestID: ce.Id,
			TransactionInfo: events.EntityTransactionInfoJson{
				EntityIds: result.EntityIDs,
			},
		}
		if result.TransactionID != "" {
			resp.TransactionInfo.TransactionID = &result.TransactionID
		}
		respCE, err := NewCloudEvent(EntityTransactionResponse, resp)
		if err != nil {
			return status.Errorf(codes.Internal, "failed to build response: %v", err)
		}
		return stream.Send(respCE)

	case EntityUpdateCollectionRequest:
		dec := json.NewDecoder(bytes.NewReader(payload))
		dec.UseNumber()
		var req events.EntityUpdateCollectionRequestJson
		if err := dec.Decode(&req); err != nil {
			return status.Errorf(codes.InvalidArgument, "invalid payload: %v", err)
		}

		// ONE deadline for the whole per-item loop below — not one per item —
		// so transactionTimeoutMs bounds the entire collection update, matching
		// the transactionWindow-chunked HTTP endpoint's contract.
		opCtx, cancelTimeout, terr := resolveEventTimeout(ctx, req.TransactionTimeoutMs, "transactionTimeoutMs")
		if terr != nil {
			respCE, ceErr := entityTransactionError(ctx, ce.Id, terr)
			if ceErr != nil {
				return status.Errorf(codes.Internal, "failed to build error response: %v", ceErr)
			}
			return stream.Send(respCE)
		}
		defer cancelTimeout()

		if len(req.Payloads) == 0 {
			return status.Errorf(codes.InvalidArgument, "payloads array is empty")
		}

		format := string(req.DataFormat)
		if format == "" {
			format = "JSON"
		}

		// Update each entity individually, collecting IDs.
		allIDs := make([]string, 0, len(req.Payloads))
		var lastTxID string
		for _, p := range req.Payloads {
			// Generic cancellation check at the iteration head (spec D9) —
			// fires on ANY ctx cancellation, not only our own feature
			// deadline. Routed through the identical whole-envelope error
			// path a genuine per-item failure below already takes: earlier
			// items already committed and are durable, this item and any
			// after it are never attempted.
			if ctxErr := opCtx.Err(); ctxErr != nil {
				loopErr := fmt.Errorf("operation aborted: %w", ctxErr)
				if appErr := common.ClassifyRequestTimeout(opCtx, loopErr, common.ErrCodeTransactionTimeout); appErr != nil {
					loopErr = appErr
				}
				slog.Error("operation failed", "pkg", "grpc", "rpc", "entityManageCollection", "type", eventType, "ceId", ce.Id, "error", loopErr.Error())
				respCE, ceErr := entityTransactionError(ctx, ce.Id, loopErr)
				if ceErr != nil {
					return status.Errorf(codes.Internal, "failed to build error response: %v", ceErr)
				}
				return stream.Send(respCE)
			}

			transition := ""
			if p.Transition != nil {
				transition = *p.Transition
			}

			result, err := s.entityHandler.UpdateEntity(opCtx, entity.UpdateEntityInput{
				EntityID:   p.EntityID,
				Format:     format,
				Data:       p.Data,
				Transition: transition,
			})
			if err != nil {
				if appErr := common.ClassifyRequestTimeout(opCtx, err, common.ErrCodeTransactionTimeout); appErr != nil {
					err = appErr
				}
				slog.Error("operation failed", "pkg", "grpc", "rpc", "entityManageCollection", "type", eventType, "ceId", ce.Id, "error", err.Error())
				respCE, ceErr := entityTransactionError(ctx, ce.Id, err)
				if ceErr != nil {
					return status.Errorf(codes.Internal, "failed to build error response: %v", ceErr)
				}
				return stream.Send(respCE)
			}
			allIDs = append(allIDs, result.EntityIDs...)
			lastTxID = result.TransactionID
		}

		diag := common.GetDiagnostics(ctx)
		resp := events.EntityTransactionResponseJson{
			ID:        ce.Id,
			Success:   true,
			Warnings:  diag.GetWarnings(),
			RequestID: ce.Id,
			TransactionInfo: events.EntityTransactionInfoJson{
				EntityIds: allIDs,
			},
		}
		if lastTxID != "" {
			resp.TransactionInfo.TransactionID = &lastTxID
		}
		respCE, err := NewCloudEvent(EntityTransactionResponse, resp)
		if err != nil {
			return status.Errorf(codes.Internal, "failed to build response: %v", err)
		}
		return stream.Send(respCE)

	case EntityDeleteAllRequest:
		dec := json.NewDecoder(bytes.NewReader(payload))
		dec.UseNumber()
		var req events.EntityDeleteAllRequestJson
		if err := dec.Decode(&req); err != nil {
			return status.Errorf(codes.InvalidArgument, "invalid payload: %v", err)
		}

		// An explicit transactionSize switches to the batched,
		// enumerate-then-delete path (spec D4): validate it's a positive
		// integer, reject a joined (tx-token'd) request the same way
		// resolveEventTimeout rejects transactionTimeoutMs on one (spec
		// D7) — honoring it would let a participant unilaterally
		// fragment a transaction the owner still controls. A nil field
		// (the schema no longer bakes a default) leaves today's
		// single-tx DeleteAllEntities path byte-for-byte unchanged.
		if req.TransactionSize != nil {
			size := *req.TransactionSize
			if size < 1 {
				respCE, ceErr := entityDeleteAllError(ctx, ce.Id, common.Operational(http.StatusBadRequest, common.ErrCodeBadRequest,
					"transactionSize must be a positive integer"))
				if ceErr != nil {
					return status.Errorf(codes.Internal, "failed to build error response: %v", ceErr)
				}
				return stream.Send(respCE)
			}
			if spi.GetTransaction(ctx) != nil {
				respCE, ceErr := entityDeleteAllError(ctx, ce.Id, common.Operational(http.StatusBadRequest, common.ErrCodeBadRequest,
					"transactionSize is not supported on a request that joins an open transaction"))
				if ceErr != nil {
					return status.Errorf(codes.Internal, "failed to build error response: %v", ceErr)
				}
				return stream.Send(respCE)
			}

			delRes, err := s.entityHandler.DeleteEntitiesConditional(ctx, req.Model.Name, fmt.Sprintf("%d", req.Model.Version), nil, nil, false, size)
			if err != nil {
				slog.Error("operation failed", "pkg", "grpc", "rpc", "entityManageCollection", "type", eventType, "ceId", ce.Id, "error", err.Error())
				respCE, ceErr := entityDeleteAllError(ctx, ce.Id, err)
				if ceErr != nil {
					return status.Errorf(codes.Internal, "failed to build error response: %v", ceErr)
				}
				return stream.Send(respCE)
			}

			diag := common.GetDiagnostics(ctx)
			resp := events.EntityDeleteAllResponseJson{
				ID:         ce.Id,
				Success:    true,
				Warnings:   diag.GetWarnings(),
				RequestID:  ce.Id,
				ModelID:    delRes.EntityModelID,
				NumDeleted: delRes.RemovedCount,
				EntityIds:  []string{},
				ErrorsByID: deleteAllErrorsByID(delRes.IDToError),
			}
			respCE, err := NewCloudEvent(EntityDeleteAllResponse, resp)
			if err != nil {
				return status.Errorf(codes.Internal, "failed to build response: %v", err)
			}
			return stream.Send(respCE)
		}

		result, err := s.entityHandler.DeleteAllEntities(ctx, req.Model.Name, fmt.Sprintf("%d", req.Model.Version))
		if err != nil {
			slog.Error("operation failed", "pkg", "grpc", "rpc", "entityManageCollection", "type", eventType, "ceId", ce.Id, "error", err.Error())
			respCE, ceErr := entityDeleteAllError(ctx, ce.Id, err)
			if ceErr != nil {
				return status.Errorf(codes.Internal, "failed to build error response: %v", ceErr)
			}
			return stream.Send(respCE)
		}

		diag := common.GetDiagnostics(ctx)
		resp := events.EntityDeleteAllResponseJson{
			ID:         ce.Id,
			Success:    true,
			Warnings:   diag.GetWarnings(),
			RequestID:  ce.Id,
			ModelID:    result.ModelID,
			NumDeleted: result.TotalCount,
			EntityIds:  []string{},
		}
		respCE, err := NewCloudEvent(EntityDeleteAllResponse, resp)
		if err != nil {
			return status.Errorf(codes.Internal, "failed to build response: %v", err)
		}
		return stream.Send(respCE)

	default:
		slog.Warn("unsupported event type", "pkg", "grpc", "rpc", "entityManageCollection", "type", eventType)
		return status.Errorf(codes.InvalidArgument, "unsupported collection event type: %s", eventType)
	}
}

// deleteAllErrorsByID converts a DeleteResult.IDToError map into the
// schema's ErrorsByID shape (map[string]interface{}). The batched
// (transactionSize>0) delete-all path folds per-id and per-batch-commit
// failures into IDToError and still returns Success:true — RemovedCount can
// be less than MatchedCount — so the caller must be able to see which ids
// failed and why, the same way the HTTP door's idToError field already does.
// An empty/nil input returns nil so the omitempty field is left off the
// wire, matching the schema's optionality (no failures ⇒ no field).
func deleteAllErrorsByID(idToError map[string]string) events.EntityDeleteAllResponseJsonErrorsByID {
	if len(idToError) == 0 {
		return nil
	}
	out := make(events.EntityDeleteAllResponseJsonErrorsByID, len(idToError))
	for id, msg := range idToError {
		out[id] = msg
	}
	return out
}
