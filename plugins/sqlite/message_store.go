package sqlite

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

type messageStore struct {
	db       *sql.DB
	tenantID spi.TenantID
}

func (s *messageStore) Save(ctx context.Context, id string, header spi.MessageHeader, metaData spi.MessageMetaData, payload io.Reader) error {
	headerJSON, err := json.Marshal(header)
	if err != nil {
		return fmt.Errorf("failed to marshal message header: %w", err)
	}

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

	_, err = s.db.ExecContext(ctx,
		`INSERT OR REPLACE INTO messages (tenant_id, message_id, header, metadata, payload)
		 VALUES (?, ?, jsonb(?), jsonb(?), ?)`,
		string(s.tenantID), id, headerJSON, metaJSON, payloadBytes)
	if err != nil {
		return fmt.Errorf("failed to save message %s: %w", id, err)
	}
	return nil
}

func (s *messageStore) Get(ctx context.Context, id string) (spi.MessageHeader, spi.MessageMetaData, io.ReadCloser, error) {
	var headerJSON, metaJSON []byte
	var payloadBytes []byte

	err := s.db.QueryRowContext(ctx,
		`SELECT json(header), json(metadata), payload FROM messages WHERE tenant_id = ? AND message_id = ?`,
		string(s.tenantID), id).Scan(&headerJSON, &metaJSON, &payloadBytes)
	if err != nil {
		if err == sql.ErrNoRows {
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
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM messages WHERE tenant_id = ? AND message_id = ?`,
		string(s.tenantID), id)
	if err != nil {
		return fmt.Errorf("failed to delete message %s: %w", id, err)
	}
	return nil
}

// deleteBatchChunkSize bounds the parameterized IN list per statement, well
// under SQLite's bound-variable limit (32766 in this wasm build). Message
// delete is documented non-transactional, so statement chunking changes
// nothing observable.
const deleteBatchChunkSize = 500

func (s *messageStore) DeleteBatch(ctx context.Context, ids []string) error {
	// SQLite has no ANY($1) — build a parameterized IN clause per chunk.
	for start := 0; start < len(ids); start += deleteBatchChunkSize {
		end := min(start+deleteBatchChunkSize, len(ids))
		chunk := ids[start:end]
		placeholders := make([]string, len(chunk))
		args := make([]any, 0, len(chunk)+1)
		args = append(args, string(s.tenantID))
		for i, id := range chunk {
			placeholders[i] = "?"
			args = append(args, id)
		}
		query := fmt.Sprintf(`DELETE FROM messages WHERE tenant_id = ? AND message_id IN (%s)`,
			strings.Join(placeholders, ", "))
		if _, err := s.db.ExecContext(ctx, query, args...); err != nil {
			return fmt.Errorf("failed to batch delete messages: %w", err)
		}
	}
	return nil
}
