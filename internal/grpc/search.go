package grpc

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go-spi/predicate"
	cepb "github.com/cyoda-platform/cyoda-go/api/grpc/cloudevents"
	cyodapb "github.com/cyoda-platform/cyoda-go/api/grpc/cyoda"
	events "github.com/cyoda-platform/cyoda-go/api/grpc/events"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/domain/entity"
	"github.com/cyoda-platform/cyoda-go/internal/domain/pagination"
	"github.com/cyoda-platform/cyoda-go/internal/domain/search"
	"github.com/cyoda-platform/cyoda-go/internal/logging"
)

// EntitySearch handles unary search CloudEvents by calling service methods directly.
func (s *CloudEventsServiceImpl) EntitySearch(ctx context.Context, ce *cepb.CloudEvent) (*cepb.CloudEvent, error) {
	ctx = common.WithDiagnostics(ctx)

	eventType, payload, err := ParseCloudEvent(ce)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid CloudEvent: %v", err)
	}

	slog.Debug("CloudEvent received", "pkg", "grpc", "rpc", "entitySearch", "type", eventType, "ceId", ce.Id, "payload", logging.PayloadPreview(payload, 200))

	switch eventType {
	case EntityGetRequest:
		return s.handleEntityGetRequest(ctx, ce, payload)

	case EntitySnapshotSearchRequest:
		return s.handleSnapshotSearchRequest(ctx, ce, payload)

	case SnapshotGetStatusRequest:
		return s.handleSnapshotGetStatusRequest(ctx, ce, payload)

	case SnapshotCancelRequest:
		return s.handleSnapshotCancelRequest(ctx, ce, payload)

	default:
		slog.Warn("unsupported event type", "pkg", "grpc", "rpc", "entitySearch", "type", eventType)
		return nil, status.Errorf(codes.InvalidArgument, "unsupported search event type: %s", eventType)
	}
}

// EntitySearchCollection handles server-streaming search CloudEvents.
func (s *CloudEventsServiceImpl) EntitySearchCollection(ce *cepb.CloudEvent, stream cyodapb.CloudEventsService_EntitySearchCollectionServer) error {
	ctx := stream.Context()
	ctx = common.WithDiagnostics(ctx)

	eventType, payload, err := ParseCloudEvent(ce)
	if err != nil {
		return status.Errorf(codes.InvalidArgument, "invalid CloudEvent: %v", err)
	}

	slog.Debug("CloudEvent received", "pkg", "grpc", "rpc", "entitySearchCollection", "type", eventType, "ceId", ce.Id, "payload", logging.PayloadPreview(payload, 200))

	switch eventType {
	case EntityGetAllRequest:
		return s.handleEntityGetAllRequest(ctx, ce, payload, stream)

	case EntitySearchRequest:
		return s.handleDirectSearchRequest(ctx, ce, payload, stream)

	case EntityStatsGetRequest:
		return s.handleEntityStatsGetRequest(ctx, ce, payload, stream)

	case EntityStatsByStateGetRequest:
		return s.handleEntityStatsByStateGetRequest(ctx, ce, payload, stream)

	case EntityChangesMetadataGetRequest:
		return s.handleEntityChangesMetadataGetRequest(ctx, ce, payload, stream)

	case SnapshotGetRequest:
		return s.handleSnapshotGetRequestStreaming(ctx, ce, payload, stream)

	default:
		slog.Warn("unsupported event type", "pkg", "grpc", "rpc", "entitySearchCollection", "type", eventType)
		return status.Errorf(codes.InvalidArgument, "unsupported search collection event type: %s", eventType)
	}
}

// ---------------------------------------------------------------------------
// Unary handlers
// ---------------------------------------------------------------------------

