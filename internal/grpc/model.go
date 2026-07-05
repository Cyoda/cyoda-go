package grpc

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	cepb "github.com/cyoda-platform/cyoda-go/api/grpc/cloudevents"
	events "github.com/cyoda-platform/cyoda-go/api/grpc/events"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/domain/model"
	"github.com/cyoda-platform/cyoda-go/internal/logging"
)

// setUniqueKeysPayload is the CloudEvent payload for EntityModelSetUniqueKeysRequest.
type setUniqueKeysPayload struct {
	ID         string               `json:"id"`
	Model      events.ModelSpecJson `json:"model"`
	UniqueKeys []uniqueKeyPayload   `json:"uniqueKeys"`
}

// uniqueKeyPayload is a single unique key definition in the CloudEvent payload.
type uniqueKeyPayload struct {
	ID     string   `json:"id"`
	Fields []string `json:"fields"`
}

// EntityModelManage handles unary model management CloudEvents (import, export,
// lock/unlock, delete, get all) by calling model service methods directly.
func (s *CloudEventsServiceImpl) EntityModelManage(ctx context.Context, ce *cepb.CloudEvent) (*cepb.CloudEvent, error) {
	ctx = common.WithDiagnostics(ctx)

	eventType, payload, err := ParseCloudEvent(ce)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid CloudEvent: %v", err)
	}

	slog.Debug("CloudEvent received", "pkg", "grpc", "rpc", "entityModelManage", "type", eventType, "ceId", ce.Id, "payload", logging.PayloadPreview(payload, 200))

	switch eventType {
	case EntityModelImportRequest:
		return s.handleModelImport(ctx, ce.Id, eventType, payload)
	case EntityModelExportRequest:
		return s.handleModelExport(ctx, ce.Id, eventType, payload)
	case EntityModelTransitionRequest:
		return s.handleModelTransition(ctx, ce.Id, eventType, payload)
	case EntityModelDeleteRequest:
		return s.handleModelDelete(ctx, ce.Id, eventType, payload)
	case EntityModelGetAllRequest:
		return s.handleModelGetAll(ctx, ce.Id, eventType)
	case EntityModelSetUniqueKeysRequest:
		return s.handleModelSetUniqueKeys(ctx, ce.Id, eventType, payload)
	default:
		slog.Warn("unsupported event type", "pkg", "grpc", "rpc", "entityModelManage", "type", eventType)
		return nil, status.Errorf(codes.InvalidArgument, "unsupported model event type: %s", eventType)
	}
}

func (s *CloudEventsServiceImpl) handleModelImport(ctx context.Context, ceID string, eventType string, payload json.RawMessage) (*cepb.CloudEvent, error) {
	var req events.EntityModelImportRequestJson
	if err := json.Unmarshal(payload, &req); err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid payload: %v", err)
	}

	dataFormat := string(req.DataFormat)
	if dataFormat == "" {
		dataFormat = "JSON"
	}
	converter := string(req.Converter)
	if converter == "" {
		converter = "SAMPLE_DATA"
	}

	dataBytes, err := json.Marshal(req.Payload)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "failed to marshal payload: %v", err)
	}

	result, err := s.modelHandler.ImportModel(ctx, model.ImportModelInput{
		EntityName:   req.Model.Name,
		ModelVersion: fmt.Sprintf("%d", req.Model.Version),
		Format:       dataFormat,
		Converter:    converter,
		Data:         dataBytes,
	})
	if err != nil {
		slog.Error("operation failed", "pkg", "grpc", "rpc", "entityModelManage", "type", eventType, "ceId", ceID, "error", err.Error())
		return modelImportError(ctx, ceID, err)
	}

	diag := common.GetDiagnostics(ctx)
	resp := events.EntityModelImportResponseJson{
		ID:       ceID,
		Success:  true,
		Warnings: diag.GetWarnings(),
		ModelID:  result.ModelID,
	}
	slog.Debug("CloudEvent response", "pkg", "grpc", "rpc", "entityModelManage", "type", EntityModelImportResponse, "ceId", ceID, "success", true)
	return NewCloudEvent(EntityModelImportResponse, resp)
}

func (s *CloudEventsServiceImpl) handleModelExport(ctx context.Context, ceID string, eventType string, payload json.RawMessage) (*cepb.CloudEvent, error) {
	var req events.EntityModelExportRequestJson
	if err := json.Unmarshal(payload, &req); err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid payload: %v", err)
	}

	converter := string(req.Converter)
	if converter == "" {
		converter = "JSON_SCHEMA"
	}

	result, err := s.modelHandler.ExportModel(ctx, req.Model.Name, fmt.Sprintf("%d", req.Model.Version), converter)
	if err != nil {
		slog.Error("operation failed", "pkg", "grpc", "rpc", "entityModelManage", "type", eventType, "ceId", ceID, "error", err.Error())
		return modelExportError(ctx, ceID, err)
	}

	var exportedPayload any
	if err := json.Unmarshal(result.Payload, &exportedPayload); err != nil {
		exportedPayload = string(result.Payload)
	}

	diag := common.GetDiagnostics(ctx)
	resp := events.EntityModelExportResponseJson{
		ID:       ceID,
		Success:  true,
		Warnings: diag.GetWarnings(),
		Model:    req.Model,
		Payload:  exportedPayload,
	}
	slog.Debug("CloudEvent response", "pkg", "grpc", "rpc", "entityModelManage", "type", EntityModelExportResponse, "ceId", ceID, "success", true)
	return NewCloudEvent(EntityModelExportResponse, resp)
}

