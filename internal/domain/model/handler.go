package model

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"

	spi "github.com/cyoda-platform/cyoda-go-spi"

	"github.com/google/uuid"

	genapi "github.com/cyoda-platform/cyoda-go/api"
	"github.com/cyoda-platform/cyoda-go/internal/common"
)

// Handler implements the model management HTTP endpoints.
type Handler struct {
	factory spi.StoreFactory
}

// New returns a new Handler wired to the given StoreFactory.
func New(factory spi.StoreFactory) *Handler {
	return &Handler{factory: factory}
}

// modelRef builds a ModelRef from the path parameters.
func modelRef(entityName string, modelVersion int32) spi.ModelRef {
	return spi.ModelRef{
		EntityName:   entityName,
		ModelVersion: fmt.Sprintf("%d", modelVersion),
	}
}

// deterministicID derives a stable UUID v5 from a ModelRef so the same model
// always returns the same identifier.
func deterministicID(ref spi.ModelRef) uuid.UUID {
	return uuid.NewSHA1(uuid.NameSpaceURL, []byte(ref.String()))
}

// modelNotFound returns an AppError for a missing model, including properties.
func modelNotFound(entityName string, modelVersion int32) *common.AppError {
	appErr := common.Operational(http.StatusNotFound, common.ErrCodeModelNotFound,
		fmt.Sprintf("cannot find model entityName=%s, version=%d", entityName, modelVersion))
	appErr.Props = map[string]any{
		"entityName":    entityName,
		"entityVersion": modelVersion,
	}
	return appErr
}

// successResult returns an EntityModelActionResultDto with success=true.
func successResult(msg, entityName string, modelVersion int32) genapi.EntityModelActionResultDto {
	ref := modelRef(entityName, modelVersion)
	return genapi.EntityModelActionResultDto{
		Success: true,
		Message: msg,
		ModelId: deterministicID(ref),
		ModelKey: genapi.EntityModelKey{
			Name:    entityName,
			Version: modelVersion,
		},
	}
}

// writeServiceError maps a service-layer error to an HTTP response.
func writeServiceError(w http.ResponseWriter, r *http.Request, err error) {
	var appErr *common.AppError
	if errors.As(err, &appErr) {
		common.WriteError(w, r, appErr)
		return
	}
	common.WriteError(w, r, common.Internal("unexpected error", err))
}

// ImportEntityModel handles POST /model/import/{dataFormat}/{converter}/{entityName}/{modelVersion}.
func (h *Handler) ImportEntityModel(w http.ResponseWriter, r *http.Request, dataFormat genapi.ImportEntityModelParamsDataFormat, converter genapi.ImportEntityModelParamsConverter, entityName string, modelVersion int32) {
	// Read request body (with size limit).
	r.Body = http.MaxBytesReader(w, r.Body, 10*1024*1024)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		common.WriteError(w, r, common.Operational(http.StatusBadRequest, common.ErrCodeBadRequest, "failed to read body"))
		return
	}

	result, err := h.ImportModel(r.Context(), ImportModelInput{
		EntityName:   entityName,
		ModelVersion: strconv.Itoa(int(modelVersion)),
		Format:       string(dataFormat),
		Converter:    string(converter),
		Data:         body,
	})
	if err != nil {
		writeServiceError(w, r, err)
		return
	}

	common.WriteJSON(w, http.StatusOK, result.ModelID)
}

