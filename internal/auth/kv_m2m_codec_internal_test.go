package auth

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// testHash is a bcrypt hash of "s" at bcrypt.DefaultCost, made once: the
// codec refuses any other cost.
var testHash = sync.OnceValue(func() string {
	h, err := bcrypt.GenerateFromPassword([]byte("s"), bcrypt.DefaultCost)
	if err != nil {
		panic(err)
	}
	return string(h)
})

// hashAtCost returns testHash with its cost field set to cost. Generating a
// hash at a high cost takes hours; bcrypt.Cost reads only this field.
func hashAtCost(t *testing.T, cost int) string {
	t.Helper()
	h := []byte(testHash())
	copy(h[4:6], fmt.Sprintf("%02d", cost))
	if got, err := bcrypt.Cost(h); err != nil || got != cost {
		t.Fatalf("hashAtCost(%d): cost %d, %v", cost, got, err)
	}
	return string(h)
}

// corruptBodyHash has a valid bcrypt header at bcrypt.DefaultCost and a body
// outside the bcrypt alphabet. bcrypt.Cost accepts it; every comparison
// against it fails with a decoding error, not a mismatch.
var corruptBodyHash = "$2a$10$" + strings.Repeat("!", 53)

func validClient(t *testing.T) *M2MClient {
	t.Helper()
	now := time.Now().UTC().Truncate(time.Millisecond)
	return &M2MClient{ClientID: "ABC123", HashedSecret: testHash(), TenantID: "acme", UserID: "ABC123", Roles: []string{"ROLE_M2M"}, SecretGen: 1, CreatedAt: now, UpdatedAt: now}
}

// A stored bcrypt cost outside [DefaultCost, DefaultCost+4] is undecodable,
// and the encoder refuses it: a low cost weakens the hash, and a high cost
// lets one unauthenticated token request burn hours of CPU.
func TestM2MCodec_BcryptCostBounds(t *testing.T) {
	for _, tc := range []struct {
		cost int
		ok   bool
	}{
		{bcrypt.MinCost, false},
		{bcrypt.DefaultCost - 1, false},
		{bcrypt.DefaultCost, true},
		{bcrypt.DefaultCost + 4, true},
		{bcrypt.DefaultCost + 5, false},
		{bcrypt.MaxCost, false},
	} {
		t.Run(fmt.Sprintf("cost %d", tc.cost), func(t *testing.T) {
			h := hashAtCost(t, tc.cost)
			data := validRecordJSON(t, func(r *m2mClientRecord) { r.HashedSecret = h })
			_, err := decodeClientRecord("acme", "ABC123", data)
			if tc.ok && err != nil {
				t.Fatalf("decode: %v, want accepted", err)
			}
			if !tc.ok && !errors.Is(err, errM2MUndecodable) {
				t.Fatalf("decode: %v, want errM2MUndecodable", err)
			}
			c := validClient(t)
			c.HashedSecret = h
			if _, err := encodeClientRecord(c); (err == nil) != tc.ok {
				t.Fatalf("encode: %v, want accepted=%v", err, tc.ok)
			}
		})
	}
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
		got.OnBehalfOf != c.OnBehalfOf || got.SecretGen != c.SecretGen ||
		!got.CreatedAt.Equal(c.CreatedAt) || !got.UpdatedAt.Equal(c.UpdatedAt) || strings.Join(got.Roles, ",") != "ROLE_M2M" {
		t.Fatalf("round trip: %+v", got)
	}
	ib, _ := encodeIndexEntry("acme")
	if tn, err := decodeIndexEntry(ib); err != nil || tn != "acme" {
		t.Fatalf("index: %v %v", tn, err)
	}
}

// OnBehalfOf and a SecretGen above 1 round-trip too: the zero value of
// OnBehalfOf is a valid (and common) value, so this is the only test that
// would notice the field being dropped on the wire.
func TestM2MCodec_RoundTripOnBehalfOfAndSecretGen(t *testing.T) {
	c := validClient(t)
	c.OnBehalfOf = true
	c.SecretGen = 3
	b, err := encodeClientRecord(c)
	if err != nil {
		t.Fatal(err)
	}
	got, err := decodeClientRecord("acme", "ABC123", b)
	if err != nil {
		t.Fatal(err)
	}
	if !got.OnBehalfOf || got.SecretGen != 3 {
		t.Fatalf("round trip: OnBehalfOf=%v SecretGen=%d, want true, 3", got.OnBehalfOf, got.SecretGen)
	}
}

func TestM2MCodec_EncoderRefuses(t *testing.T) {
	for name, mut := range map[string]func(c *M2MClient){
		"bad id":            func(c *M2MClient) { c.ClientID = "a-b" },
		"bad tenant":        func(c *M2MClient) { c.TenantID = spi.TenantID("a:b") },
		"bad user":          func(c *M2MClient) { c.UserID = "system" },
		"no roles":          func(c *M2MClient) { c.Roles = nil },
		"empty role":        func(c *M2MClient) { c.Roles = []string{""} },
		"not a hash":        func(c *M2MClient) { c.HashedSecret = "plaintext" },
		"corrupt hash body": func(c *M2MClient) { c.HashedSecret = corruptBodyHash },
		"year 10000":        func(c *M2MClient) { c.CreatedAt = time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC) },
		"secretGen zero":    func(c *M2MClient) { c.SecretGen = 0 },
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
		OnBehalfOf:   c.OnBehalfOf,
		SecretGen:    c.SecretGen,
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
		"bad user":                              {"acme", "ABC123", validRecordJSON(t, func(r *m2mClientRecord) { r.UserID = "SYSTEM" })},
		"no roles":                              {"acme", "ABC123", validRecordJSON(t, func(r *m2mClientRecord) { r.Roles = []string{} })},
		"empty role":                            {"acme", "ABC123", validRecordJSON(t, func(r *m2mClientRecord) { r.Roles = []string{""} })},
		"not a hash":                            {"acme", "ABC123", validRecordJSON(t, func(r *m2mClientRecord) { r.HashedSecret = "plaintext" })},
		"hash body outside the bcrypt alphabet": {"acme", "ABC123", validRecordJSON(t, func(r *m2mClientRecord) { r.HashedSecret = corruptBodyHash })},
		"hash version outside 2a, 2b, 2y":       {"acme", "ABC123", validRecordJSON(t, func(r *m2mClientRecord) { r.HashedSecret = "$2x$" + testHash()[4:] })},
		"hash longer than 60":                   {"acme", "ABC123", validRecordJSON(t, func(r *m2mClientRecord) { r.HashedSecret = testHash() + "a" })},
		"timestamp out of range": {"acme", "ABC123", validRecordJSON(t, func(r *m2mClientRecord) {
			r.CreatedAt = "0000-01-01T00:00:00Z"
		})},
		"timestamp not parseable": {"acme", "ABC123", validRecordJSON(t, func(r *m2mClientRecord) {
			r.CreatedAt = "not-a-date"
		})},
		"secretGen zero": {"acme", "ABC123", validRecordJSON(t, func(r *m2mClientRecord) { r.SecretGen = 0 })},
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