func (s *CloudEventsServiceImpl) handleEntityGetRequest(ctx context.Context, ce *cepb.CloudEvent, payload json.RawMessage) (*cepb.CloudEvent, error) {
	var req events.EntityGetRequestJson
	if err := json.Unmarshal(payload, &req); err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid payload: %v", err)
	}

	envelope, err := s.entityHandler.GetEntity(ctx, entity.GetOneEntityInput{
		EntityID:    req.EntityID,
		PointInTime: req.PointInTime,
	})
	if err != nil {
		slog.Error("operation failed", "pkg", "grpc", "rpc", "entitySearch", "type", EntityGetRequest, "ceId", ce.Id, "error", err.Error())
		return entityResponseError(ctx, ce.Id, err)
	}

	diag := common.GetDiagnostics(ctx)
	resp := events.EntityResponseJson{
		ID:        ce.Id,
		Success:   true,
		Warnings:  diag.GetWarnings(),
		RequestID: ce.Id,
		Payload: events.DataPayloadJson{
			Type: "JSON",
			Data: envelope.Data,
			Meta: envelope.Meta,
		},
	}
	slog.Debug("CloudEvent response", "pkg", "grpc", "rpc", "entitySearch", "type", EntityResponse, "ceId", ce.Id, "success", true)
	return NewCloudEvent(EntityResponse, resp)
}

func (s *CloudEventsServiceImpl) handleSnapshotSearchRequest(ctx context.Context, ce *cepb.CloudEvent, payload json.RawMessage) (*cepb.CloudEvent, error) {
	var req events.EntitySnapshotSearchRequestJson
	if err := json.Unmarshal(payload, &req); err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid payload: %v", err)
	}

	condBytes, err := json.Marshal(req.Condition)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "failed to marshal condition: %v", err)
	}
	cond, err := predicate.ParseCondition(condBytes)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid condition: %v", err)
	}

	modelRef := spi.ModelRef{
		EntityName:   req.Model.Name,
		ModelVersion: fmt.Sprintf("%d", req.Model.Version),
	}
	opts := search.SearchOptions{
		PointInTime: req.PointInTime,
	}
	for _, o := range req.OrderBy {
		src := spi.SourceData
		if o.Source == events.EntitySnapshotSearchRequestJsonOrderByElemSourceMeta {
			src = spi.SourceMeta
		}
		opts.OrderBy = append(opts.OrderBy, search.OrderKey{Path: o.Path, Source: src, Desc: o.Desc})
	}

	snapshotID, err := s.searchService.SubmitAsyncSearch(ctx, modelRef, cond, opts)
	if err != nil {
		slog.Error("operation failed", "pkg", "grpc", "rpc", "entitySearch", "type", EntitySnapshotSearchRequest, "ceId", ce.Id, "error", err.Error())
		return snapshotSearchError(ctx, ce.Id, err)
	}

	// Return immediately with the snapshot ID. The caller polls for status
	// via handleSnapshotGetStatusRequest. No artificial sleep — async means async.
	diag := common.GetDiagnostics(ctx)
	zero := 0
	resp := events.EntitySnapshotSearchResponseJson{
		ID:       ce.Id,
		Success:  true,
		Warnings: diag.GetWarnings(),
		Status: events.SearchSnapshotStatusJson{
			SnapshotID:    snapshotID,
			Status:        events.SearchSnapshotStatusJsonStatus("RUNNING"),
			EntitiesCount: &zero,
		},
	}
	slog.Debug("CloudEvent response", "pkg", "grpc", "rpc", "entitySearch", "type", EntitySnapshotSearchResponse, "ceId", ce.Id, "success", true)
	return NewCloudEvent(EntitySnapshotSearchResponse, resp)
}

func (s *CloudEventsServiceImpl) handleSnapshotGetStatusRequest(ctx context.Context, ce *cepb.CloudEvent, payload json.RawMessage) (*cepb.CloudEvent, error) {
	var req events.SnapshotGetStatusRequestJson
	if err := json.Unmarshal(payload, &req); err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid payload: %v", err)
	}

	snapStatus, err := s.searchService.GetAsyncSearchStatus(ctx, req.SnapshotID)
	if err != nil {
		slog.Error("operation failed", "pkg", "grpc", "rpc", "entitySearch", "type", SnapshotGetStatusRequest, "ceId", ce.Id, "error", err.Error())
		return snapshotSearchError(ctx, ce.Id, err)
	}

	diag := common.GetDiagnostics(ctx)
	count := snapStatus.EntitiesCount
	resp := events.EntitySnapshotSearchResponseJson{
		ID:       ce.Id,
		Success:  true,
		Warnings: diag.GetWarnings(),
		Status: events.SearchSnapshotStatusJson{
			SnapshotID:    snapStatus.SnapshotID,
			Status:        events.SearchSnapshotStatusJsonStatus(snapStatus.Status),
			EntitiesCount: &count,
		},
	}
	slog.Debug("CloudEvent response", "pkg", "grpc", "rpc", "entitySearch", "type", EntitySnapshotSearchResponse, "ceId", ce.Id, "success", true)
	return NewCloudEvent(EntitySnapshotSearchResponse, resp)
}