// ExportMetadata handles GET /model/export/{converter}/{entityName}/{modelVersion}.
func (h *Handler) ExportMetadata(w http.ResponseWriter, r *http.Request, converter genapi.ExportMetadataParamsConverter, entityName string, modelVersion int32) {
	result, err := h.ExportModel(r.Context(), entityName, strconv.Itoa(int(modelVersion)), string(converter))
	if err != nil {
		writeServiceError(w, r, err)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	w.Write(result.Payload)
}

// LockEntityModel handles PUT /model/{entityName}/{modelVersion}/lock.
func (h *Handler) LockEntityModel(w http.ResponseWriter, r *http.Request, entityName string, modelVersion int32) {
	_, err := h.LockModel(r.Context(), entityName, strconv.Itoa(int(modelVersion)))
	if err != nil {
		writeServiceError(w, r, err)
		return
	}

	common.WriteJSON(w, http.StatusOK, successResult(
		fmt.Sprintf("Model %s:%d locked", entityName, modelVersion),
		entityName, modelVersion))
}

// UnlockEntityModel handles PUT /model/{entityName}/{modelVersion}/unlock.
func (h *Handler) UnlockEntityModel(w http.ResponseWriter, r *http.Request, entityName string, modelVersion int32) {
	_, err := h.UnlockModel(r.Context(), entityName, strconv.Itoa(int(modelVersion)))
	if err != nil {
		writeServiceError(w, r, err)
		return
	}

	common.WriteJSON(w, http.StatusOK, successResult(
		fmt.Sprintf("Model %s:%d unlocked", entityName, modelVersion),
		entityName, modelVersion))
}

// DeleteEntityModel handles DELETE /model/{entityName}/{modelVersion}.
func (h *Handler) DeleteEntityModel(w http.ResponseWriter, r *http.Request, entityName string, modelVersion int32) {
	err := h.DeleteModel(r.Context(), entityName, strconv.Itoa(int(modelVersion)))
	if err != nil {
		writeServiceError(w, r, err)
		return
	}

	common.WriteJSON(w, http.StatusOK, successResult(
		fmt.Sprintf("model %s:%d deleted", entityName, modelVersion),
		entityName, modelVersion))
}

// SetEntityModelChangeLevel handles POST /model/{entityName}/{modelVersion}/changeLevel/{changeLevel}.
func (h *Handler) SetEntityModelChangeLevel(w http.ResponseWriter, r *http.Request, entityName string, modelVersion int32, changeLevel genapi.SetEntityModelChangeLevelParamsChangeLevel) {
	err := h.SetChangeLevel(r.Context(), entityName, strconv.Itoa(int(modelVersion)), string(changeLevel))
	if err != nil {
		writeServiceError(w, r, err)
		return
	}

	common.WriteJSON(w, http.StatusOK, successResult(
		fmt.Sprintf("model %s:%d now at change level %s", entityName, modelVersion, changeLevel),
		entityName, modelVersion))
}

// GetAvailableEntityModels handles GET /model/.
func (h *Handler) GetAvailableEntityModels(w http.ResponseWriter, r *http.Request) {
	infos, err := h.ListModels(r.Context())
	if err != nil {
		writeServiceError(w, r, err)
		return
	}

	models := make([]genapi.EntityModelDto, 0, len(infos))
	for _, info := range infos {
		now := info.UpdateDate
		models = append(models, genapi.EntityModelDto{
			Id:              uuid.MustParse(info.ID),
			ModelName:       info.Name,
			ModelVersion:    int32(info.Version),
			CurrentState:    info.State,
			ModelUpdateDate: &now,
		})
	}

	common.WriteJSON(w, http.StatusOK, models)
}

// ValidateEntityModel handles POST /model/validate/{entityName}/{modelVersion}.
func (h *Handler) ValidateEntityModel(w http.ResponseWriter, r *http.Request, entityName string, modelVersion int32) {
	r.Body = http.MaxBytesReader(w, r.Body, 10*1024*1024)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		common.WriteError(w, r, common.Operational(http.StatusBadRequest, common.ErrCodeBadRequest, "failed to parse request body"))
		return
	}

	ver := strconv.Itoa(int(modelVersion))
	err = h.ValidateModel(r.Context(), entityName, ver, body)
	if err != nil {
		// ValidationError means validation failed but it's not an HTTP error.
		if ve, ok := err.(*ValidationError); ok {
			ref := modelRef(entityName, modelVersion)
			result := genapi.EntityModelActionResultDto{
				Success: false,
				Message: ve.Message,
				ModelId: deterministicID(ref),
				ModelKey: genapi.EntityModelKey{
					Name:    entityName,
					Version: modelVersion,
				},
			}
			common.WriteJSON(w, http.StatusOK, result)
			return
		}
		writeServiceError(w, r, err)
		return
	}

	result := successResult("Validation passed", entityName, modelVersion)
	common.WriteJSON(w, http.StatusOK, result)
}

// SetEntityModelUniqueKeys handles PUT /model/{entityName}/{modelVersion}/unique-keys.
func (h *Handler) SetEntityModelUniqueKeys(w http.ResponseWriter, r *http.Request, entityName string, modelVersion int32) {
	r.Body = http.MaxBytesReader(w, r.Body, 1*1024*1024)
	var req genapi.SetUniqueKeysRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		common.WriteError(w, r, common.Operational(http.StatusBadRequest, common.ErrCodeBadRequest, "failed to parse request body"))
		return
	}

	keys := make([]spi.UniqueKey, 0, len(req.UniqueKeys))
	for _, k := range req.UniqueKeys {
		keys = append(keys, spi.UniqueKey{
			ID:     k.Id,
			Fields: k.Fields,
		})
	}

	_, err := h.SetUniqueKeys(r.Context(), entityName, strconv.Itoa(int(modelVersion)), keys)
	if err != nil {
		writeServiceError(w, r, err)
		return
	}

	common.WriteJSON(w, http.StatusOK, successResult(
		fmt.Sprintf("unique keys set on model %s:%d", entityName, modelVersion),
		entityName, modelVersion))
}
