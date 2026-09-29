package auth

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"time"

	"golang.org/x/crypto/bcrypt"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/common"
)

const (
	m2mClientsNamespacePrefix = "m2m-clients:"
	m2mClientIndexNamespace   = "m2m-client-ids"
	// m2mDecoyKey is read on an index miss so every decided token request
	// makes two reads. It is outside the client-id grammar, so never written.
	m2mDecoyKey = "-"
)

var errM2MUndecodable = errors.New("stored m2m client data does not decode")

var clientIDGrammar = regexp.MustCompile(`^[A-Za-z0-9]{1,100}$`)

// ValidClientID reports whether id matches the client-id grammar.
func ValidClientID(id string) bool { return clientIDGrammar.MatchString(id) }

// m2mTenantNamespace is the KV namespace holding tenant's client records.
// Tenant ids cannot contain ':', so namespaces cannot alias.
func m2mTenantNamespace(t spi.TenantID) string { return m2mClientsNamespacePrefix + string(t) }

type m2mClientRecord struct {
	ClientID     string   `json:"clientId"`
	TenantID     string   `json:"tenantId"`
	UserID       string   `json:"userId"`
	Roles        []string `json:"roles"`
	HashedSecret string   `json:"hashedSecret"`
	CreatedAt    string   `json:"createdAt"`
	UpdatedAt    string   `json:"updatedAt"`
}

type m2mIndexEntry struct {
	TenantID string `json:"tenantId"`
}

func validateM2MClient(c *M2MClient) error {
	if !ValidClientID(c.ClientID) {
		return errors.New("client id outside the grammar")
	}
	if err := common.ValidateTenantID(c.TenantID); err != nil {
		return fmt.Errorf("tenant: %w", err)
	}
	if err := common.ValidateFirstPartyUserID(c.UserID); err != nil {
		return fmt.Errorf("user: %w", err)
	}
	if len(c.Roles) == 0 {
		return errors.New("no roles")
	}
	for _, r := range c.Roles {
		if r == "" {
			return errors.New("empty role")
		}
	}
	if _, err := bcrypt.Cost([]byte(c.HashedSecret)); err != nil {
		return errors.New("hashedSecret is not a bcrypt hash")
	}
	if !StorableTime(c.CreatedAt) || !StorableTime(c.UpdatedAt) {
		return errors.New("timestamp out of range")
	}
	return nil
}

// encodeClientRecord refuses anything decodeClientRecord would reject.
func encodeClientRecord(c *M2MClient) ([]byte, error) {
	if err := validateM2MClient(c); err != nil {
		return nil, fmt.Errorf("failed to encode m2m client record: %w", err)
	}
	return json.Marshal(m2mClientRecord{
		ClientID: c.ClientID, TenantID: string(c.TenantID), UserID: c.UserID,
		Roles: append([]string(nil), c.Roles...), HashedSecret: c.HashedSecret,
		CreatedAt: c.CreatedAt.UTC().Format(time.RFC3339Nano), UpdatedAt: c.UpdatedAt.UTC().Format(time.RFC3339Nano),
	})
}

// decodeClientRecord binds the record to its KV key and namespace tenant.
func decodeClientRecord(tenant spi.TenantID, key string, data []byte) (*M2MClient, error) {
	var r m2mClientRecord
	if err := json.Unmarshal(data, &r); err != nil {
		return nil, fmt.Errorf("%w: %w", errM2MUndecodable, err)
	}
	if r.ClientID != key || spi.TenantID(r.TenantID) != tenant {
		return nil, fmt.Errorf("%w: record does not match its key or namespace", errM2MUndecodable)
	}
	created, err1 := time.Parse(time.RFC3339Nano, r.CreatedAt)
	updated, err2 := time.Parse(time.RFC3339Nano, r.UpdatedAt)
	if err1 != nil || err2 != nil {
		return nil, fmt.Errorf("%w: bad timestamp", errM2MUndecodable)
	}
	c := &M2MClient{ClientID: r.ClientID, HashedSecret: r.HashedSecret, TenantID: spi.TenantID(r.TenantID), UserID: r.UserID, Roles: r.Roles, CreatedAt: created, UpdatedAt: updated}
	if err := validateM2MClient(c); err != nil {
		return nil, fmt.Errorf("%w: %w", errM2MUndecodable, err)
	}
	return c, nil
}

func encodeIndexEntry(t spi.TenantID) ([]byte, error) {
	if err := common.ValidateTenantID(t); err != nil {
		return nil, fmt.Errorf("failed to encode m2m client index entry: %w", err)
	}
	return json.Marshal(m2mIndexEntry{TenantID: string(t)})
}

func decodeIndexEntry(data []byte) (spi.TenantID, error) {
	var e m2mIndexEntry
	if err := json.Unmarshal(data, &e); err != nil {
		return "", fmt.Errorf("%w: %w", errM2MUndecodable, err)
	}
	if err := common.ValidateTenantID(spi.TenantID(e.TenantID)); err != nil {
		return "", fmt.Errorf("%w: %w", errM2MUndecodable, err)
	}
	return spi.TenantID(e.TenantID), nil
}