func (s *CloudEventsServiceImpl) handleSnapshotCancelRequest(ctx context.Context, ce *cepb.CloudEvent, payload json.RawMessage) (*cepb.CloudEvent, error) {
	var req events.SnapshotCancelRequestJson
	if err := json.Unmarshal(payload, &req); err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid payload: %v", err)
	}

	err := s.searchService.CancelAsyncSearch(ctx, req.SnapshotID)
	if err != nil {
		slog.Error("operation failed", "pkg", "grpc", "rpc", "entitySearch", "type", SnapshotCancelRequest, "ceId", ce.Id, "error", err.Error())
		return snapshotSearchError(ctx, ce.Id, err)
	}

	// After cancel, fetch updated status.
	snapStatus, err := s.searchService.GetAsyncSearchStatus(ctx, req.SnapshotID)
	if err != nil {
		slog.Error("operation failed", "pkg", "grpc", "rpc", "entitySearch", "type", SnapshotCancelRequest, "ceId", ce.Id, "error", err.Error())
		return snapshotSearchError(ctx, ce.Id, err)
	}

	diag := common.GetDiagnostics(ctx)
	count := snapStatus.EntitiesCount
	resp := events.EntitySnapshotSearchResponseJson{
		ID:       ce.Id,
		Success:  true,
		Warnings: diag.GetWarnings(),
		Status: events.SearchSnapshotStatusJson{
			SnapshotID:    snapStatus.SnapshotID,
			Status:        events.SearchSnapshotStatusJsonStatus(snapStatus.Status),
			EntitiesCount: &count,
		},
	}
	slog.Debug("CloudEvent response", "pkg", "grpc", "rpc", "entitySearch", "type", EntitySnapshotSearchResponse, "ceId", ce.Id, "success", true)
	return NewCloudEvent(EntitySnapshotSearchResponse, resp)
}

// ---------------------------------------------------------------------------
// Streaming handlers
// ---------------------------------------------------------------------------

func (s *CloudEventsServiceImpl) handleEntityGetAllRequest(ctx context.Context, ce *cepb.CloudEvent, payload json.RawMessage, stream cyodapb.CloudEventsService_EntitySearchCollectionServer) error {
	var req events.EntityGetAllRequestJson
	if err := json.Unmarshal(payload, &req); err != nil {
		return status.Errorf(codes.InvalidArgument, "invalid payload: %v", err)
	}

	pageSize := req.PageSize
	if pageSize <= 0 {
		pageSize = 20
	}
	pageNumber := req.PageNumber

	// Reject negative / over-cap / overflow-prone values BEFORE the
	// storage lookup (PR #149 follow-up). Without this guard, an
	// attacker-supplied PageNumber up to MaxInt would propagate to the
	// service layer and panic with a slice-bounds error.
	if vErr := pagination.ValidateOffset(int64(pageNumber), int64(pageSize)); vErr != nil {
		slog.Error("operation failed", "pkg", "grpc", "rpc", "entitySearchCollection", "type", EntityGetAllRequest, "ceId", ce.Id, "error", vErr.Error())
		errCE, ceErr := entityResponseError(ctx, ce.Id, vErr)
		if ceErr != nil {
			return status.Errorf(codes.Internal, "failed to build error response: %v", ceErr)
		}
		return stream.Send(errCE)
	}

	envelopes, err := s.entityHandler.ListEntities(ctx, req.Model.Name, fmt.Sprintf("%d", req.Model.Version), entity.PaginationParams{
		PageSize:   int32(pageSize),
		PageNumber: int32(pageNumber),
	}, nil)
	if err != nil {
		slog.Error("operation failed", "pkg", "grpc", "rpc", "entitySearchCollection", "type", EntityGetAllRequest, "ceId", ce.Id, "error", err.Error())
		errCE, ceErr := entityResponseError(ctx, ce.Id, err)
		if ceErr != nil {
			return status.Errorf(codes.Internal, "failed to build error response: %v", ceErr)
		}
		return stream.Send(errCE)
	}

	diag := common.GetDiagnostics(ctx)
	warnings := diag.GetWarnings()
	for _, env := range envelopes {
		resp := events.EntityResponseJson{
			ID:        ce.Id,
			Success:   true,
			Warnings:  warnings,
			RequestID: ce.Id,
			Payload: events.DataPayloadJson{
				Type: "JSON",
				Data: env.Data,
				Meta: env.Meta,
			},
		}
		respCE, err := NewCloudEvent(EntityResponse, resp)
		if err != nil {
			return status.Errorf(codes.Internal, "failed to build response: %v", err)
		}
		if err := stream.Send(respCE); err != nil {
			return err
		}
	}
	return nil
}

