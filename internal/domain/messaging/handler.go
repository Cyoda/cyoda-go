package messaging

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/google/uuid"

	spi "github.com/cyoda-platform/cyoda-go-spi"

	genapi "github.com/cyoda-platform/cyoda-go/api"
	"github.com/cyoda-platform/cyoda-go/internal/common"
)

// Handler implements the edge messaging endpoints.
type Handler struct {
	factory spi.StoreFactory
	uuids   spi.UUIDGenerator
}

// New creates a Handler with the given StoreFactory and UUIDGenerator.
func New(factory spi.StoreFactory, uuids spi.UUIDGenerator) *Handler {
	return &Handler{factory: factory, uuids: uuids}
}

// NewMessage creates and stores a new edge message.
func (h *Handler) NewMessage(w http.ResponseWriter, r *http.Request, subject string, params genapi.NewMessageParams) {
	r.Body = http.MaxBytesReader(w, r.Body, 10*1024*1024)

	// Messaging has no transaction and no joined path of its own, but the
	// txjoin middleware is global — a routed compute-node callback can still
	// present a joined tx on ctx here, so the reject-on-joined rule (spec D7)
	// applies uniformly even though this handler never begins a tx.
	opCtx := r.Context()
	if params.TransactionTimeoutMillis != nil {
		if appErr := common.ValidateRequestTimeoutMillis(*params.TransactionTimeoutMillis); appErr != nil {
			common.WriteError(w, r, appErr)
			return
		}
		if spi.GetTransaction(opCtx) != nil {
			common.WriteError(w, r, common.Operational(http.StatusBadRequest, common.ErrCodeBadRequest,
				"transactionTimeoutMillis is not supported on a request that joins an open transaction"))
			return
		}
		var cancel context.CancelFunc
		opCtx, cancel = common.WithRequestTimeout(opCtx, *params.TransactionTimeoutMillis)
		defer cancel()
	}

	rawBody, err := io.ReadAll(r.Body)
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			common.WriteError(w, r, common.Operational(http.StatusRequestEntityTooLarge, common.ErrCodeBadRequest, "request payload exceeds maximum allowed limit of 10MB"))
			return
		}
		common.WriteError(w, r, common.Internal("failed to read request body", err))
		return
	}

	var envelope struct {
		Payload  json.RawMessage `json:"payload"`
		MetaData map[string]any  `json:"metaData"`
	}
	if err := json.Unmarshal(rawBody, &envelope); err != nil {
		common.WriteError(w, r, common.Operational(http.StatusBadRequest, common.ErrCodeBadRequest, "invalid JSON: expected an object with a 'payload' field"))
		return
	}

	if envelope.Payload == nil {
		common.WriteError(w, r, common.Operational(http.StatusBadRequest, common.ErrCodeBadRequest, "missing required field: payload"))
		return
	}

	// Compact the payload JSON to normalize whitespace (matches Cyoda Cloud behavior).
	// json.Unmarshal above already validated envelope.Payload as a JSON value, so
	// json.Compact must succeed. If it fails, that is an invariant violation caused
	// by a future code path constructing Payload by hand instead of via Unmarshal.
	var compacted bytes.Buffer
	if err := json.Compact(&compacted, envelope.Payload); err != nil {
		common.WriteError(w, r, common.Internal("payload validation invariant broken", err))
		return
	}
	payloadString := compacted.String()

	header := spi.MessageHeader{
		Subject:         subject,
		ContentType:     params.ContentType,
		ContentLength:   params.ContentLength,
		ContentEncoding: "UTF-8",
		MessageID:       derefStr(params.XMessageID),
		UserID:          derefStr(params.XUserID),
		Recipient:       derefStr(params.XRecipient),
		ReplyTo:         derefStr(params.XReplyTo),
		CorrelationID:   derefStr(params.XCorrelationID),
	}
	if params.ContentEncoding != nil {
		header.ContentEncoding = *params.ContentEncoding
	}

	metaData := spi.MessageMetaData{
		Values:        make(map[string]any),
		IndexedValues: make(map[string]any),
	}
	for k, v := range envelope.MetaData {
		metaData.IndexedValues[k] = v
	}

	id := uuid.UUID(h.uuids.NewTimeUUID())
	txID := uuid.UUID(h.uuids.NewTimeUUID())

	store, err := h.factory.MessageStore(opCtx)
	if err != nil {
		if appErr := common.ClassifyRequestTimeout(opCtx, err, common.ErrCodeTransactionTimeout); appErr != nil {
			common.WriteError(w, r, appErr)
			return
		}
		common.WriteError(w, r, common.Internal("failed to get message store", err))
		return
	}

	// Spec D2: the Save is this path's commit boundary — pre-check then shield,
	// so a 408 can only mean the save never began (save-wins). The pre-check's
	// ClassifyRequestTimeout returns nil for a bare context.Canceled (only
	// DeadlineExceeded qualifies for 408); that case falls back to a 500 —
	// the client disconnecting is not "nothing was committed by design", it's
	// an aborted request.
	if err := opCtx.Err(); err != nil {
		if appErr := common.ClassifyRequestTimeout(opCtx, err, common.ErrCodeTransactionTimeout); appErr != nil {
			common.WriteError(w, r, appErr)
			return
		}
		common.WriteError(w, r, common.Internal("request cancelled before save", err))
		return
	}
	saveErr := common.ShieldedCommit(opCtx, func(saveCtx context.Context) error {
		return store.Save(saveCtx, id.String(), header, metaData, strings.NewReader(payloadString))
	})
	if saveErr != nil {
		common.WriteError(w, r, common.Internal("failed to save message", saveErr))
		return
	}

	resp := []map[string]any{
		{
			"entityIds":     []string{id.String()},
			"transactionId": txID.String(),
		},
	}
	common.WriteJSON(w, http.StatusOK, resp)
}

