package auth

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/common"
)

// m2mClientsNamespacePrefix prefixes the KV namespace of each tenant's
// client records. There is one namespace per tenant and no other: a client
// is found by (tenant, client id).
const m2mClientsNamespacePrefix = "m2m-clients:"

// The bcrypt cost a stored secret hash may carry. The encoder writes
// bcrypt.DefaultCost; a lower cost weakens the hash, and a stored cost bounds
// the CPU an unauthenticated token request can burn.
const (
	minM2MBcryptCost = bcrypt.DefaultCost
	maxM2MBcryptCost = bcrypt.DefaultCost + 4
)

var errM2MUndecodable = errors.New("stored m2m client data does not decode")

// clientIDGrammar is the tenant grammar: a client id may be chosen by its
// tenant, and appears in URLs and in Basic credentials, so it uses only
// characters that never need encoding.
var clientIDGrammar = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,99}$`)

// bcryptHashShape is the exact shape of a bcrypt hash: a 2a, 2b or 2y
// version, a two-digit cost, and 53 characters of salt and hash in the
// bcrypt alphabet — 60 characters in all. bcrypt.Cost reads only the header,
// so a hash with a damaged body would pass it and then fail every comparison
// as if the secret were wrong.
var bcryptHashShape = regexp.MustCompile(`^\$2[aby]\$[0-9]{2}\$[./A-Za-z0-9]{53}$`)

// ValidClientID reports whether id is a client id: the client-id grammar,
// and not "system" in any letter case — a client's id is its user id on its
// tokens and in audit records, where "system" is reserved.
func ValidClientID(id string) bool {
	return clientIDGrammar.MatchString(id) && !strings.EqualFold(id, common.ReservedSystemUserID)
}

// m2mTenantNamespace is the KV namespace holding tenant's client records,
// keyed by client id. Tenant ids cannot contain ':', so namespaces cannot
// alias.
func m2mTenantNamespace(t spi.TenantID) string { return m2mClientsNamespacePrefix + string(t) }

type m2mClientRecord struct {
	ClientID     string   `json:"clientId"`
	TenantID     string   `json:"tenantId"`
	UserID       string   `json:"userId"`
	Roles        []string `json:"roles"`
	HashedSecret string   `json:"hashedSecret"`
	OnBehalfOf   bool     `json:"onBehalfOf"`
	SecretGen    uint64   `json:"secretGen"`
	CreatedAt    string   `json:"createdAt"`
	UpdatedAt    string   `json:"updatedAt"`
}

func validateM2MClient(c *M2MClient) error {
	if !ValidClientID(c.ClientID) {
		return errors.New("client id outside the grammar")
	}
	if err := common.ValidateTenantID(c.TenantID); err != nil {
		return fmt.Errorf("tenant: %w", err)
	}
	if err := common.ValidateUserID(c.UserID); err != nil {
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
	if !bcryptHashShape.MatchString(c.HashedSecret) {
		return errors.New("hashedSecret is not a bcrypt hash")
	}
	cost, err := bcrypt.Cost([]byte(c.HashedSecret))
	if err != nil {
		return errors.New("hashedSecret is not a bcrypt hash")
	}
	if cost < minM2MBcryptCost || cost > maxM2MBcryptCost {
		return fmt.Errorf("hashedSecret has bcrypt cost %d, outside [%d, %d]", cost, minM2MBcryptCost, maxM2MBcryptCost)
	}
	if c.SecretGen < 1 || c.SecretGen >= genLimit {
		return errors.New("secretGen outside [1, 2^53)")
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
		OnBehalfOf: c.OnBehalfOf, SecretGen: c.SecretGen,
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
	// storableTimestamp (signing_records.go) is the same parse-and-range
	// check decodeSigningRecord uses; reused here so a record with an
	// unparseable or out-of-range timestamp is rejected the same way on
	// every KV-backed store this package has.
	if !storableTimestamp(r.CreatedAt) || !storableTimestamp(r.UpdatedAt) {
		return nil, fmt.Errorf("%w: bad timestamp", errM2MUndecodable)
	}
	created, _ := time.Parse(time.RFC3339Nano, r.CreatedAt)
	updated, _ := time.Parse(time.RFC3339Nano, r.UpdatedAt)
	c := &M2MClient{ClientID: r.ClientID, HashedSecret: r.HashedSecret, TenantID: spi.TenantID(r.TenantID), UserID: r.UserID, Roles: r.Roles, OnBehalfOf: r.OnBehalfOf, SecretGen: r.SecretGen, CreatedAt: created, UpdatedAt: updated}
	if err := validateM2MClient(c); err != nil {
		return nil, fmt.Errorf("%w: %w", errM2MUndecodable, err)
	}
	return c, nil
}