func (s *CloudEventsServiceImpl) handleDirectSearchRequest(ctx context.Context, ce *cepb.CloudEvent, payload json.RawMessage, stream cyodapb.CloudEventsService_EntitySearchCollectionServer) error {
	var req events.EntitySearchRequestJson
	if err := json.Unmarshal(payload, &req); err != nil {
		return status.Errorf(codes.InvalidArgument, "invalid payload: %v", err)
	}

	condBytes, err := json.Marshal(req.Condition)
	if err != nil {
		return status.Errorf(codes.InvalidArgument, "failed to marshal condition: %v", err)
	}
	cond, err := predicate.ParseCondition(condBytes)
	if err != nil {
		return status.Errorf(codes.InvalidArgument, "invalid condition: %v", err)
	}

	modelRef := spi.ModelRef{
		EntityName:   req.Model.Name,
		ModelVersion: fmt.Sprintf("%d", req.Model.Version),
	}
	opts := search.SearchOptions{
		PointInTime:  req.PointInTime,
		TrackingRead: req.TrackingRead,
	}
	if req.Limit != nil {
		opts.Limit = *req.Limit
	}
	for _, o := range req.OrderBy {
		src := spi.SourceData
		if o.Source == events.EntitySearchRequestJsonOrderByElemSourceMeta {
			src = spi.SourceMeta
		}
		opts.OrderBy = append(opts.OrderBy, search.OrderKey{Path: o.Path, Source: src, Desc: o.Desc})
	}

	results, err := s.searchService.DirectSearch(ctx, modelRef, cond, opts)
	if err != nil {
		slog.Error("operation failed", "pkg", "grpc", "rpc", "entitySearchCollection", "type", EntitySearchRequest, "ceId", ce.Id, "error", err.Error())
		errCE, ceErr := entityResponseError(ctx, ce.Id, err)
		if ceErr != nil {
			return status.Errorf(codes.Internal, "failed to build error response: %v", ceErr)
		}
		return stream.Send(errCE)
	}

	diag := common.GetDiagnostics(ctx)
	warnings := diag.GetWarnings()
	for _, e := range results {
		var data any
		dec := json.NewDecoder(bytes.NewReader(e.Data))
		dec.UseNumber()
		if err := dec.Decode(&data); err != nil {
			slog.Warn("failed to unmarshal entity data", "pkg", "grpc", "entityId", e.Meta.ID, "error", err)
			continue
		}

		resp := events.EntityResponseJson{
			ID:        ce.Id,
			Success:   true,
			Warnings:  warnings,
			RequestID: ce.Id,
			Payload: events.DataPayloadJson{
				Type: "JSON",
				Data: data,
				Meta: buildEntityMeta(e, nil),
			},
		}
		respCE, err := NewCloudEvent(EntityResponse, resp)
		if err != nil {
			return status.Errorf(codes.Internal, "failed to build response: %v", err)
		}
		if err := stream.Send(respCE); err != nil {
			return err
		}
	}
	return nil
}

