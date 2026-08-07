package grpc

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/google/uuid"

	cepb "github.com/cyoda-platform/cyoda-go/api/grpc/cloudevents"
	events "github.com/cyoda-platform/cyoda-go/api/grpc/events"
	"github.com/cyoda-platform/cyoda-go/internal/common"
)

const nilUUID = "00000000-0000-0000-0000-000000000000"

func strPtr(s string) *string { return &s }

// buildErrorFields extracts code, message, and retryable flag from an error.
// For operational AppErrors the client-safe message is returned directly.
// For internal/fatal AppErrors and raw errors a ticket UUID is generated and
// the detail is logged server-side only.
func buildErrorFields(err error) (code, message string, retryable *bool) {
	var appErr *common.AppError
	if errors.As(err, &appErr) {
		if appErr.Level == common.LevelOperational {
			code = "CLIENT_ERROR"
			message = appErr.Message // Already "CODE: detail"
			if appErr.Retryable {
				r := true
				retryable = &r
			}
			// An operational error whose cause is INFRASTRUCTURE rather than
			// domain (a storage failure, say) attaches that cause with
			// WithCause and keeps it out of the client-facing message. The log
			// is then its only breadcrumb — so it has to exist on this door
			// too, not just on the HTTP one. Same message and field name as
			// common.WriteError's operational branch, so the two entry points
			// read alike in a log aggregator. Errors without a cause put their
			// full detail in the message the client already has; logging those
			// again would be noise on every bad request.
			if appErr.Err != nil {
				slog.Info("operational error", "pkg", "grpc",
					"code", appErr.Code, "message", appErr.Message, "cause", appErr.Err.Error())
			}
			return
		}
		// Internal/Fatal — reuse the caller's pinned ticket when it has one
		// (it has already logged the detail under it; a second one would name
		// nothing), otherwise mint.
		ticket := appErr.Ticket
		if ticket == "" {
			ticket = uuid.New().String()
		}
		slog.Error("internal error", "ticket", ticket, "code", appErr.Code, "detail", appErr.Detail)
		code = "SERVER_ERROR"
		message = fmt.Sprintf("SERVER_ERROR: internal error [ticket: %s]", ticket)
		return
	}
	// A raw error carrying the storage layer's transient-unavailability marker is
	// NOT unclassified. Several service methods (the async-search family) return
	// the marker on a raw error rather than a pre-classified AppError; the HTTP
	// door recognises it via common.Internal and answers a retryable 503
	// STORAGE_UNAVAILABLE. Classifying it here too keeps one outage on one service
	// method from reading as "retry shortly" on one door and "your request is
	// hopeless" on the other. Re-entering through the operational branch also
	// keeps the cause in the log and out of the envelope, exactly as the HTTP door
	// does. Every one of this file's envelopes funnels through here, so this is
	// the only place the check belongs.
	if appErr := common.StorageUnavailable(err); appErr != nil {
		return buildErrorFields(appErr)
	}
	// Raw error — should not happen
	ticket := uuid.New().String()
	slog.Error("unclassified error", "ticket", ticket, "detail", err.Error())
	code = "SERVER_ERROR"
	message = fmt.Sprintf("SERVER_ERROR: internal error [ticket: %s]", ticket)
	return
}

// ctxWarnings returns accumulated diagnostics warnings from the context.
func ctxWarnings(ctx context.Context) []string {
	diag := common.GetDiagnostics(ctx)
	return diag.GetWarnings()
}

// entityTransactionError builds a schema-valid EntityTransactionResponse error.
func entityTransactionError(ctx context.Context, ceID string, err error) (*cepb.CloudEvent, error) {
	code, msg, retryable := buildErrorFields(err)
	resp := events.EntityTransactionResponseJson{
		ID:        ceID,
		Success:   false,
		Warnings:  ctxWarnings(ctx),
		RequestID: ceID,
		TransactionInfo: events.EntityTransactionInfoJson{
			EntityIds: []string{},
		},
		Error: &events.EntityTransactionResponseJsonError{
			Code:      code,
			Message:   msg,
			Retryable: retryable,
		},
	}
	return NewCloudEvent(EntityTransactionResponse, resp)
}