// GetMessage retrieves an edge message by ID.
func (h *Handler) GetMessage(w http.ResponseWriter, r *http.Request, messageId uuid.UUID) {
	store, err := h.factory.MessageStore(r.Context())
	if err != nil {
		common.WriteError(w, r, common.Internal("failed to get message store", err))
		return
	}

	msgIDStr := messageId.String()
	header, metaData, payloadReader, err := store.Get(r.Context(), msgIDStr)
	if err != nil {
		if errors.Is(err, spi.ErrNotFound) {
			appErr := common.Operational(http.StatusNotFound, common.ErrCodeEntityNotFound, fmt.Sprintf("message id=%s not found", msgIDStr))
			appErr.Props = map[string]any{"messageId": msgIDStr}
			common.WriteError(w, r, appErr)
			return
		}
		common.WriteError(w, r, common.Internal("failed to get message", err))
		return
	}
	defer payloadReader.Close()

	payloadBytes, err := io.ReadAll(payloadReader)
	if err != nil {
		common.WriteError(w, r, common.Internal("failed to read message payload", err))
		return
	}

	respHeader := map[string]any{
		"subject":         header.Subject,
		"contentType":     header.ContentType,
		"contentLength":   header.ContentLength,
		"contentEncoding": header.ContentEncoding,
	}
	if header.MessageID != "" {
		respHeader["messageId"] = header.MessageID
	}
	if header.UserID != "" {
		respHeader["userId"] = header.UserID
	}
	if header.Recipient != "" {
		respHeader["recipient"] = header.Recipient
	}
	if header.ReplyTo != "" {
		respHeader["replyTo"] = header.ReplyTo
	}
	if header.CorrelationID != "" {
		respHeader["correlationId"] = header.CorrelationID
	}

	// Flat metadata map — symmetric with the submitted `metaData`. The
	// values/indexedValues split and the injected typeReferences were
	// cyoda-cloud indexing artifacts, not part of the cyoda-go contract.
	metaMap := map[string]any{}
	// IndexedValues is merged last, so it wins on a key collision — which is
	// currently impossible: cyoda-go routes all client metaData to IndexedValues,
	// leaving Values empty.
	for k, v := range metaData.Values {
		metaMap[k] = v
	}
	for k, v := range metaData.IndexedValues {
		metaMap[k] = v
	}

	resp := map[string]any{
		"header":   respHeader,
		"metaData": metaMap,
		// json.RawMessage embeds the payload as-is (fixes the JSON-in-string defect).
		"content": json.RawMessage(payloadBytes),
	}

	common.WriteJSON(w, http.StatusOK, resp)
}