func (s *CloudEventsServiceImpl) handleEntityStatsGetRequest(ctx context.Context, ce *cepb.CloudEvent, payload json.RawMessage, stream cyodapb.CloudEventsService_EntitySearchCollectionServer) error {
	var req events.EntityStatsGetRequestJson
	if err := json.Unmarshal(payload, &req); err != nil {
		return status.Errorf(codes.InvalidArgument, "invalid payload: %v", err)
	}

	// If a specific model is requested, get stats for that model only.
	if req.Model != nil && req.Model.Name != "" {
		stat, err := s.entityHandler.GetStatisticsForModel(ctx, req.Model.Name, fmt.Sprintf("%d", req.Model.Version))
		if err != nil {
			slog.Error("operation failed", "pkg", "grpc", "rpc", "entitySearchCollection", "type", EntityStatsGetRequest, "ceId", ce.Id, "error", err.Error())
			errCE, ceErr := entityStatsError(ctx, ce.Id, err)
			if ceErr != nil {
				return status.Errorf(codes.Internal, "failed to build error response: %v", ceErr)
			}
			return stream.Send(errCE)
		}

		diag := common.GetDiagnostics(ctx)
		ver, _ := strconv.Atoi(stat.ModelVersion)
		resp := events.EntityStatsResponseJson{
			ID:           ce.Id,
			Success:      true,
			Warnings:     diag.GetWarnings(),
			RequestID:    ce.Id,
			ModelName:    stat.ModelName,
			ModelVersion: ver,
			Count:        int(stat.Count),
		}
		respCE, err := NewCloudEvent(EntityStatsResponse, resp)
		if err != nil {
			return status.Errorf(codes.Internal, "failed to build response: %v", err)
		}
		return stream.Send(respCE)
	}

	// All models.
	stats, err := s.entityHandler.GetStatistics(ctx)
	if err != nil {
		slog.Error("operation failed", "pkg", "grpc", "rpc", "entitySearchCollection", "type", EntityStatsGetRequest, "ceId", ce.Id, "error", err.Error())
		errCE, ceErr := entityStatsError(ctx, ce.Id, err)
		if ceErr != nil {
			return status.Errorf(codes.Internal, "failed to build error response: %v", ceErr)
		}
		return stream.Send(errCE)
	}

	diag := common.GetDiagnostics(ctx)
	warnings := diag.GetWarnings()
	for _, stat := range stats {
		ver, _ := strconv.Atoi(stat.ModelVersion)
		resp := events.EntityStatsResponseJson{
			ID:           ce.Id,
			Success:      true,
			Warnings:     warnings,
			RequestID:    ce.Id,
			ModelName:    stat.ModelName,
			ModelVersion: ver,
			Count:        int(stat.Count),
		}
		respCE, err := NewCloudEvent(EntityStatsResponse, resp)
		if err != nil {
			return status.Errorf(codes.Internal, "failed to build response: %v", err)
		}
		if err := stream.Send(respCE); err != nil {
			return err
		}
	}
	return nil
}

