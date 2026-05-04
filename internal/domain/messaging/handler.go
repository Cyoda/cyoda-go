package messaging

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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

	rawBody, err := io.ReadAll(r.Body)
	if err != nil {
		if err.Error() == "http: request body too large" {
			common.WriteError(w, r, common.Operational(http.StatusRequestEntityTooLarge, common.ErrCodeBadRequest, "request payload exceeds maximum allowed limit of 10MB"))
			return
		}
		common.WriteError(w, r, common.Internal("failed to read request body", err))
		return
	}

	var envelope struct {
		Payload  json.RawMessage `json:"payload"`
		MetaData map[string]any  `json:"meta-data"`
	}
	if err := json.Unmarshal(rawBody, &envelope); err != nil {
		common.WriteError(w, r, common.Operational(http.StatusBadRequest, common.ErrCodeBadRequest, fmt.Sprintf("invalid JSON: %v", err)))
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

	store, err := h.factory.MessageStore(r.Context())
	if err != nil {
		common.WriteError(w, r, common.Internal("failed to get message store", err))
		return
	}

	if err := store.Save(r.Context(), id.String(), header, metaData, strings.NewReader(payloadString)); err != nil {
		common.WriteError(w, r, common.Internal("failed to save message", err))
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
func (h *Handler) GetMessage(w http.ResponseWriter, r *http.Request, messageId string) {
	store, err := h.factory.MessageStore(r.Context())
	if err != nil {
		common.WriteError(w, r, common.Internal("failed to get message store", err))
		return
	}

	header, metaData, payloadReader, err := store.Get(r.Context(), messageId)
	if err != nil {
		if errors.Is(err, spi.ErrNotFound) {
			appErr := common.Operational(http.StatusNotFound, common.ErrCodeEntityNotFound, fmt.Sprintf("message id=%s not found", messageId))
			appErr.Props = map[string]any{"messageId": messageId}
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

	valuesMap := map[string]any{"typeReferences": map[string]any{}}
	if metaData.Values != nil {
		for k, v := range metaData.Values {
			valuesMap[k] = v
		}
	}

	indexedMap := map[string]any{"typeReferences": map[string]any{}}
	if metaData.IndexedValues != nil {
		for k, v := range metaData.IndexedValues {
			indexedMap[k] = v
		}
	}

	resp := map[string]any{
		"header": respHeader,
		"metaData": map[string]any{
			"values":        valuesMap,
			"indexedValues": indexedMap,
		},
		// json.RawMessage embeds the payload as-is in the JSON output instead of
		// wrapping the bytes in a JSON string. This fixes the #21 JSON-in-string defect.
		"content": json.RawMessage(payloadBytes),
	}

	common.WriteJSON(w, http.StatusOK, resp)
}

// DeleteMessage deletes a single edge message by ID.
func (h *Handler) DeleteMessage(w http.ResponseWriter, r *http.Request, messageId string) {
	store, err := h.factory.MessageStore(r.Context())
	if err != nil {
		common.WriteError(w, r, common.Internal("failed to get message store", err))
		return
	}

	// Check existence by trying to get first
	_, _, rc, err := store.Get(r.Context(), messageId)
	if err != nil {
		if errors.Is(err, spi.ErrNotFound) {
			appErr := common.Operational(http.StatusNotFound, common.ErrCodeEntityNotFound, fmt.Sprintf("message id=%s not found", messageId))
			appErr.Props = map[string]any{"messageId": messageId}
			common.WriteError(w, r, appErr)
			return
		}
		common.WriteError(w, r, common.Internal("failed to check message", err))
		return
	}
	rc.Close()

	if err := store.Delete(r.Context(), messageId); err != nil {
		common.WriteError(w, r, common.Internal("failed to delete message", err))
		return
	}

	common.WriteJSON(w, http.StatusOK, map[string]any{
		"entityIds": []string{messageId},
	})
}

// DeleteMessages deletes multiple edge messages by ID.
func (h *Handler) DeleteMessages(w http.ResponseWriter, r *http.Request, params genapi.DeleteMessagesParams) {
	r.Body = http.MaxBytesReader(w, r.Body, 10*1024*1024)

	rawBody, err := io.ReadAll(r.Body)
	if err != nil {
		common.WriteError(w, r, common.Internal("failed to read request body", err))
		return
	}

	var ids []string
	if err := json.Unmarshal(rawBody, &ids); err != nil {
		common.WriteError(w, r, common.Operational(http.StatusBadRequest, common.ErrCodeBadRequest, fmt.Sprintf("invalid JSON: expected array of UUID strings: %v", err)))
		return
	}

	store, err := h.factory.MessageStore(r.Context())
	if err != nil {
		common.WriteError(w, r, common.Internal("failed to get message store", err))
		return
	}

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
}

func derefStr(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
