package entity

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/common"
)

// maxGroupedStatsBodySize bounds the request body for the grouped-stats
// endpoint at 10 MiB. Matches maxEntityBodySize and the cap applied by
// every other JSON-body POST handler in this package — keeps a uniform
// 413 ceiling on the public surface.
const maxGroupedStatsBodySize = 10 * 1024 * 1024

// StoreResolver returns the EntityStore (as any, since capability
// detection happens via type assertion inside the service) and the
// resolved ModelRef for the given entity name and model version. The ok
// return is false when the model is not found for the calling tenant —
// the handler maps that to 404 MODEL_NOT_FOUND.
//
// The handler holds a StoreResolver rather than (factory, modelStore)
// directly so it can be unit-tested in isolation: tests inject a
// closure that returns the desired fake store + model. Production
// wiring at app construction supplies a closure that uses the existing
// StoreFactory + ModelStore plumbing (see app/app.go).
type StoreResolver func(r *http.Request, entityName, modelVersion string) (store any, model spi.ModelRef, ok bool, err error)

// GroupedStatsHandler is the HTTP handler for
// POST /api/entity/stats/{entityName}/{modelVersion}/query.
//
// Wiring is outside the OpenAPI-generated mux because the endpoint is
// new and not yet in api/openapi.yaml (cf. existing transition routes
// registered the same way in app/app.go).
type GroupedStatsHandler struct {
	resolve    StoreResolver
	svc        *GroupedStatsService
	maxBuckets int
}

// NewGroupedStatsHandler builds a handler. resolve may be nil for
// early-rejection tests that never exercise the dispatch path
// (body-size, JSON parse, validation) — in production the app always
// supplies a non-nil resolver.
func NewGroupedStatsHandler(resolve StoreResolver, maxBuckets int) *GroupedStatsHandler {
	return &GroupedStatsHandler{
		resolve:    resolve,
		svc:        NewGroupedStatsService(maxBuckets),
		maxBuckets: maxBuckets,
	}
}

// ServeHTTP implements http.Handler. Error responses use the
// common.WriteError problem+json shape so SDKs that already key on
// `properties.errorCode` continue to work uniformly across the entity
// surface.
func (h *GroupedStatsHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// 413 — body too large. We read into memory so the JSON decoder
	// gets a contiguous buffer; 10 MiB is consistent with every other
	// entity-domain POST cap.
	r.Body = http.MaxBytesReader(w, r.Body, maxGroupedStatsBodySize)
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			common.WriteError(w, r, common.Operational(
				http.StatusRequestEntityTooLarge,
				common.ErrCodeBadRequest,
				"request body exceeds 10 MiB",
			))
			return
		}
		common.WriteError(w, r, common.Operational(
			http.StatusBadRequest,
			"MALFORMED_REQUEST",
			"failed to read request body",
		))
		return
	}

	// 400 — malformed JSON. DisallowUnknownFields keeps us strict: a
	// typo'd `agregations` field rejects with 400 rather than silently
	// running with zero aggregations and surprising the client.
	var req GroupedStatsRequest
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		common.WriteError(w, r, common.Operational(
			http.StatusBadRequest,
			"MALFORMED_REQUEST",
			"invalid JSON: "+err.Error(),
		))
		return
	}

	// 400 — validation (spec §3 error codes propagated 1:1 from the
	// validation layer).
	validated, err := ValidateGroupedStatsRequest(req, h.maxBuckets)
	if err != nil {
		var ve *GroupedStatsValidationError
		if errors.As(err, &ve) {
			common.WriteError(w, r, common.Operational(
				http.StatusBadRequest, ve.Code, ve.Message,
			))
			return
		}
		// Defensive: ValidateGroupedStatsRequest only ever returns
		// *GroupedStatsValidationError on the 4xx path. Anything else
		// is a programming error — report as 400 MALFORMED_REQUEST so
		// the client can still inspect the message.
		common.WriteError(w, r, common.Operational(
			http.StatusBadRequest, "MALFORMED_REQUEST", err.Error(),
		))
		return
	}

	// Resolve model + store. Without a resolver this is an
	// early-rejection test path — the validation has succeeded but we
	// have no backend; surface 500 so a misconfigured production code
	// path (resolver=nil) is loud rather than silent.
	if h.resolve == nil {
		common.WriteError(w, r, common.Internal("store resolver not configured", nil))
		return
	}
	entityName := r.PathValue("entityName")
	modelVersion := r.PathValue("modelVersion")
	store, model, ok, err := h.resolve(r, entityName, modelVersion)
	if err != nil {
		common.WriteError(w, r, common.Internal("failed to resolve store", err))
		return
	}
	if !ok {
		common.WriteError(w, r, common.Operational(
			http.StatusNotFound, common.ErrCodeModelNotFound,
			"model not found",
		))
		return
	}

	// Dispatch to the service layer. QueryGroupedStats already classifies
	// the known domain/SPI sentinels into *common.AppError (transport-
	// symmetric translation site) — forward it as-is; anything else is an
	// unclassified storage/driver failure.
	buckets, err := h.svc.QueryGroupedStats(r.Context(), store, model, validated)
	if err != nil {
		var appErr *common.AppError
		if errors.As(err, &appErr) {
			common.WriteError(w, r, appErr)
			return
		}
		common.WriteError(w, r, common.Internal("grouped-stats dispatch failed", err))
		return
	}

	common.WriteJSON(w, http.StatusOK, buckets)
}
