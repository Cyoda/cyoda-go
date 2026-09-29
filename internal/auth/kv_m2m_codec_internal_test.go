package auth

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

func validClient(t *testing.T) *M2MClient {
	t.Helper()
	h, _ := bcrypt.GenerateFromPassword([]byte("s"), bcrypt.MinCost)
	now := time.Now().UTC().Truncate(time.Millisecond)
	return &M2MClient{ClientID: "ABC123", HashedSecret: string(h), TenantID: "acme", UserID: "ABC123", Roles: []string{"ROLE_M2M"}, CreatedAt: now, UpdatedAt: now}
}

func TestM2MCodec_RoundTrip(t *testing.T) {
	c := validClient(t)
	b, err := encodeClientRecord(c)
	if err != nil {
		t.Fatal(err)
	}
	got, err := decodeClientRecord("acme", "ABC123", b)
	if err != nil {
		t.Fatal(err)
	}
	if got.ClientID != c.ClientID || got.TenantID != c.TenantID || got.UserID != c.UserID || got.HashedSecret != c.HashedSecret ||
		!got.CreatedAt.Equal(c.CreatedAt) || !got.UpdatedAt.Equal(c.UpdatedAt) || strings.Join(got.Roles, ",") != "ROLE_M2M" {
		t.Fatalf("round trip: %+v", got)
	}
	ib, _ := encodeIndexEntry("acme")
	if tn, err := decodeIndexEntry(ib); err != nil || tn != "acme" {
		t.Fatalf("index: %v %v", tn, err)
	}
}

func TestM2MCodec_EncoderRefuses(t *testing.T) {
	for name, mut := range map[string]func(c *M2MClient){
		"bad id":     func(c *M2MClient) { c.ClientID = "a-b" },
		"bad tenant": func(c *M2MClient) { c.TenantID = spi.TenantID("a:b") },
		"bad user":   func(c *M2MClient) { c.UserID = "oidc:x" },
		"no roles":   func(c *M2MClient) { c.Roles = nil },
		"empty role": func(c *M2MClient) { c.Roles = []string{""} },
		"not a hash": func(c *M2MClient) { c.HashedSecret = "plaintext" },
		"year 10000": func(c *M2MClient) { c.CreatedAt = time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC) },
	} {
		t.Run(name, func(t *testing.T) {
			c := validClient(t)
			mut(c)
			if _, err := encodeClientRecord(c); err == nil {
				t.Fatal("want error")
			}
		})
	}
}

// validRecordJSON marshals a valid m2mClientRecord (mirroring validClient's
// fields) and applies mutate to it before encoding, so a test can put a
// single corrupted field on the wire without going through encodeClientRecord
// (which would refuse it before it ever reached decodeClientRecord).
func validRecordJSON(t *testing.T, mutate func(r *m2mClientRecord)) []byte {
	t.Helper()
	c := validClient(t)
	ts := c.CreatedAt.UTC().Format(time.RFC3339Nano)
	r := m2mClientRecord{
		ClientID:     c.ClientID,
		TenantID:     string(c.TenantID),
		UserID:       c.UserID,
		Roles:        append([]string(nil), c.Roles...),
		HashedSecret: c.HashedSecret,
		CreatedAt:    ts,
		UpdatedAt:    ts,
	}
	if mutate != nil {
		mutate(&r)
	}
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestM2MCodec_DecoderRefuses(t *testing.T) {
	good, _ := encodeClientRecord(validClient(t))
	cases := map[string]struct {
		tenant spi.TenantID
		key    string
		data   []byte
	}{
		"not json":            {"acme", "ABC123", []byte("{")},
		"key differs":         {"acme", "OTHER1", good},
		"tenant differs":      {"other", "ABC123", good},
		"key outside grammar": {"acme", "a-b", []byte(strings.Replace(string(good), `"ABC123"`, `"a-b"`, 1))},
		// These carry a valid key/tenant/clientId so they reach
		// validateM2MClient inside decodeClientRecord itself, rather than
		// being rejected by encodeClientRecord before ever hitting the
		// wire (which is all TestM2MCodec_EncoderRefuses exercises).
		"bad user":   {"acme", "ABC123", validRecordJSON(t, func(r *m2mClientRecord) { r.UserID = "oidc:x" })},
		"no roles":   {"acme", "ABC123", validRecordJSON(t, func(r *m2mClientRecord) { r.Roles = []string{} })},
		"empty role": {"acme", "ABC123", validRecordJSON(t, func(r *m2mClientRecord) { r.Roles = []string{""} })},
		"not a hash": {"acme", "ABC123", validRecordJSON(t, func(r *m2mClientRecord) { r.HashedSecret = "plaintext" })},
		"timestamp out of range": {"acme", "ABC123", validRecordJSON(t, func(r *m2mClientRecord) {
			r.CreatedAt = "0000-01-01T00:00:00Z"
		})},
		"timestamp not parseable": {"acme", "ABC123", validRecordJSON(t, func(r *m2mClientRecord) {
			r.CreatedAt = "not-a-date"
		})},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := decodeClientRecord(tc.tenant, tc.key, tc.data); !errors.Is(err, errM2MUndecodable) {
				t.Fatalf("err = %v, want errM2MUndecodable", err)
			}
		})
	}
	if _, err := decodeIndexEntry([]byte(`{"tenantId":"a:b"}`)); !errors.Is(err, errM2MUndecodable) {
		t.Fatalf("index with bad tenant: %v", err)
	}
	if _, err := decodeIndexEntry([]byte("{")); !errors.Is(err, errM2MUndecodable) {
		t.Fatalf("index not json: %v", err)
	}
}

func TestM2MCodec_RecordHoldsNoPlaintext(t *testing.T) {
	c := validClient(t)
	b, _ := encodeClientRecord(c)
	if strings.Contains(string(b), `"s"`) {
		t.Fatal("record contains the plaintext secret")
	}
}