func (s *CloudEventsServiceImpl) handleEntityStatsByStateGetRequest(ctx context.Context, ce *cepb.CloudEvent, payload json.RawMessage, stream cyodapb.CloudEventsService_EntitySearchCollectionServer) error {
	var req events.EntityStatsByStateGetRequestJson
	if err := json.Unmarshal(payload, &req); err != nil {
		return status.Errorf(codes.InvalidArgument, "invalid payload: %v", err)
	}

	var statesFilter *[]string
	if len(req.States) > 0 {
		statesFilter = &req.States
	}

	var results []entity.EntityStatByState
	if req.Model != nil && req.Model.Name != "" {
		var err error
		results, err = s.entityHandler.GetStatisticsByStateForModel(ctx, req.Model.Name, fmt.Sprintf("%d", req.Model.Version), statesFilter)
		if err != nil {
			slog.Error("operation failed", "pkg", "grpc", "rpc", "entitySearchCollection", "type", EntityStatsByStateGetRequest, "ceId", ce.Id, "error", err.Error())
			errCE, ceErr := entityStatsByStateError(ctx, ce.Id, err)
			if ceErr != nil {
				return status.Errorf(codes.Internal, "failed to build error response: %v", ceErr)
			}
			return stream.Send(errCE)
		}
	} else {
		var err error
		results, err = s.entityHandler.GetStatisticsByState(ctx, statesFilter)
		if err != nil {
			slog.Error("operation failed", "pkg", "grpc", "rpc", "entitySearchCollection", "type", EntityStatsByStateGetRequest, "ceId", ce.Id, "error", err.Error())
			errCE, ceErr := entityStatsByStateError(ctx, ce.Id, err)
			if ceErr != nil {
				return status.Errorf(codes.Internal, "failed to build error response: %v", ceErr)
			}
			return stream.Send(errCE)
		}
	}

	diag := common.GetDiagnostics(ctx)
	warnings := diag.GetWarnings()
	for _, stat := range results {
		ver, _ := strconv.Atoi(stat.ModelVersion)
		resp := events.EntityStatsByStateResponseJson{
			ID:           ce.Id,
			Success:      true,
			Warnings:     warnings,
			RequestID:    ce.Id,
			ModelName:    stat.ModelName,
			ModelVersion: ver,
			State:        stat.State,
			Count:        int(stat.Count),
		}
		respCE, err := NewCloudEvent(EntityStatsByStateResponse, resp)
		if err != nil {
			return status.Errorf(codes.Internal, "failed to build response: %v", err)
		}
		if err := stream.Send(respCE); err != nil {
			return err
		}
	}
	return nil
}

