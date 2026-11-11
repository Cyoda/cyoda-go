package grpc

import (
	"context"
	"errors"
	"strings"

	cepb "github.com/cyoda-platform/cyoda-go/api/grpc/cloudevents"
	events "github.com/cyoda-platform/cyoda-go/api/grpc/events"
	"github.com/cyoda-platform/cyoda-go/internal/common"
)

const nilUUID = "00000000-0000-0000-0000-000000000000"

// requestEventKey carries the gRPC request an error envelope answers, so the
// funnel's one log line for a failure names it.
type requestEventKey struct{}

type requestEvent struct{ rpc, eventType, ceID string }

// withRequestEvent records, at an RPC's entry, which request the work under
// ctx answers.
func withRequestEvent(ctx context.Context, rpc, eventType, ceID string) context.Context {
	return context.WithValue(ctx, requestEventKey{}, requestEvent{rpc: rpc, eventType: eventType, ceID: ceID})
}

// withRequestMessage records the request message's type and id under the RPC
// already recorded on ctx, for a path that reads the message after its entry.
func withRequestMessage(ctx context.Context, ce *cepb.CloudEvent) context.Context {
	ev, _ := ctx.Value(requestEventKey{}).(requestEvent)
	return withRequestEvent(ctx, ev.rpc, ce.Type, ce.Id)
}

// rpcName is the log name of a CloudEventsService method — its last path
// segment with a lower-case first letter, as the RPC entries name it
// ("/…CloudEventsService/EntityManage" → "entityManage").
func rpcName(fullMethod string) string {
	name := fullMethod[strings.LastIndex(fullMethod, "/")+1:]
	if name == "" {
		return name
	}
	return strings.ToLower(name[:1]) + name[1:]
}

// requestAttrs are the log fields naming the request ctx answers.
func requestAttrs(ctx context.Context) []any {
	attrs := []any{"pkg", "grpc"}
	if ev, ok := ctx.Value(requestEventKey{}).(requestEvent); ok {
		attrs = append(attrs, "rpc", ev.rpc, "type", ev.eventType, "ceId", ev.ceID)
	}
	return attrs
}

// buildErrorFields extracts code, message, and retryable flag from an error,
// and logs the failure once through the funnel both doors share
// (common.LogError), naming the request it answers (requestAttrs). An
// operational AppError answers its client-safe message, logged at its level
// (INFO unless the refusing site set another); an internal or fatal one, or a
// raw error that does not classify as operational, answers the generic text
// with the ticket its detail is logged under at ERROR — unless the cause is
// marked as the client having gone away mid-request (common.ErrClientGone),
// which is logged at DEBUG with no ticket. A cancellation on the chain alone
// does not mark it: a joined request's context is detached once it holds the
// transaction, so a ctx.Canceled may be unrelated to this, still-live
// request, and stays a ticketed fault.
func buildErrorFields(ctx context.Context, err error) (code, message string, retryable *bool) {
	var appErr *common.AppError
	if !errors.As(err, &appErr) {
		// A raw error carrying a store sentinel or marker — a transient
		// storage outage, a lost write race, a unique-key refusal, a lost
		// shard — is NOT unclassified. Several service methods return one on
		// a raw error rather than a pre-classified AppError, and the HTTP door
		// classifies it through common.Internal. Classifying it here the same
		// way keeps one failure from reading as "retry shortly" on one door
		// and "your request is hopeless" on the other; anything else is a
		// server-side failure, and a departed client stays a departed client
		// (common.Internal keeps the cause, which carries the marker).
		appErr = common.Internal("unclassified error", err)
	}
	common.RecordOutcome(ctx, appErr)
	// The one log line, as the HTTP door writes it, naming the request.
	message = common.SanitizedText(appErr, common.LogError(ctx, appErr, requestAttrs(ctx)...))
	if appErr.Level == common.LevelOperational {
		if appErr.Retryable {
			r := true
			retryable = &r
		}
		return "CLIENT_ERROR", message, retryable
	}
	return "SERVER_ERROR", message, nil
}