func (s *CloudEventsServiceImpl) handleModelTransition(ctx context.Context, ceID string, eventType string, payload json.RawMessage) (*cepb.CloudEvent, error) {
	var req events.EntityModelTransitionRequestJson
	if err := json.Unmarshal(payload, &req); err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid payload: %v", err)
	}

	transition := strings.ToUpper(string(req.Transition))
	version := fmt.Sprintf("%d", req.Model.Version)

	var result *model.ModelTransitionResult
	var err error

	switch transition {
	case "LOCK":
		result, err = s.modelHandler.LockModel(ctx, req.Model.Name, version)
	case "UNLOCK":
		result, err = s.modelHandler.UnlockModel(ctx, req.Model.Name, version)
	default:
		return nil, status.Errorf(codes.InvalidArgument, "unsupported transition: %s", transition)
	}

	if err != nil {
		slog.Error("operation failed", "pkg", "grpc", "rpc", "entityModelManage", "type", eventType, "ceId", ceID, "error", err.Error())
		return modelTransitionError(ctx, ceID, err)
	}

	diag := common.GetDiagnostics(ctx)
	resp := events.EntityModelTransitionResponseJson{
		ID:       ceID,
		Success:  true,
		Warnings: diag.GetWarnings(),
		ModelID:  result.ModelID,
		State:    result.State,
	}
	slog.Debug("CloudEvent response", "pkg", "grpc", "rpc", "entityModelManage", "type", EntityModelTransitionResponse, "ceId", ceID, "success", true)
	return NewCloudEvent(EntityModelTransitionResponse, resp)
}

func (s *CloudEventsServiceImpl) handleModelDelete(ctx context.Context, ceID string, eventType string, payload json.RawMessage) (*cepb.CloudEvent, error) {
	var req events.EntityModelDeleteRequestJson
	if err := json.Unmarshal(payload, &req); err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid payload: %v", err)
	}

	if err := s.modelHandler.DeleteModel(ctx, req.Model.Name, fmt.Sprintf("%d", req.Model.Version)); err != nil {
		slog.Error("operation failed", "pkg", "grpc", "rpc", "entityModelManage", "type", eventType, "ceId", ceID, "error", err.Error())
		return modelDeleteError(ctx, ceID, err)
	}

	diag := common.GetDiagnostics(ctx)
	resp := events.EntityModelDeleteResponseJson{
		ID:       ceID,
		Success:  true,
		Warnings: diag.GetWarnings(),
	}
	slog.Debug("CloudEvent response", "pkg", "grpc", "rpc", "entityModelManage", "type", EntityModelDeleteResponse, "ceId", ceID, "success", true)
	return NewCloudEvent(EntityModelDeleteResponse, resp)
}

func (s *CloudEventsServiceImpl) handleModelGetAll(ctx context.Context, ceID string, eventType string) (*cepb.CloudEvent, error) {
	models, err := s.modelHandler.ListModels(ctx)
	if err != nil {
		slog.Error("operation failed", "pkg", "grpc", "rpc", "entityModelManage", "type", eventType, "ceId", ceID, "error", err.Error())
		return modelGetAllError(ctx, ceID, err)
	}

	modelInfos := make([]events.ModelInfoJson, 0, len(models))
	for _, m := range models {
		modelInfos = append(modelInfos, events.ModelInfoJson{
			ID:      m.ID,
			Name:    m.Name,
			Version: m.Version,
			State:   m.State,
		})
	}

	diag := common.GetDiagnostics(ctx)
	resp := events.EntityModelGetAllResponseJson{
		ID:       ceID,
		Success:  true,
		Warnings: diag.GetWarnings(),
		Models:   modelInfos,
	}
	slog.Debug("CloudEvent response", "pkg", "grpc", "rpc", "entityModelManage", "type", EntityModelGetAllResponse, "ceId", ceID, "success", true)
	return NewCloudEvent(EntityModelGetAllResponse, resp)
}

func (s *CloudEventsServiceImpl) handleModelSetUniqueKeys(ctx context.Context, ceID string, eventType string, payload json.RawMessage) (*cepb.CloudEvent, error) {
	var req setUniqueKeysPayload
	if err := json.Unmarshal(payload, &req); err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid payload: %v", err)
	}

	keys := make([]spi.UniqueKey, 0, len(req.UniqueKeys))
	for _, k := range req.UniqueKeys {
		keys = append(keys, spi.UniqueKey{ID: k.ID, Fields: k.Fields})
	}

	result, err := s.modelHandler.SetUniqueKeys(ctx, req.Model.Name, fmt.Sprintf("%d", req.Model.Version), keys)
	if err != nil {
		slog.Error("operation failed", "pkg", "grpc", "rpc", "entityModelManage", "type", eventType, "ceId", ceID, "error", err.Error())
		return modelSetUniqueKeysError(ctx, ceID, err)
	}

	diag := common.GetDiagnostics(ctx)
	resp := events.EntityModelTransitionResponseJson{
		ID:       ceID,
		Success:  true,
		Warnings: diag.GetWarnings(),
		ModelID:  result.ModelID,
		State:    result.State,
	}
	slog.Debug("CloudEvent response", "pkg", "grpc", "rpc", "entityModelManage", "type", EntityModelSetUniqueKeysResponse, "ceId", ceID, "success", true)
	return NewCloudEvent(EntityModelSetUniqueKeysResponse, resp)
}