func (s *CloudEventsServiceImpl) handleEntityChangesMetadataGetRequest(ctx context.Context, ce *cepb.CloudEvent, payload json.RawMessage, stream cyodapb.CloudEventsService_EntitySearchCollectionServer) error {
	var req events.EntityChangesMetadataGetRequestJson
	if err := json.Unmarshal(payload, &req); err != nil {
		return status.Errorf(codes.InvalidArgument, "invalid payload: %v", err)
	}

	entries, err := s.entityHandler.GetChangesMetadata(ctx, req.EntityID, req.PointInTime)
	if err != nil {
		slog.Error("operation failed", "pkg", "grpc", "rpc", "entitySearchCollection", "type", EntityChangesMetadataGetRequest, "ceId", ce.Id, "error", err.Error())
		errCE, ceErr := entityChangesMetadataError(ctx, ce.Id, err)
		if ceErr != nil {
			return status.Errorf(codes.Internal, "failed to build error response: %v", ceErr)
		}
		return stream.Send(errCE)
	}

	diag := common.GetDiagnostics(ctx)
	warnings := diag.GetWarnings()
	for _, entry := range entries {
		changeType := events.EntityChangeMetaJsonChangeType(mapChangeType(entry.ChangeType))

		t, _ := time.Parse(time.RFC3339Nano, entry.TimeOfChange)

		changeMeta := events.EntityChangeMetaJson{
			ChangeType:   changeType,
			TimeOfChange: t,
			User:         entry.User,
		}
		if entry.TransactionID != "" {
			txID := entry.TransactionID
			changeMeta.TransactionID = &txID
		}

		resp := events.EntityChangesMetadataResponseJson{
			ID:         ce.Id,
			Success:    true,
			Warnings:   warnings,
			RequestID:  ce.Id,
			ChangeMeta: changeMeta,
		}
		respCE, err := NewCloudEvent(EntityChangesMetadataResponse, resp)
		if err != nil {
			return status.Errorf(codes.Internal, "failed to build response: %v", err)
		}
		if err := stream.Send(respCE); err != nil {
			return err
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// buildEntityMeta builds the meta map for an entity response. Mirrors the HTTP
// meta shape (design §6.3): includes modelKey, and pointInTime when the read
// was as-at a supplied point-in-time.
func buildEntityMeta(e *spi.Entity, pointInTime *time.Time) map[string]any {
	modelVersion, _ := strconv.Atoi(e.Meta.ModelRef.ModelVersion)
	meta := map[string]any{
		"id":             e.Meta.ID,
		"modelKey":       map[string]any{"name": e.Meta.ModelRef.EntityName, "version": modelVersion},
		"state":          e.Meta.State,
		"creationDate":   e.Meta.CreationDate.UTC().Format(time.RFC3339Nano),
		"lastUpdateTime": e.Meta.LastModifiedDate.UTC().Format(time.RFC3339Nano),
		"transactionId":  e.Meta.TransactionID,
	}
	if e.Meta.TransitionForLatestSave != "" {
		meta["transitionForLatestSave"] = e.Meta.TransitionForLatestSave
	}
	if pointInTime != nil {
		meta["pointInTime"] = pointInTime.UTC().Format(time.RFC3339Nano)
	}
	return meta
}

// mapChangeType maps internal change type strings to the enum values expected
// by the generated EntityChangeMetaJsonChangeType.
// Delegates to common.CanonicalChangeType (E8 shared mapping).
func mapChangeType(ct string) string {
	return common.CanonicalChangeType(ct)
}

// handleSnapshotGetRequestStreaming streams entity results from a completed snapshot.
func (s *CloudEventsServiceImpl) handleSnapshotGetRequestStreaming(
	ctx context.Context,
	ce *cepb.CloudEvent,
	payload json.RawMessage,
	stream cyodapb.CloudEventsService_EntitySearchCollectionServer,
) error {
	var req events.SnapshotGetRequestJson
	if err := json.Unmarshal(payload, &req); err != nil {
		return status.Errorf(codes.InvalidArgument, "invalid payload: %v", err)
	}

	// Reject negative / over-cap / overflow-prone values BEFORE the
	// snapshot lookup (PR #149 follow-up). The HTTP async-results path
	// already validates here; the gRPC entry point did not, leaving the
	// same offset = pageNumber*pageSize multiplication exposed.
	if vErr := pagination.ValidateOffset(int64(req.PageNumber), int64(req.PageSize)); vErr != nil {
		slog.Error("operation failed", "pkg", "grpc", "rpc", "entitySearchCollection", "type", SnapshotGetRequest, "ceId", ce.Id, "error", vErr.Error())
		errCE, ceErr := entityResponseError(ctx, ce.Id, vErr)
		if ceErr != nil {
			return status.Errorf(codes.Internal, "failed to build error response: %v", ceErr)
		}
		return stream.Send(errCE)
	}

	results, err := s.searchService.GetAsyncSearchResults(ctx, req.SnapshotID, req.PageNumber, req.PageSize)
	if err != nil {
		slog.Error("operation failed", "pkg", "grpc", "rpc", "entitySearchCollection", "type", SnapshotGetRequest, "ceId", ce.Id, "error", err.Error())
		errCE, ceErr := entityResponseError(ctx, ce.Id, err)
		if ceErr != nil {
			return status.Errorf(codes.Internal, "failed to build error response: %v", ceErr)
		}
		return stream.Send(errCE)
	}

	diag := common.GetDiagnostics(ctx)
	warnings := diag.GetWarnings()
	for _, e := range results {
		var data any
		if e.Data != nil {
			dec := json.NewDecoder(bytes.NewReader(e.Data))
			dec.UseNumber()
			if err := dec.Decode(&data); err != nil {
				slog.Warn("failed to unmarshal entity data", "pkg", "grpc", "entityId", e.Meta.ID, "error", err)
				continue
			}
		}
		resp := events.EntityResponseJson{
			ID:        ce.Id,
			Success:   true,
			Warnings:  warnings,
			RequestID: ce.Id,
			Payload: events.DataPayloadJson{
				Type: "JSON",
				Data: data,
				Meta: buildEntityMeta(e, nil),
			},
		}
		respCE, err := NewCloudEvent(EntityResponse, resp)
		if err != nil {
			return status.Errorf(codes.Internal, "failed to build response: %v", err)
		}
		if err := stream.Send(respCE); err != nil {
			return err
		}
	}
	return nil
}