// entityDeleteError builds a schema-valid EntityDeleteResponse error.
func entityDeleteError(ctx context.Context, ceID, entityID string, err error) (*cepb.CloudEvent, error) {
	code, msg, retryable := buildErrorFields(err)
	resp := events.EntityDeleteResponseJson{
		ID:            ceID,
		Success:       false,
		Warnings:      ctxWarnings(ctx),
		RequestID:     ceID,
		EntityID:      entityID,
		TransactionID: nilUUID,
		Model:         events.ModelSpecJson{},
		Error: &events.EntityDeleteResponseJsonError{
			Code:      code,
			Message:   msg,
			Retryable: retryable,
		},
	}
	return NewCloudEvent(EntityDeleteResponse, resp)
}

// entityDeleteAllError builds a schema-valid EntityDeleteAllResponse error.
func entityDeleteAllError(ctx context.Context, ceID string, err error) (*cepb.CloudEvent, error) {
	code, msg, retryable := buildErrorFields(err)
	resp := events.EntityDeleteAllResponseJson{
		ID:        ceID,
		Success:   false,
		Warnings:  ctxWarnings(ctx),
		RequestID: ceID,
		EntityIds: []string{},
		Error: &events.EntityDeleteAllResponseJsonError{
			Code:      code,
			Message:   msg,
			Retryable: retryable,
		},
	}
	return NewCloudEvent(EntityDeleteAllResponse, resp)
}

// entityTransitionError builds a schema-valid EntityTransitionResponse error.
func entityTransitionError(ctx context.Context, ceID string, err error) (*cepb.CloudEvent, error) {
	code, msg, retryable := buildErrorFields(err)
	resp := events.EntityTransitionResponseJson{
		ID:       ceID,
		Success:  false,
		Warnings: ctxWarnings(ctx),
		Error: &events.EntityTransitionResponseJsonError{
			Code:      code,
			Message:   msg,
			Retryable: retryable,
		},
	}
	return NewCloudEvent(EntityTransitionResponse, resp)
}

// modelImportError builds a schema-valid EntityModelImportResponse error.
func modelImportError(ctx context.Context, ceID string, err error) (*cepb.CloudEvent, error) {
	code, msg, retryable := buildErrorFields(err)
	resp := events.EntityModelImportResponseJson{
		ID:       ceID,
		Success:  false,
		Warnings: ctxWarnings(ctx),
		ModelID:  nilUUID,
		Error: &events.EntityModelImportResponseJsonError{
			Code:      code,
			Message:   msg,
			Retryable: retryable,
		},
	}
	return NewCloudEvent(EntityModelImportResponse, resp)
}

// modelExportError builds a schema-valid EntityModelExportResponse error.
func modelExportError(ctx context.Context, ceID string, err error) (*cepb.CloudEvent, error) {
	code, msg, retryable := buildErrorFields(err)
	resp := events.EntityModelExportResponseJson{
		ID:       ceID,
		Success:  false,
		Warnings: ctxWarnings(ctx),
		ModelID:  nilUUID,
		Model:    events.ModelSpecJson{},
		Payload:  map[string]any{},
		Error: &events.EntityModelExportResponseJsonError{
			Code:      code,
			Message:   msg,
			Retryable: retryable,
		},
	}
	return NewCloudEvent(EntityModelExportResponse, resp)
}

// modelTransitionError builds a schema-valid EntityModelTransitionResponse error.
func modelTransitionError(ctx context.Context, ceID string, err error) (*cepb.CloudEvent, error) {
	code, msg, retryable := buildErrorFields(err)
	resp := events.EntityModelTransitionResponseJson{
		ID:       ceID,
		Success:  false,
		Warnings: ctxWarnings(ctx),
		ModelID:  nilUUID,
		State:    "",
		Error: &events.EntityModelTransitionResponseJsonError{
			Code:      code,
			Message:   msg,
			Retryable: retryable,
		},
	}
	return NewCloudEvent(EntityModelTransitionResponse, resp)
}

// modelDeleteError builds a schema-valid EntityModelDeleteResponse error.
func modelDeleteError(ctx context.Context, ceID string, err error) (*cepb.CloudEvent, error) {
	code, msg, retryable := buildErrorFields(err)
	resp := events.EntityModelDeleteResponseJson{
		ID:       ceID,
		Success:  false,
		Warnings: ctxWarnings(ctx),
		Error: &events.EntityModelDeleteResponseJsonError{
			Code:      code,
			Message:   msg,
			Retryable: retryable,
		},
	}
	return NewCloudEvent(EntityModelDeleteResponse, resp)
}

