package postgres

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/jackc/pgx/v5"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// messageStore implements spi.MessageStore backed by PostgreSQL.
type messageStore struct {
	q        Querier
	tenantID spi.TenantID
}

func (s *messageStore) Save(ctx context.Context, id string, header spi.MessageHeader, metaData spi.MessageMetaData, payload io.Reader) error {
	headerJSON, err := json.Marshal(header)
	if err != nil {
		return fmt.Errorf("failed to marshal message header: %w", err)
	}

	// Ensure nil maps become empty objects in JSON for consistency.
	if metaData.Values == nil {
		metaData.Values = map[string]any{}
	}
	if metaData.IndexedValues == nil {
		metaData.IndexedValues = map[string]any{}
	}
	metaJSON, err := json.Marshal(metaData)
	if err != nil {
		return fmt.Errorf("failed to marshal message metadata: %w", err)
	}

	payloadBytes, err := io.ReadAll(payload)
	if err != nil {
		return fmt.Errorf("failed to read message payload: %w", err)
	}

	_, err = s.q.Exec(ctx,
		`INSERT INTO messages (tenant_id, message_id, header, metadata, payload)
		 VALUES ($1, $2, $3, $4, $5)
		 ON CONFLICT (tenant_id, message_id) DO UPDATE
		   SET header = EXCLUDED.header,
		       metadata = EXCLUDED.metadata,
		       payload = EXCLUDED.payload`,
		string(s.tenantID), id, headerJSON, metaJSON, payloadBytes)
	if err != nil {
		return fmt.Errorf("failed to save message %s: %w", id, err)
	}
	return nil
}

func (s *messageStore) Get(ctx context.Context, id string) (spi.MessageHeader, spi.MessageMetaData, io.ReadCloser, error) {
	var headerJSON, metaJSON []byte
	var payloadBytes []byte

	err := s.q.QueryRow(ctx,
		`SELECT header, metadata, payload FROM messages WHERE tenant_id = $1 AND message_id = $2`,
		string(s.tenantID), id).Scan(&headerJSON, &metaJSON, &payloadBytes)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return spi.MessageHeader{}, spi.MessageMetaData{}, nil,
				fmt.Errorf("message %s not found: %w", id, spi.ErrNotFound)
		}
		return spi.MessageHeader{}, spi.MessageMetaData{}, nil,
			fmt.Errorf("failed to get message %s: %w", id, err)
	}

	var header spi.MessageHeader
	if err := json.Unmarshal(headerJSON, &header); err != nil {
		return spi.MessageHeader{}, spi.MessageMetaData{}, nil,
			fmt.Errorf("failed to unmarshal message header: %w", err)
	}

	var metaData spi.MessageMetaData
	if err := json.Unmarshal(metaJSON, &metaData); err != nil {
		return spi.MessageHeader{}, spi.MessageMetaData{}, nil,
			fmt.Errorf("failed to unmarshal message metadata: %w", err)
	}

	return header, metaData, io.NopCloser(bytes.NewReader(payloadBytes)), nil
}

func (s *messageStore) Delete(ctx context.Context, id string) error {
	_, err := s.q.Exec(ctx,
		`DELETE FROM messages WHERE tenant_id = $1 AND message_id = $2`,
		string(s.tenantID), id)
	if err != nil {
		return fmt.Errorf("failed to delete message %s: %w", id, err)
	}
	return nil
}

func (s *messageStore) DeleteBatch(ctx context.Context, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	_, err := s.q.Exec(ctx,
		`DELETE FROM messages WHERE tenant_id = $1 AND message_id = ANY($2)`,
		string(s.tenantID), ids)
	if err != nil {
		return fmt.Errorf("failed to batch delete messages: %w", err)
	}
	return nil
}
