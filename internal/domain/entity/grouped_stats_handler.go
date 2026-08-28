package entity

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go-spi/predicate"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/domain/model/schema"
	"github.com/cyoda-platform/cyoda-go/internal/domain/search"
)

// maxGroupedStatsBodySize bounds the request body for the grouped-stats
// endpoint at 10 MiB. Matches maxEntityBodySize and the cap applied by
// every other JSON-body POST handler in this package — keeps a uniform
// 413 ceiling on the public surface.
const maxGroupedStatsBodySize = 10 * 1024 * 1024

// StoreResolver returns the EntityStore (as any, since capability
// detection happens via type assertion inside the service), the resolved
// ModelRef, and the model's declared field-type map for the given entity
// name and model version. `fields` is used by the service to stamp declared
// types onto the pushdown filter and the streaming residual evaluator, so
// grouped-stats comparison is type-directed exactly like the search path; it
// may be nil when the model has no schema bound. The ok return is false when
// the model is not found for the calling tenant — the handler maps that to
// 404 MODEL_NOT_FOUND.
//
// modelStore is returned alongside so the handler can hold the condition,
// groupBy and aggregate paths to the model the way /search/direct and
// conditional delete do — including their bounded single RefreshAndGet, which
// is what stops a stale cached schema from falsely rejecting a field a peer
// node has already extended the model with (.claude/rules/multi-node-primary.md).
//
// The handler holds a StoreResolver rather than (factory, modelStore)
// directly so it can be unit-tested in isolation: tests inject a
// closure that returns the desired fake store + model. Production
// wiring at app construction supplies a closure that uses the existing
// StoreFactory + ModelStore plumbing (see app/app.go).
type StoreResolver func(r *http.Request, entityName, modelVersion string) (store any, model spi.ModelRef, fields map[string]schema.FieldDescriptor, modelStore spi.ModelStore, ok bool, err error)

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
			common.ErrCodeMalformedRequest,
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
			common.ErrCodeMalformedRequest,
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
			http.StatusBadRequest, common.ErrCodeMalformedRequest, err.Error(),
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
	store, model, fields, modelStore, ok, err := h.resolve(r, entityName, modelVersion)
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

	// Hold every path this request names to the model, the way /search/direct
	// and conditional DELETE do. Grouped stats used to check none of them, so
	// a condition, a groupBy or an aggregate naming a field the model does not
	// declare was answered rather than refused — and the numbers came back
	// looking like a real answer: an undeclared condition leaf annihilates to
	// a non-match, an undeclared groupBy buckets every entity under "absent",
	// and an undeclared SUM reports 0 as though it were the total.
	//
	// modelStore may be nil only from a test resolver that supplies none; a
	// request that reached here in production always carries one.
	if modelStore != nil {
		refreshed, pErr := search.ValidateKnownPaths(
			r.Context(), modelStore, model, requestFieldPaths(validated), fields)
		if pErr != nil {
			var appErr *common.AppError
			if !errors.As(pErr, &appErr) {
				appErr = common.Internal("field-path validation failed", pErr)
			}
			common.WriteError(w, r, appErr)
			return
		}
		fields = refreshed

		// Type-soundness against the REAL model. The service layer also calls
		// ValidateConditionValueTypes, but with a nil model, so only its
		// model-independent arm (meta fields, temporal operands) ran and an
		// operand parsing into none of a declared field's types was accepted
		// where /search/direct returns 400 CONDITION_TYPE_MISMATCH.
		if len(validated.Condition) > 0 {
			node, nErr := search.LoadModelNode(r.Context(), modelStore, model)
			if nErr != nil {
				common.WriteError(w, r, common.Internal("failed to load model schema for condition validation", nErr))
				return
			}
			if node != nil {
				if cond, pErr := predicate.ParseCondition(validated.Condition); pErr == nil {
					if tErr := search.ValidateConditionValueTypes(node, cond); tErr != nil {
						code := common.ErrCodeConditionTypeMismatch
						if errors.Is(tErr, search.ErrInvalidFieldPath) {
							code = common.ErrCodeInvalidFieldPath
						}
						common.WriteError(w, r, common.Operational(http.StatusBadRequest, code, tErr.Error()))
						return
					}
				}
			}
		}
	}

	// Dispatch to the service layer. QueryGroupedStats already classifies
	// the known domain/SPI sentinels into *common.AppError (transport-
	// symmetric translation site) — forward it as-is; anything else is an
	// unclassified storage/driver failure.
	buckets, err := h.svc.QueryGroupedStats(r.Context(), store, model, fields, validated)
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

// requestFieldPaths collects every data path a grouped-stats request names —
// the condition's leaves, the groupBy entries and the aggregate fields — as one
// set for [search.ValidateKnownPaths].
//
// The groupBy "state" entry addresses entity metadata, not a schema field, and
// is excluded; validateGroupedStatsRequest has already marked it IsState. Both
// remaining kinds are stored in wire form ("$.x"), which is the fields-map key
// convention, so they need no rewriting.
//
// A condition that will not parse contributes nothing rather than failing here:
// the service layer parses it too and produces the classified INVALID_CONDITION
// this handler would otherwise pre-empt with a worse diagnostic.
func requestFieldPaths(req *ValidatedGroupedStatsRequest) []string {
	seen := make(map[string]struct{})
	var out []string
	add := func(p string) {
		if p == "" {
			return
		}
		if _, dup := seen[p]; dup {
			return
		}
		seen[p] = struct{}{}
		out = append(out, p)
	}

	if len(req.Condition) > 0 {
		if cond, err := predicate.ParseCondition(req.Condition); err == nil {
			for _, p := range search.ConditionFieldPaths(cond) {
				add(p)
			}
		}
	}
	for _, g := range req.GroupBy {
		if !g.IsState {
			add(g.Path)
		}
	}
	for _, a := range req.Aggregations {
		add(a.Field)
	}
	return out
}