// modelSetUniqueKeysError builds a schema-valid set-unique-keys response error,
// reusing the EntityModelTransitionResponse envelope.
func modelSetUniqueKeysError(ctx context.Context, ceID string, err error) (*cepb.CloudEvent, error) {
	code, msg, retryable := buildErrorFields(err)
	resp := events.EntityModelTransitionResponseJson{
		ID:       ceID,
		Success:  false,
		Warnings: ctxWarnings(ctx),
		ModelID:  nilUUID,
		State:    "",
		Error: &events.EntityModelTransitionResponseJsonError{
			Code:      code,
			Message:   msg,
			Retryable: retryable,
		},
	}
	return NewCloudEvent(EntityModelSetUniqueKeysResponse, resp)
}

// modelGetAllError builds a schema-valid EntityModelGetAllResponse error.
func modelGetAllError(ctx context.Context, ceID string, err error) (*cepb.CloudEvent, error) {
	code, msg, retryable := buildErrorFields(err)
	resp := events.EntityModelGetAllResponseJson{
		ID:       ceID,
		Success:  false,
		Warnings: ctxWarnings(ctx),
		Models:   []events.ModelInfoJson{},
		Error: &events.EntityModelGetAllResponseJsonError{
			Code:      code,
			Message:   msg,
			Retryable: retryable,
		},
	}
	return NewCloudEvent(EntityModelGetAllResponse, resp)
}

// entityResponseError builds a schema-valid EntityResponse error.
func entityResponseError(ctx context.Context, ceID string, err error) (*cepb.CloudEvent, error) {
	code, msg, retryable := buildErrorFields(err)
	resp := events.EntityResponseJson{
		ID:        ceID,
		Success:   false,
		Warnings:  ctxWarnings(ctx),
		RequestID: ceID,
		Payload:   events.DataPayloadJson{Type: "JSON"},
		Error: &events.EntityResponseJsonError{
			Code:      code,
			Message:   msg,
			Retryable: retryable,
		},
	}
	return NewCloudEvent(EntityResponse, resp)
}

// snapshotSearchError builds a schema-valid EntitySnapshotSearchResponse error.
func snapshotSearchError(ctx context.Context, ceID string, err error) (*cepb.CloudEvent, error) {
	code, msg, retryable := buildErrorFields(err)
	resp := events.EntitySnapshotSearchResponseJson{
		ID:       ceID,
		Success:  false,
		Warnings: ctxWarnings(ctx),
		Status: events.SearchSnapshotStatusJson{
			SnapshotID: nilUUID,
			Status:     events.SearchSnapshotStatusJsonStatusFAILED,
		},
		Error: &events.EntitySnapshotSearchResponseJsonError{
			Code:      code,
			Message:   msg,
			Retryable: retryable,
		},
	}
	return NewCloudEvent(EntitySnapshotSearchResponse, resp)
}

// entityStatsError builds a schema-valid EntityStatsResponse error.
func entityStatsError(ctx context.Context, ceID string, err error) (*cepb.CloudEvent, error) {
	code, msg, retryable := buildErrorFields(err)
	resp := events.EntityStatsResponseJson{
		ID:        ceID,
		Success:   false,
		Warnings:  ctxWarnings(ctx),
		RequestID: ceID,
		Error: &events.EntityStatsResponseJsonError{
			Code:      code,
			Message:   msg,
			Retryable: retryable,
		},
	}
	return NewCloudEvent(EntityStatsResponse, resp)
}

// entityStatsByStateError builds a schema-valid EntityStatsByStateResponse error.
func entityStatsByStateError(ctx context.Context, ceID string, err error) (*cepb.CloudEvent, error) {
	code, msg, retryable := buildErrorFields(err)
	resp := events.EntityStatsByStateResponseJson{
		ID:        ceID,
		Success:   false,
		Warnings:  ctxWarnings(ctx),
		RequestID: ceID,
		Error: &events.EntityStatsByStateResponseJsonError{
			Code:      code,
			Message:   msg,
			Retryable: retryable,
		},
	}
	return NewCloudEvent(EntityStatsByStateResponse, resp)
}

// entityChangesMetadataError builds a schema-valid EntityChangesMetadataResponse error.
func entityChangesMetadataError(ctx context.Context, ceID string, err error) (*cepb.CloudEvent, error) {
	code, msg, retryable := buildErrorFields(err)
	resp := events.EntityChangesMetadataResponseJson{
		ID:         ceID,
		Success:    false,
		Warnings:   ctxWarnings(ctx),
		RequestID:  ceID,
		ChangeMeta: events.EntityChangeMetaJson{},
		Error: &events.EntityChangesMetadataResponseJsonError{
			Code:      code,
			Message:   msg,
			Retryable: retryable,
		},
	}
	return NewCloudEvent(EntityChangesMetadataResponse, resp)
}