// DeleteMessage deletes a single edge message by ID.
func (h *Handler) DeleteMessage(w http.ResponseWriter, r *http.Request, messageId uuid.UUID) {
	store, err := h.factory.MessageStore(r.Context())
	if err != nil {
		common.WriteError(w, r, common.Internal("failed to get message store", err))
		return
	}

	msgIDStr := messageId.String()
	// Check existence by trying to get first
	_, _, rc, err := store.Get(r.Context(), msgIDStr)
	if err != nil {
		if errors.Is(err, spi.ErrNotFound) {
			appErr := common.Operational(http.StatusNotFound, common.ErrCodeEntityNotFound, fmt.Sprintf("message id=%s not found", msgIDStr))
			appErr.Props = map[string]any{"messageId": msgIDStr}
			common.WriteError(w, r, appErr)
			return
		}
		common.WriteError(w, r, common.Internal("failed to check message", err))
		return
	}
	rc.Close()

	if err := store.Delete(r.Context(), msgIDStr); err != nil {
		common.WriteError(w, r, common.Internal("failed to delete message", err))
		return
	}

	common.WriteJSON(w, http.StatusOK, map[string]any{
		"entityIds": []string{msgIDStr},
	})
}

// DeleteMessages deletes multiple edge messages by ID.
func (h *Handler) DeleteMessages(w http.ResponseWriter, r *http.Request, params genapi.DeleteMessagesParams) {
	r.Body = http.MaxBytesReader(w, r.Body, 10*1024*1024)

	rawBody, err := io.ReadAll(r.Body)
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			common.WriteError(w, r, common.Operational(http.StatusRequestEntityTooLarge, common.ErrCodeBadRequest, "request payload exceeds maximum allowed limit of 10MB"))
			return
		}
		common.WriteError(w, r, common.Internal("failed to read request body", err))
		return
	}

	var ids []string
	if err := json.Unmarshal(rawBody, &ids); err != nil {
		common.WriteError(w, r, common.Operational(http.StatusBadRequest, common.ErrCodeBadRequest, "invalid JSON: expected array of UUID strings"))
		return
	}

	for _, id := range ids {
		if _, err := uuid.Parse(id); err != nil {
			common.WriteError(w, r, common.Operational(http.StatusBadRequest, common.ErrCodeBadRequest, "id list contains a value that is not a valid UUID"))
			return
		}
	}

	// Messaging has no transaction and no joined path of its own, but the
	// txjoin middleware is global — a routed compute-node callback can still
	// present a joined tx on ctx here, so the reject-on-joined rule (spec D7)
	// applies uniformly even though this handler never begins a tx.
	batchSize := 0
	if params.TransactionSize != nil {
		if *params.TransactionSize < 1 {
			common.WriteError(w, r, common.Operational(http.StatusBadRequest, common.ErrCodeBadRequest,
				"transactionSize must be a positive integer"))
			return
		}
		if spi.GetTransaction(r.Context()) != nil {
			common.WriteError(w, r, common.Operational(http.StatusBadRequest, common.ErrCodeBadRequest,
				"transactionSize is not supported on a request that joins an open transaction"))
			return
		}
		batchSize = int(*params.TransactionSize)
	}

	store, err := h.factory.MessageStore(r.Context())
	if err != nil {
		common.WriteError(w, r, common.Internal("failed to get message store", err))
		return
	}

	if batchSize == 0 {
		if err := store.DeleteBatch(r.Context(), ids); err != nil {
			common.WriteError(w, r, common.Internal("failed to delete messages", err))
			return
		}

		resp := []map[string]any{
			{
				"entityIds": ids,
				"success":   true,
			},
		}
		common.WriteJSON(w, http.StatusOK, resp)
		return
	}

	// Spec D4: one store call per chunk of <=batchSize ids, one response
	// element per chunk; a failed chunk reports success:false but does not
	// stop later chunks from being attempted.
	resp := make([]map[string]any, 0, (len(ids)+batchSize-1)/batchSize)
	for start := 0; start < len(ids); start += batchSize {
		end := min(start+batchSize, len(ids))
		chunk := ids[start:end]
		err := store.DeleteBatch(r.Context(), chunk)
		if err != nil {
			slog.Warn("message delete batch failed", "pkg", "messaging", "batchStart", start, "err", err)
		}
		resp = append(resp, map[string]any{"entityIds": chunk, "success": err == nil})
	}
	common.WriteJSON(w, http.StatusOK, resp)
}

func derefStr(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
