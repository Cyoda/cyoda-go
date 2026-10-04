package sqlite

import (
	"context"
	"database/sql"
	"fmt"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

type kvStore struct {
	db       *sql.DB
	tenantID spi.TenantID
}

func (s *kvStore) Put(ctx context.Context, namespace string, key string, value []byte) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT OR REPLACE INTO kv_store (tenant_id, namespace, key, value) VALUES (?, ?, ?, ?)`,
		string(s.tenantID), namespace, key, nonNil(value))
	if err != nil {
		return fmt.Errorf("failed to put key %s/%s: %w", namespace, key, err)
	}
	return nil
}

func (s *kvStore) Get(ctx context.Context, namespace string, key string) ([]byte, error) {
	var value []byte
	err := s.db.QueryRowContext(ctx,
		`SELECT value FROM kv_store WHERE tenant_id = ? AND namespace = ? AND key = ?`,
		string(s.tenantID), namespace, key).Scan(&value)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, fmt.Errorf("key %s not found in namespace %s: %w", key, namespace, spi.ErrNotFound)
		}
		return nil, fmt.Errorf("failed to get key %s/%s: %w", namespace, key, err)
	}
	return value, nil
}

func (s *kvStore) Delete(ctx context.Context, namespace string, key string) error {
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM kv_store WHERE tenant_id = ? AND namespace = ? AND key = ?`,
		string(s.tenantID), namespace, key)
	if err != nil {
		return fmt.Errorf("failed to delete key %s/%s: %w", namespace, key, err)
	}
	return nil
}

func (s *kvStore) List(ctx context.Context, namespace string) (map[string][]byte, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT key, value FROM kv_store WHERE tenant_id = ? AND namespace = ?`,
		string(s.tenantID), namespace)
	if err != nil {
		return nil, fmt.Errorf("failed to list namespace %s: %w", namespace, err)
	}
	defer rows.Close()

	result := make(map[string][]byte)
	for rows.Next() {
		var k string
		var v []byte
		if err := rows.Scan(&k, &v); err != nil {
			return nil, fmt.Errorf("failed to scan row: %w", err)
		}
		result[k] = v
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("row iteration error: %w", err)
	}
	return result, nil
}

// nonNil stores a nil value as the empty value (spi.KeyValueStore contract).
func nonNil(b []byte) []byte {
	if b == nil {
		return []byte{}
	}
	return b
}

// affectedOne reports whether res changed exactly one row.
func affectedOne(res sql.Result, op string) (bool, error) {
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("failed to read rows affected by %s: %w", op, err)
	}
	return n == 1, nil
}

func (s *kvStore) PutIfAbsent(ctx context.Context, namespace, key string, value []byte) (bool, error) {
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO kv_store (tenant_id, namespace, key, value) VALUES (?, ?, ?, ?)
		 ON CONFLICT (tenant_id, namespace, key) DO NOTHING`,
		string(s.tenantID), namespace, key, nonNil(value))
	if err != nil {
		return false, fmt.Errorf("failed to put-if-absent key %s/%s: %w", namespace, key, err)
	}
	return affectedOne(res, "put-if-absent")
}

func (s *kvStore) CompareAndPut(ctx context.Context, namespace, key string, expected, value []byte) (bool, error) {
	res, err := s.db.ExecContext(ctx,
		`UPDATE kv_store SET value = ? WHERE tenant_id = ? AND namespace = ? AND key = ? AND value = ?`,
		nonNil(value), string(s.tenantID), namespace, key, nonNil(expected))
	if err != nil {
		return false, fmt.Errorf("failed to compare-and-put key %s/%s: %w", namespace, key, err)
	}
	return affectedOne(res, "compare-and-put")
}

func (s *kvStore) DeleteIfEqual(ctx context.Context, namespace, key string, expected []byte) (bool, error) {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM kv_store WHERE tenant_id = ? AND namespace = ? AND key = ? AND value = ?`,
		string(s.tenantID), namespace, key, nonNil(expected))
	if err != nil {
		return false, fmt.Errorf("failed to delete-if-equal key %s/%s: %w", namespace, key, err)
	}
	return affectedOne(res, "delete-if-equal")
}
