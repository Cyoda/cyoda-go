package entity

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/common"
)

// HandleGetTransitions returns available transitions for an entity at a given point in time.
// GET /entity/{entityId}/transitions?pointInTime=...&transactionId=...
func (h *Handler) HandleGetTransitions(w http.ResponseWriter, r *http.Request) {
	entityID := r.PathValue("entityId")
	if entityID == "" {
		common.WriteError(w, r, common.Operational(http.StatusBadRequest, common.ErrCodeBadRequest, "entityId is required"))
		return
	}

	pitStr := r.URL.Query().Get("pointInTime")
	txIDStr := r.URL.Query().Get("transactionId")

	if pitStr != "" && txIDStr != "" {
		common.WriteError(w, r, common.Operational(http.StatusBadRequest, common.ErrCodeBadRequest,
			"pointInTime and transactionId are mutually exclusive"))
		return
	}

	// A point in time is used only when the caller supplies one (directly or via
	// a transaction's submit time). With neither, this is a request for the
	// CURRENT state and must read the current version — NOT GetAsAt(time.Now()).
	// Version times are stamped by the backend (the database itself, on
	// postgres), so a process-clock "now" compared against them is a two-clock
	// comparison: a database clock running ahead of this process makes a
	// just-written version compare as not-yet-valid and the entity read as
	// missing. See internal/e2e/transitions_clockskew_test.go.
	var pointInTime time.Time
	var usePointInTime bool
	if txIDStr != "" {
		submitTime, err := h.txMgr.GetSubmitTime(r.Context(), txIDStr)
		if err != nil {
			common.WriteError(w, r, common.Operational(http.StatusBadRequest, common.ErrCodeBadRequest, err.Error()))
			return
		}
		pointInTime, usePointInTime = submitTime, true
	} else if pitStr != "" {
		parsed, err := time.Parse(time.RFC3339, pitStr)
		if err != nil {
			common.WriteError(w, r, common.Operational(http.StatusBadRequest, common.ErrCodeBadRequest, "invalid pointInTime format"))
			return
		}
		pointInTime, usePointInTime = parsed, true
	}

	// Load entity to get its modelRef.
	entityStore, err := h.factory.EntityStore(r.Context())
	if err != nil {
		common.WriteError(w, r, common.Internal("failed to access entity store", err))
		return
	}
	var entity *spi.Entity
	if usePointInTime {
		entity, err = entityStore.GetAsAt(r.Context(), entityID, pointInTime)
	} else {
		entity, err = entityStore.Get(r.Context(), entityID)
	}
	if err != nil {
		common.WriteError(w, r, common.Operational(http.StatusNotFound, common.ErrCodeEntityNotFound,
			fmt.Sprintf("entity %s not found", entityID)))
		return
	}

	transitions, err := h.engine.GetAvailableTransitionsForEntity(r.Context(), entity)
	if err != nil {
		common.WriteError(w, r, classifyError(err))
		return
	}

	common.WriteJSON(w, http.StatusOK, transitions)
}

// HandleFetchTransitions returns available transitions using the platform library format.
// GET /platform-api/entity/fetch/transitions?entityClass=Offer.1&entityId=<uuid>
func (h *Handler) HandleFetchTransitions(w http.ResponseWriter, r *http.Request) {
	entityClass := r.URL.Query().Get("entityClass")
	entityID := r.URL.Query().Get("entityId")

	if entityClass == "" || entityID == "" {
		common.WriteError(w, r, common.Operational(http.StatusBadRequest, common.ErrCodeBadRequest,
			"entityClass and entityId are required"))
		return
	}

	lastDot := strings.LastIndex(entityClass, ".")
	if lastDot < 0 || lastDot == len(entityClass)-1 {
		common.WriteError(w, r, common.Operational(http.StatusBadRequest, common.ErrCodeBadRequest,
			"entityClass must be in format Name.Version (e.g., Offer.1)"))
		return
	}
	entityName := entityClass[:lastDot]
	modelVersion := entityClass[lastDot+1:]

	modelRef := spi.ModelRef{EntityName: entityName, ModelVersion: modelVersion}

	transitions, err := h.engine.GetAvailableTransitions(r.Context(), entityID, modelRef)
	if err != nil {
		common.WriteError(w, r, classifyError(err))
		return
	}

	common.WriteJSON(w, http.StatusOK, transitions)
}