// ctxWarnings returns accumulated diagnostics warnings from the context.
func ctxWarnings(ctx context.Context) []string {
	diag := common.GetDiagnostics(ctx)
	return diag.GetWarnings()
}

// entityTransactionError builds a schema-valid EntityTransactionResponse error.
func entityTransactionError(ctx context.Context, ceID string, err error) (*cepb.CloudEvent, error) {
	code, msg, retryable := buildErrorFields(ctx, err)
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
	code, msg, retryable := buildErrorFields(ctx, err)
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
	code, msg, retryable := buildErrorFields(ctx, err)
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
	code, msg, retryable := buildErrorFields(ctx, err)
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
	code, msg, retryable := buildErrorFields(ctx, err)
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
	code, msg, retryable := buildErrorFields(ctx, err)
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
	code, msg, retryable := buildErrorFields(ctx, err)
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
	code, msg, retryable := buildErrorFields(ctx, err)
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

// modelSetUniqueKeysError builds a schema-valid EntityModelSetUniqueKeysResponse error.
func modelSetUniqueKeysError(ctx context.Context, ceID string, err error) (*cepb.CloudEvent, error) {
	code, msg, retryable := buildErrorFields(ctx, err)
	resp := events.EntityModelSetUniqueKeysResponseJson{
		ID:       ceID,
		Success:  false,
		Warnings: ctxWarnings(ctx),
		ModelID:  nilUUID,
		State:    "",
		Error: &events.EntityModelSetUniqueKeysResponseJsonError{
			Code:      code,
			Message:   msg,
			Retryable: retryable,
		},
	}
	return NewCloudEvent(EntityModelSetUniqueKeysResponse, resp)
}

// modelGetAllError builds a schema-valid EntityModelGetAllResponse error.
func modelGetAllError(ctx context.Context, ceID string, err error) (*cepb.CloudEvent, error) {
	code, msg, retryable := buildErrorFields(ctx, err)
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
	code, msg, retryable := buildErrorFields(ctx, err)
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
	code, msg, retryable := buildErrorFields(ctx, err)
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

// snapshotCancelRefused builds the answer to a cancel of a snapshot that has
// already ended: the error envelope, carrying the snapshot's own status in
// place of the generic FAILED.
func snapshotCancelRefused(ctx context.Context, ceID string, refusal error, st events.SearchSnapshotStatusJson) (*cepb.CloudEvent, error) {
	code, msg, retryable := buildErrorFields(ctx, refusal)
	resp := events.EntitySnapshotSearchResponseJson{
		ID:       ceID,
		Success:  false,
		Warnings: ctxWarnings(ctx),
		Status:   st,
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
	code, msg, retryable := buildErrorFields(ctx, err)
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

// consistencyTimeError builds a schema-valid EntityConsistencyTimeResponse error.
func consistencyTimeError(ctx context.Context, ceID string, err error) (*cepb.CloudEvent, error) {
	code, msg, retryable := buildErrorFields(ctx, err)
	resp := events.EntityConsistencyTimeResponseJson{
		ID:        ceID,
		Success:   false,
		Warnings:  ctxWarnings(ctx),
		RequestID: ceID,
		Error: &events.EntityConsistencyTimeResponseJsonError{
			Code:      code,
			Message:   msg,
			Retryable: retryable,
		},
	}
	return NewCloudEvent(EntityConsistencyTimeResponse, resp)
}

// entityStatsByStateError builds a schema-valid EntityStatsByStateResponse error.
func entityStatsByStateError(ctx context.Context, ceID string, err error) (*cepb.CloudEvent, error) {
	code, msg, retryable := buildErrorFields(ctx, err)
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
	code, msg, retryable := buildErrorFields(ctx, err)
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
